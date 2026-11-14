package admin

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/onaonbir/Cloodsy-S3/db"
	"github.com/onaonbir/Cloodsy-S3/httpx"
	"github.com/onaonbir/Cloodsy-S3/service"
	"github.com/onaonbir/Cloodsy-S3/storage"
)

// Admin multipart upload: the same staging, part records, assembly and
// commit path as the S3 API (service.Objects), so quota, versioning, image
// optimization and webhooks behave identically. Uploads started by S3 clients
// are visible (and abortable) here too.

// loadUploadFor writes 404 and returns nil unless uploadID is an incomplete
// upload of bucket.
func (h *Handler) loadUploadFor(w http.ResponseWriter, bucket *db.Bucket, uploadID string) *db.MultipartUpload {
	if _, err := uuid.Parse(uploadID); err != nil || len(uploadID) != 36 {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "upload not found"})
		return nil
	}
	upload, err := h.DB.GetMultipartUpload(uploadID)
	if err != nil {
		h.Logger.Error("get multipart upload error", "upload", httpx.SanitizeLog(uploadID), "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return nil
	}
	if upload == nil || upload.BucketID != bucket.ID {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "upload not found"})
		return nil
	}
	return upload
}

// handleCreateUpload starts a multipart upload.
// POST /admin/buckets/{bucket}/uploads?key=   (Content-Type header = object type)
func (h *Handler) handleCreateUpload(w http.ResponseWriter, r *http.Request, bucketName string) {
	bucket := h.loadBucket(w, bucketName)
	if bucket == nil {
		return
	}
	key := r.URL.Query().Get("key")
	if msg := validateUploadKey(key); msg != "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": msg})
		return
	}
	contentType := r.Header.Get("Content-Type")
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	if !validContentType(contentType) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid content type"})
		return
	}
	upload := &db.MultipartUpload{
		ID:          uuid.New().String(),
		BucketID:    bucket.ID,
		Key:         key,
		ContentType: contentType,
		Metadata:    "{}",
		CreatedAt:   time.Now().UTC(),
	}
	if err := h.DB.CreateMultipartUpload(upload); err != nil {
		h.Logger.Error("create multipart upload error", "bucket", bucketName, "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	h.Logger.Info("multipart upload started via admin API", "bucket", bucketName, "key", httpx.SanitizeLog(key), "upload", upload.ID)
	writeJSON(w, http.StatusOK, map[string]interface{}{"upload_id": upload.ID, "key": key})
}

// handleUploadPart stores one part from the raw request body (exempt from the
// JSON body cap). The 5 MiB minimum applies to all but the last part and is
// enforced at completion, like S3.
// PUT /admin/buckets/{bucket}/uploads/{id}/parts/{n}
func (h *Handler) handleUploadPart(w http.ResponseWriter, r *http.Request, bucketName, uploadID, partStr string) {
	bucket := h.loadBucket(w, bucketName)
	if bucket == nil {
		return
	}
	partNumber, err := strconv.Atoi(partStr)
	if err != nil || partNumber < 1 || partNumber > service.MaxParts {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": fmt.Sprintf("part number must be 1..%d", service.MaxParts)})
		return
	}
	if h.loadUploadFor(w, bucket, uploadID) == nil {
		return
	}
	declared := r.ContentLength
	if declared > service.MaxPartSize {
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": fmt.Sprintf("part too large (max %d bytes)", service.MaxPartSize)})
		return
	}

	var body io.Reader = http.NoBody
	if r.Body != nil {
		body = r.Body
	}
	tracker := &readTracker{r: body}
	part, err := h.Objects.PutPart(bucket, uploadID, partNumber, tracker, storage.PutOptions{
		MaxSize: service.MaxPartSize,
		Verify: func(size int64, _ []byte) error {
			if declared >= 0 && size != declared {
				return errIncompleteUpload
			}
			return nil
		},
	})
	if err != nil {
		switch {
		case errors.Is(err, service.ErrNoSuchUpload):
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "upload not found"})
		case errors.Is(err, service.ErrTooLarge):
			writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": fmt.Sprintf("part too large (max %d bytes)", service.MaxPartSize)})
		case errors.Is(err, errIncompleteUpload), tracker.err != nil:
			h.Logger.Warn("part upload aborted", "bucket", bucketName, "upload", uploadID, "part", partNumber, "error", err)
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "incomplete request body"})
		default:
			h.Logger.Error("part upload error", "bucket", bucketName, "upload", uploadID, "part", partNumber, "error", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		}
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"part_number": part.PartNumber,
		"etag":        part.ETag,
		"size":        part.Size,
	})
}

