package admin

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/onaonbir/Cloodsy-S3/db"
	"github.com/onaonbir/Cloodsy-S3/httpx"
	"github.com/onaonbir/Cloodsy-S3/service"
)

const (
	maxKeyLen    = 1024
	deleteBatch  = 1000
	listMaxKeys  = 1000
	listDefKeys  = 200
	prefixMaxLen = maxKeyLen
)

// validatePrefix rejects prefixes that could escape the bucket or that never
// match a real key. It returns a client-facing message or "".
func validatePrefix(p string) string {
	switch {
	case p == "":
		return "prefix is required"
	case len(p) > prefixMaxLen:
		return "prefix too long"
	case strings.HasPrefix(p, "/"):
		return "prefix must not start with '/'"
	case strings.ContainsRune(p, 0):
		return "prefix contains NUL"
	case strings.Contains(p, "\\"):
		return "prefix must not contain backslashes"
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." || seg == "." {
			return "prefix must not contain '.' or '..' segments"
		}
	}
	return ""
}

// validateKey mirrors the S3 key rules the API enforces.
func validateKey(k string) string {
	if k == "" {
		return "object key required"
	}
	if len(k) > maxKeyLen {
		return "object key too long"
	}
	if strings.ContainsRune(k, 0) {
		return "object key contains NUL"
	}
	for _, seg := range strings.Split(k, "/") {
		if seg == ".." {
			return "object key must not contain '..' segments"
		}
	}
	return ""
}

func (h *Handler) handleListObjects(w http.ResponseWriter, r *http.Request, bucketName string) {
	bucket, err := h.DB.GetBucket(bucketName)
	if err != nil || bucket == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "bucket not found"})
		return
	}

	prefix := r.URL.Query().Get("prefix")
	delimiter := r.URL.Query().Get("delimiter")
	marker := r.URL.Query().Get("marker")
	maxKeysStr := r.URL.Query().Get("max-keys")

	if delimiter == "" {
		delimiter = "/"
	}
	if len(prefix) > prefixMaxLen || strings.ContainsRune(prefix, 0) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid prefix"})
		return
	}

	maxKeys := listDefKeys
	if maxKeysStr != "" {
		if mk, err := strconv.Atoi(maxKeysStr); err == nil && mk > 0 && mk <= listMaxKeys {
			maxKeys = mk
		}
	}

	objects, prefixes, truncated, nextMarker, err := h.DB.ListObjectsMeta(
		bucket.ID, prefix, marker, delimiter, maxKeys,
	)
	if err != nil {
		h.Logger.Error("list objects error", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	if prefixes == nil {
		prefixes = []string{}
	}

	result := make([]map[string]interface{}, 0, len(objects))
	for _, obj := range objects {
		result = append(result, map[string]interface{}{
			"key":           obj.Key,
			"size":          obj.Size,
			"etag":          obj.ETag,
			"content_type":  obj.ContentType,
			"last_modified": obj.LastModified.UTC().Format("2006-01-02T15:04:05Z"),
		})
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"objects":     result,
		"prefixes":    prefixes,
		"prefix":      prefix,
		"truncated":   truncated,
		"next_marker": nextMarker,
	})
}

