// Package lifecycle applies bucket lifecycle rules: current-version
// expiration, noncurrent-version expiration, orphan delete-marker removal and
// incomplete multipart upload cleanup.
package lifecycle

import (
	"context"
	"log/slog"
	"time"

	"github.com/onaonbir/Cloodsy-S3/db"
	"github.com/onaonbir/Cloodsy-S3/service"
	"github.com/onaonbir/Cloodsy-S3/storage"
)

const batchSize = 100

// StartCleaner runs a background goroutine that periodically cleans up expired objects.
func StartCleaner(ctx context.Context, database *db.DB, store storage.Backend, objects *service.Objects, interval time.Duration, logger *slog.Logger) {
	if interval <= 0 {
		interval = time.Hour
	}
	if objects == nil {
		objects = service.New(database, store, logger)
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	RunOnce(ctx, database, store, objects, logger)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			RunOnce(ctx, database, store, objects, logger)
		}
	}
}

// RunOnce evaluates every rule a single time.
func RunOnce(ctx context.Context, database *db.DB, store storage.Backend, objects *service.Objects, logger *slog.Logger) {
	rules, err := database.GetAllLifecycleRules()
	if err != nil {
		logger.Error("lifecycle: failed to get rules", "error", err)
		return
	}
	if len(rules) == 0 {
		return
	}

	total := 0
	for _, rule := range rules {
		if ctx.Err() != nil {
			return
		}
		if rule.Status != "Enabled" {
			continue
		}
		bucket, err := database.GetBucket(rule.BucketName)
		if err != nil || bucket == nil {
			continue
		}
		if rule.ExpirationDays > 0 {
			total += expireCurrent(ctx, database, objects, bucket, rule, logger)
		}
		if rule.NoncurrentDays > 0 {
			total += expireNoncurrent(ctx, database, objects, bucket, rule, logger)
		}
		if rule.ExpireDeleteMarkers {
			total += expireDeleteMarkers(ctx, database, objects, bucket, rule, logger)
		}
		if rule.AbortMultipartDays > 0 {
			total += abortMultipart(ctx, database, store, bucket, rule, logger)
		}
	}
	if total > 0 {
		logger.Info("lifecycle cleanup complete", "objectsDeleted", total)
	}
}

// expireCurrent removes (or delete-marks, in versioned buckets) current
// objects older than the rule's expiration. The id cursor guarantees progress
// even when individual deletes fail.
func expireCurrent(ctx context.Context, database *db.DB, objects *service.Objects, bucket *db.Bucket, rule db.LifecycleRule, logger *slog.Logger) int {
	cleaned := 0
	var afterID int64
	for ctx.Err() == nil {
		batch, err := database.GetExpiredObjects(rule.BucketName, rule.Prefix, rule.ExpirationDays, afterID, batchSize)
		if err != nil {
			logger.Error("lifecycle: failed to get expired objects", "bucket", rule.BucketName, "error", err)
			return cleaned
		}
		if len(batch) == 0 {
			return cleaned
		}
		for _, obj := range batch {
			afterID = obj.ID
			if _, err := objects.Delete(bucket, obj.Key, ""); err != nil {
				logger.Error("lifecycle: failed to expire object", "bucket", rule.BucketName, "key", obj.Key, "error", err)
				continue
			}
			cleaned++
		}
	}
	return cleaned
}

// expireNoncurrent permanently deletes noncurrent versions (and noncurrent
// delete markers) older than NoncurrentDays.
func expireNoncurrent(ctx context.Context, database *db.DB, objects *service.Objects, bucket *db.Bucket, rule db.LifecycleRule, logger *slog.Logger) int {
	cleaned := 0
	var afterID int64
	for ctx.Err() == nil {
		batch, err := database.GetExpiredNoncurrentVersions(rule.BucketName, rule.Prefix, rule.NoncurrentDays, afterID, batchSize)
		if err != nil {
			logger.Error("lifecycle: failed to get noncurrent versions", "bucket", rule.BucketName, "error", err)
			return cleaned
		}
		if len(batch) == 0 {
			return cleaned
		}
		for _, obj := range batch {
			afterID = obj.ID
			if _, err := objects.Delete(bucket, obj.Key, versionForDelete(obj.VersionID)); err != nil {
				logger.Error("lifecycle: failed to delete noncurrent version", "bucket", rule.BucketName, "key", obj.Key, "version", obj.VersionID, "error", err)
				continue
			}
			cleaned++
		}
	}
	return cleaned
}

// expireDeleteMarkers removes delete markers that are the only remaining
// version of their key.
func expireDeleteMarkers(ctx context.Context, database *db.DB, objects *service.Objects, bucket *db.Bucket, rule db.LifecycleRule, logger *slog.Logger) int {
	cleaned := 0
	var afterID int64
	for ctx.Err() == nil {
		batch, err := database.GetOrphanDeleteMarkers(rule.BucketName, rule.Prefix, afterID, batchSize)
		if err != nil {
			logger.Error("lifecycle: failed to get delete markers", "bucket", rule.BucketName, "error", err)
			return cleaned
		}
		if len(batch) == 0 {
			return cleaned
		}
		for _, obj := range batch {
			afterID = obj.ID
			if _, err := objects.Delete(bucket, obj.Key, versionForDelete(obj.VersionID)); err != nil {
				logger.Error("lifecycle: failed to remove delete marker", "bucket", rule.BucketName, "key", obj.Key, "error", err)
				continue
			}
			cleaned++
		}
	}
	return cleaned
}

func abortMultipart(ctx context.Context, database *db.DB, store storage.Backend, bucket *db.Bucket, rule db.LifecycleRule, logger *slog.Logger) int {
	uploads, err := database.ListStaleMultipartUploadsForBucket(bucket.ID, rule.Prefix, time.Duration(rule.AbortMultipartDays)*24*time.Hour)
	if err != nil {
		logger.Error("lifecycle: failed to list multipart uploads", "bucket", rule.BucketName, "error", err)
		return 0
	}
	cleaned := 0
	for _, u := range uploads {
		if ctx.Err() != nil {
			break
		}
		if err := store.DeleteMultipartParts(bucket.Name, u.ID); err != nil {
			logger.Error("lifecycle: failed to delete multipart parts", "uploadId", u.ID, "error", err)
			continue
		}
		if _, err := database.DeleteMultipartUpload(u.ID); err != nil {
			logger.Error("lifecycle: failed to delete multipart record", "uploadId", u.ID, "error", err)
			continue
		}
		cleaned++
	}
	return cleaned
}

func versionForDelete(v string) string {
	if v == "" {
		return "null"
	}
	return v
}
