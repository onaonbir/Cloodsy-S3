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
		Owner: defaultOwner,
	}
	for _, b := range buckets {
		result.Buckets.Bucket = append(result.Buckets.Bucket, s3xml.BucketInfo{
			Name:         b.Name,
			CreationDate: b.CreatedAt.UTC().Format(lastModifiedFormat),
		})
	}
	h.writeXML(w, http.StatusOK, result)
}

// CreateBucket handles PUT /<bucket>.
//
// Credentials are bound to exactly one bucket, which is created by an
// operator (CLI or Admin API). A PUT for the credential's own bucket is
// therefore idempotent (BucketAlreadyOwnedByYou), and any other name is
// denied instead of creating an orphan bucket nobody can access.
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

	existing, err := h.DB.GetBucket(bucketName)
	if err != nil {
		h.Logger.Error("db error", "error", err)
		s3err.WriteError(w, r, s3err.ErrInternalError)
		return
	}
	if existing != nil {
		if cred.BucketID == existing.ID {
			w.Header().Set("Location", "/"+bucketName)
			s3err.WriteError(w, r, s3err.ErrBucketAlreadyOwnedByYou)
		} else {
			s3err.WriteError(w, r, s3err.ErrBucketAlreadyExists)
		}
		return
	}
	s3err.WriteErrorMsg(w, r, s3err.ErrAccessDenied, "Buckets are created by an administrator (cloodsys3 bucket create).")
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

	// Any row — including noncurrent versions and delete markers — blocks deletion, as on S3.
	hasRows, err := h.DB.BucketHasAnyRows(bucket.ID)
	if err != nil {
		h.Logger.Error("db error", "error", err)
		s3err.WriteError(w, r, s3err.ErrInternalError)
		return
	}
	if hasRows {
		s3err.WriteError(w, r, s3err.ErrBucketNotEmpty)
		return
	}

	if err := h.DB.DeleteBucket(bucketName); err != nil {
		h.Logger.Error("failed to delete bucket", "error", err)
		s3err.WriteError(w, r, s3err.ErrInternalError)
		return
	}
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
	if _, ok = h.checkBucketAccess(w, r, cred, bucketName); !ok {
		return
	}
	w.Header().Set("x-amz-bucket-region", h.Config.Server.Region)
	w.WriteHeader(http.StatusOK)
}
