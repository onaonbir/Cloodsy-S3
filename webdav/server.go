package webdav

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/onaonbir/Cloodsy-S3/config"
	"github.com/onaonbir/Cloodsy-S3/db"
	"github.com/onaonbir/Cloodsy-S3/httpx"
	"github.com/onaonbir/Cloodsy-S3/service"
	"github.com/onaonbir/Cloodsy-S3/storage"
	"golang.org/x/net/webdav"
)

// progressIdle aborts a request only after this long without any byte moving
// in either direction, so multi-GB transfers are never cut off mid-way.
const progressIdle = 60 * time.Second

// RunServer starts the WebDAV listener and returns its *http.Server (nil on
// listen failure). Mirrors admin.RunServer so main can manage it the same way.
func RunServer(database *db.DB, store storage.Backend, objects *service.Objects, cfg *config.Config, logger *slog.Logger) *http.Server {
	srv := &http.Server{
		Addr:              cfg.WebDAV.Listen,
		Handler:           newHandler(database, objects, cfg, logger),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		// Whole-request timeouts would cap large uploads/downloads; the
		// per-request ProgressTimeout middleware handles stalls instead.
		ReadTimeout:  0,
		WriteTimeout: 0,
	}

	certFile, keyFile := "", ""
	if cfg.WebDAV.TLS.Enabled {
		certFile, keyFile = cfg.WebDAV.TLS.CertFile, cfg.WebDAV.TLS.KeyFile
	}
	ln, err := httpx.Listen(cfg.WebDAV.Listen, certFile, keyFile, cfg.Server.MaxConnections)
	if err != nil {
		logger.Error("webdav server listen failed", "addr", cfg.WebDAV.Listen, "error", err)
		return nil
	}

	go func() {
		scheme := "http"
		if certFile != "" {
			scheme = "https"
		}
		logger.Info(fmt.Sprintf("WebDAV server listening on %s (%s)", cfg.WebDAV.Listen, scheme))
		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			logger.Error("webdav server error", "error", err)
		}
	}()

	return srv
}

// newHandler assembles the full middleware stack around the x/net handler.
// It is separated from RunServer so tests can drive it through httptest.
func newHandler(database *db.DB, objects *service.Objects, cfg *config.Config, logger *slog.Logger) http.Handler {
	// A prefix of "/" is equivalent to no prefix; normalize to "" so the webdav
	// handler doesn't emit doubled slashes in hrefs.
	prefix := cfg.WebDAV.Prefix
	if prefix == "/" {
		prefix = ""
	}
	lockCap := config.Duration(cfg.WebDAV.LockTimeout, 10*time.Minute)

	dav := &webdav.Handler{
		Prefix:     prefix,
		FileSystem: &davFS{db: database, objects: objects, logger: logger},
		LockSystem: cappedLockSystem{LockSystem: webdav.NewMemLS(), max: lockCap},
		Logger: func(r *http.Request, err error) {
			if err != nil {
				logger.Debug("webdav request", "method", r.Method, "path", httpx.SanitizeLog(r.URL.Path), "error", err)
			}
		},
	}

	var h http.Handler = dav
	h = mapStatus(h)
	h = lockTimeoutFix(h, lockCap)
	h = finiteDepth(h)
	h = validatePaths(prefix, h)
	h = basicAuth(database, newFailLimiter(), h)
	h = noCache(h)
	h = davHeaders(h)
	h = httpx.ProgressTimeout(h, progressIdle)
	h = httpx.Recover(h, logger)
	return h
}

// --- lock cap ---------------------------------------------------------------

// cappedLockSystem bounds every lock's lifetime. Clients that omit Timeout
// (or ask for Infinite) would otherwise hold a lock forever after a crash.
type cappedLockSystem struct {
	webdav.LockSystem
	max time.Duration
}

func (c cappedLockSystem) capDuration(d time.Duration) time.Duration {
	if d <= 0 || d > c.max {
		return c.max
	}
	return d
}

