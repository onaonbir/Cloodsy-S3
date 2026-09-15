package webdav

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/onaonbir/Cloodsy-S3/config"
	"github.com/onaonbir/Cloodsy-S3/db"
	"github.com/onaonbir/Cloodsy-S3/service"
	"github.com/onaonbir/Cloodsy-S3/storage"
)

const (
	testBucket = "davtest"
	testAccess = "AKIADAVTEST"
	testSecret = "davsecret123"
)

type env struct {
	t      *testing.T
	db     *db.DB
	bucket *db.Bucket
	srv    *httptest.Server
}

func newEnv(t *testing.T) *env {
	t.Helper()
	database, err := db.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { database.Close() })
	store, err := storage.NewFileSystem(t.TempDir())
	if err != nil {
		t.Fatalf("storage: %v", err)
	}
	objects := service.New(database, store, slog.Default())

	bucket, err := database.CreateBucket(testBucket, "")
	if err != nil {
		t.Fatalf("create bucket: %v", err)
	}
	if _, err := database.CreateCredential(bucket.ID, "dav", testAccess, testSecret, "read-write"); err != nil {
		t.Fatalf("create credential: %v", err)
	}
	if err := database.SetBucketWebDAVEnabled(testBucket, true); err != nil {
		t.Fatalf("enable webdav: %v", err)
	}
	if err := database.SetBucketVersioning(testBucket, "Enabled"); err != nil {
		t.Fatalf("enable versioning: %v", err)
	}
	bucket, _ = database.GetBucket(testBucket)

	cfg := &config.Config{}
	cfg.WebDAV.Prefix = "/"
	cfg.WebDAV.LockTimeout = "10m"
	srv := httptest.NewServer(newHandler(database, objects, cfg, slog.Default()))
	t.Cleanup(srv.Close)
	return &env{t: t, db: database, bucket: bucket, srv: srv}
}

// do issues an authenticated request and returns the response with its body read.
func (e *env) do(method, path string, body []byte, hdr map[string]string) (*http.Response, []byte) {
	e.t.Helper()
	return e.doAs(testAccess, testSecret, method, path, body, hdr)
}

func (e *env) doAs(user, pass, method, path string, body []byte, hdr map[string]string) (*http.Response, []byte) {
	e.t.Helper()
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, e.srv.URL+path, rd)
	if err != nil {
		e.t.Fatalf("new request: %v", err)
	}
	req.SetBasicAuth(user, pass)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp, out
}

func (e *env) latestRows(key string) (latest, total int) {
	e.t.Helper()
	if err := e.db.Reader().QueryRow(`SELECT COUNT(*) FROM objects WHERE bucket_id = ? AND key = ? AND is_latest = 1`, e.bucket.ID, key).Scan(&latest); err != nil {
		e.t.Fatalf("count latest: %v", err)
	}
	if err := e.db.Reader().QueryRow(`SELECT COUNT(*) FROM objects WHERE bucket_id = ? AND key = ?`, e.bucket.ID, key).Scan(&total); err != nil {
		e.t.Fatalf("count total: %v", err)
	}
	return latest, total
}

