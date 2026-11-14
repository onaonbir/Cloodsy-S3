package admin

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/onaonbir/Cloodsy-S3/db"
	applogger "github.com/onaonbir/Cloodsy-S3/logger"
	"github.com/onaonbir/Cloodsy-S3/metrics"
	"github.com/onaonbir/Cloodsy-S3/service"
	"github.com/onaonbir/Cloodsy-S3/webhook"
)

// doJSON sends an optional JSON body and decodes a JSON object response.
func (e *contentEnv) doJSON(t *testing.T, method, path string, body interface{}) (int, map[string]interface{}) {
	t.Helper()
	var rd io.Reader
	hdr := map[string]string{}
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		rd = bytes.NewReader(raw)
		hdr["Content-Type"] = "application/json"
	}
	resp := e.do(t, method, path, rd, hdr)
	raw, _ := io.ReadAll(resp.Body)
	out := map[string]interface{}{}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatalf("%s %s: non-JSON response %d: %s", method, path, resp.StatusCode, raw)
		}
	}
	return resp.StatusCode, out
}

func (e *contentEnv) mustUpload(t *testing.T, key, body, ct string) uploadResp {
	t.Helper()
	code, out := e.upload(t, key, []byte(body), ct)
	if code != http.StatusOK {
		t.Fatalf("upload %q: %d %+v", key, code, out)
	}
	return out
}

func (e *contentEnv) addBucket(t *testing.T, name string, quota int64) *db.Bucket {
	t.Helper()
	if _, err := e.h.DB.CreateBucket(name, ""); err != nil {
		t.Fatal(err)
	}
	if quota > 0 {
		e.h.DB.SetBucketQuota(name, quota)
	}
	e.h.Storage.CreateBucketDir(name)
	b, _ := e.h.DB.GetBucket(name)
	return b
}

func q(key string) string { return url.QueryEscape(key) }

// --- 1. object info ------------------------------------------------------------

func TestObjectInfo(t *testing.T) {
	e := newContentEnv(t, "Enabled")
	e.h.Config.Image.Enabled = true
	v1 := e.mustUpload(t, "pics/a.png", "one", "image/png")
	v2 := e.mustUpload(t, "pics/a.png", "two!", "image/png")

	code, info := e.doJSON(t, http.MethodGet, "/admin/buckets/demo/object/info?key="+q("pics/a.png"), nil)
	if code != http.StatusOK {
		t.Fatalf("info: %d %v", code, info)
	}
	if info["key"] != "pics/a.png" || info["size"].(float64) != 4 || info["version_id"] != v2.VersionID || info["content_type"] != "image/png" {
		t.Fatalf("info fields: %v", info)
	}
	if !strings.HasSuffix(info["last_modified"].(string), "Z") {
		t.Errorf("last_modified: %v", info["last_modified"])
	}
	disk := info["disk_path"].(string)
	if got, err := os.ReadFile(disk); err != nil || string(got) != "two!" {
		t.Fatalf("disk_path %q: %q %v", disk, got, err)
	}
	if bd := info["bucket_dir"].(string); !strings.HasPrefix(disk, bd+string(os.PathSeparator)) {
		t.Fatalf("disk_path %q not under bucket_dir %q", disk, bd)
	}
	if _, ok := info["metadata"].(map[string]interface{}); !ok {
		t.Fatalf("metadata must be an object: %v", info["metadata"])
	}
	variant, ok := info["variant"].(map[string]interface{})
	if !ok || variant["exists"] != false || variant["disk_path"] == "" {
		t.Fatalf("variant: %v", info["variant"])
	}
	vs := info["versions"].([]interface{})
	if len(vs) != 2 || vs[0].(map[string]interface{})["version_id"] != v2.VersionID || vs[1].(map[string]interface{})["version_id"] != v1.VersionID {
		t.Fatalf("versions: %v", vs)
	}
	if p := vs[1].(map[string]interface{})["disk_path"].(string); p == "" || p == disk {
		t.Fatalf("v1 disk path: %q", p)
	}

	// Non-image → variant null.
	e.mustUpload(t, "doc.txt", "x", "text/plain")
	_, info = e.doJSON(t, http.MethodGet, "/admin/buckets/demo/object/info?key=doc.txt", nil)
	if info["variant"] != nil {
		t.Fatalf("variant for text: %v", info["variant"])
	}

	// Latest is a delete marker: 200, size 0, no disk path, versions listed.
	if _, err := e.h.Objects.Delete(e.b, "pics/a.png", ""); err != nil {
		t.Fatal(err)
	}
	code, info = e.doJSON(t, http.MethodGet, "/admin/buckets/demo/object/info?key="+q("pics/a.png"), nil)
	if code != http.StatusOK || info["size"].(float64) != 0 || info["disk_path"] != "" || len(info["versions"].([]interface{})) != 3 {
		t.Fatalf("delete-marker info: %d %v", code, info)
	}
	if m := info["versions"].([]interface{})[0].(map[string]interface{}); m["is_delete_marker"] != true || m["disk_path"] != "" {
		t.Fatalf("marker entry: %v", m)
	}

	if code, out := e.doJSON(t, http.MethodGet, "/admin/buckets/demo/object/info?key=nope", nil); code != http.StatusNotFound || out["error"] != "object not found" {
		t.Fatalf("missing: %d %v", code, out)
	}
	if code, _ := e.doJSON(t, http.MethodGet, "/admin/buckets/demo/object/info?key="+q("a/../b"), nil); code != http.StatusBadRequest {
		t.Fatalf("invalid key: %d", code)
	}
}

// --- 2. range / HEAD / inline ---------------------------------------------------

