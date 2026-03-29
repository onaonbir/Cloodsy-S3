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

// ParseAuthHeader parses the Authorization header and returns the access key and other SigV4 components.
type SigV4Auth struct {
	AccessKey     string
	SignedHeaders []string
	Signature     string
	Region        string
	Service       string
	Date          string
	Credential    string
}

func ParseAuthorizationHeader(header string) (*SigV4Auth, error) {
	// AWS4-HMAC-SHA256 Credential=AKID/20230101/us-east-1/s3/aws4_request, SignedHeaders=host;x-amz-content-sha256;x-amz-date, Signature=abcdef
	if !strings.HasPrefix(header, "AWS4-HMAC-SHA256 ") {
		return nil, fmt.Errorf("unsupported auth scheme")
	}

	parts := header[len("AWS4-HMAC-SHA256 "):]
	auth := &SigV4Auth{}

	for _, part := range strings.Split(parts, ", ") {
		kv := strings.SplitN(part, "=", 2)
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
		return nil, fmt.Errorf("invalid service in credential: expected s3")
	}

	if len(auth.SignedHeaders) == 0 {
		return nil, fmt.Errorf("missing signed headers")
	}

	if auth.Signature == "" {
		return nil, fmt.Errorf("missing signature")
	}

	hasHost := false
	for _, h := range auth.SignedHeaders {
		if strings.EqualFold(h, "host") {
			hasHost = true
			break
		}
	}
	if !hasHost {
		return nil, fmt.Errorf("missing required signed header: host")
	}

	return auth, nil
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

	amzDate := q.Get("X-Amz-Date")
	if amzDate == "" {
		return nil, fmt.Errorf("missing X-Amz-Date")
	}

	expiresStr := q.Get("X-Amz-Expires")
	if expiresStr == "" {
		return nil, fmt.Errorf("missing X-Amz-Expires")
	}

	expires, err := strconv.Atoi(expiresStr)
	if err != nil || expires < 1 || expires > 604800 {
		return nil, fmt.Errorf("invalid X-Amz-Expires: must be 1-604800 seconds")
	}

	// Check if expired
	reqTime, err := time.Parse("20060102T150405Z", amzDate)
	if err != nil {
		return nil, fmt.Errorf("invalid X-Amz-Date format")
	}
	if time.Since(reqTime) > time.Duration(expires)*time.Second {
		return nil, fmt.Errorf("presigned URL has expired")
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
		return nil, fmt.Errorf("invalid service in credential: expected s3")
	}

	// Validate "host" is in signed headers
	hasHost := false
	for _, h := range auth.SignedHeaders {
		if strings.EqualFold(h, "host") {
			hasHost = true
			break
		}
	}
	if !hasHost {
		return nil, fmt.Errorf("missing required signed header: host")
	}

	return auth, nil
}
