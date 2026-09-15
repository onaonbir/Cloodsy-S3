package handler

import (
	"context"
	"crypto/md5"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"hash"
	"hash/crc32"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/onaonbir/Cloodsy-S3/auth"
	"github.com/onaonbir/Cloodsy-S3/config"
	"github.com/onaonbir/Cloodsy-S3/db"
	imageutil "github.com/onaonbir/Cloodsy-S3/image"
	"github.com/onaonbir/Cloodsy-S3/s3err"
	"github.com/onaonbir/Cloodsy-S3/service"
	"github.com/onaonbir/Cloodsy-S3/storage"
	"github.com/onaonbir/Cloodsy-S3/webhook"
)

// Limits
const (
	maxObjectSize     int64 = 5 * 1024 * 1024 * 1024        // 5 GB per PutObject
	maxPartSize       int64 = 5 * 1024 * 1024 * 1024        // 5 GB per part
	minPartSize       int64 = 5 * 1024 * 1024               // 5 MB for every part except the last
	maxMultipartSize  int64 = 5 * 1024 * 1024 * 1024 * 1024 // 5 TB max assembled multipart
	maxXMLBodySize    int64 = 1 * 1024 * 1024               // 1 MB for XML request bodies
	maxDeleteObjects        = 1000                          // S3 limit for batch delete
	maxMetadataSize         = 2048                          // 2 KB total metadata per S3 spec
	maxMetadataKeyLen       = 128                           // Max metadata key length
	maxMetadataValLen       = 256                           // Max metadata value length
	maxChunkSize      int64 = 5 * 1024 * 1024 * 1024        // Max chunk size in aws-chunked
	maxParts                = 10000                         // S3 max parts per multipart upload
	maxKeyLength            = 1024
)

// validBucketName matches S3 bucket naming rules: 3-63 chars, lowercase alphanumeric + hyphens, no leading/trailing hyphens.
var validBucketName = regexp.MustCompile(`^[a-z0-9][a-z0-9\-]{1,61}[a-z0-9]$`)

// Handler holds shared dependencies for all S3 handlers.
type Handler struct {
	DB          *db.DB
	Storage     storage.Backend
	Config      *config.Config
	Logger      *slog.Logger
	Dispatcher  *webhook.Dispatcher
	ImageWorker *imageutil.Worker // optional; set by server.Run when image.enabled
	Objects     *service.Objects
	Transforms  *imageutil.Limiter // bounds concurrent on-access transforms
}

func New(database *db.DB, store storage.Backend, cfg *config.Config, logger *slog.Logger) *Handler {
	svc := service.New(database, store, logger)
	svc.ImageCfg = cfg.Image
	return &Handler{
		DB:         database,
		Storage:    store,
		Config:     cfg,
		Logger:     logger,
		Objects:    svc,
		Transforms: imageutil.NewLimiter(cfg.Image.MaxConcurrent),
	}
}

// SetDispatcher wires the webhook dispatcher into the handler and service.
func (h *Handler) SetDispatcher(d *webhook.Dispatcher) {
	h.Dispatcher = d
	h.Objects.Dispatcher = d
}

// SetImageWorker wires the image optimizer into the handler and service.
func (h *Handler) SetImageWorker(w *imageutil.Worker) {
	h.ImageWorker = w
	h.Objects.Image = w
}

// writeXML writes an XML response with the given status code.
func (h *Handler) writeXML(w http.ResponseWriter, statusCode int, v interface{}) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(statusCode)
	w.Write([]byte(xml.Header))
	xml.NewEncoder(w).Encode(v)
}

// --- request context -------------------------------------------------------

type ctxKey int

const (
	ctxKeyAuth ctxKey = iota
	ctxKeyBucketKey
)

type bucketKey struct{ bucket, key string }

// SetBucketAndKey is called by the router once it has resolved the bucket and
// key (path-style or virtual-hosted-style) so handlers share one parser.
func SetBucketAndKey(r *http.Request, bucket, key string) {
	*r = *r.WithContext(context.WithValue(r.Context(), ctxKeyBucketKey, bucketKey{bucket, key}))
}

// getBucketAndKey extracts bucket name and object key from the request.
func getBucketAndKey(r *http.Request) (string, string) {
	if bk, ok := r.Context().Value(ctxKeyBucketKey).(bucketKey); ok {
		return bk.bucket, bk.key
	}
	path := strings.TrimPrefix(r.URL.Path, "/")
	idx := strings.IndexByte(path, '/')
	if idx < 0 {
		return path, ""
	}
	return path[:idx], path[idx+1:]
}

