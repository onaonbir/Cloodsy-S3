package s3err

import (
	"encoding/xml"
	"net/http"
)

type ErrorResponse struct {
	XMLName   xml.Name `xml:"Error"`
	Code      string   `xml:"Code"`
	Message   string   `xml:"Message"`
	Resource  string   `xml:"Resource,omitempty"`
	RequestID string   `xml:"RequestId"`
}

type S3Error struct {
	HTTPStatus int
	Code       string
	Message    string
}

var (
	ErrAccessDenied              = S3Error{http.StatusForbidden, "AccessDenied", "Access Denied"}
	ErrBucketAlreadyExists       = S3Error{http.StatusConflict, "BucketAlreadyExists", "The requested bucket name is not available."}
	ErrBucketAlreadyOwnedByYou   = S3Error{http.StatusConflict, "BucketAlreadyOwnedByYou", "Your previous request to create the named bucket succeeded and you already own it."}
	ErrBucketNotEmpty            = S3Error{http.StatusConflict, "BucketNotEmpty", "The bucket you tried to delete is not empty."}
	ErrInternalError             = S3Error{http.StatusInternalServerError, "InternalError", "We encountered an internal error. Please try again."}
	ErrInvalidArgument           = S3Error{http.StatusBadRequest, "InvalidArgument", "Invalid Argument."}
	ErrInvalidBucketName         = S3Error{http.StatusBadRequest, "InvalidBucketName", "The specified bucket is not valid."}
	ErrInvalidPart               = S3Error{http.StatusBadRequest, "InvalidPart", "One or more of the specified parts could not be found."}
	ErrInvalidPartOrder          = S3Error{http.StatusBadRequest, "InvalidPartOrder", "The list of parts was not in ascending order."}
	ErrMalformedXML              = S3Error{http.StatusBadRequest, "MalformedXML", "The XML you provided was not well-formed."}
	ErrMethodNotAllowed          = S3Error{http.StatusMethodNotAllowed, "MethodNotAllowed", "The specified method is not allowed against this resource."}
	ErrMissingContentLength      = S3Error{http.StatusLengthRequired, "MissingContentLength", "You must provide the Content-Length HTTP header."}
	ErrNoSuchBucket              = S3Error{http.StatusNotFound, "NoSuchBucket", "The specified bucket does not exist."}
	ErrNoSuchKey                 = S3Error{http.StatusNotFound, "NoSuchKey", "The specified key does not exist."}
	ErrNoSuchUpload              = S3Error{http.StatusNotFound, "NoSuchUpload", "The specified multipart upload does not exist."}
	ErrSignatureDoesNotMatch     = S3Error{http.StatusForbidden, "SignatureDoesNotMatch", "The request signature we calculated does not match the signature you provided."}
	ErrInvalidAccessKeyId        = S3Error{http.StatusForbidden, "InvalidAccessKeyId", "The AWS Access Key Id you provided does not exist in our records."}
	ErrMissingSecurityHeader     = S3Error{http.StatusBadRequest, "MissingSecurityHeader", "Your request was missing a required header."}
	ErrRequestTimeTooSkewed      = S3Error{http.StatusForbidden, "RequestTimeTooSkewed", "The difference between the request time and the current time is too large."}
	ErrEntityTooLarge                = S3Error{http.StatusBadRequest, "EntityTooLarge", "Your proposed upload exceeds the maximum allowed object size."}
	ErrNoSuchLifecycleConfiguration  = S3Error{http.StatusNotFound, "NoSuchLifecycleConfiguration", "The lifecycle configuration does not exist."}
)

func WriteError(w http.ResponseWriter, r *http.Request, s3err S3Error) {
	resp := ErrorResponse{
		Code:      s3err.Code,
		Message:   s3err.Message,
		Resource:  r.URL.Path,
		RequestID: w.Header().Get("x-amz-request-id"),
	}

	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(s3err.HTTPStatus)
	xml.NewEncoder(w).Encode(resp)
}

func WriteErrorMsg(w http.ResponseWriter, r *http.Request, s3err S3Error, msg string) {
	resp := ErrorResponse{
		Code:      s3err.Code,
		Message:   msg,
		Resource:  r.URL.Path,
		RequestID: w.Header().Get("x-amz-request-id"),
	}

	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(s3err.HTTPStatus)
	xml.NewEncoder(w).Encode(resp)
}
