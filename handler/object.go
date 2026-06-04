package handler

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/onaonbir/Cloodsy-S3/db"
	imageutil "github.com/onaonbir/Cloodsy-S3/image"
	"github.com/onaonbir/Cloodsy-S3/s3err"
	"github.com/onaonbir/Cloodsy-S3/storage"
	"github.com/onaonbir/Cloodsy-S3/webhook"
)

// PutObject handles PUT /<bucket>/<key>
func (h *Handler) PutObject(w http.ResponseWriter, r *http.Request) {
	cred, ok := h.authenticateRequest(w, r)
	if !ok {
		return
	}

	bucketName, key := getBucketAndKey(r)
	if key == "" || !isValidObjectKey(key) {
		s3err.WriteError(w, r, s3err.ErrInvalidArgument)
		return
	}

	if !h.checkWriteAccess(w, r, cred) {
		return
	}

	bucket, ok := h.checkBucketAccess(w, r, cred, bucketName)
	if !ok {
		return
	}

	// Check for copy source
	copySource := r.Header.Get("X-Amz-Copy-Source")
	if copySource != "" {
		h.copyObject(w, r, cred, bucket, key, copySource)
		return
	}

	// Determine versioning behavior
	versionID := ""
	versioned := bucket.Versioning == "Enabled"
	suspended := bucket.Versioning == "Suspended"

	if versioned {
		versionID = generateVersionID()
	} else if suspended {
		versionID = "null"
	}

	// Write object data
	body := io.LimitReader(getRequestBody(r), maxObjectSize+1)
	var size int64
	var etag string
	var writeErr error

	if versionID != "" && versionID != "null" {
		size, etag, writeErr = h.Storage.PutVersionedObject(bucketName, key, versionID, body)
	} else {
		size, etag, writeErr = h.Storage.PutObject(bucketName, key, body)
	}
	if writeErr != nil {
		h.Logger.Error("failed to write object", "error", writeErr)
		s3err.WriteError(w, r, s3err.ErrInternalError)
		return
	}

	// Check if the object exceeded the size limit
	if size > maxObjectSize {
		if versionID != "" && versionID != "null" {
			h.Storage.DeleteVersionedObject(bucketName, key, versionID)
		} else {
			h.Storage.DeleteObject(bucketName, key)
		}
		s3err.WriteError(w, r, s3err.ErrEntityTooLarge)
		return
	}

	// Check bucket quota
	if !h.checkQuota(w, r, bucket, size) {
		if versionID != "" && versionID != "null" {
			h.Storage.DeleteVersionedObject(bucketName, key, versionID)
		} else {
			h.Storage.DeleteObject(bucketName, key)
		}
		return
	}

	contentType := r.Header.Get("Content-Type")
	if contentType == "" {
		contentType = "application/octet-stream"
	}

	metadata := collectMetadata(r)
	now := time.Now().UTC()

	meta := &db.ObjectMeta{
		BucketID:     bucket.ID,
		Key:          key,
		Size:         size,
		ETag:         etag,
		ContentType:  contentType,
		LastModified: now,
		Metadata:     metadata,
		VersionID:    versionID,
		IsLatest:     true,
	}

	var err error
	if versioned || suspended {
		err = h.DB.PutObjectMetaVersioned(meta)
	} else {
		err = h.DB.PutObjectMeta(meta)
	}
	if err != nil {
		h.Logger.Error("failed to save object metadata", "error", err)
		if versionID != "" && versionID != "null" {
			h.Storage.DeleteVersionedObject(bucketName, key, versionID)
		} else {
			h.Storage.DeleteObject(bucketName, key)
		}
		s3err.WriteError(w, r, s3err.ErrInternalError)
		return
	}

	w.Header().Set("ETag", etag)
	if versionID != "" {
		w.Header().Set("x-amz-version-id", versionID)
	}
	h.setExpirationHeader(w, bucketName, key, now)

	// Emit webhook event
	if h.Dispatcher != nil {
		h.Dispatcher.Emit(webhook.Event{
			BucketName: bucketName,
			EventType:  "s3:ObjectCreated:Put",
			Key:        key,
			Size:       size,
			ETag:       etag,
			VersionID:  versionID,
			Timestamp:  now,
		})
	}

	// Best-effort image optimization (original is preserved; an optimized
	// variant is generated alongside). Small images inline, large ones queued.
	// Never affects the PUT result.
	if h.ImageWorker != nil && h.Config.Image.Enabled && imageutil.IsImageContentType(contentType) {
		job := imageutil.Job{Bucket: bucketName, Key: key, VersionID: versionID, ETag: etag, ContentType: contentType}
		if size <= h.Config.Image.SyncMaxBytes {
			h.ImageWorker.Process(job)
		} else {
			h.ImageWorker.Enqueue(job)
		}
	}

	w.WriteHeader(http.StatusOK)
}

