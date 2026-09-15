package image

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"runtime/debug"
	"sync"

	"github.com/onaonbir/Cloodsy-S3/storage"
)

// VariantStore is the subset of storage.Backend the optimizer needs. Declaring
// it here keeps the dependency surface small and explicit.
type VariantStore interface {
	GetObject(bucket, key string) (io.ReadCloser, error)
	GetVersionedObject(bucket, key, versionID string) (io.ReadCloser, error)
	PutVariant(bucket, cacheKey string, data []byte) error
}

// Job describes a single object to optimize. The original is identified by
// bucket/key (+ version) and its ETag pins the variant cache entry.
type Job struct {
	Bucket      string
	Key         string
	VersionID   string
	ETag        string
	ContentType string
}

// WorkerConfig configures the optimizer pool.
type WorkerConfig struct {
	Quality        int
	Workers        int
	QueueSize      int
	MaxSourceBytes int64    // objects larger than this are skipped (0 = 32 MB)
	Limiter        *Limiter // optional shared decode limiter
}

// Worker generates optimized image variants in the background. The optimized
// variant is just a no-resize re-encode at the configured quality, stored in
// the same sibling variant cache used by on-access resizing. Originals are
// never modified; all work is best-effort.
type Worker struct {
	store  VariantStore
	cfg    WorkerConfig
	logger *slog.Logger
	jobs   chan Job
	wg     sync.WaitGroup
	mu     sync.RWMutex
	closed bool
}

// NewWorker builds a Worker. Call Start to launch the pool.
func NewWorker(store VariantStore, cfg WorkerConfig, logger *slog.Logger) *Worker {
	if cfg.Quality <= 0 || cfg.Quality > 100 {
		cfg.Quality = DefaultQuality
	}
	if cfg.Workers <= 0 {
		cfg.Workers = 2
	}
	if cfg.QueueSize <= 0 {
		cfg.QueueSize = 256
	}
	if cfg.MaxSourceBytes <= 0 {
		cfg.MaxSourceBytes = 32 << 20
	}
	return &Worker{
		store:  store,
		cfg:    cfg,
		logger: logger,
		jobs:   make(chan Job, cfg.QueueSize),
	}
}

// Start launches the worker goroutines. They drain the queue until Stop is
// called or ctx is cancelled. A panic in a decoder is contained per job.
func (w *Worker) Start(ctx context.Context) {
	for i := 0; i < w.cfg.Workers; i++ {
		w.wg.Add(1)
		go func() {
			defer w.wg.Done()
			for {
				select {
				case <-ctx.Done():
					return
				case job, ok := <-w.jobs:
					if !ok {
						return
					}
					w.Process(job)
				}
			}
		}()
	}
}

// Enqueue submits a job for asynchronous optimization. It never blocks: if the
// queue is full (or the worker is stopped) the job is dropped rather than
// stalling the PUT.
func (w *Worker) Enqueue(job Job) bool {
	w.mu.RLock()
	defer w.mu.RUnlock()
	if w.closed {
		return false
	}
	select {
	case w.jobs <- job:
		return true
	default:
		w.logger.Warn("image optimize queue full, dropping job", "bucket", job.Bucket, "key", job.Key)
		return false
	}
}

// Process optimizes a single object synchronously. Best-effort: any error or
// panic is logged and the original is left untouched.
func (w *Worker) Process(job Job) {
	defer func() {
		if rec := recover(); rec != nil {
			w.logger.Error("image optimizer panic", "key", job.Key, "panic", fmt.Sprint(rec), "stack", string(debug.Stack()))
		}
	}()
	if w.cfg.Limiter != nil {
		w.cfg.Limiter.Acquire(context.Background())
		defer w.cfg.Limiter.Release()
	}
	if err := OptimizeOne(w.store, job, w.cfg.Quality, w.cfg.MaxSourceBytes); err != nil {
		w.logger.Debug("optimize failed", "key", job.Key, "error", err)
	}
}

// OptimizeOne generates the optimized (no-resize, given quality) variant for a
// single object and writes it to the variant cache. The original is never
// touched. Returns nil for non-image content types (nothing to do). Shared by
// the background Worker and the CLI/admin reprocess paths.
func OptimizeOne(store VariantStore, job Job, quality int, maxSourceBytes int64) error {
	if !IsImageContentType(job.ContentType) {
		return nil
	}
	if maxSourceBytes <= 0 {
		maxSourceBytes = 32 << 20
	}
	p := Params{Mode: ModeFit, Quality: quality}
	cacheKey := storage.VariantCacheKey(job.Key, job.VersionID, job.ETag, p.Spec())

	var src io.ReadCloser
	var err error
	if job.VersionID != "" && job.VersionID != "null" {
		src, err = store.GetVersionedObject(job.Bucket, job.Key, job.VersionID)
	} else {
		src, err = store.GetObject(job.Bucket, job.Key)
	}
	if err != nil {
		return err
	}
	defer src.Close()

	data, _, terr := Transform(io.LimitReader(src, maxSourceBytes+1), job.ContentType, p)
	if terr != nil {
		return terr
	}
	return store.PutVariant(job.Bucket, cacheKey, data)
}

// Stop closes the queue and waits for in-flight jobs to finish. Safe to call
// more than once.
func (w *Worker) Stop() {
	w.mu.Lock()
	if !w.closed {
		w.closed = true
		close(w.jobs)
	}
	w.mu.Unlock()
	w.wg.Wait()
}
