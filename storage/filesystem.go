package storage

import (
	"bytes"
	"crypto/md5"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
)

const (
	safeExt    = ".cloodsys3ext"
	versionSep = ".v--"
	tmpPrefix  = ".tmp-"
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
	abs, err := filepath.Abs(rootDir)
	if err != nil {
		return nil, fmt.Errorf("resolve root dir: %w", err)
	}
	return &FileSystem{RootDir: abs, customDirs: make(map[string]string)}, nil
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

// BucketDir returns the custom base directory registered for a bucket ("" if none).
func (fs *FileSystem) BucketDir(bucket string) string {
	fs.mu.RLock()
	defer fs.mu.RUnlock()
	return fs.customDirs[bucket]
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

// validBucketDirName mirrors the S3 bucket naming rules enforced by the API so
// that a bucket name can never be "." / ".." or contain a path separator.
var validBucketDirName = regexp.MustCompile(`^[a-z0-9][a-z0-9\-]{1,61}[a-z0-9]$`)

// bucketRoot returns the absolute directory holding a bucket's objects.
func (fs *FileSystem) bucketRoot(bucket string) (string, error) {
	if !validBucketDirName.MatchString(bucket) {
		return "", fmt.Errorf("invalid bucket name %q", bucket)
	}
	base, err := filepath.Abs(fs.bucketBasePath(bucket))
	if err != nil {
		return "", fmt.Errorf("resolve base path: %w", err)
	}
	return filepath.Join(base, bucket), nil
}

// siblingDir returns <base>/.<bucket>-<suffix> for staging/cache trees.
func (fs *FileSystem) siblingDir(bucket, suffix string) (string, error) {
	if !validBucketDirName.MatchString(bucket) {
		return "", fmt.Errorf("invalid bucket name %q", bucket)
	}
	base, err := filepath.Abs(fs.bucketBasePath(bucket))
	if err != nil {
		return "", fmt.Errorf("resolve base path: %w", err)
	}
	return filepath.Join(base, "."+bucket+"-"+suffix), nil
}

// safeJoin joins rel under root and guarantees the result stays inside root.
func safeJoin(root, rel string) (string, error) {
	joined := filepath.Join(root, filepath.FromSlash(rel))
	if joined != root && !strings.HasPrefix(joined, root+string(os.PathSeparator)) {
		return "", fmt.Errorf("path %q escapes %q", rel, root)
	}
	return joined, nil
}

func (fs *FileSystem) objectPath(bucket, key string) (string, error) {
	root, err := fs.bucketRoot(bucket)
	if err != nil {
		return "", err
	}
	rel, err := EncodeKeyPath(key)
	if err != nil {
		return "", err
	}
	p, err := safeJoin(root, rel)
	if err != nil {
		return "", err
	}
	return p + safeExt, nil
}

// versionedObjectPath returns the storage path for a specific version of an object.
func (fs *FileSystem) versionedObjectPath(bucket, key, versionID string) (string, error) {
	if versionID == "" || strings.ContainsAny(versionID, "/\\\x00") || versionID == "." || versionID == ".." {
		return "", fmt.Errorf("invalid version id")
	}
	root, err := fs.bucketRoot(bucket)
	if err != nil {
		return "", err
	}
	rel, err := EncodeKeyPath(key)
	if err != nil {
		return "", err
	}
	p, err := safeJoin(root, rel)
	if err != nil {
		return "", err
	}
	return p + versionSep + versionID + safeExt, nil
}

// LegacyObjectPath returns the path an object was stored at before the
// segment-encoding layout (raw key joined under the bucket dir). Used by the
// one-time layout migration; returns "" when the legacy path equals the
// current path (no move needed) or cannot be resolved.
func (fs *FileSystem) LegacyObjectPath(bucket, key, versionID string) (current, legacy string) {
	var err error
	if versionID != "" && versionID != "null" {
		current, err = fs.versionedObjectPath(bucket, key, versionID)
	} else {
		current, err = fs.objectPath(bucket, key)
	}
	if err != nil {
		return "", ""
	}
	root, err := fs.bucketRoot(bucket)
	if err != nil {
		return "", ""
	}
	name := key
	if versionID != "" && versionID != "null" {
		name = key + versionSep + versionID
	}
	legacy = filepath.Join(root, filepath.FromSlash(name)) + safeExt
	if legacy == current || !strings.HasPrefix(legacy, root+string(os.PathSeparator)) {
		return current, ""
	}
	return current, legacy
}

// MoveLegacyObject renames a legacy-layout file to the current layout path.
// It is a no-op when the legacy file does not exist or the current one already does.
func (fs *FileSystem) MoveLegacyObject(current, legacy string) (bool, error) {
	if legacy == "" {
		return false, nil
	}
	if _, err := os.Lstat(current); err == nil {
		return false, nil
	}
	fi, err := os.Lstat(legacy)
	if err != nil || !fi.Mode().IsRegular() {
		return false, nil
	}
	if err := os.MkdirAll(filepath.Dir(current), 0700); err != nil {
		return false, err
	}
	if err := os.Rename(legacy, current); err != nil {
		return false, err
	}
	return true, nil
}

// writeAtomic streams reader into a temp file next to dst, enforces opts,
// fsyncs, and renames into place. Returns size and the raw MD5 digest.
func writeAtomic(dst string, reader io.Reader, opts PutOptions) (int64, []byte, error) {
	dir := filepath.Dir(dst)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return 0, nil, fmt.Errorf("create parent dir: %w", err)
	}
	tmpFile, err := os.CreateTemp(dir, tmpPrefix+"*")
	if err != nil {
		return 0, nil, fmt.Errorf("create temp file: %w", err)
	}
	tmpPath := tmpFile.Name()
	cleanup := func() {
		tmpFile.Close()
		os.Remove(tmpPath)
	}

	src := reader
	if opts.MaxSize > 0 {
		src = io.LimitReader(reader, opts.MaxSize+1)
	}
	hash := md5.New()
	size, err := io.Copy(io.MultiWriter(tmpFile, hash), src)
	if err != nil {
		cleanup()
		return 0, nil, fmt.Errorf("write object: %w", err)
	}
	if opts.MaxSize > 0 && size > opts.MaxSize {
		cleanup()
		return 0, nil, ErrTooLarge
	}
	if err := tmpFile.Sync(); err != nil {
		cleanup()
		return 0, nil, fmt.Errorf("fsync: %w", err)
	}
	if err := tmpFile.Close(); err != nil {
		os.Remove(tmpPath)
		return 0, nil, fmt.Errorf("close temp file: %w", err)
	}
	sum := hash.Sum(nil)
	if opts.Verify != nil {
		if err := opts.Verify(size, sum); err != nil {
			os.Remove(tmpPath)
			return 0, nil, err
		}
	}
	if err := os.Rename(tmpPath, dst); err != nil {
		os.Remove(tmpPath)
		return 0, nil, fmt.Errorf("rename temp file: %w", err)
	}
	syncDir(dir)
	return size, sum, nil
}

// syncDir fsyncs a directory so a rename survives power loss (best effort;
// not supported on every platform/filesystem).
func syncDir(dir string) {
	d, err := os.Open(dir)
	if err != nil {
		return
	}
	d.Sync()
	d.Close()
}

func quoteETag(sum []byte) string {
	return "\"" + hex.EncodeToString(sum) + "\""
}

func (fs *FileSystem) PutObject(bucket, key string, reader io.Reader, opts PutOptions) (int64, string, error) {
	objPath, err := fs.objectPath(bucket, key)
	if err != nil {
		return 0, "", fmt.Errorf("resolve object path: %w", err)
	}
	size, sum, err := writeAtomic(objPath, reader, opts)
	if err != nil {
		return 0, "", err
	}
	return size, quoteETag(sum), nil
}

func openRegular(path string) (io.ReadCloser, error) {
	// Open with O_NOFOLLOW to prevent symlink attacks (TOCTOU-safe).
	f, err := os.OpenFile(path, os.O_RDONLY|openNoFollow, 0)
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

func (fs *FileSystem) GetObject(bucket, key string) (io.ReadCloser, error) {
	objPath, err := fs.objectPath(bucket, key)
	if err != nil {
		return nil, fmt.Errorf("resolve object path: %w", err)
	}
	return openRegular(objPath)
}

func (fs *FileSystem) DeleteObject(bucket, key string) error {
	objPath, err := fs.objectPath(bucket, key)
	if err != nil {
		return fmt.Errorf("resolve object path: %w", err)
	}
	err = os.Remove(objPath)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	fs.cleanEmptyParents(bucket, objPath)
	return nil
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
	dir, err := fs.bucketRoot(bucket)
	if err != nil {
		return fmt.Errorf("invalid bucket path: %w", err)
	}
	return os.MkdirAll(dir, 0700)
}

func (fs *FileSystem) DeleteBucketDir(bucket string) error {
	dir, err := fs.bucketRoot(bucket)
	if err != nil {
		return fmt.Errorf("invalid bucket path: %w", err)
	}
	err = os.RemoveAll(dir)
	// Also nuke the sibling multipart staging tree so abandoned parts don't linger.
	if mpDir, mpErr := fs.siblingDir(bucket, "multipart"); mpErr == nil {
		os.RemoveAll(mpDir)
	}
	// And the sibling variant cache tree (resized/optimized derivatives).
	if cacheDir, cErr := fs.siblingDir(bucket, "cache"); cErr == nil {
		os.RemoveAll(cacheDir)
	}
	fs.RemoveBucketDir(bucket)
	return err
}

// cleanEmptyParents removes empty parent directories after a file is deleted,
// walking up from the file's directory until reaching the bucket root.
// Errors (including races with concurrent writers creating the directory
// again) are ignored: a leftover empty directory is harmless.
func (fs *FileSystem) cleanEmptyParents(bucket, objPath string) {
	root, err := fs.bucketRoot(bucket)
	if err != nil {
		return
	}
	dir := filepath.Dir(objPath)
	for len(dir) > len(root) && strings.HasPrefix(dir, root+string(os.PathSeparator)) {
		if err := os.Remove(dir); err != nil {
			break // not empty, in use, or already gone — stop
		}
		dir = filepath.Dir(dir)
	}
}

// CleanEmptyParents is kept for callers that delete files themselves.
func (fs *FileSystem) CleanEmptyParents(bucket, key string) {
	if p, err := fs.objectPath(bucket, key); err == nil {
		fs.cleanEmptyParents(bucket, p)
	}
}

// DeletePrefix removes the directory tree for a folder prefix (must end with "/").
// Prefixes that do not denote a directory are ignored (nothing to remove: the
// per-object deletes already handled the files).
func (fs *FileSystem) DeletePrefix(bucket, prefix string) error {
	if prefix == "" || !strings.HasSuffix(prefix, "/") {
		return nil
	}
	root, err := fs.bucketRoot(bucket)
	if err != nil {
		return fmt.Errorf("invalid bucket path: %w", err)
	}
	// Encode every complete segment; the trailing "/" yields an empty last
	// segment that we drop (it is the directory itself).
	rel, err := EncodeKeyPath(strings.TrimSuffix(prefix, "/"))
	if err != nil {
		return fmt.Errorf("invalid prefix: %w", err)
	}
	dir, err := safeJoin(root, rel)
	if err != nil {
		return fmt.Errorf("invalid prefix path: %w", err)
	}
	if dir == root {
		return fmt.Errorf("refusing to remove bucket root")
	}
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	fs.cleanEmptyParents(bucket, filepath.Join(dir, "x"))
	return nil
}

// validUUID matches a standard UUID format.
var validUUID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// multipartDir returns the staging directory for a multipart upload. Parts are
// stored as a sibling directory of the bucket data directory under the bucket's
// effective base path: <bucketBase>/.<bucket>-multipart/<uploadID>/. This keeps
// multipart traffic on the same volume the bucket is configured to use.
func (fs *FileSystem) multipartDir(bucket, uploadID string) (string, error) {
	if !validUUID.MatchString(uploadID) {
		return "", fmt.Errorf("invalid upload ID format")
	}
	base, err := fs.siblingDir(bucket, "multipart")
	if err != nil {
		return "", err
	}
	return filepath.Join(base, uploadID), nil
}

func partPath(dir string, partNumber int) (string, error) {
	if partNumber < 1 || partNumber > 10000 {
		return "", fmt.Errorf("invalid part number")
	}
	return filepath.Join(dir, strconv.Itoa(partNumber)+safeExt), nil
}

func (fs *FileSystem) PutMultipartPart(bucket, uploadID string, partNumber int, reader io.Reader, opts PutOptions) (int64, string, error) {
	dir, err := fs.multipartDir(bucket, uploadID)
	if err != nil {
		return 0, "", err
	}
	p, err := partPath(dir, partNumber)
	if err != nil {
		return 0, "", err
	}
	size, sum, err := writeAtomic(p, reader, opts)
	if err != nil {
		return 0, "", err
	}
	return size, quoteETag(sum), nil
}

func (fs *FileSystem) DeleteMultipartPart(bucket, uploadID string, partNumber int) error {
	dir, err := fs.multipartDir(bucket, uploadID)
	if err != nil {
		return err
	}
	p, err := partPath(dir, partNumber)
	if err != nil {
		return err
	}
	err = os.Remove(p)
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

// assemble concatenates the staged parts into dst and returns the S3
// multipart ETag: md5 of the concatenated binary part digests, dash, part count.
func (fs *FileSystem) assemble(dst, bucket, uploadID string, partNumbers []int, opts PutOptions) (int64, string, error) {
	dir, err := fs.multipartDir(bucket, uploadID)
	if err != nil {
		return 0, "", err
	}
	pr, pw := io.Pipe()
	digests := md5.New()
	go func() {
		var werr error
		for _, pn := range partNumbers {
			p, err := partPath(dir, pn)
			if err != nil {
				werr = err
				break
			}
			pf, err := openRegular(p)
			if err != nil {
				werr = fmt.Errorf("open part %d: %w", pn, err)
				break
			}
			partHash := md5.New()
			_, err = io.Copy(io.MultiWriter(pw, partHash), pf)
			pf.Close()
			if err != nil {
				werr = fmt.Errorf("copy part %d: %w", pn, err)
				break
			}
			digests.Write(partHash.Sum(nil))
		}
		pw.CloseWithError(werr)
	}()

	size, _, err := writeAtomic(dst, pr, opts)
	if err != nil {
		pr.CloseWithError(err)
		return 0, "", err
	}
	etag := fmt.Sprintf("\"%x-%d\"", digests.Sum(nil), len(partNumbers))
	return size, etag, nil
}

func (fs *FileSystem) AssembleMultipartParts(bucket, key, uploadID string, partNumbers []int, opts PutOptions) (int64, string, error) {
	objPath, err := fs.objectPath(bucket, key)
	if err != nil {
		return 0, "", fmt.Errorf("resolve object path: %w", err)
	}
	return fs.assemble(objPath, bucket, uploadID, partNumbers, opts)
}

func (fs *FileSystem) AssembleMultipartPartsVersioned(bucket, key, versionID, uploadID string, partNumbers []int, opts PutOptions) (int64, string, error) {
	objPath, err := fs.versionedObjectPath(bucket, key, versionID)
	if err != nil {
		return 0, "", fmt.Errorf("resolve versioned object path: %w", err)
	}
	return fs.assemble(objPath, bucket, uploadID, partNumbers, opts)
}

func (fs *FileSystem) PutVersionedObject(bucket, key, versionID string, reader io.Reader, opts PutOptions) (int64, string, error) {
	objPath, err := fs.versionedObjectPath(bucket, key, versionID)
	if err != nil {
		return 0, "", fmt.Errorf("resolve versioned object path: %w", err)
	}
	size, sum, err := writeAtomic(objPath, reader, opts)
	if err != nil {
		return 0, "", err
	}
	return size, quoteETag(sum), nil
}

func (fs *FileSystem) GetVersionedObject(bucket, key, versionID string) (io.ReadCloser, error) {
	objPath, err := fs.versionedObjectPath(bucket, key, versionID)
	if err != nil {
		return nil, fmt.Errorf("resolve versioned object path: %w", err)
	}
	return openRegular(objPath)
}

func (fs *FileSystem) DeleteVersionedObject(bucket, key, versionID string) error {
	objPath, err := fs.versionedObjectPath(bucket, key, versionID)
	if err != nil {
		return fmt.Errorf("resolve versioned object path: %w", err)
	}
	err = os.Remove(objPath)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	fs.cleanEmptyParents(bucket, objPath)
	return nil
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
	if parent, err := fs.siblingDir(bucket, "multipart"); err == nil {
		os.Remove(parent) // fails (harmlessly) when not empty
	}
	return nil
}

// DeleteAllMultipartForBucket removes the entire .<bucket>-multipart staging tree.
// Called when a bucket is being deleted so that orphan parts don't linger on disk.
func (fs *FileSystem) DeleteAllMultipartForBucket(bucket string) error {
	dir, err := fs.siblingDir(bucket, "multipart")
	if err != nil {
		return err
	}
	return os.RemoveAll(dir)
}

// --- Variant cache (resized / optimized image derivatives) ---
//
// Derivatives live in a sibling directory of the bucket data dir, under the
// bucket's effective base path: <bucketBase>/.<bucket>-cache/<keyHash>/<variantHash>.cloodsys3ext
// Grouping by keyHash (SHA-256 of the object key) lets DeleteVariantsForKey
// remove every derivative of an object with a single RemoveAll. The variant
// hash embeds the source etag/version, so a changed original yields new
// filenames and stale entries are pruned on object/bucket delete.

// VariantCacheKey builds the opaque cache identifier for a derivative. spec
// encodes the transform request (e.g. "w800h0mfq75" or "opt"). The returned
// value is "<keyHash>/<variantHash>" and is safe to pass to Get/PutVariant.
func VariantCacheKey(key, versionID, etag, spec string) string {
	keyHash := sha256.Sum256([]byte(key))
	vh := sha256.Sum256([]byte(key + "\x00" + versionID + "\x00" + etag + "\x00" + spec))
	return hex.EncodeToString(keyHash[:]) + "/" + hex.EncodeToString(vh[:])
}

// VariantCacheDir returns the absolute cache root for a bucket (for janitors).
func (fs *FileSystem) VariantCacheDir(bucket string) (string, error) {
	return fs.siblingDir(bucket, "cache")
}

// validCacheKey matches "<64 hex>/<64 hex>" as produced by VariantCacheKey.
var validCacheKey = regexp.MustCompile(`^[0-9a-f]{64}/[0-9a-f]{64}$`)

func (fs *FileSystem) variantPath(bucket, cacheKey string) (string, error) {
	if !validCacheKey.MatchString(cacheKey) {
		return "", fmt.Errorf("invalid cache key format")
	}
	base, err := fs.siblingDir(bucket, "cache")
	if err != nil {
		return "", err
	}
	return filepath.Join(base, filepath.FromSlash(cacheKey)) + safeExt, nil
}

// GetVariant returns a reader for a cached derivative plus its size, or
// (nil, 0, nil) on a cache miss.
func (fs *FileSystem) GetVariant(bucket, cacheKey string) (io.ReadCloser, int64, error) {
	p, err := fs.variantPath(bucket, cacheKey)
	if err != nil {
		return nil, 0, err
	}
	f, err := os.OpenFile(p, os.O_RDONLY|openNoFollow, 0)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, 0, nil // miss
		}
		return nil, 0, err
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, 0, err
	}
	if !fi.Mode().IsRegular() {
		f.Close()
		return nil, 0, fmt.Errorf("not a regular file")
	}
	return f, fi.Size(), nil
}

// PutVariant atomically writes a derivative into the cache (temp file + rename).
func (fs *FileSystem) PutVariant(bucket, cacheKey string, data []byte) error {
	p, err := fs.variantPath(bucket, cacheKey)
	if err != nil {
		return err
	}
	_, _, err = writeAtomic(p, bytes.NewReader(data), PutOptions{})
	return err
}

// DeleteVariantsForKey removes every cached derivative belonging to an object.
func (fs *FileSystem) DeleteVariantsForKey(bucket, key string) error {
	base, err := fs.siblingDir(bucket, "cache")
	if err != nil {
		return err
	}
	keyHash := sha256.Sum256([]byte(key))
	return os.RemoveAll(filepath.Join(base, hex.EncodeToString(keyHash[:])))
}

// DeleteAllVariantsForBucket removes the entire .<bucket>-cache tree.
func (fs *FileSystem) DeleteAllVariantsForBucket(bucket string) error {
	dir, err := fs.siblingDir(bucket, "cache")
	if err != nil {
		return err
	}
	return os.RemoveAll(dir)
}

// IsNotExist reports whether err means the object file is missing.
func IsNotExist(err error) bool {
	return errors.Is(err, os.ErrNotExist)
}
