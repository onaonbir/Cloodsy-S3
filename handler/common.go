package handler

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/onaonbir/Cloodsy-S3/auth"
	"github.com/onaonbir/Cloodsy-S3/config"
	"github.com/onaonbir/Cloodsy-S3/db"
	"github.com/onaonbir/Cloodsy-S3/s3err"
	"github.com/onaonbir/Cloodsy-S3/storage"
	"github.com/onaonbir/Cloodsy-S3/webhook"
)

// Limits
const (
	maxObjectSize    int64 = 5 * 1024 * 1024 * 1024 // 5 GB per PutObject
	maxPartSize      int64 = 5 * 1024 * 1024 * 1024 // 5 GB per part
	maxMultipartSize int64 = 5 * 1024 * 1024 * 1024 * 1024 // 5 TB max assembled multipart
	maxXMLBodySize   int64 = 1 * 1024 * 1024        // 1 MB for XML request bodies
	maxDeleteObjects       = 1000                    // S3 limit for batch delete
	maxMetadataSize        = 2048                    // 2 KB total metadata per S3 spec
	maxMetadataKeyLen      = 128                     // Max metadata key length
	maxMetadataValLen      = 256                     // Max metadata value length
	maxChunkSize     int64 = 5 * 1024 * 1024 * 1024 // Max chunk size in aws-chunked
	maxParts               = 10000                   // S3 max parts per multipart upload
)

// validBucketName matches S3 bucket naming rules: 3-63 chars, lowercase alphanumeric + hyphens, no leading/trailing hyphens.
var validBucketName = regexp.MustCompile(`^[a-z0-9][a-z0-9\-]{1,61}[a-z0-9]$`)

// Handler holds shared dependencies for all S3 handlers.
type Handler struct {
	DB         *db.DB
	Storage    storage.Backend
	Config     *config.Config
	Logger     *slog.Logger
	Dispatcher *webhook.Dispatcher
}

func New(database *db.DB, store storage.Backend, cfg *config.Config, logger *slog.Logger) *Handler {
	return &Handler{
		DB:      database,
		Storage: store,
		Config:  cfg,
		Logger:  logger,
	}
}

// writeXML writes an XML response with the given status code.
func (h *Handler) writeXML(w http.ResponseWriter, statusCode int, v interface{}) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(statusCode)
	w.Write([]byte(xml.Header))
	xml.NewEncoder(w).Encode(v)
}

// getBucketAndKey extracts bucket name and object key from the request path.
func getBucketAndKey(r *http.Request) (string, string) {
	path := strings.TrimPrefix(r.URL.Path, "/")
	idx := strings.IndexByte(path, '/')
	if idx < 0 {
		return path, ""
	}
	return path[:idx], path[idx+1:]
}

// isValidBucketName checks if a bucket name conforms to S3 naming rules.
func isValidBucketName(name string) bool {
	if len(name) < 3 || len(name) > 63 {
		return false
	}
	return validBucketName.MatchString(name)
}

// isValidObjectKey checks if an object key is valid (no path traversal, reasonable length).
func isValidObjectKey(key string) bool {
	if key == "" || len(key) > 1024 {
		return false
	}
	// Reject path traversal attempts
	if strings.Contains(key, "..") {
		return false
	}
	// Reject null bytes
	if strings.ContainsRune(key, 0) {
		return false
	}
	return true
}

// limitedXMLDecode decodes XML from a size-limited reader to prevent resource exhaustion.
func limitedXMLDecode(r io.Reader, v interface{}) error {
	limited := io.LimitReader(r, maxXMLBodySize)
	return xml.NewDecoder(limited).Decode(v)
}

// authenticateRequest verifies the request (header auth or presigned URL) and returns the credential.
func (h *Handler) authenticateRequest(w http.ResponseWriter, r *http.Request) (*db.BucketCredential, bool) {
	// Check for presigned URL (query string auth)
	if r.URL.Query().Get("X-Amz-Algorithm") != "" {
		return h.authenticatePresigned(w, r)
	}

	authHeader := r.Header.Get("Authorization")
	if authHeader == "" {
		s3err.WriteError(w, r, s3err.ErrMissingSecurityHeader)
		return nil, false
	}

	parsed, err := auth.ParseAuthorizationHeader(authHeader)
	if err != nil {
		s3err.WriteError(w, r, s3err.ErrMissingSecurityHeader)
		return nil, false
	}

	cred, err := h.DB.GetCredentialByAccessKey(parsed.AccessKey)
	if err != nil {
		h.Logger.Error("db error looking up credential", "error", err)
		s3err.WriteError(w, r, s3err.ErrInternalError)
		return nil, false
	}
	if cred == nil {
		s3err.WriteError(w, r, s3err.ErrInvalidAccessKeyId)
		return nil, false
	}

	// Verify signature
	if err := auth.VerifySignature(r, cred.SecretKey, h.Config.Server.Region); err != nil {
		maskedKey := parsed.AccessKey
	if len(maskedKey) > 6 {
		maskedKey = maskedKey[:6] + "***"
	}
	h.Logger.Debug("signature verification failed", "error", err, "accessKey", maskedKey)
		s3err.WriteError(w, r, s3err.ErrSignatureDoesNotMatch)
		return nil, false
	}

	return cred, true
}

