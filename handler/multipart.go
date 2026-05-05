package handler

import (
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/onaonbir/Cloodsy-S3/db"
	"github.com/onaonbir/Cloodsy-S3/s3err"
	"github.com/onaonbir/Cloodsy-S3/s3xml"
	"github.com/onaonbir/Cloodsy-S3/webhook"
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
	if key == "" || !isValidObjectKey(key) {
		s3err.WriteError(w, r, s3err.ErrInvalidArgument)
		return
	}

	bucket, ok := h.checkBucketAccess(w, r, cred, bucketName)
	if !ok {
		return
	}

	uploadID := uuid.New().String()
	contentType := r.Header.Get("Content-Type")
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	metadata := collectMetadata(r)

	upload := &db.MultipartUpload{
		ID:          uploadID,
		BucketID:    bucket.ID,
		Key:         key,
		ContentType: contentType,
		Metadata:    metadata,
		CreatedAt:   time.Now(),
	}

	if err := h.DB.CreateMultipartUpload(upload); err != nil {
		h.Logger.Error("failed to create multipart upload", "error", err)
		s3err.WriteError(w, r, s3err.ErrInternalError)
		return
	}

	result := s3xml.InitiateMultipartUploadResult{
		Xmlns:    "http://s3.amazonaws.com/doc/2006-03-01/",
		Bucket:   bucketName,
		Key:      key,
		UploadId: uploadID,
	}

	h.writeXML(w, http.StatusOK, result)
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
	partNumberStr := r.URL.Query().Get("partNumber")

	upload, err := h.DB.GetMultipartUpload(uploadID)
	if err != nil {
		h.Logger.Error("db error", "error", err)
		s3err.WriteError(w, r, s3err.ErrInternalError)
		return
	}
	if upload == nil {
		s3err.WriteError(w, r, s3err.ErrNoSuchUpload)
		return
	}

	// Validate upload belongs to the correct bucket (prevent cross-bucket hijacking)
	if upload.BucketID != bucket.ID {
		s3err.WriteError(w, r, s3err.ErrNoSuchUpload)
		return
	}

	partNumber, err := strconv.Atoi(partNumberStr)
	if err != nil || partNumber < 1 || partNumber > maxParts {
		s3err.WriteError(w, r, s3err.ErrInvalidArgument)
		return
	}

	// Write part data (decode aws-chunked if needed) with size limit
	body := io.LimitReader(getRequestBody(r), maxPartSize+1)
	size, etag, err := h.Storage.PutMultipartPart(bucketName, uploadID, partNumber, body)
	if err != nil {
		h.Logger.Error("failed to write part", "error", err)
		s3err.WriteError(w, r, s3err.ErrInternalError)
		return
	}

	// Check size limit; clean up oversized part
	if size > maxPartSize {
		h.Storage.DeleteMultipartParts(bucketName, uploadID)
		s3err.WriteError(w, r, s3err.ErrEntityTooLarge)
		return
	}

	// Record part in DB
	part := &db.MultipartPart{
		UploadID:   uploadID,
		PartNumber: partNumber,
		Size:       size,
		ETag:       etag,
	}
	if err := h.DB.PutMultipartPart(part); err != nil {
		h.Logger.Error("failed to save part metadata", "error", err)
		s3err.WriteError(w, r, s3err.ErrInternalError)
		return
	}

	w.Header().Set("ETag", etag)
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

	upload, err := h.DB.GetMultipartUpload(uploadID)
	if err != nil {
		h.Logger.Error("db error", "error", err)
		s3err.WriteError(w, r, s3err.ErrInternalError)
		return
	}
	if upload == nil {
		s3err.WriteError(w, r, s3err.ErrNoSuchUpload)
		return
	}

	// Validate upload belongs to the correct bucket (prevent cross-bucket hijacking)
	if upload.BucketID != bucket.ID {
		s3err.WriteError(w, r, s3err.ErrNoSuchUpload)
		return
	}

	// Parse request body with size limit
	var completeReq s3xml.CompleteMultipartUpload
	if err := limitedXMLDecode(r.Body, &completeReq); err != nil {
		s3err.WriteError(w, r, s3err.ErrMalformedXML)
		return
	}

	// Enforce part count limit
	if len(completeReq.Parts) > maxParts {
		s3err.WriteError(w, r, s3err.ErrInvalidArgument)
		return
	}

	// Validate parts are in order
	dbParts, err := h.DB.ListMultipartParts(uploadID)
	if err != nil {
		h.Logger.Error("db error", "error", err)
		s3err.WriteError(w, r, s3err.ErrInternalError)
		return
	}

	partMap := make(map[int]db.MultipartPart)
	for _, p := range dbParts {
		partMap[p.PartNumber] = p
	}

	var partNumbers []int
	prevPartNum := 0
	for _, p := range completeReq.Parts {
		if p.PartNumber <= prevPartNum {
			s3err.WriteError(w, r, s3err.ErrInvalidPartOrder)
			return
		}
		etag := strings.Trim(p.ETag, "\"")
		dbPart, exists := partMap[p.PartNumber]
		if !exists {
			s3err.WriteError(w, r, s3err.ErrInvalidPart)
			return
		}
		dbEtag := strings.Trim(dbPart.ETag, "\"")
		if etag != dbEtag {
			s3err.WriteError(w, r, s3err.ErrInvalidPart)
			return
		}
		partNumbers = append(partNumbers, p.PartNumber)
		prevPartNum = p.PartNumber
	}

	// Calculate total size before assembly to enforce limits and quota
	var expectedSize int64
	for _, pn := range partNumbers {
		if p, ok := partMap[pn]; ok {
			expectedSize += p.Size
			if expectedSize < 0 { // integer overflow check
				s3err.WriteError(w, r, s3err.ErrEntityTooLarge)
				return
			}
		}
	}
	if expectedSize > maxMultipartSize {
		s3err.WriteError(w, r, s3err.ErrEntityTooLarge)
		return
	}

	// Pre-check bucket quota before expensive assembly
	if !h.checkQuota(w, r, bucket, expectedSize) {
		return
	}

	// Determine versioning
	versionID := ""
	versioned := bucket.Versioning == "Enabled"
	suspended := bucket.Versioning == "Suspended"
	if versioned {
		versionID = generateVersionID()
	} else if suspended {
		versionID = "null"
	}

	// Assemble parts into final object
	var totalSize int64
	var etag string
	if versionID != "" && versionID != "null" {
		totalSize, etag, err = h.Storage.AssembleMultipartPartsVersioned(bucketName, key, versionID, uploadID, partNumbers)
	} else {
		totalSize, etag, err = h.Storage.AssembleMultipartParts(bucketName, key, uploadID, partNumbers)
	}
	if err != nil {
		h.Logger.Error("failed to assemble parts", "error", err)
		s3err.WriteError(w, r, s3err.ErrInternalError)
		return
	}

	now := time.Now().UTC()

	meta := &db.ObjectMeta{
		BucketID:     bucket.ID,
		Key:          key,
		Size:         totalSize,
		ETag:         etag,
		ContentType:  upload.ContentType,
		LastModified: now,
		Metadata:     upload.Metadata,
		VersionID:    versionID,
		IsLatest:     true,
	}

	if versioned || suspended {
		err = h.DB.PutObjectMetaVersioned(meta)
	} else {
		err = h.DB.PutObjectMeta(meta)
	}
	if err != nil {
		h.Logger.Error("failed to save object metadata", "error", err)
		s3err.WriteError(w, r, s3err.ErrInternalError)
		return
	}

	// Clean up multipart data
	if err := h.Storage.DeleteMultipartParts(bucketName, uploadID); err != nil {
		h.Logger.Error("failed to clean up multipart parts", "uploadId", uploadID, "error", err)
	}
	h.DB.DeleteMultipartUpload(uploadID)

	if versionID != "" {
		w.Header().Set("x-amz-version-id", versionID)
	}

	// Emit webhook event
	if h.Dispatcher != nil {
		h.Dispatcher.Emit(webhook.Event{
			BucketName: bucketName,
			EventType:  "s3:ObjectCreated:CompleteMultipartUpload",
			Key:        key,
			Size:       totalSize,
			ETag:       etag,
			VersionID:  versionID,
			Timestamp:  now,
		})
	}

	result := s3xml.CompleteMultipartUploadResult{
		Xmlns:    "http://s3.amazonaws.com/doc/2006-03-01/",
		Location: "/" + bucketName + "/" + key,
		Bucket:   bucketName,
		Key:      key,
		ETag:     etag,
	}

	h.writeXML(w, http.StatusOK, result)
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

	upload, err := h.DB.GetMultipartUpload(uploadID)
	if err != nil {
		h.Logger.Error("db error", "error", err)
		s3err.WriteError(w, r, s3err.ErrInternalError)
		return
	}
	if upload == nil {
		s3err.WriteError(w, r, s3err.ErrNoSuchUpload)
		return
	}

	// Validate upload belongs to the correct bucket
	if upload.BucketID != bucket.ID {
		s3err.WriteError(w, r, s3err.ErrNoSuchUpload)
		return
	}

	// Clean up
	h.Storage.DeleteMultipartParts(bucketName, uploadID)
	h.DB.DeleteMultipartUpload(uploadID)

	w.WriteHeader(http.StatusNoContent)
}
