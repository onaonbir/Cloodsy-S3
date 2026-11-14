package admin

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/onaonbir/Cloodsy-S3/db"
	"github.com/onaonbir/Cloodsy-S3/httpx"
)

// lifecycleRuleJSON renders one rule for the admin API.
func lifecycleRuleJSON(r db.LifecycleRule) map[string]interface{} {
	status := r.Status
	if status == "" {
		status = "Enabled"
	}
	return map[string]interface{}{
		"id":                    r.ID,
		"name":                  r.Name,
		"prefix":                r.Prefix,
		"status":                status,
		"expiration_days":       r.ExpirationDays,
		"noncurrent_days":       r.NoncurrentDays,
		"abort_multipart_days":  r.AbortMultipartDays,
		"expire_delete_markers": r.ExpireDeleteMarkers,
		"created_at":            fmtTime(r.CreatedAt),
	}
}

// lifecycleRequest is the POST/PUT body. The v1 body (name, prefix,
// expiration_days) is a subset.
type lifecycleRequest struct {
	Name                string `json:"name"`
	Prefix              string `json:"prefix"`
	Status              string `json:"status"`
	ExpirationDays      int    `json:"expiration_days"`
	NoncurrentDays      int    `json:"noncurrent_days"`
	AbortMultipartDays  int    `json:"abort_multipart_days"`
	ExpireDeleteMarkers bool   `json:"expire_delete_markers"`
}

// toRule validates the request with the same rules the S3
// PutBucketLifecycle path applies, so admin-created rules behave identically
// in the lifecycle runner. It returns a client-facing message on error.
func (req lifecycleRequest) toRule(bucket string) (db.LifecycleRule, string) {
	r := db.LifecycleRule{
		BucketName:          bucket,
		Name:                req.Name,
		Prefix:              req.Prefix,
		Status:              req.Status,
		ExpirationDays:      req.ExpirationDays,
		NoncurrentDays:      req.NoncurrentDays,
		AbortMultipartDays:  req.AbortMultipartDays,
		ExpireDeleteMarkers: req.ExpireDeleteMarkers,
	}
	if r.Status == "" {
		r.Status = "Enabled"
	}
	switch {
	case r.Status != "Enabled" && r.Status != "Disabled":
		return r, "status must be Enabled or Disabled"
	case len(r.Name) > 255 || strings.ContainsAny(r.Name, "\r\n\x00"):
		return r, "invalid name"
	case strings.ContainsRune(r.Prefix, 0) || len(r.Prefix) > prefixMaxLen:
		return r, "invalid prefix"
	case r.ExpirationDays < 0:
		return r, "expiration_days must be positive"
	case r.NoncurrentDays < 0:
		return r, "noncurrent_days must be positive"
	case r.AbortMultipartDays < 0:
		return r, "abort_multipart_days must be positive"
	case r.ExpireDeleteMarkers && r.ExpirationDays > 0:
		// S3: ExpiredObjectDeleteMarker cannot be combined with Days.
		return r, "expire_delete_markers cannot be combined with expiration_days"
	case r.ExpirationDays == 0 && r.NoncurrentDays == 0 && r.AbortMultipartDays == 0 && !r.ExpireDeleteMarkers:
		return r, "rule needs at least one action (expiration_days, noncurrent_days, abort_multipart_days or expire_delete_markers)"
	}
	return r, ""
}

