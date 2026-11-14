package admin

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/onaonbir/Cloodsy-S3/db"
	"github.com/onaonbir/Cloodsy-S3/httpx"
	"github.com/onaonbir/Cloodsy-S3/service"
	"github.com/onaonbir/Cloodsy-S3/storage"
)

// Capabilities advertised in GET /admin/status so clients can feature-detect.
var adminFeatures = []string{
	"object-download", "object-upload", "object-versions",
	"object-info", "object-range", "object-copy", "object-search", "prefix-stats", "object-metadata",
	"multipart", "lifecycle-v2", "webhooks-v2", "maintenance", "sessions", "logs", "config", "metrics",
}

var (
	errFolderMarkerBody = errors.New("folder marker must have an empty body")
	errIncompleteUpload = errors.New("request body shorter than Content-Length")
)

// isObjectContentPath reports whether an /admin/... URL path addresses
// /admin/buckets/{bucket}/object. Shared by the router and by the listener,
// which exempts uploads to this route from the JSON body cap.
func isObjectContentPath(urlPath string) (bucket string, ok bool) {
	p := strings.TrimSuffix(strings.TrimPrefix(urlPath, "/admin"), "/")
	parts := strings.Split(strings.TrimPrefix(p, "/"), "/")
	if len(parts) == 3 && parts[0] == "buckets" && parts[1] != "" && parts[2] == "object" {
		return parts[1], true
	}
	return "", false
}

// isPartUploadPath reports whether an /admin/... URL path addresses
// /admin/buckets/{bucket}/uploads/{id}/parts/{n}.
func isPartUploadPath(urlPath string) bool {
	p := strings.TrimSuffix(strings.TrimPrefix(urlPath, "/admin"), "/")
	parts := strings.Split(strings.TrimPrefix(p, "/"), "/")
	return len(parts) == 6 && parts[0] == "buckets" && parts[1] != "" && parts[2] == "uploads" &&
		parts[3] != "" && parts[4] == "parts" && parts[5] != ""
}

// isObjectUpload reports whether r is a streaming upload (single object or
// multipart part), which must not be subject to the JSON body cap.
func isObjectUpload(r *http.Request) bool {
	if r.Method != http.MethodPut {
		return false
	}
	if _, ok := isObjectContentPath(r.URL.Path); ok {
		return true
	}
	return isPartUploadPath(r.URL.Path)
}

// validateUploadKey applies validateKey plus the S3 API's extra rule that
// "." segments are refused (they are ambiguous on disk).
func validateUploadKey(k string) string {
	if msg := validateKey(k); msg != "" {
		return msg
	}
	for _, seg := range strings.Split(k, "/") {
		if seg == "." {
			return "object key must not contain '.' segments"
		}
	}
	return ""
}

// contentDisposition builds an RFC 6266 attachment header carrying the key's
// basename as an RFC 5987 UTF-8 extended value.
func contentDisposition(key string) string {
	return contentDispositionType("attachment", key)
}

// contentDispositionType is contentDisposition with a chosen disposition
// type ("attachment" or "inline").
func contentDispositionType(dispType, key string) string {
	name := path.Base(strings.TrimSuffix(key, "/"))
	if name == "." || name == "/" || name == "" {
		name = "download"
	}
	const hex = "0123456789ABCDEF"
	var b strings.Builder
	for i := 0; i < len(name); i++ {
		c := name[i]
		if isAttrChar(c) {
			b.WriteByte(c)
			continue
		}
		b.WriteByte('%')
		b.WriteByte(hex[c>>4])
		b.WriteByte(hex[c&0x0f])
	}
	return dispType + "; filename*=UTF-8''" + b.String()
}

// isAttrChar matches RFC 5987 attr-char.
func isAttrChar(c byte) bool {
	switch {
	case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		return true
	}
	return strings.IndexByte("!#$&+-.^_`|~", c) >= 0
}

// apiVersionID reports the version id the way S3 does: rows written before
// versioning was enabled carry "" and are addressed as "null".
func apiVersionID(v string) string {
	if v == "" {
		return "null"
	}
	return v
}