func TestDownloadRangeHeadInline(t *testing.T) {
	e := newContentEnv(t, "")
	e.mustUpload(t, "r.txt", "0123456789", "text/plain")

	cases := []struct {
		rng, body, cr string
	}{
		{"bytes=0-4", "01234", "bytes 0-4/10"},
		{"bytes=7-", "789", "bytes 7-9/10"},
		{"bytes=-3", "789", "bytes 7-9/10"},
		{"bytes=8-100", "89", "bytes 8-9/10"},
	}
	for _, c := range cases {
		resp := e.do(t, http.MethodGet, objPath("r.txt"), nil, map[string]string{"Range": c.rng})
		got, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusPartialContent || string(got) != c.body || resp.Header.Get("Content-Range") != c.cr {
			t.Fatalf("%s: %d %q %q", c.rng, resp.StatusCode, got, resp.Header.Get("Content-Range"))
		}
		if resp.Header.Get("Content-Length") != fmt.Sprint(len(c.body)) || resp.Header.Get("Accept-Ranges") != "bytes" {
			t.Fatalf("%s headers: %v", c.rng, resp.Header)
		}
	}

	resp := e.do(t, http.MethodGet, objPath("r.txt"), nil, map[string]string{"Range": "bytes=10-"})
	if resp.StatusCode != http.StatusRequestedRangeNotSatisfiable || resp.Header.Get("Content-Range") != "bytes */10" || jsonError(t, resp) == "" {
		t.Fatalf("unsatisfiable: %d %v", resp.StatusCode, resp.Header)
	}
	// Multi-range is ignored: whole object.
	resp = e.do(t, http.MethodGet, objPath("r.txt"), nil, map[string]string{"Range": "bytes=0-1,3-4"})
	if got, _ := io.ReadAll(resp.Body); resp.StatusCode != http.StatusOK || string(got) != "0123456789" {
		t.Fatalf("multi-range: %d %q", resp.StatusCode, got)
	}

	resp = e.do(t, http.MethodHead, objPath("r.txt"), nil, nil)
	got, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || len(got) != 0 || resp.Header.Get("Content-Length") != "10" || resp.Header.Get("ETag") == "" ||
		resp.Header.Get("Accept-Ranges") != "bytes" || resp.Header.Get("Content-Security-Policy") != "sandbox" {
		t.Fatalf("HEAD: %d %q %v", resp.StatusCode, got, resp.Header)
	}
	resp = e.do(t, http.MethodHead, objPath("r.txt"), nil, map[string]string{"Range": "bytes=2-3"})
	if resp.StatusCode != http.StatusPartialContent || resp.Header.Get("Content-Length") != "2" {
		t.Fatalf("HEAD range: %d %v", resp.StatusCode, resp.Header)
	}
	if resp := e.do(t, http.MethodHead, objPath("missing"), nil, nil); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("HEAD missing: %d", resp.StatusCode)
	}

	resp = e.do(t, http.MethodGet, objPath("r.txt", "inline", "1"), nil, nil)
	if v := resp.Header.Get("Content-Disposition"); v != "inline; filename*=UTF-8''r.txt" {
		t.Fatalf("inline disposition: %q", v)
	}
	resp = e.do(t, http.MethodGet, objPath("r.txt"), nil, nil)
	if v := resp.Header.Get("Content-Disposition"); v != "attachment; filename*=UTF-8''r.txt" || resp.Header.Get("Accept-Ranges") != "bytes" {
		t.Fatalf("attachment disposition: %q", v)
	}
}

// --- 3. copy / move ---------------------------------------------------------------

func TestCopyAndMoveObject(t *testing.T) {
	e := newContentEnv(t, "")
	e.mustUpload(t, "src.txt", "payload", "text/plain")
	code, _ := e.doJSON(t, http.MethodPut, "/admin/buckets/demo/object/meta?key=src.txt", map[string]interface{}{
		"content_type": "text/markdown", "metadata": map[string]string{"Owner": "ops"},
	})
	if code != http.StatusOK {
		t.Fatalf("set meta: %d", code)
	}

	code, out := e.doJSON(t, http.MethodPost, "/admin/buckets/demo/objects/copy", map[string]interface{}{"source_key": "src.txt", "dest_key": "copy/dst.txt"})
	if code != http.StatusOK || out["key"] != "copy/dst.txt" || out["size"].(float64) != 7 || out["etag"] == "" {
		t.Fatalf("copy: %d %v", code, out)
	}
	m, _ := e.h.DB.GetObjectMeta(e.b.ID, "copy/dst.txt")
	if m == nil || m.ContentType != "text/markdown" || !strings.Contains(m.Metadata, `"owner":"ops"`) {
		t.Fatalf("copied meta: %+v", m)
	}
	if src, _ := e.h.DB.GetObjectMeta(e.b.ID, "src.txt"); src == nil {
		t.Fatal("copy removed the source")
	}

	// Cross-bucket move.
	other := e.addBucket(t, "other", 0)
	code, out = e.doJSON(t, http.MethodPost, "/admin/buckets/demo/objects/copy", map[string]interface{}{"source_key": "src.txt", "dest_bucket": "other", "dest_key": "moved.txt", "move": true})
	if code != http.StatusOK || out["key"] != "moved.txt" {
		t.Fatalf("move: %d %v", code, out)
	}
	if src, _ := e.h.DB.GetObjectMeta(e.b.ID, "src.txt"); src != nil {
		t.Fatal("move kept the source")
	}
	if !e.h.Storage.ObjectExists("other", "moved.txt") || e.h.Storage.ObjectExists("demo", "src.txt") {
		t.Fatal("move: files not where expected")
	}
	if m, _ := e.h.DB.GetObjectMeta(other.ID, "moved.txt"); m == nil || m.ContentType != "text/markdown" {
		t.Fatalf("moved meta: %+v", m)
	}

	for _, c := range []struct {
		body map[string]interface{}
		code int
	}{
		{map[string]interface{}{"source_key": "copy/dst.txt", "dest_key": "copy/dst.txt"}, http.StatusConflict},
		{map[string]interface{}{"source_key": "nope", "dest_key": "x"}, http.StatusNotFound},
		{map[string]interface{}{"source_key": "copy/dst.txt", "dest_bucket": "missing", "dest_key": "x"}, http.StatusNotFound},
		{map[string]interface{}{"source_key": "copy/dst.txt", "dest_key": "a/../b"}, http.StatusBadRequest},
		{map[string]interface{}{"source_key": "", "dest_key": "x"}, http.StatusBadRequest},
	} {
		if code, out := e.doJSON(t, http.MethodPost, "/admin/buckets/demo/objects/copy", c.body); code != c.code || out["error"] == nil {
			t.Fatalf("%v: %d %v (want %d)", c.body, code, out, c.code)
		}
	}

	// Quota on the destination bucket.
	e.addBucket(t, "tiny", 3)
	if code, out := e.doJSON(t, http.MethodPost, "/admin/buckets/demo/objects/copy", map[string]interface{}{"source_key": "copy/dst.txt", "dest_bucket": "tiny", "dest_key": "x"}); code != http.StatusRequestEntityTooLarge || out["error"] != "quota exceeded" {
		t.Fatalf("quota: %d %v", code, out)
	}
}

