package handler

import (
	"encoding/xml"
	"net/http"

	"github.com/onaonbir/Cloodsy-S3/s3err"
)

type versioningConfiguration struct {
	XMLName xml.Name `xml:"VersioningConfiguration"`
	Xmlns   string   `xml:"xmlns,attr,omitempty"`
	Status  string   `xml:"Status,omitempty"`
}

// GetBucketVersioning handles GET /<bucket>?versioning
func (h *Handler) GetBucketVersioning(w http.ResponseWriter, r *http.Request) {
	cred, ok := h.authenticateRequest(w, r)
	if !ok {
		return
	}

	bucketName, _ := getBucketAndKey(r)
	bucket, ok := h.checkBucketAccess(w, r, cred, bucketName)
	if !ok {
		return
	}

	result := versioningConfiguration{
		Xmlns:  "http://s3.amazonaws.com/doc/2006-03-01/",
		Status: bucket.Versioning,
	}

	h.writeXML(w, http.StatusOK, result)
}

// PutBucketVersioning handles PUT /<bucket>?versioning
func (h *Handler) PutBucketVersioning(w http.ResponseWriter, r *http.Request) {
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

	var req versioningConfiguration
	if err := limitedXMLDecode(r.Body, &req); err != nil {
		s3err.WriteError(w, r, s3err.ErrMalformedXML)
		return
	}

	if req.Status != "Enabled" && req.Status != "Suspended" {
		s3err.WriteError(w, r, s3err.ErrInvalidArgument)
		return
	}

	if err := h.DB.SetBucketVersioning(bucketName, req.Status); err != nil {
		h.Logger.Error("failed to set bucket versioning", "error", err)
		s3err.WriteError(w, r, s3err.ErrInternalError)
		return
	}

	w.WriteHeader(http.StatusOK)
}
