package admin

import (
	"encoding/json"
	"net/http"
)

func (h *Handler) handleGetLifecycle(w http.ResponseWriter, r *http.Request, bucketName string) {
	bucket, err := h.DB.GetBucket(bucketName)
	if err != nil || bucket == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "bucket not found"})
		return
	}

	rules, err := h.DB.GetLifecycleRules(bucketName)
	if err != nil {
		h.Logger.Error("get lifecycle rules error", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	result := make([]map[string]interface{}, 0, len(rules))
	for _, r := range rules {
		result = append(result, map[string]interface{}{
			"id":              r.ID,
			"name":            r.Name,
			"prefix":          r.Prefix,
			"expiration_days": r.ExpirationDays,
			"created_at":      r.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"),
		})
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{"rules": result})
}

func (h *Handler) handleSetLifecycle(w http.ResponseWriter, r *http.Request, bucketName string) {
	bucket, err := h.DB.GetBucket(bucketName)
	if err != nil || bucket == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "bucket not found"})
		return
	}

	var req struct {
		Name           string `json:"name"`
		Prefix         string `json:"prefix"`
		ExpirationDays int    `json:"expiration_days"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}

	if req.ExpirationDays <= 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "expiration_days must be positive"})
		return
	}

	if err := h.DB.PutLifecycleRule(bucketName, req.Name, req.Prefix, req.ExpirationDays); err != nil {
		h.Logger.Error("set lifecycle rule error", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"bucket":          bucketName,
		"name":            req.Name,
		"prefix":          req.Prefix,
		"expiration_days": req.ExpirationDays,
	})
}

func (h *Handler) handleDeleteLifecycle(w http.ResponseWriter, r *http.Request, bucketName string) {
	bucket, err := h.DB.GetBucket(bucketName)
	if err != nil || bucket == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "bucket not found"})
		return
	}

	prefix := r.URL.Query().Get("prefix")
	if prefix != "" {
		if err := h.DB.DeleteLifecycleRuleByPrefix(bucketName, prefix); err != nil {
			h.Logger.Error("delete lifecycle rule error", "error", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
			return
		}
	} else {
		if err := h.DB.DeleteLifecycleRules(bucketName); err != nil {
			h.Logger.Error("delete lifecycle rules error", "error", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
			return
		}
	}

	writeJSON(w, http.StatusOK, map[string]string{"message": "lifecycle rules deleted"})
}
