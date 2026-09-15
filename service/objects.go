// Package service holds the object operations shared by every front end
// (S3 API, WebDAV, Admin API, lifecycle cleaner) so that versioning, quota,
// per-key locking, webhook events and image processing behave identically
// regardless of which door the request came through.
package service

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/onaonbir/Cloodsy-S3/config"
	"github.com/onaonbir/Cloodsy-S3/db"
	imageutil "github.com/onaonbir/Cloodsy-S3/image"
	"github.com/onaonbir/Cloodsy-S3/storage"
	"github.com/onaonbir/Cloodsy-S3/webhook"
)

var (
	ErrQuotaExceeded = errors.New("bucket quota exceeded")
	ErrTooLarge      = storage.ErrTooLarge
	ErrNoSuchKey     = errors.New("no such key")
	ErrNoSuchVersion = errors.New("no such version")
)

// Objects is the shared object service.
type Objects struct {
	DB         *db.DB
	Store      storage.Backend
	Dispatcher *webhook.Dispatcher
	Image      *imageutil.Worker // optional
	ImageCfg   config.ImageConfig
	Logger     *slog.Logger

	locks       keyLocks
	uploadLocks keyLocks
}

// New creates a service bound to the given dependencies.
func New(database *db.DB, store storage.Backend, logger *slog.Logger) *Objects {
	return &Objects{DB: database, Store: store, Logger: logger}
}

// --- per-key striped locks -------------------------------------------------

type keyLocks struct {
	stripes [256]sync.Mutex
}

func (k *keyLocks) lock(bucketID int64, key string) func() {
	h := fnv.New32a()
	fmt.Fprintf(h, "%d/%s", bucketID, key)
	m := &k.stripes[h.Sum32()%uint32(len(k.stripes))]
	m.Lock()
	return m.Unlock
}

// Lock acquires the per-key lock for callers that run multi-step operations
// (e.g. multipart completion) outside Put/Delete.
func (s *Objects) Lock(bucketID int64, key string) func() { return s.locks.lock(bucketID, key) }

// LockUpload serializes operations on one multipart upload. It uses a separate
// lock set so it can be held together with Lock without deadlocking.
func (s *Objects) LockUpload(uploadID string) func() { return s.uploadLocks.lock(0, uploadID) }

// --- versioning helpers ----------------------------------------------------

// NewVersionID creates a unique, time-ordered version id.
func NewVersionID() string {
	b := make([]byte, 3)
	rand.Read(b)
	return fmt.Sprintf("%d-%s", time.Now().UnixNano(), hex.EncodeToString(b))
}

// IsRealVersion reports whether a version id denotes a versioned file on disk
// (as opposed to the unversioned/"null" slot).
func IsRealVersion(v string) bool { return v != "" && v != "null" }

// VersionIDForPut decides the version id a new write receives.
func VersionIDForPut(bucket *db.Bucket) string {
	switch bucket.Versioning {
	case "Enabled":
		return NewVersionID()
	case "Suspended":
		return "null"
	default:
		return ""
	}
}

// Open returns a reader for the bytes of a specific object row.
func (s *Objects) Open(bucket *db.Bucket, meta *db.ObjectMeta) (io.ReadCloser, error) {
	if IsRealVersion(meta.VersionID) {
		return s.Store.GetVersionedObject(bucket.Name, meta.Key, meta.VersionID)
	}
	return s.Store.GetObject(bucket.Name, meta.Key)
}

// --- Put -------------------------------------------------------------------

// PutInput describes a single-object write.
type PutInput struct {
	Bucket       *db.Bucket
	Key          string
	Body         io.Reader
	ContentType  string
	Metadata     string // JSON (x-amz-meta-* plus "$"-prefixed system headers)
	DeclaredSize int64  // -1 when unknown; used for early quota/size rejection
	MaxSize      int64  // hard cap enforced while streaming (0 = none)
	// Verify runs after the bytes are on disk and before they become visible
	// (checksums, etc.). Errors abort the write; the old object stays intact.
	Verify    func(size int64, md5sum []byte) error
	EventType string // webhook event, default s3:ObjectCreated:Put
	// SkipImage disables the optimizer for this write.
	SkipImage bool
}

