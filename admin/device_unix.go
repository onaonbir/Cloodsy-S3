//go:build unix

package admin

import (
	"fmt"
	"os"
	"syscall"
)

// sameDevice reports whether two existing paths live on the same filesystem,
// which is what os.Rename needs to move a tree without copying.
func sameDevice(a, b string) (bool, error) {
	da, err := deviceOf(a)
	if err != nil {
		return false, err
	}
	db, err := deviceOf(b)
	if err != nil {
		return false, err
	}
	return da == db, nil
}

func deviceOf(p string) (uint64, error) {
	fi, err := os.Stat(p)
	if err != nil {
		return 0, err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, fmt.Errorf("stat %s: unexpected Sys type", p)
	}
	return uint64(st.Dev), nil //nolint:unconvert // Dev is int32 on some platforms
}
