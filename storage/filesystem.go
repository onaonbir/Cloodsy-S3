package storage

import (
	"crypto/md5"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
)

type FileSystem struct {
	RootDir    string
	mu         sync.RWMutex
	customDirs map[string]string // bucketName -> absolute base path
}

func NewFileSystem(rootDir string) (*FileSystem, error) {
	if err := os.MkdirAll(rootDir, 0700); err != nil {
		return nil, fmt.Errorf("create root dir: %w", err)
	}
	return &FileSystem{RootDir: rootDir, customDirs: make(map[string]string)}, nil
}

// LoadBucketDirs bulk-loads custom storage directories at startup.
func (fs *FileSystem) LoadBucketDirs(dirs map[string]string) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	for bucket, dir := range dirs {
		fs.customDirs[bucket] = dir
	}
}

// SetBucketDir registers a custom storage directory for a bucket.
func (fs *FileSystem) SetBucketDir(bucket, dir string) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	fs.customDirs[bucket] = dir
}

// RemoveBucketDir removes the custom storage directory registration for a bucket.
func (fs *FileSystem) RemoveBucketDir(bucket string) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	delete(fs.customDirs, bucket)
}

// bucketBasePath returns the base path for a bucket, using the custom directory if set.
func (fs *FileSystem) bucketBasePath(bucket string) string {
	fs.mu.RLock()
	defer fs.mu.RUnlock()
	if dir, ok := fs.customDirs[bucket]; ok && dir != "" {
		return dir
	}
	return fs.RootDir
}

const safeExt = ".cloodsys3ext"

// safePath ensures that the resolved path stays within the base directory,
// preventing path traversal attacks.
func safePath(base, userPath string) (string, error) {
	absBase, err := filepath.Abs(base)
	if err != nil {
		return "", fmt.Errorf("resolve base path: %w", err)
	}
	joined := filepath.Join(base, userPath)
	absJoined, err := filepath.Abs(joined)
	if err != nil {
		return "", fmt.Errorf("resolve joined path: %w", err)
	}
	// Ensure the resolved path is within the base directory.
	// Append os.PathSeparator so that "/foobar" doesn't match base "/foo".
	if !strings.HasPrefix(absJoined, absBase+string(os.PathSeparator)) && absJoined != absBase {
		return "", fmt.Errorf("path %q escapes base directory", userPath)
	}
	return absJoined, nil
}

func (fs *FileSystem) objectPath(bucket, key string) (string, error) {
	base := fs.bucketBasePath(bucket)
	safe, err := safePath(base, filepath.Join(bucket, key))
	if err != nil {
		return "", err
	}
	return safe + safeExt, nil
}

// versionedObjectPath returns the storage path for a specific version of an object.
func (fs *FileSystem) versionedObjectPath(bucket, key, versionID string) (string, error) {
	versionedKey := key + ".v--" + versionID
	base := fs.bucketBasePath(bucket)
	safe, err := safePath(base, filepath.Join(bucket, versionedKey))
	if err != nil {
		return "", err
	}
	return safe + safeExt, nil
}

func (fs *FileSystem) PutObject(bucket, key string, reader io.Reader) (int64, string, error) {
	objPath, err := fs.objectPath(bucket, key)
	if err != nil {
		return 0, "", fmt.Errorf("resolve object path: %w", err)
	}

	// Ensure parent directory exists
	if err := os.MkdirAll(filepath.Dir(objPath), 0700); err != nil {
		return 0, "", fmt.Errorf("create parent dir: %w", err)
	}

	// Write to temp file first for atomic operation
	tmpFile, err := os.CreateTemp(filepath.Dir(objPath), ".tmp-*")
	if err != nil {
		return 0, "", fmt.Errorf("create temp file: %w", err)
	}
	tmpPath := tmpFile.Name()

	hash := md5.New()
	writer := io.MultiWriter(tmpFile, hash)

	size, err := io.Copy(writer, reader)
	if err != nil {
		tmpFile.Close()
		os.Remove(tmpPath)
		return 0, "", fmt.Errorf("write object: %w", err)
	}
	tmpFile.Close()

	// Atomic rename
	if err := os.Rename(tmpPath, objPath); err != nil {
		os.Remove(tmpPath)
		return 0, "", fmt.Errorf("rename temp file: %w", err)
	}

	etag := fmt.Sprintf("\"%x\"", hash.Sum(nil))
	return size, etag, nil
}

