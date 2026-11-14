package admin

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/onaonbir/Cloodsy-S3/db"
)

// contentEnv is a handler with one bucket, served through the real listener
// chain (body cap, progress timeout) on a loopback test server.
type contentEnv struct {
	h   *Handler
	srv *httptest.Server
	tok string
	b   *db.Bucket
}

func newContentEnv(t *testing.T, versioning string) *contentEnv {
	t.Helper()
	h := newTestHandler(t, nil)
	tok := adminToken(t, h)
	if _, err := h.DB.CreateBucket("demo", ""); err != nil {
		t.Fatal(err)
	}
	if versioning != "" {
		if err := h.DB.SetBucketVersioning("demo", versioning); err != nil {
			t.Fatal(err)
		}
	}
	h.Storage.CreateBucketDir("demo")
	b, _ := h.DB.GetBucket("demo")
	srv := httptest.NewServer(newRootHandler(h, slog.New(slog.NewTextHandler(io.Discard, nil))))
	t.Cleanup(srv.Close)
	return &contentEnv{h: h, srv: srv, tok: tok, b: b}
}

func (e *contentEnv) do(t *testing.T, method, path string, body io.Reader, hdr map[string]string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, e.srv.URL+path, body)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+e.tok)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := e.srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func objPath(key string, extra ...string) string {
	q := url.Values{"key": {key}}
	for i := 0; i+1 < len(extra); i += 2 {
		q.Set(extra[i], extra[i+1])
	}
	return "/admin/buckets/demo/object?" + q.Encode()
}

type uploadResp struct {
	Key       string `json:"key"`
	Size      int64  `json:"size"`
	ETag      string `json:"etag"`
	VersionID string `json:"version_id"`
	Error     string `json:"error"`
}

func (e *contentEnv) upload(t *testing.T, key string, body []byte, ct string) (int, uploadResp) {
	t.Helper()
	hdr := map[string]string{}
	if ct != "" {
		hdr["Content-Type"] = ct
	}
	resp := e.do(t, http.MethodPut, objPath(key), bytes.NewReader(body), hdr)
	var out uploadResp
	json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func jsonError(t *testing.T, resp *http.Response) string {
	t.Helper()
	var out struct {
		Error string `json:"error"`
	}
	json.NewDecoder(resp.Body).Decode(&out)
	return out.Error
}

func TestObjectUploadDownloadRoundtrip(t *testing.T) {
	e := newContentEnv(t, "")
	body := []byte("hello, admin upload")
	key := "docs/report ğ;x.txt"

	code, up := e.upload(t, key, body, "text/plain")
	if code != http.StatusOK {
		t.Fatalf("upload: %d %+v", code, up)
	}
	if up.Key != key || up.Size != int64(len(body)) || up.ETag == "" || up.VersionID != "" {
		t.Fatalf("upload response: %+v", up)
	}

	resp := e.do(t, http.MethodGet, objPath(key), nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("download: %d", resp.StatusCode)
	}
	got, _ := io.ReadAll(resp.Body)
	if !bytes.Equal(got, body) {
		t.Fatalf("body = %q", got)
	}
	for k, want := range map[string]string{
		"Content-Type":        "text/plain",
		"Content-Length":      "19",
		"ETag":                up.ETag,
		"Content-Disposition": "attachment; filename*=UTF-8''report%20%C4%9F%3Bx.txt",
	} {
		if v := resp.Header.Get(k); v != want {
			t.Errorf("%s = %q, want %q", k, v, want)
		}
	}
	if resp.Header.Get("Last-Modified") == "" {
		t.Error("Last-Modified missing")
	}
	if v := resp.Header.Get("X-Version-Id"); v != "" {
		t.Errorf("X-Version-Id on unversioned bucket: %q", v)
	}

	// Default content type, missing key, invalid key.
	if code, _ := e.upload(t, "raw.bin", []byte{1, 2, 3}, ""); code != http.StatusOK {
		t.Fatalf("upload raw: %d", code)
	}
	if m, _ := e.h.DB.GetObjectMeta(e.b.ID, "raw.bin"); m == nil || m.ContentType != "application/octet-stream" {
		t.Fatalf("default content type: %+v", m)
	}
	if resp := e.do(t, http.MethodGet, objPath("nope"), nil, nil); resp.StatusCode != http.StatusNotFound || jsonError(t, resp) != "object not found" {
		t.Fatalf("missing key: %d", resp.StatusCode)
	}
	for _, bad := range []string{"", "a/../b"} {
		if resp := e.do(t, http.MethodGet, objPath(bad), nil, nil); resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("download invalid key %q: %d", bad, resp.StatusCode)
		}
		if code, _ := e.upload(t, bad, []byte("x"), ""); code != http.StatusBadRequest {
			t.Fatalf("upload invalid key %q: %d", bad, code)
		}
	}
	if resp := e.do(t, http.MethodGet, "/admin/buckets/missing/object?key=a", nil, nil); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown bucket: %d", resp.StatusCode)
	}
}