// handleDownloadObject streams an object's bytes.
// GET /admin/buckets/{bucket}/object?key=<key>[&versionId=<v>]
func (h *Handler) handleDownloadObject(w http.ResponseWriter, r *http.Request, bucketName string) {
	bucket, err := h.DB.GetBucket(bucketName)
	if err != nil || bucket == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "bucket not found"})
		return
	}
	q := r.URL.Query()
	key := q.Get("key")
	if msg := validateKey(key); msg != "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": msg})
		return
	}
	versionID := q.Get("versionId")
	safeKey := httpx.SanitizeLog(key)

	var meta *db.ObjectMeta
	if versionID != "" {
		meta, err = h.DB.GetObjectMetaByVersion(bucket.ID, key, versionID)
	} else {
		meta, err = h.DB.GetObjectMeta(bucket.ID, key)
	}
	if err != nil {
		h.Logger.Error("download lookup error", "bucket", bucketName, "key", safeKey, "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	if meta == nil || meta.IsDeleteMarker {
		if versionID != "" {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "version not found"})
		} else {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "object not found"})
		}
		return
	}

	reader, err := h.Objects.Open(bucket, meta)
	if err != nil {
		if storage.IsNotExist(err) {
			h.Logger.Warn("object data missing on disk", "bucket", bucketName, "key", safeKey, "version", httpx.SanitizeLog(meta.VersionID))
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "object not found"})
			return
		}
		h.Logger.Error("open object error", "bucket", bucketName, "key", safeKey, "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	defer reader.Close()

	ct := meta.ContentType
	if ct == "" {
		ct = "application/octet-stream"
	}
	disposition := "attachment"
	if q.Get("inline") == "1" {
		disposition = "inline"
	}
	hdr := w.Header()
	hdr.Set("Content-Type", ct)
	hdr.Set("Accept-Ranges", "bytes")
	hdr.Set("ETag", meta.ETag)
	hdr.Set("Last-Modified", meta.LastModified.UTC().Format(http.TimeFormat))
	hdr.Set("Content-Disposition", contentDispositionType(disposition, key))
	// Stored content is never rendered on the admin origin.
	hdr.Set("Content-Security-Policy", "sandbox")
	if bucket.Versioning != "" || meta.VersionID != "" {
		hdr.Set("X-Version-Id", apiVersionID(meta.VersionID))
	}

	// Single byte range (bytes=a-b, a-, -n). Multi-range or unparsable
	// headers are ignored and the whole object is served, as S3 does.
	status := http.StatusOK
	start, length := int64(0), meta.Size
	if rh := r.Header.Get("Range"); rh != "" {
		s, e, ok := httpx.ParseRange(rh, meta.Size)
		switch {
		case ok:
			start, length = s, e-s+1
			status = http.StatusPartialContent
			hdr.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", s, e, meta.Size))
		case s == -1:
			resetToJSON(hdr)
			hdr.Set("Content-Range", fmt.Sprintf("bytes */%d", meta.Size))
			writeJSON(w, http.StatusRequestedRangeNotSatisfiable, map[string]string{"error": "requested range not satisfiable"})
			return
		}
	}
	hdr.Set("Content-Length", strconv.FormatInt(length, 10))

	if r.Method == http.MethodHead {
		w.WriteHeader(status)
		return
	}

	if start > 0 {
		var serr error
		if seeker, ok := reader.(io.Seeker); ok {
			_, serr = seeker.Seek(start, io.SeekStart)
		} else {
			_, serr = io.CopyN(io.Discard, reader, start)
		}
		if serr != nil {
			h.Logger.Error("seek object error", "bucket", bucketName, "key", safeKey, "error", serr)
			resetToJSON(hdr)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
			return
		}
	}
	var src io.Reader = reader
	if status == http.StatusPartialContent {
		src = io.LimitReader(reader, length)
	}
	w.WriteHeader(status)

	// io.Copy writes in small chunks through the progress-timeout writer, so
	// an active download keeps extending the idle deadline.
	n, err := io.Copy(w, src)
	if err != nil {
		h.Logger.Warn("object download interrupted", "bucket", bucketName, "key", safeKey, "bytes", n, "error", err)
		return
	}
	if status == http.StatusOK {
		h.Logger.Info("object downloaded via admin API", "bucket", bucketName, "key", safeKey, "version", httpx.SanitizeLog(meta.VersionID), "size", n)
	} else {
		h.Logger.Debug("object range served via admin API", "bucket", bucketName, "key", safeKey, "start", start, "size", n)
	}
}

// resetToJSON drops the download headers before an error body is written.
func resetToJSON(hdr http.Header) {
	for _, k := range []string{"Content-Disposition", "Content-Range", "Content-Length", "ETag", "Last-Modified", "X-Version-Id", "Content-Security-Policy"} {
		hdr.Del(k)
	}
	hdr.Set("Content-Type", "application/json")
}

// readTracker remembers the first transport error from the request body so
// a client disconnect is reported as a client error, not an internal one.
type readTracker struct {
	r   io.Reader
	err error
}

func (t *readTracker) Read(p []byte) (int, error) {
	n, err := t.r.Read(p)
	if err != nil && err != io.EOF && t.err == nil {
		t.err = err
	}
	return n, err
}

