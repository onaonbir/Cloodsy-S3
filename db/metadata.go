package db

import (
	"context"
	"database/sql"
	"encoding/base64"
	"fmt"
	"strings"
	"time"
)

type ObjectMeta struct {
	ID             int64
	BucketID       int64
	Key            string
	Size           int64
	ETag           string
	ContentType    string
	LastModified   time.Time
	Metadata       string // JSON
	VersionID      string
	IsLatest       bool
	IsDeleteMarker bool
}

func (d *DB) PutObjectMeta(meta *ObjectMeta) error {
	return d.withRetry(func() error {
		_, err := d.writer.Exec(`
			INSERT INTO objects (bucket_id, key, size, etag, content_type, last_modified, metadata, version_id, is_latest, is_delete_marker)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(bucket_id, key, version_id) DO UPDATE SET
				size = excluded.size,
				etag = excluded.etag,
				content_type = excluded.content_type,
				last_modified = excluded.last_modified,
				metadata = excluded.metadata,
				is_latest = excluded.is_latest,
				is_delete_marker = excluded.is_delete_marker
		`, meta.BucketID, meta.Key, meta.Size, meta.ETag, meta.ContentType, meta.LastModified, meta.Metadata,
			meta.VersionID, meta.IsLatest, meta.IsDeleteMarker)
		return err
	})
}

// PutObjectMetaVersioned atomically marks previous versions as not latest and inserts the new version.
func (d *DB) PutObjectMetaVersioned(meta *ObjectMeta) error {
	return d.WriteTx(context.Background(), func(tx *sql.Tx) error {
		_, err := tx.Exec("UPDATE objects SET is_latest = 0 WHERE bucket_id = ? AND key = ?", meta.BucketID, meta.Key)
		if err != nil {
			return err
		}
		_, err = tx.Exec(`
			INSERT INTO objects (bucket_id, key, size, etag, content_type, last_modified, metadata, version_id, is_latest, is_delete_marker)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(bucket_id, key, version_id) DO UPDATE SET
				size = excluded.size,
				etag = excluded.etag,
				content_type = excluded.content_type,
				last_modified = excluded.last_modified,
				metadata = excluded.metadata,
				is_latest = excluded.is_latest,
				is_delete_marker = excluded.is_delete_marker
		`, meta.BucketID, meta.Key, meta.Size, meta.ETag, meta.ContentType, meta.LastModified, meta.Metadata,
			meta.VersionID, meta.IsLatest, meta.IsDeleteMarker)
		return err
	})
}

func (d *DB) GetObjectMeta(bucketID int64, key string) (*ObjectMeta, error) {
	m := &ObjectMeta{}
	err := d.reader.QueryRow(`
		SELECT id, bucket_id, key, size, etag, content_type, last_modified, metadata, version_id, is_latest, is_delete_marker
		FROM objects WHERE bucket_id = ? AND key = ? AND is_latest = 1
	`, bucketID, key).Scan(&m.ID, &m.BucketID, &m.Key, &m.Size, &m.ETag, &m.ContentType, &m.LastModified, &m.Metadata,
		&m.VersionID, &m.IsLatest, &m.IsDeleteMarker)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return m, err
}

// GetObjectMetaByVersion retrieves a specific version of an object.
func (d *DB) GetObjectMetaByVersion(bucketID int64, key, versionID string) (*ObjectMeta, error) {
	m := &ObjectMeta{}
	err := d.reader.QueryRow(`
		SELECT id, bucket_id, key, size, etag, content_type, last_modified, metadata, version_id, is_latest, is_delete_marker
		FROM objects WHERE bucket_id = ? AND key = ? AND version_id = ?
	`, bucketID, key, versionID).Scan(&m.ID, &m.BucketID, &m.Key, &m.Size, &m.ETag, &m.ContentType, &m.LastModified, &m.Metadata,
		&m.VersionID, &m.IsLatest, &m.IsDeleteMarker)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return m, err
}

// MarkPreviousVersionsNotLatest sets is_latest=0 for all versions of a key.
func (d *DB) MarkPreviousVersionsNotLatest(bucketID int64, key string) error {
	return d.withRetry(func() error {
		_, err := d.writer.Exec("UPDATE objects SET is_latest = 0 WHERE bucket_id = ? AND key = ?", bucketID, key)
		return err
	})
}

