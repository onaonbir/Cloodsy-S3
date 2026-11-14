package admin

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/onaonbir/Cloodsy-S3/db"
	"github.com/onaonbir/Cloodsy-S3/httpx"
	imageutil "github.com/onaonbir/Cloodsy-S3/image"
	"github.com/onaonbir/Cloodsy-S3/service"
	"github.com/onaonbir/Cloodsy-S3/storage"
)

// adminCopyMaxSize bounds admin copies/moves. The S3 API limits CopyObject to
// 5 GB; the admin API copies anything a multipart upload could have created.
const adminCopyMaxSize = service.MaxMultipartSize

// loadBucket writes 404 and returns nil when the bucket does not exist.
func (h *Handler) loadBucket(w http.ResponseWriter, name string) *db.Bucket {
	bucket, err := h.DB.GetBucket(name)
	if err != nil {
		h.Logger.Error("get bucket error", "bucket", httpx.SanitizeLog(name), "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return nil
	}
	if bucket == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "bucket not found"})
		return nil
	}
	return bucket
}

// diskPath returns the absolute data file of a version ("" for markers).
func (h *Handler) diskPath(bucket string, m *db.ObjectMeta) string {
	if m.IsDeleteMarker {
		return ""
	}
	p, err := h.Storage.ObjectPath(bucket, m.Key, m.VersionID)
	if err != nil {
		return ""
	}
	return p
}

// variantInfo describes the optimized image variant the upload optimizer
// stores for a version, or nil when optimization does not apply.
func (h *Handler) variantInfo(bucket string, m *db.ObjectMeta) interface{} {
	ic := h.Config.Image
	if m.IsDeleteMarker || !ic.Enabled || !imageutil.IsImageContentType(m.ContentType) {
		return nil
	}
	if ic.MaxSourceBytes > 0 && m.Size > ic.MaxSourceBytes {
		return nil
	}
	cacheKey := imageutil.OptimizedCacheKey(m.Key, m.VersionID, m.ETag, imageutil.EffectiveQuality(ic.Quality))
	p, err := h.Storage.VariantPath(bucket, cacheKey)
	if err != nil {
		return nil
	}
	out := map[string]interface{}{"exists": false, "disk_path": p, "size": int64(0)}
	if fi, err := os.Lstat(p); err == nil && fi.Mode().IsRegular() {
		out["exists"] = true
		out["size"] = fi.Size()
	}
	return out
}

// userMetadata returns the x-amz-meta-* part of stored metadata.
func userMetadata(stored string) map[string]string {
	user, _ := service.SplitMetadata(stored)
	return user
}

// objectInfo builds the GET .../object/info document. ok=false means the
// key has no versions at all.
func (h *Handler) objectInfo(bucket *db.Bucket, key string) (map[string]interface{}, bool, error) {
	rows, err := h.DB.ListVersionsForKey(bucket.ID, key)
	if err != nil {
		return nil, false, err
	}
	if len(rows) == 0 {
		return nil, false, nil
	}
	latest := &rows[0]
	for i := range rows {
		if rows[i].IsLatest {
			latest = &rows[i]
			break
		}
	}
	bucketDir, _ := h.Storage.BucketPath(bucket.Name)

	versions := make([]map[string]interface{}, 0, len(rows))
	for i := range rows {
		m := &rows[i]
		versions = append(versions, map[string]interface{}{
			"version_id":       apiVersionID(m.VersionID),
			"size":             m.Size,
			"etag":             m.ETag,
			"last_modified":    fmtTime(m.LastModified),
			"is_latest":        m.IsLatest,
			"is_delete_marker": m.IsDeleteMarker,
			"disk_path":        h.diskPath(bucket.Name, m),
		})
	}

	info := map[string]interface{}{
		"key":           key,
		"size":          latest.Size,
		"etag":          latest.ETag,
		"content_type":  latest.ContentType,
		"last_modified": fmtTime(latest.LastModified),
		"version_id":    latest.VersionID,
		"metadata":      userMetadata(latest.Metadata),
		"disk_path":     h.diskPath(bucket.Name, latest),
		"bucket_dir":    bucketDir,
		"variant":       h.variantInfo(bucket.Name, latest),
		"versions":      versions,
	}
	if latest.IsDeleteMarker {
		info["size"] = int64(0)
		info["metadata"] = map[string]string{}
	}
	return info, true, nil
}