func TestPutGetPropfindDelete(t *testing.T) {
	e := newEnv(t)
	content := []byte("hello webdav\n")

	resp, _ := e.do("PUT", "/hello.txt", content, nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("PUT status = %d", resp.StatusCode)
	}
	meta, err := e.db.GetObjectMeta(e.bucket.ID, "hello.txt")
	if err != nil || meta == nil {
		t.Fatalf("meta after PUT: %v %v", meta, err)
	}
	if got := resp.Header.Get("ETag"); got != meta.ETag {
		t.Errorf("PUT ETag = %q, DB ETag = %q", got, meta.ETag)
	}
	if !strings.HasPrefix(meta.ContentType, "text/plain") {
		t.Errorf("content type = %q, want text/plain", meta.ContentType)
	}
	if !service.IsRealVersion(meta.VersionID) {
		t.Errorf("expected a real version id on a versioned bucket, got %q", meta.VersionID)
	}

	resp, body := e.do("GET", "/hello.txt", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET status = %d", resp.StatusCode)
	}
	if !bytes.Equal(body, content) {
		t.Fatalf("GET body = %q, want %q", body, content)
	}
	if got := resp.Header.Get("Cache-Control"); !strings.Contains(got, "no-cache") {
		t.Errorf("Cache-Control = %q", got)
	}

	resp, body = e.do("PROPFIND", "/", nil, map[string]string{"Depth": "1"})
	if resp.StatusCode != 207 {
		t.Fatalf("PROPFIND status = %d", resp.StatusCode)
	}
	if !strings.Contains(string(body), "hello.txt") {
		t.Fatalf("PROPFIND listing missing hello.txt: %s", body)
	}
	if !strings.Contains(string(body), meta.ETag) {
		t.Errorf("PROPFIND getetag does not match stored ETag %s", meta.ETag)
	}

	// Second PUT: still exactly one is_latest row, two versions total, and
	// GET serves the newest bytes.
	content2 := []byte("second version")
	resp, _ = e.do("PUT", "/hello.txt", content2, nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("PUT#2 status = %d", resp.StatusCode)
	}
	if latest, total := e.latestRows("hello.txt"); latest != 1 || total != 2 {
		t.Fatalf("after two PUTs: latest=%d total=%d, want 1/2", latest, total)
	}
	resp, body = e.do("GET", "/hello.txt", nil, nil)
	if resp.StatusCode != http.StatusOK || !bytes.Equal(body, content2) {
		t.Fatalf("GET after PUT#2: status=%d body=%q", resp.StatusCode, body)
	}

	// DELETE → delete marker on a versioning-enabled bucket.
	resp, _ = e.do("DELETE", "/hello.txt", nil, nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("DELETE status = %d", resp.StatusCode)
	}
	meta, err = e.db.GetObjectMeta(e.bucket.ID, "hello.txt")
	if err != nil || meta == nil || !meta.IsDeleteMarker {
		t.Fatalf("after DELETE expected delete marker, got %+v (err %v)", meta, err)
	}
	if latest, total := e.latestRows("hello.txt"); latest != 1 || total != 3 {
		t.Fatalf("after DELETE: latest=%d total=%d, want 1/3", latest, total)
	}
	resp, _ = e.do("GET", "/hello.txt", nil, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("GET after DELETE status = %d, want 404", resp.StatusCode)
	}
	resp, body = e.do("PROPFIND", "/", nil, map[string]string{"Depth": "1"})
	if strings.Contains(string(body), "hello.txt") {
		t.Fatalf("PROPFIND still lists deleted key: %s", body)
	}
}

func TestEmptyPutAndDotfiles(t *testing.T) {
	e := newEnv(t)
	for _, p := range []string{"/empty.bin", "/.DS_Store", "/._resource", "/dir/.hidden"} {
		resp, _ := e.do("PUT", p, []byte{}, nil)
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("PUT %s status = %d", p, resp.StatusCode)
		}
		resp, body := e.do("GET", p, nil, nil)
		if resp.StatusCode != http.StatusOK || len(body) != 0 {
			t.Fatalf("GET %s: status=%d len=%d", p, resp.StatusCode, len(body))
		}
	}
}

func TestPropfindInfinityRejected(t *testing.T) {
	e := newEnv(t)
	resp, body := e.do("PROPFIND", "/", nil, map[string]string{"Depth": "infinity"})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
	if !strings.Contains(string(body), "propfind-finite-depth") {
		t.Fatalf("body missing precondition: %s", body)
	}
	// Missing Depth is downgraded to 1 rather than walking the whole bucket.
	resp, _ = e.do("PROPFIND", "/", nil, nil)
	if resp.StatusCode != 207 {
		t.Fatalf("PROPFIND without Depth status = %d, want 207", resp.StatusCode)
	}
}

func TestOptionsHeaders(t *testing.T) {
	e := newEnv(t)
	resp, _ := e.do("OPTIONS", "/", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("OPTIONS status = %d", resp.StatusCode)
	}
	if got := resp.Header.Get("DAV"); got != "1, 2" {
		t.Errorf("DAV header = %q", got)
	}
	if got := resp.Header.Get("MS-Author-Via"); got != "DAV" {
		t.Errorf("MS-Author-Via = %q", got)
	}
}

func TestMkcolMoveDirectory(t *testing.T) {
	e := newEnv(t)
	if resp, _ := e.do("MKCOL", "/dir", nil, nil); resp.StatusCode != http.StatusCreated {
		t.Fatalf("MKCOL status = %d", resp.StatusCode)
	}
	if resp, _ := e.do("PUT", "/dir/a.txt", []byte("A"), nil); resp.StatusCode != http.StatusCreated {
		t.Fatalf("PUT status = %d", resp.StatusCode)
	}
	if resp, _ := e.do("PUT", "/dir/sub/b.txt", []byte("B"), nil); resp.StatusCode != http.StatusCreated {
		t.Fatalf("PUT status = %d", resp.StatusCode)
	}

	// Into its own subtree → 403.
	resp, _ := e.do("MOVE", "/dir", nil, map[string]string{"Destination": e.srv.URL + "/dir/inner"})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("MOVE into subtree status = %d, want 403", resp.StatusCode)
	}

	resp, _ = e.do("MOVE", "/dir", nil, map[string]string{"Destination": e.srv.URL + "/dir2"})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("MOVE status = %d, want 201", resp.StatusCode)
	}
	for p, want := range map[string]string{"/dir2/a.txt": "A", "/dir2/sub/b.txt": "B"} {
		resp, body := e.do("GET", p, nil, nil)
		if resp.StatusCode != http.StatusOK || string(body) != want {
			t.Fatalf("GET %s after MOVE: status=%d body=%q", p, resp.StatusCode, body)
		}
	}
	for _, p := range []string{"/dir/a.txt", "/dir/sub/b.txt", "/dir"} {
		if resp, _ := e.do("PROPFIND", p, nil, map[string]string{"Depth": "0"}); resp.StatusCode != http.StatusNotFound {
			t.Fatalf("PROPFIND %s after MOVE status = %d, want 404", p, resp.StatusCode)
		}
	}
	// The moved keys received delete markers rather than being purged.
	m, _ := e.db.GetObjectMeta(e.bucket.ID, "dir/a.txt")
	if m == nil || !m.IsDeleteMarker {
		t.Fatalf("expected delete marker for dir/a.txt, got %+v", m)
	}

	// Recursive DELETE of the new tree.
	if resp, _ := e.do("DELETE", "/dir2", nil, nil); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("DELETE dir2 status = %d", resp.StatusCode)
	}
	if resp, _ := e.do("PROPFIND", "/dir2", nil, map[string]string{"Depth": "0"}); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("PROPFIND /dir2 after DELETE status = %d, want 404", resp.StatusCode)
	}
}

