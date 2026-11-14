//go:build linux || darwin || freebsd

package admin

import "syscall"

// diskUsage reports the total and available (to unprivileged users) bytes of
// the filesystem holding path.
func diskUsage(path string) (total, free uint64, ok bool) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, 0, false
	}
	bsize := uint64(st.Bsize) //nolint:unconvert // Bsize is int64/uint32/uint64 depending on the OS
	return uint64(st.Blocks) * bsize, uint64(st.Bavail) * bsize, true
}
