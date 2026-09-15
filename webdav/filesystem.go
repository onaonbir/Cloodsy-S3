package webdav

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path"
	"strings"
	"time"

	"github.com/onaonbir/Cloodsy-S3/db"
	"github.com/onaonbir/Cloodsy-S3/service"
	"golang.org/x/net/webdav"
)

var (
	// errPartialMove is recorded when a directory MOVE/DELETE left some keys
	// behind; the status mapper turns it into a 500.
	errPartialMove = errors.New("operation partially failed")
	// errInvalidKey is recorded for keys the object store cannot represent.
	errInvalidKey = errors.New("invalid object key")
)

// davFS implements webdav.FileSystem over the metadata DB + object service.
// The active bucket and read-only flag are pulled from the request context (set
// by the basicAuth middleware), so a single davFS instance serves every mount.
//
// S3 has no real directories: a "directory" is a key prefix. MKCOL writes a
// zero-byte directory marker (key ending in "/") through the object service so
// empty folders persist for clients (Finder/Explorer) and versioning, quota and
// webhooks see it like any other write.
type davFS struct {
	db      *db.DB
	objects *service.Objects
	logger  *slog.Logger
}

// keyFromName converts a WebDAV path to an S3 object key.
func keyFromName(name string) string {
	return strings.Trim(path.Clean("/"+name), "/")
}

// validateKey rejects keys the storage layer cannot represent. Dot segments
// never survive keyFromName's path.Clean, so only NUL and length remain.
func validateKey(key string) error {
	if key == "" || len(key) > 1024 || strings.IndexByte(key, 0) >= 0 {
		return errInvalidKey
	}
	for _, seg := range strings.Split(key, "/") {
		if seg == "." || seg == ".." {
			return errInvalidKey
		}
	}
	return nil
}

// fail records err for the status mapper and returns the os-level error the
// x/net handler expects.
func (f *davFS) fail(ctx context.Context, err error, osErr error) error {
	if st := stateFromCtx(ctx); st != nil {
		st.err = err
	}
	return osErr
}

func (f *davFS) Mkdir(ctx context.Context, name string, perm os.FileMode) error {
	if readOnlyFromCtx(ctx) {
		return os.ErrPermission
	}
	bucket := bucketFromCtx(ctx)
	if bucket == nil {
		return os.ErrPermission
	}
	key := keyFromName(name)
	if key == "" {
		return os.ErrExist // root always exists
	}
	if err := validateKey(key); err != nil {
		return f.fail(ctx, err, os.ErrInvalid)
	}
	if m, _ := f.latest(bucket, key); m != nil {
		return os.ErrExist // a file already lives at this path
	}
	if m, _ := f.latest(bucket, key+"/"); m != nil {
		return os.ErrExist
	}
	return f.putMarker(bucket, key+"/")
}

// putMarker writes a zero-byte directory placeholder through the service.
func (f *davFS) putMarker(bucket *db.Bucket, markerKey string) error {
	_, err := f.objects.Put(service.PutInput{
		Bucket:       bucket,
		Key:          markerKey,
		Body:         strings.NewReader(""),
		ContentType:  dirContentType,
		DeclaredSize: 0,
		SkipImage:    true,
	})
	return err
}

func (f *davFS) OpenFile(ctx context.Context, name string, flag int, perm os.FileMode) (webdav.File, error) {
	bucket := bucketFromCtx(ctx)
	if bucket == nil {
		return nil, os.ErrPermission
	}
	key := keyFromName(name)

	// Write intent → streaming writer that commits on Stat/Close.
	if flag&(os.O_WRONLY|os.O_RDWR|os.O_CREATE|os.O_TRUNC) != 0 {
		if readOnlyFromCtx(ctx) {
			return nil, os.ErrPermission
		}
		if err := validateKey(key); err != nil {
			return nil, f.fail(ctx, err, os.ErrInvalid)
		}
		if f.isDir(bucket, key) {
			return nil, errIsDir
		}
		return newWriteFile(f, bucket, key, stateFromCtx(ctx)), nil
	}

	// Read path.
	if key == "" {
		return f.dirFile(bucket, ""), nil
	}
	meta, err := f.latest(bucket, key)
	if err != nil {
		return nil, err
	}
	if meta != nil {
		rc, err := f.objects.Open(bucket, meta)
		if err != nil {
			if os.IsNotExist(err) {
				return nil, os.ErrNotExist
			}
			return nil, err
		}
		return newReadFile(rc, infoForObject(meta)), nil
	}
	if f.isDir(bucket, key) {
		return f.dirFile(bucket, key), nil
	}
	return nil, os.ErrNotExist
}