func TestObjectUploadFolderMarker(t *testing.T) {
	e := newContentEnv(t, "")
	if code, out := e.upload(t, "folder/", nil, ""); code != http.StatusOK || out.Size != 0 {
		t.Fatalf("folder marker: %d %+v", code, out)
	}
	if m, _ := e.h.DB.GetObjectMeta(e.b.ID, "folder/"); m == nil || m.Size != 0 {
		t.Fatalf("folder marker row: %+v", m)
	}
	if code, _ := e.upload(t, "folder2/", []byte("data"), ""); code != http.StatusBadRequest {
		t.Fatalf("folder key with body: %d, want 400", code)
	}
}

func TestObjectUploadBypassesJSONBodyCap(t *testing.T) {
	e := newContentEnv(t, "")
	big := bytes.Repeat([]byte("a"), maxJSONBody+4096)

	code, out := e.upload(t, "big.bin", big, "application/octet-stream")
	if code != http.StatusOK || out.Size != int64(len(big)) {
		t.Fatalf("large upload: %d %+v", code, out)
	}
	resp := e.do(t, http.MethodGet, objPath("big.bin"), nil, nil)
	got, _ := io.ReadAll(resp.Body)
	if len(got) != len(big) {
		t.Fatalf("downloaded %d bytes, want %d", len(got), len(big))
	}

	// The cap still guards the JSON endpoints.
	payload := `{"name":"` + strings.Repeat("x", maxJSONBody) + `"}`
	resp = e.do(t, http.MethodPost, "/admin/buckets/demo/credentials", strings.NewReader(payload), map[string]string{"Content-Type": "application/json"})
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized JSON body: %d, want 413", resp.StatusCode)
	}
	// ... including PUT routes on the same bucket that are not the upload.
	resp = e.do(t, http.MethodPut, "/admin/buckets/demo/quota", strings.NewReader(`{"quota_bytes":`+strings.Repeat(" ", maxJSONBody)+`1}`), nil)
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized quota body: %d, want 413", resp.StatusCode)
	}
}

func TestObjectUploadQuotaExceeded(t *testing.T) {
	e := newContentEnv(t, "")
	if err := e.h.DB.SetBucketQuota("demo", 10); err != nil {
		t.Fatal(err)
	}
	e.b, _ = e.h.DB.GetBucket("demo")
	if code, _ := e.upload(t, "small", []byte("12345"), ""); code != http.StatusOK {
		t.Fatalf("within quota: %d", code)
	}
	code, out := e.upload(t, "large", []byte("1234567890"), "")
	if code != http.StatusRequestEntityTooLarge || out.Error != "quota exceeded" {
		t.Fatalf("over quota: %d %+v", code, out)
	}
	if m, _ := e.h.DB.GetObjectMeta(e.b.ID, "large"); m != nil {
		t.Fatal("rejected object was stored")
	}

	// Chunked body (no Content-Length): rejected while streaming.
	resp := e.do(t, http.MethodPut, objPath("chunked"), io.MultiReader(strings.NewReader("1234567890")), nil)
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("chunked over quota: %d", resp.StatusCode)
	}
}

