package handler

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/onaonbir/Cloodsy-S3/db"
	"github.com/onaonbir/Cloodsy-S3/s3err"
	"github.com/onaonbir/Cloodsy-S3/s3xml"
	"github.com/onaonbir/Cloodsy-S3/service"
	"github.com/onaonbir/Cloodsy-S3/storage"
)

// CreateMultipartUpload handles POST /<bucket>/<key>?uploads
func (h *Handler) CreateMultipartUpload(w http.ResponseWriter, r *http.Request) {
	cred, ok := h.authenticateRequest(w, r)
	if !ok {
		return
	}
	if !h.checkWriteAccess(w, r, cred) {
		return
	}

	bucketName, key := getBucketAndKey(r)
	if len(key) > maxKeyLength {
		s3err.WriteError(w, r, s3err.ErrKeyTooLong)
		return
	}
	if key == "" || !isValidObjectKey(key) {
		s3err.WriteError(w, r, s3err.ErrInvalidArgument)
		return
	}
	bucket, ok := h.checkBucketAccess(w, r, cred, bucketName)
	if !ok {
		return
	}

	contentType := r.Header.Get("Content-Type")
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	metadata, err := collectMetadata(r)
	if err != nil {
		h.writeServiceError(w, r, err, "metadata")
		return
	}

	upload := &db.MultipartUpload{
		ID:          uuid.New().String(),
		BucketID:    bucket.ID,
		Key:         key,
		ContentType: contentType,
		Metadata:    metadata,
		CreatedAt:   time.Now().UTC(),
	}
	if err := h.DB.CreateMultipartUpload(upload); err != nil {
		h.Logger.Error("failed to create multipart upload", "error", err)
		s3err.WriteError(w, r, s3err.ErrInternalError)
		return
	}

	h.writeXML(w, http.StatusOK, s3xml.InitiateMultipartUploadResult{
		Xmlns:    "http://s3.amazonaws.com/doc/2006-03-01/",
		Bucket:   bucketName,
		Key:      key,
		UploadId: upload.ID,
	})
}

// loadUpload validates the uploadId belongs to the bucket.
func (h *Handler) loadUpload(w http.ResponseWriter, r *http.Request, bucket *db.Bucket, uploadID string) *db.MultipartUpload {
	upload, err := h.DB.GetMultipartUpload(uploadID)
	if err != nil {
		h.Logger.Error("db error", "error", err)
		s3err.WriteError(w, r, s3err.ErrInternalError)
		return nil
	}
	if upload == nil || upload.BucketID != bucket.ID {
		s3err.WriteError(w, r, s3err.ErrNoSuchUpload)
		return nil
	}
	return upload
}

// UploadPart handles PUT /<bucket>/<key>?partNumber=N&uploadId=X
func (h *Handler) UploadPart(w http.ResponseWriter, r *http.Request) {
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

	uploadID := r.URL.Query().Get("uploadId")
	if h.loadUpload(w, r, bucket, uploadID) == nil {
		return
	}
	partNumber, err := strconv.Atoi(r.URL.Query().Get("partNumber"))
	if err != nil || partNumber < 1 || partNumber > maxParts {
		s3err.WriteError(w, r, s3err.ErrInvalidArgument)
		return
	}

	body, checker, err := h.requestBody(r, cred)
	if err != nil {
		h.writeServiceError(w, r, err, "request body")
		return
	}
	if checker.declared > maxPartSize {
		s3err.WriteError(w, r, s3err.ErrEntityTooLarge)
		return
	}

	part, err := h.Objects.PutPart(bucket, uploadID, partNumber, body, storage.PutOptions{
		MaxSize: maxPartSize,
		Verify:  checker.verify,
	})
	if err != nil {
		h.writeServiceError(w, r, err, "failed to write part")
		return
	}
	etag := part.ETag

	w.Header().Set("ETag", etag)
	if checker.checksumAlgo != "" && checker.checksumHash != nil {
		w.Header().Set("x-amz-checksum-"+checker.checksumAlgo, checksumBase64(checker))
	}
	w.WriteHeader(http.StatusOK)
}

// CompleteMultipartUpload handles POST /<bucket>/<key>?uploadId=X
func (h *Handler) CompleteMultipartUpload(w http.ResponseWriter, r *http.Request) {
	cred, ok := h.authenticateRequest(w, r)
	if !ok {
		return
	}
	if !h.checkWriteAccess(w, r, cred) {
		return
	}

	bucketName, key := getBucketAndKey(r)
	if key == "" || !isValidObjectKey(key) {
		s3err.WriteError(w, r, s3err.ErrInvalidArgument)
		return
	}
	bucket, ok := h.checkBucketAccess(w, r, cred, bucketName)
	if !ok {
		return
	}

	uploadID := r.URL.Query().Get("uploadId")

	// Serialize completion per upload so a concurrent second Complete sees
	// NoSuchUpload instead of racing over the staged parts.
	unlockUpload := h.Objects.LockUpload(uploadID)
	defer unlockUpload()

	upload := h.loadUpload(w, r, bucket, uploadID)
	if upload == nil {
		return
	}

	var completeReq s3xml.CompleteMultipartUpload
	if err := limitedXMLDecode(r.Body, &completeReq); err != nil {
		s3err.WriteError(w, r, s3err.ErrMalformedXML)
		return
	}
	if len(completeReq.Parts) == 0 || len(completeReq.Parts) > maxParts {
		s3err.WriteError(w, r, s3err.ErrMalformedXML)
		return
	}

	parts := make([]service.CompletedPart, len(completeReq.Parts))
	for i, p := range completeReq.Parts {
		parts[i] = service.CompletedPart{PartNumber: p.PartNumber, ETag: p.ETag}
	}
	res, err := h.Objects.CompleteMultipart(bucket, upload, key, parts)
	if err != nil {
		h.writeServiceError(w, r, err, "failed to complete multipart upload")
		return
	}
	etag := res.ETag

	if res.VersionID != "" {
		w.Header().Set("x-amz-version-id", res.VersionID)
	}

	scheme := "http"
	if r.TLS != nil || h.Config.Server.TLS.Enabled {
		scheme = "https"
	}
	h.writeXML(w, http.StatusOK, s3xml.CompleteMultipartUploadResult{
		Xmlns:    "http://s3.amazonaws.com/doc/2006-03-01/",
		Location: scheme + "://" + r.Host + "/" + bucketName + "/" + EncodeKeyIfNeeded(key, "url"),
		Bucket:   bucketName,
		Key:      key,
		ETag:     etag,
	})
}

// AbortMultipartUpload handles DELETE /<bucket>/<key>?uploadId=X
func (h *Handler) AbortMultipartUpload(w http.ResponseWriter, r *http.Request) {
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

	uploadID := r.URL.Query().Get("uploadId")
	if h.loadUpload(w, r, bucket, uploadID) == nil {
		return
	}

	if err := h.Objects.AbortMultipart(bucket, uploadID); err != nil {
		if errors.Is(err, service.ErrNoSuchUpload) {
			s3err.WriteError(w, r, s3err.ErrNoSuchUpload)
			return
		}
		h.Logger.Error("failed to abort multipart upload", "uploadId", uploadID, "error", err)
	}
	w.WriteHeader(http.StatusNoContent)
}