func (fs *FileSystem) GetObject(bucket, key string) (io.ReadCloser, error) {
	objPath, err := fs.objectPath(bucket, key)
	if err != nil {
		return nil, fmt.Errorf("resolve object path: %w", err)
	}

	// Open with O_NOFOLLOW to prevent symlink attacks (TOCTOU-safe).
	f, err := os.OpenFile(objPath, os.O_RDONLY|openNoFollow, 0)
	if err != nil {
		return nil, err
	}

	// Verify the opened file is a regular file (not symlink, device, etc.)
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		f.Close()
		return nil, fmt.Errorf("not a regular file")
	}

	return f, nil
}

func (fs *FileSystem) DeleteObject(bucket, key string) error {
	objPath, err := fs.objectPath(bucket, key)
	if err != nil {
		return fmt.Errorf("resolve object path: %w", err)
	}
	err = os.Remove(objPath)
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

func (fs *FileSystem) ObjectExists(bucket, key string) bool {
	objPath, err := fs.objectPath(bucket, key)
	if err != nil {
		return false
	}
	info, err := os.Lstat(objPath)
	if err != nil {
		return false
	}
	return info.Mode().IsRegular()
}

func (fs *FileSystem) CreateBucketDir(bucket string) error {
	base := fs.bucketBasePath(bucket)
	dir, err := safePath(base, bucket)
	if err != nil {
		return fmt.Errorf("invalid bucket path: %w", err)
	}
	return os.MkdirAll(dir, 0700)
}

func (fs *FileSystem) DeleteBucketDir(bucket string) error {
	base := fs.bucketBasePath(bucket)
	dir, err := safePath(base, bucket)
	if err != nil {
		return fmt.Errorf("invalid bucket path: %w", err)
	}
	err = os.RemoveAll(dir)
	// Also nuke the sibling multipart staging tree so abandoned parts don't linger.
	if mpDir, mpErr := fs.multipartBucketDir(bucket); mpErr == nil {
		os.RemoveAll(mpDir)
	}
	fs.RemoveBucketDir(bucket)
	return err
}

// CleanEmptyParents removes empty parent directories after a file is deleted,
// walking up from the file's directory until reaching the bucket root.
func (fs *FileSystem) CleanEmptyParents(bucket, key string) {
	base := fs.bucketBasePath(bucket)
	bucketRoot, err := safePath(base, bucket)
	if err != nil {
		return
	}

	// Start from the directory containing the deleted file
	objPath := filepath.Join(base, bucket, key)
	dir := filepath.Dir(objPath)

	for dir != bucketRoot && len(dir) > len(bucketRoot) {
		entries, err := os.ReadDir(dir)
		if err != nil || len(entries) > 0 {
			break // not empty or can't read — stop
		}
		os.Remove(dir) // remove empty dir
		dir = filepath.Dir(dir)
	}
}

// DeletePrefix removes the directory for a given prefix (folder) inside a bucket.
func (fs *FileSystem) DeletePrefix(bucket, prefix string) error {
	base := fs.bucketBasePath(bucket)
	dir, err := safePath(base, filepath.Join(bucket, prefix))
	if err != nil {
		return fmt.Errorf("invalid prefix path: %w", err)
	}
	return os.RemoveAll(dir)
}

// validUUID matches a standard UUID format.
var validUUID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// multipartDir returns the staging directory for a multipart upload. Parts are
// stored as a sibling directory of the bucket data directory under the bucket's
// effective base path: <bucketBase>/.<bucket>-multipart/<uploadID>/. This keeps
// multipart traffic on the same volume the bucket is configured to use.
func (fs *FileSystem) multipartDir(bucket, uploadID string) (string, error) {
	if bucket == "" {
		return "", fmt.Errorf("bucket is required")
	}
	if !validUUID.MatchString(uploadID) {
		return "", fmt.Errorf("invalid upload ID format")
	}
	base := fs.bucketBasePath(bucket)
	dir, err := safePath(base, filepath.Join("."+bucket+"-multipart", uploadID))
	if err != nil {
		return "", fmt.Errorf("invalid multipart path: %w", err)
	}
	return dir, nil
}

// multipartBucketDir returns the per-bucket staging root (without an uploadID).
// Used for bulk cleanup when a bucket is being deleted.
func (fs *FileSystem) multipartBucketDir(bucket string) (string, error) {
	if bucket == "" {
		return "", fmt.Errorf("bucket is required")
	}
	base := fs.bucketBasePath(bucket)
	dir, err := safePath(base, "."+bucket+"-multipart")
	if err != nil {
		return "", fmt.Errorf("invalid multipart path: %w", err)
	}
	return dir, nil
}

func (fs *FileSystem) PutMultipartPart(bucket, uploadID string, partNumber int, reader io.Reader) (int64, string, error) {
	dir, err := fs.multipartDir(bucket, uploadID)
	if err != nil {
		return 0, "", err
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return 0, "", err
	}

	partPath := filepath.Join(dir, strconv.Itoa(partNumber)+safeExt)

	// Write to temp file first for atomic operation
	tmpFile, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return 0, "", err
	}
	tmpPath := tmpFile.Name()

	hash := md5.New()
	writer := io.MultiWriter(tmpFile, hash)

	size, err := io.Copy(writer, reader)
	if err != nil {
		tmpFile.Close()
		os.Remove(tmpPath)
		return 0, "", err
	}
	tmpFile.Close()

	// Atomic rename
	if err := os.Rename(tmpPath, partPath); err != nil {
		os.Remove(tmpPath)
		return 0, "", err
	}

	etag := fmt.Sprintf("\"%x\"", hash.Sum(nil))
	return size, etag, nil
}