func setAuth(r *http.Request, a *auth.SigV4Auth) {
	*r = *r.WithContext(context.WithValue(r.Context(), ctxKeyAuth, a))
}

func authFromRequest(r *http.Request) *auth.SigV4Auth {
	a, _ := r.Context().Value(ctxKeyAuth).(*auth.SigV4Auth)
	return a
}

// --- validation ------------------------------------------------------------

// isValidBucketName checks if a bucket name conforms to S3 naming rules.
func isValidBucketName(name string) bool {
	if len(name) < 3 || len(name) > 63 {
		return false
	}
	return validBucketName.MatchString(name)
}

// isValidObjectKey checks an object key: S3 allows any UTF-8 up to 1024
// bytes. We additionally reject NUL and "." / ".." path segments (which no
// sane client produces and which would be ambiguous on disk).
func isValidObjectKey(key string) bool {
	if key == "" || len(key) > maxKeyLength {
		return false
	}
	if strings.IndexByte(key, 0) >= 0 {
		return false
	}
	for _, seg := range strings.Split(key, "/") {
		if seg == "." || seg == ".." {
			return false
		}
	}
	return true
}

// limitedXMLDecode decodes XML from a size-limited reader to prevent resource exhaustion.
func limitedXMLDecode(r io.Reader, v interface{}) error {
	limited := io.LimitReader(r, maxXMLBodySize)
	dec := xml.NewDecoder(limited)
	dec.Strict = true
	return dec.Decode(v)
}

// --- authentication --------------------------------------------------------

// authenticateRequest verifies the request (header auth or presigned URL) and returns the credential.
func (h *Handler) authenticateRequest(w http.ResponseWriter, r *http.Request) (*db.BucketCredential, bool) {
	if r.URL.Query().Get("X-Amz-Algorithm") != "" {
		return h.authenticatePresigned(w, r)
	}

	authHeader := r.Header.Get("Authorization")
	if authHeader == "" {
		s3err.WriteError(w, r, s3err.ErrAccessDenied)
		return nil, false
	}

	parsed, err := auth.ParseAuthorizationHeader(authHeader)
	if err != nil {
		s3err.WriteErrorMsg(w, r, s3err.ErrAuthorizationHeaderMalformed, err.Error())
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

	if err := auth.VerifySignature(r, cred.SecretKey, h.Config.Server.Region, parsed); err != nil {
		h.Logger.Debug("signature verification failed", "error", err, "accessKey", maskKey(parsed.AccessKey))
		h.writeAuthError(w, r, err)
		return nil, false
	}

	if h.Config.Server.RequirePayloadSignature && r.Header.Get("X-Amz-Content-Sha256") == "UNSIGNED-PAYLOAD" {
		s3err.WriteErrorMsg(w, r, s3err.ErrInvalidRequest, "UNSIGNED-PAYLOAD is not allowed on this server.")
		return nil, false
	}

	setAuth(r, parsed)
	return cred, true
}

func maskKey(k string) string {
	if len(k) > 6 {
		return k[:6] + "***"
	}
	return k
}

func (h *Handler) writeAuthError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, auth.ErrTimeSkewed):
		s3err.WriteError(w, r, s3err.ErrRequestTimeTooSkewed)
	case errors.Is(err, auth.ErrRegionMismatch):
		s3err.WriteRegionError(w, r, h.Config.Server.Region)
	case errors.Is(err, auth.ErrMissingDate), errors.Is(err, auth.ErrInvalidDate), errors.Is(err, auth.ErrMissingContentSHA):
		s3err.WriteErrorMsg(w, r, s3err.ErrMissingSecurityHeader, err.Error())
	case errors.Is(err, auth.ErrPresignedExpired), errors.Is(err, auth.ErrPresignedNotYet):
		s3err.WriteErrorMsg(w, r, s3err.ErrAccessDenied, "Request has expired")
	case errors.Is(err, auth.ErrDateMismatch), errors.Is(err, auth.ErrServiceMismatch):
		s3err.WriteErrorMsg(w, r, s3err.ErrAuthorizationHeaderMalformed, err.Error())
	default:
		s3err.WriteError(w, r, s3err.ErrSignatureDoesNotMatch)
	}
}