// PutResult describes the committed object.
type PutResult struct {
	Size         int64
	ETag         string
	VersionID    string
	LastModified time.Time
}

// Put writes an object honoring bucket versioning and quota. All checks that
// can fail run before the previous object's bytes are replaced.
func (s *Objects) Put(in PutInput) (*PutResult, error) {
	b := in.Bucket
	if in.DeclaredSize >= 0 {
		if in.MaxSize > 0 && in.DeclaredSize > in.MaxSize {
			return nil, ErrTooLarge
		}
		if err := s.checkQuota(b, in.Key, in.DeclaredSize); err != nil {
			return nil, err
		}
	}

	unlock := s.locks.lock(b.ID, in.Key)
	defer unlock()

	versionID := VersionIDForPut(b)
	opts := storage.PutOptions{
		MaxSize: in.MaxSize,
		Verify: func(size int64, sum []byte) error {
			if err := s.checkQuota(b, in.Key, size); err != nil {
				return err
			}
			if in.Verify != nil {
				return in.Verify(size, sum)
			}
			return nil
		},
	}

	var size int64
	var etag string
	var err error
	if IsRealVersion(versionID) {
		size, etag, err = s.Store.PutVersionedObject(b.Name, in.Key, versionID, in.Body, opts)
	} else {
		size, etag, err = s.Store.PutObject(b.Name, in.Key, in.Body, opts)
	}
	if err != nil {
		return nil, err
	}

	now := time.Now().UTC()
	meta := &db.ObjectMeta{
		BucketID:     b.ID,
		Key:          in.Key,
		Size:         size,
		ETag:         etag,
		ContentType:  in.ContentType,
		LastModified: now,
		Metadata:     in.Metadata,
		VersionID:    versionID,
		IsLatest:     true,
	}
	if meta.ContentType == "" {
		meta.ContentType = "application/octet-stream"
	}
	if meta.Metadata == "" {
		meta.Metadata = "{}"
	}
	if err := s.DB.PutObjectMetaVersioned(meta); err != nil {
		// Retry once: a transient SQLITE_BUSY must not orphan bytes that are
		// already the live copy for an unversioned key.
		if err2 := s.DB.PutObjectMetaVersioned(meta); err2 != nil {
			if IsRealVersion(versionID) {
				s.Store.DeleteVersionedObject(b.Name, in.Key, versionID)
			}
			return nil, fmt.Errorf("save metadata: %w", err2)
		}
	}

	// Derived images of the previous content are stale now.
	s.Store.DeleteVariantsForKey(b.Name, in.Key)

	s.emit(b.Name, orDefault(in.EventType, "s3:ObjectCreated:Put"), in.Key, size, etag, versionID, now)
	if !in.SkipImage {
		s.queueImage(b.Name, in.Key, versionID, etag, meta.ContentType, size)
	}
	return &PutResult{Size: size, ETag: etag, VersionID: versionID, LastModified: now}, nil
}

// Commit records metadata for bytes that were already assembled on disk by
// the caller (multipart completion) and fires the usual side effects.
func (s *Objects) Commit(b *db.Bucket, key, versionID string, size int64, etag, contentType, metadata, eventType string) (*PutResult, error) {
	now := time.Now().UTC()
	meta := &db.ObjectMeta{
		BucketID: b.ID, Key: key, Size: size, ETag: etag, ContentType: contentType,
		LastModified: now, Metadata: metadata, VersionID: versionID, IsLatest: true,
	}
	if meta.ContentType == "" {
		meta.ContentType = "application/octet-stream"
	}
	if meta.Metadata == "" {
		meta.Metadata = "{}"
	}
	if err := s.DB.PutObjectMetaVersioned(meta); err != nil {
		if err2 := s.DB.PutObjectMetaVersioned(meta); err2 != nil {
			return nil, fmt.Errorf("save metadata: %w", err2)
		}
	}
	s.Store.DeleteVariantsForKey(b.Name, key)
	s.emit(b.Name, orDefault(eventType, "s3:ObjectCreated:CompleteMultipartUpload"), key, size, etag, versionID, now)
	s.queueImage(b.Name, key, versionID, etag, meta.ContentType, size)
	return &PutResult{Size: size, ETag: etag, VersionID: versionID, LastModified: now}, nil
}

