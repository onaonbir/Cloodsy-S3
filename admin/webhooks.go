package admin

import (
	"net/http"
	"strconv"

	"github.com/onaonbir/Cloodsy-S3/httpx"
	"github.com/onaonbir/Cloodsy-S3/webhook"
)

func (h *Handler) handleListWebhooks(w http.ResponseWriter, r *http.Request, bucketName string) {
	bucket, err := h.DB.GetBucket(bucketName)
	if err != nil || bucket == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "bucket not found"})
		return
	}

	hooks, err := h.DB.ListWebhooks(bucketName)
	if err != nil {
		h.Logger.Error("list webhooks error", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	result := make([]map[string]interface{}, 0, len(hooks))
	for _, hook := range hooks {
		result = append(result, map[string]interface{}{
			"id":          hook.ID,
			"name":        hook.Name,
			"url":         hook.URL,
			"event_types": hook.EventTypes,
			"active":      hook.Active,
			"created_at":  hook.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"),
		})
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{"webhooks": result})
}

func (h *Handler) handleCreateWebhook(w http.ResponseWriter, r *http.Request, bucketName string) {
	bucket, err := h.DB.GetBucket(bucketName)
	if err != nil || bucket == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "bucket not found"})
		return
	}

	var req struct {
		Name       string `json:"name"`
		URL        string `json:"url"`
		EventTypes string `json:"event_types"`
		Secret     string `json:"secret"`
	}
	if !decodeJSON(w, r, &req, false) {
		return
	}

	if req.URL == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "url is required"})
		return
	}
	if len(req.URL) > 2048 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "url too long"})
		return
	}
	// Admins may point hooks at internal services (allowPrivate=true), but the
	// scheme must still be http/https so the dispatcher never dials file:,
	// gopher: or similar.
	if err := webhook.ValidateURL(req.URL, true); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	hook, err := h.DB.CreateWebhook(bucketName, req.Name, req.URL, req.EventTypes, req.Secret)
	if err != nil {
		h.Logger.Error("create webhook error", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	h.Logger.Info("webhook created via admin API", "bucket", bucketName, "name", httpx.SanitizeLog(req.Name), "url", httpx.SanitizeLog(req.URL))
	writeJSON(w, http.StatusCreated, map[string]interface{}{
		"id":          hook.ID,
		"name":        hook.Name,
		"bucket":      bucketName,
		"url":         hook.URL,
		"event_types": hook.EventTypes,
		"active":      hook.Active,
	})
}

func (h *Handler) handleDeleteWebhook(w http.ResponseWriter, r *http.Request, idStr string) {
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid webhook id"})
		return
	}

	if err := h.DB.DeleteWebhook(id); err != nil {
		h.Logger.Error("delete webhook error", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	h.Logger.Info("webhook deleted via admin API", "id", id)
	writeJSON(w, http.StatusOK, map[string]string{"message": "webhook deleted"})
}
