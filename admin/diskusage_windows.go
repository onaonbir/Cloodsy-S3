//go:build windows

package admin

import "golang.org/x/sys/windows"

// diskUsage reports the total and available (to the calling user) bytes of
// the volume holding path.
func diskUsage(path string) (total, free uint64, ok bool) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, 0, false
	}
	var avail, tot, totalFree uint64
	if err := windows.GetDiskFreeSpaceEx(p, &avail, &tot, &totalFree); err != nil {
		return 0, 0, false
	}
	return tot, avail, true
}
