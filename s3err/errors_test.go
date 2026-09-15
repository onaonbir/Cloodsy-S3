package s3err

import (
	"encoding/xml"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func decode(t *testing.T, body string) ErrorResponse {
	t.Helper()
	var e ErrorResponse
	if err := xml.Unmarshal([]byte(body), &e); err != nil {
		t.Fatalf("unmarshal %v\n%s", err, body)
	}
	return e
}

func TestWriteError(t *testing.T) {
	rec := httptest.NewRecorder()
	rec.Header().Set("x-amz-request-id", "req-123")
	rec.Header().Set("Content-Length", "999") // must be dropped
	r := httptest.NewRequest(http.MethodGet, "/bucket/some%20key", nil)
	WriteError(rec, r, ErrNoSuchKey)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/xml" {
		t.Fatalf("content type %q", ct)
	}
	if rec.Header().Get("Content-Length") != "" {
		t.Fatal("stale Content-Length not removed")
	}
	body := rec.Body.String()
	if !strings.HasPrefix(body, xml.Header) {
		t.Fatalf("missing xml header: %q", body)
	}
	e := decode(t, body)
	if e.Code != "NoSuchKey" || e.Message != ErrNoSuchKey.Message {
		t.Fatalf("code/message %+v", e)
	}
	if e.RequestID != "req-123" {
		t.Fatalf("RequestId %q", e.RequestID)
	}
	if e.HostID != "cloodsys3" {
		t.Fatalf("HostId %q", e.HostID)
	}
	if e.Resource != "/bucket/some key" {
		t.Fatalf("Resource %q", e.Resource)
	}
	if strings.Contains(body, "<Region>") {
		t.Fatal("Region element present on a plain error")
	}
	if !strings.Contains(body, "<Error>") || !strings.Contains(body, "</Error>") {
		t.Fatalf("root element: %s", body)
	}
}

func TestWriteErrorMsg(t *testing.T) {
	rec := httptest.NewRecorder()
	WriteErrorMsg(rec, httptest.NewRequest(http.MethodPut, "/b/k", nil), ErrInvalidRequest, "custom <message> & stuff")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status %d", rec.Code)
	}
	e := decode(t, rec.Body.String())
	if e.Code != "InvalidRequest" || e.Message != "custom <message> & stuff" {
		t.Fatalf("%+v", e)
	}
	if e.RequestID != "" {
		t.Fatalf("RequestId %q without header", e.RequestID)
	}
	if !strings.Contains(rec.Body.String(), "<RequestId></RequestId>") {
		t.Fatalf("RequestId element must always be present: %s", rec.Body.String())
	}
}

func TestWriteRegionError(t *testing.T) {
	rec := httptest.NewRecorder()
	rec.Header().Set("x-amz-request-id", "r")
	WriteRegionError(rec, httptest.NewRequest(http.MethodGet, "/b", nil), "eu-central-1")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status %d", rec.Code)
	}
	e := decode(t, rec.Body.String())
	if e.Code != "AuthorizationHeaderMalformed" {
		t.Fatalf("code %q", e.Code)
	}
	if e.Region != "eu-central-1" || !strings.Contains(rec.Body.String(), "<Region>eu-central-1</Region>") {
		t.Fatalf("Region missing: %s", rec.Body.String())
	}
	if !strings.Contains(e.Message, "expecting 'eu-central-1'") {
		t.Fatalf("message %q", e.Message)
	}
}

func TestS3ErrorAsGoError(t *testing.T) {
	var err error = ErrQuotaExceeded
	if err.Error() != "QuotaExceeded: The bucket quota has been exceeded." {
		t.Fatalf("Error() %q", err.Error())
	}
	wrapped := errors.Join(errors.New("ctx"), ErrBadDigest)
	var s3e S3Error
	if !errors.As(wrapped, &s3e) || s3e.Code != "BadDigest" || s3e.HTTPStatus != http.StatusBadRequest {
		t.Fatalf("errors.As failed: %+v", s3e)
	}
	// Every predefined error carries a status, code and message.
	for _, e := range []S3Error{
		ErrAccessDenied, ErrAuthorizationHeaderMalformed, ErrBadDigest, ErrBucketAlreadyExists, ErrBucketAlreadyOwnedByYou,
		ErrBucketNotEmpty, ErrEntityTooLarge, ErrEntityTooSmall, ErrIncompleteBody, ErrInternalError, ErrInvalidAccessKeyId,
		ErrInvalidArgument, ErrInvalidBucketName, ErrInvalidDigest, ErrInvalidPart, ErrInvalidPartOrder, ErrInvalidRange,
		ErrInvalidRequest, ErrKeyTooLong, ErrMalformedXML, ErrMetadataTooLarge, ErrMethodNotAllowed, ErrMissingContentLength,
		ErrMissingSecurityHeader, ErrNoSuchBucket, ErrNoSuchKey, ErrNoSuchLifecycleConfiguration, ErrNoSuchUpload,
		ErrNoSuchVersion, ErrNotImplemented, ErrPreconditionFailed, ErrQuotaExceeded, ErrRequestTimeTooSkewed,
		ErrSignatureDoesNotMatch, ErrXAmzContentSHA256Mismatch,
	} {
		if e.HTTPStatus < 400 || e.HTTPStatus > 599 || e.Code == "" || e.Message == "" {
			t.Errorf("malformed error definition %+v", e)
		}
	}
	if ErrKeyTooLong.Code != "KeyTooLongError" || ErrInvalidRange.HTTPStatus != http.StatusRequestedRangeNotSatisfiable || ErrMissingContentLength.HTTPStatus != http.StatusLengthRequired {
		t.Fatal("spot-check of specific codes/statuses failed")
	}
}