// handleDeleteObject permanently removes an object. Without ?versionId every
// version (and marker) of the key is purged — admin semantics, not the S3
// "add a delete marker" behavior. With ?versionId only that version goes.
func (h *Handler) handleDeleteObject(w http.ResponseWriter, r *http.Request, bucketName, key string) {
	bucket, err := h.DB.GetBucket(bucketName)
	if err != nil || bucket == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "bucket not found"})
		return
	}
	if msg := validateKey(key); msg != "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": msg})
		return
	}

	versionID := r.URL.Query().Get("versionId")
	safeKey := httpx.SanitizeLog(key)

	if versionID != "" {
		res, err := h.Objects.Delete(bucket, key, versionID)
		if err != nil {
			if errors.Is(err, service.ErrNoSuchVersion) {
				writeJSON(w, http.StatusNotFound, map[string]string{"error": "version not found"})
				return
			}
			h.Logger.Error("delete object version error", "bucket", bucketName, "key", safeKey, "version", httpx.SanitizeLog(versionID), "error", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
			return
		}
		h.Logger.Info("object version deleted via admin API", "bucket", bucketName, "key", safeKey, "version", httpx.SanitizeLog(versionID), "delete_marker", res.DeleteMarker)
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"message":       "object version deleted",
			"version_id":    versionID,
			"delete_marker": res.DeleteMarker,
		})
		return
	}

	if err := h.Objects.DeleteAllVersions(bucket, key); err != nil {
		h.Logger.Error("delete object error", "bucket", bucketName, "key", safeKey, "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	// Object storage already prunes empty parents; this is a no-op safety net
	// for keys whose bytes were removed out-of-band.
	h.Storage.CleanEmptyParents(bucketName, key)

	h.Logger.Info("object deleted via admin API", "bucket", bucketName, "key", safeKey)
	writeJSON(w, http.StatusOK, map[string]string{"message": "object deleted"})
}

// handleDeletePrefix permanently deletes every object under a prefix (folder
// delete). Every key goes through the object service so versions, delete
// markers, variant caches and webhook events are handled exactly as for a
// single delete; the directory tree is removed afterwards for "/"-terminated
// prefixes.
func (h *Handler) handleDeletePrefix(w http.ResponseWriter, r *http.Request, bucketName string) {
	bucket, err := h.DB.GetBucket(bucketName)
	if err != nil || bucket == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "bucket not found"})
		return
	}

	var req struct {
		Prefix string `json:"prefix"`
	}
	if !decodeJSON(w, r, &req, false) {
		return
	}
	if msg := validatePrefix(req.Prefix); msg != "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": msg})
		return
	}
	safePrefix := httpx.SanitizeLog(req.Prefix)

	deleted, failed, err := h.deletePrefix(bucket, req.Prefix)
	if err != nil {
		h.Logger.Error("delete prefix error", "bucket", bucketName, "prefix", safePrefix, "deleted", deleted, "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]interface{}{
			"error":   "internal error",
			"deleted": deleted,
			"failed":  failed,
		})
		return
	}

	// Remove the (now empty) directory tree. Storage ignores prefixes that do
	// not end with "/" and refuses the bucket root.
	if strings.HasSuffix(req.Prefix, "/") {
		if err := h.Storage.DeletePrefix(bucketName, req.Prefix); err != nil {
			h.Logger.Error("delete prefix dir error", "bucket", bucketName, "prefix", safePrefix, "error", err)
		}
	}

	h.Logger.Info("prefix deleted via admin API", "bucket", bucketName, "prefix", safePrefix, "deleted", deleted, "failed", failed)
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"message": "prefix deleted",
		"deleted": deleted,
		"failed":  failed,
	})
}

// deletePrefix removes every key under prefix. It pages with the marker until
// the listing is exhausted; keys whose delete fails are skipped (the marker
// still advances, so a persistent failure cannot loop forever). A second pass
// over the version index catches keys that only have non-current versions or
// delete markers left, which the plain listing does not show.
func (h *Handler) deletePrefix(bucket *db.Bucket, prefix string) (deleted, failed int, err error) {
	marker := ""
	for {
		objects, _, truncated, next, lerr := h.DB.ListObjectsMeta(bucket.ID, prefix, marker, "", deleteBatch)
		if lerr != nil {
			return deleted, failed, lerr
		}
		for i := range objects {
			if derr := h.Objects.DeleteAllVersions(bucket, objects[i].Key); derr != nil {
				h.Logger.Error("delete object error", "bucket", bucket.Name, "key", httpx.SanitizeLog(objects[i].Key), "error", derr)
				failed++
				continue
			}
			deleted++
		}
		if !truncated || next == "" || next == marker {
			break
		}
		marker = next
	}

	// Sweep leftovers: markers and non-current versions.
	keyMarker := ""
	for {
		rows, truncated, lerr := h.DB.ListObjectVersions(bucket.ID, prefix, keyMarker, "", deleteBatch)
		if lerr != nil {
			return deleted, failed, lerr
		}
		if len(rows) == 0 {
			break
		}
		lastKey := ""
		for i := range rows {
			k := rows[i].Key
			if k == lastKey {
				continue
			}
			lastKey = k
			if derr := h.Objects.DeleteAllVersions(bucket, k); derr != nil {
				h.Logger.Error("delete object versions error", "bucket", bucket.Name, "key", httpx.SanitizeLog(k), "error", derr)
				failed++
				continue
			}
			deleted++
		}
		if !truncated || lastKey == "" || lastKey == keyMarker {
			break
		}
		keyMarker = lastKey
	}
	return deleted, failed, nil
}

func extractObjectKey(parts []string) string {
	for i, p := range parts {
		if p == "objects" && i+1 < len(parts) {
			return strings.Join(parts[i+1:], "/")
		}
	}
	return ""
}
