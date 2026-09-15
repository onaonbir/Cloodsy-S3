package webdav

import (
	"context"
	"errors"
	"io"
	"mime"
	"os"
	"path"
	"strings"
	"time"

	"github.com/onaonbir/Cloodsy-S3/db"
	"github.com/onaonbir/Cloodsy-S3/service"
	"golang.org/x/net/webdav"
)

var (
	errIsDir  = errors.New("is a directory")
	errNotDir = errors.New("not a directory")
)

const (
	// maxObjectSize mirrors the S3 PutObject cap (5 GB).
	maxObjectSize int64 = 5 * 1024 * 1024 * 1024
	// maxListEntries bounds a single directory listing / recursive walk so a
	// huge prefix cannot exhaust memory.
	maxListEntries = 100_000
	dirContentType = "application/x-directory"
)

// fileInfo is a lightweight os.FileInfo for DB-backed objects and virtual dirs.
// It also implements webdav.ETager / webdav.ContentTyper so PROPFIND reports
// the stored ETag and content type instead of deriving them from mtime or by
// sniffing the file.
type fileInfo struct {
	name        string
	size        int64
	modTime     time.Time
	dir         bool
	etag        string
	contentType string
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
		return time.Now().UTC()
	}
	return fi.modTime
}
func (fi fileInfo) IsDir() bool      { return fi.dir }
func (fi fileInfo) Sys() interface{} { return nil }

// ETag returns the stored object ETag; directories fall back to the default.
func (fi fileInfo) ETag(context.Context) (string, error) {
	if fi.dir || fi.etag == "" {
		return "", webdav.ErrNotImplemented
	}
	return fi.etag, nil
}

// ContentType returns the stored content type without opening the file.
func (fi fileInfo) ContentType(context.Context) (string, error) {
	if fi.dir {
		return "httpd/unix-directory", nil
	}
	if fi.contentType == "" {
		return "", webdav.ErrNotImplemented
	}
	return fi.contentType, nil
}

// infoForObject builds the FileInfo for a latest object row.
func infoForObject(m *db.ObjectMeta) fileInfo {
	return fileInfo{
		name:        path.Base(m.Key),
		size:        m.Size,
		modTime:     m.LastModified,
		etag:        m.ETag,
		contentType: m.ContentType,
	}
}

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

// Readdir lists one level below the directory. It pages through the DB until
// the listing is exhausted (bounded by maxListEntries); the count argument is
// ignored because the webdav handler always asks for everything.
func (d *dirFile) Readdir(int) ([]os.FileInfo, error) {
	prefix := ""
	if d.key != "" {
		prefix = d.key + "/"
	}
	var infos []os.FileInfo
	marker := ""
	for {
		objs, prefixes, truncated, next, err := d.fs.db.ListObjectsMeta(d.bucket.ID, prefix, marker, "/", 1000)
		if err != nil {
			return nil, err
		}
		for _, p := range prefixes {
			name := strings.TrimSuffix(strings.TrimPrefix(p, prefix), "/")
			if name == "" {
				continue
			}
			infos = append(infos, fileInfo{name: name, dir: true, modTime: d.fs.dirModTime(d.bucket, p)})
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
			infos = append(infos, infoForObject(&o))
		}
		if !truncated || next == "" || len(infos) >= maxListEntries {
			break
		}
		marker = next
	}
	return infos, nil
}

// writeFile streams an upload straight into the object service. The webdav
// handler calls Write repeatedly, then Stat, then Close; the first Write (or
// Stat/Close for an empty body) starts Objects.Put on a pipe so nothing is
// buffered to disk twice or held in RAM. Stat and Close both finalize the
// upload, because the handler asks for Stat (to build the ETag) before Close.
type writeFile struct {
	fs     *davFS
	bucket *db.Bucket
	key    string
	state  *reqState

	pw       *io.PipeWriter
	done     chan struct{}
	res      *service.PutResult
	err      error
	written  int64
	started  bool
	finished bool
}

func newWriteFile(f *davFS, bucket *db.Bucket, key string, state *reqState) *writeFile {
	return &writeFile{fs: f, bucket: bucket, key: key, state: state}
}

func contentTypeForKey(key string) string {
	ct := mime.TypeByExtension(path.Ext(key))
	if ct == "" {
		ct = "application/octet-stream"
	}
	return ct
}

func (wf *writeFile) start() {
	if wf.started {
		return
	}
	wf.started = true
	pr, pw := io.Pipe()
	wf.pw = pw
	wf.done = make(chan struct{})
	declared := int64(-1)
	if wf.state != nil {
		declared = wf.state.declaredSize
	}
	go func() {
		defer close(wf.done)
		res, err := wf.fs.objects.Put(service.PutInput{
			Bucket:       wf.bucket,
			Key:          wf.key,
			Body:         pr,
			ContentType:  contentTypeForKey(wf.key),
			DeclaredSize: declared,
			MaxSize:      maxObjectSize,
		})
		if err != nil {
			// Unblock any pending Write with the real reason.
			pr.CloseWithError(err)
			wf.err = err
			return
		}
		pr.Close()
		wf.res = res
	}()
}

func (wf *writeFile) Write(p []byte) (int, error) {
	if wf.finished {
		return 0, os.ErrClosed
	}
	wf.start()
	n, err := wf.pw.Write(p)
	wf.written += int64(n)
	return n, err
}

// finish closes the pipe, waits for the service to commit and records the
// outcome. It is idempotent.
func (wf *writeFile) finish() error {
	if wf.finished {
		return wf.err
	}
	wf.finished = true
	wf.start()
	wf.pw.Close()
	<-wf.done
	if wf.err != nil && wf.state != nil {
		wf.state.err = wf.err
	}
	return wf.err
}

func (wf *writeFile) Seek(int64, int) (int64, error)     { return 0, os.ErrPermission }
func (wf *writeFile) Read([]byte) (int, error)           { return 0, os.ErrPermission }
func (wf *writeFile) Readdir(int) ([]os.FileInfo, error) { return nil, errNotDir }

func (wf *writeFile) Stat() (os.FileInfo, error) {
	if err := wf.finish(); err != nil {
		return nil, err
	}
	return fileInfo{
		name:        path.Base(wf.key),
		size:        wf.res.Size,
		modTime:     wf.res.LastModified,
		etag:        wf.res.ETag,
		contentType: contentTypeForKey(wf.key),
	}, nil
}

func (wf *writeFile) Close() error { return wf.finish() }
