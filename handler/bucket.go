package handler

import (
	"net/http"

	"github.com/onaonbir/Cloodsy-S3/s3err"
	"github.com/onaonbir/Cloodsy-S3/s3xml"
)

// ListBuckets handles GET /
func (h *Handler) ListBuckets(w http.ResponseWriter, r *http.Request) {
	cred, ok := h.authenticateRequest(w, r)
	if !ok {
		return
	}

	// Only list buckets this credential has access to
	bucketIDs, err := h.DB.GetBucketIDsForAccessKey(cred.AccessKey)
	if err != nil {
		h.Logger.Error("failed to get bucket IDs", "error", err)
		s3err.WriteError(w, r, s3err.ErrInternalError)
		return
	}

	buckets, err := h.DB.ListBucketsByIDs(bucketIDs)
	if err != nil {
		h.Logger.Error("failed to list buckets", "error", err)
		s3err.WriteError(w, r, s3err.ErrInternalError)
		return
	}

	result := s3xml.ListAllMyBucketsResult{
		Xmlns: "http://s3.amazonaws.com/doc/2006-03-01/",
		Owner: s3xml.Owner{
			ID:          "cloodsys3",
			DisplayName: "cloodsys3",
		},
	}

	for _, b := range buckets {
		result.Buckets.Bucket = append(result.Buckets.Bucket, s3xml.BucketInfo{
			Name:         b.Name,
			CreationDate: b.CreatedAt.UTC().Format("2006-01-02T15:04:05.000Z"),
		})
	}

	h.writeXML(w, http.StatusOK, result)
}

// CreateBucket handles PUT /<bucket>
func (h *Handler) CreateBucket(w http.ResponseWriter, r *http.Request) {
	cred, ok := h.authenticateRequest(w, r)
	if !ok {
		return
	}

	if !h.checkWriteAccess(w, r, cred) {
		return
	}

	bucketName, _ := getBucketAndKey(r)
	if bucketName == "" || !isValidBucketName(bucketName) {
		s3err.WriteError(w, r, s3err.ErrInvalidBucketName)
		return
	}

	// Check if bucket already exists
	existing, err := h.DB.GetBucket(bucketName)
	if err != nil {
		h.Logger.Error("db error", "error", err)
		s3err.WriteError(w, r, s3err.ErrInternalError)
		return
	}
	if existing != nil {
		if cred.BucketID == existing.ID {
			s3err.WriteError(w, r, s3err.ErrBucketAlreadyOwnedByYou)
		} else {
			s3err.WriteError(w, r, s3err.ErrBucketAlreadyExists)
		}
		return
	}

	// Create bucket in DB
	bucket, err := h.DB.CreateBucket(bucketName, "")
	if err != nil {
		h.Logger.Error("failed to create bucket", "error", err)
		s3err.WriteError(w, r, s3err.ErrInternalError)
		return
	}

	// Create storage directory
	if err := h.Storage.CreateBucketDir(bucketName); err != nil {
		h.Logger.Error("failed to create bucket dir", "error", err)
		// Rollback DB
		h.DB.DeleteBucket(bucketName)
		s3err.WriteError(w, r, s3err.ErrInternalError)
		return
	}

	_ = bucket
	w.Header().Set("Location", "/"+bucketName)
	w.WriteHeader(http.StatusOK)
}

// DeleteBucket handles DELETE /<bucket>
func (h *Handler) DeleteBucket(w http.ResponseWriter, r *http.Request) {
	cred, ok := h.authenticateRequest(w, r)
	if !ok {
		return
	}

	if !h.checkWriteAccess(w, r, cred) {
		return
	}

	bucketName, _ := getBucketAndKey(r)
	bucket, ok := h.checkBucketAccess(w, r, cred, bucketName)
	if !ok {
		return
	}

	// Check if bucket is empty
	hasObjects, err := h.DB.BucketHasObjects(bucket.ID)
	if err != nil {
		h.Logger.Error("db error", "error", err)
		s3err.WriteError(w, r, s3err.ErrInternalError)
		return
	}
	if hasObjects {
		s3err.WriteError(w, r, s3err.ErrBucketNotEmpty)
		return
	}

	// Delete from DB (cascades to credentials and objects)
	if err := h.DB.DeleteBucket(bucketName); err != nil {
		h.Logger.Error("failed to delete bucket", "error", err)
		s3err.WriteError(w, r, s3err.ErrInternalError)
		return
	}

	// Delete storage directory
	if err := h.Storage.DeleteBucketDir(bucketName); err != nil {
		h.Logger.Error("failed to delete bucket directory", "bucket", bucketName, "error", err)
	}

	w.WriteHeader(http.StatusNoContent)
}

// HeadBucket handles HEAD /<bucket>
func (h *Handler) HeadBucket(w http.ResponseWriter, r *http.Request) {
	cred, ok := h.authenticateRequest(w, r)
	if !ok {
		return
	}

	bucketName, _ := getBucketAndKey(r)
	_, ok = h.checkBucketAccess(w, r, cred, bucketName)
	if !ok {
		return
	}

	w.Header().Set("x-amz-bucket-region", h.Config.Server.Region)
	w.WriteHeader(http.StatusOK)
}