// authenticatePresigned handles presigned URL authentication (query string parameters).
func (h *Handler) authenticatePresigned(w http.ResponseWriter, r *http.Request) (*db.BucketCredential, bool) {
	q := r.URL.Query()

	parsed, err := auth.ParsePresignedQuery(q)
	if err != nil {
		h.Logger.Debug("presigned parse failed", "error", err)
		h.writeAuthError(w, r, err)
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
		h.writeAuthError(w, r, err)
		return nil, false
	}

	setAuth(r, parsed)
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

// authenticateOrPublic gates object reads (GET/HEAD) allowing anonymous access
// when the target bucket is flagged public-read. It returns the resolved bucket
// plus the credential (nil when anonymous). Signed access is unchanged: if any
// auth material is present it is validated normally and ownership is enforced.
//
// Anonymous access is intentionally scoped to single-object reads only — it is
// never wired into listings or writes, so a public bucket exposes object GETs
// but not enumeration.
func (h *Handler) authenticateOrPublic(w http.ResponseWriter, r *http.Request, bucketName string) (*db.Bucket, *db.BucketCredential, bool) {
	hasAuth := r.Header.Get("Authorization") != "" || r.URL.Query().Get("X-Amz-Algorithm") != ""

	if hasAuth {
		cred, ok := h.authenticateRequest(w, r)
		if !ok {
			return nil, nil, false
		}
		bucket, ok := h.checkBucketAccess(w, r, cred, bucketName)
		if !ok {
			return nil, nil, false
		}
		return bucket, cred, true
	}

	bucket, err := h.DB.GetBucket(bucketName)
	if err != nil {
		h.Logger.Error("db error looking up bucket", "error", err)
		s3err.WriteError(w, r, s3err.ErrInternalError)
		return nil, nil, false
	}
	if bucket == nil {
		s3err.WriteError(w, r, s3err.ErrNoSuchBucket)
		return nil, nil, false
	}
	if !bucket.PublicRead {
		s3err.WriteError(w, r, s3err.ErrAccessDenied)
		return nil, nil, false
	}
	return bucket, nil, true
}

// checkWriteAccess returns false and writes AccessDenied if the credential is read-only.
func (h *Handler) checkWriteAccess(w http.ResponseWriter, r *http.Request, cred *db.BucketCredential) bool {
	if cred.Permission == "read-only" {
		s3err.WriteError(w, r, s3err.ErrAccessDenied)
		return false
	}
	return true
}

// writeServiceError maps service/storage/decoder errors to S3 error responses.
func (h *Handler) writeServiceError(w http.ResponseWriter, r *http.Request, err error, logMsg string) {
	var s3e s3err.S3Error
	switch {
	case errors.As(err, &s3e):
		s3err.WriteError(w, r, s3e)
	case errors.Is(err, storage.ErrTooLarge):
		s3err.WriteError(w, r, s3err.ErrEntityTooLarge)
	case errors.Is(err, service.ErrQuotaExceeded):
		s3err.WriteError(w, r, s3err.ErrQuotaExceeded)
	case errors.Is(err, service.ErrNoSuchKey):
		s3err.WriteError(w, r, s3err.ErrNoSuchKey)
	case errors.Is(err, service.ErrNoSuchVersion):
		s3err.WriteError(w, r, s3err.ErrNoSuchVersion)
	case errors.Is(err, errIncompleteChunked):
		s3err.WriteError(w, r, s3err.ErrIncompleteBody)
	case errors.Is(err, errMalformedChunk), errors.Is(err, errChunkTooLarge):
		s3err.WriteErrorMsg(w, r, s3err.ErrInvalidRequest, "Malformed aws-chunked body.")
	case errors.Is(err, errChunkSignature):
		s3err.WriteError(w, r, s3err.ErrSignatureDoesNotMatch)
	case errors.Is(err, errPayloadSHA256):
		s3err.WriteError(w, r, s3err.ErrXAmzContentSHA256Mismatch)
	case errors.Is(err, errBadDigest):
		s3err.WriteError(w, r, s3err.ErrBadDigest)
	case errors.Is(err, errInvalidDigest):
		s3err.WriteError(w, r, s3err.ErrInvalidDigest)
	case errors.Is(err, errIncompleteBody):
		s3err.WriteError(w, r, s3err.ErrIncompleteBody)
	default:
		h.Logger.Error(logMsg, "error", err)
		s3err.WriteError(w, r, s3err.ErrInternalError)
	}
}

// --- request bodies & integrity --------------------------------------------

var (
	errPayloadSHA256  = errors.New("x-amz-content-sha256 mismatch")
	errBadDigest      = errors.New("checksum mismatch")
	errInvalidDigest  = errors.New("invalid checksum header")
	errIncompleteBody = errors.New("body shorter than declared length")
)

// payloadChecker streams the body through the hashes the client declared and
// compares them once the bytes are on disk (before they become visible).
type payloadChecker struct {
	chunked      *awsChunkedReader
	sha256       hash.Hash
	expectedSHA  string
	expectedMD5  []byte
	checksumAlgo string
	checksumHash hash.Hash
	expectedSum  string // base64, from header; trailers are read after the body
	declared     int64  // -1 unknown
}

func (pc *payloadChecker) wrap(r io.Reader) io.Reader {
	var writers []io.Writer
	if pc.sha256 != nil {
		writers = append(writers, pc.sha256)
	}
	if pc.checksumHash != nil {
		writers = append(writers, pc.checksumHash)
	}
	if len(writers) == 0 {
		return r
	}
	return io.TeeReader(r, io.MultiWriter(writers...))
}

// verify is used as storage.PutOptions.Verify.
func (pc *payloadChecker) verify(size int64, md5sum []byte) error {
	if pc.chunked != nil && !pc.chunked.Complete() {
		return errIncompleteChunked
	}
	if pc.declared >= 0 && size != pc.declared {
		return errIncompleteBody
	}
	if pc.sha256 != nil {
		if hex.EncodeToString(pc.sha256.Sum(nil)) != strings.ToLower(pc.expectedSHA) {
			return errPayloadSHA256
		}
	}
	if pc.expectedMD5 != nil {
		if !bytesEqual(pc.expectedMD5, md5sum) {
			return errBadDigest
		}
	}
	if pc.checksumHash != nil {
		expected := pc.expectedSum
		if expected == "" && pc.chunked != nil {
			expected = pc.chunked.Trailers["x-amz-checksum-"+pc.checksumAlgo]
		}
		if expected != "" {
			want, err := base64.StdEncoding.DecodeString(expected)
			if err != nil {
				return errInvalidDigest
			}
			if !bytesEqual(want, pc.checksumHash.Sum(nil)) {
				return errBadDigest
			}
		}
	}
	return nil
}

func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// requestBody returns the (decoded) request body, a checker whose verify
// method must run before the write is committed, and the declared size
// (-1 if unknown). Chunked bodies are signature-verified when the request was
// header-signed.
func (h *Handler) requestBody(r *http.Request, cred *db.BucketCredential) (io.Reader, *payloadChecker, error) {
	pc := &payloadChecker{declared: -1}
	contentSha := r.Header.Get("X-Amz-Content-Sha256")
	contentEnc := r.Header.Get("Content-Encoding")

	var body io.Reader = r.Body
	if strings.HasPrefix(contentSha, "STREAMING-") || strings.Contains(contentEnc, "aws-chunked") {
		var signer *chunkSigner
		if strings.HasPrefix(contentSha, "STREAMING-AWS4-HMAC-SHA256") && cred != nil {
			signer = newChunkSigner(cred.SecretKey, authFromRequest(r))
			if signer == nil {
				return nil, nil, errChunkSignature
			}
		}
		pc.chunked = newAWSChunkedReader(r.Body, signer)
		body = pc.chunked
		if v := r.Header.Get("X-Amz-Decoded-Content-Length"); v != "" {
			if n, err := strconv.ParseInt(v, 10, 64); err == nil && n >= 0 {
				pc.declared = n
			}
		}
	} else {
		if r.ContentLength >= 0 {
			pc.declared = r.ContentLength
		}
		if len(contentSha) == 64 && isHex(contentSha) {
			pc.sha256 = sha256.New()
			pc.expectedSHA = contentSha
		}
	}

	if cm := r.Header.Get("Content-MD5"); cm != "" {
		sum, err := base64.StdEncoding.DecodeString(cm)
		if err != nil || len(sum) != md5.Size {
			return nil, nil, errInvalidDigest
		}
		pc.expectedMD5 = sum
	}

	// Flexible checksums: header form or trailer form (x-amz-trailer names the header).
	algo := ""
	for _, a := range []string{"crc32c", "crc32", "sha1", "sha256"} {
		if v := r.Header.Get("x-amz-checksum-" + a); v != "" {
			algo, pc.expectedSum = a, v
			break
		}
	}
	if algo == "" {
		if t := strings.ToLower(strings.TrimSpace(r.Header.Get("x-amz-trailer"))); strings.HasPrefix(t, "x-amz-checksum-") {
			algo = strings.TrimPrefix(t, "x-amz-checksum-")
		}
	}
	switch algo {
	case "crc32":
		pc.checksumHash = crc32.NewIEEE()
	case "crc32c":
		pc.checksumHash = crc32.New(crc32.MakeTable(crc32.Castagnoli))
	case "sha1":
		pc.checksumHash = sha1.New()
	case "sha256":
		pc.checksumHash = sha256.New()
	}
	pc.checksumAlgo = algo

	return pc.wrap(body), pc, nil
}

func isHex(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return false
		}
	}
	return true
}

