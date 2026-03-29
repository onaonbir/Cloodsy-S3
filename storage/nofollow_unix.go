//go:build !windows

package storage

import "syscall"

// openNoFollow is O_NOFOLLOW on Unix systems, preventing symlink traversal.
const openNoFollow = syscall.O_NOFOLLOW
