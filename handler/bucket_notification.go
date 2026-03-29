package handler

import (
	"net/http"

	"github.com/onaonbir/Cloodsy-S3/s3err"
	"github.com/onaonbir/Cloodsy-S3/s3xml"
)

// GetBucketNotification handles GET /<bucket>?notification
func (h *Handler) GetBucketNotification(w http.ResponseWriter, r *http.Request) {
	cred, ok := h.authenticateRequest(w, r)
	if !ok {
		return
	}

	bucketName, _ := getBucketAndKey(r)
	_, ok = h.checkBucketAccess(w, r, cred, bucketName)
	if !ok {
		return
	}

	hooks, err := h.DB.ListWebhooks(bucketName)
	if err != nil {
		h.Logger.Error("failed to list webhooks", "error", err)
		s3err.WriteError(w, r, s3err.ErrInternalError)
		return
	}

	result := s3xml.NotificationConfiguration{
		Xmlns: "http://s3.amazonaws.com/doc/2006-03-01/",
	}
	for _, hook := range hooks {
		result.Webhooks = append(result.Webhooks, s3xml.WebhookConfiguration{
			ID:         hook.ID,
			URL:        hook.URL,
			EventTypes: hook.EventTypes,
			Secret:     hook.Secret,
			Active:     hook.Active,
		})
	}

	h.writeXML(w, http.StatusOK, result)
}

// PutBucketNotification handles PUT /<bucket>?notification
func (h *Handler) PutBucketNotification(w http.ResponseWriter, r *http.Request) {
	cred, ok := h.authenticateRequest(w, r)
	if !ok {
		return
	}

	if !h.checkWriteAccess(w, r, cred) {
		return
	}

	bucketName, _ := getBucketAndKey(r)
	_, ok = h.checkBucketAccess(w, r, cred, bucketName)
	if !ok {
		return
	}

	var req s3xml.NotificationConfiguration
	if err := limitedXMLDecode(r.Body, &req); err != nil {
		s3err.WriteError(w, r, s3err.ErrMalformedXML)
		return
	}

	// Replace all existing webhooks
	if err := h.DB.DeleteAllWebhooks(bucketName); err != nil {
		h.Logger.Error("failed to delete existing webhooks", "error", err)
		s3err.WriteError(w, r, s3err.ErrInternalError)
		return
	}

	for _, wh := range req.Webhooks {
		if wh.URL == "" {
			s3err.WriteError(w, r, s3err.ErrInvalidArgument)
			return
		}
		if _, err := h.DB.CreateWebhook(bucketName, "", wh.URL, wh.EventTypes, wh.Secret); err != nil {
			h.Logger.Error("failed to create webhook", "error", err)
			s3err.WriteError(w, r, s3err.ErrInternalError)
			return
		}
	}

	w.WriteHeader(http.StatusOK)
}

// DeleteBucketNotification handles DELETE /<bucket>?notification
func (h *Handler) DeleteBucketNotification(w http.ResponseWriter, r *http.Request) {
	cred, ok := h.authenticateRequest(w, r)
	if !ok {
		return
	}

	if !h.checkWriteAccess(w, r, cred) {
		return
	}

	bucketName, _ := getBucketAndKey(r)
	_, ok = h.checkBucketAccess(w, r, cred, bucketName)
	if !ok {
		return
	}

	if err := h.DB.DeleteAllWebhooks(bucketName); err != nil {
		h.Logger.Error("failed to delete webhooks", "error", err)
		s3err.WriteError(w, r, s3err.ErrInternalError)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}
