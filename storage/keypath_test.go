package storage

import (
	"encoding/hex"
	"strings"
	"testing"
)

func mustEncode(t *testing.T, key string) string {
	t.Helper()
	p, err := EncodeKeyPath(key)
	if err != nil {
		t.Fatalf("EncodeKeyPath(%q): %v", key, err)
	}
	return p
}

func TestEncodeKeyPath_SafeKeyUnchanged(t *testing.T) {
	if got := mustEncode(t, "a/b.txt"); got != "a/b.txt" {
		t.Fatalf("got %q, want a/b.txt", got)
	}
	if got := mustEncode(t, "photos/2024/ünïcode name.jpg"); got != "photos/2024/ünïcode name.jpg" {
		t.Fatalf("unicode+space segment should stay verbatim, got %q", got)
	}
}

func TestEncodeKeyPath_Errors(t *testing.T) {
	if _, err := EncodeKeyPath(""); err == nil {
		t.Fatal("empty key must error")
	}
	if _, err := EncodeKeyPath("a\x00b"); err == nil {
		t.Fatal("NUL byte must error")
	}
}

func TestEncodeKeyPath_DirPlaceholderDiffersFromFile(t *testing.T) {
	dir := mustEncode(t, "dir/")
	file := mustEncode(t, "dir")
	if dir == file {
		t.Fatalf("dir/ and dir map to the same path %q", dir)
	}
	if !strings.HasPrefix(dir, "dir/") {
		t.Fatalf("dir/ should live under dir/, got %q", dir)
	}
}

func TestEncodeKeyPath_NoDotOrEmptySegments(t *testing.T) {
	keys := []string{"a//b", "a/./b", "a/../b", "./x", "../x", "x/.", "x/..", "/lead", "trail/", "//"}
	seen := map[string]string{}
	for _, k := range keys {
		p := mustEncode(t, k)
		for _, seg := range strings.Split(p, "/") {
			if seg == "" || seg == "." || seg == ".." {
				t.Errorf("key %q -> %q contains forbidden segment %q", k, p, seg)
			}
		}
		if prev, dup := seen[p]; dup {
			t.Errorf("keys %q and %q collide on %q", prev, k, p)
		}
		seen[p] = k
	}
}

func TestEncodeKeyPath_EncodedSegments(t *testing.T) {
	cases := []struct {
		key  string
		want string
	}{
		{"%notsafe", "%256E6F7473616665"},
		{"CON.txt", "%434F4E2E747874"},
		{"nul", "%6E756C"},
		{"x.cloodsys3ext", "%" + strings.ToUpper(hex.EncodeToString([]byte("x.cloodsys3ext")))},
		{"a/../b", "a/%2E2E/b"},
		{"dir/", "dir/%"},
		{"a//b", "a/%/b"},
		{"a.v--b", "%612E762D2D62"},
		{".tmp-abc", "%2E746D702D616263"},
		{"trailing.", "%747261696C696E672E"},
		{"trailing ", "%747261696C696E6720"},
		{"bad:colon", "%6261643A636F6C6F6E"},
	}
	for _, c := range cases {
		if got := mustEncode(t, c.key); got != c.want {
			t.Errorf("EncodeKeyPath(%q) = %q, want %q", c.key, got, c.want)
		}
	}
}

func TestEncodeKeyPath_LongSegmentHashed(t *testing.T) {
	long := strings.Repeat("x", 300)
	p := mustEncode(t, long)
	if !strings.HasPrefix(p, hashedSegmentMark) {
		t.Fatalf("long segment should be hashed, got %q", p[:10])
	}
	if len(p) > 70 {
		t.Fatalf("hashed segment too long: %d", len(p))
	}
	// A different long segment must hash differently.
	if p2 := mustEncode(t, strings.Repeat("y", 300)); p2 == p {
		t.Fatal("distinct long segments collided")
	}
	// A 150-byte unsafe segment (hex would be 301 > 200) also hashes.
	mid := strings.Repeat("?", 150)
	if pm := mustEncode(t, mid); !strings.HasPrefix(pm, hashedSegmentMark) {
		t.Fatalf("150-byte unsafe segment should hash, got %q", pm[:10])
	}
}

func TestEncodeKeyPath_Injective(t *testing.T) {
	keys := []string{
		"a", "a/", "a//", "a/b", "a//b", "a/./b", "a/../b", "./a", "../a", "a/.", "a/..",
		"%", "%%", "%a", "a%", "%2F", "%252F",
		"dir", "dir/", "dir/%", "dir/%25",
		"CON", "con", "CON.txt", "con.txt", "COM1", "LPT9.x", "NUL.", "AUX ",
		"x.cloodsys3ext", "x.cloodsys3ext/", "y.v--1", ".tmp-z", ".tmp", "tmp-",
		"a:b", "a*b", "a?b", "a\"b", "a<b", "a>b", "a|b", "a\\b", "a\tb", "a\x7fb",
		"trail.", "trail ", "trail..", " lead", ".lead", "..lead",
		"ünïcode", "ünïcode/", "日本語/ファイル.txt", "emoji 🎉.png",
		strings.Repeat("x", 200), strings.Repeat("x", 201), strings.Repeat("x", 300),
		strings.Repeat("?", 99), strings.Repeat("?", 100), strings.Repeat("?", 150),
		"\xff\xfe", "ok/\xff", "report..final.pdf", "a b/c d",
		"%48", "H", // "%48" is hex for H; encoded "%48" is "%2548"
		"%2E2E", "..", "a/%2E2E/b",
	}
	seen := map[string]string{}
	for _, k := range keys {
		p, err := EncodeKeyPath(k)
		if err != nil {
			t.Fatalf("EncodeKeyPath(%q): %v", k, err)
		}
		if prev, dup := seen[p]; dup && prev != k {
			t.Errorf("collision: %q and %q both map to %q", prev, k, p)
		}
		seen[p] = k
		for _, seg := range strings.Split(p, "/") {
			if seg == "" || seg == "." || seg == ".." {
				t.Errorf("key %q -> %q has forbidden segment %q", k, p, seg)
			}
			if len(seg) > 255 {
				t.Errorf("key %q -> segment longer than 255 bytes", k)
			}
		}
	}
}

func TestSegmentIsSafe(t *testing.T) {
	safe := []string{"a", "file.txt", "with space", "ünï", "a-b_c", "x.tmp-", "tmp-x", "v--", "a.v-b"}
	for _, s := range safe {
		if !segmentIsSafe(s) {
			t.Errorf("%q should be safe", s)
		}
	}
	unsafe := []string{"", ".", "..", "%x", "a\x00b", "a\nb", "a:b", "end.", "end ", "CON", "com9.dat", "x.cloodsys3ext", "a.v--b", ".tmp-x", "\xff", strings.Repeat("a", 201)}
	for _, s := range unsafe {
		if segmentIsSafe(s) {
			t.Errorf("%q should be unsafe", s)
		}
	}
}