func TestCopyPrefix(t *testing.T) {
	e := newContentEnv(t, "")
	for _, k := range []string{"dir/", "dir/a.txt", "dir/sub/b.txt", "dirx/c.txt"} {
		e.mustUpload(t, k, map[bool]string{true: "", false: "data-" + k}[strings.HasSuffix(k, "/")], "")
	}
	code, out := e.doJSON(t, http.MethodPost, "/admin/buckets/demo/objects/copy-prefix", map[string]interface{}{"source_prefix": "dir/", "dest_prefix": "copy/"})
	if code != http.StatusOK || out["copied"].(float64) != 3 || out["failed"].(float64) != 0 {
		t.Fatalf("copy-prefix: %d %v", code, out)
	}
	for _, k := range []string{"copy/", "copy/a.txt", "copy/sub/b.txt"} {
		if m, _ := e.h.DB.GetObjectMeta(e.b.ID, k); m == nil {
			t.Fatalf("missing %q", k)
		}
	}
	if m, _ := e.h.DB.GetObjectMeta(e.b.ID, "copy/c.txt"); m != nil {
		t.Fatal("sibling prefix dirx/ was copied")
	}

	// Rename (move) a folder.
	code, out = e.doJSON(t, http.MethodPost, "/admin/buckets/demo/objects/copy-prefix", map[string]interface{}{"source_prefix": "copy/", "dest_prefix": "renamed/", "move": true})
	if code != http.StatusOK || out["copied"].(float64) != 3 {
		t.Fatalf("move-prefix: %d %v", code, out)
	}
	if _, _, _, _, err := e.h.DB.ListObjectsMeta(e.b.ID, "copy/", "", "", 10); err != nil {
		t.Fatal(err)
	}
	if objs, _, _, _, _ := e.h.DB.ListObjectsMeta(e.b.ID, "copy/", "", "", 10); len(objs) != 0 {
		t.Fatalf("source left behind: %v", objs)
	}
	if got, _ := os.ReadFile(mustPath(t, e, "renamed/sub/b.txt")); string(got) != "data-dir/sub/b.txt" {
		t.Fatalf("renamed content: %q", got)
	}

	for _, body := range []map[string]interface{}{
		{"source_prefix": "dir/", "dest_prefix": "dir/inner/"},
		{"source_prefix": "dir/", "dest_prefix": "dir/"},
		{"source_prefix": "dir", "dest_prefix": "x/"},
		{"source_prefix": "dir/", "dest_prefix": "x"},
		{"source_prefix": "../", "dest_prefix": "x/"},
	} {
		if code, _ := e.doJSON(t, http.MethodPost, "/admin/buckets/demo/objects/copy-prefix", body); code != http.StatusBadRequest {
			t.Fatalf("%v: %d, want 400", body, code)
		}
	}
	// Into another bucket, same prefix is fine.
	e.addBucket(t, "other", 0)
	if code, out := e.doJSON(t, http.MethodPost, "/admin/buckets/demo/objects/copy-prefix", map[string]interface{}{"source_prefix": "dir/", "dest_bucket": "other", "dest_prefix": "dir/"}); code != http.StatusOK || out["copied"].(float64) != 3 {
		t.Fatalf("cross-bucket: %d %v", code, out)
	}
}

