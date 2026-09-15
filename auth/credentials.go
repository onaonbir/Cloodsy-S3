package auth

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"math/big"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// GenerateAccessKey generates a 20-character access key like AWS.
func GenerateAccessKey() (string, error) {
	b := make([]byte, 10)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate access key: %w", err)
	}
	key := "AK" + strings.ToUpper(hex.EncodeToString(b))
	return key[:20], nil
}

// GenerateSecretKey generates a 40-character secret key like AWS.
// Uses rejection sampling via crypto/rand to avoid modulo bias.
func GenerateSecretKey() (string, error) {
	const charset = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
	charsetLen := big.NewInt(int64(len(charset)))
	key := make([]byte, 40)
	for i := range key {
		idx, err := rand.Int(rand.Reader, charsetLen)
		if err != nil {
			return "", fmt.Errorf("generate secret key: %w", err)
		}
		key[i] = charset[idx.Int64()]
	}
	return string(key), nil
}

// SigV4Auth holds the parsed components of a SigV4 Authorization header or
// presigned query string. AmzDate and Scope are filled in after successful
// verification so streaming (aws-chunked) bodies can be verified too.
type SigV4Auth struct {
	AccessKey     string
	SignedHeaders []string
	Signature     string
	Region        string
	Service       string
	Date          string
	Credential    string
	AmzDate       string
	Scope         string
}

const (
	sigV4TimeFormat            = "20060102T150405Z"
	maxPresignedExpiresSeconds = 604800
)

func validatePresignedExpiry(q url.Values) (time.Time, error) {
	amzDate := q.Get("X-Amz-Date")
	if amzDate == "" {
		return time.Time{}, ErrMissingDate
	}

	expiresStr := q.Get("X-Amz-Expires")
	if expiresStr == "" {
		return time.Time{}, fmt.Errorf("missing X-Amz-Expires")
	}

	expires, err := strconv.Atoi(expiresStr)
	if err != nil || expires < 1 || expires > maxPresignedExpiresSeconds {
		return time.Time{}, fmt.Errorf("invalid X-Amz-Expires: must be 1-%d seconds", maxPresignedExpiresSeconds)
	}

	reqTime, err := time.Parse(sigV4TimeFormat, amzDate)
	if err != nil {
		return time.Time{}, ErrInvalidDate
	}
	now := time.Now()
	if reqTime.After(now.Add(MaxClockSkew)) {
		return time.Time{}, ErrPresignedNotYet
	}
	if now.Sub(reqTime) > time.Duration(expires)*time.Second {
		return time.Time{}, ErrPresignedExpired
	}

	return reqTime, nil
}

func ParseAuthorizationHeader(header string) (*SigV4Auth, error) {
	// AWS4-HMAC-SHA256 Credential=AKID/20230101/us-east-1/s3/aws4_request, SignedHeaders=host;x-amz-content-sha256;x-amz-date, Signature=abcdef
	if !strings.HasPrefix(header, "AWS4-HMAC-SHA256 ") {
		return nil, fmt.Errorf("unsupported auth scheme")
	}

	parts := header[len("AWS4-HMAC-SHA256 "):]
	auth := &SigV4Auth{}

	// Components are comma separated; whitespace around commas is optional.
	for _, part := range strings.Split(parts, ",") {
		kv := strings.SplitN(strings.TrimSpace(part), "=", 2)
		if len(kv) != 2 {
			continue
		}
		key := strings.TrimSpace(kv[0])
		val := strings.TrimSpace(kv[1])

		switch key {
		case "Credential":
			auth.Credential = val
			credParts := strings.Split(val, "/")
			if len(credParts) >= 5 {
				auth.AccessKey = credParts[0]
				auth.Date = credParts[1]
				auth.Region = credParts[2]
				auth.Service = credParts[3]
			}
		case "SignedHeaders":
			auth.SignedHeaders = strings.Split(val, ";")
		case "Signature":
			auth.Signature = val
		}
	}

	if auth.AccessKey == "" || auth.Date == "" || auth.Region == "" || auth.Service == "" {
		return nil, fmt.Errorf("incomplete credential scope")
	}
	if auth.Service != "s3" {
		return nil, ErrServiceMismatch
	}
	if len(auth.SignedHeaders) == 0 {
		return nil, fmt.Errorf("missing signed headers")
	}
	if auth.Signature == "" {
		return nil, fmt.Errorf("missing signature")
	}
	if !hasHost(auth.SignedHeaders) {
		return nil, fmt.Errorf("missing required signed header: host")
	}
	return auth, nil
}

func hasHost(signed []string) bool {
	for _, h := range signed {
		if strings.EqualFold(h, "host") {
			return true
		}
	}
	return false
}

// ParsePresignedQuery parses presigned URL query parameters into a SigV4Auth struct.
func ParsePresignedQuery(q url.Values) (*SigV4Auth, error) {
	algorithm := q.Get("X-Amz-Algorithm")
	if algorithm != "AWS4-HMAC-SHA256" {
		return nil, fmt.Errorf("unsupported algorithm: %s", algorithm)
	}

	credential := q.Get("X-Amz-Credential")
	if credential == "" {
		return nil, fmt.Errorf("missing X-Amz-Credential")
	}
	signature := q.Get("X-Amz-Signature")
	if signature == "" {
		return nil, fmt.Errorf("missing X-Amz-Signature")
	}
	signedHeaders := q.Get("X-Amz-SignedHeaders")
	if signedHeaders == "" {
		return nil, fmt.Errorf("missing X-Amz-SignedHeaders")
	}
	if _, err := validatePresignedExpiry(q); err != nil {
		return nil, err
	}

	credParts := strings.Split(credential, "/")
	if len(credParts) < 5 {
		return nil, fmt.Errorf("invalid credential format")
	}

	auth := &SigV4Auth{
		AccessKey:     credParts[0],
		Date:          credParts[1],
		Region:        credParts[2],
		Service:       credParts[3],
		Credential:    credential,
		SignedHeaders: strings.Split(signedHeaders, ";"),
		Signature:     signature,
	}
	if auth.Service != "s3" {
		return nil, ErrServiceMismatch
	}
	if !hasHost(auth.SignedHeaders) {
		return nil, fmt.Errorf("missing required signed header: host")
	}
	return auth, nil
}
