package db

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

// Queries backing the Admin API's object explorer, multipart, lifecycle,
// webhook and maintenance endpoints.

// ErrConflict reports a uniqueness violation (e.g. two lifecycle rules for
// one prefix).
var ErrConflict = errors.New("conflict")

func isUniqueViolation(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}

// EscapeLike escapes the LIKE wildcards (and the escape character "\") so s
// matches literally inside a pattern that uses ESCAPE '\'.
func EscapeLike(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s)
}

// asciiLower lower-cases ASCII letters only, matching SQLite's built-in
// LOWER() so that a lowered pattern and LOWER(key) agree on non-ASCII text.
func asciiLower(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + 'a' - 'A'
		}
	}
	return string(b)
}

// SearchObjects lists current, non-marker objects under prefix whose key
// contains q (ASCII case-insensitive), in key order after marker. It returns
// at most limit rows and whether more exist. The filter runs in SQL so
// pagination stays exact.
func (d *DB) SearchObjects(bucketID int64, prefix, q, marker string, limit int) ([]ObjectMeta, bool, error) {
	query := `SELECT ` + objectColumns + ` FROM objects WHERE bucket_id = ? AND is_latest = 1 AND is_delete_marker = 0`
	args := []interface{}{bucketID}
	if prefix != "" {
		query += " AND key >= ?"
		args = append(args, prefix)
		if ub, ok := prefixUpperBound(prefix); ok {
			query += " AND key < ?"
			args = append(args, ub)
		}
	}
	if marker != "" {
		query += " AND key > ?"
		args = append(args, marker)
	}
	if q != "" {
		query += ` AND LOWER(key) LIKE ? ESCAPE '\'`
		args = append(args, "%"+EscapeLike(asciiLower(q))+"%")
	}
	query += " ORDER BY key LIMIT ?"
	args = append(args, limit+1)
	objs, err := d.queryObjects(query, args...)
	if err != nil {
		return nil, false, err
	}
	truncated := len(objs) > limit
	if truncated {
		objs = objs[:limit]
	}
	return objs, truncated, nil
}

// PrefixStats counts current, non-marker objects under prefix and sums their sizes.
func (d *DB) PrefixStats(bucketID int64, prefix string) (objects int64, bytes int64, err error) {
	query := `SELECT COUNT(*), COALESCE(SUM(size), 0) FROM objects WHERE bucket_id = ? AND is_latest = 1 AND is_delete_marker = 0`
	args := []interface{}{bucketID}
	if prefix != "" {
		query += " AND key >= ?"
		args = append(args, prefix)
		if ub, ok := prefixUpperBound(prefix); ok {
			query += " AND key < ?"
			args = append(args, ub)
		}
	}
	err = d.reader.QueryRow(query, args...).Scan(&objects, &bytes)
	return
}

// MultipartUploadStats is an incomplete upload with its staged part totals.
type MultipartUploadStats struct {
	MultipartUpload
	Parts int
	Bytes int64
}

