package image

import (
	"context"
	"io"
	"log/slog"
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
	Quality   int
	Workers   int
	QueueSize int
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
	once   sync.Once
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
	return &Worker{
		store:  store,
		cfg:    cfg,
		logger: logger,
		jobs:   make(chan Job, cfg.QueueSize),
	}
}

// Start launches the worker goroutines. They drain the queue until Stop is
// called or ctx is cancelled.
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
// queue is full the job is dropped (and logged) rather than stalling the PUT.
func (w *Worker) Enqueue(job Job) bool {
	select {
	case w.jobs <- job:
		return true
	default:
		w.logger.Warn("image optimize queue full, dropping job", "bucket", job.Bucket, "key", job.Key)
		return false
	}
}

// Process optimizes a single object synchronously. Best-effort: any error is
// logged and the original is left untouched.
func (w *Worker) Process(job Job) {
	if err := OptimizeOne(w.store, job, w.cfg.Quality); err != nil {
		w.logger.Debug("optimize failed", "key", job.Key, "error", err)
	}
}

// OptimizeOne generates the optimized (no-resize, given quality) variant for a
// single object and writes it to the variant cache. The original is never
// touched. Returns nil for non-image content types (nothing to do). Shared by
// the background Worker and the CLI/admin reprocess paths.
func OptimizeOne(store VariantStore, job Job, quality int) error {
	if !IsImageContentType(job.ContentType) {
		return nil
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

	data, _, terr := Transform(src, job.ContentType, p)
	src.Close()
	if terr != nil {
		return terr
	}
	return store.PutVariant(job.Bucket, cacheKey, data)
}

// Stop closes the queue and waits for in-flight jobs to finish. Safe to call
// more than once.
func (w *Worker) Stop() {
	w.once.Do(func() { close(w.jobs) })
	w.wg.Wait()
}
