package handler

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/onaonbir/Cloodsy-S3/db"
	imageutil "github.com/onaonbir/Cloodsy-S3/image"
	"github.com/onaonbir/Cloodsy-S3/s3err"
	"github.com/onaonbir/Cloodsy-S3/service"
	"github.com/onaonbir/Cloodsy-S3/storage"
)

// PutObject handles PUT /<bucket>/<key>
func (h *Handler) PutObject(w http.ResponseWriter, r *http.Request) {
	cred, ok := h.authenticateRequest(w, r)
	if !ok {
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
	if !h.checkWriteAccess(w, r, cred) {
		return
	}
	bucket, ok := h.checkBucketAccess(w, r, cred, bucketName)
	if !ok {
		return
	}

	if copySource := r.Header.Get("X-Amz-Copy-Source"); copySource != "" {
		h.copyObject(w, r, cred, bucket, key, copySource)
		return
	}

	metadata, err := collectMetadata(r)
	if err != nil {
		h.writeServiceError(w, r, err, "metadata")
		return
	}

	body, checker, err := h.requestBody(r, cred)
	if err != nil {
		h.writeServiceError(w, r, err, "request body")
		return
	}

	contentType := r.Header.Get("Content-Type")
	if contentType == "" {
		contentType = "application/octet-stream"
	}

	res, err := h.Objects.Put(service.PutInput{
		Bucket:       bucket,
		Key:          key,
		Body:         body,
		ContentType:  contentType,
		Metadata:     metadata,
		DeclaredSize: checker.declared,
		MaxSize:      maxObjectSize,
		Verify:       checker.verify,
		EventType:    "s3:ObjectCreated:Put",
	})
	if err != nil {
		h.writeServiceError(w, r, err, "failed to write object")
		return
	}

	w.Header().Set("ETag", res.ETag)
	if res.VersionID != "" {
		w.Header().Set("x-amz-version-id", res.VersionID)
	}
	if checker.checksumAlgo != "" && checker.checksumHash != nil {
		w.Header().Set("x-amz-checksum-"+checker.checksumAlgo, checksumBase64(checker))
	}
	h.setExpirationHeader(w, bucketName, key, res.LastModified)
	w.WriteHeader(http.StatusOK)
}

func checksumBase64(pc *payloadChecker) string {
	if pc.expectedSum != "" {
		return pc.expectedSum
	}
	if pc.chunked != nil {
		return pc.chunked.Trailers["x-amz-checksum-"+pc.checksumAlgo]
	}
	return ""
}

// lookupObject resolves the object row for GET/HEAD honoring ?versionId.
// It writes the error response and returns nil when the object is not servable.
func (h *Handler) lookupObject(w http.ResponseWriter, r *http.Request, bucket *db.Bucket, key string) *db.ObjectMeta {
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
		return nil
	}
	if meta == nil {
		if requestedVersion != "" {
			s3err.WriteError(w, r, s3err.ErrNoSuchVersion)
		} else {
			s3err.WriteError(w, r, s3err.ErrNoSuchKey)
		}
		return nil
	}
	if meta.IsDeleteMarker {
		w.Header().Set("x-amz-delete-marker", "true")
		w.Header().Set("x-amz-version-id", apiVersionID(meta.VersionID))
		if requestedVersion != "" {
			s3err.WriteError(w, r, s3err.ErrMethodNotAllowed)
		} else {
			s3err.WriteError(w, r, s3err.ErrNoSuchKey)
		}
		return nil
	}
	return meta
}