func mustPath(t *testing.T, e *contentEnv, key string) string {
	t.Helper()
	p, err := e.h.Storage.ObjectPath("demo", key, "")
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// --- 4/5. search, prefix stats ------------------------------------------------------

func TestSearchAndPrefixStats(t *testing.T) {
	e := newContentEnv(t, "")
	for _, k := range []string{"docs/Report-2026.pdf", "docs/notes.txt", "img/report.png", "a_b.txt", "axb.txt", "100%.txt"} {
		e.mustUpload(t, k, "12345", "")
	}
	search := func(params string) map[string]interface{} {
		t.Helper()
		code, out := e.doJSON(t, http.MethodGet, "/admin/buckets/demo/search?"+params, nil)
		if code != http.StatusOK {
			t.Fatalf("search %s: %d %v", params, code, out)
		}
		if p, ok := out["prefixes"].([]interface{}); !ok || len(p) != 0 {
			t.Fatalf("prefixes must be []: %v", out["prefixes"])
		}
		return out
	}
	keys := func(out map[string]interface{}) []string {
		var ks []string
		for _, o := range out["objects"].([]interface{}) {
			ks = append(ks, o.(map[string]interface{})["key"].(string))
		}
		return ks
	}

	if got := keys(search("q=REPORT")); strings.Join(got, ",") != "docs/Report-2026.pdf,img/report.png" {
		t.Fatalf("case-insensitive: %v", got)
	}
	if got := keys(search("q=_")); strings.Join(got, ",") != "a_b.txt" {
		t.Fatalf("underscore must be literal: %v", got)
	}
	if got := keys(search("q=" + q("%"))); strings.Join(got, ",") != "100%.txt" {
		t.Fatalf("percent must be literal: %v", got)
	}
	if got := keys(search("q=report&prefix=docs/")); strings.Join(got, ",") != "docs/Report-2026.pdf" {
		t.Fatalf("prefix filter: %v", got)
	}
	if got := keys(search("")); len(got) != 6 {
		t.Fatalf("empty q lists all recursively: %v", got)
	}
	// Pagination stays exact with the filter in SQL.
	out := search("q=t&max-keys=2")
	first := keys(out)
	if len(first) != 2 || out["truncated"] != true || out["next_marker"] != first[1] {
		t.Fatalf("page 1: %v", out)
	}
	out = search("q=t&max-keys=2&marker=" + q(out["next_marker"].(string)))
	second := keys(out)
	all := keys(search("q=t"))
	if strings.Join(append(first, second...), ",") != strings.Join(all[:len(first)+len(second)], ",") {
		t.Fatalf("pages %v + %v vs %v", first, second, all)
	}

	code, st := e.doJSON(t, http.MethodGet, "/admin/buckets/demo/prefix-stats?prefix=docs/", nil)
	if code != http.StatusOK || st["prefix"] != "docs/" || st["objects"].(float64) != 2 || st["bytes"].(float64) != 10 {
		t.Fatalf("prefix stats: %d %v", code, st)
	}
	_, st = e.doJSON(t, http.MethodGet, "/admin/buckets/demo/prefix-stats", nil)
	if st["objects"].(float64) != 6 || st["bytes"].(float64) != 30 {
		t.Fatalf("bucket stats: %v", st)
	}
	if code, _ := e.doJSON(t, http.MethodGet, "/admin/buckets/missing/search?q=a", nil); code != http.StatusNotFound {
		t.Fatalf("missing bucket: %d", code)
	}
}

// --- 6. metadata edit --------------------------------------------------------------

func TestUpdateObjectMetadata(t *testing.T) {
	e := newContentEnv(t, "Enabled")
	up := e.mustUpload(t, "m.bin", "abc", "application/octet-stream")

	code, info := e.doJSON(t, http.MethodPut, "/admin/buckets/demo/object/meta?key=m.bin", map[string]interface{}{
		"content_type": "text/plain", "metadata": map[string]string{"Project": "x", "x-amz-meta-team": "y"},
	})
	if code != http.StatusOK || info["content_type"] != "text/plain" {
		t.Fatalf("meta: %d %v", code, info)
	}
	md := info["metadata"].(map[string]interface{})
	if md["project"] != "x" || md["team"] != "y" || len(md) != 2 {
		t.Fatalf("metadata: %v", md)
	}
	// Versioned bucket: a new version, bytes unchanged.
	if vs := info["versions"].([]interface{}); len(vs) != 2 || info["version_id"] == up.VersionID {
		t.Fatalf("expected a new version: %v", info)
	}
	if got, _ := os.ReadFile(info["disk_path"].(string)); string(got) != "abc" {
		t.Fatalf("content changed: %q", got)
	}

	for _, body := range []map[string]interface{}{
		{"metadata": map[string]string{"bad key": "v"}},
		{"metadata": map[string]string{"": "v"}},
		{"metadata": map[string]string{"k": "line\nbreak"}},
		{"metadata": map[string]string{"k": strings.Repeat("v", 300)}},
		{"content_type": "text/plain\r\nX: y"},
	} {
		if code, _ := e.doJSON(t, http.MethodPut, "/admin/buckets/demo/object/meta?key=m.bin", body); code != http.StatusBadRequest {
			t.Fatalf("%v: %d, want 400", body, code)
		}
	}
	if code, _ := e.doJSON(t, http.MethodPut, "/admin/buckets/demo/object/meta?key=none", map[string]interface{}{}); code != http.StatusNotFound {
		t.Fatalf("missing object: %d", code)
	}
}

// --- 7. multipart --------------------------------------------------------------

func TestAdminMultipartUpload(t *testing.T) {
	e := newContentEnv(t, "")
	code, out := e.doJSON(t, http.MethodPost, "/admin/buckets/demo/uploads?key="+q("big/file.bin"), nil)
	if code != http.StatusOK || out["key"] != "big/file.bin" {
		t.Fatalf("create: %d %v", code, out)
	}
	id := out["upload_id"].(string)
	base := "/admin/buckets/demo/uploads/" + id

	part1 := bytes.Repeat([]byte("a"), int(service.MinPartSize)) // > JSON body cap
	part2 := []byte("tail")
	putPart := func(n int, body []byte) map[string]interface{} {
		t.Helper()
		resp := e.do(t, http.MethodPut, fmt.Sprintf("%s/parts/%d", base, n), bytes.NewReader(body), nil)
		var o map[string]interface{}
		json.NewDecoder(resp.Body).Decode(&o)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("part %d: %d %v", n, resp.StatusCode, o)
		}
		return o
	}
	p1 := putPart(1, part1)
	p2 := putPart(2, part2)
	if p1["part_number"].(float64) != 1 || p1["size"].(float64) != float64(len(part1)) || p2["etag"] == "" {
		t.Fatalf("part responses: %v %v", p1, p2)
	}
	if resp := e.do(t, http.MethodPut, base+"/parts/0", strings.NewReader("x"), nil); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("part 0: %d", resp.StatusCode)
	}
	if resp := e.do(t, http.MethodPut, base+"/parts/10001", strings.NewReader("x"), nil); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("part 10001: %d", resp.StatusCode)
	}

	_, lp := e.doJSON(t, http.MethodGet, base+"/parts", nil)
	if parts := lp["parts"].([]interface{}); len(parts) != 2 || parts[0].(map[string]interface{})["last_modified"] == "" {
		t.Fatalf("list parts: %v", lp)
	}
	_, lu := e.doJSON(t, http.MethodGet, "/admin/buckets/demo/uploads", nil)
	ups := lu["uploads"].([]interface{})
	if len(ups) != 1 || ups[0].(map[string]interface{})["parts"].(float64) != 2 || ups[0].(map[string]interface{})["bytes"].(float64) != float64(len(part1)+len(part2)) {
		t.Fatalf("list uploads: %v", lu)
	}

	// Validation errors.
	if code, _ := e.doJSON(t, http.MethodPost, base+"/complete", map[string]interface{}{"parts": []map[string]interface{}{{"part_number": 2, "etag": p2["etag"]}, {"part_number": 1, "etag": p1["etag"]}}}); code != http.StatusBadRequest {
		t.Fatalf("bad order: %d", code)
	}
	if code, _ := e.doJSON(t, http.MethodPost, base+"/complete", map[string]interface{}{"parts": []map[string]interface{}{{"part_number": 1, "etag": "\"bogus\""}}}); code != http.StatusBadRequest {
		t.Fatalf("bad etag: %d", code)
	}

	code, done := e.doJSON(t, http.MethodPost, base+"/complete", map[string]interface{}{"parts": []map[string]interface{}{{"part_number": 1, "etag": p1["etag"]}, {"part_number": 2, "etag": p2["etag"]}}})
	if code != http.StatusOK || done["key"] != "big/file.bin" || done["size"].(float64) != float64(len(part1)+len(part2)) || !strings.HasSuffix(done["etag"].(string), "-2\"") {
		t.Fatalf("complete: %d %v", code, done)
	}
	resp := e.do(t, http.MethodGet, objPath("big/file.bin"), nil, map[string]string{"Range": "bytes=-4"})
	if got, _ := io.ReadAll(resp.Body); string(got) != "tail" {
		t.Fatalf("assembled tail: %q", got)
	}
	if code, _ := e.doJSON(t, http.MethodGet, base+"/parts", nil); code != http.StatusNotFound {
		t.Fatalf("upload should be gone: %d", code)
	}

	// Too-small non-last part.
	_, out = e.doJSON(t, http.MethodPost, "/admin/buckets/demo/uploads?key=small", nil)
	sid := out["upload_id"].(string)
	sbase := "/admin/buckets/demo/uploads/" + sid
	a := e.do(t, http.MethodPut, sbase+"/parts/1", strings.NewReader("aa"), nil)
	var pa map[string]interface{}
	json.NewDecoder(a.Body).Decode(&pa)
	b := e.do(t, http.MethodPut, sbase+"/parts/2", strings.NewReader("bb"), nil)
	var pb map[string]interface{}
	json.NewDecoder(b.Body).Decode(&pb)
	if code, out := e.doJSON(t, http.MethodPost, sbase+"/complete", map[string]interface{}{"parts": []map[string]interface{}{{"part_number": 1, "etag": pa["etag"]}, {"part_number": 2, "etag": pb["etag"]}}}); code != http.StatusBadRequest || !strings.Contains(out["error"].(string), "at least") {
		t.Fatalf("too small: %d %v", code, out)
	}
	// Single small part is fine (it is the last part) — but quota blocks it.
	e.h.DB.SetBucketQuota("demo", 1)
	if code, out := e.doJSON(t, http.MethodPost, sbase+"/complete", map[string]interface{}{"parts": []map[string]interface{}{{"part_number": 1, "etag": pa["etag"]}}}); code != http.StatusRequestEntityTooLarge || out["error"] != "quota exceeded" {
		t.Fatalf("quota: %d %v", code, out)
	}
	e.h.DB.SetBucketQuota("demo", 0)

	// Abort one, then abort-all.
	if code, _ := e.doJSON(t, http.MethodDelete, sbase, nil); code != http.StatusOK {
		t.Fatalf("abort: %d", code)
	}
	if code, _ := e.doJSON(t, http.MethodDelete, sbase, nil); code != http.StatusNotFound {
		t.Fatalf("abort twice: %d", code)
	}
	for i := 0; i < 2; i++ {
		e.doJSON(t, http.MethodPost, "/admin/buckets/demo/uploads?key=k", nil)
	}
	if _, out := e.doJSON(t, http.MethodDelete, "/admin/buckets/demo/uploads?older_than_days=1", nil); out["aborted"].(float64) != 0 {
		t.Fatalf("older_than_days=1 aborted fresh uploads: %v", out)
	}
	if _, out := e.doJSON(t, http.MethodDelete, "/admin/buckets/demo/uploads", nil); out["aborted"].(float64) != 2 {
		t.Fatalf("abort all: %v", out)
	}
	if _, lu := e.doJSON(t, http.MethodGet, "/admin/buckets/demo/uploads", nil); len(lu["uploads"].([]interface{})) != 0 {
		t.Fatalf("uploads left: %v", lu)
	}
	if code, _ := e.doJSON(t, http.MethodGet, "/admin/buckets/demo/uploads/not-a-uuid/parts", nil); code != http.StatusNotFound {
		t.Fatalf("bogus upload id: %d", code)
	}
}

