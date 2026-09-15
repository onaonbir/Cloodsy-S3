package server

import (
	"log/slog"
	"net"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"github.com/onaonbir/Cloodsy-S3/handler"
	"github.com/onaonbir/Cloodsy-S3/httpx"
	"github.com/onaonbir/Cloodsy-S3/s3err"
)

// maskPresignedQuery replaces sensitive presigned URL params with "***".
func maskPresignedQuery(raw string) string {
	parts := strings.Split(raw, "&")
	for i, p := range parts {
		switch {
		case strings.HasPrefix(p, "X-Amz-Signature="):
			parts[i] = "X-Amz-Signature=***"
		case strings.HasPrefix(p, "X-Amz-Credential="):
			parts[i] = "X-Amz-Credential=***"
		case strings.HasPrefix(p, "X-Amz-Security-Token="):
			parts[i] = "X-Amz-Security-Token=***"
		}
	}
	return strings.Join(parts, "&")
}

// NewRouter creates the S3-compatible HTTP router.
func NewRouter(h *handler.Handler, logger *slog.Logger) http.Handler {
	return &s3Router{handler: h, logger: logger}
}

type s3Router struct {
	handler *handler.Handler
	logger  *slog.Logger
}

// Sub-resources we understand. Anything else on a bucket or object is
// answered with NotImplemented rather than being misrouted to the bare
// bucket/object operation (which previously let `?cors` delete a bucket or
// `?retention` overwrite an object).
var (
	bucketSubresources = map[string]bool{
		"versioning": true, "lifecycle": true, "notification": true, "acl": true, "tagging": true,
		"encryption": true, "policy": true, "cors": true, "location": true, "uploads": true,
		"versions": true, "delete": true, "list-type": true,
	}
	unsupportedBucketSubresources = []string{
		"website", "logging", "replication", "requestPayment", "accelerate", "analytics",
		"inventory", "metrics", "intelligent-tiering", "object-lock", "ownershipControls",
		"publicAccessBlock", "policyStatus", "attributes",
	}
	unsupportedObjectSubresources = []string{"legal-hold", "retention", "restore", "select", "torrent"}
)