func (f *davFS) Stat(ctx context.Context, name string) (os.FileInfo, error) {
	bucket := bucketFromCtx(ctx)
	if bucket == nil {
		return nil, os.ErrPermission
	}
	key := keyFromName(name)
	if key == "" {
		return fileInfo{name: "/", dir: true}, nil
	}
	meta, err := f.latest(bucket, key)
	if err != nil {
		return nil, err
	}
	if meta != nil {
		return infoForObject(meta), nil
	}
	if f.isDir(bucket, key) {
		return fileInfo{name: path.Base(key), dir: true, modTime: f.dirModTime(bucket, key+"/")}, nil
	}
	return nil, os.ErrNotExist
}

func (f *davFS) RemoveAll(ctx context.Context, name string) error {
	if readOnlyFromCtx(ctx) {
		return os.ErrPermission
	}
	bucket := bucketFromCtx(ctx)
	if bucket == nil {
		return os.ErrPermission
	}
	key := keyFromName(name)
	if key == "" {
		return os.ErrPermission // never wipe the mount root
	}

	// A plain file?
	if m, _ := f.latest(bucket, key); m != nil {
		if err := f.deleteOne(bucket, key); err != nil {
			return err
		}
		f.cleanupEmptyParents(bucket, key)
		return nil
	}
	if !f.isDir(bucket, key) {
		return os.ErrNotExist
	}

	// A directory: remove the marker and everything beneath it. The listing
	// is collected up front so deletes never shift the pagination window.
	keys, err := f.listAllKeys(bucket, key+"/")
	if err != nil {
		return err
	}
	failed := 0
	for _, k := range keys {
		if err := f.deleteOne(bucket, k); err != nil {
			failed++
			f.logger.Warn("webdav: delete failed", "bucket", bucket.Name, "key", k, "error", err)
		}
	}
	// The directory itself is gone — drop any parent that is now empty too.
	f.cleanupEmptyParents(bucket, key)
	if failed > 0 {
		return f.fail(ctx, fmt.Errorf("%w: %d of %d keys not deleted", errPartialMove, failed, len(keys)), errPartialMove)
	}
	return nil
}

func (f *davFS) Rename(ctx context.Context, oldName, newName string) error {
	if readOnlyFromCtx(ctx) {
		return os.ErrPermission
	}
	bucket := bucketFromCtx(ctx)
	if bucket == nil {
		return os.ErrPermission
	}
	oldKey := keyFromName(oldName)
	newKey := keyFromName(newName)
	if oldKey == "" || newKey == "" {
		return os.ErrInvalid
	}
	if err := validateKey(newKey); err != nil {
		return f.fail(ctx, err, os.ErrInvalid)
	}
	// Moving a tree into itself (or onto itself) can never terminate.
	if newKey == oldKey || strings.HasPrefix(newKey, oldKey+"/") {
		return os.ErrPermission
	}

	// A plain file → copy + delete.
	if m, _ := f.latest(bucket, oldKey); m != nil {
		if err := f.copyObject(ctx, bucket, m, newKey); err != nil {
			return err
		}
		if err := f.deleteOne(bucket, oldKey); err != nil {
			return err
		}
		f.cleanupEmptyParents(bucket, oldKey)
		return nil
	}
	if !f.isDir(bucket, oldKey) {
		return os.ErrNotExist
	}

	// A directory → move the marker and every object under the prefix. The
	// full source listing is captured before the first write so the
	// destination keys (which may sort inside the scan range) can't be
	// re-listed and moved again.
	oldPrefix := oldKey + "/"
	newPrefix := newKey + "/"
	keys, err := f.listAllKeys(bucket, oldPrefix)
	if err != nil {
		return err
	}
	// Ensure the destination folder exists even when the source had no
	// explicit marker (implicit prefix).
	if err := f.putMarker(bucket, newPrefix); err != nil {
		return f.fail(ctx, err, err)
	}
	failed := 0
	for _, src := range keys {
		dst := newPrefix + strings.TrimPrefix(src, oldPrefix)
		var err error
		if strings.HasSuffix(src, "/") {
			// Directory markers carry no bytes (legacy ones have no storage
			// file at all), so they are recreated rather than copied.
			if src != oldPrefix { // the top-level marker was created above
				err = f.putMarker(bucket, dst)
			}
		} else {
			var m *db.ObjectMeta
			m, err = f.latest(bucket, src)
			if err == nil && m == nil {
				err = os.ErrNotExist
			}
			if err == nil {
				err = f.copyObject(ctx, bucket, m, dst)
			}
		}
		if err == nil {
			err = f.deleteOne(bucket, src)
		}
		if err != nil {
			failed++
			f.logger.Warn("webdav: move failed", "bucket", bucket.Name, "from", src, "to", dst, "error", err)
		}
	}
	f.cleanupEmptyParents(bucket, oldKey)
	if failed > 0 {
		return f.fail(ctx, fmt.Errorf("%w: %d of %d keys not moved", errPartialMove, failed, len(keys)), errPartialMove)
	}
	return nil
}