func TestInvalidKeys(t *testing.T) {
	e := newEnv(t)
	if resp, _ := e.do("PUT", "/a/../b", []byte("x"), nil); resp.StatusCode != http.StatusForbidden {
		t.Errorf("dot segment status = %d, want 403", resp.StatusCode)
	}
	if resp, _ := e.do("PUT", "/nul%00byte", []byte("x"), nil); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("NUL status = %d, want 400", resp.StatusCode)
	}
	long := "/" + strings.Repeat("k", 1025)
	if resp, _ := e.do("PUT", long, []byte("x"), nil); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("long key status = %d, want 400", resp.StatusCode)
	}
	if resp, _ := e.do("MOVE", "/x", nil, map[string]string{"Destination": e.srv.URL + "/../y"}); resp.StatusCode != http.StatusForbidden {
		t.Errorf("dot segment in Destination status = %d, want 403", resp.StatusCode)
	}
}

func TestQuotaExceeded(t *testing.T) {
	e := newEnv(t)
	if err := e.db.SetBucketQuota(testBucket, 10); err != nil {
		t.Fatal(err)
	}
	resp, _ := e.do("PUT", "/big.bin", bytes.Repeat([]byte("x"), 64), nil)
	if resp.StatusCode != http.StatusInsufficientStorage {
		t.Fatalf("status = %d, want 507", resp.StatusCode)
	}
	if m, _ := e.db.GetObjectMeta(e.bucket.ID, "big.bin"); m != nil {
		t.Fatalf("rejected upload left metadata: %+v", m)
	}
}

func TestAuthLimiter(t *testing.T) {
	e := newEnv(t)
	for i := 0; i < authMaxFailures; i++ {
		resp, _ := e.doAs(testAccess, "wrong", "PROPFIND", "/", nil, map[string]string{"Depth": "0"})
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("attempt %d status = %d, want 401", i, resp.StatusCode)
		}
	}
	// Even the right password is refused while locked out.
	resp, _ := e.do("PROPFIND", "/", nil, map[string]string{"Depth": "0"})
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("locked status = %d, want 429", resp.StatusCode)
	}
	if resp.Header.Get("Retry-After") == "" {
		t.Errorf("missing Retry-After")
	}
}

func TestReadOnlyCredential(t *testing.T) {
	e := newEnv(t)
	if _, err := e.db.CreateCredential(e.bucket.ID, "ro", "AKIARO", "rosecret", "read-only"); err != nil {
		t.Fatal(err)
	}
	if resp, _ := e.doAs("AKIARO", "rosecret", "PUT", "/x.txt", []byte("x"), nil); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("read-only PUT status = %d, want 403", resp.StatusCode)
	}
	if resp, _ := e.doAs("AKIARO", "rosecret", "PROPFIND", "/", nil, map[string]string{"Depth": "1"}); resp.StatusCode != 207 {
		t.Fatalf("read-only PROPFIND status = %d, want 207", resp.StatusCode)
	}
}

func TestLockCap(t *testing.T) {
	e := newEnv(t)
	body := `<?xml version="1.0" encoding="utf-8"?><D:lockinfo xmlns:D="DAV:"><D:lockscope><D:exclusive/></D:lockscope><D:locktype><D:write/></D:locktype><D:owner>t</D:owner></D:lockinfo>`
	resp, out := e.do("LOCK", "/locked.txt", []byte(body), map[string]string{"Timeout": "Infinite", "Depth": "0"})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("LOCK status = %d: %s", resp.StatusCode, out)
	}
	if !strings.Contains(string(out), "Second-600") {
		t.Fatalf("lock timeout not capped at 10m: %s", out)
	}
}
