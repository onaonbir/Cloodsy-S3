package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"
)

var multiSpaceRegex = regexp.MustCompile(`\s+`)

// Typed verification errors so handlers can map them to the S3 error codes
// AWS SDKs rely on (e.g. RequestTimeTooSkewed triggers clock correction).
var (
	ErrMissingAuth       = errors.New("missing Authorization header")
	ErrMissingDate       = errors.New("missing X-Amz-Date header")
	ErrInvalidDate       = errors.New("invalid X-Amz-Date format")
	ErrTimeSkewed        = errors.New("request time too skewed")
	ErrDateMismatch      = errors.New("credential date does not match X-Amz-Date")
	ErrRegionMismatch    = errors.New("credential region does not match")
	ErrServiceMismatch   = errors.New("credential service must be s3")
	ErrMissingContentSHA = errors.New("missing x-amz-content-sha256 header")
	ErrSignatureMismatch = errors.New("signature mismatch")
	ErrPresignedExpired  = errors.New("presigned URL has expired")
	ErrPresignedNotYet   = errors.New("presigned URL is not yet valid")
)

// MaxClockSkew mirrors AWS: requests dated more than 15 minutes away from
// server time are rejected.
const MaxClockSkew = 15 * time.Minute

// VerifySignature verifies the AWS Signature V4 of the request. auth may be
// the already-parsed Authorization header (nil parses it again); on success
// its AmzDate/Scope fields are populated.
func VerifySignature(r *http.Request, secretKey string, region string, auth *SigV4Auth) error {
	if auth == nil {
		authHeader := r.Header.Get("Authorization")
		if authHeader == "" {
			return ErrMissingAuth
		}
		var err error
		auth, err = ParseAuthorizationHeader(authHeader)
		if err != nil {
			return err
		}
	}

	amzDate := r.Header.Get("X-Amz-Date")
	if amzDate == "" {
		// SigV4 also allows the standard Date header.
		if d := r.Header.Get("Date"); d != "" {
			if t, perr := http.ParseTime(d); perr == nil {
				amzDate = t.UTC().Format(sigV4TimeFormat)
			}
		}
	}
	if amzDate == "" {
		return ErrMissingDate
	}

	reqTime, err := time.Parse(sigV4TimeFormat, amzDate)
	if err != nil {
		return ErrInvalidDate
	}

	if time.Since(reqTime).Abs() > MaxClockSkew {
		return ErrTimeSkewed
	}

	dateStamp := amzDate[:8] // YYYYMMDD from X-Amz-Date
	if auth.Date != dateStamp {
		return ErrDateMismatch
	}
	if auth.Region != region {
		return ErrRegionMismatch
	}
	if auth.Service != "s3" {
		return ErrServiceMismatch
	}
	if r.Header.Get("X-Amz-Content-Sha256") == "" {
		return ErrMissingContentSHA
	}

	canonicalRequest := buildCanonicalRequest(r, auth.SignedHeaders, r.Header.Get("X-Amz-Content-Sha256"), r.URL.Query())

	scope := dateStamp + "/" + region + "/s3/aws4_request"
	stringToSign := "AWS4-HMAC-SHA256\n" + amzDate + "\n" + scope + "\n" + hashSHA256([]byte(canonicalRequest))

	signingKey := DeriveSigningKey(secretKey, dateStamp, region, "s3")
	expectedSig := hex.EncodeToString(hmacSHA256(signingKey, []byte(stringToSign)))

	if !hmac.Equal([]byte(expectedSig), []byte(auth.Signature)) {
		return ErrSignatureMismatch
	}

	auth.AmzDate = amzDate
	auth.Scope = scope
	return nil
}

// buildCanonicalRequest assembles the SigV4 canonical request. query is the
// query string to canonicalize (presigned verification strips X-Amz-Signature).
func buildCanonicalRequest(r *http.Request, signedHeaders []string, payloadHash string, query url.Values) string {
	method := r.Method

	canonicalURI := r.URL.Path
	if canonicalURI == "" {
		canonicalURI = "/"
	}
	canonicalURI = canonicalURIEncode(canonicalURI)

	canonicalQueryString := canonicalQueryEncode(query)

	sorted := make([]string, len(signedHeaders))
	for i, h := range signedHeaders {
		sorted[i] = strings.ToLower(h)
	}
	sort.Strings(sorted)

	var canonicalHeaders strings.Builder
	for _, h := range sorted {
		var val string
		if h == "host" {
			val = r.Host
			if val == "" {
				val = r.Header.Get("Host")
			}
		} else {
			// Multiple values of the same header are joined with commas per spec.
			vals := r.Header.Values(h)
			trimmed := make([]string, len(vals))
			for i, v := range vals {
				trimmed[i] = strings.TrimSpace(v)
			}
			val = strings.Join(trimmed, ",")
		}
		val = multiSpaceRegex.ReplaceAllString(strings.TrimSpace(val), " ")
		canonicalHeaders.WriteString(h)
		canonicalHeaders.WriteString(":")
		canonicalHeaders.WriteString(val)
		canonicalHeaders.WriteString("\n")
	}

	return method + "\n" +
		canonicalURI + "\n" +
		canonicalQueryString + "\n" +
		canonicalHeaders.String() + "\n" +
		strings.Join(sorted, ";") + "\n" +
		payloadHash
}