// checkQuota verifies the bucket can absorb additional bytes for key. The
// current size of the key's latest version is credited back for unversioned
// and suspended buckets (an overwrite replaces the bytes).
func (s *Objects) checkQuota(b *db.Bucket, key string, additional int64) error {
	if b.QuotaBytes <= 0 {
		return nil
	}
	usage, err := s.DB.GetBucketUsage(b.ID)
	if err != nil {
		return fmt.Errorf("get bucket usage: %w", err)
	}
	if b.Versioning != "Enabled" {
		if cur, err := s.DB.GetObjectMeta(b.ID, key); err == nil && cur != nil && !cur.IsDeleteMarker {
			usage -= cur.Size
		}
	}
	if usage+additional > b.QuotaBytes {
		return ErrQuotaExceeded
	}
	return nil
}

// --- Delete ----------------------------------------------------------------

// DeleteResult reports what a delete did, in S3 terms.
type DeleteResult struct {
	// DeleteMarker is true when a delete marker was created or removed.
	DeleteMarker bool
	// VersionID is the version affected (the new marker's id, or the removed version).
	VersionID string
}

// Delete removes an object or a specific version following S3 semantics:
//   - versionID == "": unversioned bucket → hard delete; Enabled → new delete
//     marker; Suspended → remove the null version and add a null delete marker.
//   - versionID != "": permanently remove that version (or marker) and
//     promote the newest remaining version to latest.
func (s *Objects) Delete(b *db.Bucket, key, versionID string) (*DeleteResult, error) {
	unlock := s.locks.lock(b.ID, key)
	defer unlock()
	now := time.Now().UTC()

	if versionID != "" {
		deleted, _, err := s.DB.DeleteVersionAndPromote(b.ID, key, versionID)
		if err != nil {
			return nil, err
		}
		if deleted == nil {
			return nil, ErrNoSuchVersion
		}
		if !deleted.IsDeleteMarker {
			s.removeBytes(b.Name, key, deleted.VersionID)
		}
		s.Store.DeleteVariantsForKey(b.Name, key)
		s.emit(b.Name, "s3:ObjectRemoved:Delete", key, 0, "", versionID, now)
		return &DeleteResult{DeleteMarker: deleted.IsDeleteMarker, VersionID: versionID}, nil
	}

	switch b.Versioning {
	case "Enabled":
		markerID := NewVersionID()
		marker := &db.ObjectMeta{
			BucketID: b.ID, Key: key, LastModified: now, Metadata: "{}",
			VersionID: markerID, IsLatest: true, IsDeleteMarker: true,
		}
		if err := s.DB.PutObjectMetaVersioned(marker); err != nil {
			return nil, err
		}
		s.Store.DeleteVariantsForKey(b.Name, key)
		s.emit(b.Name, "s3:ObjectRemoved:DeleteMarkerCreated", key, 0, "", markerID, now)
		return &DeleteResult{DeleteMarker: true, VersionID: markerID}, nil

	case "Suspended":
		// Remove the null version (if any) and replace it with a null marker.
		for _, v := range []string{"null", ""} {
			deleted, _, err := s.DB.DeleteVersionAndPromote(b.ID, key, v)
			if err != nil {
				return nil, err
			}
			if deleted != nil && !deleted.IsDeleteMarker {
				s.Store.DeleteObject(b.Name, key)
			}
		}
		marker := &db.ObjectMeta{
			BucketID: b.ID, Key: key, LastModified: now, Metadata: "{}",
			VersionID: "null", IsLatest: true, IsDeleteMarker: true,
		}
		if err := s.DB.PutObjectMetaVersioned(marker); err != nil {
			return nil, err
		}
		s.Store.DeleteVariantsForKey(b.Name, key)
		s.emit(b.Name, "s3:ObjectRemoved:DeleteMarkerCreated", key, 0, "", "null", now)
		return &DeleteResult{DeleteMarker: true, VersionID: "null"}, nil

	default:
		versions, err := s.DB.ListVersionsForKey(b.ID, key)
		if err != nil {
			return nil, err
		}
		if err := s.DB.DeleteObjectMeta(b.ID, key); err != nil {
			return nil, err
		}
		for _, v := range versions {
			if !v.IsDeleteMarker {
				s.removeBytes(b.Name, key, v.VersionID)
			}
		}
		s.Store.DeleteVariantsForKey(b.Name, key)
		s.emit(b.Name, "s3:ObjectRemoved:Delete", key, 0, "", "", now)
		return &DeleteResult{}, nil
	}
}