// --- 8. lifecycle v2 ------------------------------------------------------------

func TestLifecycleV2(t *testing.T) {
	e := newContentEnv(t, "")
	code, out := e.doJSON(t, http.MethodPost, "/admin/buckets/demo/lifecycle", map[string]interface{}{
		"name": "logs", "prefix": "logs/", "noncurrent_days": 7, "abort_multipart_days": 2, "status": "Disabled",
	})
	if code != http.StatusOK || out["id"] == nil {
		t.Fatalf("create v2: %d %v", code, out)
	}
	id := int64(out["id"].(float64))
	// Old v1 body still works.
	if code, _ := e.doJSON(t, http.MethodPost, "/admin/buckets/demo/lifecycle", map[string]interface{}{"name": "tmp", "prefix": "tmp/", "expiration_days": 3}); code != http.StatusOK {
		t.Fatalf("create v1: %d", code)
	}
	_, list := e.doJSON(t, http.MethodGet, "/admin/buckets/demo/lifecycle", nil)
	rules := list["rules"].([]interface{})
	if len(rules) != 2 {
		t.Fatalf("rules: %v", rules)
	}
	r0 := rules[0].(map[string]interface{})
	for _, k := range []string{"id", "name", "prefix", "status", "expiration_days", "noncurrent_days", "abort_multipart_days", "expire_delete_markers", "created_at"} {
		if _, ok := r0[k]; !ok {
			t.Fatalf("rule field %q missing: %v", k, r0)
		}
	}
	if r0["prefix"] != "logs/" || r0["status"] != "Disabled" || r0["noncurrent_days"].(float64) != 7 {
		t.Fatalf("rule 0: %v", r0)
	}
	if r1 := rules[1].(map[string]interface{}); r1["status"] != "Enabled" || r1["expiration_days"].(float64) != 3 {
		t.Fatalf("rule 1: %v", r1)
	}

	for _, body := range []map[string]interface{}{
		{"prefix": "x/"},
		{"prefix": "x/", "expiration_days": 1, "expire_delete_markers": true},
		{"prefix": "x/", "expiration_days": -1},
		{"prefix": "x/", "expiration_days": 1, "status": "On"},
	} {
		if code, _ := e.doJSON(t, http.MethodPost, "/admin/buckets/demo/lifecycle", body); code != http.StatusBadRequest {
			t.Fatalf("%v: %d, want 400", body, code)
		}
	}

	path := fmt.Sprintf("/admin/buckets/demo/lifecycle/%d", id)
	code, upd := e.doJSON(t, http.MethodPut, path, map[string]interface{}{"name": "logs2", "prefix": "logs/", "expire_delete_markers": true})
	if code != http.StatusOK || upd["name"] != "logs2" || upd["expire_delete_markers"] != true || upd["noncurrent_days"].(float64) != 0 || upd["status"] != "Enabled" {
		t.Fatalf("update: %d %v", code, upd)
	}
	if code, _ := e.doJSON(t, http.MethodPut, path, map[string]interface{}{"prefix": "tmp/", "expiration_days": 1}); code != http.StatusConflict {
		t.Fatalf("prefix conflict: %d", code)
	}
	e.addBucket(t, "other", 0)
	if code, _ := e.doJSON(t, http.MethodDelete, fmt.Sprintf("/admin/buckets/other/lifecycle/%d", id), nil); code != http.StatusNotFound {
		t.Fatalf("delete from other bucket: %d", code)
	}
	if code, _ := e.doJSON(t, http.MethodPut, fmt.Sprintf("/admin/buckets/other/lifecycle/%d", id), map[string]interface{}{"expiration_days": 1}); code != http.StatusNotFound {
		t.Fatalf("update from other bucket: %d", code)
	}
	if code, _ := e.doJSON(t, http.MethodDelete, path, nil); code != http.StatusOK {
		t.Fatalf("delete: %d", code)
	}
	if code, _ := e.doJSON(t, http.MethodDelete, path, nil); code != http.StatusNotFound {
		t.Fatalf("delete twice: %d", code)
	}
	// Old DELETE ?prefix still works.
	if code, _ := e.doJSON(t, http.MethodDelete, "/admin/buckets/demo/lifecycle?prefix=tmp/", nil); code != http.StatusOK {
		t.Fatalf("delete by prefix: %d", code)
	}
	if rules, _ := e.h.DB.GetLifecycleRules("demo"); len(rules) != 0 {
		t.Fatalf("rules left: %v", rules)
	}
}

// --- 9. webhooks v2 ---------------------------------------------------------------