// --- metadata --------------------------------------------------------------

// systemHeaders are persisted alongside user metadata and echoed on GET/HEAD.
var systemHeaders = map[string]string{
	"Cache-Control":       service.MetaCacheControl,
	"Content-Disposition": service.MetaContentDisposition,
	"Content-Encoding":    service.MetaContentEncoding,
	"Content-Language":    service.MetaContentLanguage,
	"Expires":             service.MetaExpires,
}

// collectMetadata gathers x-amz-meta-* headers and the persisted system
// headers into a JSON string. Returns an error when S3 limits are exceeded.
func collectMetadata(r *http.Request) (string, error) {
	meta := make(map[string]string)
	totalSize := 0
	for key, vals := range r.Header {
		lower := strings.ToLower(key)
		if !strings.HasPrefix(lower, "x-amz-meta-") {
			continue
		}
		metaKey := strings.TrimPrefix(lower, "x-amz-meta-")
		val := strings.Join(vals, ",")
		if len(metaKey) > maxMetadataKeyLen || len(val) > maxMetadataValLen {
			return "", s3err.ErrMetadataTooLarge
		}
		totalSize += len(metaKey) + len(val)
		if totalSize > maxMetadataSize {
			return "", s3err.ErrMetadataTooLarge
		}
		if strings.ContainsAny(val, "\r\n") {
			return "", s3err.ErrInvalidArgument
		}
		meta[metaKey] = val
	}
	for hdr, slot := range systemHeaders {
		if v := r.Header.Get(hdr); v != "" && !strings.ContainsAny(v, "\r\n") {
			if hdr == "Content-Encoding" && strings.Contains(v, "aws-chunked") {
				// Strip the transport encoding, keep any real one (e.g. gzip).
				parts := []string{}
				for _, p := range strings.Split(v, ",") {
					if p = strings.TrimSpace(p); p != "" && p != "aws-chunked" {
						parts = append(parts, p)
					}
				}
				v = strings.Join(parts, ",")
				if v == "" {
					continue
				}
			}
			meta[slot] = v
		}
	}
	if len(meta) == 0 {
		return "{}", nil
	}
	data, err := json.Marshal(meta)
	if err != nil {
		return "{}", nil
	}
	return string(data), nil
}