// GetObject handles GET /<bucket>/<key>
func (h *Handler) GetObject(w http.ResponseWriter, r *http.Request) {
	bucketName, key := getBucketAndKey(r)
	if key == "" {
		s3err.WriteError(w, r, s3err.ErrNoSuchKey)
		return
	}

	bucket, _, ok := h.authenticateOrPublic(w, r, bucketName)
	if !ok {
		return
	}

	// Check for specific version request
	requestedVersion := r.URL.Query().Get("versionId")

	var meta *db.ObjectMeta
	var err error
	if requestedVersion != "" {
		meta, err = h.DB.GetObjectMetaByVersion(bucket.ID, key, requestedVersion)
	} else {
		meta, err = h.DB.GetObjectMeta(bucket.ID, key)
	}
	if err != nil {
		h.Logger.Error("db error", "error", err)
		s3err.WriteError(w, r, s3err.ErrInternalError)
		return
	}
	if meta == nil {
		s3err.WriteError(w, r, s3err.ErrNoSuchKey)
		return
	}

	// If this is a delete marker, return 404
	if meta.IsDeleteMarker {
		w.Header().Set("x-amz-delete-marker", "true")
		if meta.VersionID != "" {
			w.Header().Set("x-amz-version-id", meta.VersionID)
		}
		s3err.WriteError(w, r, s3err.ErrNoSuchKey)
		return
	}

	// Image transform-on-access (?w=&h=&m=&q=). Served from a sibling cache;
	// on any error serveTransformed returns false and we fall through to the
	// normal full-object path, so a transform failure never breaks a download.
	if tp, want := imageutil.ParseParams(r.URL.Query()); want && imageutil.IsImageContentType(meta.ContentType) {
		if h.serveTransformed(w, r, bucket, meta, tp) {
			return
		}
	}

	// Conditional headers check
	if !h.CheckConditionalHeaders(w, r, meta.ETag, meta.LastModified) {
		return
	}

	rangeHeader := r.Header.Get("Range")

	// Open the file (versioned or unversioned)
	var reader io.ReadCloser
	if meta.VersionID != "" && meta.VersionID != "null" {
		reader, err = h.Storage.GetVersionedObject(bucketName, key, meta.VersionID)
	} else {
		reader, err = h.Storage.GetObject(bucketName, key)
	}
	if err != nil {
		h.Logger.Error("failed to read object", "error", err)
		s3err.WriteError(w, r, s3err.ErrNoSuchKey)
		return
	}
	defer reader.Close()

	w.Header().Set("Content-Type", meta.ContentType)
	w.Header().Set("ETag", meta.ETag)
	w.Header().Set("Last-Modified", meta.LastModified.UTC().Format(http.TimeFormat))
	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("Content-Disposition", "attachment")
	if meta.VersionID != "" {
		w.Header().Set("x-amz-version-id", meta.VersionID)
	}
	h.setExpirationHeader(w, bucketName, key, meta.LastModified)

	setMetadataHeaders(w, meta.Metadata)

	if rangeHeader != "" {
		h.serveRange(w, r, reader, meta, rangeHeader)
		return
	}

	w.Header().Set("Content-Length", strconv.FormatInt(meta.Size, 10))
	w.WriteHeader(http.StatusOK)
	io.Copy(w, reader)
}

