//go:build !unix

package admin

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
)

// sameDevice probes whether os.Rename works between the two directories by
// renaming a temporary file. A cross-volume rename fails with EXDEV (or the
// platform equivalent), which we report as "different device".
func sameDevice(a, b string) (bool, error) {
	f, err := os.CreateTemp(a, ".cloodsy-move-probe-*")
	if err != nil {
		return false, err
	}
	src := f.Name()
	f.Close()
	defer os.Remove(src)

	dst := filepath.Join(b, filepath.Base(src))
	err = os.Rename(src, dst)
	if err == nil {
		os.Remove(dst)
		return true, nil
	}
	if errors.Is(err, syscall.EXDEV) {
		return false, nil
	}
	var linkErr *os.LinkError
	if errors.As(err, &linkErr) {
		// Windows reports ERROR_NOT_SAME_DEVICE (17) for cross-volume moves.
		if errno, ok := linkErr.Err.(syscall.Errno); ok && errno == 17 {
			return false, nil
		}
	}
	return false, err
}