// parseMetadata decodes the stored JSON.
func parseMetadata(metadata string) map[string]string {
	if metadata == "" || metadata == "{}" {
		return nil
	}
	var meta map[string]string
	if err := json.Unmarshal([]byte(metadata), &meta); err != nil {
		return nil
	}
	return meta
}

// setMetadataHeaders sets x-amz-meta-* and persisted system headers.
func setMetadataHeaders(w http.ResponseWriter, metadata string) {
	meta := parseMetadata(metadata)
	if meta == nil {
		return
	}
	for key, val := range meta {
		val = strings.NewReplacer("\r", "", "\n", "").Replace(val)
		if service.IsSystemMetaKey(key) {
			for hdr, slot := range systemHeaders {
				if slot == key {
					w.Header().Set(hdr, val)
				}
			}
			continue
		}
		w.Header().Set("X-Amz-Meta-"+key, val)
	}
}

// setExpirationHeader sets the x-amz-expiration header if a lifecycle rule matches.
func (h *Handler) setExpirationHeader(w http.ResponseWriter, bucketName, key string, lastModified time.Time) {
	rule, err := h.DB.GetMatchingLifecycleRule(bucketName, key)
	if err != nil || rule == nil {
		return
	}
	expiryDate := lastModified.AddDate(0, 0, rule.ExpirationDays).UTC()
	id := rule.Name
	if id == "" {
		id = fmt.Sprintf("rule-%d", rule.ID)
	}
	w.Header().Set("x-amz-expiration", fmt.Sprintf(`expiry-date="%s", rule-id="%s"`, expiryDate.Format(http.TimeFormat), id))
}

// apiVersionID maps the internal empty version id to S3's "null".
func apiVersionID(v string) string {
	if v == "" {
		return "null"
	}
	return v
}

// parseMaxKeys parses max-keys style params (0 allowed, capped at 1000).
func parseMaxKeys(v string) (int, bool) {
	if v == "" {
		return 1000, true
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return 0, false
	}
	if n > 1000 {
		n = 1000
	}
	return n, true
}