// HeadObject handles HEAD /<bucket>/<key>
func (h *Handler) HeadObject(w http.ResponseWriter, r *http.Request) {
	bucketName, key := getBucketAndKey(r)
	if key == "" {
		s3err.WriteError(w, r, s3err.ErrNoSuchKey)
		return
	}

	bucket, _, ok := h.authenticateOrPublic(w, r, bucketName)
	if !ok {
		return
	}

	requestedVersion := r.URL.Query().Get("versionId")

	var meta *db.ObjectMeta
	var err error
	if requestedVersion != "" {
		meta, err = h.DB.GetObjectMetaByVersion(bucket.ID, key, requestedVersion)
	} else {
		meta, err = h.DB.GetObjectMeta(bucket.ID, key)
	}
	if err != nil {
		h.Logger.Error("db error", "error", err)
		s3err.WriteError(w, r, s3err.ErrInternalError)
		return
	}
	if meta == nil {
		s3err.WriteError(w, r, s3err.ErrNoSuchKey)
		return
	}

	if meta.IsDeleteMarker {
		w.Header().Set("x-amz-delete-marker", "true")
		if meta.VersionID != "" {
			w.Header().Set("x-amz-version-id", meta.VersionID)
		}
		s3err.WriteError(w, r, s3err.ErrNoSuchKey)
		return
	}

	// Conditional headers check
	if !h.CheckConditionalHeaders(w, r, meta.ETag, meta.LastModified) {
		return
	}

	w.Header().Set("Content-Type", meta.ContentType)
	w.Header().Set("Content-Length", strconv.FormatInt(meta.Size, 10))
	w.Header().Set("ETag", meta.ETag)
	w.Header().Set("Last-Modified", meta.LastModified.UTC().Format(http.TimeFormat))
	w.Header().Set("Accept-Ranges", "bytes")
	if meta.VersionID != "" {
		w.Header().Set("x-amz-version-id", meta.VersionID)
	}
	h.setExpirationHeader(w, bucketName, key, meta.LastModified)
	setMetadataHeaders(w, meta.Metadata)
	w.WriteHeader(http.StatusOK)
}

