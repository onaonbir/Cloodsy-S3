package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
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

// VerifySignature verifies the AWS Signature V4 of the request.
func VerifySignature(r *http.Request, secretKey string, region string) error {
	authHeader := r.Header.Get("Authorization")
	if authHeader == "" {
		return fmt.Errorf("missing Authorization header")
	}

	auth, err := ParseAuthorizationHeader(authHeader)
	if err != nil {
		return err
	}

	// Check time skew
	amzDate := r.Header.Get("X-Amz-Date")
	if amzDate == "" {
		return fmt.Errorf("missing X-Amz-Date header")
	}

	reqTime, err := time.Parse("20060102T150405Z", amzDate)
	if err != nil {
		return fmt.Errorf("invalid X-Amz-Date format")
	}

	if time.Since(reqTime).Abs() > 5*time.Minute {
		return fmt.Errorf("request time too skewed")
	}

	// Validate credential date matches X-Amz-Date
	dateStamp := amzDate[:8] // YYYYMMDD from X-Amz-Date
	if auth.Date != dateStamp {
		return fmt.Errorf("credential date does not match X-Amz-Date")
	}

	// Validate credential region matches server region
	if auth.Region != region {
		return fmt.Errorf("credential region does not match")
	}

	// Validate credential service is "s3"
	if auth.Service != "s3" {
		return fmt.Errorf("credential service must be s3")
	}

	// Require X-Amz-Content-Sha256 header
	if r.Header.Get("X-Amz-Content-Sha256") == "" {
		return fmt.Errorf("missing x-amz-content-sha256 header")
	}

	// Build canonical request
	canonicalRequest := buildCanonicalRequest(r, auth.SignedHeaders)

	// Build string to sign
	scope := dateStamp + "/" + region + "/s3/aws4_request"
	stringToSign := "AWS4-HMAC-SHA256\n" + amzDate + "\n" + scope + "\n" + hashSHA256([]byte(canonicalRequest))

	// Calculate signature
	signingKey := deriveSigningKey(secretKey, dateStamp, region, "s3")
	expectedSig := hex.EncodeToString(hmacSHA256(signingKey, []byte(stringToSign)))

	if !hmac.Equal([]byte(expectedSig), []byte(auth.Signature)) {
		return fmt.Errorf("signature mismatch")
	}

	return nil
}

func buildCanonicalRequest(r *http.Request, signedHeaders []string) string {
	// HTTP method
	method := r.Method

	// Canonical URI
	canonicalURI := r.URL.Path
	if canonicalURI == "" {
		canonicalURI = "/"
	}
	// URI-encode each path segment
	canonicalURI = canonicalURIEncode(canonicalURI)

	// Canonical query string
	canonicalQueryString := canonicalQueryEncode(r.URL.Query())

	// Canonical headers
	sort.Strings(signedHeaders)
	var canonicalHeaders strings.Builder
	for _, h := range signedHeaders {
		val := strings.TrimSpace(r.Header.Get(h))
		if strings.EqualFold(h, "host") {
			val = r.Host
			if val == "" {
				val = r.Header.Get("Host")
			}
		}
		// Collapse sequential whitespace into a single space per AWS SigV4 spec
		val = multiSpaceRegex.ReplaceAllString(val, " ")
		canonicalHeaders.WriteString(strings.ToLower(h) + ":" + val + "\n")
	}

	signedHeadersStr := strings.Join(signedHeaders, ";")

	// Payload hash
	payloadHash := r.Header.Get("X-Amz-Content-Sha256")

	return method + "\n" +
		canonicalURI + "\n" +
		canonicalQueryString + "\n" +
		canonicalHeaders.String() + "\n" +
		signedHeadersStr + "\n" +
		payloadHash
}

func canonicalURIEncode(uri string) string {
	parts := strings.Split(uri, "/")
	var encoded []string
	for _, p := range parts {
		encoded = append(encoded, uriEncode(p, false))
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
		vals := query[k]
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

func hmacSHA256(key, data []byte) []byte {
	h := hmac.New(sha256.New, key)
	h.Write(data)
	return h.Sum(nil)
}

func deriveSigningKey(secret, dateStamp, region, service string) []byte {
	kDate := hmacSHA256([]byte("AWS4"+secret), []byte(dateStamp))
	kRegion := hmacSHA256(kDate, []byte(region))
	kService := hmacSHA256(kRegion, []byte(service))
	kSigning := hmacSHA256(kService, []byte("aws4_request"))
	return kSigning
}

// VerifyPresignedSignature verifies the signature of a presigned URL request.
func VerifyPresignedSignature(r *http.Request, secretKey string, region string, auth *SigV4Auth) error {
	amzDate := r.URL.Query().Get("X-Amz-Date")

	// Validate credential date matches X-Amz-Date
	dateStamp := amzDate[:8]
	if auth.Date != dateStamp {
		return fmt.Errorf("credential date does not match X-Amz-Date")
	}

	// Validate credential region
	if auth.Region != region {
		return fmt.Errorf("credential region does not match")
	}

	// Build canonical request for presigned URL
	canonicalRequest := buildPresignedCanonicalRequest(r, auth.SignedHeaders)

	scope := dateStamp + "/" + region + "/s3/aws4_request"
	stringToSign := "AWS4-HMAC-SHA256\n" + amzDate + "\n" + scope + "\n" + hashSHA256([]byte(canonicalRequest))

	signingKey := deriveSigningKey(secretKey, dateStamp, region, "s3")
	expectedSig := hex.EncodeToString(hmacSHA256(signingKey, []byte(stringToSign)))

	if !hmac.Equal([]byte(expectedSig), []byte(auth.Signature)) {
		return fmt.Errorf("signature mismatch")
	}

	return nil
}

// buildPresignedCanonicalRequest builds the canonical request for presigned URL verification.
// The key difference from header-based auth: query params include all X-Amz-* except X-Amz-Signature,
// and the payload hash is always "UNSIGNED-PAYLOAD".
func buildPresignedCanonicalRequest(r *http.Request, signedHeaders []string) string {
	method := r.Method

	canonicalURI := r.URL.Path
	if canonicalURI == "" {
		canonicalURI = "/"
	}
	canonicalURI = canonicalURIEncode(canonicalURI)

	// Canonical query string: include all query params EXCEPT X-Amz-Signature
	filteredQuery := make(url.Values)
	for k, v := range r.URL.Query() {
		if k != "X-Amz-Signature" {
			filteredQuery[k] = v
		}
	}
	canonicalQueryString := canonicalQueryEncode(filteredQuery)

	// Canonical headers
	sort.Strings(signedHeaders)
	var canonicalHeaders strings.Builder
	for _, h := range signedHeaders {
		val := strings.TrimSpace(r.Header.Get(h))
		if strings.EqualFold(h, "host") {
			val = r.Host
			if val == "" {
				val = r.Header.Get("Host")
			}
		}
		val = multiSpaceRegex.ReplaceAllString(val, " ")
		canonicalHeaders.WriteString(strings.ToLower(h) + ":" + val + "\n")
	}

	signedHeadersStr := strings.Join(signedHeaders, ";")

	// Presigned URLs always use UNSIGNED-PAYLOAD
	payloadHash := "UNSIGNED-PAYLOAD"

	return method + "\n" +
		canonicalURI + "\n" +
		canonicalQueryString + "\n" +
		canonicalHeaders.String() + "\n" +
		signedHeadersStr + "\n" +
		payloadHash
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