// handleObjectInfo returns everything known about one key.
// GET /admin/buckets/{bucket}/object/info?key=
func (h *Handler) handleObjectInfo(w http.ResponseWriter, r *http.Request, bucketName string) {
	bucket := h.loadBucket(w, bucketName)
	if bucket == nil {
		return
	}
	key := r.URL.Query().Get("key")
	if msg := validateKey(key); msg != "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": msg})
		return
	}
	info, ok, err := h.objectInfo(bucket, key)
	if err != nil {
		h.Logger.Error("object info error", "bucket", bucketName, "key", httpx.SanitizeLog(key), "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "object not found"})
		return
	}
	writeJSON(w, http.StatusOK, info)
}

// isTokenChar matches RFC 7230 tchar: metadata keys become header names.
func isTokenChar(c byte) bool {
	switch {
	case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		return true
	}
	return strings.IndexByte("!#$%&'*+-.^_`|~", c) >= 0
}

// normalizeUserMetadata lower-cases keys (S3 header semantics) and rejects
// keys that cannot be sent as x-amz-meta-<key> headers. Size and value rules
// are enforced by service.EncodeMetadata, exactly as on the S3 API.
func normalizeUserMetadata(in map[string]string) (map[string]string, string) {
	out := make(map[string]string, len(in))
	for k, v := range in {
		lk := strings.ToLower(strings.TrimSpace(k))
		lk = strings.TrimPrefix(lk, "x-amz-meta-")
		if lk == "" {
			return nil, "metadata keys must not be empty"
		}
		if lk[0] == '$' {
			return nil, "metadata keys must not start with '$'"
		}
		for i := 0; i < len(lk); i++ {
			if !isTokenChar(lk[i]) {
				return nil, fmt.Sprintf("invalid metadata key %q", k)
			}
		}
		if _, dup := out[lk]; dup {
			return nil, fmt.Sprintf("duplicate metadata key %q", lk)
		}
		out[lk] = v
	}
	return out, ""
}

func validContentType(ct string) bool {
	return len(ct) <= 256 && !strings.ContainsAny(ct, "\r\n\x00")
}

// writeObjectWriteError maps service errors of a copy/put to responses.
func (h *Handler) writeObjectWriteError(w http.ResponseWriter, err error, logMsg string, logArgs ...interface{}) {
	switch {
	case errors.Is(err, service.ErrQuotaExceeded):
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "quota exceeded"})
	case errors.Is(err, service.ErrTooLarge):
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "object too large"})
	case errors.Is(err, service.ErrMetadataTooLarge):
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": fmt.Sprintf("metadata too large (max %d bytes total, keys %d, values %d)", service.MaxMetadataSize, service.MaxMetadataKeyLen, service.MaxMetadataValLen)})
	case errors.Is(err, service.ErrInvalidMetadata):
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "metadata values must not contain line breaks"})
	case errors.Is(err, service.ErrNoSuchKey), storage.IsNotExist(err):
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "object not found"})
	default:
		h.Logger.Error(logMsg, append(logArgs, "error", err)...)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
	}
}

