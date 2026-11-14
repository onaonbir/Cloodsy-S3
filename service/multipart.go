package service

import (
	"errors"
	"io"
	"strings"

	"github.com/onaonbir/Cloodsy-S3/db"
	"github.com/onaonbir/Cloodsy-S3/storage"
)

// Multipart limits (S3).
const (
	MaxParts               = 10000
	MinPartSize      int64 = 5 * 1024 * 1024               // every part except the last
	MaxPartSize      int64 = 5 * 1024 * 1024 * 1024        // 5 GB
	MaxMultipartSize int64 = 5 * 1024 * 1024 * 1024 * 1024 // 5 TB assembled
)

var (
	ErrNoSuchUpload     = errors.New("no such upload")
	ErrInvalidPart      = errors.New("invalid part")
	ErrInvalidPartOrder = errors.New("invalid part order")
	ErrEntityTooSmall   = errors.New("part too small")
)

// CompletedPart is one entry of a CompleteMultipartUpload request.
type CompletedPart struct {
	PartNumber int
	ETag       string
}

// PutPart stages one part of a multipart upload and records it. opts.MaxSize
// defaults to MaxPartSize. It holds the upload's shared lock, so it never
// overlaps completion or abort of the same upload (parts of one upload still
// run in parallel), and fails with ErrNoSuchUpload when the upload is gone.
func (s *Objects) PutPart(b *db.Bucket, uploadID string, partNumber int, body io.Reader, opts storage.PutOptions) (*db.MultipartPart, error) {
	if opts.MaxSize <= 0 {
		opts.MaxSize = MaxPartSize
	}
	unlock := s.rlockUpload(uploadID)
	defer unlock()
	if err := s.checkUpload(b, uploadID); err != nil {
		return nil, err
	}
	size, etag, err := s.Store.PutMultipartPart(b.Name, uploadID, partNumber, body, opts)
	if err != nil {
		return nil, err
	}
	part := &db.MultipartPart{UploadID: uploadID, PartNumber: partNumber, Size: size, ETag: etag}
	if err := s.DB.PutMultipartPart(part); err != nil {
		s.Store.DeleteMultipartPart(b.Name, uploadID, partNumber)
		return nil, err
	}
	return part, nil
}

// CompleteMultipart validates the requested part list against the staged
// parts (S3 order: part order, then existence/ETag, then sizes), enforces
// quota, assembles the object and commits it like any other write. The caller
// must hold LockUpload(upload.ID) and have loaded upload for bucket b.
func (s *Objects) CompleteMultipart(b *db.Bucket, upload *db.MultipartUpload, key string, parts []CompletedPart) (*PutResult, error) {
	if len(parts) == 0 || len(parts) > MaxParts {
		return nil, ErrInvalidPart
	}
	dbParts, err := s.DB.ListMultipartParts(upload.ID)
	if err != nil {
		return nil, err
	}
	partMap := make(map[int]db.MultipartPart, len(dbParts))
	for _, p := range dbParts {
		partMap[p.PartNumber] = p
	}

	prev := 0
	for _, p := range parts {
		if p.PartNumber <= prev {
			return nil, ErrInvalidPartOrder
		}
		prev = p.PartNumber
	}
	partNumbers := make([]int, 0, len(parts))
	var expected int64
	for i, p := range parts {
		dbPart, ok := partMap[p.PartNumber]
		if !ok || strings.Trim(p.ETag, "\"") != strings.Trim(dbPart.ETag, "\"") {
			return nil, ErrInvalidPart
		}
		if i < len(parts)-1 && dbPart.Size < MinPartSize {
			return nil, ErrEntityTooSmall
		}
		expected += dbPart.Size
		if expected < 0 || expected > MaxMultipartSize {
			return nil, ErrTooLarge
		}
		partNumbers = append(partNumbers, p.PartNumber)
	}

	// Serialize with other writers of this key and check quota before the
	// expensive assembly.
	unlock := s.locks.lock(b.ID, key)
	defer unlock()

	if err := s.checkQuota(b, key, expected); err != nil {
		return nil, err
	}

	versionID := VersionIDForPut(b)
	opts := storage.PutOptions{
		MaxSize: MaxMultipartSize,
		Verify: func(size int64, _ []byte) error {
			return s.checkQuota(b, key, size)
		},
	}
	var total int64
	var etag string
	if IsRealVersion(versionID) {
		total, etag, err = s.Store.AssembleMultipartPartsVersioned(b.Name, key, versionID, upload.ID, partNumbers, opts)
	} else {
		total, etag, err = s.Store.AssembleMultipartParts(b.Name, key, upload.ID, partNumbers, opts)
	}
	if err != nil {
		return nil, err
	}

	res, err := s.Commit(b, key, versionID, total, etag, upload.ContentType, upload.Metadata, "s3:ObjectCreated:CompleteMultipartUpload")
	if err != nil {
		return nil, err
	}
	if err := s.Store.DeleteMultipartParts(b.Name, upload.ID); err != nil && s.Logger != nil {
		s.Logger.Error("failed to clean up multipart parts", "uploadId", upload.ID, "error", err)
	}
	s.DB.DeleteMultipartUpload(upload.ID)
	return res, nil
}

// checkUpload returns ErrNoSuchUpload unless uploadID is an incomplete upload
// of bucket b. Callers hold the upload lock, so the answer stays valid.
func (s *Objects) checkUpload(b *db.Bucket, uploadID string) error {
	u, err := s.DB.GetMultipartUpload(uploadID)
	if err != nil {
		return err
	}
	if u == nil || u.BucketID != b.ID {
		return ErrNoSuchUpload
	}
	return nil
}

// AbortMultipart discards the staged parts and the upload record. It waits
// for in-flight part uploads of the upload and returns ErrNoSuchUpload when
// the upload was already completed or aborted.
func (s *Objects) AbortMultipart(b *db.Bucket, uploadID string) error {
	unlock := s.LockUpload(uploadID)
	defer unlock()
	if err := s.checkUpload(b, uploadID); err != nil {
		return err
	}
	if err := s.Store.DeleteMultipartParts(b.Name, uploadID); err != nil {
		return err
	}
	_, err := s.DB.DeleteMultipartUpload(uploadID)
	return err
}
