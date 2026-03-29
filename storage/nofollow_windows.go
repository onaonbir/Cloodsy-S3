//go:build windows

package storage

// openNoFollow is 0 on Windows (O_NOFOLLOW not available).
// Windows symlinks require elevated privileges, so the risk is lower.
const openNoFollow = 0
