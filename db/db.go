package db

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

type DB struct {
	writer *sql.DB
	reader *sql.DB
}

type DBConfig struct {
	Path        string
	BusyTimeout int // ms
	CacheSize   int // KB
	MmapSize    int // bytes
	MaxReaders  int
}

func Open(path string) (*DB, error) {
	return OpenWithConfig(DBConfig{
		Path:        path,
		BusyTimeout: 5000,
		CacheSize:   64000,
		MmapSize:    134217728,
		MaxReaders:  4,
	})
}

func OpenWithConfig(cfg DBConfig) (*DB, error) {
	if dir := filepath.Dir(cfg.Path); dir != "." {
		if err := os.MkdirAll(dir, 0700); err != nil {
			return nil, fmt.Errorf("create database directory: %w", err)
		}
	}

	writerDSN := fmt.Sprintf("%s?_txlock=immediate&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_pragma=busy_timeout(%d)&_pragma=journal_size_limit(67108864)&_pragma=synchronous(NORMAL)&_pragma=cache_size(-%d)&_pragma=mmap_size(%d)&_pragma=temp_store(MEMORY)",
		cfg.Path, cfg.BusyTimeout, cfg.CacheSize, cfg.MmapSize)

	readerDSN := fmt.Sprintf("%s?_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_pragma=busy_timeout(%d)&_pragma=cache_size(-%d)&_pragma=mmap_size(%d)&_pragma=temp_store(MEMORY)&_pragma=query_only(1)",
		cfg.Path, cfg.BusyTimeout, cfg.CacheSize, cfg.MmapSize)

	writer, err := sql.Open("sqlite", writerDSN)
	if err != nil {
		return nil, fmt.Errorf("open writer database: %w", err)
	}
	writer.SetMaxOpenConns(1)
	writer.SetMaxIdleConns(1)
	writer.SetConnMaxLifetime(0)

	if err := writer.Ping(); err != nil {
		writer.Close()
		return nil, fmt.Errorf("ping writer database: %w", err)
	}

	reader, err := sql.Open("sqlite", readerDSN)
	if err != nil {
		writer.Close()
		return nil, fmt.Errorf("open reader database: %w", err)
	}

	maxReaders := cfg.MaxReaders
	if maxReaders <= 0 {
		maxReaders = 4
	}
	reader.SetMaxOpenConns(maxReaders)
	reader.SetMaxIdleConns(maxReaders)
	reader.SetConnMaxLifetime(0)

	if err := reader.Ping(); err != nil {
		writer.Close()
		reader.Close()
		return nil, fmt.Errorf("ping reader database: %w", err)
	}

	d := &DB{writer: writer, reader: reader}
	if err := d.migrate(); err != nil {
		d.Close()
		return nil, fmt.Errorf("migrate database: %w", err)
	}

	return d, nil
}

func (d *DB) Close() error {
	d.writer.Exec("PRAGMA wal_checkpoint(TRUNCATE)")
	err1 := d.writer.Close()
	err2 := d.reader.Close()
	if err1 != nil {
		return err1
	}
	return err2
}

// Writer returns the writer *sql.DB for direct access (e.g. migrations).
func (d *DB) Writer() *sql.DB {
	return d.writer
}

// Reader returns the reader *sql.DB for direct access.
func (d *DB) Reader() *sql.DB {
	return d.reader
}

// withRetry retries an operation up to 3 times on SQLITE_BUSY errors.
func (d *DB) withRetry(fn func() error) error {
	backoff := 10 * time.Millisecond
	for attempt := 0; attempt < 3; attempt++ {
		err := fn()
		if err == nil {
			return nil
		}
		if !isSQLiteBusy(err) {
			return err
		}
		time.Sleep(backoff)
		backoff *= 5
	}
	return fn()
}

func isSQLiteBusy(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "SQLITE_BUSY") ||
		strings.Contains(msg, "database is locked") ||
		strings.Contains(msg, "database table is locked")
}

// WriteTx executes fn within a transaction on the writer connection with retry.
func (d *DB) WriteTx(ctx context.Context, fn func(tx *sql.Tx) error) error {
	return d.withRetry(func() error {
		tx, err := d.writer.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if err := fn(tx); err != nil {
			tx.Rollback()
			return err
		}
		return tx.Commit()
	})
}

