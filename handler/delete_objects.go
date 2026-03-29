package handler

import (
	"net/http"

	"github.com/onaonbir/Cloodsy-S3/s3err"
	"github.com/onaonbir/Cloodsy-S3/s3xml"
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

	// Enforce S3 batch delete limit
	if len(deleteReq.Objects) > maxDeleteObjects {
		s3err.WriteError(w, r, s3err.ErrInvalidArgument)
		return
	}

	result := s3xml.DeleteResult{
		Xmlns: "http://s3.amazonaws.com/doc/2006-03-01/",
	}

	for _, obj := range deleteReq.Objects {
		if !isValidObjectKey(obj.Key) {
			result.Errors = append(result.Errors, s3xml.DeleteError{
				Key:     obj.Key,
				Code:    "InvalidArgument",
				Message: "Invalid object key.",
			})
			continue
		}
		err := h.DB.DeleteObjectMeta(bucket.ID, obj.Key)
		if err != nil {
			h.Logger.Error("failed to delete object metadata", "key", obj.Key, "error", err)
			result.Errors = append(result.Errors, s3xml.DeleteError{
				Key:     obj.Key,
				Code:    "InternalError",
				Message: "We encountered an internal error. Please try again.",
			})
			continue
		}
		if err := h.Storage.DeleteObject(bucketName, obj.Key); err != nil {
			h.Logger.Error("failed to delete object from storage", "key", obj.Key, "error", err)
		}
		if !deleteReq.Quiet {
			result.Deleted = append(result.Deleted, s3xml.DeletedObject{Key: obj.Key})
		}
	}

	h.writeXML(w, http.StatusOK, result)
}
