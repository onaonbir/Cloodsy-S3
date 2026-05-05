package server

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/onaonbir/Cloodsy-S3/config"
	"github.com/onaonbir/Cloodsy-S3/db"
	"github.com/onaonbir/Cloodsy-S3/handler"
	"github.com/onaonbir/Cloodsy-S3/lifecycle"
	"github.com/onaonbir/Cloodsy-S3/storage"
	"github.com/onaonbir/Cloodsy-S3/webhook"
)

// Ensure storage.Backend implements multipartCleaner at compile time.
var _ multipartCleaner = (storage.Backend)(nil)

// Build-time version info, set by the caller (main package).
var (
	Version    = "dev"
	CommitHash = "unknown"
)

func Run(cfg *config.Config, h *handler.Handler, logger *slog.Logger) error {
	router := NewRouter(h, logger)

	srv := &http.Server{
		Addr:              cfg.Server.Listen,
		Handler:           router,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    8192, // 8 KB
	}

	// Graceful shutdown
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Start stale multipart cleanup goroutine
	maxAge, err := time.ParseDuration(cfg.Storage.MultipartMaxAge)
	if err != nil {
		maxAge = 24 * time.Hour
		logger.Warn("invalid multipart_max_age, using default 24h", "value", cfg.Storage.MultipartMaxAge)
	}
	go runMultipartCleanup(ctx, h.DB, h.Storage, maxAge, logger)

	// Start lifecycle cleaner goroutine
	lifecycleInterval := time.Hour
	if cfg.Storage.LifecycleInterval != "" {
		if d, err := time.ParseDuration(cfg.Storage.LifecycleInterval); err == nil {
			lifecycleInterval = d
		}
	}
	go lifecycle.StartCleaner(ctx, h.DB, h.Storage, lifecycleInterval, logger)

	// Start webhook dispatcher
	dispatcher := webhook.NewDispatcher(h.DB, 4, logger)
	h.Dispatcher = dispatcher

	go func() {
		<-ctx.Done()
		logger.Info("shutting down server...")
		dispatcher.Stop()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		srv.Shutdown(shutdownCtx)
	}()

	logger.Info("S3 server listening on " + cfg.Server.Listen)

	if cfg.Server.TLS.Enabled {
		return srv.ListenAndServeTLS(cfg.Server.TLS.CertFile, cfg.Server.TLS.KeyFile)
	}
	return srv.ListenAndServe()
}

// multipartCleaner is the subset of storage.Backend needed for cleanup.
type multipartCleaner interface {
	DeleteMultipartParts(bucket, uploadID string) error
}

// runMultipartCleanup periodically removes stale multipart uploads.
func runMultipartCleanup(ctx context.Context, database *db.DB, store multipartCleaner, maxAge time.Duration, logger *slog.Logger) {
	ticker := time.NewTicker(1 * time.Hour)
	defer ticker.Stop()

	// Run once at startup
	cleanStaleUploads(database, store, maxAge, logger)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			cleanStaleUploads(database, store, maxAge, logger)
		}
	}
}

func cleanStaleUploads(database *db.DB, store multipartCleaner, maxAge time.Duration, logger *slog.Logger) {
	uploads, err := database.ListStaleMultipartUploads(maxAge)
	if err != nil {
		logger.Error("failed to list stale multipart uploads", "error", err)
		return
	}

	if len(uploads) == 0 {
		return
	}

	// Cache bucket-id -> name lookups; many stale uploads typically share buckets.
	bucketNames := make(map[int64]string)

	cleaned := 0
	for _, u := range uploads {
		name, ok := bucketNames[u.BucketID]
		if !ok {
			n, err := database.GetBucketNameByID(u.BucketID)
			if err != nil {
				logger.Error("failed to resolve bucket for stale upload", "uploadId", u.ID, "bucketId", u.BucketID, "error", err)
				continue
			}
			if n == "" {
				// Bucket already gone (cascade deleted); just drop the DB row.
				if err := database.DeleteMultipartUpload(u.ID); err != nil {
					logger.Error("failed to delete stale multipart record", "uploadId", u.ID, "error", err)
					continue
				}
				cleaned++
				continue
			}
			bucketNames[u.BucketID] = n
			name = n
		}
		if err := store.DeleteMultipartParts(name, u.ID); err != nil {
			logger.Error("failed to delete stale multipart parts", "uploadId", u.ID, "bucket", name, "error", err)
			continue
		}
		if err := database.DeleteMultipartUpload(u.ID); err != nil {
			logger.Error("failed to delete stale multipart record", "uploadId", u.ID, "error", err)
			continue
		}
		cleaned++
	}

	logger.Info("cleaned up stale multipart uploads", "count", cleaned, "maxAge", maxAge.String())
}
