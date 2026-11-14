//go:build !linux && !darwin && !freebsd && !windows

package admin

// diskUsage is unavailable on this platform; status omits the figures.
func diskUsage(string) (total, free uint64, ok bool) { return 0, 0, false }
