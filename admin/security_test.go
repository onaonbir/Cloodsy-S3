package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/onaonbir/Cloodsy-S3/service"
)

func writeFile(t *testing.T, p, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
}

func orphanPaths(t *testing.T, out map[string]interface{}) []string {
	t.Helper()
	var paths []string
	for _, o := range out["orphan_dirs"].([]interface{}) {
		paths = append(paths, o.(map[string]interface{})["path"].(string))
	}
	sort.Strings(paths)
	return paths
}

// A bucket whose storage_dir is a shared directory must not expose the other
// applications living next to it to the orphan scan/delete.
func TestOrphanScanOnlyReportsProvableBucketTrees(t *testing.T) {
	e := newContentEnv(t, "")
	shared := t.TempDir()
	if _, err := e.h.DB.CreateBucket("shared", shared); err != nil {
		t.Fatal(err)
	}
	e.h.Storage.SetBucketDir("shared", shared)
	e.h.Storage.CreateBucketDir("shared")
	sharedBucket, _ := e.h.DB.GetBucket("shared")
	if _, err := e.h.Objects.Put(service.PutInput{Bucket: sharedBucket, Key: "photos/a.jpg", Body: strings.NewReader("jpg"), DeclaredSize: 3}); err != nil {
		t.Fatal(err)
	}

	// Foreign data next to the bucket: must never be reported or deleted.
	writeFile(t, filepath.Join(shared, "postgresql", "16", "main", "PG_VERSION"), "16\n")
	writeFile(t, filepath.Join(shared, "www", "index.html"), "<html></html>")
	writeFile(t, filepath.Join(shared, "mixed", "a.cloodsys3ext"), "x")
	writeFile(t, filepath.Join(shared, "mixed", "notes.txt"), "x")
	writeFile(t, filepath.Join(shared, ".gone-cache", "not-a-hash", "x.cloodsys3ext"), "x")
	writeFile(t, filepath.Join(shared, "withlink", "ok.cloodsys3ext"), "x")
	symlinkOK := os.Symlink(filepath.Join(shared, "www", "index.html"), filepath.Join(shared, "withlink", "link.cloodsys3ext")) == nil
	// Config and TLS directories: content looks like bucket data, but they
	// are protected by path.
	writeFile(t, filepath.Join(shared, "cfgdir", "x.cloodsys3ext"), "x")
	e.h.Config.Path = filepath.Join(shared, "cfgdir", "config.yaml")
	writeFile(t, filepath.Join(shared, "tls-certs", "x.cloodsys3ext"), "x")
	e.h.Config.Admin.TLS.CertFile = filepath.Join(shared, "tls-certs", "cert.pem")
	// A bucket whose storage_dir is inside another bucket's tree: the key
	// directories of "shared" must not become candidates.
	sharedTree, _ := e.h.Storage.BucketPath("shared")
	if _, err := e.h.DB.CreateBucket("nested", sharedTree); err != nil {
		t.Fatal(err)
	}
	e.h.Storage.SetBucketDir("nested", sharedTree)
	e.h.Storage.CreateBucketDir("nested")

	// Real leftovers: only server-written content (and empty dirs).
	writeFile(t, filepath.Join(shared, "leftover", "a", "b.cloodsys3ext"), "12345")
	os.MkdirAll(filepath.Join(shared, "leftover", "empty", "deeper"), 0700)
	upload := uuid.New().String()
	writeFile(t, filepath.Join(shared, ".leftover-multipart", upload, "1.cloodsys3ext"), "pp")
	hash := strings.Repeat("ab", 32)
	writeFile(t, filepath.Join(shared, ".leftover-cache", hash, hash+".cloodsys3ext"), "c")

	code, out := e.doJSON(t, http.MethodPost, "/admin/maintenance/scan", nil)
	if code != http.StatusOK {
		t.Fatalf("scan: %d %v", code, out)
	}
	want := []string{
		filepath.Join(shared, ".leftover-cache"),
		filepath.Join(shared, ".leftover-multipart"),
		filepath.Join(shared, "leftover"),
	}
	sort.Strings(want)
	if got := orphanPaths(t, out); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("orphans = %v\nwant %v", got, want)
	}

	foreign := []string{"postgresql", "www", "mixed", ".gone-cache", "cfgdir", "tls-certs", "shared"}
	if symlinkOK {
		foreign = append(foreign, "withlink")
	}
	for _, name := range foreign {
		p := filepath.Join(shared, name)
		if code, out := e.doJSON(t, http.MethodPost, "/admin/maintenance/orphan-dirs/delete", map[string]interface{}{"paths": []string{p}}); code != http.StatusBadRequest {
			t.Fatalf("delete %s: %d %v, want 400", name, code, out)
		}
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("%s was touched: %v", name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(sharedTree, "photos")); err != nil {
		t.Fatal("bucket key directory touched")
	}

	// Content changed after the scan: the delete re-verifies and refuses.
	writeFile(t, filepath.Join(shared, "leftover", "README"), "not ours")
	if code, _ := e.doJSON(t, http.MethodPost, "/admin/maintenance/orphan-dirs/delete", map[string]interface{}{"paths": want}); code != http.StatusBadRequest {
		t.Fatalf("delete after content change: %d, want 400", code)
	}
	if _, err := os.Stat(filepath.Join(shared, "leftover")); err != nil {
		t.Fatal("leftover deleted despite foreign content")
	}
	os.Remove(filepath.Join(shared, "leftover", "README"))

	code, out = e.doJSON(t, http.MethodPost, "/admin/maintenance/orphan-dirs/delete", map[string]interface{}{"paths": want})
	if code != http.StatusOK || out["deleted"].(float64) != 3 {
		t.Fatalf("delete leftovers: %d %v", code, out)
	}
	for _, p := range want {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatalf("%s still exists", p)
		}
	}
	for _, name := range []string{"postgresql/16/main/PG_VERSION", "www/index.html"} {
		if _, err := os.Stat(filepath.Join(shared, filepath.FromSlash(name))); err != nil {
			t.Fatalf("foreign file %s gone: %v", name, err)
		}
	}
}

