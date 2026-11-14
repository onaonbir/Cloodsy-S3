package service

import (
	"encoding/json"
	"errors"
	"strings"
)

// S3 user-metadata limits (x-amz-meta-*).
const (
	MaxMetadataSize   = 2048 // total bytes of keys + values
	MaxMetadataKeyLen = 128
	MaxMetadataValLen = 256
)

var (
	// ErrMetadataTooLarge reports a key, value or total size over the S3 limits.
	ErrMetadataTooLarge = errors.New("metadata too large")
	// ErrInvalidMetadata reports a value that cannot be emitted as a header.
	ErrInvalidMetadata = errors.New("invalid metadata")
)

// EncodeMetadata validates user metadata (keys without the "x-amz-meta-"
// prefix) with the S3 rules and merges the persisted system header slots
// (MetaCacheControl, ...) into the stored JSON form. It is the single
// implementation behind the S3 API (x-amz-meta-* headers) and the Admin API
// metadata editor.
func EncodeMetadata(user map[string]string, system map[string]string) (string, error) {
	meta := make(map[string]string, len(user)+len(system))
	total := 0
	for k, v := range user {
		if len(k) > MaxMetadataKeyLen || len(v) > MaxMetadataValLen {
			return "", ErrMetadataTooLarge
		}
		total += len(k) + len(v)
		if total > MaxMetadataSize {
			return "", ErrMetadataTooLarge
		}
		if strings.ContainsAny(v, "\r\n") {
			return "", ErrInvalidMetadata
		}
		meta[k] = v
	}
	for k, v := range system {
		meta[k] = v
	}
	if len(meta) == 0 {
		return "{}", nil
	}
	data, err := json.Marshal(meta)
	if err != nil {
		return "{}", nil
	}
	return string(data), nil
}

// DecodeMetadata parses the stored JSON (nil for empty or malformed input).
func DecodeMetadata(metadata string) map[string]string {
	if metadata == "" || metadata == "{}" {
		return nil
	}
	var meta map[string]string
	if err := json.Unmarshal([]byte(metadata), &meta); err != nil {
		return nil
	}
	return meta
}

// SplitMetadata separates stored metadata into user metadata and the
// "$"-prefixed system header slots.
func SplitMetadata(metadata string) (user, system map[string]string) {
	user, system = map[string]string{}, map[string]string{}
	for k, v := range DecodeMetadata(metadata) {
		if IsSystemMetaKey(k) {
			system[k] = v
		} else {
			user[k] = v
		}
	}
	return user, system
}
