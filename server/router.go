package server

import (
	"log/slog"
	"net/http"
	"path"
	"strings"

	"github.com/google/uuid"
	"github.com/onaonbir/Cloodsy-S3/handler"
)

// maskPresignedQuery replaces sensitive presigned URL params with "***".
func maskPresignedQuery(raw string) string {
	parts := strings.Split(raw, "&")
	for i, p := range parts {
		if strings.HasPrefix(p, "X-Amz-Signature=") {
			parts[i] = "X-Amz-Signature=***"
		} else if strings.HasPrefix(p, "X-Amz-Credential=") {
			parts[i] = "X-Amz-Credential=***"
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

func (sr *s3Router) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Add request ID and security headers
	requestID := uuid.New().String()
	w.Header().Set("x-amz-request-id", requestID)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Cache-Control", "no-store")

	// CORS support for browser-based S3 clients
	if origin := r.Header.Get("Origin"); origin != "" {
		w.Header().Set("Access-Control-Allow-Origin", origin)
		w.Header().Set("Access-Control-Allow-Methods", "GET, PUT, POST, DELETE, HEAD, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, Content-MD5, X-Amz-Content-Sha256, X-Amz-Date, X-Amz-Security-Token, X-Amz-User-Agent, X-Amz-Copy-Source, X-Amz-Copy-Source-Range, X-Amz-Meta-*")
		w.Header().Set("Access-Control-Expose-Headers", "ETag, x-amz-request-id, x-amz-version-id, x-amz-delete-marker")
		w.Header().Set("Access-Control-Max-Age", "86400")
	}
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	// Mask sensitive query params (presigned URL signatures) in logs
	logQuery := r.URL.RawQuery
	if strings.Contains(logQuery, "X-Amz-Signature") {
		logQuery = maskPresignedQuery(logQuery)
	}

	sr.logger.Info("request",
		"method", r.Method,
		"path", r.URL.Path,
		"query", logQuery,
		"remote", r.RemoteAddr,
		"requestId", requestID,
	)

	// Normalize path to prevent double-slash and traversal confusion
	cleanPath := path.Clean(r.URL.Path)
	if cleanPath == "." {
		cleanPath = "/"
	}
	query := r.URL.Query()

	// Parse bucket and key from path
	trimmed := strings.TrimPrefix(cleanPath, "/")
	bucketName := ""
	key := ""
	if trimmed != "" {
		idx := strings.IndexByte(trimmed, '/')
		if idx < 0 {
			bucketName = trimmed
		} else {
			bucketName = trimmed[:idx]
			key = trimmed[idx+1:]
		}
	}

	// Route: GET / → ListBuckets
	if bucketName == "" {
		if r.Method == http.MethodGet {
			sr.handler.ListBuckets(w, r)
			return
		}
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	// Route: operations on bucket with key
	if key != "" {
		switch r.Method {
		case http.MethodPut:
			if query.Has("partNumber") && query.Has("uploadId") {
				// UploadPartCopy if X-Amz-Copy-Source is present
				if r.Header.Get("X-Amz-Copy-Source") != "" {
					sr.handler.UploadPartCopy(w, r)
				} else {
					sr.handler.UploadPart(w, r)
				}
			} else if query.Has("acl") {
				sr.handler.PutObjectAcl(w, r)
			} else if query.Has("tagging") {
				sr.handler.PutObjectTagging(w, r)
			} else {
				sr.handler.PutObject(w, r)
			}
		case http.MethodGet:
			if query.Has("acl") {
				sr.handler.GetObjectAcl(w, r)
			} else if query.Has("tagging") {
				sr.handler.GetObjectTagging(w, r)
			} else if query.Has("uploadId") && !query.Has("partNumber") {
				// GET /<bucket>/<key>?uploadId=X → ListParts
				sr.handler.ListParts(w, r)
			} else {
				sr.handler.GetObject(w, r)
			}
		case http.MethodHead:
			sr.handler.HeadObject(w, r)
		case http.MethodDelete:
			if query.Has("uploadId") {
				sr.handler.AbortMultipartUpload(w, r)
			} else if query.Has("tagging") {
				sr.handler.DeleteObjectTagging(w, r)
			} else {
				sr.handler.DeleteObject(w, r)
			}
		case http.MethodPost:
			if query.Has("uploadId") {
				sr.handler.CompleteMultipartUpload(w, r)
			} else if query.Has("uploads") {
				sr.handler.CreateMultipartUpload(w, r)
			} else {
				w.WriteHeader(http.StatusMethodNotAllowed)
			}
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
		return
	}

	// Route: operations on bucket (no key)
	switch r.Method {
	case http.MethodPut:
		if query.Has("versioning") {
			sr.handler.PutBucketVersioning(w, r)
		} else if query.Has("lifecycle") {
			sr.handler.PutBucketLifecycle(w, r)
		} else if query.Has("notification") {
			sr.handler.PutBucketNotification(w, r)
		} else if query.Has("acl") {
			sr.handler.PutBucketAcl(w, r)
		} else if query.Has("tagging") {
			sr.handler.PutBucketTagging(w, r)
		} else if query.Has("encryption") {
			sr.handler.PutBucketEncryption(w, r)
		} else if query.Has("policy") {
			sr.handler.PutBucketPolicy(w, r)
		} else {
			sr.handler.CreateBucket(w, r)
		}
	case http.MethodDelete:
		if query.Has("lifecycle") {
			sr.handler.DeleteBucketLifecycle(w, r)
		} else if query.Has("notification") {
			sr.handler.DeleteBucketNotification(w, r)
		} else if query.Has("tagging") {
			sr.handler.DeleteBucketTagging(w, r)
		} else if query.Has("policy") {
			sr.handler.DeleteBucketPolicy(w, r)
		} else {
			sr.handler.DeleteBucket(w, r)
		}
	case http.MethodHead:
		sr.handler.HeadBucket(w, r)
	case http.MethodGet:
		if query.Has("location") {
			sr.handler.GetBucketLocation(w, r)
		} else if query.Has("uploads") {
			sr.handler.ListMultipartUploads(w, r)
		} else if query.Has("acl") {
			sr.handler.GetBucketAcl(w, r)
		} else if query.Has("tagging") {
			sr.handler.GetBucketTagging(w, r)
		} else if query.Has("encryption") {
			sr.handler.GetBucketEncryption(w, r)
		} else if query.Has("policy") {
			sr.handler.GetBucketPolicy(w, r)
		} else if query.Has("versioning") {
			sr.handler.GetBucketVersioning(w, r)
		} else if query.Has("versions") {
			sr.handler.ListObjectVersions(w, r)
		} else if query.Has("lifecycle") {
			sr.handler.GetBucketLifecycle(w, r)
		} else if query.Has("notification") {
			sr.handler.GetBucketNotification(w, r)
		} else {
			sr.handler.ListObjects(w, r)
		}
	case http.MethodPost:
		if query.Has("delete") {
			sr.handler.DeleteMultipleObjects(w, r)
		} else {
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}
