// Package webdav exposes S3 buckets over the WebDAV protocol so clients can
// mount a bucket as a network drive. It wraps golang.org/x/net/webdav with a
// FileSystem backed by the metadata DB (listings) and the storage backend
// (content). Authentication is HTTP Basic: username = access key,
// password = secret key. One credential maps to exactly one bucket, which
// becomes the mount root.
package webdav

import (
	"context"
	"crypto/subtle"
	"net/http"

	"github.com/onaonbir/Cloodsy-S3/db"
)

type ctxKey int

const (
	ctxBucket ctxKey = iota
	ctxReadOnly
)

func bucketFromCtx(ctx context.Context) *db.Bucket {
	b, _ := ctx.Value(ctxBucket).(*db.Bucket)
	return b
}

func readOnlyFromCtx(ctx context.Context) bool {
	ro, _ := ctx.Value(ctxReadOnly).(bool)
	return ro
}

// basicAuth authenticates the request via HTTP Basic, resolves the credential's
// bucket, and stashes it (plus the read-only flag) in the request context for
// the FileSystem to scope every operation to that single bucket.
func basicAuth(database *db.DB, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		if !ok {
			unauthorized(w)
			return
		}
		cred, err := database.GetCredentialByAccessKey(user)
		if err != nil || cred == nil ||
			subtle.ConstantTimeCompare([]byte(cred.SecretKey), []byte(pass)) != 1 {
			unauthorized(w)
			return
		}
		bucket, err := database.GetBucketByID(cred.BucketID)
		if err != nil || bucket == nil {
			unauthorized(w)
			return
		}

		// Per-bucket gate: the global WebDAV server may be on, but each bucket
		// must opt in to be mountable.
		if !bucket.WebDAVEnabled {
			http.Error(w, "Forbidden: WebDAV is disabled for this bucket", http.StatusForbidden)
			return
		}

		readOnly := cred.Permission == "read-only"
		if readOnly && isWriteMethod(r.Method) {
			http.Error(w, "Forbidden: read-only credential", http.StatusForbidden)
			return
		}

		ctx := context.WithValue(r.Context(), ctxBucket, bucket)
		ctx = context.WithValue(ctx, ctxReadOnly, readOnly)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// isWriteMethod reports whether a WebDAV method mutates state.
func isWriteMethod(method string) bool {
	switch method {
	case "PUT", "DELETE", "MKCOL", "MOVE", "COPY", "PROPPATCH", "LOCK", "UNLOCK":
		return true
	}
	return false
}

func unauthorized(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", `Basic realm="Cloodsy WebDAV"`)
	http.Error(w, "Unauthorized", http.StatusUnauthorized)
}
