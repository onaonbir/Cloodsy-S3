package webdav

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/onaonbir/Cloodsy-S3/config"
	"github.com/onaonbir/Cloodsy-S3/db"
	"github.com/onaonbir/Cloodsy-S3/storage"
	"golang.org/x/net/webdav"
)

// RunServer starts the WebDAV listener and returns its *http.Server (nil on
// listen failure). Mirrors admin.RunServer so main can manage it the same way.
func RunServer(database *db.DB, store storage.Backend, cfg *config.Config, logger *slog.Logger) *http.Server {
	// A prefix of "/" is equivalent to no prefix; normalize to "" so the webdav
	// handler doesn't emit doubled slashes in hrefs.
	prefix := cfg.WebDAV.Prefix
	if prefix == "/" {
		prefix = ""
	}

	handler := &webdav.Handler{
		Prefix:     prefix,
		FileSystem: &davFS{db: database, store: store},
		LockSystem: webdav.NewMemLS(),
		Logger: func(r *http.Request, err error) {
			if err != nil {
				logger.Debug("webdav request", "method", r.Method, "path", r.URL.Path, "error", err)
			}
		},
	}

	srv := &http.Server{
		Addr:              cfg.WebDAV.Listen,
		Handler:           basicAuth(database, handler),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	ln, err := net.Listen("tcp", cfg.WebDAV.Listen)
	if err != nil {
		logger.Error("webdav server listen failed", "addr", cfg.WebDAV.Listen, "error", err)
		return nil
	}

	go func() {
		logger.Info(fmt.Sprintf("WebDAV server listening on %s", cfg.WebDAV.Listen))
		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			logger.Error("webdav server error", "error", err)
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
		logger.Error("webdav server shutdown error", "error", err)
	}
}