// handleUploadObject stores the raw request body as an object through the
// shared object service (same quota, versioning, image and webhook handling
// as S3 PutObject).
// PUT /admin/buckets/{bucket}/object?key=<key>
func (h *Handler) handleUploadObject(w http.ResponseWriter, r *http.Request, bucketName string) {
	bucket, err := h.DB.GetBucket(bucketName)
	if err != nil || bucket == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "bucket not found"})
		return
	}
	key := r.URL.Query().Get("key")
	if msg := validateUploadKey(key); msg != "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": msg})
		return
	}
	folder := strings.HasSuffix(key, "/")
	if folder && r.ContentLength > 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": errFolderMarkerBody.Error()})
		return
	}

	contentType := r.Header.Get("Content-Type")
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	declared := r.ContentLength // -1 when unknown (chunked)

	var body io.Reader = http.NoBody
	if r.Body != nil {
		body = r.Body
	}
	tracker := &readTracker{r: body}
	safeKey := httpx.SanitizeLog(key)

	res, err := h.Objects.Put(service.PutInput{
		Bucket:       bucket,
		Key:          key,
		Body:         tracker,
		ContentType:  contentType,
		DeclaredSize: declared,
		MaxSize:      service.MaxPutObjectSize,
		Verify: func(size int64, _ []byte) error {
			if folder && size != 0 {
				return errFolderMarkerBody
			}
			if declared >= 0 && size != declared {
				return errIncompleteUpload
			}
			return nil
		},
		EventType: "s3:ObjectCreated:Put",
	})
	if err != nil {
		switch {
		case errors.Is(err, service.ErrQuotaExceeded):
			writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "quota exceeded"})
		case errors.Is(err, service.ErrTooLarge):
			writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{
				"error": fmt.Sprintf("object too large (max %d bytes)", service.MaxPutObjectSize),
			})
		case errors.Is(err, errFolderMarkerBody):
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": errFolderMarkerBody.Error()})
		case errors.Is(err, errIncompleteUpload), tracker.err != nil:
			h.Logger.Warn("object upload aborted", "bucket", bucketName, "key", safeKey, "error", err)
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "incomplete request body"})
		default:
			h.Logger.Error("object upload error", "bucket", bucketName, "key", safeKey, "error", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		}
		return
	}

	h.Logger.Info("object uploaded via admin API", "bucket", bucketName, "key", safeKey, "size", res.Size, "version", httpx.SanitizeLog(res.VersionID))
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"key":        key,
		"size":       res.Size,
		"etag":       res.ETag,
		"version_id": res.VersionID,
	})
}

// handleListKeyVersions lists every version and delete marker of one key,
// newest first.
// GET /admin/buckets/{bucket}/versions?key=<key>
func (h *Handler) handleListKeyVersions(w http.ResponseWriter, r *http.Request, bucketName string) {
	bucket, err := h.DB.GetBucket(bucketName)
	if err != nil || bucket == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "bucket not found"})
		return
	}
	key := r.URL.Query().Get("key")
	if msg := validateKey(key); msg != "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": msg})
		return
	}
	rows, err := h.DB.ListVersionsForKey(bucket.ID, key)
	if err != nil {
		h.Logger.Error("list versions error", "bucket", bucketName, "key", httpx.SanitizeLog(key), "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	out := make([]map[string]interface{}, 0, len(rows))
	for _, m := range rows {
		out = append(out, map[string]interface{}{
			"version_id":       apiVersionID(m.VersionID),
			"size":             m.Size,
			"etag":             m.ETag,
			"content_type":     m.ContentType,
			"last_modified":    m.LastModified.UTC().Format("2006-01-02T15:04:05Z"),
			"is_latest":        m.IsLatest,
			"is_delete_marker": m.IsDeleteMarker,
		})
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"versions": out})
}

// statusTLS reports which listeners serve TLS according to the effective config.
func (h *Handler) statusTLS() map[string]interface{} {
	c := h.Config
	acmeOn := c.Server.TLS.Enabled && c.Server.TLS.ACME.Enabled
	domains := []string{}
	if acmeOn {
		domains = append(domains, c.Server.TLS.ACME.Domains...)
	}
	return map[string]interface{}{
		"s3":           c.Server.TLS.Enabled,
		"admin":        c.Admin.TLS.Enabled,
		"webdav":       c.WebDAV.Enabled && c.WebDAV.TLS.Enabled,
		"acme":         acmeOn,
		"acme_domains": domains,
	}
}

// statusStorage reports the default storage root and its filesystem usage.
// total_bytes / free_bytes are omitted when the platform cannot report them.
func (h *Handler) statusStorage() map[string]interface{} {
	root := h.Config.Storage.RootDir
	if h.Storage != nil && h.Storage.RootDir != "" {
		root = h.Storage.RootDir
	}
	out := map[string]interface{}{"path": root}
	if total, free, ok := diskUsage(root); ok {
		out["total_bytes"] = total
		out["free_bytes"] = free
	}
	return out
}

func (h *Handler) uptimeSeconds() int64 {
	if h.startedAt.IsZero() {
		return 0
	}
	return int64(time.Since(h.startedAt) / time.Second)
}
