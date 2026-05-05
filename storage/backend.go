package storage

import "io"

// Backend defines the storage interface for object data.
type Backend interface {
	// PutObject writes object data and returns the number of bytes written and the MD5 ETag.
	PutObject(bucket, key string, reader io.Reader) (int64, string, error)

	// GetObject returns a reader for the object data.
	GetObject(bucket, key string) (io.ReadCloser, error)

	// DeleteObject deletes the object data.
	DeleteObject(bucket, key string) error

	// ObjectExists checks if the object data file exists.
	ObjectExists(bucket, key string) bool

	// CreateBucketDir creates the bucket directory.
	CreateBucketDir(bucket string) error

	// DeleteBucketDir deletes the bucket directory (must be empty).
	DeleteBucketDir(bucket string) error

	// PutMultipartPart writes a multipart part. Parts are staged under the bucket's
	// effective base path (sibling to the bucket directory) so custom storage_dir
	// settings apply to multipart staging as well.
	PutMultipartPart(bucket, uploadID string, partNumber int, reader io.Reader) (int64, string, error)

	// AssembleMultipartParts combines parts into the final object. Returns total size and ETag.
	AssembleMultipartParts(bucket, key, uploadID string, partNumbers []int) (int64, string, error)

	// DeleteMultipartParts removes the staging directory for an upload.
	DeleteMultipartParts(bucket, uploadID string) error

	// Versioned object operations
	PutVersionedObject(bucket, key, versionID string, reader io.Reader) (int64, string, error)
	GetVersionedObject(bucket, key, versionID string) (io.ReadCloser, error)
	DeleteVersionedObject(bucket, key, versionID string) error
	AssembleMultipartPartsVersioned(bucket, key, versionID, uploadID string, partNumbers []int) (int64, string, error)
}