// DeleteObject handles DELETE /<bucket>/<key>
func (h *Handler) DeleteObject(w http.ResponseWriter, r *http.Request) {
	cred, ok := h.authenticateRequest(w, r)
	if !ok {
		return
	}

	if !h.checkWriteAccess(w, r, cred) {
		return
	}

	bucketName, key := getBucketAndKey(r)
	if key == "" || !isValidObjectKey(key) {
		s3err.WriteError(w, r, s3err.ErrNoSuchKey)
		return
	}

	bucket, ok := h.checkBucketAccess(w, r, cred, bucketName)
	if !ok {
		return
	}

	// Check for specific version deletion
	requestedVersion := r.URL.Query().Get("versionId")

	if requestedVersion != "" {
		// Delete a specific version permanently
		meta, err := h.DB.GetObjectMetaByVersion(bucket.ID, key, requestedVersion)
		if err != nil {
			h.Logger.Error("db error", "error", err)
			s3err.WriteError(w, r, s3err.ErrInternalError)
			return
		}
		if meta == nil {
			s3err.WriteError(w, r, s3err.ErrNoSuchKey)
			return
		}

		if err := h.DB.DeleteObjectMetaByVersion(bucket.ID, key, requestedVersion); err != nil {
			h.Logger.Error("failed to delete version metadata", "error", err)
			s3err.WriteError(w, r, s3err.ErrInternalError)
			return
		}

		// Delete the actual file (if not a delete marker)
		if !meta.IsDeleteMarker {
			if meta.VersionID != "" && meta.VersionID != "null" {
				h.Storage.DeleteVersionedObject(bucketName, key, meta.VersionID)
			} else {
				h.Storage.DeleteObject(bucketName, key)
			}
		}

		w.Header().Set("x-amz-version-id", requestedVersion)
		if meta.IsDeleteMarker {
			w.Header().Set("x-amz-delete-marker", "true")
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}

	versioned := bucket.Versioning == "Enabled"
	suspended := bucket.Versioning == "Suspended"

	if versioned {
		// Add a delete marker instead of deleting
		versionID := generateVersionID()

		deleteMarker := &db.ObjectMeta{
			BucketID:       bucket.ID,
			Key:            key,
			Size:           0,
			ETag:           "",
			ContentType:    "",
			LastModified:   time.Now().UTC(),
			VersionID:      versionID,
			IsLatest:       true,
			IsDeleteMarker: true,
		}
		if err := h.DB.PutObjectMetaVersioned(deleteMarker); err != nil {
			h.Logger.Error("failed to create delete marker", "error", err)
			s3err.WriteError(w, r, s3err.ErrInternalError)
			return
		}

		w.Header().Set("x-amz-version-id", versionID)
		w.Header().Set("x-amz-delete-marker", "true")
	} else if suspended {
		// In suspended mode, delete the "null" version if it exists
		if err := h.DB.DeleteObjectMetaByVersion(bucket.ID, key, "null"); err != nil {
			h.Logger.Error("failed to delete null version", "error", err)
		}
		h.Storage.DeleteObject(bucketName, key)
		h.Storage.DeleteVariantsForKey(bucketName, key)

		// Also delete the latest-marked version from DB
		if err := h.DB.DeleteObjectMeta(bucket.ID, key); err != nil {
			h.Logger.Error("failed to delete object metadata", "error", err)
			s3err.WriteError(w, r, s3err.ErrInternalError)
			return
		}
	} else {
		// Unversioned: simple delete
		if err := h.DB.DeleteObjectMeta(bucket.ID, key); err != nil {
			h.Logger.Error("failed to delete object metadata", "error", err)
			s3err.WriteError(w, r, s3err.ErrInternalError)
			return
		}
		if err := h.Storage.DeleteObject(bucketName, key); err != nil {
			h.Logger.Error("failed to delete object from storage", "bucket", bucketName, "key", key, "error", err)
		}
		h.Storage.DeleteVariantsForKey(bucketName, key)
	}

	// Emit webhook event
	if h.Dispatcher != nil {
		h.Dispatcher.Emit(webhook.Event{
			BucketName: bucketName,
			EventType:  "s3:ObjectRemoved:Delete",
			Key:        key,
			Timestamp:  time.Now().UTC(),
		})
	}

	w.WriteHeader(http.StatusNoContent)
}

// serveTransformed serves a resized/optimized derivative of an image object.
// It returns true if it produced the response (cache hit, fresh transform, or a
// conditional 304). It returns false WITHOUT writing anything when it cannot
// transform (open error or decode/encode failure) so the caller falls back to
// streaming the original object untouched. The original bytes are never
// modified — derivatives live in a sibling .<bucket>-cache tree.
func (h *Handler) serveTransformed(w http.ResponseWriter, r *http.Request, bucket *db.Bucket, meta *db.ObjectMeta, tp imageutil.Params) bool {
	cacheKey := storage.VariantCacheKey(meta.Key, meta.VersionID, meta.ETag, tp.Spec())
	variantETag := variantETagFromKey(cacheKey, meta.ETag)

	// Cache hit → serve straight from disk.
	if rc, size, err := h.Storage.GetVariant(bucket.Name, cacheKey); err == nil && rc != nil {
		defer rc.Close()
		if !h.CheckConditionalHeaders(w, r, variantETag, meta.LastModified) {
			return true
		}
		h.writeVariantHeaders(w, variantContentType(meta.ContentType), variantETag, meta.LastModified, size)
		w.WriteHeader(http.StatusOK)
		io.Copy(w, rc)
		return true
	}

	// Miss → open the original and transform it in memory.
	var src io.ReadCloser
	var err error
	if meta.VersionID != "" && meta.VersionID != "null" {
		src, err = h.Storage.GetVersionedObject(bucket.Name, meta.Key, meta.VersionID)
	} else {
		src, err = h.Storage.GetObject(bucket.Name, meta.Key)
	}
	if err != nil {
		return false // fall back to normal serving
	}
	data, ct, terr := imageutil.Transform(src, meta.ContentType, tp)
	src.Close()
	if terr != nil {
		h.Logger.Warn("image transform failed; serving original", "key", meta.Key, "error", terr)
		return false
	}

	// Best-effort cache write — a failure here doesn't affect the response.
	if perr := h.Storage.PutVariant(bucket.Name, cacheKey, data); perr != nil {
		h.Logger.Debug("variant cache write failed", "error", perr)
	}

	if !h.CheckConditionalHeaders(w, r, variantETag, meta.LastModified) {
		return true
	}
	h.writeVariantHeaders(w, ct, variantETag, meta.LastModified, int64(len(data)))
	w.WriteHeader(http.StatusOK)
	w.Write(data)
	return true
}

// writeVariantHeaders sets response headers for a transformed image. Range is
// not honored (the byte length differs from the original), and the response is
// marked inline + cacheable so it renders in <img> tags and survives at CDNs.
func (h *Handler) writeVariantHeaders(w http.ResponseWriter, ct, etag string, lastMod time.Time, size int64) {
	w.Header().Set("Content-Type", ct)
	w.Header().Set("ETag", etag)
	w.Header().Set("Last-Modified", lastMod.UTC().Format(http.TimeFormat))
	w.Header().Set("Accept-Ranges", "none")
	w.Header().Set("Content-Disposition", "inline")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
}

// variantETagFromKey derives a stable, distinct ETag for a derivative from its
// content-addressed cache key (the variant-hash segment), so caches never
// confuse a resized image with the original. Falls back to the original ETag.
func variantETagFromKey(cacheKey, fallback string) string {
	if i := strings.IndexByte(cacheKey, '/'); i >= 0 && len(cacheKey) >= i+33 {
		return "\"" + cacheKey[i+1:i+33] + "\""
	}
	return fallback
}

// variantContentType mirrors the encoder choice in image.Transform: PNG/GIF/WebP
// sources become PNG, everything else becomes JPEG. Used on the cache-hit path
// where the stored content-type isn't persisted separately.
func variantContentType(srcCT string) string {
	switch strings.ToLower(strings.TrimSpace(strings.Split(srcCT, ";")[0])) {
	case "image/png", "image/gif", "image/webp":
		return "image/png"
	default:
		return "image/jpeg"
	}
}

// serveRange handles Range requests
func (h *Handler) serveRange(w http.ResponseWriter, r *http.Request, reader io.ReadCloser, meta *db.ObjectMeta, rangeHeader string) {
	// Parse "bytes=start-end"
	rangeSpec := strings.TrimPrefix(rangeHeader, "bytes=")
	parts := strings.SplitN(rangeSpec, "-", 2)
	if len(parts) != 2 {
		w.Header().Set("Content-Length", strconv.FormatInt(meta.Size, 10))
		w.WriteHeader(http.StatusOK)
		io.Copy(w, reader)
		return
	}

	var start, end int64
	totalSize := meta.Size

	if parts[0] == "" {
		// suffix range: -N means last N bytes
		suffixLen, err := strconv.ParseInt(parts[1], 10, 64)
		if err != nil || suffixLen <= 0 {
			w.Header().Set("Content-Length", strconv.FormatInt(meta.Size, 10))
			w.WriteHeader(http.StatusOK)
			io.Copy(w, reader)
			return
		}
		if suffixLen > totalSize {
			suffixLen = totalSize
		}
		start = totalSize - suffixLen
		end = totalSize - 1
	} else {
		var err error
		start, err = strconv.ParseInt(parts[0], 10, 64)
		if err != nil {
			w.Header().Set("Content-Length", strconv.FormatInt(meta.Size, 10))
			w.WriteHeader(http.StatusOK)
			io.Copy(w, reader)
			return
		}
		if parts[1] == "" {
			end = totalSize - 1
		} else {
			end, err = strconv.ParseInt(parts[1], 10, 64)
			if err != nil {
				end = totalSize - 1
			}
		}
	}

	if start < 0 {
		start = 0
	}
	if end >= totalSize {
		end = totalSize - 1
	}
	if start > end {
		w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
		return
	}

	// Seek to start if possible
	if seeker, ok := reader.(io.Seeker); ok {
		seeker.Seek(start, io.SeekStart)
	} else {
		io.CopyN(io.Discard, reader, start)
	}

	length := end - start + 1
	w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, totalSize))
	w.Header().Set("Content-Length", strconv.FormatInt(length, 10))
	w.WriteHeader(http.StatusPartialContent)
	io.CopyN(w, reader, length)
}