func (c cappedLockSystem) Create(now time.Time, details webdav.LockDetails) (string, error) {
	details.Duration = c.capDuration(details.Duration)
	return c.LockSystem.Create(now, details)
}

func (c cappedLockSystem) Refresh(now time.Time, token string, duration time.Duration) (webdav.LockDetails, error) {
	return c.LockSystem.Refresh(now, token, c.capDuration(duration))
}

// lockTimeoutRe matches the timeout element x/net writes in a LOCK response.
var lockTimeoutRe = regexp.MustCompile(`<D:timeout>Second-(-?\d+)</D:timeout>`)

// lockTimeoutFix rewrites the <D:timeout> in a LOCK-create response. The x/net
// handler echoes the duration the client asked for rather than the one the
// LockSystem granted, so an "Infinite" request would be reported as Second-0
// even though the lock really expires after the cap.
func lockTimeoutFix(next http.Handler, capDur time.Duration) http.Handler {
	capSecs := int64(capDur / time.Second)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "LOCK" {
			next.ServeHTTP(w, r)
			return
		}
		rec := &bufferedWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		body := rec.buf.Bytes()
		if rec.status < 300 {
			body = lockTimeoutRe.ReplaceAllFunc(body, func(m []byte) []byte {
				sub := lockTimeoutRe.FindSubmatch(m)
				n, err := strconv.ParseInt(string(sub[1]), 10, 64)
				if err != nil || (n > 0 && n <= capSecs) {
					return m
				}
				return []byte(fmt.Sprintf("<D:timeout>Second-%d</D:timeout>", capSecs))
			})
		}
		w.Header().Del("Content-Length")
		w.WriteHeader(rec.status)
		w.Write(body)
	})
}

// bufferedWriter captures a small response so it can be post-processed.
type bufferedWriter struct {
	http.ResponseWriter
	buf    bytes.Buffer
	status int
	wrote  bool
}

func (b *bufferedWriter) WriteHeader(status int) {
	if !b.wrote {
		b.wrote = true
		b.status = status
	}
}

func (b *bufferedWriter) Write(p []byte) (int, error) {
	if !b.wrote {
		b.WriteHeader(http.StatusOK)
	}
	return b.buf.Write(p)
}

// --- middleware -------------------------------------------------------------

// davHeaders advertises class 1+2 compliance on every response. The Windows
// Mini-Redirector probes OPTIONS and refuses to mount unless it sees DAV and
// MS-Author-Via; the x/net handler only sets them on OPTIONS for existing
// paths, so set them unconditionally here.
func davHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("DAV", "1, 2")
		w.Header().Set("MS-Author-Via", "DAV")
		next.ServeHTTP(w, r)
	})
}

// noCache tells WebDAV clients to revalidate rather than serve from their local
// cache. The server itself holds no cache — listings come live from the metadata
// DB and content live from disk — but OS clients (the Windows WebClient
// redirector, Finder, davfs2) cache aggressively, which is the usual cause of
// "stale" directory listings. These headers don't disable client caching
// entirely (that's not possible at the protocol level) but push clients to check
// for changes far more often.
func noCache(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
		w.Header().Set("Pragma", "no-cache")
		w.Header().Set("Expires", "0")
		next.ServeHTTP(w, r)
	})
}

const propfindFiniteDepthBody = `<?xml version="1.0" encoding="utf-8"?>` + "\n" +
	`<D:error xmlns:D="DAV:"><D:propfind-finite-depth/></D:error>` + "\n"

// finiteDepth refuses PROPFIND Depth: infinity (RFC 4918 §9.1 allows servers
// to do so with the propfind-finite-depth precondition) — walking a whole
// bucket per request is a trivial DoS. A missing Depth header, which the RFC
// says means infinity, is treated as Depth: 1 so simple clients keep working.
func finiteDepth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "PROPFIND" {
			switch strings.ToLower(strings.TrimSpace(r.Header.Get("Depth"))) {
			case "infinity":
				w.Header().Set("Content-Type", "application/xml; charset=utf-8")
				w.WriteHeader(http.StatusForbidden)
				w.Write([]byte(propfindFiniteDepthBody))
				return
			case "":
				r.Header.Set("Depth", "1")
			}
		}
		next.ServeHTTP(w, r)
	})
}

