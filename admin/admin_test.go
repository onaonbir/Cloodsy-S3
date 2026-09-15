package admin

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/onaonbir/Cloodsy-S3/config"
	"github.com/onaonbir/Cloodsy-S3/db"
	"github.com/onaonbir/Cloodsy-S3/service"
	"github.com/onaonbir/Cloodsy-S3/storage"
	"golang.org/x/crypto/bcrypt"
)

func newTestHandler(t *testing.T, cfg *config.Config) *Handler {
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
	if cfg == nil {
		cfg = &config.Config{}
	}
	cfg.Storage.RootDir = store.RootDir
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	return New(database, store, cfg, logger)
}

func createAdmin(t *testing.T, h *Handler, user, pass string) {
	t.Helper()
	hash, err := bcrypt.GenerateFromPassword([]byte(pass), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.DB.CreateAdmin(user, string(hash)); err != nil {
		t.Fatalf("create admin: %v", err)
	}
}

func doLogin(h *Handler, remote, user, pass string) *httptest.ResponseRecorder {
	body, _ := json.Marshal(map[string]string{"username": user, "password": pass})
	req := httptest.NewRequest(http.MethodPost, "/admin/login", bytes.NewReader(body))
	req.RemoteAddr = remote
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestClientIP(t *testing.T) {
	trusted := parseTrustedProxies([]string{"10.0.0.0/8", "::1"}, nil)

	cases := []struct {
		name    string
		remote  string
		xff     string
		trusted []*net.IPNet
		want    string
	}{
		{"plain ipv4", "203.0.113.5:4321", "", nil, "203.0.113.5"},
		{"ipv6 with port", "[2001:db8::1]:443", "", nil, "2001:db8::1"},
		{"xff ignored without trusted proxies", "203.0.113.5:4321", "1.2.3.4", nil, "203.0.113.5"},
		{"xff ignored from untrusted peer", "203.0.113.5:4321", "1.2.3.4", trusted, "203.0.113.5"},
		{"xff honored from trusted peer", "10.1.2.3:555", "1.2.3.4, 10.1.2.3", trusted, "1.2.3.4"},
		{"xff honored from trusted v6 peer", "[::1]:555", "198.51.100.7", trusted, "198.51.100.7"},
		{"xff with port form", "10.1.2.3:555", "1.2.3.4:8080", trusted, "1.2.3.4"},
		{"garbage xff falls back to peer", "10.1.2.3:555", "not-an-ip", trusted, "10.1.2.3"},
		{"no port at all", "203.0.113.9", "", trusted, "203.0.113.9"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/admin/status", nil)
			req.RemoteAddr = tc.remote
			if tc.xff != "" {
				req.Header.Set("X-Forwarded-For", tc.xff)
			}
			if got := clientIP(req, tc.trusted); got != tc.want {
				t.Fatalf("clientIP = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestLoginLimiterPerIPAndPerUser(t *testing.T) {
	h := newTestHandler(t, nil)
	createAdmin(t, h, "alice", "correct-horse")
	createAdmin(t, h, "bob", "battery-staple")

	// Five failures from one IP lock that IP ...
	for i := 0; i < maxLoginAttempts; i++ {
		if rec := doLogin(h, "198.51.100.1:1000", "alice", "wrong"); rec.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d: got %d, want 401", i, rec.Code)
		}
	}
	if rec := doLogin(h, "198.51.100.1:1000", "alice", "correct-horse"); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("locked ip: got %d, want 429", rec.Code)
	}
	// ... and the username too, even from a fresh address.
	if rec := doLogin(h, "198.51.100.2:1000", "alice", "correct-horse"); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("locked user from new ip: got %d, want 429", rec.Code)
	}
	// Another user from another address is unaffected.
	if rec := doLogin(h, "198.51.100.3:1000", "bob", "battery-staple"); rec.Code != http.StatusOK {
		t.Fatalf("unrelated user: got %d, want 200: %s", rec.Code, rec.Body.String())
	}

	// Once the window lapses the counters are purged and alice can log in.
	h.loginMu.Lock()
	for _, a := range h.loginRate {
		a.LockedAt = a.LockedAt.Add(-2 * loginLockDuration)
		a.WindowStart = a.WindowStart.Add(-2 * loginLockDuration)
	}
	h.loginMu.Unlock()
	if rec := doLogin(h, "198.51.100.1:1000", "alice", "correct-horse"); rec.Code != http.StatusOK {
		t.Fatalf("after lock expiry: got %d, want 200: %s", rec.Code, rec.Body.String())
	}
	h.loginMu.Lock()
	remaining := len(h.loginRate)
	h.loginMu.Unlock()
	if remaining != 0 {
		t.Fatalf("expected counters reset after success, got %d entries", remaining)
	}
}

func TestLoginUnknownUserCountsAndIsIndistinguishable(t *testing.T) {
	h := newTestHandler(t, nil)
	createAdmin(t, h, "alice", "correct-horse")

	r1 := doLogin(h, "192.0.2.1:1", "nobody", "whatever1")
	r2 := doLogin(h, "192.0.2.1:1", "alice", "whatever1")
	if r1.Code != http.StatusUnauthorized || r2.Code != http.StatusUnauthorized {
		t.Fatalf("codes %d/%d, want 401/401", r1.Code, r2.Code)
	}
	if r1.Body.String() != r2.Body.String() {
		t.Fatalf("bodies differ: %q vs %q", r1.Body.String(), r2.Body.String())
	}
	h.loginMu.Lock()
	_, ipTracked := h.loginRate["ip:192.0.2.1"]
	_, userTracked := h.loginRate["user:nobody"]
	h.loginMu.Unlock()
	if !ipTracked || !userTracked {
		t.Fatalf("unknown-user failure not counted (ip=%v user=%v)", ipTracked, userTracked)
	}
}

func TestLoginCleanupPurgesExpiredCounters(t *testing.T) {
	h := newTestHandler(t, nil)
	h.recordLoginFailure("ip:1.1.1.1")
	h.recordLoginFailure("ip:2.2.2.2")
	h.loginMu.Lock()
	h.loginRate["ip:1.1.1.1"].WindowStart = time.Now().Add(-2 * loginLockDuration)
	h.loginMu.Unlock()

	h.cleanupOnce(time.Now())

	h.loginMu.Lock()
	defer h.loginMu.Unlock()
	if _, ok := h.loginRate["ip:1.1.1.1"]; ok {
		t.Fatal("expired counter (Count>0) was not purged")
	}
	if _, ok := h.loginRate["ip:2.2.2.2"]; !ok {
		t.Fatal("live counter was purged")
	}
}

func TestLoginTableIsCapped(t *testing.T) {
	h := newTestHandler(t, nil)
	for i := 0; i < maxLoginEntries+50; i++ {
		h.recordLoginFailure(fmt.Sprintf("ip:10.0.%d.%d", i/256, i%256))
	}
	h.loginMu.Lock()
	defer h.loginMu.Unlock()
	if len(h.loginRate) > maxLoginEntries {
		t.Fatalf("table grew to %d entries, cap is %d", len(h.loginRate), maxLoginEntries)
	}
	if _, ok := h.loginRate["ip:10.0.0.0"]; ok {
		t.Fatal("oldest entry should have been evicted first")
	}
}

func TestSessionRevokedOnPasswordChangeAndDelete(t *testing.T) {
	h := newTestHandler(t, nil)
	createAdmin(t, h, "root", "correct-horse")
	createAdmin(t, h, "alice", "correct-horse")

	login := func(user string) string {
		rec := doLogin(h, "127.0.0.1:1", user, "correct-horse")
		if rec.Code != http.StatusOK {
			t.Fatalf("login %s: %d %s", user, rec.Code, rec.Body.String())
		}
		var out struct {
			Token string `json:"token"`
		}
		json.Unmarshal(rec.Body.Bytes(), &out)
		return out.Token
	}
	authed := func(method, path, token, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	rootTok := login("root")
	aliceTok := login("alice")
	if rec := authed(http.MethodGet, "/admin/status", aliceTok, ""); rec.Code != http.StatusOK {
		t.Fatalf("alice status: %d", rec.Code)
	}

	// Malformed body must not rotate the password.
	if rec := authed(http.MethodPut, "/admin/admins/alice/password", rootTok, `{"password": `); rec.Code != http.StatusBadRequest {
		t.Fatalf("malformed password body: got %d, want 400", rec.Code)
	}
	if rec := authed(http.MethodGet, "/admin/status", aliceTok, ""); rec.Code != http.StatusOK {
		t.Fatalf("alice session should survive a rejected request: %d", rec.Code)
	}
	if rec := doLogin(h, "127.0.0.1:2", "alice", "correct-horse"); rec.Code != http.StatusOK {
		t.Fatalf("password must be unchanged after malformed body: %d", rec.Code)
	}

	// Too-long and too-short passwords are rejected with 400.
	long := strings.Repeat("x", 73)
	if rec := authed(http.MethodPut, "/admin/admins/alice/password", rootTok, `{"password":"`+long+`"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("73-byte password: got %d, want 400", rec.Code)
	}
	if rec := authed(http.MethodPut, "/admin/admins/alice/password", rootTok, `{"password":"short"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("short password: got %d, want 400", rec.Code)
	}

	// A real change revokes alice's session.
	if rec := authed(http.MethodPut, "/admin/admins/alice/password", rootTok, `{"password":"new-password-1"}`); rec.Code != http.StatusOK {
		t.Fatalf("password change: %d %s", rec.Code, rec.Body.String())
	}
	if rec := authed(http.MethodGet, "/admin/status", aliceTok, ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("alice session should be revoked after password change: %d", rec.Code)
	}

	// Deleting the account revokes any remaining session.
	rec := doLogin(h, "127.0.0.1:3", "alice", "new-password-1")
	var out struct {
		Token string `json:"token"`
	}
	json.Unmarshal(rec.Body.Bytes(), &out)
	if rec := authed(http.MethodDelete, "/admin/admins/alice", rootTok, ""); rec.Code != http.StatusOK {
		t.Fatalf("delete admin: %d %s", rec.Code, rec.Body.String())
	}
	if rec := authed(http.MethodGet, "/admin/status", out.Token, ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("deleted admin session should be revoked: %d", rec.Code)
	}
}

func TestCredentialCreateRejectsMalformedBody(t *testing.T) {
	h := newTestHandler(t, nil)
	createAdmin(t, h, "root", "correct-horse")
	tok := func() string {
		rec := doLogin(h, "127.0.0.1:1", "root", "correct-horse")
		var out struct {
			Token string `json:"token"`
		}
		json.Unmarshal(rec.Body.Bytes(), &out)
		return out.Token
	}()
	if _, err := h.DB.CreateBucket("demo", ""); err != nil {
		t.Fatal(err)
	}
	post := func(body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/admin/buckets/demo/credentials", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+tok)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	if rec := post(`{"permission": "read-`); rec.Code != http.StatusBadRequest {
		t.Fatalf("malformed: got %d, want 400", rec.Code)
	}
	if rec := post(`{"permission": "admin"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad permission: got %d, want 400", rec.Code)
	}
	if rec := post(`{"name":"ro","permission":"read-only"}`); rec.Code != http.StatusCreated {
		t.Fatalf("valid: got %d %s", rec.Code, rec.Body.String())
	}
	if rec := post(``); rec.Code != http.StatusCreated {
		t.Fatalf("empty body (defaults): got %d %s", rec.Code, rec.Body.String())
	}
}

func TestValidatePrefix(t *testing.T) {
	bad := []string{"", "/abs", "a/../b", "..", "nul\x00", "back\\slash", strings.Repeat("a", prefixMaxLen+1)}
	for _, p := range bad {
		if validatePrefix(p) == "" {
			t.Errorf("prefix %q should be rejected", p)
		}
	}
	good := []string{"photos/", "photos/2024", "a.b/c..d/"}
	for _, p := range good {
		if msg := validatePrefix(p); msg != "" {
			t.Errorf("prefix %q rejected: %s", p, msg)
		}
	}
}

func adminToken(t *testing.T, h *Handler) string {
	t.Helper()
	createAdmin(t, h, "root", "correct-horse")
	rec := doLogin(h, "127.0.0.1:1", "root", "correct-horse")
	if rec.Code != http.StatusOK {
		t.Fatalf("login: %d %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Token string `json:"token"`
	}
	json.Unmarshal(rec.Body.Bytes(), &out)
	return out.Token
}

func putObject(t *testing.T, h *Handler, b *db.Bucket, key, body string) {
	t.Helper()
	if _, err := h.Objects.Put(service.PutInput{Bucket: b, Key: key, Body: strings.NewReader(body), ContentType: "text/plain", DeclaredSize: int64(len(body)), SkipImage: true}); err != nil {
		t.Fatalf("put %s: %v", key, err)
	}
}

func TestDeletePrefixPurgesEverythingUnderPrefix(t *testing.T) {
	h := newTestHandler(t, nil)
	tok := adminToken(t, h)
	if _, err := h.DB.CreateBucket("demo", ""); err != nil {
		t.Fatal(err)
	}
	if err := h.DB.SetBucketVersioning("demo", "Enabled"); err != nil {
		t.Fatal(err)
	}
	b, _ := h.DB.GetBucket("demo")
	h.Storage.CreateBucketDir("demo")
	putObject(t, h, b, "photos/a.txt", "1")
	putObject(t, h, b, "photos/a.txt", "2") // second version
	putObject(t, h, b, "photos/sub/b.txt", "3")
	putObject(t, h, b, "photos/gone.txt", "4")
	if _, err := h.Objects.Delete(b, "photos/gone.txt", ""); err != nil { // leaves marker + version
		t.Fatal(err)
	}
	putObject(t, h, b, "photosX.txt", "5") // shares the string prefix "photos" but not the folder
	putObject(t, h, b, "other/c.txt", "6")

	call := func(body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/admin/buckets/demo/objects/delete-prefix", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+tok)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	for _, bad := range []string{`{"prefix":"/photos/"}`, `{"prefix":"photos/../other/"}`, `{"prefix":""}`, `{"prefix":`} {
		if rec := call(bad); rec.Code != http.StatusBadRequest {
			t.Fatalf("%s: got %d, want 400", bad, rec.Code)
		}
	}
	rec := call(`{"prefix":"photos/"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("delete-prefix: %d %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Deleted int `json:"deleted"`
	}
	json.Unmarshal(rec.Body.Bytes(), &out)
	if out.Deleted != 3 {
		t.Fatalf("deleted = %d, want 3 (a.txt, sub/b.txt, gone.txt)", out.Deleted)
	}
	rows, _, err := h.DB.ListObjectVersions(b.ID, "photos/", "", "", 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Fatalf("expected no rows under photos/, got %d", len(rows))
	}
	for _, keep := range []string{"photosX.txt", "other/c.txt"} {
		if m, _ := h.DB.GetObjectMeta(b.ID, keep); m == nil {
			t.Fatalf("%s should have survived", keep)
		}
	}
	if _, err := os.Stat(filepath.Join(h.Storage.RootDir, "demo", "photos")); !os.IsNotExist(err) {
		t.Fatalf("photos/ directory should be gone, stat err=%v", err)
	}
}

func TestBucketDeleteRequiresForceWhenOnlyVersionsRemain(t *testing.T) {
	h := newTestHandler(t, nil)
	tok := adminToken(t, h)
	if _, err := h.DB.CreateBucket("demo", ""); err != nil {
		t.Fatal(err)
	}
	h.DB.SetBucketVersioning("demo", "Enabled")
	b, _ := h.DB.GetBucket("demo")
	h.Storage.CreateBucketDir("demo")
	putObject(t, h, b, "k", "v")
	h.Objects.Delete(b, "k", "") // now only a marker + noncurrent version remain

	del := func(q string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodDelete, "/admin/buckets/demo"+q, nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	if rec := del(""); rec.Code != http.StatusConflict {
		t.Fatalf("delete without force: got %d, want 409", rec.Code)
	}
	if rec := del("?force=true"); rec.Code != http.StatusOK {
		t.Fatalf("delete with force: got %d %s", rec.Code, rec.Body.String())
	}
	if got, _ := h.DB.GetBucket("demo"); got != nil {
		t.Fatal("bucket row still present")
	}
	if _, err := os.Stat(filepath.Join(h.Storage.RootDir, "demo")); !os.IsNotExist(err) {
		t.Fatal("bucket dir still present")
	}
}

func TestStorageMoveMovesSiblingsAndUpdatesRegistry(t *testing.T) {
	h := newTestHandler(t, nil)
	tok := adminToken(t, h)
	if _, err := h.DB.CreateBucket("demo", ""); err != nil {
		t.Fatal(err)
	}
	b, _ := h.DB.GetBucket("demo")
	h.Storage.CreateBucketDir("demo")
	putObject(t, h, b, "dir/file.txt", "hello")
	// Fake a multipart staging tree and a variant cache next to the bucket.
	mp := filepath.Join(h.Storage.RootDir, ".demo-multipart", "u1")
	os.MkdirAll(mp, 0700)
	os.WriteFile(filepath.Join(mp, "part"), []byte("p"), 0600)
	if err := h.Storage.PutVariant("demo", storage.VariantCacheKey("dir/file.txt", "", "\"etag\"", "fit_q75"), []byte("img")); err != nil {
		t.Fatal(err)
	}

	// New base on the same filesystem (sibling of the temp root).
	newBase := filepath.Join(filepath.Dir(h.Storage.RootDir), "moved-base")
	body, _ := json.Marshal(map[string]string{"storage_dir": newBase})
	req := httptest.NewRequest(http.MethodPut, "/admin/buckets/demo/storage", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("move: %d %s", rec.Code, rec.Body.String())
	}

	for _, p := range []string{
		filepath.Join(newBase, "demo"),
		filepath.Join(newBase, ".demo-multipart", "u1", "part"),
		filepath.Join(newBase, ".demo-cache"),
	} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("expected %s after move: %v", p, err)
		}
	}
	for _, p := range []string{
		filepath.Join(h.Storage.RootDir, "demo"),
		filepath.Join(h.Storage.RootDir, ".demo-multipart"),
		filepath.Join(h.Storage.RootDir, ".demo-cache"),
	} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatalf("old path %s should be gone (err=%v)", p, err)
		}
	}
	if got := h.Storage.BucketDir("demo"); got != newBase {
		t.Fatalf("registry dir = %q, want %q", got, newBase)
	}
	if b2, _ := h.DB.GetBucket("demo"); b2.StorageDir != newBase {
		t.Fatalf("db dir = %q, want %q", b2.StorageDir, newBase)
	}
	// Object is still readable through the normal path.
	rc, err := h.Storage.GetObject("demo", "dir/file.txt")
	if err != nil {
		t.Fatalf("get after move: %v", err)
	}
	data, _ := io.ReadAll(rc)
	rc.Close()
	if string(data) != "hello" {
		t.Fatalf("content after move = %q", data)
	}

	// Concurrent reprocess while another op holds the bucket → 409.
	if ok, _ := h.tryLockBucket("demo", "test"); !ok {
		t.Fatal("lock should be free")
	}
	req = httptest.NewRequest(http.MethodPost, "/admin/buckets/demo/reprocess", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("reprocess while busy: got %d, want 409", rec.Code)
	}
	h.unlockBucket("demo")
}