func TestIsWithinCaseFolding(t *testing.T) {
	sep := string(os.PathSeparator)
	root := sep + "Data" + sep + "Store"
	if !isWithin(root+sep+"x", root) || isWithin(sep+"Data"+sep+"Storex", root) {
		t.Fatal("basic containment")
	}
	prev := foldPathCase
	defer func() { foldPathCase = prev }()
	foldPathCase = true
	if !isWithin(sep+"data"+sep+"store"+sep+"x", root) || !pathEqual(sep+"DATA", sep+"data") {
		t.Fatal("case-insensitive containment")
	}
	foldPathCase = false
	if isWithin(sep+"data"+sep+"store"+sep+"x", root) {
		t.Fatal("case-sensitive containment")
	}
}

func TestMaintenanceScanIsSerialized(t *testing.T) {
	e := newContentEnv(t, "")
	e.h.scanBusy.Store(true) // a scan is running
	if code, out := e.doJSON(t, http.MethodPost, "/admin/maintenance/scan", nil); code != http.StatusConflict || out["error"] != "a scan is already running" {
		t.Fatalf("concurrent scan: %d %v", code, out)
	}
	if code, out := e.doJSON(t, http.MethodPost, "/admin/maintenance/orphan-dirs/delete", map[string]interface{}{"paths": []string{"/x"}}); code != http.StatusConflict {
		t.Fatalf("concurrent orphan delete: %d %v", code, out)
	}
	e.h.scanBusy.Store(false)
	if code, out := e.doJSON(t, http.MethodPost, "/admin/maintenance/scan", nil); code != http.StatusOK {
		t.Fatalf("scan after release: %d %v", code, out)
	}
	if e.h.scanBusy.Load() {
		t.Fatal("scan slot not released")
	}
}