// setObjectHeaders writes the standard metadata headers for GET/HEAD.
func (h *Handler) setObjectHeaders(w http.ResponseWriter, r *http.Request, bucket *db.Bucket, meta *db.ObjectMeta, signed bool) {
	w.Header().Set("Content-Type", meta.ContentType)
	w.Header().Set("ETag", meta.ETag)
	w.Header().Set("Last-Modified", meta.LastModified.UTC().Format(http.TimeFormat))
	w.Header().Set("Accept-Ranges", "bytes")
	if bucket.Versioning != "" || meta.VersionID != "" {
		w.Header().Set("x-amz-version-id", apiVersionID(meta.VersionID))
	}
	h.setExpirationHeader(w, bucket.Name, meta.Key, meta.LastModified)
	setMetadataHeaders(w, meta.Metadata)

	// response-* overrides are only honored on signed requests (as on AWS).
	if signed {
		q := r.URL.Query()
		for param, hdr := range map[string]string{
			"response-content-type":        "Content-Type",
			"response-content-language":    "Content-Language",
			"response-expires":             "Expires",
			"response-cache-control":       "Cache-Control",
			"response-content-disposition": "Content-Disposition",
			"response-content-encoding":    "Content-Encoding",
		} {
			if v := q.Get(param); v != "" && !strings.ContainsAny(v, "\r\n") {
				w.Header().Set(hdr, v)
			}
		}
	}
}

