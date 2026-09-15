package handler

import (
	"fmt"
	"net/http"

	"github.com/onaonbir/Cloodsy-S3/db"
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
	if _, ok = h.checkBucketAccess(w, r, cred, bucketName); !ok {
		return
	}

	rules, err := h.DB.GetLifecycleRules(bucketName)
	if err != nil {
		h.Logger.Error("failed to get lifecycle rules", "error", err)
		s3err.WriteError(w, r, s3err.ErrInternalError)
		return
	}
	if len(rules) == 0 {
		s3err.WriteError(w, r, s3err.ErrNoSuchLifecycleConfiguration)
		return
	}

	result := s3xml.LifecycleConfiguration{Xmlns: "http://s3.amazonaws.com/doc/2006-03-01/"}
	for _, rule := range rules {
		prefix := rule.Prefix
		id := rule.Name
		if id == "" {
			id = fmt.Sprintf("rule-%d", rule.ID)
		}
		out := s3xml.LifecycleRule{
			ID:     id,
			Filter: &s3xml.LifecycleFilter{Prefix: &prefix},
			Status: rule.Status,
		}
		if rule.ExpirationDays > 0 || rule.ExpireDeleteMarkers {
			out.Expiration = &s3xml.LifecycleExpiration{Days: rule.ExpirationDays, ExpiredObjectDeleteMarker: rule.ExpireDeleteMarkers && rule.ExpirationDays == 0}
		}
		if rule.NoncurrentDays > 0 {
			out.NoncurrentVersionExpiration = &s3xml.NoncurrentVersionExpiration{NoncurrentDays: rule.NoncurrentDays}
		}
		if rule.AbortMultipartDays > 0 {
			out.AbortIncompleteMultipartUpload = &s3xml.AbortIncompleteMultipartUpload{DaysAfterInitiation: rule.AbortMultipartDays}
		}
		result.Rules = append(result.Rules, out)
	}
	h.writeXML(w, http.StatusOK, result)
}

// lifecycleRuleFromXML validates one rule and converts it to the DB model.
func lifecycleRuleFromXML(bucketName string, rule s3xml.LifecycleRule) (db.LifecycleRule, error) {
	out := db.LifecycleRule{BucketName: bucketName, Name: rule.ID, Status: rule.Status}

	// Prefix: <Filter><Prefix>, <Filter><And><Prefix>, or legacy top-level <Prefix>.
	switch {
	case rule.Filter != nil && rule.Filter.Prefix != nil:
		out.Prefix = *rule.Filter.Prefix
	case rule.Filter != nil && rule.Filter.And != nil && rule.Filter.And.Prefix != nil:
		out.Prefix = *rule.Filter.And.Prefix
	case rule.Prefix != nil:
		out.Prefix = *rule.Prefix
	}
	if rule.Filter != nil && (rule.Filter.Tag != nil || (rule.Filter.And != nil && len(rule.Filter.And.Tags) > 0)) {
		return out, fmt.Errorf("tag-based lifecycle filters are not supported")
	}

	switch rule.Status {
	case "Enabled", "Disabled":
	default:
		return out, fmt.Errorf("invalid Status")
	}

	hasAction := false
	if rule.Expiration != nil {
		if rule.Expiration.Date != "" {
			return out, fmt.Errorf("Expiration/Date is not supported; use Days")
		}
		if rule.Expiration.Days < 0 {
			return out, fmt.Errorf("Expiration/Days must be positive")
		}
		if rule.Expiration.Days > 0 {
			out.ExpirationDays = rule.Expiration.Days
			hasAction = true
		}
		if rule.Expiration.ExpiredObjectDeleteMarker {
			if rule.Expiration.Days > 0 {
				return out, fmt.Errorf("ExpiredObjectDeleteMarker cannot be combined with Days")
			}
			out.ExpireDeleteMarkers = true
			hasAction = true
		}
	}
	if rule.NoncurrentVersionExpiration != nil {
		if rule.NoncurrentVersionExpiration.NoncurrentDays <= 0 {
			return out, fmt.Errorf("NoncurrentDays must be positive")
		}
		out.NoncurrentDays = rule.NoncurrentVersionExpiration.NoncurrentDays
		hasAction = true
	}
	if rule.AbortIncompleteMultipartUpload != nil {
		if rule.AbortIncompleteMultipartUpload.DaysAfterInitiation <= 0 {
			return out, fmt.Errorf("DaysAfterInitiation must be positive")
		}
		out.AbortMultipartDays = rule.AbortIncompleteMultipartUpload.DaysAfterInitiation
		hasAction = true
	}
	if len(rule.Transitions) > 0 && !hasAction {
		return out, fmt.Errorf("storage class transitions are not supported")
	}
	if !hasAction {
		return out, fmt.Errorf("rule has no supported action")
	}
	return out, nil
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
	if _, ok = h.checkBucketAccess(w, r, cred, bucketName); !ok {
		return
	}

	var req s3xml.LifecycleConfiguration
	if err := limitedXMLDecode(r.Body, &req); err != nil {
		s3err.WriteError(w, r, s3err.ErrMalformedXML)
		return
	}
	if len(req.Rules) == 0 || len(req.Rules) > 1000 {
		s3err.WriteError(w, r, s3err.ErrMalformedXML)
		return
	}

	rules := make([]db.LifecycleRule, 0, len(req.Rules))
	seen := map[string]bool{}
	for _, rule := range req.Rules {
		converted, err := lifecycleRuleFromXML(bucketName, rule)
		if err != nil {
			s3err.WriteErrorMsg(w, r, s3err.ErrInvalidArgument, err.Error())
			return
		}
		if seen[converted.Prefix] {
			s3err.WriteErrorMsg(w, r, s3err.ErrInvalidArgument, "Only one rule per prefix is supported.")
			return
		}
		seen[converted.Prefix] = true
		rules = append(rules, converted)
	}

	if err := h.DB.ReplaceLifecycleRules(bucketName, rules); err != nil {
		h.Logger.Error("failed to save lifecycle rules", "error", err)
		s3err.WriteError(w, r, s3err.ErrInternalError)
		return
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
	if _, ok = h.checkBucketAccess(w, r, cred, bucketName); !ok {
		return
	}
	if err := h.DB.DeleteLifecycleRules(bucketName); err != nil {
		h.Logger.Error("failed to delete lifecycle rules", "error", err)
		s3err.WriteError(w, r, s3err.ErrInternalError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
