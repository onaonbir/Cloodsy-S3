package image

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// CacheDirLister is implemented by storage backends that keep variant caches
// on the local filesystem.
type CacheDirLister interface {
	VariantCacheDir(bucket string) (string, error)
}

// BucketNamer lists bucket names (satisfied by *db.DB).
type BucketNamer interface {
	ListBucketNames() ([]string, error)
}

// RunCacheJanitor periodically trims every bucket's variant cache to maxBytes
// by removing the least recently modified files first. maxBytes <= 0 disables
// trimming.
func RunCacheJanitor(ctx context.Context, buckets BucketNamer, store interface{}, maxBytes int64, logger *slog.Logger) {
	lister, ok := store.(CacheDirLister)
	if !ok || maxBytes <= 0 {
		return
	}
	ticker := time.NewTicker(15 * time.Minute)
	defer ticker.Stop()
	trim := func() {
		names, err := buckets.ListBucketNames()
		if err != nil {
			return
		}
		for _, name := range names {
			if ctx.Err() != nil {
				return
			}
			dir, err := lister.VariantCacheDir(name)
			if err != nil {
				continue
			}
			if n, freed := trimDir(dir, maxBytes); n > 0 {
				logger.Info("variant cache trimmed", "bucket", name, "removed", n, "freedBytes", freed)
			}
		}
	}
	trim()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			trim()
		}
	}
}

type cacheEntry struct {
	path  string
	size  int64
	mtime time.Time
}

func trimDir(dir string, maxBytes int64) (removed int, freed int64) {
	var entries []cacheEntry
	var total int64
	filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		entries = append(entries, cacheEntry{p, info.Size(), info.ModTime()})
		total += info.Size()
		return nil
	})
	if total <= maxBytes {
		return 0, 0
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].mtime.Before(entries[j].mtime) })
	for _, e := range entries {
		if total <= maxBytes {
			break
		}
		if err := os.Remove(e.path); err == nil {
			total -= e.size
			freed += e.size
			removed++
			os.Remove(filepath.Dir(e.path)) // drop empty key dir (fails harmlessly otherwise)
		}
	}
	return removed, freed
}
