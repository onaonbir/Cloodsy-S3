package admin

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/onaonbir/Cloodsy-S3/httpx"
	"github.com/onaonbir/Cloodsy-S3/metrics"
)

// progressIdle is the per-request idle timeout: a connection that neither
// sends nor receives a byte for this long is dropped. Total duration is not
// capped so slow-but-alive clients are never cut off.
const progressIdle = 60 * time.Second

// newRootHandler builds the listener's handler chain: request metrics, panic
// recovery, the per-request idle (progress) timeout and the JSON body cap.
// The cap applies to every endpoint except the streaming uploads
// (PUT /admin/buckets/{bucket}/object and multipart part uploads), whose size
// is bounded by quota and the per-object/per-part maximum instead.
func newRootHandler(handler *Handler, logger *slog.Logger) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/admin/", handler)

	capped := httpx.MaxBody(mux, maxJSONBody)
	var root http.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isObjectUpload(r) {
			mux.ServeHTTP(w, r)
			return
		}
		capped.ServeHTTP(w, r)
	})
	root = httpx.ProgressTimeout(root, progressIdle)
	root = httpx.Recover(root, logger)
	return metrics.Middleware(root, metrics.Admin)
}

// RunServer starts the Admin API listener. It returns nil when the listener
// could not be opened (the error is logged).
func RunServer(handler *Handler, listen string, logger *slog.Logger) *http.Server {
	root := newRootHandler(handler, logger)

	srv := &http.Server{
		Addr:              listen,
		Handler:           root,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       0,
		WriteTimeout:      0,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    64 << 10,
	}

	certFile, keyFile := "", ""
	scheme := "http"
	var ln net.Listener
	var err error
	switch {
	case handler != nil && handler.Config != nil && handler.Config.Admin.TLS.Enabled && handler.TLSConfig != nil:
		scheme = "https"
		ln, err = httpx.ListenTLS(listen, handler.TLSConfig, 0)
	case handler != nil && handler.Config != nil && handler.Config.Admin.TLS.Enabled:
		certFile = handler.Config.Admin.TLS.CertFile
		keyFile = handler.Config.Admin.TLS.KeyFile
		if certFile == "" || keyFile == "" {
			logger.Error("admin TLS enabled but cert_file/key_file missing; refusing to start admin listener", "addr", listen)
			return nil
		}
		scheme = "https"
		ln, err = httpx.Listen(listen, certFile, keyFile, 0)
	default:
		ln, err = httpx.Listen(listen, "", "", 0)
	}
	if err != nil {
		logger.Error("admin server listen failed", "addr", listen, "error", err)
		return nil
	}

	go func() {
		defer func() {
			if rec := recover(); rec != nil {
				logger.Error("admin server goroutine panic", "panic", fmt.Sprint(rec))
			}
		}()
		logger.Info(fmt.Sprintf("Admin API listening on %s://%s", scheme, listen))
		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			logger.Error("admin server error", "error", err)
		}
	}()

	return srv
}

func StopServer(srv *http.Server, logger *slog.Logger) {
	if srv == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		logger.Error("admin server shutdown error", "error", err)
	}
}

// clearDeadlines lifts the progress (idle) deadline for a request that does
// long server-side work without moving bytes (multipart assembly, large
// copies). The deadline is re-armed by the next byte written.
func clearDeadlines(w http.ResponseWriter) {
	rc := http.NewResponseController(w)
	_ = rc.SetReadDeadline(time.Time{})
	_ = rc.SetWriteDeadline(time.Time{})
}