// DeleteAllVersions permanently removes every version of a key (admin use).
func (s *Objects) DeleteAllVersions(b *db.Bucket, key string) error {
	unlock := s.locks.lock(b.ID, key)
	defer unlock()
	versions, err := s.DB.ListVersionsForKey(b.ID, key)
	if err != nil {
		return err
	}
	if err := s.DB.DeleteObjectMeta(b.ID, key); err != nil {
		return err
	}
	for _, v := range versions {
		if !v.IsDeleteMarker {
			s.removeBytes(b.Name, key, v.VersionID)
		}
	}
	s.Store.DeleteVariantsForKey(b.Name, key)
	s.emit(b.Name, "s3:ObjectRemoved:Delete", key, 0, "", "", time.Now().UTC())
	return nil
}

func (s *Objects) removeBytes(bucket, key, versionID string) {
	var err error
	if IsRealVersion(versionID) {
		err = s.Store.DeleteVersionedObject(bucket, key, versionID)
	} else {
		err = s.Store.DeleteObject(bucket, key)
	}
	if err != nil && s.Logger != nil {
		s.Logger.Error("failed to delete object data", "bucket", bucket, "key", key, "version", versionID, "error", err)
	}
}

// --- side effects ----------------------------------------------------------

func (s *Objects) emit(bucket, eventType, key string, size int64, etag, versionID string, ts time.Time) {
	if s.Dispatcher == nil {
		return
	}
	s.Dispatcher.Emit(webhook.Event{
		BucketName: bucket, EventType: eventType, Key: key, Size: size, ETag: etag, VersionID: versionID, Timestamp: ts,
	})
}

func (s *Objects) queueImage(bucket, key, versionID, etag, contentType string, size int64) {
	if s.Image == nil || !s.ImageCfg.Enabled || !imageutil.IsImageContentType(contentType) {
		return
	}
	if s.ImageCfg.MaxSourceBytes > 0 && size > s.ImageCfg.MaxSourceBytes {
		return
	}
	job := imageutil.Job{Bucket: bucket, Key: key, VersionID: versionID, ETag: etag, ContentType: contentType}
	if size <= s.ImageCfg.SyncMaxBytes {
		s.Image.Process(job)
	} else {
		s.Image.Enqueue(job)
	}
}

func orDefault(v, d string) string {
	if v == "" {
		return d
	}
	return v
}

// --- metadata JSON helpers -------------------------------------------------

// System header keys stored inside the metadata JSON, prefixed with "$" so
// they never collide with user metadata (x-amz-meta-* keys are lower-case
// tokens that cannot contain "$").
const (
	MetaCacheControl       = "$cache-control"
	MetaContentDisposition = "$content-disposition"
	MetaContentEncoding    = "$content-encoding"
	MetaContentLanguage    = "$content-language"
	MetaExpires            = "$expires"
)

// IsSystemMetaKey reports whether a metadata key is an internal header slot.
func IsSystemMetaKey(k string) bool { return strings.HasPrefix(k, "$") }

// ctx is unused for now but keeps the door open for cancellation plumbing.
var _ = context.Background