func TestWebhooksV2(t *testing.T) {
	e := newContentEnv(t, "")
	var hits atomic.Int32
	var lastSig atomic.Value
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		lastSig.Store(r.Header.Get("X-Cloodsy-Signature"))
		if r.Header.Get("X-Cloodsy-Event") == webhook.TestEventType {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(target.Close)

	code, created := e.doJSON(t, http.MethodPost, "/admin/buckets/demo/webhooks", map[string]interface{}{"name": "n", "url": target.URL, "secret": "s3cr3t"})
	if code != http.StatusCreated || created["has_secret"] != true {
		t.Fatalf("create: %d %v", code, created)
	}
	id := int64(created["id"].(float64))
	_, list := e.doJSON(t, http.MethodGet, "/admin/buckets/demo/webhooks", nil)
	item := list["webhooks"].([]interface{})[0].(map[string]interface{})
	if item["has_secret"] != true {
		t.Fatalf("has_secret: %v", item)
	}
	if _, leaked := item["secret"]; leaked {
		t.Fatal("secret returned in list")
	}

	// Update without secret keeps it; active toggles.
	path := fmt.Sprintf("/admin/webhooks/%d", id)
	code, upd := e.doJSON(t, http.MethodPut, path, map[string]interface{}{"name": "renamed", "url": target.URL, "event_types": "s3:ObjectCreated:*", "active": false})
	if code != http.StatusOK || upd["name"] != "renamed" || upd["active"] != false || upd["has_secret"] != true || upd["event_types"] != "s3:ObjectCreated:*" {
		t.Fatalf("update: %d %v", code, upd)
	}
	if hook, _ := e.h.DB.GetWebhook(id); hook.Secret != "s3cr3t" {
		t.Fatalf("secret changed: %q", hook.Secret)
	}
	if code, _ := e.doJSON(t, http.MethodPut, path, map[string]interface{}{"url": "ftp://x/"}); code != http.StatusBadRequest {
		t.Fatalf("invalid url: %d", code)
	}
	if code, _ := e.doJSON(t, http.MethodPut, "/admin/webhooks/99999", map[string]interface{}{"url": target.URL}); code != http.StatusNotFound {
		t.Fatalf("unknown hook: %d", code)
	}

	// Synchronous test delivery (signed, recorded).
	code, res := e.doJSON(t, http.MethodPost, path+"/test", nil)
	if code != http.StatusOK || res["status_code"].(float64) != http.StatusAccepted || res["error"] != "" || res["duration_ms"] == nil {
		t.Fatalf("test: %d %v", code, res)
	}
	if sig, _ := lastSig.Load().(string); !strings.HasPrefix(sig, "sha256=") {
		t.Fatalf("test event not signed: %q", sig)
	}
	_, dl := e.doJSON(t, http.MethodGet, path+"/deliveries", nil)
	ds := dl["deliveries"].([]interface{})
	if len(ds) != 1 {
		t.Fatalf("deliveries: %v", dl)
	}
	d0 := ds[0].(map[string]interface{})
	if d0["event_type"] != webhook.TestEventType || d0["object_key"] != "cloodsy-test" || d0["status_code"].(float64) != 202 || d0["attempt"].(float64) != 1 || !strings.HasSuffix(d0["created_at"].(string), "Z") {
		t.Fatalf("delivery: %v", d0)
	}

	// Clearing the secret.
	_, upd = e.doJSON(t, http.MethodPut, path, map[string]interface{}{"url": target.URL, "active": true, "secret": ""})
	if upd["has_secret"] != false {
		t.Fatalf("secret not cleared: %v", upd)
	}

	// Real events are recorded by the dispatcher.
	d := webhook.NewDispatcher(e.h.DB, 1, slog.New(slog.NewTextHandler(io.Discard, nil)))
	e.h.Objects.Dispatcher = d
	t.Cleanup(d.Stop)
	e.mustUpload(t, "evt.txt", "x", "")
	deadline := time.Now().Add(5 * time.Second)
	for {
		rows, _ := e.h.DB.ListWebhookDeliveries(id, 10)
		if len(rows) == 2 && rows[0].ObjectKey == "evt.txt" && rows[0].StatusCode == 200 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("event delivery not recorded: %+v", rows)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if code, out := e.doJSON(t, http.MethodGet, path+"/deliveries?limit=1", nil); code != http.StatusOK || len(out["deliveries"].([]interface{})) != 1 {
		t.Fatalf("limit: %d %v", code, out)
	}
}

func TestWebhookDeliveryPrune(t *testing.T) {
	e := newContentEnv(t, "")
	hook, err := e.h.DB.CreateWebhook("demo", "", "http://127.0.0.1:9/", "*", "")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < db.WebhookDeliveryKeep+25; i++ {
		if err := e.h.DB.InsertWebhookDelivery(db.WebhookDelivery{WebhookID: hook.ID, EventType: "e", ObjectKey: fmt.Sprint(i), Attempt: 1}); err != nil {
			t.Fatal(err)
		}
	}
	rows, _ := e.h.DB.ListWebhookDeliveries(hook.ID, 1000)
	if len(rows) != db.WebhookDeliveryKeep || rows[0].ObjectKey != fmt.Sprint(db.WebhookDeliveryKeep+24) {
		t.Fatalf("kept %d, newest %q", len(rows), rows[0].ObjectKey)
	}
	// Deleting the hook drops its log; records for unknown hooks are ignored.
	e.h.DB.DeleteWebhook(hook.ID)
	if err := e.h.DB.InsertWebhookDelivery(db.WebhookDelivery{WebhookID: hook.ID, EventType: "e"}); err != nil {
		t.Fatal(err)
	}
	if rows, _ := e.h.DB.ListWebhookDeliveries(hook.ID, 1000); len(rows) != 0 {
		t.Fatalf("orphan deliveries: %d", len(rows))
	}
}

// --- 10. maintenance ------------------------------------------------------------

func TestMaintenanceScanAndOrphanDelete(t *testing.T) {
	e := newContentEnv(t, "")
	root := e.h.Storage.RootDir
	e.h.Config.Database.Path = filepath.Join(root, "db", "x.db") // db dir inside the root: must be protected
	os.MkdirAll(filepath.Join(root, "db"), 0700)

	// Orphans: a bucket-looking dir and a hidden sibling of a deleted bucket.
	os.MkdirAll(filepath.Join(root, "ghost", "sub"), 0700)
	os.WriteFile(filepath.Join(root, "ghost", "sub", "f.cloodsys3ext"), []byte("12345"), 0600)
	os.MkdirAll(filepath.Join(root, ".ghost-cache"), 0700)
	// Not orphans: hidden sibling of a live bucket, non-bucket names, a custom
	// storage dir that lives under the root.
	os.MkdirAll(filepath.Join(root, ".demo-multipart"), 0700)
	os.MkdirAll(filepath.Join(root, "Not_A_Bucket"), 0700)
	custom := filepath.Join(root, "fastdisk")
	if _, err := e.h.DB.CreateBucket("moved", custom); err != nil {
		t.Fatal(err)
	}
	e.h.Storage.SetBucketDir("moved", custom)
	e.h.Storage.CreateBucketDir("moved")

	// Missing file and untracked file in "demo".
	e.mustUpload(t, "gone.txt", "x", "")
	e.mustUpload(t, "kept.txt", "y", "")
	os.Remove(mustPath(t, e, "gone.txt"))
	bucketDir, _ := e.h.Storage.BucketPath("demo")
	stray := filepath.Join(bucketDir, "stray.cloodsys3ext")
	os.WriteFile(stray, []byte("zz"), 0600)
	os.WriteFile(filepath.Join(bucketDir, ".tmp-123"), []byte("t"), 0600)

	code, out := e.doJSON(t, http.MethodPost, "/admin/maintenance/scan", nil)
	if code != http.StatusOK {
		t.Fatalf("scan: %d %v", code, out)
	}
	var orphans []string
	for _, o := range out["orphan_dirs"].([]interface{}) {
		m := o.(map[string]interface{})
		orphans = append(orphans, m["path"].(string))
		if m["path"] == filepath.Join(root, "ghost") && m["bytes"].(float64) != 5 {
			t.Errorf("ghost bytes: %v", m)
		}
	}
	want := []string{filepath.Join(root, ".ghost-cache"), filepath.Join(root, "ghost")}
	if strings.Join(orphans, "|") != strings.Join(want, "|") {
		t.Fatalf("orphans = %v, want %v", orphans, want)
	}
	missing := out["missing_files"].([]interface{})
	if len(missing) != 1 || missing[0].(map[string]interface{})["key"] != "gone.txt" || missing[0].(map[string]interface{})["bucket"] != "demo" {
		t.Fatalf("missing: %v", missing)
	}
	untracked := out["untracked_files"].([]interface{})
	if len(untracked) != 1 || untracked[0].(map[string]interface{})["path"] != stray || untracked[0].(map[string]interface{})["bytes"].(float64) != 2 {
		t.Fatalf("untracked: %v", untracked)
	}
	if out["scanned_objects"].(float64) != 2 || out["truncated"] != false {
		t.Fatalf("counters: %v", out)
	}

	// Delete: anything not in a fresh scan is refused as a whole.
	for _, bad := range [][]string{{bucketDir}, {filepath.Join(root, "ghost", "..", "demo")}, {root}, {filepath.Join(root, "ghost") + "/"}, {}} {
		if code, _ := e.doJSON(t, http.MethodPost, "/admin/maintenance/orphan-dirs/delete", map[string]interface{}{"paths": bad}); code != http.StatusBadRequest {
			t.Fatalf("%v: %d, want 400", bad, code)
		}
	}
	if _, err := os.Stat(bucketDir); err != nil {
		t.Fatal("bucket dir touched")
	}
	code, out = e.doJSON(t, http.MethodPost, "/admin/maintenance/orphan-dirs/delete", map[string]interface{}{"paths": want})
	if code != http.StatusOK || out["deleted"].(float64) != 2 || len(out["failed"].([]interface{})) != 0 {
		t.Fatalf("delete: %d %v", code, out)
	}
	for _, p := range want {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatalf("%s still exists", p)
		}
	}
}

// --- 11. sessions -----------------------------------------------------------------

func TestSessionsListAndRevoke(t *testing.T) {
	e := newContentEnv(t, "")
	rec := doLogin(e.h, "198.51.100.9:1234", "root", "correct-horse")
	var second struct {
		Token string `json:"token"`
	}
	json.Unmarshal(rec.Body.Bytes(), &second)

	code, out := e.doJSON(t, http.MethodGet, "/admin/sessions", nil)
	if code != http.StatusOK {
		t.Fatalf("list: %d", code)
	}
	ss := out["sessions"].([]interface{})
	if len(ss) != 2 {
		t.Fatalf("sessions: %v", ss)
	}
	var otherID string
	currents := 0
	for _, s := range ss {
		m := s.(map[string]interface{})
		for _, k := range []string{"id", "username", "created_at", "last_seen_at", "expires_at", "ip", "user_agent", "current"} {
			if _, ok := m[k]; !ok {
				t.Fatalf("field %q missing: %v", k, m)
			}
		}
		if id := m["id"].(string); len(id) != 12 || strings.Contains(e.tok+second.Token, id) {
			t.Fatalf("id must be a 12-char digest, not a token slice: %q", id)
		}
		if m["current"] == true {
			currents++
		} else {
			otherID = m["id"].(string)
			if m["ip"] != "198.51.100.9" {
				t.Fatalf("ip: %v", m["ip"])
			}
		}
	}
	if currents != 1 || otherID == "" {
		t.Fatalf("current flags: %v", ss)
	}
	if code, _ := e.doJSON(t, http.MethodDelete, "/admin/sessions/"+otherID, nil); code != http.StatusOK {
		t.Fatalf("revoke other: %d", code)
	}
	if _, ok := e.h.validateToken(second.Token); ok {
		t.Fatal("revoked token still valid")
	}
	if code, _ := e.doJSON(t, http.MethodDelete, "/admin/sessions/"+otherID, nil); code != http.StatusNotFound {
		t.Fatalf("revoke twice: %d", code)
	}
	// Revoking the current session logs out.
	if code, out := e.doJSON(t, http.MethodDelete, "/admin/sessions/"+sessionID(e.tok), nil); code != http.StatusOK || out["current"] != true {
		t.Fatalf("revoke self: %d %v", code, out)
	}
	if code, _ := e.doJSON(t, http.MethodGet, "/admin/sessions", nil); code != http.StatusUnauthorized {
		t.Fatalf("after self-revoke: %d", code)
	}
}

// --- 12. logs ---------------------------------------------------------------------

func TestLogsEndpoint(t *testing.T) {
	e := newContentEnv(t, "")
	ring := applogger.NewRing(10)
	e.h.LogRing = ring
	e.h.Logger = slog.New(applogger.Tee(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelDebug}), ring))
	e.h.Logger.Debug("debug line")
	e.h.Logger.Info("hello world", "bucket", "demo")
	e.h.Logger.With("component", "x").Warn("careful", "Key", "Needle\nInjected")
	e.h.Logger.Error("boom", "error", fmt.Errorf("disk full"))

	code, out := e.doJSON(t, http.MethodGet, "/admin/logs?level=WARN", nil)
	if code != http.StatusOK {
		t.Fatalf("logs: %d", code)
	}
	entries := out["entries"].([]interface{})
	if len(entries) != 2 {
		t.Fatalf("WARN+ entries: %v", entries)
	}
	w := entries[0].(map[string]interface{})
	attrs := w["attrs"].(map[string]interface{})
	if w["level"] != "WARN" || w["message"] != "careful" || attrs["component"] != "x" || attrs["Key"] != `Needle\nInjected` {
		t.Fatalf("warn entry: %v", w)
	}
	if !strings.HasSuffix(w["time"].(string), "Z") {
		t.Fatalf("time: %v", w["time"])
	}
	last := int64(out["last_id"].(float64))

	_, out = e.doJSON(t, http.MethodGet, "/admin/logs?q=needle", nil)
	if es := out["entries"].([]interface{}); len(es) != 1 {
		t.Fatalf("q filter over attrs: %v", es)
	}
	_, out = e.doJSON(t, http.MethodGet, "/admin/logs?q=DISK", nil)
	if es := out["entries"].([]interface{}); len(es) != 1 || es[0].(map[string]interface{})["level"] != "ERROR" {
		t.Fatalf("q filter: %v", es)
	}
	_, out = e.doJSON(t, http.MethodGet, fmt.Sprintf("/admin/logs?since_id=%d", last), nil)
	es := out["entries"].([]interface{})
	// Only records logged by the handler after "last" (its own request logs, if any).
	for _, en := range es {
		if int64(en.(map[string]interface{})["id"].(float64)) <= last {
			t.Fatalf("since_id not honored: %v", es)
		}
	}
	_, out = e.doJSON(t, http.MethodGet, "/admin/logs?limit=1", nil)
	if es := out["entries"].([]interface{}); len(es) != 1 {
		t.Fatalf("limit: %v", es)
	}
	if code, _ := e.doJSON(t, http.MethodGet, "/admin/logs?level=LOUD", nil); code != http.StatusBadRequest {
		t.Fatalf("bad level: %d", code)
	}
}