// collectMetadata gathers x-amz-meta-* headers into a JSON string.
// Enforces S3 metadata limits: 2 KB total, individual key/value length limits.
func collectMetadata(r *http.Request) string {
	meta := make(map[string]string)
	totalSize := 0
	for key, vals := range r.Header {
		lower := strings.ToLower(key)
		if strings.HasPrefix(lower, "x-amz-meta-") {
			metaKey := strings.TrimPrefix(lower, "x-amz-meta-")
			if len(metaKey) > maxMetadataKeyLen {
				continue // skip oversized key
			}
			val := vals[0]
			if len(val) > maxMetadataValLen {
				continue // skip oversized value
			}
			entrySize := len(metaKey) + len(val)
			if totalSize+entrySize > maxMetadataSize {
				break // total metadata limit reached
			}
			// Reject CRLF in values to prevent header injection
			if strings.ContainsAny(val, "\r\n") {
				continue
			}
			meta[metaKey] = val
			totalSize += entrySize
		}
	}
	if len(meta) == 0 {
		return "{}"
	}
	data, err := json.Marshal(meta)
	if err != nil {
		return "{}"
	}
	return string(data)
}

// setMetadataHeaders sets x-amz-meta-* response headers from JSON metadata.
func setMetadataHeaders(w http.ResponseWriter, metadata string) {
	if metadata == "" || metadata == "{}" {
		return
	}
	var meta map[string]string
	if err := json.Unmarshal([]byte(metadata), &meta); err != nil {
		return
	}
	for key, val := range meta {
		// Sanitize: strip CRLF to prevent header injection
		val = strings.NewReplacer("\r", "", "\n", "").Replace(val)
		w.Header().Set("X-Amz-Meta-"+key, val)
	}
}