func TestObjectVersionsAndVersionedDownload(t *testing.T) {
	e := newContentEnv(t, "Enabled")
	code, v1 := e.upload(t, "k.txt", []byte("one"), "text/plain")
	if code != http.StatusOK || v1.VersionID == "" {
		t.Fatalf("v1: %d %+v", code, v1)
	}
	_, v2 := e.upload(t, "k.txt", []byte("two!"), "text/plain")
	if v2.VersionID == "" || v2.VersionID == v1.VersionID {
		t.Fatalf("v2: %+v", v2)
	}

	type version struct {
		VersionID      string `json:"version_id"`
		Size           int64  `json:"size"`
		ETag           string `json:"etag"`
		ContentType    string `json:"content_type"`
		LastModified   string `json:"last_modified"`
		IsLatest       bool   `json:"is_latest"`
		IsDeleteMarker bool   `json:"is_delete_marker"`
	}
	listVersions := func(key string) []version {
		resp := e.do(t, http.MethodGet, "/admin/buckets/demo/versions?key="+url.QueryEscape(key), nil, nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("versions: %d", resp.StatusCode)
		}
		raw, _ := io.ReadAll(resp.Body)
		if !bytes.Contains(raw, []byte(`"versions":[`)) {
			t.Fatalf("versions must be an array: %s", raw)
		}
		var out struct {
			Versions []version `json:"versions"`
		}
		json.Unmarshal(raw, &out)
		return out.Versions
	}

	vs := listVersions("k.txt")
	if len(vs) != 2 {
		t.Fatalf("versions = %d, want 2", len(vs))
	}
	if vs[0].VersionID != v2.VersionID || !vs[0].IsLatest || vs[1].VersionID != v1.VersionID || vs[1].IsLatest {
		t.Fatalf("order/latest wrong: %+v", vs)
	}
	if vs[1].Size != 3 || vs[1].ContentType != "text/plain" || vs[1].ETag != v1.ETag || !strings.HasSuffix(vs[1].LastModified, "Z") {
		t.Fatalf("v1 fields: %+v", vs[1])
	}
	if got := listVersions("none"); len(got) != 0 {
		t.Fatalf("unknown key versions: %+v", got)
	}
	if resp := e.do(t, http.MethodGet, "/admin/buckets/missing/versions?key=a", nil, nil); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown bucket: %d", resp.StatusCode)
	}

	// Download by version id.
	resp := e.do(t, http.MethodGet, objPath("k.txt", "versionId", v1.VersionID), nil, nil)
	got, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || string(got) != "one" || resp.Header.Get("X-Version-Id") != v1.VersionID {
		t.Fatalf("v1 download: %d %q %q", resp.StatusCode, got, resp.Header.Get("X-Version-Id"))
	}
	resp = e.do(t, http.MethodGet, objPath("k.txt"), nil, nil)
	got, _ = io.ReadAll(resp.Body)
	if string(got) != "two!" || resp.Header.Get("X-Version-Id") != v2.VersionID {
		t.Fatalf("latest download: %q %q", got, resp.Header.Get("X-Version-Id"))
	}
	if resp := e.do(t, http.MethodGet, objPath("k.txt", "versionId", "bogus"), nil, nil); resp.StatusCode != http.StatusNotFound || jsonError(t, resp) != "version not found" {
		t.Fatalf("unknown version: %d", resp.StatusCode)
	}

	// Delete marker becomes latest: plain download 404s, old versions stay reachable.
	if _, err := e.h.Objects.Delete(e.b, "k.txt", ""); err != nil {
		t.Fatal(err)
	}
	if resp := e.do(t, http.MethodGet, objPath("k.txt"), nil, nil); resp.StatusCode != http.StatusNotFound || jsonError(t, resp) != "object not found" {
		t.Fatalf("delete-marker latest: %d", resp.StatusCode)
	}
	vs = listVersions("k.txt")
	if len(vs) != 3 || !vs[0].IsDeleteMarker || !vs[0].IsLatest {
		t.Fatalf("after delete: %+v", vs)
	}
	if resp := e.do(t, http.MethodGet, objPath("k.txt", "versionId", vs[0].VersionID), nil, nil); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("download of delete marker version: %d", resp.StatusCode)
	}
	if resp := e.do(t, http.MethodGet, objPath("k.txt", "versionId", v2.VersionID), nil, nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("noncurrent version download: %d", resp.StatusCode)
	}
}

