package lifecycle

import (
	"context"
	"log/slog"
	"time"

	"github.com/onaonbir/Cloodsy-S3/db"
	"github.com/onaonbir/Cloodsy-S3/storage"
)

// StartCleaner runs a background goroutine that periodically cleans up expired objects.
func StartCleaner(ctx context.Context, database *db.DB, store storage.Backend, interval time.Duration, logger *slog.Logger) {
	if interval <= 0 {
		interval = time.Hour
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	// Run once at startup
	cleanExpiredObjects(database, store, logger)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			cleanExpiredObjects(database, store, logger)
		}
	}
}

func cleanExpiredObjects(database *db.DB, store storage.Backend, logger *slog.Logger) {
	rules, err := database.GetAllLifecycleRules()
	if err != nil {
		logger.Error("lifecycle: failed to get rules", "error", err)
		return
	}

	if len(rules) == 0 {
		return
	}

	totalCleaned := 0
	for _, rule := range rules {
		cleaned := cleanForRule(database, store, rule, logger)
		totalCleaned += cleaned
	}

	if totalCleaned > 0 {
		logger.Info("lifecycle cleanup complete", "objectsDeleted", totalCleaned)
	}
}

func cleanForRule(database *db.DB, store storage.Backend, rule db.LifecycleRule, logger *slog.Logger) int {
	cleaned := 0

	for {
		objects, err := database.GetExpiredObjects(rule.BucketName, rule.Prefix, rule.ExpirationDays, 100)
		if err != nil {
			logger.Error("lifecycle: failed to get expired objects", "bucket", rule.BucketName, "error", err)
			break
		}

		if len(objects) == 0 {
			break
		}

		for _, obj := range objects {
			// Delete from storage
			if obj.VersionID != "" && obj.VersionID != "null" {
				if err := store.DeleteVersionedObject(rule.BucketName, obj.Key, obj.VersionID); err != nil {
					logger.Error("lifecycle: failed to delete versioned object", "key", obj.Key, "error", err)
					continue
				}
				if err := database.DeleteObjectMetaByVersion(obj.BucketID, obj.Key, obj.VersionID); err != nil {
					logger.Error("lifecycle: failed to delete version metadata", "key", obj.Key, "error", err)
					continue
				}
			} else {
				if err := store.DeleteObject(rule.BucketName, obj.Key); err != nil {
					logger.Error("lifecycle: failed to delete object", "key", obj.Key, "error", err)
					continue
				}
				if err := database.DeleteObjectMeta(obj.BucketID, obj.Key); err != nil {
					logger.Error("lifecycle: failed to delete metadata", "key", obj.Key, "error", err)
					continue
				}
			}
			cleaned++
		}

		if len(objects) < 100 {
			break
		}
	}

	return cleaned
}
