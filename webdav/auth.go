// Package webdav exposes S3 buckets over the WebDAV protocol so clients can
// mount a bucket as a network drive. It wraps golang.org/x/net/webdav with a
// FileSystem backed by the metadata DB (listings) and the shared object
// service (content, versioning, quota, webhooks). Authentication is HTTP
// Basic: username = access key, password = secret key. One credential maps to
// exactly one bucket, which becomes the mount root.
package webdav

import (
	"context"
	"crypto/subtle"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/onaonbir/Cloodsy-S3/db"
)

type ctxKey int

const (
	ctxBucket ctxKey = iota
	ctxReadOnly
	ctxReqState
)

// reqState carries per-request facts between the HTTP middleware and the
// FileSystem (which only sees a context): the declared upload size and the
// last domain error, which the status mapper turns into a precise HTTP code.
type reqState struct {
	declaredSize int64 // -1 when unknown / not a PUT
	err          error
}

func bucketFromCtx(ctx context.Context) *db.Bucket {
	b, _ := ctx.Value(ctxBucket).(*db.Bucket)
	return b
}

func readOnlyFromCtx(ctx context.Context) bool {
	ro, _ := ctx.Value(ctxReadOnly).(bool)
	return ro
}

func stateFromCtx(ctx context.Context) *reqState {
	st, _ := ctx.Value(ctxReqState).(*reqState)
	return st
}

// --- per-IP failure limiter -------------------------------------------------

const (
	authMaxFailures = 5
	authLockFor     = 15 * time.Minute
)

type failEntry struct {
	fails       int
	lockedUntil time.Time
	last        time.Time
}

// failLimiter locks an IP out for authLockFor after authMaxFailures
// consecutive bad credentials. Successful auth resets the counter.
type failLimiter struct {
	mu      sync.Mutex
	entries map[string]*failEntry
	now     func() time.Time
}

func newFailLimiter() *failLimiter {
	return &failLimiter{entries: make(map[string]*failEntry), now: time.Now}
}

// blocked reports whether ip is currently locked out and, if so, for how long.
func (l *failLimiter) blocked(ip string) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	e := l.entries[ip]
	if e == nil {
		return false, 0
	}
	now := l.now()
	if now.Before(e.lockedUntil) {
		return true, e.lockedUntil.Sub(now)
	}
	return false, 0
}

func (l *failLimiter) fail(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	l.pruneLocked(now)
	e := l.entries[ip]
	if e == nil {
		e = &failEntry{}
		l.entries[ip] = e
	}
	// A stale streak (older than the lock window) starts over.
	if now.Sub(e.last) > authLockFor {
		e.fails = 0
	}
	e.fails++
	e.last = now
	if e.fails >= authMaxFailures {
		e.lockedUntil = now.Add(authLockFor)
		e.fails = 0
	}
}

func (l *failLimiter) reset(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.entries, ip)
}

// pruneLocked drops entries that carry no information any more so the map
// cannot grow without bound under a distributed brute-force attempt.
func (l *failLimiter) pruneLocked(now time.Time) {
	if len(l.entries) < 4096 {
		return
	}
	for ip, e := range l.entries {
		if now.After(e.lockedUntil) && now.Sub(e.last) > authLockFor {
			delete(l.entries, ip)
		}
	}
}

// clientIP extracts the peer address without the port.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// basicAuth authenticates the request via HTTP Basic, resolves the credential's
// bucket, and stashes it (plus the read-only flag) in the request context for
// the FileSystem to scope every operation to that single bucket.
func basicAuth(database *db.DB, limiter *failLimiter, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ip := clientIP(r)
		if locked, remaining := limiter.blocked(ip); locked {
			w.Header().Set("Retry-After", strconv.Itoa(int(remaining.Seconds())+1))
			http.Error(w, "Too Many Requests: authentication temporarily locked", http.StatusTooManyRequests)
			return
		}

		user, pass, ok := r.BasicAuth()
		if !ok {
			unauthorized(w)
			return
		}
		cred, err := database.GetCredentialByAccessKey(user)
		if err != nil || cred == nil ||
			subtle.ConstantTimeCompare([]byte(cred.SecretKey), []byte(pass)) != 1 {
			limiter.fail(ip)
			unauthorized(w)
			return
		}
		bucket, err := database.GetBucketByID(cred.BucketID)
		if err != nil || bucket == nil {
			limiter.fail(ip)
			unauthorized(w)
			return
		}
		limiter.reset(ip)

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
