package server

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/onaonbir/Cloodsy-S3/config"
	"github.com/onaonbir/Cloodsy-S3/db"
	"github.com/onaonbir/Cloodsy-S3/handler"
	"github.com/onaonbir/Cloodsy-S3/httpx"
	imageutil "github.com/onaonbir/Cloodsy-S3/image"
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

// Run starts the S3 listener and background workers and blocks until the
// process receives SIGINT/SIGTERM (or the optional parent ctx is cancelled).
// It returns nil after a clean shutdown.
func Run(cfg *config.Config, h *handler.Handler, logger *slog.Logger) error {
	return RunContext(context.Background(), cfg, h, logger)
}

func RunContext(parent context.Context, cfg *config.Config, h *handler.Handler, logger *slog.Logger) error {
	idle := config.Duration(cfg.Server.IdleTimeout, 60*time.Second)
	var root http.Handler = NewRouter(h, logger)
	root = httpx.ProgressTimeout(root, idle)
	root = httpx.Recover(root, logger)

	srv := &http.Server{
		Addr:              cfg.Server.Listen,
		Handler:           root,
		ReadHeaderTimeout: 10 * time.Second,
		// Read/Write timeouts stay 0: large uploads and downloads must not be
		// capped by wall-clock time. Stalled connections are cut by the
		// per-request progress deadline installed above.
		ReadTimeout:    0,
		WriteTimeout:   0,
		IdleTimeout:    120 * time.Second,
		MaxHeaderBytes: 32 * 1024,
		ErrorLog:       slog.NewLogLogger(logger.Handler(), slog.LevelDebug),
	}

	ctx, stop := signal.NotifyContext(parent, os.Interrupt, syscall.SIGTERM)
	defer stop()

	maxAge := config.Duration(cfg.Storage.MultipartMaxAge, 24*time.Hour)
	go runMultipartCleanup(ctx, h.DB, h.Storage, maxAge, logger)

	lifecycleInterval := config.Duration(cfg.Storage.LifecycleInterval, time.Hour)
	go lifecycle.StartCleaner(ctx, h.DB, h.Storage, h.Objects, lifecycleInterval, logger)

	dispatcher := webhook.NewDispatcher(h.DB, 4, logger)
	dispatcher.SetRegion(cfg.Server.Region)
	h.SetDispatcher(dispatcher)

	var imageWorker *imageutil.Worker
	if cfg.Image.Enabled {
		imageWorker = imageutil.NewWorker(h.Storage, imageutil.WorkerConfig{
			Quality:        cfg.Image.Quality,
			Workers:        cfg.Image.Workers,
			QueueSize:      cfg.Image.QueueSize,
			MaxSourceBytes: cfg.Image.MaxSourceBytes,
			Limiter:        h.Transforms,
		}, logger)
		imageWorker.Start(ctx)
		h.SetImageWorker(imageWorker)
		logger.Info("image optimizer enabled", "quality", cfg.Image.Quality, "syncMaxBytes", cfg.Image.SyncMaxBytes)
	}
	// On-access transforms (?w=&h=) are always available; keep their cache bounded.
	go imageutil.RunCacheJanitor(ctx, h.DB, h.Storage, cfg.Image.CacheMaxBytes, logger)

	certFile, keyFile := tlsFiles(cfg.Server.TLS)
	ln, err := httpx.Listen(cfg.Server.Listen, certFile, keyFile, cfg.Server.MaxConnections)
	if err != nil {
		return err
	}
	if cfg.Server.TLS.Enabled {
		logger.Info("S3 server listening (TLS) on " + cfg.Server.Listen)
	} else {
		logger.Info("S3 server listening on " + cfg.Server.Listen)
	}

	serveErr := make(chan error, 1)
	go func() {
		err := srv.Serve(ln)
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		serveErr <- err
	}()

	select {
	case err := <-serveErr:
		stop()
		dispatcher.Stop()
		if imageWorker != nil {
			imageWorker.Stop()
		}
		return err
	case <-ctx.Done():
	}

	logger.Info("shutting down server...")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	// Order matters: stop accepting requests and drain in-flight ones first,
	// then stop the workers those requests may still enqueue into.
	if err := srv.Shutdown(shutdownCtx); err != nil {
		logger.Warn("forced shutdown; some requests were interrupted", "error", err)
	}
	<-serveErr
	if imageWorker != nil {
		imageWorker.Stop()
	}
	dispatcher.Stop()
	logger.Info("server stopped")
	return nil
}

func tlsFiles(t config.TLSConfig) (string, string) {
	if !t.Enabled {
		return "", ""
	}
	return t.CertFile, t.KeyFile
}

// multipartCleaner is the subset of storage.Backend needed for cleanup.
type multipartCleaner interface {
	DeleteMultipartParts(bucket, uploadID string) error
}

// runMultipartCleanup periodically removes stale multipart uploads.
func runMultipartCleanup(ctx context.Context, database *db.DB, store multipartCleaner, maxAge time.Duration, logger *slog.Logger) {
	ticker := time.NewTicker(1 * time.Hour)
	defer ticker.Stop()

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
				if _, err := database.DeleteMultipartUpload(u.ID); err != nil {
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
		if _, err := database.DeleteMultipartUpload(u.ID); err != nil {
			logger.Error("failed to delete stale multipart record", "uploadId", u.ID, "error", err)
			continue
		}
		cleaned++
	}
	logger.Info("cleaned up stale multipart uploads", "count", cleaned, "maxAge", maxAge.String())
}