func (fs *FileSystem) AssembleMultipartParts(bucket, key, uploadID string, partNumbers []int) (int64, string, error) {
	objPath, err := fs.objectPath(bucket, key)
	if err != nil {
		return 0, "", fmt.Errorf("resolve object path: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(objPath), 0700); err != nil {
		return 0, "", err
	}

	tmpFile, err := os.CreateTemp(filepath.Dir(objPath), ".tmp-*")
	if err != nil {
		return 0, "", err
	}
	tmpPath := tmpFile.Name()

	hash := md5.New()
	var totalSize int64
	dir, err := fs.multipartDir(bucket, uploadID)
	if err != nil {
		tmpFile.Close()
		os.Remove(tmpPath)
		return 0, "", err
	}

	for _, pn := range partNumbers {
		partPath := filepath.Join(dir, strconv.Itoa(pn)+safeExt)
		// Open with O_NOFOLLOW to prevent symlink attacks during assembly.
		pf, err := os.OpenFile(partPath, os.O_RDONLY|openNoFollow, 0)
		if err != nil {
			tmpFile.Close()
			os.Remove(tmpPath)
			return 0, "", fmt.Errorf("open part %d: %w", pn, err)
		}
		n, err := io.Copy(io.MultiWriter(tmpFile, hash), pf)
		pf.Close()
		if err != nil {
			tmpFile.Close()
			os.Remove(tmpPath)
			return 0, "", fmt.Errorf("copy part %d: %w", pn, err)
		}
		totalSize += n
	}
	tmpFile.Close()

	if err := os.Rename(tmpPath, objPath); err != nil {
		os.Remove(tmpPath)
		return 0, "", err
	}

	etag := fmt.Sprintf("\"%x-%d\"", hash.Sum(nil), len(partNumbers))
	return totalSize, etag, nil
}

func (fs *FileSystem) PutVersionedObject(bucket, key, versionID string, reader io.Reader) (int64, string, error) {
	objPath, err := fs.versionedObjectPath(bucket, key, versionID)
	if err != nil {
		return 0, "", fmt.Errorf("resolve versioned object path: %w", err)
	}

	if err := os.MkdirAll(filepath.Dir(objPath), 0700); err != nil {
		return 0, "", fmt.Errorf("create parent dir: %w", err)
	}

	tmpFile, err := os.CreateTemp(filepath.Dir(objPath), ".tmp-*")
	if err != nil {
		return 0, "", fmt.Errorf("create temp file: %w", err)
	}
	tmpPath := tmpFile.Name()

	hash := md5.New()
	writer := io.MultiWriter(tmpFile, hash)

	size, err := io.Copy(writer, reader)
	if err != nil {
		tmpFile.Close()
		os.Remove(tmpPath)
		return 0, "", fmt.Errorf("write object: %w", err)
	}
	tmpFile.Close()

	if err := os.Rename(tmpPath, objPath); err != nil {
		os.Remove(tmpPath)
		return 0, "", fmt.Errorf("rename temp file: %w", err)
	}

	etag := fmt.Sprintf("\"%x\"", hash.Sum(nil))
	return size, etag, nil
}

func (fs *FileSystem) GetVersionedObject(bucket, key, versionID string) (io.ReadCloser, error) {
	objPath, err := fs.versionedObjectPath(bucket, key, versionID)
	if err != nil {
		return nil, fmt.Errorf("resolve versioned object path: %w", err)
	}

	f, err := os.OpenFile(objPath, os.O_RDONLY|openNoFollow, 0)
	if err != nil {
		return nil, err
	}

	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		f.Close()
		return nil, fmt.Errorf("not a regular file")
	}

	return f, nil
}

