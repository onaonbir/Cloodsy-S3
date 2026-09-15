package lifecycle_test

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
	"github.com/onaonbir/Cloodsy-S3/db"
	"github.com/onaonbir/Cloodsy-S3/lifecycle"
	"github.com/onaonbir/Cloodsy-S3/service"
	"github.com/onaonbir/Cloodsy-S3/storage"
)

const bucketName = "lc-bucket"

type env struct {
	t      *testing.T
	db     *db.DB
	store  *storage.FileSystem
	svc    *service.Objects
	bucket *db.Bucket
	logger *slog.Logger
}

func newEnv(t *testing.T) *env {
	t.Helper()
	dir := t.TempDir()
	database, err := db.Open(filepath.Join(dir, "lc.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	store, err := storage.NewFileSystem(filepath.Join(dir, "data"))
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	svc := service.New(database, store, logger)
	b, err := database.CreateBucket(bucketName, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CreateBucketDir(bucketName); err != nil {
		t.Fatal(err)
	}
	return &env{t: t, db: database, store: store, svc: svc, bucket: b, logger: logger}
}

func (e *env) run() {
	lifecycle.RunOnce(context.Background(), e.db, e.store, e.svc, e.logger)
}

func (e *env) setVersioning(v string) {
	e.t.Helper()
	if err := e.db.SetBucketVersioning(bucketName, v); err != nil {
		e.t.Fatal(err)
	}
	b, _ := e.db.GetBucket(bucketName)
	e.bucket = b
}

func (e *env) put(key, body string) *service.PutResult {
	e.t.Helper()
	res, err := e.svc.Put(service.PutInput{Bucket: e.bucket, Key: key, Body: bytes.NewReader([]byte(body)), DeclaredSize: int64(len(body))})
	if err != nil {
		e.t.Fatalf("put %s: %v", key, err)
	}
	return res
}

// backdate moves every row of key (or of one version) N days into the past.
func (e *env) backdate(key string, days int) {
	e.t.Helper()
	if _, err := e.db.Writer().Exec("UPDATE objects SET last_modified = datetime('now', '-' || ? || ' days') WHERE bucket_id = ? AND key = ?", days, e.bucket.ID, key); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) backdateVersion(key, versionID string, days int) {
	e.t.Helper()
	if _, err := e.db.Writer().Exec("UPDATE objects SET last_modified = datetime('now', '-' || ? || ' days') WHERE bucket_id = ? AND key = ? AND version_id = ?", days, e.bucket.ID, key, versionID); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) rule(r db.LifecycleRule) {
	e.t.Helper()
	r.BucketName = bucketName
	if r.Name == "" {
		r.Name = "rule-" + r.Prefix
	}
	if err := e.db.PutLifecycleRuleFull(r); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) latest(key string) *db.ObjectMeta {
	e.t.Helper()
	m, err := e.db.GetObjectMeta(e.bucket.ID, key)
	if err != nil {
		e.t.Fatal(err)
	}
	return m
}

func (e *env) rows(key string) []db.ObjectMeta {
	e.t.Helper()
	vs, err := e.db.ListVersionsForKey(e.bucket.ID, key)
	if err != nil {
		e.t.Fatal(err)
	}
	return vs
}

func TestRunOnce_NoRulesIsNoop(t *testing.T) {
	e := newEnv(t)
	e.put("a", "x")
	e.backdate("a", 100)
	e.run()
	if e.latest("a") == nil {
		t.Fatal("object removed without any rule")
	}
}

func TestExpiration_CurrentVersions(t *testing.T) {
	e := newEnv(t)
	e.put("logs/old", "o")
	e.put("logs/new", "n")
	e.put("other/old", "o")
	e.backdate("logs/old", 10)
	e.backdate("other/old", 10)
	e.rule(db.LifecycleRule{Prefix: "logs/", Status: "Enabled", ExpirationDays: 7})
	e.run()

	if e.latest("logs/old") != nil {
		t.Fatal("expired object still present")
	}
	if e.store.ObjectExists(bucketName, "logs/old") {
		t.Fatal("expired object bytes still on disk")
	}
	if e.latest("logs/new") == nil {
		t.Fatal("fresh object removed")
	}
	if e.latest("other/old") == nil {
		t.Fatal("object outside the rule prefix removed")
	}
	// Exactly-on-the-boundary objects (younger than N days) survive.
	e.put("logs/edge", "e")
	e.backdate("logs/edge", 6)
	e.run()
	if e.latest("logs/edge") == nil {
		t.Fatal("6-day-old object expired by a 7-day rule")
	}
}

func TestExpiration_DisabledRuleIsSkipped(t *testing.T) {
	e := newEnv(t)
	e.put("logs/old", "o")
	e.backdate("logs/old", 10)
	e.rule(db.LifecycleRule{Prefix: "logs/", Status: "Disabled", ExpirationDays: 1, NoncurrentDays: 1, AbortMultipartDays: 1, ExpireDeleteMarkers: true})
	e.run()
	if e.latest("logs/old") == nil {
		t.Fatal("disabled rule expired an object")
	}
}

func TestExpiration_PrefixIsByteExact(t *testing.T) {
	e := newEnv(t)
	for _, k := range []string{"tmp/x", "TMP/x", "tmp-x", "tmpx", "tmp/"} {
		e.put(k, "x")
		e.backdate(k, 10)
	}
	e.rule(db.LifecycleRule{Prefix: "tmp/", Status: "Enabled", ExpirationDays: 1})
	e.run()
	if e.latest("tmp/x") != nil || e.latest("tmp/") != nil {
		t.Fatal("tmp/ objects not expired")
	}
	for _, k := range []string{"TMP/x", "tmp-x", "tmpx"} {
		if e.latest(k) == nil {
			t.Fatalf("%q expired by rule with prefix %q", k, "tmp/")
		}
	}
	// Empty prefix matches everything.
	e.rule(db.LifecycleRule{Name: "all", Prefix: "", Status: "Enabled", ExpirationDays: 1})
	e.run()
	for _, k := range []string{"TMP/x", "tmp-x", "tmpx"} {
		if e.latest(k) != nil {
			t.Fatalf("%q survived the catch-all rule", k)
		}
	}
}

func TestExpiration_VersionedCreatesDeleteMarker(t *testing.T) {
	e := newEnv(t)
	e.setVersioning("Enabled")
	r := e.put("v/k", "one")
	e.backdate("v/k", 10)
	e.rule(db.LifecycleRule{Prefix: "v/", Status: "Enabled", ExpirationDays: 1})
	e.run()

	rows := e.rows("v/k")
	if len(rows) != 2 {
		t.Fatalf("rows %d want 2 (version + marker)", len(rows))
	}
	if !rows[0].IsDeleteMarker || !rows[0].IsLatest {
		t.Fatalf("newest row is not a latest delete marker: %+v", rows[0])
	}
	if rows[1].VersionID != r.VersionID || rows[1].IsLatest {
		t.Fatalf("old version %+v", rows[1])
	}
	if rc, err := e.store.GetVersionedObject(bucketName, "v/k", r.VersionID); err != nil {
		t.Fatal("versioned bytes removed by expiration")
	} else {
		rc.Close()
	}
	// A second run does not stack markers: the key's current version is a
	// marker, which GetExpiredObjects excludes.
	e.run()
	if n := len(e.rows("v/k")); n != 2 {
		t.Fatalf("second run changed rows to %d", n)
	}
}

func TestExpiration_Noncurrent(t *testing.T) {
	e := newEnv(t)
	e.setVersioning("Enabled")
	r1 := e.put("n/k", "one")
	r2 := e.put("n/k", "two")
	r3 := e.put("n/k", "three")
	e.backdateVersion("n/k", r1.VersionID, 10)
	e.backdateVersion("n/k", r2.VersionID, 2)
	e.backdateVersion("n/k", r3.VersionID, 10) // current: must survive a noncurrent rule
	e.rule(db.LifecycleRule{Prefix: "n/", Status: "Enabled", NoncurrentDays: 5})
	e.run()

	rows := e.rows("n/k")
	if len(rows) != 2 {
		t.Fatalf("rows %d want 2", len(rows))
	}
	if rows[0].VersionID != r3.VersionID || !rows[0].IsLatest || rows[1].VersionID != r2.VersionID {
		t.Fatalf("rows %+v", rows)
	}
	if _, err := e.store.GetVersionedObject(bucketName, "n/k", r1.VersionID); err == nil {
		t.Fatal("expired noncurrent version bytes still on disk")
	}
	if rc, err := e.store.GetVersionedObject(bucketName, "n/k", r3.VersionID); err != nil {
		t.Fatal("current version bytes removed")
	} else {
		rc.Close()
	}
	// Noncurrent delete markers are removed too.
	mk, _ := e.svc.Delete(e.bucket, "n/k", "") // marker becomes current
	e.put("n/k", "four")                       // marker becomes noncurrent
	e.backdateVersion("n/k", mk.VersionID, 10)
	e.run()
	for _, r := range e.rows("n/k") {
		if r.VersionID == mk.VersionID {
			t.Fatal("old noncurrent delete marker not removed")
		}
	}
}

func TestExpiration_OrphanDeleteMarkers(t *testing.T) {
	e := newEnv(t)
	e.setVersioning("Enabled")
	r := e.put("m/orphan", "x")
	e.put("m/keep", "y")
	e.svc.Delete(e.bucket, "m/orphan", "")          // marker over a version
	e.svc.Delete(e.bucket, "m/orphan", r.VersionID) // remove the version → orphan marker
	e.svc.Delete(e.bucket, "m/keep", "")            // marker with a version below it
	if rows := e.rows("m/orphan"); len(rows) != 1 || !rows[0].IsDeleteMarker {
		t.Fatalf("setup rows %+v", rows)
	}
	e.rule(db.LifecycleRule{Prefix: "m/", Status: "Enabled", ExpireDeleteMarkers: true})
	e.run()
	if n := len(e.rows("m/orphan")); n != 0 {
		t.Fatalf("orphan marker not removed: %d rows", n)
	}
	if rows := e.rows("m/keep"); len(rows) != 2 || !rows[0].IsDeleteMarker {
		t.Fatalf("non-orphan marker touched: %+v", rows)
	}
}

func TestAbortMultipart(t *testing.T) {
	e := newEnv(t)
	create := func(key string) string {
		id := uuid.New().String()
		if err := e.db.CreateMultipartUpload(&db.MultipartUpload{ID: id, BucketID: e.bucket.ID, Key: key, ContentType: "application/octet-stream", Metadata: "{}"}); err != nil {
			t.Fatal(err)
		}
		if _, _, err := e.store.PutMultipartPart(bucketName, id, 1, bytes.NewReader([]byte("part")), storage.PutOptions{}); err != nil {
			t.Fatal(err)
		}
		if err := e.db.PutMultipartPart(&db.MultipartPart{UploadID: id, PartNumber: 1, Size: 4, ETag: "\"e\""}); err != nil {
			t.Fatal(err)
		}
		return id
	}
	partDir := func(id string) string {
		return filepath.Join(e.store.RootDir, "."+bucketName+"-multipart", id)
	}
	stale := create("up/stale")
	fresh := create("up/fresh")
	outside := create("elsewhere/stale")
	for _, id := range []string{stale, outside} {
		if _, err := e.db.Writer().Exec("UPDATE multipart_uploads SET created_at = datetime('now', '-5 days') WHERE id = ?", id); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Stat(partDir(stale)); err != nil {
		t.Fatalf("staged part dir missing before cleanup: %v", err)
	}
	e.rule(db.LifecycleRule{Prefix: "up/", Status: "Enabled", AbortMultipartDays: 3})
	e.run()

	if u, _ := e.db.GetMultipartUpload(stale); u != nil {
		t.Fatal("stale upload row still present")
	}
	if _, err := os.Stat(partDir(stale)); !os.IsNotExist(err) {
		t.Fatalf("staged parts of the stale upload remain: %v", err)
	}
	if u, _ := e.db.GetMultipartUpload(fresh); u == nil {
		t.Fatal("fresh upload aborted")
	}
	if _, err := os.Stat(partDir(fresh)); err != nil {
		t.Fatal("fresh upload parts removed")
	}
	if u, _ := e.db.GetMultipartUpload(outside); u == nil {
		t.Fatal("upload outside the rule prefix aborted")
	}
	if parts, _ := e.db.ListMultipartParts(stale); len(parts) != 0 {
		t.Fatalf("part rows of aborted upload remain: %d", len(parts))
	}
}

func TestRunOnce_ContextCancelled(t *testing.T) {
	e := newEnv(t)
	e.put("a", "x")
	e.backdate("a", 10)
	e.rule(db.LifecycleRule{Prefix: "", Status: "Enabled", ExpirationDays: 1})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	lifecycle.RunOnce(ctx, e.db, e.store, e.svc, e.logger)
	if e.latest("a") == nil {
		t.Fatal("cancelled run still deleted objects")
	}
}