// ListMultipartUploadsWithStats returns every incomplete upload of a bucket
// (oldest first) with its part count and staged bytes.
func (d *DB) ListMultipartUploadsWithStats(bucketID int64) ([]MultipartUploadStats, error) {
	rows, err := d.reader.Query(`
		SELECT u.id, u.bucket_id, u.key, COALESCE(u.content_type, ''), COALESCE(u.metadata, '{}'), u.created_at,
			COUNT(p.part_number), COALESCE(SUM(p.size), 0)
		FROM multipart_uploads u LEFT JOIN multipart_parts p ON p.upload_id = u.id
		WHERE u.bucket_id = ?
		GROUP BY u.id
		ORDER BY u.created_at, u.id`, bucketID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MultipartUploadStats
	for rows.Next() {
		var u MultipartUploadStats
		if err := rows.Scan(&u.ID, &u.BucketID, &u.Key, &u.ContentType, &u.Metadata, &u.CreatedAt, &u.Parts, &u.Bytes); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// IterateBucketObjects calls fn for every row (all versions, markers
// included) of one bucket in id order. fn returning false stops the walk.
func (d *DB) IterateBucketObjects(bucketID int64, batch int, fn func(m ObjectMeta) bool) error {
	var afterID int64
	for {
		objs, err := d.queryObjects(`SELECT `+objectColumns+` FROM objects WHERE bucket_id = ? AND id > ? ORDER BY id LIMIT ?`, bucketID, afterID, batch)
		if err != nil {
			return err
		}
		if len(objs) == 0 {
			return nil
		}
		for _, m := range objs {
			if !fn(m) {
				return nil
			}
			afterID = m.ID
		}
	}
}

// --- lifecycle rules by id ---------------------------------------------------

// GetLifecycleRuleByID returns one rule, or nil when it does not exist.
func (d *DB) GetLifecycleRuleByID(id int64) (*LifecycleRule, error) {
	r := &LifecycleRule{}
	err := scanLifecycle(d.reader.QueryRow(`SELECT `+lifecycleColumns+` FROM lifecycle_rules WHERE id = ?`, id), r)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return r, err
}

// UpdateLifecycleRule overwrites every field of rule r.ID. It returns
// ErrConflict when another rule of the bucket already uses r.Prefix.
func (d *DB) UpdateLifecycleRule(r LifecycleRule) error {
	if r.Status == "" {
		r.Status = "Enabled"
	}
	err := d.withRetry(func() error {
		_, err := d.writer.Exec(`UPDATE lifecycle_rules SET name = ?, prefix = ?, status = ?, expiration_days = ?,
			noncurrent_days = ?, abort_multipart_days = ?, expire_delete_markers = ? WHERE id = ? AND bucket_name = ?`,
			r.Name, r.Prefix, r.Status, r.ExpirationDays, r.NoncurrentDays, r.AbortMultipartDays, r.ExpireDeleteMarkers, r.ID, r.BucketName)
		return err
	})
	if isUniqueViolation(err) {
		return ErrConflict
	}
	return err
}

// DeleteLifecycleRuleByID removes one rule of a bucket and reports whether it existed.
func (d *DB) DeleteLifecycleRuleByID(bucketName string, id int64) (bool, error) {
	var n int64
	err := d.withRetry(func() error {
		res, err := d.writer.Exec("DELETE FROM lifecycle_rules WHERE id = ? AND bucket_name = ?", id, bucketName)
		if err != nil {
			return err
		}
		n, _ = res.RowsAffected()
		return nil
	})
	return n > 0, err
}

// --- webhooks ------------------------------------------------------------------

// GetWebhook returns one webhook (secret included), or nil.
func (d *DB) GetWebhook(id int64) (*BucketWebhook, error) {
	h := &BucketWebhook{}
	err := d.reader.QueryRow(`SELECT id, bucket_name, name, url, event_types, secret, active, created_at FROM bucket_webhooks WHERE id = ?`, id).
		Scan(&h.ID, &h.BucketName, &h.Name, &h.URL, &h.EventTypes, &h.Secret, &h.Active, &h.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return h, err
}

// UpdateWebhook overwrites name, url, event types and active flag; the
// secret is replaced only when secret is non-nil.
func (d *DB) UpdateWebhook(id int64, name, url, eventTypes string, active bool, secret *string) error {
	if eventTypes == "" {
		eventTypes = "*"
	}
	return d.withRetry(func() error {
		if secret != nil {
			_, err := d.writer.Exec(`UPDATE bucket_webhooks SET name = ?, url = ?, event_types = ?, active = ?, secret = ? WHERE id = ?`,
				name, url, eventTypes, active, *secret, id)
			return err
		}
		_, err := d.writer.Exec(`UPDATE bucket_webhooks SET name = ?, url = ?, event_types = ?, active = ? WHERE id = ?`,
			name, url, eventTypes, active, id)
		return err
	})
}

// WebhookDeliveryKeep is how many delivery records are kept per webhook.
const WebhookDeliveryKeep = 200

// WebhookDelivery is one recorded delivery attempt.
type WebhookDelivery struct {
	ID         int64
	WebhookID  int64
	EventType  string
	ObjectKey  string
	StatusCode int
	Error      string
	DurationMs int64
	Attempt    int
	CreatedAt  time.Time
}

// InsertWebhookDelivery records one attempt and prunes the hook's log to the
// newest WebhookDeliveryKeep rows. Deliveries for a webhook that no longer
// exists are silently dropped.
func (d *DB) InsertWebhookDelivery(r WebhookDelivery) error {
	if r.CreatedAt.IsZero() {
		r.CreatedAt = time.Now()
	}
	err := d.withRetry(func() error {
		_, err := d.writer.Exec(`INSERT INTO webhook_deliveries (webhook_id, event_type, object_key, status_code, error, duration_ms, attempt, created_at)
			SELECT ?, ?, ?, ?, ?, ?, ?, ? WHERE EXISTS (SELECT 1 FROM bucket_webhooks WHERE id = ?)`,
			r.WebhookID, r.EventType, r.ObjectKey, r.StatusCode, r.Error, r.DurationMs, r.Attempt, r.CreatedAt.UTC(), r.WebhookID)
		if err != nil {
			return err
		}
		_, err = d.writer.Exec(`DELETE FROM webhook_deliveries WHERE webhook_id = ? AND id <= (
			SELECT id FROM webhook_deliveries WHERE webhook_id = ? ORDER BY id DESC LIMIT 1 OFFSET ?)`,
			r.WebhookID, r.WebhookID, WebhookDeliveryKeep)
		return err
	})
	return err
}

// ListWebhookDeliveries returns the newest deliveries of a hook, newest first.
func (d *DB) ListWebhookDeliveries(webhookID int64, limit int) ([]WebhookDelivery, error) {
	rows, err := d.reader.Query(`SELECT id, webhook_id, event_type, object_key, status_code, error, duration_ms, attempt, created_at
		FROM webhook_deliveries WHERE webhook_id = ? ORDER BY id DESC LIMIT ?`, webhookID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []WebhookDelivery
	for rows.Next() {
		var r WebhookDelivery
		if err := rows.Scan(&r.ID, &r.WebhookID, &r.EventType, &r.ObjectKey, &r.StatusCode, &r.Error, &r.DurationMs, &r.Attempt, &r.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// DeleteObjectRows deletes the given rows (by row id) of one key in a single
// transaction and, when the latest row was among them, marks the newest
// remaining row of the key as latest. Rows written after the caller took its
// snapshot are never touched. Returns how many rows were deleted.
func (d *DB) DeleteObjectRows(bucketID int64, key string, ids []int64) (int, error) {
	deleted := 0
	err := d.WriteTx(context.Background(), func(tx *sql.Tx) error {
		deleted = 0
		for _, id := range ids {
			res, err := tx.Exec("DELETE FROM objects WHERE id = ? AND bucket_id = ? AND key = ?", id, bucketID, key)
			if err != nil {
				return err
			}
			n, _ := res.RowsAffected()
			deleted += int(n)
		}
		var latest int
		if err := tx.QueryRow("SELECT COUNT(*) FROM objects WHERE bucket_id = ? AND key = ? AND is_latest = 1", bucketID, key).Scan(&latest); err != nil {
			return err
		}
		if latest > 0 {
			return nil
		}
		_, err := tx.Exec("UPDATE objects SET is_latest = 1 WHERE id = (SELECT id FROM objects WHERE bucket_id = ? AND key = ? ORDER BY id DESC LIMIT 1)", bucketID, key)
		return err
	})
	return deleted, err
}
