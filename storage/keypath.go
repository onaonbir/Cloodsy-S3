package storage

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"unicode/utf8"
)

// On-disk key encoding.
//
// Object keys are mapped to filesystem paths segment by segment. A segment
// that is "safe" is stored verbatim (this keeps the layout of existing
// deployments untouched). A segment that would be ambiguous or invalid on
// disk is encoded as "%" followed by the upper-case hex of its bytes, or as
// "%H" + sha256 for long segments. Because a verbatim segment is never
// allowed to start with "%", encoded and verbatim segments can never
// collide, so two distinct keys always map to two distinct paths.
//
// Examples:
//
//	"a/b.txt"      -> a/b.txt
//	"dir/"         -> dir/%           (empty trailing segment)
//	"a//b"         -> a/%/b
//	"a/../b"       -> a/%2E2E/b
//	"x.cloodsys3ext" -> %782E636C6F6F64737933657874

const (
	maxVerbatimSegment = 200 // bytes; leaves room for suffixes within 255-byte filename limits
	hashedSegmentMark  = "%H"
)

// windowsReserved lists device names that cannot be used as file names on
// Windows regardless of extension.
var windowsReserved = map[string]bool{
	"CON": true, "PRN": true, "AUX": true, "NUL": true,
	"COM1": true, "COM2": true, "COM3": true, "COM4": true, "COM5": true, "COM6": true, "COM7": true, "COM8": true, "COM9": true,
	"LPT1": true, "LPT2": true, "LPT3": true, "LPT4": true, "LPT5": true, "LPT6": true, "LPT7": true, "LPT8": true, "LPT9": true,
}

// EncodeKeyPath converts an object key into a slash-separated relative path
// that is safe to join under a bucket directory. It never returns a path that
// contains ".", ".." or empty segments.
func EncodeKeyPath(key string) (string, error) {
	if key == "" {
		return "", fmt.Errorf("empty key")
	}
	if strings.IndexByte(key, 0) >= 0 {
		return "", fmt.Errorf("key contains NUL byte")
	}
	segs := strings.Split(key, "/")
	out := make([]string, len(segs))
	for i, s := range segs {
		out[i] = encodeSegment(s)
	}
	return strings.Join(out, "/"), nil
}

// LegacyKeyPath returns the pre-encoding on-disk relative path for a key
// (the raw key). It is only used by the one-time layout migration.
func LegacyKeyPath(key string) string {
	return key
}

func encodeSegment(seg string) string {
	if segmentIsSafe(seg) {
		return seg
	}
	if len(seg)*2+1 > maxVerbatimSegment {
		sum := sha256.Sum256([]byte(seg))
		return hashedSegmentMark + hex.EncodeToString(sum[:])
	}
	return "%" + strings.ToUpper(hex.EncodeToString([]byte(seg)))
}

// segmentIsSafe reports whether a key segment can be used verbatim as a
// directory or file name component on every supported platform without
// colliding with another key's path or with internal file names.
func segmentIsSafe(seg string) bool {
	if seg == "" || seg == "." || seg == ".." {
		return false
	}
	if len(seg) > maxVerbatimSegment {
		return false
	}
	if seg[0] == '%' {
		return false
	}
	if !utf8.ValidString(seg) {
		return false
	}
	for i := 0; i < len(seg); i++ {
		c := seg[i]
		if c < 0x20 || c == 0x7F {
			return false
		}
		switch c {
		case '\\', ':', '*', '?', '"', '<', '>', '|':
			return false
		}
	}
	last := seg[len(seg)-1]
	if last == '.' || last == ' ' {
		return false
	}
	// Internal suffixes / prefixes used by the storage layer.
	if strings.HasSuffix(seg, safeExt) || strings.Contains(seg, versionSep) || strings.HasPrefix(seg, tmpPrefix) {
		return false
	}
	// Windows device names (CON, NUL, COM1...), with or without extension.
	base := seg
	if i := strings.IndexByte(base, '.'); i >= 0 {
		base = base[:i]
	}
	if windowsReserved[strings.ToUpper(base)] {
		return false
	}
	return true
}
