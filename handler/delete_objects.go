package handler

import (
	"errors"
	"net/http"

	"github.com/onaonbir/Cloodsy-S3/s3err"
	"github.com/onaonbir/Cloodsy-S3/s3xml"
	"github.com/onaonbir/Cloodsy-S3/service"
)

// DeleteMultipleObjects handles POST /<bucket>?delete
func (h *Handler) DeleteMultipleObjects(w http.ResponseWriter, r *http.Request) {
	cred, ok := h.authenticateRequest(w, r)
	if !ok {
		return
	}
	if !h.checkWriteAccess(w, r, cred) {
		return
	}

	bucketName, _ := getBucketAndKey(r)
	bucket, ok := h.checkBucketAccess(w, r, cred, bucketName)
	if !ok {
		return
	}

	var deleteReq s3xml.DeleteRequest
	if err := limitedXMLDecode(r.Body, &deleteReq); err != nil {
		s3err.WriteError(w, r, s3err.ErrMalformedXML)
		return
	}
	if len(deleteReq.Objects) > maxDeleteObjects {
		s3err.WriteError(w, r, s3err.ErrMalformedXML)
		return
	}

	result := s3xml.DeleteResult{Xmlns: "http://s3.amazonaws.com/doc/2006-03-01/"}

	for _, obj := range deleteReq.Objects {
		if !isValidObjectKey(obj.Key) {
			result.Errors = append(result.Errors, s3xml.DeleteError{
				Key: obj.Key, VersionId: obj.VersionId, Code: "InvalidArgument", Message: "Invalid object key.",
			})
			continue
		}
		res, err := h.Objects.Delete(bucket, obj.Key, obj.VersionId)
		if err != nil && !errors.Is(err, service.ErrNoSuchVersion) {
			h.Logger.Error("failed to delete object", "key", obj.Key, "error", err)
			result.Errors = append(result.Errors, s3xml.DeleteError{
				Key: obj.Key, VersionId: obj.VersionId, Code: "InternalError", Message: "We encountered an internal error. Please try again.",
			})
			continue
		}
		if deleteReq.Quiet {
			continue
		}
		d := s3xml.DeletedObject{Key: obj.Key, VersionId: obj.VersionId}
		if res != nil && res.DeleteMarker {
			d.DeleteMarker = true
			if obj.VersionId == "" {
				d.DeleteMarkerVersionId = res.VersionID
			}
		}
		result.Deleted = append(result.Deleted, d)
	}

	h.writeXML(w, http.StatusOK, result)
}
