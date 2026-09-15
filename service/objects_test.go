package service_test

import (
	"bytes"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"github.com/onaonbir/Cloodsy-S3/db"
	"github.com/onaonbir/Cloodsy-S3/service"
	"github.com/onaonbir/Cloodsy-S3/storage"
)

const bucketName = "svc-bucket"

type env struct {
	t      *testing.T
	db     *db.DB
	store  *storage.FileSystem
	svc    *service.Objects
	bucket *db.Bucket
	root   string
}

func newEnv(t *testing.T) *env {
	t.Helper()
	dir := t.TempDir()
	database, err := db.Open(filepath.Join(dir, "svc.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	root := filepath.Join(dir, "data")
	store, err := storage.NewFileSystem(root)
	if err != nil {
		t.Fatal(err)
	}
	svc := service.New(database, store, slog.New(slog.NewTextHandler(io.Discard, nil)))
	b, err := database.CreateBucket(bucketName, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CreateBucketDir(bucketName); err != nil {
		t.Fatal(err)
	}
	return &env{t: t, db: database, store: store, svc: svc, bucket: b, root: root}
}

func (e *env) setVersioning(v string) {
	e.t.Helper()
	if err := e.db.SetBucketVersioning(bucketName, v); err != nil {
		e.t.Fatal(err)
	}
	e.refresh()
}

func (e *env) refresh() {
	e.t.Helper()
	b, err := e.db.GetBucket(bucketName)
	if err != nil || b == nil {
		e.t.Fatalf("refresh bucket: %v", err)
	}
	e.bucket = b
}

func (e *env) put(key string, body []byte, in service.PutInput) *service.PutResult {
	e.t.Helper()
	in.Bucket = e.bucket
	in.Key = key
	in.Body = bytes.NewReader(body)
	if in.DeclaredSize == 0 {
		in.DeclaredSize = int64(len(body))
	}
	res, err := e.svc.Put(in)
	if err != nil {
		e.t.Fatalf("put %s: %v", key, err)
	}
	return res
}

func (e *env) read(meta *db.ObjectMeta) []byte {
	e.t.Helper()
	rc, err := e.svc.Open(e.bucket, meta)
	if err != nil {
		e.t.Fatalf("open %s@%s: %v", meta.Key, meta.VersionID, err)
	}
	defer rc.Close()
	b, err := io.ReadAll(rc)
	if err != nil {
		e.t.Fatal(err)
	}
	return b
}

func (e *env) versions(key string) []db.ObjectMeta {
	e.t.Helper()
	vs, err := e.db.ListVersionsForKey(e.bucket.ID, key)
	if err != nil {
		e.t.Fatal(err)
	}
	return vs
}

func (e *env) latest(key string) *db.ObjectMeta {
	e.t.Helper()
	m, err := e.db.GetObjectMeta(e.bucket.ID, key)
	if err != nil {
		e.t.Fatal(err)
	}
	return m
}

func (e *env) versionedExists(key, versionID string) bool {
	rc, err := e.store.GetVersionedObject(bucketName, key, versionID)
	if err != nil {
		return false
	}
	rc.Close()
	return true
}

// tmpFiles lists every leftover .tmp-* file under the storage root.
func (e *env) tmpFiles() []string {
	var out []string
	filepath.WalkDir(e.root, func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.HasPrefix(d.Name(), ".tmp-") {
			out = append(out, p)
		}
		return nil
	})
	return out
}

// --- Put -------------------------------------------------------------------

func TestPut_UnversionedOverwrite(t *testing.T) {
	e := newEnv(t)
	r1 := e.put("k", []byte("first"), service.PutInput{ContentType: "text/plain"})
	if r1.VersionID != "" || r1.Size != 5 || r1.ETag == "" {
		t.Fatalf("result %+v", r1)
	}
	r2 := e.put("k", []byte("second!"), service.PutInput{})
	if r2.Size != 7 {
		t.Fatalf("size %d", r2.Size)
	}
	vs := e.versions("k")
	if len(vs) != 1 {
		t.Fatalf("unversioned overwrite left %d rows", len(vs))
	}
	if !vs[0].IsLatest || vs[0].VersionID != "" || vs[0].Size != 7 {
		t.Fatalf("row %+v", vs[0])
	}
	if got := e.read(&vs[0]); string(got) != "second!" {
		t.Fatalf("body %q", got)
	}
	// Defaults fill in.
	if vs[0].ContentType != "application/octet-stream" || vs[0].Metadata != "{}" {
		t.Fatalf("defaults %+v", vs[0])
	}
}

func TestPut_VersioningEnabled(t *testing.T) {
	e := newEnv(t)
	e.setVersioning("Enabled")
	r1 := e.put("k", []byte("one"), service.PutInput{})
	r2 := e.put("k", []byte("two"), service.PutInput{})
	if r1.VersionID == "" || r2.VersionID == "" || r1.VersionID == r2.VersionID || r1.VersionID == "null" {
		t.Fatalf("version ids %q %q", r1.VersionID, r2.VersionID)
	}
	vs := e.versions("k")
	if len(vs) != 2 {
		t.Fatalf("rows %d", len(vs))
	}
	latest := 0
	for _, v := range vs {
		if v.IsLatest {
			latest++
			if v.VersionID != r2.VersionID {
				t.Fatalf("latest is %q want %q", v.VersionID, r2.VersionID)
			}
		}
	}
	if latest != 1 {
		t.Fatalf("%d latest rows", latest)
	}
	for _, v := range vs {
		want := "two"
		if v.VersionID == r1.VersionID {
			want = "one"
		}
		if got := e.read(&v); string(got) != want {
			t.Fatalf("version %s body %q want %q", v.VersionID, got, want)
		}
	}
	// No unversioned file is created for versioned writes.
	if e.store.ObjectExists(bucketName, "k") {
		t.Fatal("unversioned file present on Enabled bucket")
	}
}

func TestPut_Suspended(t *testing.T) {
	e := newEnv(t)
	e.setVersioning("Enabled")
	r1 := e.put("k", []byte("versioned"), service.PutInput{})
	e.setVersioning("Suspended")
	r2 := e.put("k", []byte("null-one"), service.PutInput{})
	if r2.VersionID != "null" {
		t.Fatalf("suspended put version %q", r2.VersionID)
	}
	e.put("k", []byte("null-two"), service.PutInput{})
	vs := e.versions("k")
	if len(vs) != 2 {
		t.Fatalf("rows %d (want versioned + one null slot)", len(vs))
	}
	m := e.latest("k")
	if m.VersionID != "null" || string(e.read(m)) != "null-two" {
		t.Fatalf("latest %+v", m)
	}
	if !e.versionedExists("k", r1.VersionID) {
		t.Fatal("versioned file removed by suspended overwrite")
	}
}

// --- Delete ----------------------------------------------------------------

func TestDelete_Unversioned(t *testing.T) {
	e := newEnv(t)
	e.put("k", []byte("x"), service.PutInput{})
	res, err := e.svc.Delete(e.bucket, "k", "")
	if err != nil {
		t.Fatal(err)
	}
	if res.DeleteMarker || res.VersionID != "" {
		t.Fatalf("result %+v", res)
	}
	if e.store.ObjectExists(bucketName, "k") {
		t.Fatal("file still on disk")
	}
	if len(e.versions("k")) != 0 {
		t.Fatal("rows remain")
	}
	// Deleting a missing key is not an error (S3 semantics).
	if _, err := e.svc.Delete(e.bucket, "missing", ""); err != nil {
		t.Fatalf("delete missing: %v", err)
	}
}

func TestDelete_EnabledCreatesMarker(t *testing.T) {
	e := newEnv(t)
	e.setVersioning("Enabled")
	r := e.put("k", []byte("x"), service.PutInput{})
	res, err := e.svc.Delete(e.bucket, "k", "")
	if err != nil {
		t.Fatal(err)
	}
	if !res.DeleteMarker || res.VersionID == "" || res.VersionID == "null" {
		t.Fatalf("result %+v", res)
	}
	vs := e.versions("k")
	if len(vs) != 2 {
		t.Fatalf("rows %d", len(vs))
	}
	if !vs[0].IsDeleteMarker || !vs[0].IsLatest || vs[0].VersionID != res.VersionID {
		t.Fatalf("marker row %+v", vs[0])
	}
	if vs[1].IsLatest {
		t.Fatal("old version still latest")
	}
	if !e.versionedExists("k", r.VersionID) {
		t.Fatal("versioned bytes removed by marker creation")
	}
	m := e.latest("k")
	if m == nil || !m.IsDeleteMarker {
		t.Fatalf("latest %+v", m)
	}
}

func TestDelete_SuspendedNullMarker(t *testing.T) {
	e := newEnv(t)
	e.setVersioning("Enabled")
	r1 := e.put("k", []byte("versioned"), service.PutInput{})
	e.setVersioning("Suspended")
	e.put("k", []byte("null"), service.PutInput{})
	if !e.store.ObjectExists(bucketName, "k") {
		t.Fatal("null version file missing before delete")
	}
	res, err := e.svc.Delete(e.bucket, "k", "")
	if err != nil {
		t.Fatal(err)
	}
	if !res.DeleteMarker || res.VersionID != "null" {
		t.Fatalf("result %+v", res)
	}
	if e.store.ObjectExists(bucketName, "k") {
		t.Fatal("null version file not removed")
	}
	vs := e.versions("k")
	if len(vs) != 2 {
		t.Fatalf("rows %d", len(vs))
	}
	if !vs[0].IsDeleteMarker || vs[0].VersionID != "null" || !vs[0].IsLatest {
		t.Fatalf("marker %+v", vs[0])
	}
	if vs[1].VersionID != r1.VersionID || !e.versionedExists("k", r1.VersionID) {
		t.Fatalf("versioned row/file gone: %+v", vs[1])
	}
	// Deleting again replaces the null marker with a fresh null marker; still 2 rows.
	if _, err := e.svc.Delete(e.bucket, "k", ""); err != nil {
		t.Fatal(err)
	}
	if n := len(e.versions("k")); n != 2 {
		t.Fatalf("rows after second suspended delete: %d", n)
	}
}

func TestDelete_ByVersionPromotesPrevious(t *testing.T) {
	e := newEnv(t)
	e.setVersioning("Enabled")
	r1 := e.put("k", []byte("one"), service.PutInput{})
	r2 := e.put("k", []byte("two"), service.PutInput{})

	res, err := e.svc.Delete(e.bucket, "k", r2.VersionID)
	if err != nil {
		t.Fatal(err)
	}
	if res.DeleteMarker || res.VersionID != r2.VersionID {
		t.Fatalf("result %+v", res)
	}
	if e.versionedExists("k", r2.VersionID) {
		t.Fatal("deleted version bytes remain")
	}
	m := e.latest("k")
	if m == nil || m.VersionID != r1.VersionID || !m.IsLatest {
		t.Fatalf("promoted %+v", m)
	}
	if string(e.read(m)) != "one" {
		t.Fatal("promoted body mismatch")
	}
	// Unknown version.
	if _, err := e.svc.Delete(e.bucket, "k", "does-not-exist"); !errors.Is(err, service.ErrNoSuchVersion) {
		t.Fatalf("want ErrNoSuchVersion, got %v", err)
	}
	// Removing a delete marker by version restores the object.
	mk, _ := e.svc.Delete(e.bucket, "k", "")
	res, err = e.svc.Delete(e.bucket, "k", mk.VersionID)
	if err != nil || !res.DeleteMarker {
		t.Fatalf("marker removal: %+v %v", res, err)
	}
	if m := e.latest("k"); m == nil || m.IsDeleteMarker || m.VersionID != r1.VersionID {
		t.Fatalf("after marker removal %+v", m)
	}
	// Removing the last version leaves nothing.
	if _, err := e.svc.Delete(e.bucket, "k", r1.VersionID); err != nil {
		t.Fatal(err)
	}
	if m := e.latest("k"); m != nil {
		t.Fatalf("expected no rows, got %+v", m)
	}
}

func TestDeleteAllVersions(t *testing.T) {
	e := newEnv(t)
	e.put("k", []byte("plain"), service.PutInput{})
	e.setVersioning("Enabled")
	r1 := e.put("k", []byte("v1"), service.PutInput{})
	r2 := e.put("k", []byte("v2"), service.PutInput{})
	e.svc.Delete(e.bucket, "k", "") // marker
	if n := len(e.versions("k")); n != 4 {
		t.Fatalf("setup rows %d", n)
	}
	if err := e.svc.DeleteAllVersions(e.bucket, "k"); err != nil {
		t.Fatal(err)
	}
	if n := len(e.versions("k")); n != 0 {
		t.Fatalf("rows remain: %d", n)
	}
	if e.store.ObjectExists(bucketName, "k") || e.versionedExists("k", r1.VersionID) || e.versionedExists("k", r2.VersionID) {
		t.Fatal("bytes remain on disk")
	}
}

// --- quota / size / verify -------------------------------------------------

func TestPut_Quota(t *testing.T) {
	e := newEnv(t)
	if err := e.db.SetBucketQuota(bucketName, 100); err != nil {
		t.Fatal(err)
	}
	e.refresh()
	if e.bucket.QuotaBytes != 100 {
		t.Fatalf("quota %d", e.bucket.QuotaBytes)
	}
	first := bytes.Repeat([]byte("a"), 80)
	e.put("a", first, service.PutInput{})

	// 30 more bytes under a different key: rejected up front, nothing written.
	_, err := e.svc.Put(service.PutInput{Bucket: e.bucket, Key: "b", Body: bytes.NewReader(bytes.Repeat([]byte("b"), 30)), DeclaredSize: 30})
	if !errors.Is(err, service.ErrQuotaExceeded) {
		t.Fatalf("want ErrQuotaExceeded, got %v", err)
	}
	if e.store.ObjectExists(bucketName, "b") || e.latest("b") != nil {
		t.Fatal("over-quota object persisted")
	}
	// Same rejection when the size is unknown up front (checked at Verify time).
	_, err = e.svc.Put(service.PutInput{Bucket: e.bucket, Key: "b", Body: bytes.NewReader(bytes.Repeat([]byte("b"), 30)), DeclaredSize: -1})
	if !errors.Is(err, service.ErrQuotaExceeded) {
		t.Fatalf("streamed: want ErrQuotaExceeded, got %v", err)
	}
	if e.store.ObjectExists(bucketName, "b") {
		t.Fatal("streamed over-quota object persisted")
	}
	if len(e.tmpFiles()) != 0 {
		t.Fatalf("temp files left: %v", e.tmpFiles())
	}
	// First object intact.
	if got := e.read(e.latest("a")); !bytes.Equal(got, first) {
		t.Fatal("first object damaged")
	}
	// Overwrite of the 80-byte key with 90 bytes: old size is credited.
	e.put("a", bytes.Repeat([]byte("c"), 90), service.PutInput{})
	if usage, _ := e.db.GetBucketUsage(e.bucket.ID); usage != 90 {
		t.Fatalf("usage %d", usage)
	}
	// 101 bytes never fits.
	_, err = e.svc.Put(service.PutInput{Bucket: e.bucket, Key: "a", Body: bytes.NewReader(make([]byte, 101)), DeclaredSize: 101})
	if !errors.Is(err, service.ErrQuotaExceeded) {
		t.Fatalf("want ErrQuotaExceeded, got %v", err)
	}
	if got := e.read(e.latest("a")); len(got) != 90 {
		t.Fatalf("object damaged after rejection: %d bytes", len(got))
	}
}

func TestPut_QuotaVersioningEnabledDoesNotCredit(t *testing.T) {
	e := newEnv(t)
	e.db.SetBucketQuota(bucketName, 100)
	e.setVersioning("Enabled")
	e.put("a", bytes.Repeat([]byte("a"), 80), service.PutInput{})
	// A new version of the same key adds to usage; 80 + 30 > 100.
	_, err := e.svc.Put(service.PutInput{Bucket: e.bucket, Key: "a", Body: bytes.NewReader(make([]byte, 30)), DeclaredSize: 30})
	if !errors.Is(err, service.ErrQuotaExceeded) {
		t.Fatalf("want ErrQuotaExceeded, got %v", err)
	}
	if n := len(e.versions("a")); n != 1 {
		t.Fatalf("rows %d", n)
	}
}

type mustNotRead struct{ t *testing.T }

func (m mustNotRead) Read([]byte) (int, error) {
	m.t.Error("body was read although the request should be rejected up front")
	return 0, io.EOF
}

func TestPut_DeclaredSizeTooLarge(t *testing.T) {
	e := newEnv(t)
	_, err := e.svc.Put(service.PutInput{Bucket: e.bucket, Key: "big", Body: mustNotRead{t}, DeclaredSize: 11, MaxSize: 10})
	if !errors.Is(err, service.ErrTooLarge) || !errors.Is(err, storage.ErrTooLarge) {
		t.Fatalf("want ErrTooLarge, got %v", err)
	}
	if e.store.ObjectExists(bucketName, "big") || len(e.tmpFiles()) != 0 {
		t.Fatal("disk touched")
	}
	// Streaming past MaxSize is also rejected and leaves no temp file.
	_, err = e.svc.Put(service.PutInput{Bucket: e.bucket, Key: "big", Body: bytes.NewReader(make([]byte, 11)), DeclaredSize: -1, MaxSize: 10})
	if !errors.Is(err, service.ErrTooLarge) {
		t.Fatalf("streamed: want ErrTooLarge, got %v", err)
	}
	if e.store.ObjectExists(bucketName, "big") || len(e.tmpFiles()) != 0 {
		t.Fatal("disk touched by streamed oversize")
	}
}

func TestPut_VerifyFailureKeepsPrevious(t *testing.T) {
	e := newEnv(t)
	e.put("k", []byte("good"), service.PutInput{})
	boom := errors.New("checksum mismatch")
	var gotSize int64
	_, err := e.svc.Put(service.PutInput{
		Bucket: e.bucket, Key: "k", Body: bytes.NewReader([]byte("replacement")), DeclaredSize: 11,
		Verify: func(size int64, md5sum []byte) error {
			gotSize = size
			if len(md5sum) != 16 {
				t.Errorf("md5 len %d", len(md5sum))
			}
			return boom
		},
	})
	if !errors.Is(err, boom) {
		t.Fatalf("want verify error, got %v", err)
	}
	if gotSize != 11 {
		t.Fatalf("verify saw size %d", gotSize)
	}
	if got := e.read(e.latest("k")); string(got) != "good" {
		t.Fatalf("previous object replaced: %q", got)
	}
	if n := len(e.versions("k")); n != 1 {
		t.Fatalf("rows %d", n)
	}
	if tmp := e.tmpFiles(); len(tmp) != 0 {
		t.Fatalf("temp files left behind: %v", tmp)
	}
	// Same on a versioned bucket: no new row, no new file.
	e.setVersioning("Enabled")
	_, err = e.svc.Put(service.PutInput{
		Bucket: e.bucket, Key: "k", Body: bytes.NewReader([]byte("v")), DeclaredSize: 1,
		Verify: func(int64, []byte) error { return boom },
	})
	if !errors.Is(err, boom) {
		t.Fatal(err)
	}
	if n := len(e.versions("k")); n != 1 {
		t.Fatalf("versioned rows %d", n)
	}
	if tmp := e.tmpFiles(); len(tmp) != 0 {
		t.Fatalf("temp files left behind: %v", tmp)
	}
}

func TestVersionHelpers(t *testing.T) {
	if service.IsRealVersion("") || service.IsRealVersion("null") || !service.IsRealVersion("123-abc") {
		t.Fatal("IsRealVersion")
	}
	if v := service.VersionIDForPut(&db.Bucket{}); v != "" {
		t.Fatalf("unversioned put id %q", v)
	}
	if v := service.VersionIDForPut(&db.Bucket{Versioning: "Suspended"}); v != "null" {
		t.Fatalf("suspended put id %q", v)
	}
	if v := service.VersionIDForPut(&db.Bucket{Versioning: "Enabled"}); !service.IsRealVersion(v) {
		t.Fatalf("enabled put id %q", v)
	}
	if !service.IsSystemMetaKey(service.MetaCacheControl) || service.IsSystemMetaKey("owner") {
		t.Fatal("IsSystemMetaKey")
	}
	a, b := service.NewVersionID(), service.NewVersionID()
	if a == b {
		t.Fatal("version ids collide")
	}
}