func (fs *FileSystem) DeleteVersionedObject(bucket, key, versionID string) error {
	objPath, err := fs.versionedObjectPath(bucket, key, versionID)
	if err != nil {
		return fmt.Errorf("resolve versioned object path: %w", err)
	}
	err = os.Remove(objPath)
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

func (fs *FileSystem) AssembleMultipartPartsVersioned(bucket, key, versionID, uploadID string, partNumbers []int) (int64, string, error) {
	objPath, err := fs.versionedObjectPath(bucket, key, versionID)
	if err != nil {
		return 0, "", fmt.Errorf("resolve versioned object path: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(objPath), 0700); err != nil {
		return 0, "", err
	}

	tmpFile, err := os.CreateTemp(filepath.Dir(objPath), ".tmp-*")
	if err != nil {
		return 0, "", err
	}
	tmpPath := tmpFile.Name()

	hash := md5.New()
	var totalSize int64
	dir, err := fs.multipartDir(bucket, uploadID)
	if err != nil {
		tmpFile.Close()
		os.Remove(tmpPath)
		return 0, "", err
	}

	for _, pn := range partNumbers {
		partPath := filepath.Join(dir, strconv.Itoa(pn)+safeExt)
		pf, err := os.OpenFile(partPath, os.O_RDONLY|openNoFollow, 0)
		if err != nil {
			tmpFile.Close()
			os.Remove(tmpPath)
			return 0, "", fmt.Errorf("open part %d: %w", pn, err)
		}
		n, err := io.Copy(io.MultiWriter(tmpFile, hash), pf)
		pf.Close()
		if err != nil {
			tmpFile.Close()
			os.Remove(tmpPath)
			return 0, "", fmt.Errorf("copy part %d: %w", pn, err)
		}
		totalSize += n
	}
	tmpFile.Close()

	if err := os.Rename(tmpPath, objPath); err != nil {
		os.Remove(tmpPath)
		return 0, "", err
	}

	etag := fmt.Sprintf("\"%x-%d\"", hash.Sum(nil), len(partNumbers))
	return totalSize, etag, nil
}

func (fs *FileSystem) DeleteMultipartParts(bucket, uploadID string) error {
	dir, err := fs.multipartDir(bucket, uploadID)
	if err != nil {
		return err
	}
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	// Best-effort: remove the per-bucket staging root if it became empty.
	if parent, err := fs.multipartBucketDir(bucket); err == nil {
		if entries, err := os.ReadDir(parent); err == nil && len(entries) == 0 {
			os.Remove(parent)
		}
	}
	return nil
}

// DeleteAllMultipartForBucket removes the entire .<bucket>-multipart staging tree.
// Called when a bucket is being deleted so that orphan parts don't linger on disk.
func (fs *FileSystem) DeleteAllMultipartForBucket(bucket string) error {
	dir, err := fs.multipartBucketDir(bucket)
	if err != nil {
		return err
	}
	return os.RemoveAll(dir)
}
