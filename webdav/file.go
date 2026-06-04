package webdav

import (
	"errors"
	"io"
	"mime"
	"os"
	"path"
	"strings"
	"time"

	"github.com/onaonbir/Cloodsy-S3/db"
)

var (
	errIsDir  = errors.New("is a directory")
	errNotDir = errors.New("not a directory")
)

// fileInfo is a lightweight os.FileInfo for DB-backed objects and virtual dirs.
type fileInfo struct {
	name    string
	size    int64
	modTime time.Time
	dir     bool
}

func (fi fileInfo) Name() string { return fi.name }
func (fi fileInfo) Size() int64  { return fi.size }
func (fi fileInfo) Mode() os.FileMode {
	if fi.dir {
		return os.ModeDir | 0o755
	}
	return 0o644
}
func (fi fileInfo) ModTime() time.Time {
	if fi.modTime.IsZero() {
		return time.Unix(0, 0)
	}
	return fi.modTime
}
func (fi fileInfo) IsDir() bool      { return fi.dir }
func (fi fileInfo) Sys() interface{} { return nil }

// readFile wraps a storage reader (an *os.File, hence seekable) as a webdav.File.
type readFile struct {
	rc   io.ReadCloser
	info os.FileInfo
}

func newReadFile(rc io.ReadCloser, info os.FileInfo) *readFile {
	return &readFile{rc: rc, info: info}
}

func (rf *readFile) Read(p []byte) (int, error) { return rf.rc.Read(p) }
func (rf *readFile) Close() error               { return rf.rc.Close() }
func (rf *readFile) Write([]byte) (int, error)  { return 0, os.ErrPermission }
func (rf *readFile) Seek(offset int64, whence int) (int64, error) {
	if s, ok := rf.rc.(io.Seeker); ok {
		return s.Seek(offset, whence)
	}
	return 0, errors.New("seek not supported")
}
func (rf *readFile) Readdir(int) ([]os.FileInfo, error) { return nil, errNotDir }
func (rf *readFile) Stat() (os.FileInfo, error)         { return rf.info, nil }

// dirFile is a virtual directory whose contents are derived from a key prefix.
type dirFile struct {
	fs     *davFS
	bucket *db.Bucket
	key    string // "" for root, else the prefix without trailing slash
	info   os.FileInfo
}

func (d *dirFile) Read([]byte) (int, error)       { return 0, errIsDir }
func (d *dirFile) Write([]byte) (int, error)      { return 0, os.ErrPermission }
func (d *dirFile) Seek(int64, int) (int64, error) { return 0, errIsDir }
func (d *dirFile) Close() error                   { return nil }
func (d *dirFile) Stat() (os.FileInfo, error)     { return d.info, nil }

func (d *dirFile) Readdir(count int) ([]os.FileInfo, error) {
	prefix := ""
	if d.key != "" {
		prefix = d.key + "/"
	}
	objs, prefixes, _, _, err := d.fs.db.ListObjectsMeta(d.bucket.ID, prefix, "", "/", 1000)
	if err != nil {
		return nil, err
	}
	var infos []os.FileInfo
	for _, p := range prefixes {
		name := strings.TrimSuffix(strings.TrimPrefix(p, prefix), "/")
		if name == "" {
			continue
		}
		infos = append(infos, fileInfo{name: name, dir: true})
	}
	for i := range objs {
		o := objs[i]
		if o.IsDeleteMarker || strings.HasSuffix(o.Key, "/") {
			continue // delete markers and directory markers are not files
		}
		name := strings.TrimPrefix(o.Key, prefix)
		if name == "" || strings.Contains(name, "/") {
			continue
		}
		infos = append(infos, fileInfo{name: name, size: o.Size, modTime: o.LastModified})
	}
	return infos, nil
}

// writeFile buffers an upload to a temp file and commits it to storage + DB on
// Close, so unknown-length / chunked PUTs work without holding the body in RAM.
type writeFile struct {
	fs     *davFS
	bucket *db.Bucket
	key    string
	tmp    *os.File
	size   int64
	closed bool
}

func newWriteFile(f *davFS, bucket *db.Bucket, key string) (*writeFile, error) {
	tmp, err := os.CreateTemp("", "cloodsy-dav-*")
	if err != nil {
		return nil, err
	}
	return &writeFile{fs: f, bucket: bucket, key: key, tmp: tmp}, nil
}

func (wf *writeFile) Write(p []byte) (int, error) {
	n, err := wf.tmp.Write(p)
	wf.size += int64(n)
	return n, err
}

func (wf *writeFile) Seek(offset int64, whence int) (int64, error) {
	return wf.tmp.Seek(offset, whence)
}

func (wf *writeFile) Read([]byte) (int, error)           { return 0, os.ErrPermission }
func (wf *writeFile) Readdir(int) ([]os.FileInfo, error) { return nil, errNotDir }

func (wf *writeFile) Stat() (os.FileInfo, error) {
	return fileInfo{name: path.Base(wf.key), size: wf.size, modTime: time.Now()}, nil
}

func (wf *writeFile) Close() error {
	if wf.closed {
		return nil
	}
	wf.closed = true
	tmpName := wf.tmp.Name()
	defer func() {
		wf.tmp.Close()
		os.Remove(tmpName)
	}()

	if _, err := wf.tmp.Seek(0, io.SeekStart); err != nil {
		return err
	}
	size, etag, err := wf.fs.store.PutObject(wf.bucket.Name, wf.key, wf.tmp)
	if err != nil {
		return err
	}
	ct := mime.TypeByExtension(path.Ext(wf.key))
	if ct == "" {
		ct = "application/octet-stream"
	}
	// New content invalidates any cached image derivatives for this key.
	wf.fs.store.DeleteVariantsForKey(wf.bucket.Name, wf.key)
	return wf.fs.db.PutObjectMeta(&db.ObjectMeta{
		BucketID:     wf.bucket.ID,
		Key:          wf.key,
		Size:         size,
		ETag:         etag,
		ContentType:  ct,
		LastModified: time.Now().UTC(),
		IsLatest:     true,
	})
}
