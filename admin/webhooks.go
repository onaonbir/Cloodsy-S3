package admin

import (
	"encoding/json"
	"net/http"
	"strconv"
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
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}

	if req.URL == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "url is required"})
		return
	}

	hook, err := h.DB.CreateWebhook(bucketName, req.Name, req.URL, req.EventTypes, req.Secret)
	if err != nil {
		h.Logger.Error("create webhook error", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	h.Logger.Info("webhook created via admin API", "bucket", bucketName, "name", req.Name, "url", req.URL)
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