// validatePaths rejects request and Destination paths that can never name a
// valid object key before they reach the filesystem: NUL bytes and over-long
// keys are 400, dot segments (".", "..") are 403. It also seeds the per-request
// state used to pass the declared upload size and domain errors around.
func validatePaths(prefix string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if status := checkPath(prefix, r.URL.Path); status != 0 {
			http.Error(w, http.StatusText(status), status)
			return
		}
		if dst := r.Header.Get("Destination"); dst != "" && (r.Method == "MOVE" || r.Method == "COPY") {
			if u, err := url.Parse(dst); err == nil {
				if status := checkPath(prefix, u.Path); status != 0 {
					http.Error(w, http.StatusText(status), status)
					return
				}
			}
		}
		st := &reqState{declaredSize: -1}
		if r.Method == http.MethodPut {
			st.declaredSize = r.ContentLength
		}
		ctx := context.WithValue(r.Context(), ctxReqState, st)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// checkPath returns the HTTP status to reject p with, or 0 when it is fine.
func checkPath(prefix, p string) int {
	if strings.IndexByte(p, 0) >= 0 {
		return http.StatusBadRequest
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == "." || seg == ".." {
			return http.StatusForbidden
		}
	}
	rel := strings.TrimPrefix(p, prefix)
	if len(keyFromName(rel)) > 1024 {
		return http.StatusBadRequest
	}
	return 0
}

// mapStatus rewrites the generic error codes the x/net handler emits (it
// only knows os.ErrNotExist / os.ErrPermission) into precise ones when the
// filesystem recorded a domain error for the request: 413 for oversized
// uploads, 507 when the bucket quota is exhausted, 500 for a partially
// applied directory move/delete and 400 for an unrepresentable key.
func mapStatus(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		st := stateFromCtx(r.Context())
		if st == nil {
			next.ServeHTTP(w, r)
			return
		}
		next.ServeHTTP(&statusWriter{ResponseWriter: w, state: st}, r)
	})
}

type statusWriter struct {
	http.ResponseWriter
	state    *reqState
	status   int
	remapped bool
	wrote    bool
}

func (s *statusWriter) mapped(status int) int {
	if status < 400 || s.state.err == nil {
		return status
	}
	err := s.state.err
	switch {
	case errors.Is(err, service.ErrTooLarge):
		return http.StatusRequestEntityTooLarge
	case errors.Is(err, service.ErrQuotaExceeded):
		return http.StatusInsufficientStorage
	case errors.Is(err, errPartialMove):
		return http.StatusInternalServerError
	case errors.Is(err, errInvalidKey):
		return http.StatusBadRequest
	}
	return status
}

func (s *statusWriter) WriteHeader(status int) {
	if s.wrote {
		return
	}
	s.wrote = true
	s.status = s.mapped(status)
	if s.status != status {
		s.remapped = true
		s.ResponseWriter.Header().Set("Content-Type", "text/plain; charset=utf-8")
	}
	s.ResponseWriter.WriteHeader(s.status)
}

func (s *statusWriter) Write(p []byte) (int, error) {
	if !s.wrote {
		s.WriteHeader(http.StatusOK)
	}
	if s.remapped {
		// The handler is about to write the text of the status it chose; the
		// error is already known, so give the client the mapped one instead.
		s.remapped = false
		return s.ResponseWriter.Write([]byte(http.StatusText(s.status) + "\n"))
	}
	return s.ResponseWriter.Write(p)
}

func (s *statusWriter) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (s *statusWriter) Unwrap() http.ResponseWriter { return s.ResponseWriter }

func StopServer(srv *http.Server, logger *slog.Logger) {
	if srv == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		logger.Error("webdav server shutdown error", "error", err)
	}
}
