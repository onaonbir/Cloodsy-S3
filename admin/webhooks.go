package admin

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/onaonbir/Cloodsy-S3/db"
	"github.com/onaonbir/Cloodsy-S3/httpx"
	"github.com/onaonbir/Cloodsy-S3/webhook"
)

func webhookJSON(hook db.BucketWebhook) map[string]interface{} {
	return map[string]interface{}{
		"id":          hook.ID,
		"name":        hook.Name,
		"url":         hook.URL,
		"event_types": hook.EventTypes,
		"active":      hook.Active,
		"has_secret":  hook.Secret != "",
		"created_at":  fmtTime(hook.CreatedAt),
	}
}

// validateWebhookFields applies the create-time checks. It returns a
// client-facing message or "".
func validateWebhookFields(name, url, eventTypes string) string {
	if url == "" {
		return "url is required"
	}
	if len(url) > 2048 {
		return "url too long"
	}
	// Admins may point hooks at internal services (allowPrivate=true), but the
	// scheme must still be http/https so the dispatcher never dials file:,
	// gopher: or similar.
	if err := webhook.ValidateURL(url, true); err != nil {
		return err.Error()
	}
	if len(name) > 255 || strings.ContainsAny(name, "\r\n\x00") {
		return "invalid name"
	}
	if len(eventTypes) > 1024 || strings.ContainsAny(eventTypes, "\r\n\x00") {
		return "invalid event_types"
	}
	return ""
}

func (h *Handler) handleListWebhooks(w http.ResponseWriter, r *http.Request, bucketName string) {
	if h.loadBucket(w, bucketName) == nil {
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
		result = append(result, webhookJSON(hook))
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"webhooks": result})
}

func (h *Handler) handleCreateWebhook(w http.ResponseWriter, r *http.Request, bucketName string) {
	if h.loadBucket(w, bucketName) == nil {
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
	if msg := validateWebhookFields(req.Name, req.URL, req.EventTypes); msg != "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": msg})
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
		"has_secret":  hook.Secret != "",
	})
}

// loadWebhook parses the id and writes 400/404 on failure.
func (h *Handler) loadWebhook(w http.ResponseWriter, idStr string) *db.BucketWebhook {
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil || id <= 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid webhook id"})
		return nil
	}
	hook, err := h.DB.GetWebhook(id)
	if err != nil {
		h.Logger.Error("get webhook error", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return nil
	}
	if hook == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "webhook not found"})
		return nil
	}
	return hook
}

// handleUpdateWebhook replaces a hook's settings. An omitted secret keeps
// the stored one; "" clears it.
// PUT /admin/webhooks/{id}
func (h *Handler) handleUpdateWebhook(w http.ResponseWriter, r *http.Request, idStr string) {
	hook := h.loadWebhook(w, idStr)
	if hook == nil {
		return
	}
	var req struct {
		Name       string  `json:"name"`
		URL        string  `json:"url"`
		EventTypes string  `json:"event_types"`
		Active     *bool   `json:"active"`
		Secret     *string `json:"secret"`
	}
	if !decodeJSON(w, r, &req, false) {
		return
	}
	if msg := validateWebhookFields(req.Name, req.URL, req.EventTypes); msg != "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": msg})
		return
	}
	active := hook.Active
	if req.Active != nil {
		active = *req.Active
	}
	if err := h.DB.UpdateWebhook(hook.ID, req.Name, req.URL, req.EventTypes, active, req.Secret); err != nil {
		h.Logger.Error("update webhook error", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	updated, err := h.DB.GetWebhook(hook.ID)
	if err != nil || updated == nil {
		h.Logger.Error("reload webhook error", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	h.Logger.Info("webhook updated via admin API", "id", hook.ID, "bucket", hook.BucketName, "url", httpx.SanitizeLog(req.URL), "secret_changed", req.Secret != nil)
	writeJSON(w, http.StatusOK, webhookJSON(*updated))
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

// handleWebhookDeliveries lists the recorded delivery attempts, newest first.
// GET /admin/webhooks/{id}/deliveries?limit=50
func (h *Handler) handleWebhookDeliveries(w http.ResponseWriter, r *http.Request, idStr string) {
	hook := h.loadWebhook(w, idStr)
	if hook == nil {
		return
	}
	limit := 50
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid limit"})
			return
		}
		if n > db.WebhookDeliveryKeep {
			n = db.WebhookDeliveryKeep
		}
		limit = n
	}
	rows, err := h.DB.ListWebhookDeliveries(hook.ID, limit)
	if err != nil {
		h.Logger.Error("list webhook deliveries error", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	out := make([]map[string]interface{}, 0, len(rows))
	for _, d := range rows {
		out = append(out, map[string]interface{}{
			"id":          d.ID,
			"webhook_id":  d.WebhookID,
			"event_type":  d.EventType,
			"object_key":  d.ObjectKey,
			"status_code": d.StatusCode,
			"error":       d.Error,
			"duration_ms": d.DurationMs,
			"attempt":     d.Attempt,
			"created_at":  fmtTime(d.CreatedAt),
		})
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"deliveries": out})
}

// handleTestWebhook sends a synthetic s3:TestEvent synchronously.
// POST /admin/webhooks/{id}/test
func (h *Handler) handleTestWebhook(w http.ResponseWriter, r *http.Request, idStr string) {
	hook := h.loadWebhook(w, idStr)
	if hook == nil {
		return
	}
	d := h.Objects.Dispatcher
	if d == nil {
		d = webhook.NewSender(h.DB, h.Config.Server.Region, h.Logger)
	}
	res := d.SendTest(*hook)
	h.Logger.Info("webhook test sent via admin API", "id", hook.ID, "status", res.StatusCode, "error", httpx.SanitizeLog(res.Error))
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"status_code": res.StatusCode,
		"error":       res.Error,
		"duration_ms": res.Duration.Milliseconds(),
	})
}