func (d *DB) migrate() error {
	schema := `
	CREATE TABLE IF NOT EXISTS buckets (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		name TEXT UNIQUE NOT NULL,
		quota_bytes INTEGER DEFAULT 0,
		versioning TEXT NOT NULL DEFAULT '',
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP
	);

	CREATE TABLE IF NOT EXISTS bucket_credentials (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		bucket_id INTEGER NOT NULL,
		access_key TEXT UNIQUE NOT NULL,
		secret_key TEXT NOT NULL,
		permission TEXT DEFAULT 'read-write',
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
		FOREIGN KEY (bucket_id) REFERENCES buckets(id) ON DELETE CASCADE
	);

	CREATE TABLE IF NOT EXISTS objects (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		bucket_id INTEGER NOT NULL,
		key TEXT NOT NULL,
		size INTEGER NOT NULL,
		etag TEXT NOT NULL,
		content_type TEXT DEFAULT 'application/octet-stream',
		last_modified DATETIME DEFAULT CURRENT_TIMESTAMP,
		metadata TEXT DEFAULT '{}',
		version_id TEXT NOT NULL DEFAULT '',
		is_latest INTEGER NOT NULL DEFAULT 1,
		is_delete_marker INTEGER NOT NULL DEFAULT 0,
		FOREIGN KEY (bucket_id) REFERENCES buckets(id) ON DELETE CASCADE,
		UNIQUE(bucket_id, key, version_id)
	);

	CREATE TABLE IF NOT EXISTS multipart_uploads (
		id TEXT PRIMARY KEY,
		bucket_id INTEGER NOT NULL,
		key TEXT NOT NULL,
		content_type TEXT,
		metadata TEXT DEFAULT '{}',
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
		FOREIGN KEY (bucket_id) REFERENCES buckets(id) ON DELETE CASCADE
	);

	CREATE TABLE IF NOT EXISTS multipart_parts (
		upload_id TEXT NOT NULL,
		part_number INTEGER NOT NULL,
		size INTEGER NOT NULL,
		etag TEXT NOT NULL,
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
		PRIMARY KEY (upload_id, part_number),
		FOREIGN KEY (upload_id) REFERENCES multipart_uploads(id) ON DELETE CASCADE
	);
	`

	if _, err := d.writer.Exec(schema); err != nil {
		return err
	}

	// Add columns for existing databases (idempotent)
	migrations := []string{
		"ALTER TABLE buckets ADD COLUMN quota_bytes INTEGER DEFAULT 0",
		"ALTER TABLE bucket_credentials ADD COLUMN permission TEXT DEFAULT 'read-write'",
		"ALTER TABLE buckets ADD COLUMN versioning TEXT NOT NULL DEFAULT ''",
		"ALTER TABLE objects ADD COLUMN version_id TEXT NOT NULL DEFAULT ''",
		"ALTER TABLE objects ADD COLUMN is_latest INTEGER NOT NULL DEFAULT 1",
		"ALTER TABLE objects ADD COLUMN is_delete_marker INTEGER NOT NULL DEFAULT 0",
		"ALTER TABLE buckets ADD COLUMN storage_dir TEXT NOT NULL DEFAULT ''",
		"ALTER TABLE bucket_credentials ADD COLUMN name TEXT NOT NULL DEFAULT ''",
		"ALTER TABLE bucket_webhooks ADD COLUMN name TEXT NOT NULL DEFAULT ''",
		"ALTER TABLE lifecycle_rules ADD COLUMN name TEXT NOT NULL DEFAULT ''",
	}
	for _, m := range migrations {
		_, err := d.writer.Exec(m)
		if err != nil {
			// Ignore "duplicate column" errors which are expected for existing databases
			errMsg := err.Error()
			if !strings.Contains(errMsg, "duplicate column") && !strings.Contains(errMsg, "already exists") && !strings.Contains(errMsg, "no such table") {
				return fmt.Errorf("migration failed: %s: %w", m, err)
			}
		}
	}

	// Create index for object listing performance (idempotent)
	d.writer.Exec("CREATE INDEX IF NOT EXISTS idx_objects_bucket_key ON objects(bucket_id, key)")
	d.writer.Exec("CREATE INDEX IF NOT EXISTS idx_objects_bucket_key_version ON objects(bucket_id, key, version_id)")
	d.writer.Exec("CREATE INDEX IF NOT EXISTS idx_objects_bucket_latest ON objects(bucket_id, is_latest)")

	// Lifecycle rules table
	d.writer.Exec(`CREATE TABLE IF NOT EXISTS lifecycle_rules (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		bucket_name TEXT NOT NULL,
		name TEXT NOT NULL DEFAULT '',
		prefix TEXT NOT NULL DEFAULT '',
		expiration_days INTEGER NOT NULL,
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
		FOREIGN KEY (bucket_name) REFERENCES buckets(name) ON DELETE CASCADE,
		UNIQUE(bucket_name, prefix)
	)`)

	// Admin credentials table
	d.writer.Exec(`CREATE TABLE IF NOT EXISTS admin_credentials (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		username TEXT UNIQUE NOT NULL,
		password_hash TEXT NOT NULL,
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP
	)`)

	// Webhook notifications table
	d.writer.Exec(`CREATE TABLE IF NOT EXISTS bucket_webhooks (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		bucket_name TEXT NOT NULL,
		name TEXT NOT NULL DEFAULT '',
		url TEXT NOT NULL,
		event_types TEXT NOT NULL DEFAULT '*',
		secret TEXT NOT NULL DEFAULT '',
		active INTEGER NOT NULL DEFAULT 1,
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
		FOREIGN KEY (bucket_name) REFERENCES buckets(name) ON DELETE CASCADE
	)`)

	return nil
}