// DeleteObjectMetaByVersion deletes a specific version of an object.
func (d *DB) DeleteObjectMetaByVersion(bucketID int64, key, versionID string) error {
	return d.withRetry(func() error {
		_, err := d.writer.Exec("DELETE FROM objects WHERE bucket_id = ? AND key = ? AND version_id = ?", bucketID, key, versionID)
		return err
	})
}

// ListObjectVersions lists all versions of objects in a bucket.
func (d *DB) ListObjectVersions(bucketID int64, prefix, keyMarker, versionMarker string, maxKeys int) ([]ObjectMeta, bool, error) {
	query := `SELECT id, bucket_id, key, size, etag, content_type, last_modified, metadata, version_id, is_latest, is_delete_marker
		FROM objects WHERE bucket_id = ? AND key LIKE ? ESCAPE '\'`
	args := []interface{}{bucketID}

	escapedPrefix := strings.ReplaceAll(prefix, "%", `\%`)
	escapedPrefix = strings.ReplaceAll(escapedPrefix, "_", `\_`)
	args = append(args, escapedPrefix+"%")

	if keyMarker != "" {
		if versionMarker != "" {
			query += " AND (key > ? OR (key = ? AND version_id > ?))"
			args = append(args, keyMarker, keyMarker, versionMarker)
		} else {
			query += " AND key > ?"
			args = append(args, keyMarker)
		}
	}

	query += " ORDER BY key, version_id LIMIT ?"
	args = append(args, maxKeys+1)

	rows, err := d.reader.Query(query, args...)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()

	var objects []ObjectMeta
	for rows.Next() {
		var m ObjectMeta
		if err := rows.Scan(&m.ID, &m.BucketID, &m.Key, &m.Size, &m.ETag, &m.ContentType, &m.LastModified, &m.Metadata,
			&m.VersionID, &m.IsLatest, &m.IsDeleteMarker); err != nil {
			return nil, false, err
		}
		objects = append(objects, m)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}

	isTruncated := len(objects) > maxKeys
	if isTruncated {
		objects = objects[:maxKeys]
	}

	return objects, isTruncated, nil
}