// copyObject handles PUT with X-Amz-Copy-Source header
func (h *Handler) copyObject(w http.ResponseWriter, r *http.Request, cred *db.BucketCredential, destBucket *db.Bucket, destKey, copySource string) {
	// Parse copy source: /bucket/key or bucket/key
	copySource = strings.TrimPrefix(copySource, "/")
	idx := strings.IndexByte(copySource, '/')
	if idx < 0 {
		s3err.WriteError(w, r, s3err.ErrInvalidArgument)
		return
	}
	srcBucketName := copySource[:idx]
	srcKey := copySource[idx+1:]

	if !isValidBucketName(srcBucketName) || !isValidObjectKey(srcKey) {
		s3err.WriteError(w, r, s3err.ErrInvalidArgument)
		return
	}

	// Get source bucket
	srcBucket, ok := h.checkBucketAccess(w, r, cred, srcBucketName)
	if !ok {
		return
	}

	// Get source object metadata
	srcMeta, err := h.DB.GetObjectMeta(srcBucket.ID, srcKey)
	if err != nil {
		h.Logger.Error("db error", "error", err)
		s3err.WriteError(w, r, s3err.ErrInternalError)
		return
	}
	if srcMeta == nil {
		s3err.WriteError(w, r, s3err.ErrNoSuchKey)
		return
	}

	// Check quota before copy
	if !h.checkQuota(w, r, destBucket, srcMeta.Size) {
		return
	}

	// Read source object (use versioned path if source has a version)
	var reader io.ReadCloser
	if srcMeta.VersionID != "" && srcMeta.VersionID != "null" {
		reader, err = h.Storage.GetVersionedObject(srcBucketName, srcKey, srcMeta.VersionID)
	} else {
		reader, err = h.Storage.GetObject(srcBucketName, srcKey)
	}
	if err != nil {
		h.Logger.Error("failed to read source object", "error", err)
		s3err.WriteError(w, r, s3err.ErrInternalError)
		return
	}
	defer reader.Close()

	// Determine destination versioning
	destVersionID := ""
	destVersioned := destBucket.Versioning == "Enabled"
	destSuspended := destBucket.Versioning == "Suspended"
	if destVersioned {
		destVersionID = generateVersionID()
	} else if destSuspended {
		destVersionID = "null"
	}

	// Write to destination
	var size int64
	var etag string
	if destVersionID != "" && destVersionID != "null" {
		size, etag, err = h.Storage.PutVersionedObject(destBucket.Name, destKey, destVersionID, reader)
	} else {
		size, etag, err = h.Storage.PutObject(destBucket.Name, destKey, reader)
	}
	if err != nil {
		h.Logger.Error("failed to write dest object", "error", err)
		s3err.WriteError(w, r, s3err.ErrInternalError)
		return
	}

	now := time.Now().UTC()

	meta := &db.ObjectMeta{
		BucketID:     destBucket.ID,
		Key:          destKey,
		Size:         size,
		ETag:         etag,
		ContentType:  srcMeta.ContentType,
		LastModified: now,
		Metadata:     srcMeta.Metadata,
		VersionID:    destVersionID,
		IsLatest:     true,
	}

	if destVersioned || destSuspended {
		err = h.DB.PutObjectMetaVersioned(meta)
	} else {
		err = h.DB.PutObjectMeta(meta)
	}
	if err != nil {
		h.Logger.Error("failed to save object metadata", "error", err)
		if destVersionID != "" && destVersionID != "null" {
			h.Storage.DeleteVersionedObject(destBucket.Name, destKey, destVersionID)
		} else {
			h.Storage.DeleteObject(destBucket.Name, destKey)
		}
		s3err.WriteError(w, r, s3err.ErrInternalError)
		return
	}

	if destVersionID != "" {
		w.Header().Set("x-amz-version-id", destVersionID)
	}

	// Emit webhook event
	if h.Dispatcher != nil {
		h.Dispatcher.Emit(webhook.Event{
			BucketName: destBucket.Name,
			EventType:  "s3:ObjectCreated:Copy",
			Key:        destKey,
			Size:       size,
			ETag:       etag,
			VersionID:  destVersionID,
			Timestamp:  now,
		})
	}

	result := struct {
		XMLName      string `xml:"CopyObjectResult"`
		LastModified string `xml:"LastModified"`
		ETag         string `xml:"ETag"`
	}{
		LastModified: now.Format("2006-01-02T15:04:05.000Z"),
		ETag:         etag,
	}
	h.writeXML(w, http.StatusOK, result)
}