// --- 13. config -------------------------------------------------------------------

func TestConfigEndpointMasksSecrets(t *testing.T) {
	e := newContentEnv(t, "")
	e.h.Config.Server.TLS.CertFile = "/etc/cert.pem"
	e.h.Config.Server.TLS.KeyFile = "/etc/key.pem"
	e.h.Config.Server.Listen = ":9000"
	code, out := e.doJSON(t, http.MethodGet, "/admin/config", nil)
	if code != http.StatusOK {
		t.Fatalf("config: %d", code)
	}
	tls := out["server"].(map[string]interface{})["tls"].(map[string]interface{})
	if tls["cert_file"] != "/etc/cert.pem" || tls["key_file"] != "/etc/key.pem" {
		t.Fatalf("file paths must be shown: %v", tls)
	}
	if _, ok := out["admin"]; !ok {
		t.Fatal("admin section missing")
	}

	doc := map[string]interface{}{
		"password": "p", "webhook_secret": "s", "api_token": "t", "private_key": "k", "key_file": "/k", "cert_file": "/c",
		"nested": []interface{}{map[string]interface{}{"secret_key": "x", "name": "n"}},
	}
	maskSecrets(doc)
	for _, k := range []string{"password", "webhook_secret", "api_token", "private_key"} {
		if doc[k] != maskedValue {
			t.Errorf("%s not masked: %v", k, doc[k])
		}
	}
	if doc["key_file"] != "/k" || doc["cert_file"] != "/c" {
		t.Errorf("file paths masked: %v", doc)
	}
	n := doc["nested"].([]interface{})[0].(map[string]interface{})
	if n["secret_key"] != maskedValue || n["name"] != "n" {
		t.Errorf("nested: %v", n)
	}
}