// authenticatePresigned handles presigned URL authentication (query string parameters).
func (h *Handler) authenticatePresigned(w http.ResponseWriter, r *http.Request) (*db.BucketCredential, bool) {
	q := r.URL.Query()

	parsed, err := auth.ParsePresignedQuery(q)
	if err != nil {
		h.Logger.Debug("presigned parse failed", "error", err)
		s3err.WriteError(w, r, s3err.ErrMissingSecurityHeader)
		return nil, false
	}

	cred, err := h.DB.GetCredentialByAccessKey(parsed.AccessKey)
	if err != nil {
		h.Logger.Error("db error looking up credential", "error", err)
		s3err.WriteError(w, r, s3err.ErrInternalError)
		return nil, false
	}
	if cred == nil {
		s3err.WriteError(w, r, s3err.ErrInvalidAccessKeyId)
		return nil, false
	}

	if err := auth.VerifyPresignedSignature(r, cred.SecretKey, h.Config.Server.Region, parsed); err != nil {
		h.Logger.Debug("presigned signature verification failed", "error", err)
		s3err.WriteError(w, r, s3err.ErrSignatureDoesNotMatch)
		return nil, false
	}

	return cred, true
}

// checkBucketAccess verifies the credential has access to the specified bucket.
func (h *Handler) checkBucketAccess(w http.ResponseWriter, r *http.Request, cred *db.BucketCredential, bucketName string) (*db.Bucket, bool) {
	bucket, err := h.DB.GetBucket(bucketName)
	if err != nil {
		h.Logger.Error("db error looking up bucket", "error", err)
		s3err.WriteError(w, r, s3err.ErrInternalError)
		return nil, false
	}
	if bucket == nil {
		s3err.WriteError(w, r, s3err.ErrNoSuchBucket)
		return nil, false
	}

	if cred.BucketID != bucket.ID {
		s3err.WriteError(w, r, s3err.ErrAccessDenied)
		return nil, false
	}

	return bucket, true
}

// checkWriteAccess returns false and writes AccessDenied if the credential is read-only.
func (h *Handler) checkWriteAccess(w http.ResponseWriter, r *http.Request, cred *db.BucketCredential) bool {
	if cred.Permission == "read-only" {
		s3err.WriteError(w, r, s3err.ErrAccessDenied)
		return false
	}
	return true
}

// checkQuota verifies the bucket has enough quota for the given additional size.
// Returns true if within quota, false if quota exceeded.
func (h *Handler) checkQuota(w http.ResponseWriter, r *http.Request, bucket *db.Bucket, additionalSize int64) bool {
	if bucket.QuotaBytes <= 0 {
		return true // no quota set
	}
	usage, err := h.DB.GetBucketUsage(bucket.ID)
	if err != nil {
		h.Logger.Error("failed to get bucket usage", "error", err)
		s3err.WriteError(w, r, s3err.ErrInternalError)
		return false
	}
	if usage+additionalSize > bucket.QuotaBytes {
		s3err.WriteError(w, r, s3err.ErrEntityTooLarge)
		return false
	}
	return true
}

// generateVersionID creates a unique version ID using timestamp + random suffix.
func generateVersionID() string {
	b := make([]byte, 3)
	rand.Read(b)
	return fmt.Sprintf("%d-%s", time.Now().UnixNano(), hex.EncodeToString(b))
}

// setExpirationHeader sets the x-amz-expiration header if a lifecycle rule matches.
func (h *Handler) setExpirationHeader(w http.ResponseWriter, bucketName, key string, lastModified time.Time) {
	rule, err := h.DB.GetMatchingLifecycleRule(bucketName, key)
	if err != nil || rule == nil {
		return
	}
	expiryDate := lastModified.AddDate(0, 0, rule.ExpirationDays).UTC()
	w.Header().Set("x-amz-expiration", fmt.Sprintf(`expiry-date="%s", rule-id="%d"`,
		expiryDate.Format(http.TimeFormat), rule.ID))
}

// getRequestBody returns the request body, automatically decoding
// AWS chunked transfer encoding if present.
func getRequestBody(r *http.Request) io.Reader {
	contentSha := r.Header.Get("X-Amz-Content-Sha256")
	contentEnc := r.Header.Get("Content-Encoding")

	if strings.HasPrefix(contentSha, "STREAMING-") ||
		strings.Contains(contentEnc, "aws-chunked") {
		return newAWSChunkedReader(r.Body)
	}
	return r.Body
}