// handleUpdateObjectMeta replaces an object's content type and user metadata
// the way S3 CopyObject onto itself with MetadataDirective=REPLACE does (on a
// versioned bucket this creates a new version). System headers stored with
// the object (Cache-Control, Content-Disposition, ...) are kept.
// PUT /admin/buckets/{bucket}/object/meta?key=
func (h *Handler) handleUpdateObjectMeta(w http.ResponseWriter, r *http.Request, bucketName string) {
	bucket := h.loadBucket(w, bucketName)
	if bucket == nil {
		return
	}
	key := r.URL.Query().Get("key")
	if msg := validateKey(key); msg != "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": msg})
		return
	}
	var req struct {
		ContentType string            `json:"content_type"`
		Metadata    map[string]string `json:"metadata"`
	}
	if !decodeJSON(w, r, &req, false) {
		return
	}
	if !validContentType(req.ContentType) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid content_type"})
		return
	}
	user, msg := normalizeUserMetadata(req.Metadata)
	if msg != "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": msg})
		return
	}

	src, err := h.DB.GetObjectMeta(bucket.ID, key)
	if err != nil {
		h.Logger.Error("metadata lookup error", "bucket", bucketName, "key", httpx.SanitizeLog(key), "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	if src == nil || src.IsDeleteMarker {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "object not found"})
		return
	}
	_, system := service.SplitMetadata(src.Metadata)
	metadata, err := service.EncodeMetadata(user, system)
	if err != nil {
		h.writeObjectWriteError(w, err, "encode metadata error")
		return
	}
	contentType := src.ContentType
	if req.ContentType != "" {
		contentType = req.ContentType
	}

	clearDeadlines(w)
	safeKey := httpx.SanitizeLog(key)
	if _, err := h.Objects.Copy(service.CopyInput{
		SrcBucket: bucket, Src: src, DstBucket: bucket, DstKey: key,
		ContentType: contentType, Metadata: metadata, MaxSize: adminCopyMaxSize,
	}); err != nil {
		h.writeObjectWriteError(w, err, "metadata update error", "bucket", bucketName, "key", safeKey)
		return
	}
	h.Logger.Info("object metadata updated via admin API", "bucket", bucketName, "key", safeKey)

	info, ok, err := h.objectInfo(bucket, key)
	if err != nil || !ok {
		h.Logger.Error("object info after metadata update", "bucket", bucketName, "key", safeKey, "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	writeJSON(w, http.StatusOK, info)
}

// handleCopyObject copies (or moves) one object server-side.
// POST /admin/buckets/{bucket}/objects/copy
func (h *Handler) handleCopyObject(w http.ResponseWriter, r *http.Request, bucketName string) {
	srcBucket := h.loadBucket(w, bucketName)
	if srcBucket == nil {
		return
	}
	var req struct {
		SourceKey  string `json:"source_key"`
		DestBucket string `json:"dest_bucket"`
		DestKey    string `json:"dest_key"`
		Move       bool   `json:"move"`
	}
	if !decodeJSON(w, r, &req, false) {
		return
	}
	if msg := validateKey(req.SourceKey); msg != "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "source_key: " + msg})
		return
	}
	if msg := validateUploadKey(req.DestKey); msg != "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "dest_key: " + msg})
		return
	}
	dstBucket := srcBucket
	if req.DestBucket != "" && req.DestBucket != srcBucket.Name {
		b, err := h.DB.GetBucket(req.DestBucket)
		if err != nil {
			h.Logger.Error("get bucket error", "bucket", httpx.SanitizeLog(req.DestBucket), "error", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
			return
		}
		if b == nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "destination bucket not found"})
			return
		}
		dstBucket = b
	}
	if dstBucket.ID == srcBucket.ID && req.DestKey == req.SourceKey {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "destination is the source object"})
		return
	}
	if dstBucket.ID == srcBucket.ID && caseOnlyDifference(req.SourceKey, req.DestKey) {
		writeJSON(w, http.StatusConflict, map[string]string{"error": errCaseOnlyRename})
		return
	}

	src, err := h.DB.GetObjectMeta(srcBucket.ID, req.SourceKey)
	if err != nil {
		h.Logger.Error("copy lookup error", "bucket", bucketName, "key", httpx.SanitizeLog(req.SourceKey), "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	if src == nil || src.IsDeleteMarker {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "object not found"})
		return
	}

	clearDeadlines(w)
	res, moveErr, err := h.copyOne(srcBucket, src, dstBucket, req.DestKey, req.Move)
	safeSrc, safeDst := httpx.SanitizeLog(req.SourceKey), httpx.SanitizeLog(req.DestKey)
	if errors.Is(err, service.ErrSourceChanged) {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "source changed during move"})
		return
	}
	if err != nil {
		h.writeObjectWriteError(w, err, "copy object error", "bucket", bucketName, "key", safeSrc, "dest_bucket", dstBucket.Name, "dest_key", safeDst)
		return
	}
	if moveErr != nil {
		h.Logger.Error("move: source delete failed", "bucket", bucketName, "key", safeSrc, "error", moveErr)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "copied, but deleting the source failed"})
		return
	}
	op := "copied"
	if req.Move {
		op = "moved"
	}
	h.Logger.Info("object "+op+" via admin API", "bucket", bucketName, "key", safeSrc, "dest_bucket", dstBucket.Name, "dest_key", safeDst, "size", res.Size)
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"key":        req.DestKey,
		"size":       res.Size,
		"etag":       res.ETag,
		"version_id": res.VersionID,
	})
}

// errCaseOnlyRename is returned for a copy/move between two keys of one
// bucket that differ only in letter case: on a case-insensitive filesystem
// (Windows, macOS) both keys are the same file, so the copy would overwrite
// the source and a move would then delete the only copy.
const errCaseOnlyRename = "keys differ only in letter case; rename through a temporary name"

