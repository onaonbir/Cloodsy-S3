package handler

import (
	"net/http"

	"github.com/onaonbir/Cloodsy-S3/s3err"
	"github.com/onaonbir/Cloodsy-S3/s3xml"
)

// GetBucketLifecycle handles GET /<bucket>?lifecycle
func (h *Handler) GetBucketLifecycle(w http.ResponseWriter, r *http.Request) {
	cred, ok := h.authenticateRequest(w, r)
	if !ok {
		return
	}

	bucketName, _ := getBucketAndKey(r)
	_, ok = h.checkBucketAccess(w, r, cred, bucketName)
	if !ok {
		return
	}

	rules, err := h.DB.GetLifecycleRules(bucketName)
	if err != nil {
		h.Logger.Error("failed to get lifecycle rules", "error", err)
		s3err.WriteError(w, r, s3err.ErrInternalError)
		return
	}

	if len(rules) == 0 {
		s3err.WriteErrorMsg(w, r, s3err.ErrNoSuchLifecycleConfiguration, "The lifecycle configuration does not exist.")
		return
	}

	result := s3xml.LifecycleConfiguration{
		Xmlns: "http://s3.amazonaws.com/doc/2006-03-01/",
	}
	for _, rule := range rules {
		result.Rules = append(result.Rules, s3xml.LifecycleRule{
			ID:     rule.Prefix,
			Prefix: rule.Prefix,
			Status: "Enabled",
			Expiration: s3xml.LifecycleExpiration{
				Days: rule.ExpirationDays,
			},
		})
	}

	h.writeXML(w, http.StatusOK, result)
}

// PutBucketLifecycle handles PUT /<bucket>?lifecycle
func (h *Handler) PutBucketLifecycle(w http.ResponseWriter, r *http.Request) {
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

	var req s3xml.LifecycleConfiguration
	if err := limitedXMLDecode(r.Body, &req); err != nil {
		s3err.WriteError(w, r, s3err.ErrMalformedXML)
		return
	}

	// Delete existing rules and replace with new ones
	if err := h.DB.DeleteLifecycleRules(bucketName); err != nil {
		h.Logger.Error("failed to delete existing lifecycle rules", "error", err)
		s3err.WriteError(w, r, s3err.ErrInternalError)
		return
	}

	for _, rule := range req.Rules {
		if rule.Expiration.Days <= 0 {
			s3err.WriteError(w, r, s3err.ErrInvalidArgument)
			return
		}
		if err := h.DB.PutLifecycleRule(bucketName, "", rule.Prefix, rule.Expiration.Days); err != nil {
			h.Logger.Error("failed to put lifecycle rule", "error", err)
			s3err.WriteError(w, r, s3err.ErrInternalError)
			return
		}
	}

	w.WriteHeader(http.StatusOK)
}

// DeleteBucketLifecycle handles DELETE /<bucket>?lifecycle
func (h *Handler) DeleteBucketLifecycle(w http.ResponseWriter, r *http.Request) {
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

	if err := h.DB.DeleteLifecycleRules(bucketName); err != nil {
		h.Logger.Error("failed to delete lifecycle rules", "error", err)
		s3err.WriteError(w, r, s3err.ErrInternalError)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}
