package handler

import (
	"net/http"

	"github.com/onaonbir/Cloodsy-S3/db"
	"github.com/onaonbir/Cloodsy-S3/s3err"
	"github.com/onaonbir/Cloodsy-S3/s3xml"
	"github.com/onaonbir/Cloodsy-S3/webhook"
)

// GetBucketNotification handles GET /<bucket>?notification. Secrets are never
// returned; an existing secret is reported as "***".
func (h *Handler) GetBucketNotification(w http.ResponseWriter, r *http.Request) {
	cred, ok := h.authenticateRequest(w, r)
	if !ok {
		return
	}
	bucketName, _ := getBucketAndKey(r)
	if _, ok = h.checkBucketAccess(w, r, cred, bucketName); !ok {
		return
	}

	hooks, err := h.DB.ListWebhooks(bucketName)
	if err != nil {
		h.Logger.Error("failed to list webhooks", "error", err)
		s3err.WriteError(w, r, s3err.ErrInternalError)
		return
	}

	result := s3xml.NotificationConfiguration{Xmlns: "http://s3.amazonaws.com/doc/2006-03-01/"}
	for _, hook := range hooks {
		secret := ""
		if hook.Secret != "" {
			secret = "***"
		}
		result.Webhooks = append(result.Webhooks, s3xml.WebhookConfiguration{
			ID:         hook.ID,
			Name:       hook.Name,
			URL:        hook.URL,
			EventTypes: hook.EventTypes,
			Secret:     secret,
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
	if _, ok = h.checkBucketAccess(w, r, cred, bucketName); !ok {
		return
	}

	var req s3xml.NotificationConfiguration
	if err := limitedXMLDecode(r.Body, &req); err != nil {
		s3err.WriteError(w, r, s3err.ErrMalformedXML)
		return
	}
	if len(req.Webhooks) > 100 {
		s3err.WriteError(w, r, s3err.ErrMalformedXML)
		return
	}

	existing, _ := h.DB.ListWebhooks(bucketName)
	hooks := make([]db.BucketWebhook, 0, len(req.Webhooks))
	for _, wh := range req.Webhooks {
		if wh.URL == "" {
			s3err.WriteErrorMsg(w, r, s3err.ErrInvalidArgument, "Url is required")
			return
		}
		// Bucket-credential holders may not point hooks at internal networks.
		if err := webhook.ValidateURL(wh.URL, false); err != nil {
			s3err.WriteErrorMsg(w, r, s3err.ErrInvalidArgument, err.Error())
			return
		}
		secret := wh.Secret
		if secret == "***" {
			// Keep the previously stored secret for the same URL.
			for _, e := range existing {
				if e.URL == wh.URL {
					secret = e.Secret
				}
			}
		}
		hooks = append(hooks, db.BucketWebhook{Name: wh.Name, URL: wh.URL, EventTypes: wh.EventTypes, Secret: secret})
	}

	if err := h.DB.ReplaceWebhooks(bucketName, hooks); err != nil {
		h.Logger.Error("failed to save webhooks", "error", err)
		s3err.WriteError(w, r, s3err.ErrInternalError)
		return
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
	if _, ok = h.checkBucketAccess(w, r, cred, bucketName); !ok {
		return
	}
	if err := h.DB.DeleteAllWebhooks(bucketName); err != nil {
		h.Logger.Error("failed to delete webhooks", "error", err)
		s3err.WriteError(w, r, s3err.ErrInternalError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