// caseOnlyDifference reports whether a and b are different keys that a
// case-insensitive filesystem may store as the same path. It is checked on
// every platform: the data directory may live on such a filesystem (a
// mounted volume, a container on a Mac/Windows host) whatever the OS.
func caseOnlyDifference(a, b string) bool {
	return a != b && strings.EqualFold(a, b)
}

// copyOne copies src to dst/dstKey (metadata and content type preserved).
// A move holds the source and destination key locks across copy and delete,
// refuses a source that changed since it was looked up
// (service.ErrSourceChanged) and deletes only the source versions that
// existed when the copy started. moveErr reports a failed source delete
// after a good copy.
func (h *Handler) copyOne(srcBucket *db.Bucket, src *db.ObjectMeta, dst *db.Bucket, dstKey string, move bool) (res *service.PutResult, moveErr, err error) {
	in := service.CopyInput{
		SrcBucket: srcBucket, Src: src, DstBucket: dst, DstKey: dstKey,
		ContentType: src.ContentType, Metadata: src.Metadata, MaxSize: adminCopyMaxSize,
	}
	if move {
		return h.Objects.Move(in)
	}
	res, err = h.Objects.Copy(in)
	return res, nil, err
}

// handleCopyPrefix copies or moves every current object under a folder
// prefix to another prefix (folder rename / copy).
// POST /admin/buckets/{bucket}/objects/copy-prefix
func (h *Handler) handleCopyPrefix(w http.ResponseWriter, r *http.Request, bucketName string) {
	srcBucket := h.loadBucket(w, bucketName)
	if srcBucket == nil {
		return
	}
	var req struct {
		SourcePrefix string `json:"source_prefix"`
		DestBucket   string `json:"dest_bucket"`
		DestPrefix   string `json:"dest_prefix"`
		Move         bool   `json:"move"`
	}
	if !decodeJSON(w, r, &req, false) {
		return
	}
	for name, p := range map[string]string{"source_prefix": req.SourcePrefix, "dest_prefix": req.DestPrefix} {
		if msg := validatePrefix(p); msg != "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": name + ": " + msg})
			return
		}
		if !strings.HasSuffix(p, "/") {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": name + " must end with '/'"})
			return
		}
	}
	dstBucket := srcBucket
	if req.DestBucket != "" && req.DestBucket != srcBucket.Name {
		b, err := h.DB.GetBucket(req.DestBucket)
		if err != nil {
			h.Logger.Error("get bucket error", "bucket", httpx.SanitizeLog(req.DestBucket), "error", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
			return
		}
		if b == nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "destination bucket not found"})
			return
		}
		dstBucket = b
	}
	if dstBucket.ID == srcBucket.ID && strings.HasPrefix(req.DestPrefix, req.SourcePrefix) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "dest_prefix must not be inside source_prefix"})
		return
	}
	if dstBucket.ID == srcBucket.ID && caseOnlyDifference(req.SourcePrefix, req.DestPrefix) {
		writeJSON(w, http.StatusConflict, map[string]string{"error": errCaseOnlyRename})
		return
	}

	// One prefix copy per bucket at a time, and never alongside a storage
	// move, reprocess or delete of either bucket.
	if ok, op := h.tryLockBucket(srcBucket.Name, "copy-prefix"); !ok {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "bucket is busy: " + op + " in progress"})
		return
	}
	defer h.unlockBucket(srcBucket.Name)
	if dstBucket.ID != srcBucket.ID {
		if ok, op := h.tryLockBucket(dstBucket.Name, "copy-prefix"); !ok {
			writeJSON(w, http.StatusConflict, map[string]string{"error": "bucket is busy: " + op + " in progress"})
			return
		}
		defer h.unlockBucket(dstBucket.Name)
	}

	ctx := r.Context()
	clearDeadlines(w)
	safeSrc, safeDst := httpx.SanitizeLog(req.SourcePrefix), httpx.SanitizeLog(req.DestPrefix)
	copied, failed := 0, 0
	marker := ""
	for {
		if ctx.Err() != nil {
			h.Logger.Warn("copy prefix stopped: client went away", "bucket", bucketName, "prefix", safeSrc, "copied", copied, "failed", failed)
			return
		}
		objects, _, truncated, next, err := h.DB.ListObjectsMeta(srcBucket.ID, req.SourcePrefix, marker, "", deleteBatch)
		if err != nil {
			h.Logger.Error("copy prefix list error", "bucket", bucketName, "prefix", safeSrc, "error", err)
			writeJSON(w, http.StatusInternalServerError, map[string]interface{}{"error": "internal error", "copied": copied, "failed": failed})
			return
		}
		for i := range objects {
			if ctx.Err() != nil {
				h.Logger.Warn("copy prefix stopped: client went away", "bucket", bucketName, "prefix", safeSrc, "copied", copied, "failed", failed)
				return
			}
			src := &objects[i]
			dstKey := req.DestPrefix + strings.TrimPrefix(src.Key, req.SourcePrefix)
			if validateUploadKey(dstKey) != "" || (dstBucket.ID == srcBucket.ID && caseOnlyDifference(src.Key, dstKey)) {
				failed++
				continue
			}
			_, moveErr, err := h.copyOne(srcBucket, src, dstBucket, dstKey, req.Move)
			if err != nil || moveErr != nil {
				h.Logger.Warn("copy prefix: object failed", "bucket", bucketName, "key", httpx.SanitizeLog(src.Key), "copy_error", err, "delete_error", moveErr)
				failed++
				continue
			}
			copied++
		}
		if !truncated || next == "" || next == marker {
			break
		}
		marker = next
	}

	h.Logger.Info("prefix copied via admin API", "bucket", bucketName, "prefix", safeSrc, "dest_bucket", dstBucket.Name, "dest_prefix", safeDst, "move", req.Move, "copied", copied, "failed", failed)
	writeJSON(w, http.StatusOK, map[string]interface{}{"copied": copied, "failed": failed})
}