// --- 14. metrics ------------------------------------------------------------------

func TestMetricsEndpoint(t *testing.T) {
	e := newContentEnv(t, "")
	before := metrics.S3.Snapshot()
	s3 := httptest.NewServer(metrics.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		if r.Method == http.MethodPut {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		w.Write([]byte("hello"))
	}), metrics.S3))
	t.Cleanup(s3.Close)
	http.Get(s3.URL)
	req, _ := http.NewRequest(http.MethodPut, s3.URL, strings.NewReader("123"))
	resp, _ := http.DefaultClient.Do(req)
	resp.Body.Close()

	code, out := e.doJSON(t, http.MethodGet, "/admin/metrics", nil)
	if code != http.StatusOK {
		t.Fatalf("metrics: %d", code)
	}
	m := out["s3"].(map[string]interface{})
	if m["requests"].(float64)-float64(before.Requests) < 2 || m["errors_4xx"].(float64)-float64(before.Err4xx) < 1 ||
		m["bytes_in"].(float64)-float64(before.BytesIn) < 3 || m["bytes_out"].(float64)-float64(before.BytesOut) < 5 {
		t.Fatalf("s3 counters: %v (before %+v)", m, before)
	}
	if bm := m["by_method"].(map[string]interface{}); bm["GET"].(float64) < 1 || bm["PUT"].(float64) < 1 {
		t.Fatalf("by_method: %v", bm)
	}
	series := out["series"].([]interface{})
	if len(series) != metrics.SeriesLen {
		t.Fatalf("series len %d", len(series))
	}
	lastPt := series[len(series)-1].(map[string]interface{})
	if lastPt["requests"].(float64) < 2 || !strings.HasSuffix(lastPt["time"].(string), "Z") {
		t.Fatalf("current minute: %v", lastPt)
	}
	if _, ok := out["since"].(string); !ok {
		t.Fatalf("since: %v", out["since"])
	}
}

// --- 15. status -------------------------------------------------------------------

func TestStatusV2Fields(t *testing.T) {
	e := newContentEnv(t, "")
	e.h.Config.Server.Listen = ":9000"
	e.h.Config.Server.Region = "eu-west-1"
	_, out := e.doJSON(t, http.MethodGet, "/admin/status", nil)
	feats := map[string]bool{}
	for _, f := range out["features"].([]interface{}) {
		feats[f.(string)] = true
	}
	for _, f := range []string{"object-info", "object-range", "object-copy", "object-search", "prefix-stats", "object-metadata", "multipart", "lifecycle-v2", "webhooks-v2", "maintenance", "sessions", "logs", "config", "metrics"} {
		if !feats[f] {
			t.Errorf("feature %q missing", f)
		}
	}
	s3 := out["s3"].(map[string]interface{})
	if s3["listen"] != ":9000" || s3["region"] != "eu-west-1" || s3["tls"] != false {
		t.Errorf("s3: %v", s3)
	}
	if d, ok := s3["virtual_host_domains"].([]interface{}); !ok || len(d) != 0 {
		t.Errorf("virtual_host_domains must be []: %v", s3["virtual_host_domains"])
	}
	host := out["host"].(map[string]interface{})
	if host["os"] == "" || host["hostname"] == nil {
		t.Errorf("host: %v", host)
	}
	if _, ok := host["wsl_distro"]; !ok {
		t.Errorf("wsl_distro missing: %v", host)
	}
}

func TestRoutesRequireAuth(t *testing.T) {
	e := newContentEnv(t, "")
	for _, rt := range []struct{ method, path string }{
		{http.MethodGet, "/admin/buckets/demo/object/info?key=a"},
		{http.MethodHead, "/admin/buckets/demo/object?key=a"},
		{http.MethodPost, "/admin/buckets/demo/objects/copy"},
		{http.MethodGet, "/admin/buckets/demo/search"},
		{http.MethodPut, "/admin/buckets/demo/uploads/x/parts/1"},
		{http.MethodGet, "/admin/sessions"},
		{http.MethodGet, "/admin/logs"},
		{http.MethodGet, "/admin/config"},
		{http.MethodGet, "/admin/metrics"},
		{http.MethodPost, "/admin/maintenance/scan"},
		{http.MethodPost, "/admin/webhooks/1/test"},
	} {
		req, _ := http.NewRequest(rt.method, e.srv.URL+rt.path, nil)
		resp, err := e.srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s %s without token: %d", rt.method, rt.path, resp.StatusCode)
		}
	}
}