// GetExpiredObjects returns objects matching a lifecycle rule that have expired.
func (d *DB) GetExpiredObjects(bucketName, prefix string, expirationDays int, limit int) ([]ObjectMeta, error) {
	escapedPrefix := strings.ReplaceAll(prefix, "%", `\%`)
	escapedPrefix = strings.ReplaceAll(escapedPrefix, "_", `\_`)

	rows, err := d.reader.Query(`
		SELECT o.id, o.bucket_id, o.key, o.size, o.etag, o.content_type, o.last_modified, o.metadata, o.version_id, o.is_latest, o.is_delete_marker
		FROM objects o
		JOIN buckets b ON o.bucket_id = b.id
		WHERE b.name = ? AND o.key LIKE ? ESCAPE '\' AND o.is_latest = 1 AND o.is_delete_marker = 0
		AND o.last_modified < datetime('now', '-' || ? || ' days')
		LIMIT ?
	`, bucketName, escapedPrefix+"%", expirationDays, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var objects []ObjectMeta
	for rows.Next() {
		var m ObjectMeta
		if err := rows.Scan(&m.ID, &m.BucketID, &m.Key, &m.Size, &m.ETag, &m.ContentType, &m.LastModified, &m.Metadata,
			&m.VersionID, &m.IsLatest, &m.IsDeleteMarker); err != nil {
			return nil, err
		}
		objects = append(objects, m)
	}
	return objects, rows.Err()
}

func (d *DB) DeleteObjectMeta(bucketID int64, key string) error {
	return d.withRetry(func() error {
		_, err := d.writer.Exec("DELETE FROM objects WHERE bucket_id = ? AND key = ?", bucketID, key)
		return err
	})
}

func (d *DB) ListObjectsMeta(bucketID int64, prefix, marker, delimiter string, maxKeys int) ([]ObjectMeta, []string, bool, string, error) {
	// Get all matching objects (only latest, non-delete-marker)
	query := `SELECT id, bucket_id, key, size, etag, content_type, last_modified, metadata, version_id, is_latest, is_delete_marker
		FROM objects WHERE bucket_id = ? AND key LIKE ? ESCAPE '\' AND key > ? AND is_latest = 1 AND is_delete_marker = 0 ORDER BY key LIMIT ?`

	escapedPrefix := strings.ReplaceAll(prefix, "%", `\%`)
	escapedPrefix = strings.ReplaceAll(escapedPrefix, "_", `\_`)
	likePrefix := escapedPrefix + "%"
	rows, err := d.reader.Query(query, bucketID, likePrefix, marker, maxKeys+1)
	if err != nil {
		return nil, nil, false, "", err
	}
	defer rows.Close()

	var allObjects []ObjectMeta
	for rows.Next() {
		var m ObjectMeta
		if err := rows.Scan(&m.ID, &m.BucketID, &m.Key, &m.Size, &m.ETag, &m.ContentType, &m.LastModified, &m.Metadata,
			&m.VersionID, &m.IsLatest, &m.IsDeleteMarker); err != nil {
			return nil, nil, false, "", err
		}
		allObjects = append(allObjects, m)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, false, "", err
	}

	// Process delimiter
	if delimiter == "" {
		isTruncated := len(allObjects) > maxKeys
		if isTruncated {
			allObjects = allObjects[:maxKeys]
		}
		nextMarker := ""
		if isTruncated && len(allObjects) > 0 {
			nextMarker = allObjects[len(allObjects)-1].Key
		}
		return allObjects, nil, isTruncated, nextMarker, nil
	}

	// With delimiter, group into common prefixes
	var objects []ObjectMeta
	commonPrefixSet := make(map[string]bool)
	prefixLen := len(prefix)

	for _, obj := range allObjects {
		rest := obj.Key[prefixLen:]
		delimIdx := indexOf(rest, delimiter)
		if delimIdx >= 0 {
			cp := prefix + rest[:delimIdx+len(delimiter)]
			commonPrefixSet[cp] = true
		} else {
			objects = append(objects, obj)
		}
	}

	var commonPrefixes []string
	for cp := range commonPrefixSet {
		commonPrefixes = append(commonPrefixes, cp)
	}
	// Sort common prefixes
	sortStrings(commonPrefixes)

	totalCount := len(objects) + len(commonPrefixes)
	isTruncated := totalCount > maxKeys

	// Truncate to maxKeys
	if isTruncated {
		// Simple truncation: we limit the result set
		if len(objects) > maxKeys {
			objects = objects[:maxKeys]
		}
	}

	nextMarker := ""
	if isTruncated && len(objects) > 0 {
		nextMarker = objects[len(objects)-1].Key
	}

	return objects, commonPrefixes, isTruncated, nextMarker, nil
}

func (d *DB) ListObjectsMetaV2(bucketID int64, prefix, startAfter, continuationToken, delimiter string, maxKeys int) ([]ObjectMeta, []string, bool, string, error) {
	marker := startAfter
	if continuationToken != "" {
		decoded, err := base64.StdEncoding.DecodeString(continuationToken)
		if err != nil {
			return nil, nil, false, "", fmt.Errorf("invalid continuation token: %w", err)
		}
		marker = string(decoded)
	}
	objects, commonPrefixes, isTruncated, nextMarker, err := d.ListObjectsMeta(bucketID, prefix, marker, delimiter, maxKeys)
	if err != nil {
		return nil, nil, false, "", err
	}
	nextToken := ""
	if isTruncated && nextMarker != "" {
		nextToken = base64.StdEncoding.EncodeToString([]byte(nextMarker))
	}
	return objects, commonPrefixes, isTruncated, nextToken, nil
}

func (d *DB) CountObjects(bucketID int64) (int64, error) {
	var count int64
	err := d.reader.QueryRow("SELECT COUNT(*) FROM objects WHERE bucket_id = ? AND is_latest = 1 AND is_delete_marker = 0", bucketID).Scan(&count)
	return count, err
}

// Multipart operations

type MultipartUpload struct {
	ID          string
	BucketID    int64
	Key         string
	ContentType string
	Metadata    string
	CreatedAt   time.Time
}

type MultipartPart struct {
	UploadID   string
	PartNumber int
	Size       int64
	ETag       string
	CreatedAt  time.Time
}

func (d *DB) CreateMultipartUpload(upload *MultipartUpload) error {
	return d.withRetry(func() error {
		_, err := d.writer.Exec(`
			INSERT INTO multipart_uploads (id, bucket_id, key, content_type, metadata)
			VALUES (?, ?, ?, ?, ?)
		`, upload.ID, upload.BucketID, upload.Key, upload.ContentType, upload.Metadata)
		return err
	})
}

func (d *DB) GetMultipartUpload(uploadID string) (*MultipartUpload, error) {
	u := &MultipartUpload{}
	err := d.reader.QueryRow(`
		SELECT id, bucket_id, key, content_type, metadata, created_at
		FROM multipart_uploads WHERE id = ?
	`, uploadID).Scan(&u.ID, &u.BucketID, &u.Key, &u.ContentType, &u.Metadata, &u.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return u, err
}

func (d *DB) PutMultipartPart(part *MultipartPart) error {
	return d.withRetry(func() error {
		_, err := d.writer.Exec(`
			INSERT INTO multipart_parts (upload_id, part_number, size, etag)
			VALUES (?, ?, ?, ?)
			ON CONFLICT(upload_id, part_number) DO UPDATE SET
				size = excluded.size,
				etag = excluded.etag,
				created_at = CURRENT_TIMESTAMP
		`, part.UploadID, part.PartNumber, part.Size, part.ETag)
		return err
	})
}

func (d *DB) ListMultipartParts(uploadID string) ([]MultipartPart, error) {
	rows, err := d.reader.Query(`
		SELECT upload_id, part_number, size, etag, created_at
		FROM multipart_parts WHERE upload_id = ? ORDER BY part_number
	`, uploadID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var parts []MultipartPart
	for rows.Next() {
		var p MultipartPart
		if err := rows.Scan(&p.UploadID, &p.PartNumber, &p.Size, &p.ETag, &p.CreatedAt); err != nil {
			return nil, err
		}
		parts = append(parts, p)
	}
	return parts, rows.Err()
}

func (d *DB) DeleteMultipartUpload(uploadID string) error {
	return d.withRetry(func() error {
		_, err := d.writer.Exec("DELETE FROM multipart_uploads WHERE id = ?", uploadID)
		return err
	})
}

// ListMultipartUploads returns active multipart uploads for a bucket.
func (d *DB) ListMultipartUploads(bucketID int64, prefix, keyMarker, uploadIDMarker string, maxUploads int) ([]MultipartUpload, bool, error) {
	query := `SELECT id, bucket_id, key, content_type, metadata, created_at
		FROM multipart_uploads WHERE bucket_id = ? AND key LIKE ? AND (key > ? OR (key = ? AND id > ?))
		ORDER BY key, id LIMIT ?`
	escapedPrefix := strings.ReplaceAll(prefix, "%", `\%`)
	escapedPrefix = strings.ReplaceAll(escapedPrefix, "_", `\_`)
	rows, err := d.reader.Query(query, bucketID, escapedPrefix+"%", keyMarker, keyMarker, uploadIDMarker, maxUploads+1)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()

	var uploads []MultipartUpload
	for rows.Next() {
		var u MultipartUpload
		if err := rows.Scan(&u.ID, &u.BucketID, &u.Key, &u.ContentType, &u.Metadata, &u.CreatedAt); err != nil {
			return nil, false, err
		}
		uploads = append(uploads, u)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	truncated := len(uploads) > maxUploads
	if truncated {
		uploads = uploads[:maxUploads]
	}
	return uploads, truncated, nil
}

// ListStaleMultipartUploads returns uploads older than the given duration.
func (d *DB) ListStaleMultipartUploads(olderThan time.Duration) ([]MultipartUpload, error) {
	cutoff := time.Now().Add(-olderThan)
	rows, err := d.reader.Query(`
		SELECT id, bucket_id, key, content_type, metadata, created_at
		FROM multipart_uploads WHERE created_at < ?
	`, cutoff)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var uploads []MultipartUpload
	for rows.Next() {
		var u MultipartUpload
		if err := rows.Scan(&u.ID, &u.BucketID, &u.Key, &u.ContentType, &u.Metadata, &u.CreatedAt); err != nil {
			return nil, err
		}
		uploads = append(uploads, u)
	}
	return uploads, rows.Err()
}

// Helper functions

func indexOf(s, substr string) int {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return i
		}
	}
	return -1
}

func sortStrings(s []string) {
	// Simple insertion sort for small slices
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}