func (sr *s3Router) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	requestID := uuid.New().String()
	w.Header().Set("x-amz-request-id", requestID)
	w.Header().Set("x-amz-id-2", requestID)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Server", "Cloodsy-S3")

	// HSTS header (when TLS is enabled)
	if sr.handler.Config.Server.TLS.Enabled {
		w.Header().Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
	}

	// CORS support — only for configured origins
	if origin := r.Header.Get("Origin"); origin != "" {
		allowed := false
		for _, o := range sr.handler.Config.Server.CORSOrigins {
			if o == "*" || strings.EqualFold(o, origin) {
				allowed = true
				break
			}
		}
		if allowed {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Add("Vary", "Origin")
			w.Header().Set("Access-Control-Allow-Methods", "GET, PUT, POST, DELETE, HEAD, OPTIONS")
			reqHeaders := r.Header.Get("Access-Control-Request-Headers")
			if reqHeaders == "" {
				reqHeaders = "Authorization, Content-Type, Content-MD5, Content-Disposition, Cache-Control, Range, If-Match, If-None-Match, If-Modified-Since, If-Unmodified-Since, X-Amz-Content-Sha256, X-Amz-Date, X-Amz-Security-Token, X-Amz-User-Agent, X-Amz-Copy-Source, X-Amz-Copy-Source-Range, X-Amz-Acl, X-Amz-Meta-Filename"
			}
			w.Header().Set("Access-Control-Allow-Headers", reqHeaders)
			w.Header().Set("Access-Control-Expose-Headers", "ETag, Content-Length, Content-Range, Accept-Ranges, Last-Modified, x-amz-request-id, x-amz-version-id, x-amz-delete-marker, x-amz-expiration, x-amz-meta-*")
			w.Header().Set("Access-Control-Max-Age", "86400")
		}
	}
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	// Mask sensitive query params (presigned URL signatures) in logs
	logQuery := r.URL.RawQuery
	if strings.Contains(logQuery, "X-Amz-") {
		logQuery = maskPresignedQuery(logQuery)
	}
	sr.logger.Info(r.Method+" "+httpx.SanitizeLog(r.URL.Path),
		"remote", r.RemoteAddr,
		"query", httpx.SanitizeLog(logQuery),
		"requestId", requestID,
	)

	bucketName, key := sr.resolve(r)
	handler.SetBucketAndKey(r, bucketName, key)
	query := r.URL.Query()

	// Route: GET / → ListBuckets
	if bucketName == "" {
		if r.Method == http.MethodGet {
			sr.handler.ListBuckets(w, r)
			return
		}
		s3err.WriteError(w, r, s3err.ErrMethodNotAllowed)
		return
	}

	// Route: operations on bucket with key
	if key != "" {
		for _, sub := range unsupportedObjectSubresources {
			if query.Has(sub) {
				s3err.WriteError(w, r, s3err.ErrNotImplemented)
				return
			}
		}
		switch r.Method {
		case http.MethodPut:
			switch {
			case query.Has("partNumber") && query.Has("uploadId"):
				if r.Header.Get("X-Amz-Copy-Source") != "" {
					sr.handler.UploadPartCopy(w, r)
				} else {
					sr.handler.UploadPart(w, r)
				}
			case query.Has("uploadId") || query.Has("partNumber"):
				s3err.WriteError(w, r, s3err.ErrInvalidArgument)
			case query.Has("acl"):
				sr.handler.PutObjectAcl(w, r)
			case query.Has("tagging"):
				sr.handler.PutObjectTagging(w, r)
			default:
				sr.handler.PutObject(w, r)
			}
		case http.MethodGet:
			switch {
			case query.Has("acl"):
				sr.handler.GetObjectAcl(w, r)
			case query.Has("tagging"):
				sr.handler.GetObjectTagging(w, r)
			case query.Has("uploadId"):
				sr.handler.ListParts(w, r)
			default:
				sr.handler.GetObject(w, r)
			}
		case http.MethodHead:
			sr.handler.HeadObject(w, r)
		case http.MethodDelete:
			switch {
			case query.Has("uploadId"):
				sr.handler.AbortMultipartUpload(w, r)
			case query.Has("tagging"):
				sr.handler.DeleteObjectTagging(w, r)
			default:
				sr.handler.DeleteObject(w, r)
			}
		case http.MethodPost:
			switch {
			case query.Has("uploadId"):
				sr.handler.CompleteMultipartUpload(w, r)
			case query.Has("uploads"):
				sr.handler.CreateMultipartUpload(w, r)
			default:
				s3err.WriteError(w, r, s3err.ErrMethodNotAllowed)
			}
		default:
			s3err.WriteError(w, r, s3err.ErrMethodNotAllowed)
		}
		return
	}

	// Route: operations on bucket (no key)
	for _, sub := range unsupportedBucketSubresources {
		if query.Has(sub) {
			s3err.WriteError(w, r, s3err.ErrNotImplemented)
			return
		}
	}
	switch r.Method {
	case http.MethodPut:
		switch {
		case query.Has("versioning"):
			sr.handler.PutBucketVersioning(w, r)
		case query.Has("lifecycle"):
			sr.handler.PutBucketLifecycle(w, r)
		case query.Has("notification"):
			sr.handler.PutBucketNotification(w, r)
		case query.Has("acl"):
			sr.handler.PutBucketAcl(w, r)
		case query.Has("tagging"):
			sr.handler.PutBucketTagging(w, r)
		case query.Has("encryption"):
			sr.handler.PutBucketEncryption(w, r)
		case query.Has("policy"):
			sr.handler.PutBucketPolicy(w, r)
		case query.Has("cors"):
			sr.handler.PutBucketCors(w, r)
		case hasAnySubresource(query):
			s3err.WriteError(w, r, s3err.ErrNotImplemented)
		default:
			sr.handler.CreateBucket(w, r)
		}
	case http.MethodDelete:
		switch {
		case query.Has("lifecycle"):
			sr.handler.DeleteBucketLifecycle(w, r)
		case query.Has("notification"):
			sr.handler.DeleteBucketNotification(w, r)
		case query.Has("tagging"):
			sr.handler.DeleteBucketTagging(w, r)
		case query.Has("policy"):
			sr.handler.DeleteBucketPolicy(w, r)
		case query.Has("encryption"):
			sr.handler.DeleteBucketEncryption(w, r)
		case query.Has("cors"):
			sr.handler.DeleteBucketCors(w, r)
		case hasAnySubresource(query):
			s3err.WriteError(w, r, s3err.ErrNotImplemented)
		default:
			sr.handler.DeleteBucket(w, r)
		}
	case http.MethodHead:
		sr.handler.HeadBucket(w, r)
	case http.MethodGet:
		switch {
		case query.Has("location"):
			sr.handler.GetBucketLocation(w, r)
		case query.Has("uploads"):
			sr.handler.ListMultipartUploads(w, r)
		case query.Has("acl"):
			sr.handler.GetBucketAcl(w, r)
		case query.Has("tagging"):
			sr.handler.GetBucketTagging(w, r)
		case query.Has("encryption"):
			sr.handler.GetBucketEncryption(w, r)
		case query.Has("policy"):
			sr.handler.GetBucketPolicy(w, r)
		case query.Has("cors"):
			sr.handler.GetBucketCors(w, r)
		case query.Has("versioning"):
			sr.handler.GetBucketVersioning(w, r)
		case query.Has("versions"):
			sr.handler.ListObjectVersions(w, r)
		case query.Has("lifecycle"):
			sr.handler.GetBucketLifecycle(w, r)
		case query.Has("notification"):
			sr.handler.GetBucketNotification(w, r)
		default:
			sr.handler.ListObjects(w, r)
		}
	case http.MethodPost:
		if query.Has("delete") {
			sr.handler.DeleteMultipleObjects(w, r)
		} else {
			s3err.WriteError(w, r, s3err.ErrMethodNotAllowed)
		}
	default:
		s3err.WriteError(w, r, s3err.ErrMethodNotAllowed)
	}
}

// hasAnySubresource reports whether the query names a bucket sub-resource
// (as opposed to listing parameters such as prefix/max-keys).
func hasAnySubresource(query map[string][]string) bool {
	for k := range query {
		if bucketSubresources[k] {
			return true
		}
	}
	return false
}

// resolve extracts bucket and key from the request. Path-style is the
// default; when server.virtual_host_domains is configured, a Host header of
// "<bucket>.<domain>" selects the bucket and the whole path is the key.
func (sr *s3Router) resolve(r *http.Request) (bucket, key string) {
	path := strings.TrimPrefix(r.URL.Path, "/")

	host := r.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.ToLower(host)
	for _, domain := range sr.handler.Config.Server.VirtualHostDomains {
		domain = strings.ToLower(strings.TrimPrefix(domain, "."))
		if domain == "" || !strings.HasSuffix(host, "."+domain) {
			continue
		}
		b := strings.TrimSuffix(host, "."+domain)
		if b != "" && !strings.Contains(b, ".") {
			return b, path
		}
	}

	idx := strings.IndexByte(path, '/')
	if idx < 0 {
		return path, ""
	}
	return path[:idx], path[idx+1:]
}