func TestCopyPrefixBusyAndCancellation(t *testing.T) {
	e := newContentEnv(t, "")
	for _, k := range []string{"dir/a.txt", "dir/b.txt", "dir/c.txt"} {
		e.mustUpload(t, k, "data", "")
	}
	body := map[string]interface{}{"source_prefix": "dir/", "dest_prefix": "copy/"}

	// Another long-running operation owns the bucket → 409.
	if ok, _ := e.h.tryLockBucket("demo", "storage move"); !ok {
		t.Fatal("lock should be free")
	}
	if code, out := e.doJSON(t, http.MethodPost, "/admin/buckets/demo/objects/copy-prefix", body); code != http.StatusConflict {
		t.Fatalf("busy: %d %v", code, out)
	}
	e.h.unlockBucket("demo")

	// The client is gone: nothing is copied.
	raw, _ := json.Marshal(body)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := httptest.NewRequest(http.MethodPost, "/admin/buckets/demo/objects/copy-prefix", bytes.NewReader(raw)).WithContext(ctx)
	req.Header.Set("Authorization", "Bearer "+e.tok)
	req.Header.Set("Content-Type", "application/json")
	e.h.ServeHTTP(httptest.NewRecorder(), req)
	if objs, _, _, _, _ := e.h.DB.ListObjectsMeta(e.b.ID, "copy/", "", "", 10); len(objs) != 0 {
		t.Fatalf("copied after cancellation: %v", objs)
	}
	if ok, _ := e.h.tryLockBucket("demo", "x"); !ok {
		t.Fatal("copy-prefix did not release the bucket")
	}
	e.h.unlockBucket("demo")

	if code, out := e.doJSON(t, http.MethodPost, "/admin/buckets/demo/objects/copy-prefix", body); code != http.StatusOK || out["copied"].(float64) != 3 {
		t.Fatalf("copy-prefix: %d %v", code, out)
	}
}

func TestCaseOnlyRenameRefused(t *testing.T) {
	e := newContentEnv(t, "")
	e.mustUpload(t, "Report.TXT", "precious", "")
	e.mustUpload(t, "Dir/x.txt", "x", "")
	for _, move := range []bool{true, false} {
		code, out := e.doJSON(t, http.MethodPost, "/admin/buckets/demo/objects/copy", map[string]interface{}{"source_key": "Report.TXT", "dest_key": "report.txt", "move": move})
		if code != http.StatusConflict || out["error"] != errCaseOnlyRename {
			t.Fatalf("move=%v: %d %v", move, code, out)
		}
		code, out = e.doJSON(t, http.MethodPost, "/admin/buckets/demo/objects/copy-prefix", map[string]interface{}{"source_prefix": "Dir/", "dest_prefix": "dir/", "move": move})
		if code != http.StatusConflict || out["error"] != errCaseOnlyRename {
			t.Fatalf("prefix move=%v: %d %v", move, code, out)
		}
	}
	if got, err := os.ReadFile(mustPath(t, e, "Report.TXT")); err != nil || string(got) != "precious" {
		t.Fatalf("source damaged: %q %v", got, err)
	}
	if m, _ := e.h.DB.GetObjectMeta(e.b.ID, "report.txt"); m != nil {
		t.Fatal("case-only destination created")
	}

	// Other buckets are separate directories: allowed.
	e.addBucket(t, "other", 0)
	if code, out := e.doJSON(t, http.MethodPost, "/admin/buckets/demo/objects/copy", map[string]interface{}{"source_key": "Report.TXT", "dest_bucket": "other", "dest_key": "report.txt", "move": true}); code != http.StatusOK {
		t.Fatalf("cross-bucket: %d %v", code, out)
	}
}

// A move whose source was overwritten after the handler looked it up is
// refused (the handler answers 409 "source changed during move").
func TestCopyOneMoveRefusesChangedSource(t *testing.T) {
	e := newContentEnv(t, "Enabled")
	e.mustUpload(t, "a.txt", "v1", "")
	stale, _ := e.h.DB.GetObjectMeta(e.b.ID, "a.txt")
	e.mustUpload(t, "a.txt", "v2", "")
	_, _, err := e.h.copyOne(e.b, stale, e.b, "b.txt", true)
	if err == nil || err != service.ErrSourceChanged {
		t.Fatalf("err = %v", err)
	}
	if vs, _ := e.h.DB.ListVersionsForKey(e.b.ID, "a.txt"); len(vs) != 2 {
		t.Fatalf("source versions: %d", len(vs))
	}
}
