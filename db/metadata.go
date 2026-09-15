package db

import (
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
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

// ErrInvalidContinuationToken is returned for a malformed ListObjectsV2 token.
var ErrInvalidContinuationToken = errors.New("invalid continuation token")

const objectColumns = "id, bucket_id, key, size, etag, content_type, last_modified, metadata, version_id, is_latest, is_delete_marker"

func scanObject(sc interface{ Scan(...interface{}) error }, m *ObjectMeta) error {
	return sc.Scan(&m.ID, &m.BucketID, &m.Key, &m.Size, &m.ETag, &m.ContentType, &m.LastModified, &m.Metadata,
		&m.VersionID, &m.IsLatest, &m.IsDeleteMarker)
}

// upsertObjectSQL inserts or replaces one (bucket, key, version) row.
const upsertObjectSQL = `
	INSERT INTO objects (bucket_id, key, size, etag, content_type, last_modified, metadata, version_id, is_latest, is_delete_marker)
	VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	ON CONFLICT(bucket_id, key, version_id) DO UPDATE SET
		size = excluded.size,
		etag = excluded.etag,
		content_type = excluded.content_type,
		last_modified = excluded.last_modified,
		metadata = excluded.metadata,
		is_latest = excluded.is_latest,
		is_delete_marker = excluded.is_delete_marker`

// PutObjectMeta upserts the row for an unversioned object. Any other rows for
// the same key are marked not-latest so a key never has two latest rows
// (this also repairs keys written by older WebDAV code).
func (d *DB) PutObjectMeta(meta *ObjectMeta) error {
	return d.PutObjectMetaVersioned(meta)
}

// PutObjectMetaVersioned atomically marks previous versions as not latest and inserts the new version.
func (d *DB) PutObjectMetaVersioned(meta *ObjectMeta) error {
	return d.WriteTx(context.Background(), func(tx *sql.Tx) error {
		_, err := tx.Exec("UPDATE objects SET is_latest = 0 WHERE bucket_id = ? AND key = ? AND version_id != ?", meta.BucketID, meta.Key, meta.VersionID)
		if err != nil {
			return err
		}
		_, err = tx.Exec(upsertObjectSQL, meta.BucketID, meta.Key, meta.Size, meta.ETag, meta.ContentType, meta.LastModified.UTC(), meta.Metadata,
			meta.VersionID, meta.IsLatest, meta.IsDeleteMarker)
		return err
	})
}

func (d *DB) GetObjectMeta(bucketID int64, key string) (*ObjectMeta, error) {
	m := &ObjectMeta{}
	err := scanObject(d.reader.QueryRow(`SELECT `+objectColumns+` FROM objects WHERE bucket_id = ? AND key = ? AND is_latest = 1 ORDER BY id DESC LIMIT 1`, bucketID, key), m)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return m, err
}

// GetObjectMetaByVersion retrieves a specific version of an object. The S3
// version id "null" also matches legacy rows stored with an empty version id.
func (d *DB) GetObjectMetaByVersion(bucketID int64, key, versionID string) (*ObjectMeta, error) {
	m := &ObjectMeta{}
	var err error
	if versionID == "null" {
		err = scanObject(d.reader.QueryRow(`SELECT `+objectColumns+` FROM objects WHERE bucket_id = ? AND key = ? AND version_id IN ('null', '') ORDER BY id DESC LIMIT 1`, bucketID, key), m)
	} else {
		err = scanObject(d.reader.QueryRow(`SELECT `+objectColumns+` FROM objects WHERE bucket_id = ? AND key = ? AND version_id = ?`, bucketID, key, versionID), m)
	}
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return m, err
}

// ListVersionsForKey returns every row for a key, newest first.
func (d *DB) ListVersionsForKey(bucketID int64, key string) ([]ObjectMeta, error) {
	rows, err := d.reader.Query(`SELECT `+objectColumns+` FROM objects WHERE bucket_id = ? AND key = ? ORDER BY id DESC`, bucketID, key)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ObjectMeta
	for rows.Next() {
		var m ObjectMeta
		if err := scanObject(rows, &m); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// MarkPreviousVersionsNotLatest sets is_latest=0 for all versions of a key.
func (d *DB) MarkPreviousVersionsNotLatest(bucketID int64, key string) error {
	return d.withRetry(func() error {
		_, err := d.writer.Exec("UPDATE objects SET is_latest = 0 WHERE bucket_id = ? AND key = ?", bucketID, key)
		return err
	})
}

// DeleteObjectMetaByVersion deletes a specific version of an object without
// promoting another version. Prefer DeleteVersionAndPromote.
func (d *DB) DeleteObjectMetaByVersion(bucketID int64, key, versionID string) error {
	return d.withRetry(func() error {
		_, err := d.writer.Exec("DELETE FROM objects WHERE bucket_id = ? AND key = ? AND version_id = ?", bucketID, key, versionID)
		return err
	})
}

// DeleteVersionAndPromote deletes one version row and, if it was the latest,
// marks the newest remaining row for the key as latest. Returns the deleted
// row (nil if it did not exist) and the promoted row (nil if none).
func (d *DB) DeleteVersionAndPromote(bucketID int64, key, versionID string) (deleted *ObjectMeta, promoted *ObjectMeta, err error) {
	err = d.WriteTx(context.Background(), func(tx *sql.Tx) error {
		m := &ObjectMeta{}
		var q string
		var args []interface{}
		if versionID == "null" {
			q = `SELECT ` + objectColumns + ` FROM objects WHERE bucket_id = ? AND key = ? AND version_id IN ('null', '') ORDER BY id DESC LIMIT 1`
			args = []interface{}{bucketID, key}
		} else {
			q = `SELECT ` + objectColumns + ` FROM objects WHERE bucket_id = ? AND key = ? AND version_id = ?`
			args = []interface{}{bucketID, key, versionID}
		}
		if err := scanObject(tx.QueryRow(q, args...), m); err != nil {
			if err == sql.ErrNoRows {
				return nil
			}
			return err
		}
		if _, err := tx.Exec("DELETE FROM objects WHERE id = ?", m.ID); err != nil {
			return err
		}
		deleted = m
		if !m.IsLatest {
			return nil
		}
		p := &ObjectMeta{}
		err := scanObject(tx.QueryRow(`SELECT `+objectColumns+` FROM objects WHERE bucket_id = ? AND key = ? ORDER BY id DESC LIMIT 1`, bucketID, key), p)
		if err == sql.ErrNoRows {
			return nil
		}
		if err != nil {
			return err
		}
		if _, err := tx.Exec("UPDATE objects SET is_latest = 1 WHERE id = ?", p.ID); err != nil {
			return err
		}
		p.IsLatest = true
		promoted = p
		return nil
	})
	return
}

// DeleteObjectMeta deletes every row (all versions) for a key.
func (d *DB) DeleteObjectMeta(bucketID int64, key string) error {
	return d.withRetry(func() error {
		_, err := d.writer.Exec("DELETE FROM objects WHERE bucket_id = ? AND key = ?", bucketID, key)
		return err
	})
}

// prefixUpperBound returns the smallest string greater than every string that
// has prefix p, and ok=false when no such bound exists (empty prefix or all 0xFF).
func prefixUpperBound(p string) (string, bool) {
	b := []byte(p)
	for i := len(b) - 1; i >= 0; i-- {
		if b[i] < 0xFF {
			b[i]++
			return string(b[:i+1]), true
		}
	}
	return "", false
}

// listBatch fetches up to limit current object rows with key in the range
// defined by (prefix, lower bound). inclusive selects "key >= lower" instead
// of "key > lower".
func (d *DB) listBatch(bucketID int64, prefix, lower string, inclusive bool, limit int) ([]ObjectMeta, error) {
	q := `SELECT ` + objectColumns + ` FROM objects WHERE bucket_id = ? AND is_latest = 1 AND is_delete_marker = 0`
	args := []interface{}{bucketID}
	if prefix != "" {
		q += " AND key >= ?"
		args = append(args, prefix)
		if ub, ok := prefixUpperBound(prefix); ok {
			q += " AND key < ?"
			args = append(args, ub)
		}
	}
	if lower != "" {
		if inclusive {
			q += " AND key >= ?"
		} else {
			q += " AND key > ?"
		}
		args = append(args, lower)
	}
	q += " ORDER BY key LIMIT ?"
	args = append(args, limit)

	rows, err := d.reader.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ObjectMeta
	for rows.Next() {
		var m ObjectMeta
		if err := scanObject(rows, &m); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// ListObjectsMeta implements S3 ListObjects semantics: keys are returned in
// byte order, keys sharing a delimiter-terminated prefix are rolled up into
// CommonPrefixes (each counting as one entry toward maxKeys), and pagination
// resumes after the last returned entry (object key or common prefix).
func (d *DB) ListObjectsMeta(bucketID int64, prefix, marker, delimiter string, maxKeys int) ([]ObjectMeta, []string, bool, string, error) {
	var objects []ObjectMeta
	var commonPrefixes []string
	if maxKeys < 0 {
		maxKeys = 0
	}

	lower := marker
	inclusive := false
	count := 0
	lastEntry := ""

	// A marker that is itself a common prefix (what we hand out as NextMarker
	// in delimiter listings) means "continue after everything under it".
	if delimiter != "" && marker != "" && strings.HasSuffix(marker, delimiter) && strings.HasPrefix(marker, prefix) {
		ub, ok := prefixUpperBound(marker)
		if !ok {
			return nil, nil, false, "", nil
		}
		lower, inclusive = ub, true
	}

	// Fetch in batches; a batch may be consumed entirely by one common prefix,
	// in which case we re-query from just past that prefix.
	const batchSize = 1000
	for count < maxKeys {
		batch, err := d.listBatch(bucketID, prefix, lower, inclusive, batchSize)
		if err != nil {
			return nil, nil, false, "", err
		}
		if len(batch) == 0 {
			break
		}
		skipping := "" // common prefix currently being skipped within this batch
		consumedAll := true
		for _, obj := range batch {
			if skipping != "" && strings.HasPrefix(obj.Key, skipping) {
				continue
			}
			skipping = ""
			if count >= maxKeys {
				consumedAll = false
				break
			}
			rest := obj.Key[len(prefix):]
			if delimiter != "" {
				if idx := strings.Index(rest, delimiter); idx >= 0 {
					cp := prefix + rest[:idx+len(delimiter)]
					commonPrefixes = append(commonPrefixes, cp)
					count++
					lastEntry = cp
					skipping = cp
					continue
				}
			}
			objects = append(objects, obj)
			count++
			lastEntry = obj.Key
		}
		if !consumedAll {
			break
		}
		// Position the next batch after the last entry we emitted.
		if skipping != "" {
			ub, ok := prefixUpperBound(skipping)
			if !ok {
				break
			}
			lower, inclusive = ub, true
		} else {
			lower, inclusive = batch[len(batch)-1].Key, false
		}
		if len(batch) < batchSize && skipping == "" {
			break
		}
	}

	// Determine truncation: is there any entry after the last one we emitted?
	isTruncated := false
	switch {
	case maxKeys == 0:
		more, err := d.listBatch(bucketID, prefix, lower, inclusive, 1)
		if err != nil {
			return nil, nil, false, "", err
		}
		isTruncated = len(more) > 0
	case count >= maxKeys && lastEntry != "":
		probeLower, probeInclusive, probe := lastEntry, false, true
		if len(commonPrefixes) > 0 && commonPrefixes[len(commonPrefixes)-1] == lastEntry {
			if ub, ok := prefixUpperBound(lastEntry); ok {
				probeLower, probeInclusive = ub, true
			} else {
				probe = false // nothing can sort after an all-0xFF prefix
			}
		}
		if probe {
			more, err := d.listBatch(bucketID, prefix, probeLower, probeInclusive, 1)
			if err != nil {
				return nil, nil, false, "", err
			}
			isTruncated = len(more) > 0
		}
	}

	nextMarker := ""
	if isTruncated {
		nextMarker = lastEntry
	}
	return objects, commonPrefixes, isTruncated, nextMarker, nil
}

func (d *DB) ListObjectsMetaV2(bucketID int64, prefix, startAfter, continuationToken, delimiter string, maxKeys int) ([]ObjectMeta, []string, bool, string, error) {
	marker := startAfter
	if continuationToken != "" {
		decoded, err := base64.StdEncoding.DecodeString(continuationToken)
		if err != nil {
			return nil, nil, false, "", ErrInvalidContinuationToken
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

// ListObjectVersions lists all versions of objects in a bucket ordered by key,
// newest version first (S3 order). Pagination resumes after (keyMarker, versionMarker).
func (d *DB) ListObjectVersions(bucketID int64, prefix, keyMarker, versionMarker string, maxKeys int) ([]ObjectMeta, bool, error) {
	q := `SELECT ` + objectColumns + ` FROM objects WHERE bucket_id = ?`
	args := []interface{}{bucketID}
	if prefix != "" {
		q += " AND key >= ?"
		args = append(args, prefix)
		if ub, ok := prefixUpperBound(prefix); ok {
			q += " AND key < ?"
			args = append(args, ub)
		}
	}
	if keyMarker != "" {
		if versionMarker != "" {
			vm := versionMarker
			if vm == "null" {
				vm = ""
			}
			q += ` AND (key > ? OR (key = ? AND id < COALESCE((SELECT id FROM objects WHERE bucket_id = ? AND key = ? AND version_id IN (?, 'null') ORDER BY id DESC LIMIT 1), 0)))`
			args = append(args, keyMarker, keyMarker, bucketID, keyMarker, vm)
		} else {
			q += " AND key > ?"
			args = append(args, keyMarker)
		}
	}
	q += " ORDER BY key, id DESC LIMIT ?"
	args = append(args, maxKeys+1)

	rows, err := d.reader.Query(q, args...)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()

	var objects []ObjectMeta
	for rows.Next() {
		var m ObjectMeta
		if err := scanObject(rows, &m); err != nil {
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

// GetExpiredObjects returns current, non-marker objects under prefix whose
// last_modified is older than expirationDays, with id > afterID, ordered by id.
func (d *DB) GetExpiredObjects(bucketName, prefix string, expirationDays int, afterID int64, limit int) ([]ObjectMeta, error) {
	q := `SELECT o.id, o.bucket_id, o.key, o.size, o.etag, o.content_type, o.last_modified, o.metadata, o.version_id, o.is_latest, o.is_delete_marker
		FROM objects o JOIN buckets b ON o.bucket_id = b.id
		WHERE b.name = ? AND o.is_latest = 1 AND o.is_delete_marker = 0 AND o.id > ?
		AND o.last_modified < datetime('now', '-' || ? || ' days')`
	args := []interface{}{bucketName, afterID, expirationDays}
	if prefix != "" {
		q += " AND o.key >= ?"
		args = append(args, prefix)
		if ub, ok := prefixUpperBound(prefix); ok {
			q += " AND o.key < ?"
			args = append(args, ub)
		}
	}
	q += " ORDER BY o.id LIMIT ?"
	args = append(args, limit)
	return d.queryObjects(q, args...)
}

// GetExpiredNoncurrentVersions returns non-latest versions (including delete
// markers) under prefix older than days, with id > afterID.
func (d *DB) GetExpiredNoncurrentVersions(bucketName, prefix string, days int, afterID int64, limit int) ([]ObjectMeta, error) {
	q := `SELECT o.id, o.bucket_id, o.key, o.size, o.etag, o.content_type, o.last_modified, o.metadata, o.version_id, o.is_latest, o.is_delete_marker
		FROM objects o JOIN buckets b ON o.bucket_id = b.id
		WHERE b.name = ? AND o.is_latest = 0 AND o.id > ?
		AND o.last_modified < datetime('now', '-' || ? || ' days')`
	args := []interface{}{bucketName, afterID, days}
	if prefix != "" {
		q += " AND o.key >= ?"
		args = append(args, prefix)
		if ub, ok := prefixUpperBound(prefix); ok {
			q += " AND o.key < ?"
			args = append(args, ub)
		}
	}
	q += " ORDER BY o.id LIMIT ?"
	args = append(args, limit)
	return d.queryObjects(q, args...)
}

// GetOrphanDeleteMarkers returns latest delete markers with no other versions
// left for the key (safe to remove), with id > afterID.
func (d *DB) GetOrphanDeleteMarkers(bucketName, prefix string, afterID int64, limit int) ([]ObjectMeta, error) {
	q := `SELECT o.id, o.bucket_id, o.key, o.size, o.etag, o.content_type, o.last_modified, o.metadata, o.version_id, o.is_latest, o.is_delete_marker
		FROM objects o JOIN buckets b ON o.bucket_id = b.id
		WHERE b.name = ? AND o.is_latest = 1 AND o.is_delete_marker = 1 AND o.id > ?
		AND NOT EXISTS (SELECT 1 FROM objects x WHERE x.bucket_id = o.bucket_id AND x.key = o.key AND x.id != o.id)`
	args := []interface{}{bucketName, afterID}
	if prefix != "" {
		q += " AND o.key >= ?"
		args = append(args, prefix)
		if ub, ok := prefixUpperBound(prefix); ok {
			q += " AND o.key < ?"
			args = append(args, ub)
		}
	}
	q += " ORDER BY o.id LIMIT ?"
	args = append(args, limit)
	return d.queryObjects(q, args...)
}

func (d *DB) queryObjects(q string, args ...interface{}) ([]ObjectMeta, error) {
	rows, err := d.reader.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var objects []ObjectMeta
	for rows.Next() {
		var m ObjectMeta
		if err := scanObject(rows, &m); err != nil {
			return nil, err
		}
		objects = append(objects, m)
	}
	return objects, rows.Err()
}

// IterateObjects calls fn for every row (all versions) in id order, in batches.
// Used by maintenance tasks such as the storage layout migration.
func (d *DB) IterateObjects(batch int, fn func(m ObjectMeta) error) error {
	var afterID int64
	for {
		objs, err := d.queryObjects(`SELECT `+objectColumns+` FROM objects WHERE id > ? ORDER BY id LIMIT ?`, afterID, batch)
		if err != nil {
			return err
		}
		if len(objs) == 0 {
			return nil
		}
		for _, m := range objs {
			if err := fn(m); err != nil {
				return err
			}
			afterID = m.ID
		}
	}
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

// DeleteMultipartUpload removes the upload (and its parts via cascade).
// Returns true if a row was deleted.
func (d *DB) DeleteMultipartUpload(uploadID string) (bool, error) {
	var n int64
	err := d.withRetry(func() error {
		res, err := d.writer.Exec("DELETE FROM multipart_uploads WHERE id = ?", uploadID)
		if err != nil {
			return err
		}
		n, _ = res.RowsAffected()
		return nil
	})
	return n > 0, err
}

// ListMultipartUploads returns active multipart uploads for a bucket.
func (d *DB) ListMultipartUploads(bucketID int64, prefix, keyMarker, uploadIDMarker string, maxUploads int) ([]MultipartUpload, bool, error) {
	q := `SELECT id, bucket_id, key, content_type, metadata, created_at FROM multipart_uploads WHERE bucket_id = ?`
	args := []interface{}{bucketID}
	if prefix != "" {
		q += " AND key >= ?"
		args = append(args, prefix)
		if ub, ok := prefixUpperBound(prefix); ok {
			q += " AND key < ?"
			args = append(args, ub)
		}
	}
	if keyMarker != "" {
		if uploadIDMarker != "" {
			q += " AND (key > ? OR (key = ? AND id > ?))"
			args = append(args, keyMarker, keyMarker, uploadIDMarker)
		} else {
			q += " AND key > ?"
			args = append(args, keyMarker)
		}
	}
	q += " ORDER BY key, id LIMIT ?"
	args = append(args, maxUploads+1)
	rows, err := d.reader.Query(q, args...)
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
	return d.listStaleUploads(`SELECT id, bucket_id, key, content_type, metadata, created_at FROM multipart_uploads WHERE created_at < ?`, time.Now().UTC().Add(-olderThan))
}

// ListStaleMultipartUploadsForBucket is the per-bucket, prefix-scoped variant
// used by lifecycle AbortIncompleteMultipartUpload rules.
func (d *DB) ListStaleMultipartUploadsForBucket(bucketID int64, prefix string, olderThan time.Duration) ([]MultipartUpload, error) {
	q := `SELECT id, bucket_id, key, content_type, metadata, created_at FROM multipart_uploads WHERE bucket_id = ? AND created_at < ?`
	args := []interface{}{bucketID, time.Now().UTC().Add(-olderThan)}
	if prefix != "" {
		q += " AND key >= ?"
		args = append(args, prefix)
		if ub, ok := prefixUpperBound(prefix); ok {
			q += " AND key < ?"
			args = append(args, ub)
		}
	}
	return d.listStaleUploads(q, args...)
}

func (d *DB) listStaleUploads(q string, args ...interface{}) ([]MultipartUpload, error) {
	rows, err := d.reader.Query(q, args...)
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