// handleSearchObjects lists current objects whose key contains q
// (case-insensitive), recursively under prefix.
// GET /admin/buckets/{bucket}/search?q=&prefix=&marker=&max-keys=
func (h *Handler) handleSearchObjects(w http.ResponseWriter, r *http.Request, bucketName string) {
	bucket := h.loadBucket(w, bucketName)
	if bucket == nil {
		return
	}
	q := r.URL.Query()
	prefix, marker, search := q.Get("prefix"), q.Get("marker"), q.Get("q")
	if len(prefix) > prefixMaxLen || strings.ContainsRune(prefix, 0) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid prefix"})
		return
	}
	if len(search) > maxKeyLen || strings.ContainsRune(search, 0) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid q"})
		return
	}
	maxKeys := listDefKeys
	if v := q.Get("max-keys"); v != "" {
		if mk, err := strconv.Atoi(v); err == nil && mk > 0 {
			maxKeys = mk
			if maxKeys > listMaxKeys {
				maxKeys = listMaxKeys
			}
		}
	}

	objects, truncated, err := h.DB.SearchObjects(bucket.ID, prefix, search, marker, maxKeys)
	if err != nil {
		h.Logger.Error("search objects error", "bucket", bucketName, "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	result := make([]map[string]interface{}, 0, len(objects))
	for _, obj := range objects {
		result = append(result, map[string]interface{}{
			"key":           obj.Key,
			"size":          obj.Size,
			"etag":          obj.ETag,
			"content_type":  obj.ContentType,
			"last_modified": fmtTime(obj.LastModified),
		})
	}
	nextMarker := ""
	if truncated && len(objects) > 0 {
		nextMarker = objects[len(objects)-1].Key
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"objects":     result,
		"prefixes":    []string{},
		"prefix":      prefix,
		"truncated":   truncated,
		"next_marker": nextMarker,
	})
}

// handlePrefixStats counts current objects and bytes under a prefix.
// GET /admin/buckets/{bucket}/prefix-stats?prefix=
func (h *Handler) handlePrefixStats(w http.ResponseWriter, r *http.Request, bucketName string) {
	bucket := h.loadBucket(w, bucketName)
	if bucket == nil {
		return
	}
	prefix := r.URL.Query().Get("prefix")
	if len(prefix) > prefixMaxLen || strings.ContainsRune(prefix, 0) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid prefix"})
		return
	}
	objects, bytes, err := h.DB.PrefixStats(bucket.ID, prefix)
	if err != nil {
		h.Logger.Error("prefix stats error", "bucket", bucketName, "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"prefix": prefix, "objects": objects, "bytes": bytes})
}
