package admin

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/onaonbir/Cloodsy-S3/httpx"
)

// progressIdle is the per-request idle timeout: a connection that neither
// sends nor receives a byte for this long is dropped. Total duration is not
// capped so slow-but-alive clients are never cut off.
const progressIdle = 60 * time.Second

// RunServer starts the Admin API listener. It returns nil when the listener
// could not be opened (the error is logged).
func RunServer(handler *Handler, listen string, logger *slog.Logger) *http.Server {
	mux := http.NewServeMux()
	mux.Handle("/admin/", handler)

	// Every admin endpoint carries a small JSON body (there is no streaming
	// object upload on this listener), so a global body cap is safe.
	var root http.Handler = httpx.MaxBody(mux, maxJSONBody)
	root = httpx.ProgressTimeout(root, progressIdle)
	root = httpx.Recover(root, logger)

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
	if handler != nil && handler.Config != nil && handler.Config.Admin.TLS.Enabled {
		certFile = handler.Config.Admin.TLS.CertFile
		keyFile = handler.Config.Admin.TLS.KeyFile
		if certFile == "" || keyFile == "" {
			logger.Error("admin TLS enabled but cert_file/key_file missing; refusing to start admin listener", "addr", listen)
			return nil
		}
		scheme = "https"
	}

	ln, err := httpx.Listen(listen, certFile, keyFile, 0)
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
