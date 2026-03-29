package admin

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
)

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

	maxKeys := 200
	if maxKeysStr != "" {
		if mk, err := strconv.Atoi(maxKeysStr); err == nil && mk > 0 && mk <= 1000 {
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

func (h *Handler) handleDeleteObject(w http.ResponseWriter, r *http.Request, bucketName, key string) {
	bucket, err := h.DB.GetBucket(bucketName)
	if err != nil || bucket == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "bucket not found"})
		return
	}

	if err := h.DB.DeleteObjectMeta(bucket.ID, key); err != nil {
		h.Logger.Error("delete object meta error", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	if err := h.Storage.DeleteObject(bucketName, key); err != nil {
		h.Logger.Error("delete object file error", "bucket", bucketName, "key", key, "error", err)
	}

	// Clean up empty parent directories
	h.Storage.CleanEmptyParents(bucketName, key)

	h.Logger.Info("object deleted via admin API", "bucket", bucketName, "key", key)
	writeJSON(w, http.StatusOK, map[string]string{"message": "object deleted"})
}

// handleDeletePrefix deletes all objects under a given prefix (folder delete).
func (h *Handler) handleDeletePrefix(w http.ResponseWriter, r *http.Request, bucketName string) {
	bucket, err := h.DB.GetBucket(bucketName)
	if err != nil || bucket == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "bucket not found"})
		return
	}

	var req struct {
		Prefix string `json:"prefix"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Prefix == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "prefix is required"})
		return
	}

	// List all objects under this prefix (no delimiter = flat list)
	objects, _, _, _, err := h.DB.ListObjectsMeta(bucket.ID, req.Prefix, "", "", 10000)
	if err != nil {
		h.Logger.Error("list objects for prefix delete error", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	deleted := 0
	for _, obj := range objects {
		if err := h.DB.DeleteObjectMeta(bucket.ID, obj.Key); err != nil {
			h.Logger.Error("delete object meta error", "key", obj.Key, "error", err)
			continue
		}
		if err := h.Storage.DeleteObject(bucketName, obj.Key); err != nil {
			h.Logger.Error("delete object file error", "key", obj.Key, "error", err)
		}
		deleted++
	}

	// Remove the directory tree from disk
	if err := h.Storage.DeletePrefix(bucketName, req.Prefix); err != nil {
		h.Logger.Error("delete prefix dir error", "prefix", req.Prefix, "error", err)
	}

	h.Logger.Info("prefix deleted via admin API", "bucket", bucketName, "prefix", req.Prefix, "deleted", deleted)
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"message": "prefix deleted",
		"deleted": deleted,
	})
}

func extractObjectKey(parts []string) string {
	for i, p := range parts {
		if p == "objects" && i+1 < len(parts) {
			return strings.Join(parts[i+1:], "/")
		}
	}
	return ""
}