func canonicalURIEncode(uri string) string {
	parts := strings.Split(uri, "/")
	encoded := make([]string, len(parts))
	for i, p := range parts {
		encoded[i] = uriEncode(p, false)
	}
	return strings.Join(encoded, "/")
}

func canonicalQueryEncode(query url.Values) string {
	keys := make([]string, 0, len(query))
	for k := range query {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var pairs []string
	for _, k := range keys {
		vals := append([]string(nil), query[k]...)
		sort.Strings(vals)
		for _, v := range vals {
			pairs = append(pairs, uriEncode(k, true)+"="+uriEncode(v, true))
		}
	}
	return strings.Join(pairs, "&")
}

func uriEncode(s string, encodeSlash bool) string {
	var buf strings.Builder
	for _, b := range []byte(s) {
		if isUnreserved(b) {
			buf.WriteByte(b)
		} else if b == '/' && !encodeSlash {
			buf.WriteByte(b)
		} else {
			fmt.Fprintf(&buf, "%%%02X", b)
		}
	}
	return buf.String()
}

func isUnreserved(c byte) bool {
	return (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') ||
		(c >= '0' && c <= '9') || c == '-' || c == '.' || c == '_' || c == '~'
}

func hashSHA256(data []byte) string {
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}

// HashSHA256Hex returns the lower-case hex SHA-256 of data.
func HashSHA256Hex(data []byte) string { return hashSHA256(data) }

func hmacSHA256(key, data []byte) []byte {
	h := hmac.New(sha256.New, key)
	h.Write(data)
	return h.Sum(nil)
}

// HMACSHA256 is exported for streaming (aws-chunked) signature verification.
func HMACSHA256(key, data []byte) []byte { return hmacSHA256(key, data) }

// DeriveSigningKey derives the SigV4 signing key for a date/region/service.
func DeriveSigningKey(secret, dateStamp, region, service string) []byte {
	kDate := hmacSHA256([]byte("AWS4"+secret), []byte(dateStamp))
	kRegion := hmacSHA256(kDate, []byte(region))
	kService := hmacSHA256(kRegion, []byte(service))
	return hmacSHA256(kService, []byte("aws4_request"))
}

// VerifyPresignedSignature verifies the signature of a presigned URL request.
func VerifyPresignedSignature(r *http.Request, secretKey string, region string, auth *SigV4Auth) error {
	if auth == nil {
		return fmt.Errorf("missing presigned auth")
	}

	query := r.URL.Query()
	reqTime, err := validatePresignedExpiry(query)
	if err != nil {
		return err
	}
	amzDate := query.Get("X-Amz-Date")

	dateStamp := reqTime.Format("20060102")
	if auth.Date != dateStamp {
		return ErrDateMismatch
	}
	if auth.Region != region {
		return ErrRegionMismatch
	}

	// Canonical query: every param except X-Amz-Signature; payload is UNSIGNED.
	filtered := make(url.Values, len(query))
	for k, v := range query {
		if k != "X-Amz-Signature" {
			filtered[k] = v
		}
	}
	canonicalRequest := buildCanonicalRequest(r, auth.SignedHeaders, "UNSIGNED-PAYLOAD", filtered)

	scope := dateStamp + "/" + region + "/s3/aws4_request"
	stringToSign := "AWS4-HMAC-SHA256\n" + amzDate + "\n" + scope + "\n" + hashSHA256([]byte(canonicalRequest))

	signingKey := DeriveSigningKey(secretKey, dateStamp, region, "s3")
	expectedSig := hex.EncodeToString(hmacSHA256(signingKey, []byte(stringToSign)))

	if !hmac.Equal([]byte(expectedSig), []byte(auth.Signature)) {
		return ErrSignatureMismatch
	}
	auth.AmzDate = amzDate
	auth.Scope = scope
	return nil
}

// HashPayload computes SHA256 of request body for signature verification.
func HashPayload(body io.Reader) string {
	if body == nil {
		return hashSHA256([]byte(""))
	}
	h := sha256.New()
	io.Copy(h, body)
	return hex.EncodeToString(h.Sum(nil))
}
