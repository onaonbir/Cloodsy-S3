package storage

import (
	"errors"
	"io"
)

// ErrTooLarge is returned by Put* operations when the body exceeds PutOptions.MaxSize.
// The temporary file is removed and nothing is renamed into place.
var ErrTooLarge = errors.New("object exceeds maximum allowed size")

// PutOptions controls how a write is validated before it becomes visible.
type PutOptions struct {
	// MaxSize aborts the write (ErrTooLarge) once more than MaxSize bytes have
	// been read. 0 means unlimited.
	MaxSize int64
	// Verify, when set, is called with the final size and raw MD5 digest after
	// the data is fully written and fsynced but BEFORE the temp file is renamed
	// over the destination. Returning an error aborts the write and leaves any
	// existing object untouched.
	Verify func(size int64, md5sum []byte) error
}

// Backend defines the storage interface for object data.
type Backend interface {
	// PutObject writes object data and returns the number of bytes written and the quoted MD5 ETag.
	PutObject(bucket, key string, reader io.Reader, opts PutOptions) (int64, string, error)

	// GetObject returns a reader for the object data.
	GetObject(bucket, key string) (io.ReadCloser, error)

	// DeleteObject deletes the object data.
	DeleteObject(bucket, key string) error

	// ObjectExists checks if the object data file exists.
	ObjectExists(bucket, key string) bool

	// CreateBucketDir creates the bucket directory.
	CreateBucketDir(bucket string) error

	// DeleteBucketDir deletes the bucket directory and its sibling staging/cache trees.
	DeleteBucketDir(bucket string) error

	// DeletePrefix removes the on-disk directory for a "folder" prefix (one that
	// ends with "/") inside a bucket. Callers must have removed the metadata rows
	// under that prefix first.
	DeletePrefix(bucket, prefix string) error

	// PutMultipartPart writes a multipart part. Parts are staged under the bucket's
	// effective base path (sibling to the bucket directory) so custom storage_dir
	// settings apply to multipart staging as well.
	PutMultipartPart(bucket, uploadID string, partNumber int, reader io.Reader, opts PutOptions) (int64, string, error)

	// DeleteMultipartPart removes a single staged part.
	DeleteMultipartPart(bucket, uploadID string, partNumber int) error

	// AssembleMultipartParts combines parts into the final object. Returns total
	// size and the S3-style multipart ETag ("md5-of-part-md5s-N").
	AssembleMultipartParts(bucket, key, uploadID string, partNumbers []int, opts PutOptions) (int64, string, error)

	// DeleteMultipartParts removes the staging directory for an upload.
	DeleteMultipartParts(bucket, uploadID string) error

	// Versioned object operations
	PutVersionedObject(bucket, key, versionID string, reader io.Reader, opts PutOptions) (int64, string, error)
	GetVersionedObject(bucket, key, versionID string) (io.ReadCloser, error)
	DeleteVersionedObject(bucket, key, versionID string) error
	AssembleMultipartPartsVersioned(bucket, key, versionID, uploadID string, partNumbers []int, opts PutOptions) (int64, string, error)

	// Variant cache operations — derived (resized/optimized) images are cached
	// in a sibling .<bucket>-cache tree so the original object is never touched.
	// cacheKey is an opaque content-addressed identifier (includes the object's
	// etag/version) so cached entries self-invalidate when the original changes.
	GetVariant(bucket, cacheKey string) (io.ReadCloser, int64, error)
	PutVariant(bucket, cacheKey string, data []byte) error
	DeleteVariantsForKey(bucket, key string) error
	DeleteAllVariantsForBucket(bucket string) error
}