// handleListUploadParts lists the staged parts (for resuming).
// GET /admin/buckets/{bucket}/uploads/{id}/parts
func (h *Handler) handleListUploadParts(w http.ResponseWriter, r *http.Request, bucketName, uploadID string) {
	bucket := h.loadBucket(w, bucketName)
	if bucket == nil {
		return
	}
	if h.loadUploadFor(w, bucket, uploadID) == nil {
		return
	}
	parts, err := h.DB.ListMultipartParts(uploadID)
	if err != nil {
		h.Logger.Error("list parts error", "upload", uploadID, "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	out := make([]map[string]interface{}, 0, len(parts))
	for _, p := range parts {
		out = append(out, map[string]interface{}{
			"part_number":   p.PartNumber,
			"etag":          p.ETag,
			"size":          p.Size,
			"last_modified": fmtTime(p.CreatedAt),
		})
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"parts": out})
}

// handleCompleteUpload assembles the listed parts into the object.
// POST /admin/buckets/{bucket}/uploads/{id}/complete
func (h *Handler) handleCompleteUpload(w http.ResponseWriter, r *http.Request, bucketName, uploadID string) {
	bucket := h.loadBucket(w, bucketName)
	if bucket == nil {
		return
	}
	var req struct {
		Parts []struct {
			PartNumber int    `json:"part_number"`
			ETag       string `json:"etag"`
		} `json:"parts"`
	}
	if !decodeJSON(w, r, &req, false) {
		return
	}
	if len(req.Parts) == 0 || len(req.Parts) > service.MaxParts {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": fmt.Sprintf("parts must list 1..%d parts", service.MaxParts)})
		return
	}

	unlock := h.Objects.LockUpload(uploadID)
	defer unlock()
	upload := h.loadUploadFor(w, bucket, uploadID)
	if upload == nil {
		return
	}
	parts := make([]service.CompletedPart, len(req.Parts))
	for i, p := range req.Parts {
		parts[i] = service.CompletedPart{PartNumber: p.PartNumber, ETag: p.ETag}
	}

	// Assembly of a large object moves no bytes over the wire for a while.
	clearDeadlines(w)
	res, err := h.Objects.CompleteMultipart(bucket, upload, upload.Key, parts)
	safeKey := httpx.SanitizeLog(upload.Key)
	if err != nil {
		switch {
		case errors.Is(err, service.ErrQuotaExceeded):
			writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "quota exceeded"})
		case errors.Is(err, service.ErrTooLarge):
			writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "object too large"})
		case errors.Is(err, service.ErrInvalidPartOrder):
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "parts must be listed in ascending part_number order"})
		case errors.Is(err, service.ErrInvalidPart):
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "a listed part is missing or its etag does not match"})
		case errors.Is(err, service.ErrEntityTooSmall):
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": fmt.Sprintf("every part except the last must be at least %d bytes", service.MinPartSize)})
		default:
			h.Logger.Error("complete multipart error", "bucket", bucketName, "key", safeKey, "upload", uploadID, "error", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		}
		return
	}
	h.Logger.Info("multipart upload completed via admin API", "bucket", bucketName, "key", safeKey, "size", res.Size, "parts", len(parts))
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"key":        upload.Key,
		"size":       res.Size,
		"etag":       res.ETag,
		"version_id": res.VersionID,
	})
}

// handleAbortUpload discards one incomplete upload.
// DELETE /admin/buckets/{bucket}/uploads/{id}
func (h *Handler) handleAbortUpload(w http.ResponseWriter, r *http.Request, bucketName, uploadID string) {
	bucket := h.loadBucket(w, bucketName)
	if bucket == nil {
		return
	}
	if h.loadUploadFor(w, bucket, uploadID) == nil {
		return
	}
	if err := h.Objects.AbortMultipart(bucket, uploadID); err != nil {
		if errors.Is(err, service.ErrNoSuchUpload) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "upload not found"})
			return
		}
		h.Logger.Error("abort multipart error", "bucket", bucketName, "upload", uploadID, "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	h.Logger.Info("multipart upload aborted via admin API", "bucket", bucketName, "upload", uploadID)
	writeJSON(w, http.StatusOK, map[string]string{"message": "upload aborted"})
}

// handleListUploads lists every incomplete multipart upload of the bucket,
// whoever started it.
// GET /admin/buckets/{bucket}/uploads
func (h *Handler) handleListUploads(w http.ResponseWriter, r *http.Request, bucketName string) {
	bucket := h.loadBucket(w, bucketName)
	if bucket == nil {
		return
	}
	uploads, err := h.DB.ListMultipartUploadsWithStats(bucket.ID)
	if err != nil {
		h.Logger.Error("list uploads error", "bucket", bucketName, "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	out := make([]map[string]interface{}, 0, len(uploads))
	for _, u := range uploads {
		out = append(out, map[string]interface{}{
			"upload_id": u.ID,
			"key":       u.Key,
			"initiated": fmtTime(u.CreatedAt),
			"parts":     u.Parts,
			"bytes":     u.Bytes,
		})
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"uploads": out})
}

// handleAbortAllUploads aborts every incomplete upload of the bucket, or only
// those initiated more than older_than_days days ago.
// DELETE /admin/buckets/{bucket}/uploads[?older_than_days=N]
func (h *Handler) handleAbortAllUploads(w http.ResponseWriter, r *http.Request, bucketName string) {
	bucket := h.loadBucket(w, bucketName)
	if bucket == nil {
		return
	}
	days := 0
	if v := r.URL.Query().Get("older_than_days"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 || n > 36500 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid older_than_days"})
			return
		}
		days = n
	}
	uploads, err := h.DB.ListMultipartUploadsWithStats(bucket.ID)
	if err != nil {
		h.Logger.Error("list uploads error", "bucket", bucketName, "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	cutoff := time.Now().Add(-time.Duration(days) * 24 * time.Hour)
	aborted := 0
	for _, u := range uploads {
		if days > 0 && !u.CreatedAt.Before(cutoff) {
			continue
		}
		if err := h.Objects.AbortMultipart(bucket, u.ID); err != nil {
			h.Logger.Error("abort multipart error", "bucket", bucketName, "upload", u.ID, "error", err)
			continue
		}
		aborted++
	}
	h.Logger.Info("multipart uploads aborted via admin API", "bucket", bucketName, "aborted", aborted, "older_than_days", days)
	writeJSON(w, http.StatusOK, map[string]interface{}{"aborted": aborted})
}