// GetObject handles GET /<bucket>/<key>
func (h *Handler) GetObject(w http.ResponseWriter, r *http.Request) {
	bucketName, key := getBucketAndKey(r)
	if key == "" {
		s3err.WriteError(w, r, s3err.ErrNoSuchKey)
		return
	}

	bucket, cred, ok := h.authenticateOrPublic(w, r, bucketName)
	if !ok {
		return
	}
	meta := h.lookupObject(w, r, bucket, key)
	if meta == nil {
		return
	}

	// Image transform-on-access (?w=&h=&m=&q=). Served from a sibling cache;
	// on any error serveTransformed returns false and we fall through to the
	// normal full-object path, so a transform failure never breaks a download.
	if tp, want := imageutil.ParseParams(r.URL.Query()); want && imageutil.IsImageContentType(meta.ContentType) {
		if h.serveTransformed(w, r, bucket, meta, tp.Quantize(h.Config.Image.DimensionStep)) {
			return
		}
	}

	if !h.checkConditional(w, r, meta) {
		return
	}

	reader, err := h.Objects.Open(bucket, meta)
	if err != nil {
		if storage.IsNotExist(err) {
			h.Logger.Warn("object data missing on disk", "bucket", bucketName, "key", key, "version", meta.VersionID)
			s3err.WriteError(w, r, s3err.ErrNoSuchKey)
			return
		}
		h.Logger.Error("failed to open object", "bucket", bucketName, "key", key, "error", err)
		s3err.WriteError(w, r, s3err.ErrInternalError)
		return
	}
	defer reader.Close()

	h.setObjectHeaders(w, r, bucket, meta, cred != nil)

	if rangeHeader := r.Header.Get("Range"); rangeHeader != "" {
		if ir := r.Header.Get("If-Range"); ir == "" || ifRangeMatches(ir, meta) {
			h.serveRange(w, r, reader, meta, rangeHeader)
			return
		}
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

	bucket, cred, ok := h.authenticateOrPublic(w, r, bucketName)
	if !ok {
		return
	}
	meta := h.lookupObject(w, r, bucket, key)
	if meta == nil {
		return
	}
	if !h.checkConditional(w, r, meta) {
		return
	}

	h.setObjectHeaders(w, r, bucket, meta, cred != nil)

	if rangeHeader := r.Header.Get("Range"); rangeHeader != "" {
		if start, end, ok := parseRange(rangeHeader, meta.Size); ok {
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, meta.Size))
			w.Header().Set("Content-Length", strconv.FormatInt(end-start+1, 10))
			w.WriteHeader(http.StatusPartialContent)
			return
		}
	}
	w.Header().Set("Content-Length", strconv.FormatInt(meta.Size, 10))
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

	res, err := h.Objects.Delete(bucket, key, r.URL.Query().Get("versionId"))
	if err != nil {
		if errors.Is(err, service.ErrNoSuchVersion) {
			// S3 returns 204 for deleting a version that does not exist.
			w.WriteHeader(http.StatusNoContent)
			return
		}
		h.writeServiceError(w, r, err, "failed to delete object")
		return
	}
	if res.VersionID != "" {
		w.Header().Set("x-amz-version-id", res.VersionID)
	}
	if res.DeleteMarker {
		w.Header().Set("x-amz-delete-marker", "true")
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- image transforms ------------------------------------------------------

// serveTransformed serves a resized/optimized derivative of an image object.
// It returns true if it produced the response (cache hit, fresh transform, or a
// conditional 304). It returns false WITHOUT writing anything when it cannot
// transform (open error or decode/encode failure) so the caller falls back to
// streaming the original object untouched. The original bytes are never
// modified — derivatives live in a sibling .<bucket>-cache tree.
func (h *Handler) serveTransformed(w http.ResponseWriter, r *http.Request, bucket *db.Bucket, meta *db.ObjectMeta, tp imageutil.Params) bool {
	if h.Config.Image.MaxSourceBytes > 0 && meta.Size > h.Config.Image.MaxSourceBytes {
		return false
	}
	cacheKey := storage.VariantCacheKey(meta.Key, meta.VersionID, meta.ETag, tp.Spec())
	variantETag := variantETagFromKey(cacheKey, meta.ETag)

	// Cache hit → serve straight from disk.
	if rc, size, err := h.Storage.GetVariant(bucket.Name, cacheKey); err == nil && rc != nil {
		defer rc.Close()
		if !h.checkConditionalETag(w, r, variantETag, meta.LastModified) {
			return true
		}
		head := make([]byte, 512)
		n, _ := io.ReadFull(rc, head)
		h.writeVariantHeaders(w, http.DetectContentType(head[:n]), variantETag, meta.LastModified, size)
		w.WriteHeader(http.StatusOK)
		io.Copy(w, io.MultiReader(bytes.NewReader(head[:n]), rc))
		return true
	}

	// Miss → transform (bounded concurrency).
	if !h.Transforms.Acquire(r.Context()) {
		return false
	}
	defer h.Transforms.Release()

	src, err := h.Objects.Open(bucket, meta)
	if err != nil {
		return false // fall back to normal serving
	}
	data, ct, terr := imageutil.Transform(io.LimitReader(src, h.Config.Image.MaxSourceBytes+1), meta.ContentType, tp)
	src.Close()
	if terr != nil {
		h.Logger.Warn("image transform failed; serving original", "key", meta.Key, "error", terr)
		return false
	}

	// Best-effort cache write — a failure here doesn't affect the response.
	if perr := h.Storage.PutVariant(bucket.Name, cacheKey, data); perr != nil {
		h.Logger.Debug("variant cache write failed", "error", perr)
	}

	if !h.checkConditionalETag(w, r, variantETag, meta.LastModified) {
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
	w.Header().Set("Vary", "Accept")
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

// --- ranges ----------------------------------------------------------------

// parseRange parses a single "bytes=start-end" range against size. Multi-range
// requests and unparsable ranges return ok=false (S3 then serves the whole
// object). An unsatisfiable range returns ok=false with start=-1.
func parseRange(rangeHeader string, size int64) (start, end int64, ok bool) {
	spec, found := strings.CutPrefix(strings.TrimSpace(rangeHeader), "bytes=")
	if !found || strings.Contains(spec, ",") {
		return 0, 0, false
	}
	first, last, found := strings.Cut(spec, "-")
	if !found {
		return 0, 0, false
	}
	first, last = strings.TrimSpace(first), strings.TrimSpace(last)
	if first == "" {
		// suffix range: -N means last N bytes
		n, err := strconv.ParseInt(last, 10, 64)
		if err != nil || n <= 0 {
			return -1, 0, false
		}
		if size == 0 {
			return -1, 0, false
		}
		if n > size {
			n = size
		}
		return size - n, size - 1, true
	}
	s, err := strconv.ParseInt(first, 10, 64)
	if err != nil || s < 0 {
		return 0, 0, false
	}
	e := size - 1
	if last != "" {
		e, err = strconv.ParseInt(last, 10, 64)
		if err != nil || e < s {
			return 0, 0, false
		}
		if e >= size {
			e = size - 1
		}
	}
	if s >= size {
		return -1, 0, false
	}
	return s, e, true
}

func ifRangeMatches(ifRange string, meta *db.ObjectMeta) bool {
	ifRange = strings.TrimSpace(ifRange)
	if strings.HasPrefix(ifRange, "\"") || strings.HasPrefix(ifRange, "W/") {
		return etagMatch(ifRange, meta.ETag)
	}
	if t, err := http.ParseTime(ifRange); err == nil {
		return !meta.LastModified.Truncate(time.Second).After(t)
	}
	return false
}

// serveRange handles Range requests
func (h *Handler) serveRange(w http.ResponseWriter, r *http.Request, reader io.ReadCloser, meta *db.ObjectMeta, rangeHeader string) {
	start, end, ok := parseRange(rangeHeader, meta.Size)
	if !ok {
		if start == -1 {
			w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", meta.Size))
			s3err.WriteError(w, r, s3err.ErrInvalidRange)
			return
		}
		w.Header().Set("Content-Length", strconv.FormatInt(meta.Size, 10))
		w.WriteHeader(http.StatusOK)
		io.Copy(w, reader)
		return
	}

	if seeker, ok := reader.(io.Seeker); ok {
		if _, err := seeker.Seek(start, io.SeekStart); err != nil {
			s3err.WriteError(w, r, s3err.ErrInternalError)
			return
		}
	} else if _, err := io.CopyN(io.Discard, reader, start); err != nil {
		s3err.WriteError(w, r, s3err.ErrInternalError)
		return
	}

	length := end - start + 1
	w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, meta.Size))
	w.Header().Set("Content-Length", strconv.FormatInt(length, 10))
	w.WriteHeader(http.StatusPartialContent)
	io.CopyN(w, reader, length)
}

// --- copy ------------------------------------------------------------------

// parseCopySource decodes "/bucket/key[?versionId=v]" (URL-encoded by SDKs).
func parseCopySource(raw string) (bucket, key, versionID string, err error) {
	raw = strings.TrimPrefix(raw, "/")
	if i := strings.Index(raw, "?versionId="); i >= 0 {
		versionID = raw[i+len("?versionId="):]
		raw = raw[:i]
	}
	decoded, derr := url.PathUnescape(raw)
	if derr != nil {
		return "", "", "", derr
	}
	b, k, found := strings.Cut(decoded, "/")
	if !found || b == "" || k == "" {
		return "", "", "", fmt.Errorf("invalid copy source")
	}
	return b, k, versionID, nil
}

// copyObject handles PUT with X-Amz-Copy-Source header
func (h *Handler) copyObject(w http.ResponseWriter, r *http.Request, cred *db.BucketCredential, destBucket *db.Bucket, destKey, copySource string) {
	srcBucketName, srcKey, srcVersion, err := parseCopySource(copySource)
	if err != nil || !isValidBucketName(srcBucketName) || !isValidObjectKey(srcKey) {
		s3err.WriteError(w, r, s3err.ErrInvalidArgument)
		return
	}

	srcBucket, ok := h.checkBucketAccess(w, r, cred, srcBucketName)
	if !ok {
		return
	}

	var srcMeta *db.ObjectMeta
	if srcVersion != "" {
		srcMeta, err = h.DB.GetObjectMetaByVersion(srcBucket.ID, srcKey, srcVersion)
	} else {
		srcMeta, err = h.DB.GetObjectMeta(srcBucket.ID, srcKey)
	}
	if err != nil {
		h.Logger.Error("db error", "error", err)
		s3err.WriteError(w, r, s3err.ErrInternalError)
		return
	}
	if srcMeta == nil || (srcMeta.IsDeleteMarker && srcVersion == "") {
		if srcVersion != "" {
			s3err.WriteError(w, r, s3err.ErrNoSuchVersion)
		} else {
			s3err.WriteError(w, r, s3err.ErrNoSuchKey)
		}
		return
	}
	if srcMeta.IsDeleteMarker {
		s3err.WriteError(w, r, s3err.ErrInvalidRequest)
		return
	}

	// Conditional copy headers.
	if v := r.Header.Get("x-amz-copy-source-if-match"); v != "" && !etagMatch(v, srcMeta.ETag) {
		s3err.WriteError(w, r, s3err.ErrPreconditionFailed)
		return
	}
	if v := r.Header.Get("x-amz-copy-source-if-none-match"); v != "" && etagMatch(v, srcMeta.ETag) {
		s3err.WriteError(w, r, s3err.ErrPreconditionFailed)
		return
	}
	if v := r.Header.Get("x-amz-copy-source-if-unmodified-since"); v != "" {
		if t, err := http.ParseTime(v); err == nil && srcMeta.LastModified.Truncate(time.Second).After(t) {
			s3err.WriteError(w, r, s3err.ErrPreconditionFailed)
			return
		}
	}
	if v := r.Header.Get("x-amz-copy-source-if-modified-since"); v != "" {
		if t, err := http.ParseTime(v); err == nil && !srcMeta.LastModified.Truncate(time.Second).After(t) {
			s3err.WriteError(w, r, s3err.ErrPreconditionFailed)
			return
		}
	}

	directive := strings.ToUpper(r.Header.Get("x-amz-metadata-directive"))
	sameObject := srcBucket.ID == destBucket.ID && srcKey == destKey && srcVersion == ""
	if sameObject && directive != "REPLACE" {
		s3err.WriteErrorMsg(w, r, s3err.ErrInvalidRequest, "This copy request is illegal because it is trying to copy an object to itself without changing the object's metadata, storage class, website redirect location or encryption attributes.")
		return
	}

	contentType := srcMeta.ContentType
	metadata := srcMeta.Metadata
	if directive == "REPLACE" {
		metadata, err = collectMetadata(r)
		if err != nil {
			h.writeServiceError(w, r, err, "metadata")
			return
		}
		if ct := r.Header.Get("Content-Type"); ct != "" {
			contentType = ct
		}
	}

	reader, err := h.Objects.Open(srcBucket, srcMeta)
	if err != nil {
		h.Logger.Error("failed to read source object", "error", err)
		s3err.WriteError(w, r, s3err.ErrInternalError)
		return
	}
	defer reader.Close()

	res, err := h.Objects.Put(service.PutInput{
		Bucket:       destBucket,
		Key:          destKey,
		Body:         reader,
		ContentType:  contentType,
		Metadata:     metadata,
		DeclaredSize: srcMeta.Size,
		MaxSize:      maxObjectSize,
		EventType:    "s3:ObjectCreated:Copy",
	})
	if err != nil {
		h.writeServiceError(w, r, err, "failed to write dest object")
		return
	}

	if res.VersionID != "" {
		w.Header().Set("x-amz-version-id", res.VersionID)
	}
	if srcBucket.Versioning != "" || srcMeta.VersionID != "" {
		w.Header().Set("x-amz-copy-source-version-id", apiVersionID(srcMeta.VersionID))
	}

	result := struct {
		XMLName      string `xml:"CopyObjectResult"`
		Xmlns        string `xml:"xmlns,attr"`
		LastModified string `xml:"LastModified"`
		ETag         string `xml:"ETag"`
	}{
		Xmlns:        "http://s3.amazonaws.com/doc/2006-03-01/",
		LastModified: res.LastModified.Format("2006-01-02T15:04:05.000Z"),
		ETag:         res.ETag,
	}
	h.writeXML(w, http.StatusOK, result)
}