func TestStatusReportsNewFields(t *testing.T) {
	e := newContentEnv(t, "")
	e.h.Config.Server.TLS.Enabled = true
	e.h.Config.Server.TLS.ACME.Enabled = true
	e.h.Config.Server.TLS.ACME.Domains = []string{"s3.example.com"}
	e.h.Config.Admin.TLS.Enabled = true

	resp := e.do(t, http.MethodGet, "/admin/status", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: %d", resp.StatusCode)
	}
	var out map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"status", "version", "buckets", "admin_exists", "webdav"} {
		if _, ok := out[k]; !ok {
			t.Errorf("existing field %q missing", k)
		}
	}
	feats, _ := out["features"].([]interface{})
	want := map[string]bool{"object-download": false, "object-upload": false, "object-versions": false}
	for _, f := range feats {
		if s, ok := f.(string); ok {
			want[s] = true
		}
	}
	for f, seen := range want {
		if !seen {
			t.Errorf("feature %q missing: %v", f, out["features"])
		}
	}
	if _, ok := out["uptime_seconds"].(float64); !ok {
		t.Errorf("uptime_seconds: %v", out["uptime_seconds"])
	}
	tlsInfo, _ := out["tls"].(map[string]interface{})
	if tlsInfo["s3"] != true || tlsInfo["admin"] != true || tlsInfo["webdav"] != false || tlsInfo["acme"] != true {
		t.Errorf("tls: %v", tlsInfo)
	}
	if d, _ := tlsInfo["acme_domains"].([]interface{}); len(d) != 1 || d[0] != "s3.example.com" {
		t.Errorf("acme_domains: %v", tlsInfo["acme_domains"])
	}
	st, _ := out["storage"].(map[string]interface{})
	if st["path"] != e.h.Storage.RootDir {
		t.Errorf("storage.path = %v, want %s", st["path"], e.h.Storage.RootDir)
	}
	// Linux CI: statfs works, so the figures must be present and sane.
	total, _ := st["total_bytes"].(float64)
	free, okFree := st["free_bytes"].(float64)
	if _, _, ok := diskUsage(e.h.Storage.RootDir); ok && (total <= 0 || !okFree || free > total) {
		t.Errorf("storage figures: %v", st)
	}
}

func TestContentDisposition(t *testing.T) {
	cases := map[string]string{
		"a/b/file.txt":  "attachment; filename*=UTF-8''file.txt",
		"dir/":          "attachment; filename*=UTF-8''dir",
		`we"ird,name=1`: "attachment; filename*=UTF-8''we%22ird%2Cname%3D1",
		"çok güzel.pdf": "attachment; filename*=UTF-8''%C3%A7ok%20g%C3%BCzel.pdf",
	}
	for in, want := range cases {
		if got := contentDisposition(in); got != want {
			t.Errorf("contentDisposition(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCORSPreflightForUpload(t *testing.T) {
	h := newTestHandler(t, nil)
	h.Config.Admin.CORSOrigins = []string{"https://gui.example"}
	req := httptest.NewRequest(http.MethodOptions, "/admin/buckets/demo/object?key=a", nil)
	req.Header.Set("Origin", "https://gui.example")
	req.Header.Set("Access-Control-Request-Method", "PUT")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("preflight: %d", rec.Code)
	}
	if !strings.Contains(rec.Header().Get("Access-Control-Allow-Methods"), "PUT") {
		t.Fatalf("PUT not allowed: %q", rec.Header().Get("Access-Control-Allow-Methods"))
	}
	if !strings.Contains(rec.Header().Get("Access-Control-Expose-Headers"), "X-Version-Id") {
		t.Fatalf("expose headers: %q", rec.Header().Get("Access-Control-Expose-Headers"))
	}
}
