package handler

import (
	"net/http"
	"strconv"
	"strings"
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

	size, etag, err := h.Storage.PutMultipartPart(bucketName, uploadID, partNumber, body, storage.PutOptions{
		MaxSize: maxPartSize,
		Verify:  checker.verify,
	})
	if err != nil {
		h.writeServiceError(w, r, err, "failed to write part")
		return
	}

	part := &db.MultipartPart{UploadID: uploadID, PartNumber: partNumber, Size: size, ETag: etag}
	if err := h.DB.PutMultipartPart(part); err != nil {
		h.Storage.DeleteMultipartPart(bucketName, uploadID, partNumber)
		h.Logger.Error("failed to save part metadata", "error", err)
		s3err.WriteError(w, r, s3err.ErrInternalError)
		return
	}

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

	dbParts, err := h.DB.ListMultipartParts(uploadID)
	if err != nil {
		h.Logger.Error("db error", "error", err)
		s3err.WriteError(w, r, s3err.ErrInternalError)
		return
	}
	partMap := make(map[int]db.MultipartPart, len(dbParts))
	for _, p := range dbParts {
		partMap[p.PartNumber] = p
	}

	// Validate in S3 order: part order, then part existence/ETag, then sizes.
	prevPartNum := 0
	for _, p := range completeReq.Parts {
		if p.PartNumber <= prevPartNum {
			s3err.WriteError(w, r, s3err.ErrInvalidPartOrder)
			return
		}
		prevPartNum = p.PartNumber
	}
	var partNumbers []int
	var expectedSize int64
	for i, p := range completeReq.Parts {
		dbPart, exists := partMap[p.PartNumber]
		if !exists || strings.Trim(p.ETag, "\"") != strings.Trim(dbPart.ETag, "\"") {
			s3err.WriteError(w, r, s3err.ErrInvalidPart)
			return
		}
		if i < len(completeReq.Parts)-1 && dbPart.Size < minPartSize {
			s3err.WriteError(w, r, s3err.ErrEntityTooSmall)
			return
		}
		expectedSize += dbPart.Size
		if expectedSize < 0 || expectedSize > maxMultipartSize {
			s3err.WriteError(w, r, s3err.ErrEntityTooLarge)
			return
		}
		partNumbers = append(partNumbers, p.PartNumber)
	}

	// Serialize with other writers of this key and check quota before the
	// expensive assembly.
	unlockKey := h.Objects.Lock(bucket.ID, key)
	defer unlockKey()

	if err := h.checkQuotaFor(bucket, key, expectedSize); err != nil {
		h.writeServiceError(w, r, err, "quota")
		return
	}

	versionID := service.VersionIDForPut(bucket)
	opts := storage.PutOptions{
		MaxSize: maxMultipartSize,
		Verify: func(size int64, _ []byte) error {
			return h.checkQuotaFor(bucket, key, size)
		},
	}
	var totalSize int64
	var etag string
	if service.IsRealVersion(versionID) {
		totalSize, etag, err = h.Storage.AssembleMultipartPartsVersioned(bucketName, key, versionID, uploadID, partNumbers, opts)
	} else {
		totalSize, etag, err = h.Storage.AssembleMultipartParts(bucketName, key, uploadID, partNumbers, opts)
	}
	if err != nil {
		h.writeServiceError(w, r, err, "failed to assemble parts")
		return
	}

	res, err := h.Objects.Commit(bucket, key, versionID, totalSize, etag, upload.ContentType, upload.Metadata, "s3:ObjectCreated:CompleteMultipartUpload")
	if err != nil {
		h.Logger.Error("failed to save object metadata", "error", err)
		s3err.WriteError(w, r, s3err.ErrInternalError)
		return
	}

	if err := h.Storage.DeleteMultipartParts(bucketName, uploadID); err != nil {
		h.Logger.Error("failed to clean up multipart parts", "uploadId", uploadID, "error", err)
	}
	h.DB.DeleteMultipartUpload(uploadID)

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

// checkQuotaFor mirrors the service quota rule for callers that assemble
// bytes themselves.
func (h *Handler) checkQuotaFor(bucket *db.Bucket, key string, additional int64) error {
	if bucket.QuotaBytes <= 0 {
		return nil
	}
	usage, err := h.DB.GetBucketUsage(bucket.ID)
	if err != nil {
		return err
	}
	if bucket.Versioning != "Enabled" {
		if cur, err := h.DB.GetObjectMeta(bucket.ID, key); err == nil && cur != nil && !cur.IsDeleteMarker {
			usage -= cur.Size
		}
	}
	if usage+additional > bucket.QuotaBytes {
		return service.ErrQuotaExceeded
	}
	return nil
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

	h.Storage.DeleteMultipartParts(bucketName, uploadID)
	h.DB.DeleteMultipartUpload(uploadID)
	w.WriteHeader(http.StatusNoContent)
}