// --- helpers ---

// latest returns the current row for key, or nil when the key does not exist
// or its newest version is a delete marker.
func (f *davFS) latest(bucket *db.Bucket, key string) (*db.ObjectMeta, error) {
	m, err := f.db.GetObjectMeta(bucket.ID, key)
	if err != nil {
		return nil, err
	}
	if m == nil || m.IsDeleteMarker {
		return nil, nil
	}
	return m, nil
}

// listAllKeys returns every live key under prefix (no delimiter), following
// the pagination marker until the listing is exhausted or maxListEntries hit.
func (f *davFS) listAllKeys(bucket *db.Bucket, prefix string) ([]string, error) {
	var keys []string
	marker := ""
	for {
		objs, _, truncated, next, err := f.db.ListObjectsMeta(bucket.ID, prefix, marker, "", 1000)
		if err != nil {
			return nil, err
		}
		for i := range objs {
			if objs[i].IsDeleteMarker {
				continue
			}
			keys = append(keys, objs[i].Key)
		}
		if !truncated || next == "" {
			return keys, nil
		}
		if len(keys) >= maxListEntries {
			return nil, fmt.Errorf("prefix %q has more than %d entries", prefix, maxListEntries)
		}
		marker = next
	}
}

// cleanupEmptyParents walks up from key removing any parent directory marker
// that became empty after the delete/move. S3 has no real folders, so a folder
// created via MKCOL persists as a marker key ("dir/") even after its last child
// is gone; without this it would linger as an empty directory. Walks deepest
// first and stops at the first ancestor that still has children.
func (f *davFS) cleanupEmptyParents(bucket *db.Bucket, key string) {
	for {
		idx := strings.LastIndex(key, "/")
		if idx < 0 {
			return // reached the bucket root
		}
		key = key[:idx] // parent directory key, without the trailing slash
		if key == "" {
			return
		}
		prefix := key + "/"
		// List one level under this prefix. The directory's own marker has key
		// == prefix and must not count as a child.
		objs, prefixes, _, _, err := f.db.ListObjectsMeta(bucket.ID, prefix, "", "/", 2)
		if err != nil {
			return
		}
		children := len(prefixes)
		for i := range objs {
			if objs[i].Key != prefix {
				children++
			}
		}
		if children > 0 {
			return // still in use → this dir and every ancestor stays
		}
		// Empty: drop the marker if one exists, then keep walking up.
		if m, _ := f.latest(bucket, prefix); m != nil {
			if err := f.deleteOne(bucket, prefix); err != nil {
				return
			}
		}
	}
}

func (f *davFS) dirFile(bucket *db.Bucket, key string) *dirFile {
	name := "/"
	var mod time.Time
	if key != "" {
		name = path.Base(key)
		mod = f.dirModTime(bucket, key+"/")
	}
	return &dirFile{fs: f, bucket: bucket, key: key, info: fileInfo{name: name, dir: true, modTime: mod}}
}

// dirModTime returns the marker's timestamp when the folder was created
// explicitly, otherwise "now" (a zero time would render as 1970 in clients).
func (f *davFS) dirModTime(bucket *db.Bucket, markerKey string) time.Time {
	if m, _ := f.latest(bucket, markerKey); m != nil && !m.LastModified.IsZero() {
		return m.LastModified
	}
	return time.Now().UTC()
}

// isDir reports whether key is a directory prefix (has a marker or any children).
func (f *davFS) isDir(bucket *db.Bucket, key string) bool {
	if m, _ := f.latest(bucket, key+"/"); m != nil {
		return true
	}
	objs, prefixes, _, _, err := f.db.ListObjectsMeta(bucket.ID, key+"/", "", "/", 1)
	if err != nil {
		return false
	}
	return len(objs) > 0 || len(prefixes) > 0
}

// deleteOne removes the current version with S3 semantics (delete marker on
// versioning-enabled buckets, hard delete otherwise).
func (f *davFS) deleteOne(bucket *db.Bucket, key string) error {
	_, err := f.objects.Delete(bucket, key, "")
	return err
}

// copyObject copies the bytes of a specific row to dstKey through the service.
func (f *davFS) copyObject(ctx context.Context, bucket *db.Bucket, m *db.ObjectMeta, dstKey string) error {
	rc, err := f.objects.Open(bucket, m)
	if err != nil {
		return err
	}
	defer rc.Close()
	_, err = f.objects.Put(service.PutInput{
		Bucket:       bucket,
		Key:          dstKey,
		Body:         rc,
		ContentType:  m.ContentType,
		Metadata:     m.Metadata,
		DeclaredSize: m.Size,
		MaxSize:      maxObjectSize,
		EventType:    "s3:ObjectCreated:Copy",
	})
	if err != nil {
		return f.fail(ctx, err, err)
	}
	return nil
}
