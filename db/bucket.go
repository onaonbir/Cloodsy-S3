package db

import (
	"database/sql"
	"time"
)

type Bucket struct {
	ID         int64
	Name       string
	QuotaBytes int64
	Versioning string // "", "Enabled", "Suspended"
	StorageDir string // custom storage directory; empty = use global RootDir
	CreatedAt  time.Time
}

type BucketCredential struct {
	ID         int64
	BucketID   int64
	Name       string
	AccessKey  string
	SecretKey  string
	Permission string // "read-write" or "read-only"
	CreatedAt  time.Time
}

func (d *DB) CreateBucket(name, storageDir string) (*Bucket, error) {
	var b *Bucket
	err := d.withRetry(func() error {
		res, err := d.writer.Exec("INSERT INTO buckets (name, storage_dir) VALUES (?, ?)", name, storageDir)
		if err != nil {
			return err
		}
		id, _ := res.LastInsertId()
		b = &Bucket{ID: id, Name: name, StorageDir: storageDir, CreatedAt: time.Now()}
		return nil
	})
	return b, err
}

func (d *DB) GetBucket(name string) (*Bucket, error) {
	b := &Bucket{}
	err := d.reader.QueryRow("SELECT id, name, quota_bytes, versioning, storage_dir, created_at FROM buckets WHERE name = ?", name).
		Scan(&b.ID, &b.Name, &b.QuotaBytes, &b.Versioning, &b.StorageDir, &b.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return b, err
}

// GetBucketNameByID returns just the bucket name for an id. Used by the multipart
// cleaner where the full bucket struct is unnecessary.
func (d *DB) GetBucketNameByID(id int64) (string, error) {
	var name string
	err := d.reader.QueryRow("SELECT name FROM buckets WHERE id = ?", id).Scan(&name)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return name, err
}

func (d *DB) GetBucketByID(id int64) (*Bucket, error) {
	b := &Bucket{}
	err := d.reader.QueryRow("SELECT id, name, quota_bytes, versioning, storage_dir, created_at FROM buckets WHERE id = ?", id).
		Scan(&b.ID, &b.Name, &b.QuotaBytes, &b.Versioning, &b.StorageDir, &b.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return b, err
}

func (d *DB) ListBuckets() ([]Bucket, error) {
	rows, err := d.reader.Query("SELECT id, name, quota_bytes, versioning, storage_dir, created_at FROM buckets ORDER BY name")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var buckets []Bucket
	for rows.Next() {
		var b Bucket
		if err := rows.Scan(&b.ID, &b.Name, &b.QuotaBytes, &b.Versioning, &b.StorageDir, &b.CreatedAt); err != nil {
			return nil, err
		}
		buckets = append(buckets, b)
	}
	return buckets, rows.Err()
}

func (d *DB) ListBucketsByIDs(ids []int64) ([]Bucket, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	query := "SELECT id, name, quota_bytes, versioning, storage_dir, created_at FROM buckets WHERE id IN ("
	args := make([]interface{}, len(ids))
	for i, id := range ids {
		if i > 0 {
			query += ","
		}
		query += "?"
		args[i] = id
	}
	query += ") ORDER BY name"

	rows, err := d.reader.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var buckets []Bucket
	for rows.Next() {
		var b Bucket
		if err := rows.Scan(&b.ID, &b.Name, &b.QuotaBytes, &b.Versioning, &b.StorageDir, &b.CreatedAt); err != nil {
			return nil, err
		}
		buckets = append(buckets, b)
	}
	return buckets, rows.Err()
}

// GetAllBucketStorageDirs returns a map of bucket name to custom storage directory
// for all buckets that have a non-empty storage_dir.
func (d *DB) GetAllBucketStorageDirs() (map[string]string, error) {
	rows, err := d.reader.Query("SELECT name, storage_dir FROM buckets WHERE storage_dir != ''")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	dirs := make(map[string]string)
	for rows.Next() {
		var name, storageDir string
		if err := rows.Scan(&name, &storageDir); err != nil {
			return nil, err
		}
		dirs[name] = storageDir
	}
	return dirs, rows.Err()
}

func (d *DB) SetBucketStorageDir(name, storageDir string) error {
	return d.withRetry(func() error {
		_, err := d.writer.Exec("UPDATE buckets SET storage_dir = ? WHERE name = ?", storageDir, name)
		return err
	})
}

func (d *DB) SetBucketQuota(name string, quotaBytes int64) error {
	return d.withRetry(func() error {
		_, err := d.writer.Exec("UPDATE buckets SET quota_bytes = ? WHERE name = ?", quotaBytes, name)
		return err
	})
}

func (d *DB) GetBucketUsage(bucketID int64) (int64, error) {
	var usage int64
	err := d.reader.QueryRow("SELECT COALESCE(SUM(size), 0) FROM objects WHERE bucket_id = ? AND is_latest = 1 AND is_delete_marker = 0", bucketID).Scan(&usage)
	return usage, err
}

func (d *DB) DeleteBucket(name string) error {
	return d.withRetry(func() error {
		_, err := d.writer.Exec("DELETE FROM buckets WHERE name = ?", name)
		return err
	})
}

func (d *DB) BucketHasObjects(bucketID int64) (bool, error) {
	var one int
	err := d.reader.QueryRow("SELECT 1 FROM objects WHERE bucket_id = ? AND is_latest = 1 AND is_delete_marker = 0 LIMIT 1", bucketID).Scan(&one)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// Credential operations

func (d *DB) CreateCredential(bucketID int64, name, accessKey, secretKey, permission string) (*BucketCredential, error) {
	if permission == "" {
		permission = "read-write"
	}
	var cred *BucketCredential
	err := d.withRetry(func() error {
		res, err := d.writer.Exec(
			"INSERT INTO bucket_credentials (bucket_id, name, access_key, secret_key, permission) VALUES (?, ?, ?, ?, ?)",
			bucketID, name, accessKey, secretKey, permission,
		)
		if err != nil {
			return err
		}
		id, _ := res.LastInsertId()
		cred = &BucketCredential{
			ID:         id,
			BucketID:   bucketID,
			Name:       name,
			AccessKey:  accessKey,
			SecretKey:  secretKey,
			Permission: permission,
			CreatedAt:  time.Now(),
		}
		return nil
	})
	return cred, err
}

func (d *DB) GetCredentialByAccessKey(accessKey string) (*BucketCredential, error) {
	c := &BucketCredential{}
	err := d.reader.QueryRow(
		"SELECT id, bucket_id, name, access_key, secret_key, permission, created_at FROM bucket_credentials WHERE access_key = ?",
		accessKey,
	).Scan(&c.ID, &c.BucketID, &c.Name, &c.AccessKey, &c.SecretKey, &c.Permission, &c.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return c, err
}

func (d *DB) ListCredentials(bucketID int64) ([]BucketCredential, error) {
	rows, err := d.reader.Query(
		"SELECT id, bucket_id, name, access_key, permission, created_at FROM bucket_credentials WHERE bucket_id = ?",
		bucketID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var creds []BucketCredential
	for rows.Next() {
		var c BucketCredential
		if err := rows.Scan(&c.ID, &c.BucketID, &c.Name, &c.AccessKey, &c.Permission, &c.CreatedAt); err != nil {
			return nil, err
		}
		creds = append(creds, c)
	}
	return creds, rows.Err()
}

func (d *DB) ListCredentialsFull(bucketID int64) ([]BucketCredential, error) {
	rows, err := d.reader.Query(
		"SELECT id, bucket_id, name, access_key, secret_key, permission, created_at FROM bucket_credentials WHERE bucket_id = ?",
		bucketID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var creds []BucketCredential
	for rows.Next() {
		var c BucketCredential
		if err := rows.Scan(&c.ID, &c.BucketID, &c.Name, &c.AccessKey, &c.SecretKey, &c.Permission, &c.CreatedAt); err != nil {
			return nil, err
		}
		creds = append(creds, c)
	}
	return creds, rows.Err()
}

func (d *DB) DeleteCredential(accessKey string) error {
	return d.withRetry(func() error {
		_, err := d.writer.Exec("DELETE FROM bucket_credentials WHERE access_key = ?", accessKey)
		return err
	})
}

func (d *DB) GetBucketIDsForAccessKey(accessKey string) ([]int64, error) {
	cred, err := d.GetCredentialByAccessKey(accessKey)
	if err != nil || cred == nil {
		return nil, err
	}
	return []int64{cred.BucketID}, nil
}

// SetBucketVersioning sets the versioning state of a bucket.
func (d *DB) SetBucketVersioning(name, versioning string) error {
	return d.withRetry(func() error {
		_, err := d.writer.Exec("UPDATE buckets SET versioning = ? WHERE name = ?", versioning, name)
		return err
	})
}

// Lifecycle rule operations

type LifecycleRule struct {
	ID             int64
	BucketName     string
	Name           string
	Prefix         string
	ExpirationDays int
	CreatedAt      time.Time
}

func (d *DB) PutLifecycleRule(bucketName, name, prefix string, expirationDays int) error {
	return d.withRetry(func() error {
		_, err := d.writer.Exec(`
			INSERT INTO lifecycle_rules (bucket_name, name, prefix, expiration_days)
			VALUES (?, ?, ?, ?)
			ON CONFLICT(bucket_name, prefix) DO UPDATE SET
				expiration_days = excluded.expiration_days,
				name = excluded.name
		`, bucketName, name, prefix, expirationDays)
		return err
	})
}

func (d *DB) GetLifecycleRules(bucketName string) ([]LifecycleRule, error) {
	rows, err := d.reader.Query(`
		SELECT id, bucket_name, name, prefix, expiration_days, created_at
		FROM lifecycle_rules WHERE bucket_name = ? ORDER BY prefix
	`, bucketName)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var rules []LifecycleRule
	for rows.Next() {
		var r LifecycleRule
		if err := rows.Scan(&r.ID, &r.BucketName, &r.Name, &r.Prefix, &r.ExpirationDays, &r.CreatedAt); err != nil {
			return nil, err
		}
		rules = append(rules, r)
	}
	return rules, rows.Err()
}

func (d *DB) DeleteLifecycleRules(bucketName string) error {
	return d.withRetry(func() error {
		_, err := d.writer.Exec("DELETE FROM lifecycle_rules WHERE bucket_name = ?", bucketName)
		return err
	})
}

func (d *DB) DeleteLifecycleRuleByPrefix(bucketName, prefix string) error {
	return d.withRetry(func() error {
		_, err := d.writer.Exec("DELETE FROM lifecycle_rules WHERE bucket_name = ? AND prefix = ?", bucketName, prefix)
		return err
	})
}

func (d *DB) GetAllLifecycleRules() ([]LifecycleRule, error) {
	rows, err := d.reader.Query(`SELECT id, bucket_name, name, prefix, expiration_days, created_at FROM lifecycle_rules`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var rules []LifecycleRule
	for rows.Next() {
		var r LifecycleRule
		if err := rows.Scan(&r.ID, &r.BucketName, &r.Name, &r.Prefix, &r.ExpirationDays, &r.CreatedAt); err != nil {
			return nil, err
		}
		rules = append(rules, r)
	}
	return rules, rows.Err()
}

// GetMatchingLifecycleRule returns the best matching lifecycle rule for a key in a bucket.
func (d *DB) GetMatchingLifecycleRule(bucketName, key string) (*LifecycleRule, error) {
	rules, err := d.GetLifecycleRules(bucketName)
	if err != nil {
		return nil, err
	}
	var best *LifecycleRule
	for i, r := range rules {
		if key == r.Prefix || len(r.Prefix) == 0 || (len(key) >= len(r.Prefix) && key[:len(r.Prefix)] == r.Prefix) {
			if best == nil || len(r.Prefix) > len(best.Prefix) {
				best = &rules[i]
			}
		}
	}
	return best, nil
}

// Webhook operations

type BucketWebhook struct {
	ID         int64
	BucketName string
	Name       string
	URL        string
	EventTypes string
	Secret     string
	Active     bool
	CreatedAt  time.Time
}

func (d *DB) CreateWebhook(bucketName, name, url, eventTypes, secret string) (*BucketWebhook, error) {
	if eventTypes == "" {
		eventTypes = "*"
	}
	var hook *BucketWebhook
	err := d.withRetry(func() error {
		res, err := d.writer.Exec(`
			INSERT INTO bucket_webhooks (bucket_name, name, url, event_types, secret)
			VALUES (?, ?, ?, ?, ?)
		`, bucketName, name, url, eventTypes, secret)
		if err != nil {
			return err
		}
		id, _ := res.LastInsertId()
		hook = &BucketWebhook{
			ID:         id,
			BucketName: bucketName,
			Name:       name,
			URL:        url,
			EventTypes: eventTypes,
			Secret:     secret,
			Active:     true,
			CreatedAt:  time.Now(),
		}
		return nil
	})
	return hook, err
}

func (d *DB) ListWebhooks(bucketName string) ([]BucketWebhook, error) {
	rows, err := d.reader.Query(`
		SELECT id, bucket_name, name, url, event_types, secret, active, created_at
		FROM bucket_webhooks WHERE bucket_name = ? ORDER BY id
	`, bucketName)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var hooks []BucketWebhook
	for rows.Next() {
		var h BucketWebhook
		if err := rows.Scan(&h.ID, &h.BucketName, &h.Name, &h.URL, &h.EventTypes, &h.Secret, &h.Active, &h.CreatedAt); err != nil {
			return nil, err
		}
		hooks = append(hooks, h)
	}
	return hooks, rows.Err()
}

func (d *DB) DeleteWebhook(id int64) error {
	return d.withRetry(func() error {
		_, err := d.writer.Exec("DELETE FROM bucket_webhooks WHERE id = ?", id)
		return err
	})
}

func (d *DB) DeleteAllWebhooks(bucketName string) error {
	return d.withRetry(func() error {
		_, err := d.writer.Exec("DELETE FROM bucket_webhooks WHERE bucket_name = ?", bucketName)
		return err
	})
}

func (d *DB) GetActiveWebhooksForBucket(bucketName string) ([]BucketWebhook, error) {
	rows, err := d.reader.Query(`
		SELECT id, bucket_name, name, url, event_types, secret, active, created_at
		FROM bucket_webhooks WHERE bucket_name = ? AND active = 1
	`, bucketName)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var hooks []BucketWebhook
	for rows.Next() {
		var h BucketWebhook
		if err := rows.Scan(&h.ID, &h.BucketName, &h.Name, &h.URL, &h.EventTypes, &h.Secret, &h.Active, &h.CreatedAt); err != nil {
			return nil, err
		}
		hooks = append(hooks, h)
	}
	return hooks, rows.Err()
}