func (h *Handler) handleGetLifecycle(w http.ResponseWriter, r *http.Request, bucketName string) {
	if h.loadBucket(w, bucketName) == nil {
		return
	}
	rules, err := h.DB.GetLifecycleRules(bucketName)
	if err != nil {
		h.Logger.Error("get lifecycle rules error", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	result := make([]map[string]interface{}, 0, len(rules))
	for _, rule := range rules {
		result = append(result, lifecycleRuleJSON(rule))
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"rules": result})
}

// handleSetLifecycle creates a rule, or replaces the bucket's rule for the
// same prefix (one rule per prefix, as on the S3 API).
// POST /admin/buckets/{bucket}/lifecycle
func (h *Handler) handleSetLifecycle(w http.ResponseWriter, r *http.Request, bucketName string) {
	if h.loadBucket(w, bucketName) == nil {
		return
	}
	var req lifecycleRequest
	if !decodeJSON(w, r, &req, false) {
		return
	}
	rule, msg := req.toRule(bucketName)
	if msg != "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": msg})
		return
	}
	if err := h.DB.PutLifecycleRuleFull(rule); err != nil {
		h.Logger.Error("set lifecycle rule error", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	h.Logger.Info("lifecycle rule saved via admin API", "bucket", bucketName, "prefix", httpx.SanitizeLog(rule.Prefix))

	out := map[string]interface{}{
		"bucket":          bucketName,
		"name":            rule.Name,
		"prefix":          rule.Prefix,
		"expiration_days": rule.ExpirationDays,
	}
	// Return the stored rule (with id and created_at) when it can be found.
	if rules, err := h.DB.GetLifecycleRules(bucketName); err == nil {
		for _, saved := range rules {
			if saved.Prefix == rule.Prefix {
				for k, v := range lifecycleRuleJSON(saved) {
					out[k] = v
				}
				break
			}
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func parseRuleID(w http.ResponseWriter, s string) (int64, bool) {
	id, err := strconv.ParseInt(s, 10, 64)
	if err != nil || id <= 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid rule id"})
		return 0, false
	}
	return id, true
}

// handleUpdateLifecycleRule fully replaces one rule.
// PUT /admin/buckets/{bucket}/lifecycle/{id}
func (h *Handler) handleUpdateLifecycleRule(w http.ResponseWriter, r *http.Request, bucketName, idStr string) {
	if h.loadBucket(w, bucketName) == nil {
		return
	}
	id, ok := parseRuleID(w, idStr)
	if !ok {
		return
	}
	existing, err := h.DB.GetLifecycleRuleByID(id)
	if err != nil {
		h.Logger.Error("get lifecycle rule error", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	if existing == nil || existing.BucketName != bucketName {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "rule not found"})
		return
	}
	var req lifecycleRequest
	if !decodeJSON(w, r, &req, false) {
		return
	}
	rule, msg := req.toRule(bucketName)
	if msg != "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": msg})
		return
	}
	rule.ID = id
	if err := h.DB.UpdateLifecycleRule(rule); err != nil {
		if errors.Is(err, db.ErrConflict) {
			writeJSON(w, http.StatusConflict, map[string]string{"error": "another rule already uses this prefix"})
			return
		}
		h.Logger.Error("update lifecycle rule error", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	saved, err := h.DB.GetLifecycleRuleByID(id)
	if err != nil || saved == nil {
		h.Logger.Error("reload lifecycle rule error", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	h.Logger.Info("lifecycle rule updated via admin API", "bucket", bucketName, "id", id)
	writeJSON(w, http.StatusOK, lifecycleRuleJSON(*saved))
}

// handleDeleteLifecycleRule removes one rule by id.
// DELETE /admin/buckets/{bucket}/lifecycle/{id}
func (h *Handler) handleDeleteLifecycleRule(w http.ResponseWriter, r *http.Request, bucketName, idStr string) {
	if h.loadBucket(w, bucketName) == nil {
		return
	}
	id, ok := parseRuleID(w, idStr)
	if !ok {
		return
	}
	found, err := h.DB.DeleteLifecycleRuleByID(bucketName, id)
	if err != nil {
		h.Logger.Error("delete lifecycle rule error", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	if !found {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "rule not found"})
		return
	}
	h.Logger.Info("lifecycle rule deleted via admin API", "bucket", bucketName, "id", id)
	writeJSON(w, http.StatusOK, map[string]string{"message": "lifecycle rule deleted"})
}

// handleDeleteLifecycle deletes the rule for ?prefix=, or every rule of the
// bucket when no prefix is given (v1 behaviour).
// DELETE /admin/buckets/{bucket}/lifecycle[?prefix=]
func (h *Handler) handleDeleteLifecycle(w http.ResponseWriter, r *http.Request, bucketName string) {
	if h.loadBucket(w, bucketName) == nil {
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
