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
	Region    string   `xml:"Region,omitempty"`
	RequestID string   `xml:"RequestId"`
	HostID    string   `xml:"HostId"`
}

type S3Error struct {
	HTTPStatus int
	Code       string
	Message    string
}

// Error makes S3Error usable as a Go error so service-layer code can return
// it and handlers can surface it unchanged.
func (e S3Error) Error() string { return e.Code + ": " + e.Message }

var (
	ErrAccessDenied                 = S3Error{http.StatusForbidden, "AccessDenied", "Access Denied"}
	ErrAuthorizationHeaderMalformed = S3Error{http.StatusBadRequest, "AuthorizationHeaderMalformed", "The authorization header is malformed."}
	ErrBadDigest                    = S3Error{http.StatusBadRequest, "BadDigest", "The Content-MD5 or checksum value that you specified did not match what the server received."}
	ErrBucketAlreadyExists          = S3Error{http.StatusConflict, "BucketAlreadyExists", "The requested bucket name is not available."}
	ErrBucketAlreadyOwnedByYou      = S3Error{http.StatusConflict, "BucketAlreadyOwnedByYou", "Your previous request to create the named bucket succeeded and you already own it."}
	ErrBucketNotEmpty               = S3Error{http.StatusConflict, "BucketNotEmpty", "The bucket you tried to delete is not empty."}
	ErrEntityTooLarge               = S3Error{http.StatusBadRequest, "EntityTooLarge", "Your proposed upload exceeds the maximum allowed object size."}
	ErrEntityTooSmall               = S3Error{http.StatusBadRequest, "EntityTooSmall", "Your proposed upload is smaller than the minimum allowed object size."}
	ErrIncompleteBody               = S3Error{http.StatusBadRequest, "IncompleteBody", "You did not provide the number of bytes specified by the Content-Length HTTP header."}
	ErrInternalError                = S3Error{http.StatusInternalServerError, "InternalError", "We encountered an internal error. Please try again."}
	ErrInvalidAccessKeyId           = S3Error{http.StatusForbidden, "InvalidAccessKeyId", "The AWS Access Key Id you provided does not exist in our records."}
	ErrInvalidArgument              = S3Error{http.StatusBadRequest, "InvalidArgument", "Invalid Argument."}
	ErrInvalidBucketName            = S3Error{http.StatusBadRequest, "InvalidBucketName", "The specified bucket is not valid."}
	ErrInvalidDigest                = S3Error{http.StatusBadRequest, "InvalidDigest", "The Content-MD5 or checksum value that you specified is not valid."}
	ErrInvalidPart                  = S3Error{http.StatusBadRequest, "InvalidPart", "One or more of the specified parts could not be found."}
	ErrInvalidPartOrder             = S3Error{http.StatusBadRequest, "InvalidPartOrder", "The list of parts was not in ascending order."}
	ErrInvalidRange                 = S3Error{http.StatusRequestedRangeNotSatisfiable, "InvalidRange", "The requested range is not satisfiable."}
	ErrInvalidRequest               = S3Error{http.StatusBadRequest, "InvalidRequest", "Invalid Request."}
	ErrKeyTooLong                   = S3Error{http.StatusBadRequest, "KeyTooLongError", "Your key is too long."}
	ErrMalformedXML                 = S3Error{http.StatusBadRequest, "MalformedXML", "The XML you provided was not well-formed or did not validate against our published schema."}
	ErrMetadataTooLarge             = S3Error{http.StatusBadRequest, "MetadataTooLarge", "Your metadata headers exceed the maximum allowed metadata size."}
	ErrMethodNotAllowed             = S3Error{http.StatusMethodNotAllowed, "MethodNotAllowed", "The specified method is not allowed against this resource."}
	ErrMissingContentLength         = S3Error{http.StatusLengthRequired, "MissingContentLength", "You must provide the Content-Length HTTP header."}
	ErrMissingSecurityHeader        = S3Error{http.StatusBadRequest, "MissingSecurityHeader", "Your request was missing a required header."}
	ErrNoSuchBucket                 = S3Error{http.StatusNotFound, "NoSuchBucket", "The specified bucket does not exist."}
	ErrNoSuchKey                    = S3Error{http.StatusNotFound, "NoSuchKey", "The specified key does not exist."}
	ErrNoSuchLifecycleConfiguration = S3Error{http.StatusNotFound, "NoSuchLifecycleConfiguration", "The lifecycle configuration does not exist."}
	ErrNoSuchUpload                 = S3Error{http.StatusNotFound, "NoSuchUpload", "The specified multipart upload does not exist."}
	ErrNoSuchVersion                = S3Error{http.StatusNotFound, "NoSuchVersion", "The specified version does not exist."}
	ErrNotImplemented               = S3Error{http.StatusNotImplemented, "NotImplemented", "A header or query you provided implies functionality that is not implemented."}
	ErrPreconditionFailed           = S3Error{http.StatusPreconditionFailed, "PreconditionFailed", "At least one of the pre-conditions you specified did not hold."}
	ErrQuotaExceeded                = S3Error{http.StatusForbidden, "QuotaExceeded", "The bucket quota has been exceeded."}
	ErrRequestTimeTooSkewed         = S3Error{http.StatusForbidden, "RequestTimeTooSkewed", "The difference between the request time and the current time is too large."}
	ErrSignatureDoesNotMatch        = S3Error{http.StatusForbidden, "SignatureDoesNotMatch", "The request signature we calculated does not match the signature you provided."}
	ErrXAmzContentSHA256Mismatch    = S3Error{http.StatusBadRequest, "XAmzContentSHA256Mismatch", "The provided 'x-amz-content-sha256' header does not match what was computed."}
)

func write(w http.ResponseWriter, r *http.Request, s3err S3Error, msg, region string) {
	resp := ErrorResponse{
		Code:      s3err.Code,
		Message:   msg,
		Resource:  r.URL.Path,
		Region:    region,
		RequestID: w.Header().Get("x-amz-request-id"),
		HostID:    "cloodsys3",
	}
	w.Header().Set("Content-Type", "application/xml")
	w.Header().Del("Content-Length")
	w.WriteHeader(s3err.HTTPStatus)
	w.Write([]byte(xml.Header))
	xml.NewEncoder(w).Encode(resp)
}

func WriteError(w http.ResponseWriter, r *http.Request, s3err S3Error) {
	write(w, r, s3err, s3err.Message, "")
}

func WriteErrorMsg(w http.ResponseWriter, r *http.Request, s3err S3Error, msg string) {
	write(w, r, s3err, msg, "")
}

// WriteRegionError writes AuthorizationHeaderMalformed with the expected
// region so SDKs can auto-correct.
func WriteRegionError(w http.ResponseWriter, r *http.Request, region string) {
	write(w, r, ErrAuthorizationHeaderMalformed, "The authorization header is malformed; the region is wrong; expecting '"+region+"'.", region)
}
