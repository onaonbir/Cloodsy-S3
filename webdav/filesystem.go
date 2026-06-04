package webdav

import (
	"context"
	"os"
	"path"
	"strings"
	"time"

	"github.com/onaonbir/Cloodsy-S3/db"
	"github.com/onaonbir/Cloodsy-S3/storage"
	"golang.org/x/net/webdav"
)

// davFS implements webdav.FileSystem over the metadata DB + storage backend.
// The active bucket and read-only flag are pulled from the request context (set
// by the basicAuth middleware), so a single davFS instance serves every mount.
//
// S3 has no real directories: a "directory" is a key prefix. MKCOL writes a
// zero-byte directory marker (key ending in "/") in the DB only, so empty
// folders persist for clients (Finder/Explorer) without a storage file.
type davFS struct {
	db    *db.DB
	store storage.Backend
}

// keyFromName converts a WebDAV path to an S3 object key.
func keyFromName(name string) string {
	return strings.Trim(path.Clean("/"+name), "/")
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
	// DB-only directory marker (no storage file): avoids a path collision with a
	// sibling file of the same name and keeps the marker out of GetObject reads.
	return f.db.PutObjectMeta(&db.ObjectMeta{
		BucketID:     bucket.ID,
		Key:          key + "/",
		Size:         0,
		ContentType:  "application/x-directory",
		LastModified: time.Now().UTC(),
		IsLatest:     true,
	})
}

func (f *davFS) OpenFile(ctx context.Context, name string, flag int, perm os.FileMode) (webdav.File, error) {
	bucket := bucketFromCtx(ctx)
	if bucket == nil {
		return nil, os.ErrPermission
	}
	key := keyFromName(name)

	// Write intent → buffered writer that commits on Close.
	if flag&(os.O_WRONLY|os.O_RDWR|os.O_CREATE|os.O_TRUNC) != 0 {
		if readOnlyFromCtx(ctx) {
			return nil, os.ErrPermission
		}
		if key == "" {
			return nil, os.ErrInvalid
		}
		return newWriteFile(f, bucket, key)
	}

	// Read path.
	if key == "" {
		return f.dirFile(bucket, ""), nil
	}
	meta, err := f.db.GetObjectMeta(bucket.ID, key)
	if err != nil {
		return nil, err
	}
	if meta != nil && !meta.IsDeleteMarker {
		rc, err := f.store.GetObject(bucket.Name, key)
		if err != nil {
			return nil, err
		}
		return newReadFile(rc, fileInfo{name: path.Base(key), size: meta.Size, modTime: meta.LastModified}), nil
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
	meta, err := f.db.GetObjectMeta(bucket.ID, key)
	if err != nil {
		return nil, err
	}
	if meta != nil && !meta.IsDeleteMarker {
		return fileInfo{name: path.Base(key), size: meta.Size, modTime: meta.LastModified}, nil
	}
	if f.isDir(bucket, key) {
		return fileInfo{name: path.Base(key), dir: true}, nil
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
	if m, _ := f.db.GetObjectMeta(bucket.ID, key); m != nil {
		f.deleteOne(bucket, key)
		return nil
	}

	// Otherwise a directory: remove the marker and everything beneath it.
	prefix := key + "/"
	if m, _ := f.db.GetObjectMeta(bucket.ID, prefix); m != nil {
		f.db.DeleteObjectMeta(bucket.ID, prefix)
	}
	marker := ""
	for {
		objs, _, truncated, next, err := f.db.ListObjectsMeta(bucket.ID, prefix, marker, "", 1000)
		if err != nil {
			return err
		}
		for i := range objs {
			f.deleteOne(bucket, objs[i].Key)
		}
		if !truncated {
			break
		}
		marker = next
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

	// A plain file → copy + delete.
	if m, _ := f.db.GetObjectMeta(bucket.ID, oldKey); m != nil {
		if err := f.copyObject(bucket, oldKey, newKey, m); err != nil {
			return err
		}
		f.deleteOne(bucket, oldKey)
		return nil
	}

	// A directory → move the marker and every object under the prefix.
	oldPrefix := oldKey + "/"
	newPrefix := newKey + "/"
	if m, _ := f.db.GetObjectMeta(bucket.ID, oldPrefix); m != nil {
		f.db.PutObjectMeta(&db.ObjectMeta{
			BucketID: bucket.ID, Key: newPrefix, Size: 0,
			ContentType: "application/x-directory", LastModified: time.Now().UTC(), IsLatest: true,
		})
		f.db.DeleteObjectMeta(bucket.ID, oldPrefix)
	}
	marker := ""
	for {
		objs, _, truncated, next, err := f.db.ListObjectsMeta(bucket.ID, oldPrefix, marker, "", 1000)
		if err != nil {
			return err
		}
		for i := range objs {
			src := objs[i].Key
			dst := newPrefix + strings.TrimPrefix(src, oldPrefix)
			if err := f.copyObject(bucket, src, dst, &objs[i]); err == nil {
				f.deleteOne(bucket, src)
			}
		}
		if !truncated {
			break
		}
		marker = next
	}
	return nil
}

// --- helpers ---

func (f *davFS) dirFile(bucket *db.Bucket, key string) *dirFile {
	name := "/"
	if key != "" {
		name = path.Base(key)
	}
	return &dirFile{fs: f, bucket: bucket, key: key, info: fileInfo{name: name, dir: true}}
}

// isDir reports whether key is a directory prefix (has a marker or any children).
func (f *davFS) isDir(bucket *db.Bucket, key string) bool {
	if m, _ := f.db.GetObjectMeta(bucket.ID, key+"/"); m != nil {
		return true
	}
	objs, prefixes, _, _, err := f.db.ListObjectsMeta(bucket.ID, key+"/", "", "/", 1)
	if err != nil {
		return false
	}
	return len(objs) > 0 || len(prefixes) > 0
}

func (f *davFS) deleteOne(bucket *db.Bucket, key string) {
	f.db.DeleteObjectMeta(bucket.ID, key)
	f.store.DeleteObject(bucket.Name, key)
	f.store.DeleteVariantsForKey(bucket.Name, key)
}

func (f *davFS) copyObject(bucket *db.Bucket, srcKey, dstKey string, m *db.ObjectMeta) error {
	rc, err := f.store.GetObject(bucket.Name, srcKey)
	if err != nil {
		return err
	}
	defer rc.Close()
	size, etag, err := f.store.PutObject(bucket.Name, dstKey, rc)
	if err != nil {
		return err
	}
	return f.db.PutObjectMeta(&db.ObjectMeta{
		BucketID:     bucket.ID,
		Key:          dstKey,
		Size:         size,
		ETag:         etag,
		ContentType:  m.ContentType,
		LastModified: time.Now().UTC(),
		IsLatest:     true,
	})
}
