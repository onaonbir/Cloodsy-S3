package handler_test

import (
	"bytes"
	"crypto/md5"
	"encoding/base64"
	"encoding/xml"
	"fmt"
	"hash/crc32"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

func md5Quoted(b []byte) string {
	sum := md5.Sum(b)
	return fmt.Sprintf("\"%x\"", sum[:])
}

func crc32B64(b []byte) string {
	c := crc32.ChecksumIEEE(b)
	return base64.StdEncoding.EncodeToString([]byte{byte(c >> 24), byte(c >> 16), byte(c >> 8), byte(c)})
}

func TestPutGetHeadRoundtrip(t *testing.T) {
	s := newStack(t)
	body := []byte("hello roundtrip")
	hdr := http.Header{}
	hdr.Set("Content-Type", "text/plain; charset=utf-8")
	hdr.Set("X-Amz-Meta-Owner", "alice")
	hdr.Set("X-Amz-Meta-Tag", "x")
	hdr.Set("Cache-Control", "max-age=3600")
	resp, b := s.put("docs/readme.txt", body, hdr)
	s.expectStatus(resp, b, http.StatusOK)
	if resp.Header.Get("ETag") != md5Quoted(body) {
		t.Fatalf("PUT ETag %q", resp.Header.Get("ETag"))
	}
	if resp.Header.Get("x-amz-version-id") != "" {
		t.Fatal("unversioned bucket must not return a version id")
	}

	for _, method := range []string{http.MethodGet, http.MethodHead} {
		resp, b := s.do(req{method: method, key: "docs/readme.txt"})
		s.expectStatus(resp, b, http.StatusOK)
		h := resp.Header
		if h.Get("ETag") != md5Quoted(body) {
			t.Errorf("%s ETag %q", method, h.Get("ETag"))
		}
		if h.Get("Content-Type") != "text/plain; charset=utf-8" {
			t.Errorf("%s Content-Type %q", method, h.Get("Content-Type"))
		}
		if h.Get("X-Amz-Meta-Owner") != "alice" || h.Get("X-Amz-Meta-Tag") != "x" {
			t.Errorf("%s user metadata missing: %v", method, h)
		}
		if h.Get("Cache-Control") != "max-age=3600" {
			t.Errorf("%s Cache-Control %q", method, h.Get("Cache-Control"))
		}
		if h.Get("Content-Disposition") != "" {
			t.Errorf("%s unexpected Content-Disposition %q", method, h.Get("Content-Disposition"))
		}
		if h.Get("Content-Length") != fmt.Sprint(len(body)) {
			t.Errorf("%s Content-Length %q", method, h.Get("Content-Length"))
		}
		if h.Get("Accept-Ranges") != "bytes" {
			t.Errorf("%s Accept-Ranges %q", method, h.Get("Accept-Ranges"))
		}
		if lm, err := http.ParseTime(h.Get("Last-Modified")); err != nil || time.Since(lm) > time.Minute {
			t.Errorf("%s Last-Modified %q: %v", method, h.Get("Last-Modified"), err)
		}
		if method == http.MethodGet && !bytes.Equal(b, body) {
			t.Errorf("GET body %q", b)
		}
		if method == http.MethodHead && len(b) != 0 {
			t.Errorf("HEAD returned a body")
		}
	}
	resp, b = s.get("missing", nil)
	s.expectError(resp, b, http.StatusNotFound, "NoSuchKey")
	resp, b = s.del("docs/readme.txt", nil)
	s.expectStatus(resp, b, http.StatusNoContent)
	resp, b = s.get("docs/readme.txt", nil)
	s.expectError(resp, b, http.StatusNotFound, "NoSuchKey")
	// Deleting a missing key is still 204.
	resp, b = s.del("docs/readme.txt", nil)
	s.expectStatus(resp, b, http.StatusNoContent)
}

func TestPutIntegrityChecks(t *testing.T) {
	s := newStack(t)
	body := []byte("integrity payload")

	t.Run("wrong sha256", func(t *testing.T) {
		resp, b := s.do(req{method: http.MethodPut, key: "sha", body: body, opts: signOpts{payloadHash: sha256Hex([]byte("other"))}})
		s.expectError(resp, b, http.StatusBadRequest, "XAmzContentSHA256Mismatch")
		resp, b = s.get("sha", nil)
		s.expectError(resp, b, http.StatusNotFound, "NoSuchKey")
		if s.store.ObjectExists(testBucket, "sha") {
			t.Fatal("bytes left on disk")
		}
	})
	t.Run("bad content-md5", func(t *testing.T) {
		hdr := http.Header{}
		wrong := md5.Sum([]byte("nope"))
		hdr.Set("Content-MD5", base64.StdEncoding.EncodeToString(wrong[:]))
		resp, b := s.put("md5", body, hdr)
		s.expectError(resp, b, http.StatusBadRequest, "BadDigest")
		hdr.Set("Content-MD5", "not-base64!")
		resp, b = s.put("md5", body, hdr)
		s.expectError(resp, b, http.StatusBadRequest, "InvalidDigest")
		right := md5.Sum(body)
		hdr.Set("Content-MD5", base64.StdEncoding.EncodeToString(right[:]))
		resp, b = s.put("md5", body, hdr)
		s.expectStatus(resp, b, http.StatusOK)
	})
	t.Run("crc32 checksum", func(t *testing.T) {
		hdr := http.Header{}
		hdr.Set("x-amz-checksum-crc32", crc32B64(body))
		resp, b := s.put("crc", body, hdr)
		s.expectStatus(resp, b, http.StatusOK)
		if resp.Header.Get("x-amz-checksum-crc32") != crc32B64(body) {
			t.Fatalf("checksum not echoed: %v", resp.Header)
		}
		hdr.Set("x-amz-checksum-crc32", crc32B64([]byte("different")))
		resp, b = s.put("crc2", body, hdr)
		s.expectError(resp, b, http.StatusBadRequest, "BadDigest")
		if s.store.ObjectExists(testBucket, "crc2") {
			t.Fatal("bytes left on disk after BadDigest")
		}
	})
	t.Run("unsigned payload", func(t *testing.T) {
		resp, b := s.do(req{method: http.MethodPut, key: "unsigned", body: body, opts: signOpts{payloadHash: "UNSIGNED-PAYLOAD"}})
		s.expectStatus(resp, b, http.StatusOK)
		resp, b = s.get("unsigned", nil)
		s.expectStatus(resp, b, http.StatusOK)
		if !bytes.Equal(b, body) {
			t.Fatal("body mismatch")
		}
		// Server-side opt-out.
		s.cfg.Server.RequirePayloadSignature = true
		defer func() { s.cfg.Server.RequirePayloadSignature = false }()
		resp, b = s.do(req{method: http.MethodPut, key: "unsigned2", body: body, opts: signOpts{payloadHash: "UNSIGNED-PAYLOAD"}})
		s.expectError(resp, b, http.StatusBadRequest, "InvalidRequest")
	})
	t.Run("aws-chunked signed", func(t *testing.T) {
		payload := bytes.Repeat([]byte("chunky!"), 20000) // 140 KB across several chunks
		resp, b := s.putChunked("chunked", payload, 65536, nil, nil)
		s.expectStatus(resp, b, http.StatusOK)
		if resp.Header.Get("ETag") != md5Quoted(payload) {
			t.Fatalf("ETag %q", resp.Header.Get("ETag"))
		}
		resp, b = s.get("chunked", nil)
		s.expectStatus(resp, b, http.StatusOK)
		if !bytes.Equal(b, payload) {
			t.Fatalf("decoded body mismatch (%d bytes)", len(b))
		}
		// Tampered chunk byte → signature mismatch, object unchanged.
		resp, b = s.putChunked("chunked", []byte("replacement"), 4, nil, func(enc []byte) {
			i := bytes.Index(enc, []byte("\r\nrepl"))
			enc[i+2] = 'X'
		})
		s.expectError(resp, b, http.StatusForbidden, "SignatureDoesNotMatch")
		resp, b = s.get("chunked", nil)
		s.expectStatus(resp, b, http.StatusOK)
		if !bytes.Equal(b, payload) {
			t.Fatal("original clobbered by tampered upload")
		}
		// Truncated stream (final chunk missing) → IncompleteBody.
		resp, b = s.putChunked("chunked-trunc", []byte("abcdefgh"), 4, nil, func(enc []byte) {
			// Blank out the terminating chunk so the decoder sees garbage.
			i := bytes.LastIndex(enc, []byte("0;chunk-signature="))
			copy(enc[i:], bytes.Repeat([]byte("\r\n"), (len(enc)-i)/2))
		})
		if resp.StatusCode == http.StatusOK {
			t.Fatal("truncated chunked upload accepted")
		}
		if s.store.ObjectExists(testBucket, "chunked-trunc") {
			t.Fatal("bytes left on disk after truncated upload")
		}
	})
	t.Run("declared length mismatch", func(t *testing.T) {
		u, _ := url.Parse(s.srv.URL)
		u.Path = "/" + testBucket + "/short"
		r, _ := http.NewRequest(http.MethodPut, u.String(), strings.NewReader("abc"))
		r.ContentLength = 10
		signHTTP(r, u.Path, signOpts{accessKey: testAccessKey, secret: testSecret, region: testRegion, now: time.Now(), payloadHash: "UNSIGNED-PAYLOAD"})
		resp, err := s.client.Do(r)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				t.Fatal("short body accepted")
			}
		}
		if s.store.ObjectExists(testBucket, "short") {
			t.Fatal("short object persisted")
		}
	})
}

func TestRangeRequests(t *testing.T) {
	s := newStack(t)
	body := []byte("0123456789abcdefghij") // 20 bytes
	s.mustPut("range", body)
	rng := func(v string) (*http.Response, []byte) {
		h := http.Header{}
		h.Set("Range", v)
		return s.get("range", h)
	}
	cases := []struct {
		hdr, want, cr string
	}{
		{"bytes=0-4", "01234", "bytes 0-4/20"},
		{"bytes=-5", "fghij", "bytes 15-19/20"},
		{"bytes=15-", "fghij", "bytes 15-19/20"},
		{"bytes=5-100", "56789abcdefghij", "bytes 5-19/20"},
	}
	for _, c := range cases {
		resp, b := rng(c.hdr)
		s.expectStatus(resp, b, http.StatusPartialContent)
		if string(b) != c.want || resp.Header.Get("Content-Range") != c.cr || resp.Header.Get("Content-Length") != fmt.Sprint(len(c.want)) {
			t.Errorf("%s: body %q cr %q cl %q", c.hdr, b, resp.Header.Get("Content-Range"), resp.Header.Get("Content-Length"))
		}
	}
	// Multi-range → whole object, 200.
	resp, b := rng("bytes=0-1,5-6")
	s.expectStatus(resp, b, http.StatusOK)
	if !bytes.Equal(b, body) {
		t.Fatalf("multi-range body %q", b)
	}
	// Out of range → 416 with Content-Range bytes */N.
	resp, b = rng("bytes=100-200")
	s.expectError(resp, b, http.StatusRequestedRangeNotSatisfiable, "InvalidRange")
	if resp.Header.Get("Content-Range") != "bytes */20" {
		t.Fatalf("Content-Range %q", resp.Header.Get("Content-Range"))
	}
	// HEAD with range reports partial content headers.
	h := http.Header{}
	h.Set("Range", "bytes=2-3")
	resp, b = s.head("range", h)
	s.expectStatus(resp, b, http.StatusPartialContent)
	if resp.Header.Get("Content-Length") != "2" || resp.Header.Get("Content-Range") != "bytes 2-3/20" {
		t.Fatalf("HEAD range headers %v", resp.Header)
	}
	// If-Range with a stale ETag serves the whole object.
	h = http.Header{}
	h.Set("Range", "bytes=0-1")
	h.Set("If-Range", "\"stale\"")
	resp, b = s.get("range", h)
	s.expectStatus(resp, b, http.StatusOK)
	if !bytes.Equal(b, body) {
		t.Fatal("If-Range mismatch should serve the full object")
	}
}

func TestConditionalRequests(t *testing.T) {
	s := newStack(t)
	body := []byte("conditional")
	etag := s.mustPut("cond", body)

	h := http.Header{}
	h.Set("If-None-Match", etag)
	resp, b := s.get("cond", h)
	s.expectStatus(resp, b, http.StatusNotModified)
	if resp.Header.Get("ETag") != etag {
		t.Fatalf("304 without ETag: %v", resp.Header)
	}
	if len(b) != 0 {
		t.Fatal("304 must not carry a body")
	}
	h.Set("If-None-Match", "\"other\"")
	resp, b = s.get("cond", h)
	s.expectStatus(resp, b, http.StatusOK)

	h = http.Header{}
	h.Set("If-Match", "\"other\"")
	resp, b = s.get("cond", h)
	s.expectError(resp, b, http.StatusPreconditionFailed, "PreconditionFailed")
	h.Set("If-Match", etag)
	resp, b = s.get("cond", h)
	s.expectStatus(resp, b, http.StatusOK)

	h = http.Header{}
	h.Set("If-Modified-Since", time.Now().Add(time.Hour).UTC().Format(http.TimeFormat))
	resp, b = s.head("cond", h)
	s.expectStatus(resp, b, http.StatusNotModified)
	h = http.Header{}
	h.Set("If-Unmodified-Since", time.Now().Add(-time.Hour).UTC().Format(http.TimeFormat))
	resp, b = s.get("cond", h)
	s.expectError(resp, b, http.StatusPreconditionFailed, "PreconditionFailed")
}

func TestObjectKeys(t *testing.T) {
	s := newStack(t)
	t.Run("unicode and spaces", func(t *testing.T) {
		key := "fotoğraflar/2024 yaz/plaj günü #1 +ü&.jpg"
		body := []byte("jpeg-ish")
		s.mustPut(key, body)
		resp, b := s.get(key, nil)
		s.expectStatus(resp, b, http.StatusOK)
		if !bytes.Equal(b, body) {
			t.Fatal("body mismatch")
		}
		resp, b = s.do(req{method: http.MethodGet, query: url.Values{"prefix": {"fotoğraflar/"}}})
		s.expectStatus(resp, b, http.StatusOK)
		lr := parseList(t, b)
		if len(lr.Contents) != 1 || lr.Contents[0].Key != key {
			t.Fatalf("listed %+v", lr.Contents)
		}
	})
	t.Run("double dots inside a segment", func(t *testing.T) {
		s.mustPut("report..final.pdf", []byte("pdf"))
		resp, b := s.get("report..final.pdf", nil)
		s.expectStatus(resp, b, http.StatusOK)
		s.mustPut("..dotfile", []byte("x"))
	})
	t.Run("path traversal segment", func(t *testing.T) {
		resp, b := s.put("a/../x", []byte("x"), nil)
		s.expectError(resp, b, http.StatusBadRequest, "InvalidArgument")
		resp, b = s.put("../x", []byte("x"), nil)
		s.expectError(resp, b, http.StatusBadRequest, "InvalidArgument")
		resp, b = s.put("a/./x", []byte("x"), nil)
		s.expectError(resp, b, http.StatusBadRequest, "InvalidArgument")
		if s.store.ObjectExists(testBucket, "x") {
			t.Fatal("traversal wrote a file")
		}
	})
	t.Run("directory placeholder", func(t *testing.T) {
		s.mustPut("dir/", nil)
		s.mustPut("dir", []byte("file"))
		resp, b := s.get("dir/", nil)
		s.expectStatus(resp, b, http.StatusOK)
		if len(b) != 0 {
			t.Fatalf("placeholder body %q", b)
		}
		resp, b = s.get("dir", nil)
		s.expectStatus(resp, b, http.StatusOK)
		if string(b) != "file" {
			t.Fatalf("file body %q", b)
		}
		resp, b = s.del("dir/", nil)
		s.expectStatus(resp, b, http.StatusNoContent)
		resp, b = s.get("dir", nil)
		s.expectStatus(resp, b, http.StatusOK)
		resp, b = s.get("dir/", nil)
		s.expectError(resp, b, http.StatusNotFound, "NoSuchKey")
		resp, b = s.del("dir", nil)
		s.expectStatus(resp, b, http.StatusNoContent)
	})
	t.Run("key too long", func(t *testing.T) {
		resp, b := s.put(strings.Repeat("k", 1025), []byte("x"), nil)
		s.expectError(resp, b, http.StatusBadRequest, "KeyTooLongError")
	})
}

func TestCopyObject(t *testing.T) {
	s := newStack(t)
	src := "src dir/ünï source.txt"
	body := []byte("copy me")
	hdr := http.Header{}
	hdr.Set("Content-Type", "text/x-source")
	hdr.Set("X-Amz-Meta-Orig", "yes")
	resp, b := s.put(src, body, hdr)
	s.expectStatus(resp, b, http.StatusOK)

	copySource := "/" + testBucket + "/" + uriEncode(src, false)
	h := http.Header{}
	h.Set("X-Amz-Copy-Source", copySource)
	resp, b = s.put("dest/copy.txt", nil, h)
	s.expectStatus(resp, b, http.StatusOK)
	var cr struct {
		ETag string `xml:"ETag"`
	}
	if err := xml.Unmarshal(b, &cr); err != nil || cr.ETag != md5Quoted(body) {
		t.Fatalf("CopyObjectResult %s (%v)", b, err)
	}
	resp, b = s.get("dest/copy.txt", nil)
	s.expectStatus(resp, b, http.StatusOK)
	if !bytes.Equal(b, body) || resp.Header.Get("Content-Type") != "text/x-source" || resp.Header.Get("X-Amz-Meta-Orig") != "yes" {
		t.Fatalf("copied object: body=%q headers=%v", b, resp.Header)
	}

	// Copy to self without REPLACE → 400.
	h.Set("X-Amz-Copy-Source", copySource)
	resp, b = s.put(src, nil, h)
	s.expectError(resp, b, http.StatusBadRequest, "InvalidRequest")

	// Copy to self with REPLACE applies new metadata.
	h.Set("x-amz-metadata-directive", "REPLACE")
	h.Set("X-Amz-Meta-New", "v2")
	h.Set("Content-Type", "text/replaced")
	resp, b = s.put(src, nil, h)
	s.expectStatus(resp, b, http.StatusOK)
	resp, b = s.get(src, nil)
	s.expectStatus(resp, b, http.StatusOK)
	if !bytes.Equal(b, body) {
		t.Fatal("self-copy changed bytes")
	}
	if resp.Header.Get("X-Amz-Meta-New") != "v2" || resp.Header.Get("X-Amz-Meta-Orig") != "" || resp.Header.Get("Content-Type") != "text/replaced" {
		t.Fatalf("REPLACE metadata not applied: %v", resp.Header)
	}

	// Missing source / bad source.
	h = http.Header{}
	h.Set("X-Amz-Copy-Source", "/"+testBucket+"/does-not-exist")
	resp, b = s.put("dest2", nil, h)
	s.expectError(resp, b, http.StatusNotFound, "NoSuchKey")
	h.Set("X-Amz-Copy-Source", "/other-bucket/x")
	resp, b = s.put("dest2", nil, h)
	s.expectError(resp, b, http.StatusNotFound, "NoSuchBucket")
	h.Set("X-Amz-Copy-Source", "/"+testBucket+"/a/../b")
	resp, b = s.put("dest2", nil, h)
	s.expectError(resp, b, http.StatusBadRequest, "InvalidArgument")
	// Conditional copy.
	h.Set("X-Amz-Copy-Source", copySource)
	h.Set("x-amz-copy-source-if-match", "\"wrong\"")
	resp, b = s.put("dest3", nil, h)
	s.expectError(resp, b, http.StatusPreconditionFailed, "PreconditionFailed")
}

func TestListObjects(t *testing.T) {
	s := newStack(t)
	t.Run("encoding-type url", func(t *testing.T) {
		s.insertMeta("odd key+plus&amp/ünï.txt")
		resp, b := s.do(req{method: http.MethodGet, query: url.Values{"encoding-type": {"url"}, "prefix": {"odd "}}})
		s.expectStatus(resp, b, http.StatusOK)
		lr := parseList(t, b)
		if lr.EncodingType != "url" {
			t.Fatalf("EncodingType %q\n%s", lr.EncodingType, b)
		}
		if len(lr.Contents) != 1 || lr.Contents[0].Key != "odd%20key%2Bplus%26amp/%C3%BCn%C3%AF.txt" {
			t.Fatalf("encoded key %+v", lr.Contents)
		}
		if lr.Prefix != "odd%20" {
			t.Fatalf("encoded prefix %q", lr.Prefix)
		}
		resp, b = s.do(req{method: http.MethodGet, query: url.Values{"encoding-type": {"bogus"}}})
		s.expectError(resp, b, http.StatusBadRequest, "InvalidArgument")
	})
	t.Run("v2 continuation", func(t *testing.T) {
		for i := 0; i < 1200; i++ {
			s.insertMeta(fmt.Sprintf("v2/%05d", i))
		}
		var keys []string
		token := ""
		for page := 0; ; page++ {
			q := url.Values{"list-type": {"2"}, "prefix": {"v2/"}, "max-keys": {"500"}}
			if token != "" {
				q.Set("continuation-token", token)
			}
			resp, b := s.do(req{method: http.MethodGet, query: q})
			s.expectStatus(resp, b, http.StatusOK)
			lr := parseList(t, b)
			if lr.KeyCount != len(lr.Contents) {
				t.Fatalf("KeyCount %d vs %d", lr.KeyCount, len(lr.Contents))
			}
			for _, c := range lr.Contents {
				keys = append(keys, c.Key)
			}
			if !lr.IsTruncated {
				if lr.NextContinuationToken != "" {
					t.Fatal("token on final page")
				}
				break
			}
			token = lr.NextContinuationToken
			if page > 3 {
				t.Fatal("too many pages")
			}
		}
		if len(keys) != 1200 {
			t.Fatalf("got %d keys", len(keys))
		}
		for i, k := range keys {
			if k != fmt.Sprintf("v2/%05d", i) {
				t.Fatalf("key %d = %q", i, k)
			}
		}
		resp, b := s.do(req{method: http.MethodGet, query: url.Values{"list-type": {"2"}, "continuation-token": {"%%%"}}})
		s.expectError(resp, b, http.StatusBadRequest, "InvalidArgument")
	})
	t.Run("big folder does not hide siblings", func(t *testing.T) {
		for i := 0; i < 1001; i++ {
			s.insertMeta(fmt.Sprintf("big/%05d", i))
		}
		s.insertMeta("big-sibling.txt")
		s.insertMeta("zzz.txt")
		resp, b := s.do(req{method: http.MethodGet, query: url.Values{"delimiter": {"/"}, "prefix": {"big"}}})
		s.expectStatus(resp, b, http.StatusOK)
		lr := parseList(t, b)
		if len(lr.CommonPrefixes) != 1 || lr.CommonPrefixes[0].Prefix != "big/" {
			t.Fatalf("common prefixes %+v", lr.CommonPrefixes)
		}
		if len(lr.Contents) != 1 || lr.Contents[0].Key != "big-sibling.txt" || lr.IsTruncated {
			t.Fatalf("contents %+v truncated=%v", lr.Contents, lr.IsTruncated)
		}
		resp, b = s.do(req{method: http.MethodGet, query: url.Values{"list-type": {"2"}, "delimiter": {"/"}}})
		lr = parseList(t, b)
		var keys []string
		for _, c := range lr.Contents {
			keys = append(keys, c.Key)
		}
		if !contains(keys, "zzz.txt") || !contains(keys, "big-sibling.txt") {
			t.Fatalf("root listing lost siblings: %v (truncated=%v)", keys, lr.IsTruncated)
		}
	})
	t.Run("max-keys validation", func(t *testing.T) {
		resp, b := s.do(req{method: http.MethodGet, query: url.Values{"max-keys": {"-1"}}})
		s.expectError(resp, b, http.StatusBadRequest, "InvalidArgument")
		resp, b = s.do(req{method: http.MethodGet, query: url.Values{"max-keys": {"0"}}})
		s.expectStatus(resp, b, http.StatusOK)
		lr := parseList(t, b)
		if len(lr.Contents) != 0 || !lr.IsTruncated {
			t.Fatalf("max-keys=0: %d contents truncated=%v", len(lr.Contents), lr.IsTruncated)
		}
	})
}

// TestListObjects_DelimiterPaginationTerminates_BUG follows NextContinuationToken
// / NextMarker the way SDK paginators do when a page boundary lands on a
// CommonPrefix. See db.TestListObjectsMeta_MarkerAtCommonPrefix_BUG for the
// root cause (resume marker equal to a common prefix re-emits that prefix).
func TestListObjects_DelimiterPaginationTerminates_BUG(t *testing.T) {
	s := newStack(t)
	for _, folder := range []string{"a", "b", "c"} {
		for i := 0; i < 3; i++ {
			s.insertMeta(fmt.Sprintf("%s/%d", folder, i))
		}
	}
	// V2
	var prefixes []string
	token := ""
	for page := 0; page < 10; page++ {
		q := url.Values{"list-type": {"2"}, "delimiter": {"/"}, "max-keys": {"1"}}
		if token != "" {
			q.Set("continuation-token", token)
		}
		resp, b := s.do(req{method: http.MethodGet, query: q})
		s.expectStatus(resp, b, http.StatusOK)
		lr := parseList(t, b)
		for _, cp := range lr.CommonPrefixes {
			prefixes = append(prefixes, cp.Prefix)
		}
		if !lr.IsTruncated {
			break
		}
		token = lr.NextContinuationToken
	}
	if strings.Join(prefixes, ",") != "a/,b/,c/" {
		t.Fatalf("BUG: ListObjectsV2 with delimiter loops/duplicates on a prefix boundary: got %v want [a/ b/ c/]", prefixes)
	}
	// V1
	prefixes = nil
	marker := ""
	for page := 0; page < 10; page++ {
		q := url.Values{"delimiter": {"/"}, "max-keys": {"1"}}
		if marker != "" {
			q.Set("marker", marker)
		}
		resp, b := s.do(req{method: http.MethodGet, query: q})
		s.expectStatus(resp, b, http.StatusOK)
		lr := parseList(t, b)
		for _, cp := range lr.CommonPrefixes {
			prefixes = append(prefixes, cp.Prefix)
		}
		if !lr.IsTruncated {
			break
		}
		marker = lr.NextMarker
	}
	if strings.Join(prefixes, ",") != "a/,b/,c/" {
		t.Fatalf("BUG: ListObjects v1 with delimiter loops/duplicates on a prefix boundary: got %v want [a/ b/ c/]", prefixes)
	}
}

func contains(list []string, v string) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
}

type deleteResult struct {
	Deleted []struct {
		Key                   string `xml:"Key"`
		VersionId             string `xml:"VersionId"`
		DeleteMarker          bool   `xml:"DeleteMarker"`
		DeleteMarkerVersionId string `xml:"DeleteMarkerVersionId"`
	} `xml:"Deleted"`
	Errors []struct {
		Key  string `xml:"Key"`
		Code string `xml:"Code"`
	} `xml:"Error"`
}

func TestVersioningSemantics(t *testing.T) {
	s := newStack(t)
	s.setVersioning("Enabled")

	resp, b := s.put("v", []byte("one"), nil)
	s.expectStatus(resp, b, http.StatusOK)
	v1 := resp.Header.Get("x-amz-version-id")
	resp, b = s.put("v", []byte("two"), nil)
	s.expectStatus(resp, b, http.StatusOK)
	v2 := resp.Header.Get("x-amz-version-id")
	if v1 == "" || v2 == "" || v1 == v2 {
		t.Fatalf("version ids %q %q", v1, v2)
	}
	resp, b = s.do(req{method: http.MethodGet, key: "v", query: url.Values{"versionId": {v1}}})
	s.expectStatus(resp, b, http.StatusOK)
	if string(b) != "one" {
		t.Fatalf("v1 body %q", b)
	}
	resp, b = s.do(req{method: http.MethodGet, key: "v", query: url.Values{"versionId": {"nope"}}})
	s.expectError(resp, b, http.StatusNotFound, "NoSuchVersion")

	// DeleteObjects creates delete markers; rows remain.
	s.mustPut("w", []byte("w"))
	body := []byte(`<Delete><Object><Key>v</Key></Object><Object><Key>w</Key></Object><Object><Key>../bad</Key></Object></Delete>`)
	resp, b = s.do(req{method: http.MethodPost, query: url.Values{"delete": {""}}, body: body})
	s.expectStatus(resp, b, http.StatusOK)
	var dr deleteResult
	if err := xml.Unmarshal(b, &dr); err != nil {
		t.Fatal(err)
	}
	if len(dr.Deleted) != 2 || len(dr.Errors) != 1 || dr.Errors[0].Code != "InvalidArgument" {
		t.Fatalf("delete result %+v\n%s", dr, b)
	}
	var markerID string
	for _, d := range dr.Deleted {
		if !d.DeleteMarker || d.DeleteMarkerVersionId == "" {
			t.Fatalf("expected delete marker for %s: %+v", d.Key, d)
		}
		if d.Key == "v" {
			markerID = d.DeleteMarkerVersionId
		}
	}
	resp, b = s.get("v", nil)
	s.expectError(resp, b, http.StatusNotFound, "NoSuchKey")
	if resp.Header.Get("x-amz-delete-marker") != "true" {
		t.Fatalf("missing x-amz-delete-marker on 404: %v", resp.Header)
	}
	versions, err := s.db.ListVersionsForKey(s.bucket.ID, "v")
	if err != nil || len(versions) != 3 {
		t.Fatalf("rows for v: %d %v", len(versions), err)
	}
	// GET a marker by version → 405.
	resp, b = s.do(req{method: http.MethodGet, key: "v", query: url.Values{"versionId": {markerID}}})
	s.expectError(resp, b, http.StatusMethodNotAllowed, "MethodNotAllowed")

	// ListObjectVersions shows markers and versions newest-first.
	resp, b = s.do(req{method: http.MethodGet, query: url.Values{"versions": {""}, "prefix": {"v"}}})
	s.expectStatus(resp, b, http.StatusOK)
	var lv struct {
		Versions []struct {
			VersionId string `xml:"VersionId"`
			IsLatest  bool   `xml:"IsLatest"`
		} `xml:"Version"`
		DeleteMarkers []struct {
			VersionId string `xml:"VersionId"`
			IsLatest  bool   `xml:"IsLatest"`
		} `xml:"DeleteMarker"`
	}
	if err := xml.Unmarshal(b, &lv); err != nil {
		t.Fatal(err)
	}
	if len(lv.DeleteMarkers) != 1 || !lv.DeleteMarkers[0].IsLatest || lv.DeleteMarkers[0].VersionId != markerID {
		t.Fatalf("markers %+v", lv.DeleteMarkers)
	}
	if len(lv.Versions) != 2 || lv.Versions[0].VersionId != v2 || lv.Versions[1].VersionId != v1 || lv.Versions[0].IsLatest {
		t.Fatalf("versions %+v", lv.Versions)
	}

	// Deleting the marker by version restores the object.
	resp, b = s.del("v", url.Values{"versionId": {markerID}})
	s.expectStatus(resp, b, http.StatusNoContent)
	if resp.Header.Get("x-amz-delete-marker") != "true" || resp.Header.Get("x-amz-version-id") != markerID {
		t.Fatalf("marker delete headers %v", resp.Header)
	}
	resp, b = s.get("v", nil)
	s.expectStatus(resp, b, http.StatusOK)
	if string(b) != "two" || resp.Header.Get("x-amz-version-id") != v2 {
		t.Fatalf("restored body %q version %q", b, resp.Header.Get("x-amz-version-id"))
	}
	// Deleting a specific version permanently removes it and promotes the rest.
	resp, b = s.del("v", url.Values{"versionId": {v2}})
	s.expectStatus(resp, b, http.StatusNoContent)
	resp, b = s.get("v", nil)
	s.expectStatus(resp, b, http.StatusOK)
	if string(b) != "one" {
		t.Fatalf("after deleting v2 body %q", b)
	}
	if s.store.ObjectExists(testBucket, "v") {
		t.Fatal("unexpected unversioned file")
	}
	// Deleting a non-existent version → 204.
	resp, b = s.del("v", url.Values{"versionId": {"ghost"}})
	s.expectStatus(resp, b, http.StatusNoContent)

	// Bucket delete blocked while noncurrent rows / markers exist.
	resp, b = s.do(req{method: http.MethodDelete})
	s.expectError(resp, b, http.StatusConflict, "BucketNotEmpty")
	resp, b = s.del("v", nil) // adds a marker; still not empty
	s.expectStatus(resp, b, http.StatusNoContent)
	resp, b = s.do(req{method: http.MethodDelete})
	s.expectError(resp, b, http.StatusConflict, "BucketNotEmpty")

	// Suspended: delete creates a "null" marker.
	s.setVersioning("Suspended")
	s.mustPut("susp", []byte("s"))
	resp, b = s.head("susp", nil)
	s.expectStatus(resp, b, http.StatusOK)
	if resp.Header.Get("x-amz-version-id") != "null" {
		t.Fatalf("suspended put version id %q", resp.Header.Get("x-amz-version-id"))
	}
	resp, b = s.del("susp", nil)
	s.expectStatus(resp, b, http.StatusNoContent)
	if resp.Header.Get("x-amz-version-id") != "null" || resp.Header.Get("x-amz-delete-marker") != "true" {
		t.Fatalf("suspended delete headers %v", resp.Header)
	}
	m, _ := s.db.GetObjectMetaByVersion(s.bucket.ID, "susp", "null")
	if m == nil || !m.IsDeleteMarker {
		t.Fatalf("null marker row %+v", m)
	}
	if s.store.ObjectExists(testBucket, "susp") {
		t.Fatal("suspended delete left bytes")
	}
	// GET ?versioning reflects state.
	resp, b = s.do(req{method: http.MethodGet, query: url.Values{"versioning": {""}}})
	s.expectStatus(resp, b, http.StatusOK)
	if !strings.Contains(string(b), "<Status>Suspended</Status>") {
		t.Fatalf("versioning xml %s", b)
	}
	resp, b = s.do(req{method: http.MethodPut, query: url.Values{"versioning": {""}}, body: []byte(`<VersioningConfiguration><Status>Weird</Status></VersioningConfiguration>`)})
	s.expectError(resp, b, http.StatusBadRequest, "InvalidArgument")
}

type initiateResult struct {
	UploadId string `xml:"UploadId"`
}

func (s *stack) createUpload(key string, hdr http.Header) string {
	s.t.Helper()
	resp, b := s.do(req{method: http.MethodPost, key: key, query: url.Values{"uploads": {""}}, header: hdr})
	s.expectStatus(resp, b, http.StatusOK)
	var ir initiateResult
	if err := xml.Unmarshal(b, &ir); err != nil || ir.UploadId == "" {
		s.t.Fatalf("initiate: %v %s", err, b)
	}
	return ir.UploadId
}

func (s *stack) uploadPart(key, uploadID string, n int, body []byte) string {
	s.t.Helper()
	resp, b := s.do(req{method: http.MethodPut, key: key, query: url.Values{"uploadId": {uploadID}, "partNumber": {fmt.Sprint(n)}}, body: body})
	s.expectStatus(resp, b, http.StatusOK)
	if resp.Header.Get("ETag") != md5Quoted(body) {
		s.t.Fatalf("part etag %q", resp.Header.Get("ETag"))
	}
	return resp.Header.Get("ETag")
}

func completeXML(parts [][2]string) []byte {
	var sb strings.Builder
	sb.WriteString("<CompleteMultipartUpload>")
	for _, p := range parts {
		sb.WriteString("<Part><PartNumber>" + p[0] + "</PartNumber><ETag>" + p[1] + "</ETag></Part>")
	}
	sb.WriteString("</CompleteMultipartUpload>")
	return []byte(sb.String())
}

func TestMultipart(t *testing.T) {
	s := newStack(t)
	key := "multi/big.bin"
	hdr := http.Header{}
	hdr.Set("Content-Type", "application/x-big")
	hdr.Set("X-Amz-Meta-Part", "yes")
	uploadID := s.createUpload(key, hdr)

	part1 := bytes.Repeat([]byte("A"), 5*1024*1024) // exactly 5 MiB
	part2 := []byte("tail-part")
	e1 := s.uploadPart(key, uploadID, 1, part1)
	e2 := s.uploadPart(key, uploadID, 2, part2)

	// ListParts with max-parts=1 paginates.
	resp, b := s.do(req{method: http.MethodGet, key: key, query: url.Values{"uploadId": {uploadID}, "max-parts": {"1"}}})
	s.expectStatus(resp, b, http.StatusOK)
	var lp struct {
		IsTruncated          bool `xml:"IsTruncated"`
		NextPartNumberMarker int  `xml:"NextPartNumberMarker"`
		Parts                []struct {
			PartNumber int    `xml:"PartNumber"`
			ETag       string `xml:"ETag"`
			Size       int64  `xml:"Size"`
		} `xml:"Part"`
	}
	if err := xml.Unmarshal(b, &lp); err != nil {
		t.Fatal(err)
	}
	if len(lp.Parts) != 1 || lp.Parts[0].PartNumber != 1 || !lp.IsTruncated || lp.NextPartNumberMarker != 1 || lp.Parts[0].Size != int64(len(part1)) {
		t.Fatalf("ListParts page1 %+v", lp)
	}
	resp, b = s.do(req{method: http.MethodGet, key: key, query: url.Values{"uploadId": {uploadID}, "max-parts": {"1"}, "part-number-marker": {"1"}}})
	lp.Parts = nil
	xml.Unmarshal(b, &lp)
	if len(lp.Parts) != 1 || lp.Parts[0].PartNumber != 2 || lp.IsTruncated {
		t.Fatalf("ListParts page2 %+v", lp)
	}
	// ListMultipartUploads shows it.
	resp, b = s.do(req{method: http.MethodGet, query: url.Values{"uploads": {""}}})
	s.expectStatus(resp, b, http.StatusOK)
	if !strings.Contains(string(b), "<UploadId>"+uploadID+"</UploadId>") {
		t.Fatalf("upload not listed: %s", b)
	}

	// Out-of-order parts.
	resp, b = s.do(req{method: http.MethodPost, key: key, query: url.Values{"uploadId": {uploadID}}, body: completeXML([][2]string{{"2", e2}, {"1", e1}})})
	s.expectError(resp, b, http.StatusBadRequest, "InvalidPartOrder")
	// Wrong etag.
	resp, b = s.do(req{method: http.MethodPost, key: key, query: url.Values{"uploadId": {uploadID}}, body: completeXML([][2]string{{"1", "\"bad\""}, {"2", e2}})})
	s.expectError(resp, b, http.StatusBadRequest, "InvalidPart")
	// Malformed XML.
	resp, b = s.do(req{method: http.MethodPost, key: key, query: url.Values{"uploadId": {uploadID}}, body: []byte("<nope")})
	s.expectError(resp, b, http.StatusBadRequest, "MalformedXML")

	// Complete.
	resp, b = s.do(req{method: http.MethodPost, key: key, query: url.Values{"uploadId": {uploadID}}, body: completeXML([][2]string{{"1", e1}, {"2", e2}})})
	s.expectStatus(resp, b, http.StatusOK)
	d1, d2 := md5.Sum(part1), md5.Sum(part2)
	concat := md5.Sum(append(d1[:], d2[:]...))
	wantETag := fmt.Sprintf("\"%x-2\"", concat[:])
	if !strings.Contains(strings.ReplaceAll(strings.ReplaceAll(string(b), "&#34;", "\""), "&quot;", "\""), "<ETag>"+wantETag+"</ETag>") {
		t.Fatalf("complete etag: want %s in %s", wantETag, b)
	}
	resp, b = s.get(key, nil)
	s.expectStatus(resp, b, http.StatusOK)
	if !bytes.Equal(b, append(append([]byte{}, part1...), part2...)) {
		t.Fatalf("assembled object mismatch: %d bytes", len(b))
	}
	if resp.Header.Get("ETag") != wantETag || resp.Header.Get("Content-Type") != "application/x-big" || resp.Header.Get("X-Amz-Meta-Part") != "yes" {
		t.Fatalf("assembled headers %v", resp.Header)
	}
	// Upload is gone afterwards.
	resp, b = s.do(req{method: http.MethodGet, key: key, query: url.Values{"uploadId": {uploadID}}})
	s.expectError(resp, b, http.StatusNotFound, "NoSuchUpload")
	if up, _ := s.db.GetMultipartUpload(uploadID); up != nil {
		t.Fatal("upload row still present")
	}

	// Small first part → EntityTooSmall.
	u2 := s.createUpload("multi/small", nil)
	p1 := s.uploadPart("multi/small", u2, 1, []byte("tiny"))
	p2 := s.uploadPart("multi/small", u2, 2, []byte("tiny2"))
	resp, b = s.do(req{method: http.MethodPost, key: "multi/small", query: url.Values{"uploadId": {u2}}, body: completeXML([][2]string{{"1", p1}, {"2", p2}})})
	s.expectError(resp, b, http.StatusBadRequest, "EntityTooSmall")
	// A single small part is fine (last part may be small).
	resp, b = s.do(req{method: http.MethodPost, key: "multi/small", query: url.Values{"uploadId": {u2}}, body: completeXML([][2]string{{"1", p1}})})
	s.expectStatus(resp, b, http.StatusOK)
	resp, b = s.get("multi/small", nil)
	s.expectStatus(resp, b, http.StatusOK)
	if string(b) != "tiny" {
		t.Fatalf("single-part body %q", b)
	}

	// Abort.
	u3 := s.createUpload("multi/abort", nil)
	s.uploadPart("multi/abort", u3, 1, []byte("x"))
	resp, b = s.do(req{method: http.MethodDelete, key: "multi/abort", query: url.Values{"uploadId": {u3}}})
	s.expectStatus(resp, b, http.StatusNoContent)
	resp, b = s.do(req{method: http.MethodPut, key: "multi/abort", query: url.Values{"uploadId": {u3}, "partNumber": {"2"}}, body: []byte("x")})
	s.expectError(resp, b, http.StatusNotFound, "NoSuchUpload")
	// Bad part numbers.
	u4 := s.createUpload("multi/parts", nil)
	for _, pn := range []string{"0", "10001", "abc"} {
		resp, b = s.do(req{method: http.MethodPut, key: "multi/parts", query: url.Values{"uploadId": {u4}, "partNumber": {pn}}, body: []byte("x")})
		s.expectError(resp, b, http.StatusBadRequest, "InvalidArgument")
	}
	// partNumber without uploadId is rejected by the router.
	resp, b = s.do(req{method: http.MethodPut, key: "multi/parts", query: url.Values{"partNumber": {"1"}}, body: []byte("x")})
	s.expectError(resp, b, http.StatusBadRequest, "InvalidArgument")
}

func TestUploadPartCopyCrossBucketDenied(t *testing.T) {
	s := newStack(t)
	// A second bucket owned by someone else, with an object in it.
	other, err := s.db.CreateBucket("other-bucket", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.store.CreateBucketDir("other-bucket"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.CreateCredential(other.ID, "o", "AKIAOTHEROTHEROTHER1", "othersecret", "read-write"); err != nil {
		t.Fatal(err)
	}
	resp, b := s.do(req{method: http.MethodPut, bucket: "other-bucket", key: "secret.txt", body: []byte("top secret"), opts: signOpts{accessKey: "AKIAOTHEROTHEROTHER1", secret: "othersecret"}})
	s.expectStatus(resp, b, http.StatusOK)

	uploadID := s.createUpload("stolen", nil)
	h := http.Header{}
	h.Set("X-Amz-Copy-Source", "/other-bucket/secret.txt")
	resp, b = s.do(req{method: http.MethodPut, key: "stolen", query: url.Values{"uploadId": {uploadID}, "partNumber": {"1"}}, header: h})
	s.expectError(resp, b, http.StatusForbidden, "AccessDenied")
	parts, _ := s.db.ListMultipartParts(uploadID)
	if len(parts) != 0 {
		t.Fatal("part was staged from a foreign bucket")
	}
	// Same-bucket UploadPartCopy works, including a range.
	s.mustPut("src.bin", []byte("0123456789"))
	h.Set("X-Amz-Copy-Source", "/"+testBucket+"/src.bin")
	h.Set("X-Amz-Copy-Source-Range", "bytes=2-5")
	resp, b = s.do(req{method: http.MethodPut, key: "stolen", query: url.Values{"uploadId": {uploadID}, "partNumber": {"1"}}, header: h})
	s.expectStatus(resp, b, http.StatusOK)
	parts, _ = s.db.ListMultipartParts(uploadID)
	if len(parts) != 1 || parts[0].Size != 4 || parts[0].ETag != md5Quoted([]byte("2345")) {
		t.Fatalf("copied part %+v", parts)
	}
	// Plain CopyObject from the foreign bucket is denied too.
	h = http.Header{}
	h.Set("X-Amz-Copy-Source", "/other-bucket/secret.txt")
	resp, b = s.put("stolen2", nil, h)
	s.expectError(resp, b, http.StatusForbidden, "AccessDenied")
	// And a foreign credential cannot touch this bucket.
	resp, b = s.do(req{method: http.MethodGet, key: "src.bin", opts: signOpts{accessKey: "AKIAOTHEROTHEROTHER1", secret: "othersecret"}})
	s.expectError(resp, b, http.StatusForbidden, "AccessDenied")
}

func TestLifecycleConfig(t *testing.T) {
	s := newStack(t)
	resp, b := s.do(req{method: http.MethodGet, query: url.Values{"lifecycle": {""}}})
	s.expectError(resp, b, http.StatusNotFound, "NoSuchLifecycleConfiguration")

	body := []byte(`<LifecycleConfiguration xmlns="http://s3.amazonaws.com/doc/2006-03-01/">
  <Rule><ID>expire-logs</ID><Filter><Prefix>logs/</Prefix></Filter><Status>Enabled</Status><Expiration><Days>30</Days></Expiration></Rule>
  <Rule><ID>off</ID><Filter><And><Prefix>tmp/</Prefix></And></Filter><Status>Disabled</Status>
    <NoncurrentVersionExpiration><NoncurrentDays>7</NoncurrentDays></NoncurrentVersionExpiration>
    <AbortIncompleteMultipartUpload><DaysAfterInitiation>2</DaysAfterInitiation></AbortIncompleteMultipartUpload>
  </Rule>
  <Rule><ID>markers</ID><Filter><Prefix></Prefix></Filter><Status>Enabled</Status><Expiration><ExpiredObjectDeleteMarker>true</ExpiredObjectDeleteMarker></Expiration></Rule>
</LifecycleConfiguration>`)
	resp, b = s.do(req{method: http.MethodPut, query: url.Values{"lifecycle": {""}}, body: body})
	s.expectStatus(resp, b, http.StatusOK)

	rules, err := s.db.GetLifecycleRules(testBucket)
	if err != nil || len(rules) != 3 {
		t.Fatalf("rules %v %v", rules, err)
	}
	byName := map[string]int{}
	for i, r := range rules {
		byName[r.Name] = i
	}
	if r := rules[byName["expire-logs"]]; r.Prefix != "logs/" || r.ExpirationDays != 30 || r.Status != "Enabled" {
		t.Fatalf("expire-logs %+v", r)
	}
	if r := rules[byName["off"]]; r.Prefix != "tmp/" || r.Status != "Disabled" || r.NoncurrentDays != 7 || r.AbortMultipartDays != 2 {
		t.Fatalf("off %+v", r)
	}
	if r := rules[byName["markers"]]; !r.ExpireDeleteMarkers || r.ExpirationDays != 0 {
		t.Fatalf("markers %+v", r)
	}

	resp, b = s.do(req{method: http.MethodGet, query: url.Values{"lifecycle": {""}}})
	s.expectStatus(resp, b, http.StatusOK)
	var got struct {
		Rules []struct {
			ID     string `xml:"ID"`
			Status string `xml:"Status"`
			Filter struct {
				Prefix string `xml:"Prefix"`
			} `xml:"Filter"`
			Expiration *struct {
				Days                      int  `xml:"Days"`
				ExpiredObjectDeleteMarker bool `xml:"ExpiredObjectDeleteMarker"`
			} `xml:"Expiration"`
			Noncurrent *struct {
				Days int `xml:"NoncurrentDays"`
			} `xml:"NoncurrentVersionExpiration"`
		} `xml:"Rule"`
	}
	if err := xml.Unmarshal(b, &got); err != nil {
		t.Fatalf("%v\n%s", err, b)
	}
	seen := map[string]bool{}
	for _, r := range got.Rules {
		seen[r.ID] = true
		switch r.ID {
		case "expire-logs":
			if r.Filter.Prefix != "logs/" || r.Status != "Enabled" || r.Expiration == nil || r.Expiration.Days != 30 {
				t.Errorf("GET expire-logs %+v", r)
			}
		case "off":
			if r.Filter.Prefix != "tmp/" || r.Status != "Disabled" || r.Noncurrent == nil || r.Noncurrent.Days != 7 || r.Expiration != nil {
				t.Errorf("GET off %+v", r)
			}
		case "markers":
			if r.Expiration == nil || !r.Expiration.ExpiredObjectDeleteMarker {
				t.Errorf("GET markers %+v", r)
			}
		}
	}
	if len(seen) != 3 {
		t.Fatalf("GET returned %d rules: %s", len(got.Rules), b)
	}
	// x-amz-expiration header on matching objects.
	s.mustPut("logs/today.log", []byte("l"))
	resp, b = s.head("logs/today.log", nil)
	s.expectStatus(resp, b, http.StatusOK)
	if exp := resp.Header.Get("x-amz-expiration"); !strings.Contains(exp, `rule-id="expire-logs"`) {
		t.Fatalf("x-amz-expiration %q", exp)
	}
	// Invalid rules.
	for _, bad := range []string{
		`<LifecycleConfiguration><Rule><Filter><Prefix>x</Prefix></Filter><Status>Enabled</Status><Expiration><Date>2030-01-01T00:00:00Z</Date></Expiration></Rule></LifecycleConfiguration>`,
		`<LifecycleConfiguration><Rule><Filter><Prefix>x</Prefix></Filter><Status>Maybe</Status><Expiration><Days>1</Days></Expiration></Rule></LifecycleConfiguration>`,
		`<LifecycleConfiguration><Rule><Filter><Tag><Key>k</Key><Value>v</Value></Tag></Filter><Status>Enabled</Status><Expiration><Days>1</Days></Expiration></Rule></LifecycleConfiguration>`,
		`<LifecycleConfiguration><Rule><Filter><Prefix>x</Prefix></Filter><Status>Enabled</Status><Expiration><Days>1</Days></Expiration></Rule><Rule><Filter><Prefix>x</Prefix></Filter><Status>Enabled</Status><Expiration><Days>2</Days></Expiration></Rule></LifecycleConfiguration>`,
		`<LifecycleConfiguration></LifecycleConfiguration>`,
	} {
		resp, b = s.do(req{method: http.MethodPut, query: url.Values{"lifecycle": {""}}, body: []byte(bad)})
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("accepted bad lifecycle: %s\n%s", bad, b)
		}
	}
	// Previous config survives a rejected PUT.
	if rules, _ := s.db.GetLifecycleRules(testBucket); len(rules) != 3 {
		t.Fatal("rules changed by rejected PUT")
	}
	resp, b = s.do(req{method: http.MethodDelete, query: url.Values{"lifecycle": {""}}})
	s.expectStatus(resp, b, http.StatusNoContent)
	if rules, _ := s.db.GetLifecycleRules(testBucket); len(rules) != 0 {
		t.Fatal("rules not deleted")
	}
}

func TestSubresourcesAndStubs(t *testing.T) {
	s := newStack(t)
	s.mustPut("obj", []byte("original"))

	resp, b := s.do(req{method: http.MethodGet, query: url.Values{"cors": {""}}})
	s.expectError(resp, b, http.StatusNotFound, "NoSuchCORSConfiguration")

	resp, b = s.do(req{method: http.MethodDelete, query: url.Values{"website": {""}}})
	s.expectError(resp, b, http.StatusNotImplemented, "NotImplemented")
	resp, b = s.do(req{method: http.MethodHead})
	s.expectStatus(resp, b, http.StatusOK)
	if resp.Header.Get("x-amz-bucket-region") != testRegion {
		t.Fatalf("HEAD bucket region %q", resp.Header.Get("x-amz-bucket-region"))
	}

	resp, b = s.do(req{method: http.MethodPut, key: "obj", query: url.Values{"retention": {""}}, body: []byte("<Retention/>")})
	s.expectError(resp, b, http.StatusNotImplemented, "NotImplemented")
	resp, b = s.get("obj", nil)
	s.expectStatus(resp, b, http.StatusOK)
	if string(b) != "original" {
		t.Fatalf("object changed by ?retention PUT: %q", b)
	}
	resp, b = s.do(req{method: http.MethodDelete, key: "obj", query: url.Values{"legal-hold": {""}}})
	s.expectError(resp, b, http.StatusNotImplemented, "NotImplemented")
	resp, b = s.get("obj", nil)
	s.expectStatus(resp, b, http.StatusOK)

	// Unknown bucket sub-resource on PUT/DELETE never hits Create/DeleteBucket.
	resp, b = s.do(req{method: http.MethodDelete, query: url.Values{"logging": {""}}})
	s.expectError(resp, b, http.StatusNotImplemented, "NotImplemented")
	resp, b = s.do(req{method: http.MethodHead})
	s.expectStatus(resp, b, http.StatusOK)

	// Stubs that exist.
	resp, b = s.do(req{method: http.MethodGet, query: url.Values{"location": {""}}})
	s.expectStatus(resp, b, http.StatusOK)
	if !strings.Contains(string(b), ">"+testRegion+"<") {
		t.Fatalf("location %s", b)
	}
	resp, b = s.do(req{method: http.MethodGet, query: url.Values{"acl": {""}}})
	s.expectStatus(resp, b, http.StatusOK)
	resp, b = s.do(req{method: http.MethodGet, key: "obj", query: url.Values{"tagging": {""}}})
	s.expectStatus(resp, b, http.StatusOK)
	resp, b = s.do(req{method: http.MethodGet, query: url.Values{"policy": {""}}})
	s.expectError(resp, b, http.StatusNotFound, "NoSuchBucketPolicy")
	resp, b = s.do(req{method: http.MethodGet, query: url.Values{"encryption": {""}}})
	s.expectError(resp, b, http.StatusNotFound, "ServerSideEncryptionConfigurationNotFoundError")
	resp, b = s.do(req{method: http.MethodPut, query: url.Values{"cors": {""}}, body: []byte("<CORSConfiguration/>")})
	s.expectStatus(resp, b, http.StatusOK)

	// CreateBucket of a new name → 403; own bucket → 409 BucketAlreadyOwnedByYou.
	resp, b = s.do(req{method: http.MethodPut, bucket: "brand-new-bucket"})
	s.expectError(resp, b, http.StatusForbidden, "AccessDenied")
	if bkt, _ := s.db.GetBucket("brand-new-bucket"); bkt != nil {
		t.Fatal("bucket was created")
	}
	resp, b = s.do(req{method: http.MethodPut})
	s.expectError(resp, b, http.StatusConflict, "BucketAlreadyOwnedByYou")
	resp, b = s.do(req{method: http.MethodPut, bucket: "Bad_Name"})
	s.expectError(resp, b, http.StatusBadRequest, "InvalidBucketName")

	// ListBuckets shows only the credential's bucket.
	u, _ := url.Parse(s.srv.URL)
	r, _ := http.NewRequest(http.MethodGet, u.String()+"/", nil)
	signHTTP(r, "/", signOpts{accessKey: testAccessKey, secret: testSecret, region: testRegion, now: time.Now(), payloadHash: sha256Hex(nil)})
	lresp, err := s.client.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	lb := new(bytes.Buffer)
	lb.ReadFrom(lresp.Body)
	lresp.Body.Close()
	if lresp.StatusCode != http.StatusOK || !strings.Contains(lb.String(), "<Name>"+testBucket+"</Name>") {
		t.Fatalf("ListBuckets %d %s", lresp.StatusCode, lb.String())
	}

	// Unknown bucket → 404.
	resp, b = s.do(req{method: http.MethodGet, bucket: "nope-bucket", key: "x"})
	s.expectError(resp, b, http.StatusNotFound, "NoSuchBucket")

	// Bucket delete works once empty.
	resp, b = s.del("obj", nil)
	s.expectStatus(resp, b, http.StatusNoContent)
	resp, b = s.do(req{method: http.MethodDelete})
	s.expectStatus(resp, b, http.StatusNoContent)
	if bkt, _ := s.db.GetBucket(testBucket); bkt != nil {
		t.Fatal("bucket row still present")
	}
}

func TestAuthentication(t *testing.T) {
	s := newStack(t)
	body := []byte("public data")
	s.mustPut("pub.txt", body)

	t.Run("presigned GET", func(t *testing.T) {
		u := s.presignURL(http.MethodGet, "pub.txt", url.Values{"response-content-disposition": {"attachment; filename=x.txt"}}, time.Now(), 300)
		resp, err := s.client.Get(u)
		if err != nil {
			t.Fatal(err)
		}
		b := new(bytes.Buffer)
		b.ReadFrom(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK || b.String() != "public data" {
			t.Fatalf("presigned: %d %s", resp.StatusCode, b.String())
		}
		if resp.Header.Get("Content-Disposition") != "attachment; filename=x.txt" {
			t.Fatalf("response-content-disposition not honored: %v", resp.Header)
		}
		// Tampered signature.
		resp, _ = s.client.Get(strings.Replace(u, "X-Amz-Signature=", "X-Amz-Signature=0", 1))
		resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("tampered presigned: %d", resp.StatusCode)
		}
		// Expired.
		resp, _ = s.client.Get(s.presignURL(http.MethodGet, "pub.txt", nil, time.Now().Add(-time.Hour), 60))
		eb := new(bytes.Buffer)
		eb.ReadFrom(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden || errCode(eb.Bytes()) != "AccessDenied" {
			t.Fatalf("expired presigned: %d %s", resp.StatusCode, eb.String())
		}
		// Presigned PUT.
		pu := s.presignURL(http.MethodPut, "presigned-put.txt", nil, time.Now(), 300)
		pr, _ := http.NewRequest(http.MethodPut, pu, strings.NewReader("via presign"))
		presp, err := s.client.Do(pr)
		if err != nil {
			t.Fatal(err)
		}
		presp.Body.Close()
		if presp.StatusCode != http.StatusOK {
			t.Fatalf("presigned PUT %d", presp.StatusCode)
		}
		resp, b2 := s.get("presigned-put.txt", nil)
		s.expectStatus(resp, b2, http.StatusOK)
	})

	t.Run("anonymous access", func(t *testing.T) {
		resp, b := s.do(req{method: http.MethodGet, key: "pub.txt", anon: true})
		s.expectError(resp, b, http.StatusForbidden, "AccessDenied")
		if err := s.db.SetBucketPublicRead(testBucket, true); err != nil {
			t.Fatal(err)
		}
		resp, b = s.do(req{method: http.MethodGet, key: "pub.txt", anon: true})
		s.expectStatus(resp, b, http.StatusOK)
		if string(b) != "public data" {
			t.Fatalf("anon body %q", b)
		}
		resp, b = s.do(req{method: http.MethodHead, key: "pub.txt", anon: true})
		s.expectStatus(resp, b, http.StatusOK)
		// response-* overrides are ignored for anonymous readers.
		resp, b = s.do(req{method: http.MethodGet, key: "pub.txt", anon: true, query: url.Values{"response-content-type": {"evil/type"}}})
		s.expectStatus(resp, b, http.StatusOK)
		if resp.Header.Get("Content-Type") == "evil/type" {
			t.Fatal("anonymous response-content-type honored")
		}
		// Listing and writes stay denied.
		resp, b = s.do(req{method: http.MethodGet, anon: true})
		s.expectError(resp, b, http.StatusForbidden, "AccessDenied")
		resp, b = s.do(req{method: http.MethodPut, key: "anon-write", body: []byte("x"), anon: true})
		s.expectError(resp, b, http.StatusForbidden, "AccessDenied")
		resp, b = s.do(req{method: http.MethodDelete, key: "pub.txt", anon: true})
		s.expectError(resp, b, http.StatusForbidden, "AccessDenied")
		resp, b = s.do(req{method: http.MethodGet, key: "pub.txt", anon: true, bucket: "nope-bucket"})
		s.expectError(resp, b, http.StatusNotFound, "NoSuchBucket")
	})

	t.Run("region mismatch", func(t *testing.T) {
		resp, b := s.do(req{method: http.MethodGet, key: "pub.txt", opts: signOpts{region: "us-west-2"}})
		s.expectError(resp, b, http.StatusBadRequest, "AuthorizationHeaderMalformed")
		var e s3ErrorXML
		xml.Unmarshal(b, &e)
		if e.Region != testRegion {
			t.Fatalf("<Region> = %q want %q\n%s", e.Region, testRegion, b)
		}
	})
	t.Run("clock skew", func(t *testing.T) {
		resp, b := s.do(req{method: http.MethodGet, key: "pub.txt", opts: signOpts{now: time.Now().Add(-30 * time.Minute)}})
		s.expectError(resp, b, http.StatusForbidden, "RequestTimeTooSkewed")
	})
	t.Run("bad credentials", func(t *testing.T) {
		resp, b := s.do(req{method: http.MethodGet, key: "pub.txt", opts: signOpts{secret: "wrong"}})
		s.expectError(resp, b, http.StatusForbidden, "SignatureDoesNotMatch")
		resp, b = s.do(req{method: http.MethodGet, key: "pub.txt", opts: signOpts{accessKey: "AKIAUNKNOWNUNKNOWN00"}})
		s.expectError(resp, b, http.StatusForbidden, "InvalidAccessKeyId")
		h := http.Header{}
		h.Set("Authorization", "AWS4-HMAC-SHA256 garbage")
		resp, b = s.do(req{method: http.MethodGet, key: "pub.txt", header: h, anon: true})
		s.expectError(resp, b, http.StatusBadRequest, "AuthorizationHeaderMalformed")
	})
	t.Run("read-only credential", func(t *testing.T) {
		if _, err := s.db.CreateCredential(s.bucket.ID, "ro", "AKIAREADONLYREADONLY", "rosecret", "read-only"); err != nil {
			t.Fatal(err)
		}
		ro := signOpts{accessKey: "AKIAREADONLYREADONLY", secret: "rosecret"}
		resp, b := s.do(req{method: http.MethodGet, key: "pub.txt", opts: ro})
		s.expectStatus(resp, b, http.StatusOK)
		resp, b = s.do(req{method: http.MethodPut, key: "ro-write", body: []byte("x"), opts: ro})
		s.expectError(resp, b, http.StatusForbidden, "AccessDenied")
		resp, b = s.do(req{method: http.MethodDelete, key: "pub.txt", opts: ro})
		s.expectError(resp, b, http.StatusForbidden, "AccessDenied")
		resp, b = s.do(req{method: http.MethodPost, key: "ro-mp", query: url.Values{"uploads": {""}}, opts: ro})
		s.expectError(resp, b, http.StatusForbidden, "AccessDenied")
	})
}

func TestQuota(t *testing.T) {
	s := newStack(t)
	if err := s.db.SetBucketQuota(testBucket, 100); err != nil {
		t.Fatal(err)
	}
	s.refreshBucket()
	first := bytes.Repeat([]byte("a"), 80)
	s.mustPut("q.bin", first)

	// Over quota with a declared length → rejected up front.
	resp, b := s.put("q2.bin", bytes.Repeat([]byte("b"), 30), nil)
	s.expectError(resp, b, http.StatusForbidden, "QuotaExceeded")
	if s.store.ObjectExists(testBucket, "q2.bin") {
		t.Fatal("over-quota object written")
	}

	// Overwrite of the same key within quota succeeds (old size credited).
	second := bytes.Repeat([]byte("c"), 95)
	resp, b = s.put("q.bin", second, nil)
	s.expectStatus(resp, b, http.StatusOK)
	resp, b = s.get("q.bin", nil)
	s.expectStatus(resp, b, http.StatusOK)
	if !bytes.Equal(b, second) {
		t.Fatal("overwrite body mismatch")
	}

	// Overwrite exceeding quota → 403 and previous bytes intact. Use a chunked
	// upload without a decoded length so the check happens after streaming.
	over := bytes.Repeat([]byte("d"), 120)
	resp, b = s.putChunked("q.bin", over, 50, http.Header{}, nil)
	s.expectError(resp, b, http.StatusForbidden, "QuotaExceeded")
	resp, b = s.get("q.bin", nil)
	s.expectStatus(resp, b, http.StatusOK)
	if !bytes.Equal(b, second) {
		t.Fatalf("previous object damaged after quota rejection: %d bytes", len(b))
	}
	if usage, _ := s.db.GetBucketUsage(s.bucket.ID); usage != 95 {
		t.Fatalf("usage %d", usage)
	}
	// Multipart completion is quota-checked too.
	uploadID := s.createUpload("mp.bin", nil)
	e1 := s.uploadPart("mp.bin", uploadID, 1, bytes.Repeat([]byte("e"), 10))
	resp, b = s.do(req{method: http.MethodPost, key: "mp.bin", query: url.Values{"uploadId": {uploadID}}, body: completeXML([][2]string{{"1", e1}})})
	s.expectError(resp, b, http.StatusForbidden, "QuotaExceeded")
	if s.store.ObjectExists(testBucket, "mp.bin") {
		t.Fatal("assembled object written despite quota")
	}
	// Delete frees quota.
	resp, b = s.del("q.bin", nil)
	s.expectStatus(resp, b, http.StatusNoContent)
	resp, b = s.do(req{method: http.MethodPost, key: "mp.bin", query: url.Values{"uploadId": {uploadID}}, body: completeXML([][2]string{{"1", e1}})})
	s.expectStatus(resp, b, http.StatusOK)
}

func TestNotificationConfig(t *testing.T) {
	s := newStack(t)
	body := []byte(`<NotificationConfiguration><WebhookConfiguration><Name>h</Name><Url>https://hooks.example.com/x</Url><Secret>s3cr3t</Secret></WebhookConfiguration></NotificationConfiguration>`)
	resp, b := s.do(req{method: http.MethodPut, query: url.Values{"notification": {""}}, body: body})
	s.expectStatus(resp, b, http.StatusOK)
	resp, b = s.do(req{method: http.MethodGet, query: url.Values{"notification": {""}}})
	s.expectStatus(resp, b, http.StatusOK)
	if strings.Contains(string(b), "s3cr3t") || !strings.Contains(string(b), "<Secret>***</Secret>") {
		t.Fatalf("secret leaked or not masked: %s", b)
	}
	// Re-PUT with the masked secret keeps the stored one.
	body = []byte(`<NotificationConfiguration><WebhookConfiguration><Name>h</Name><Url>https://hooks.example.com/x</Url><Secret>***</Secret></WebhookConfiguration></NotificationConfiguration>`)
	resp, b = s.do(req{method: http.MethodPut, query: url.Values{"notification": {""}}, body: body})
	s.expectStatus(resp, b, http.StatusOK)
	hooks, _ := s.db.ListWebhooks(testBucket)
	if len(hooks) != 1 || hooks[0].Secret != "s3cr3t" {
		t.Fatalf("hooks %+v", hooks)
	}
	// Internal targets rejected.
	body = []byte(`<NotificationConfiguration><WebhookConfiguration><Url>http://127.0.0.1:9/x</Url></WebhookConfiguration></NotificationConfiguration>`)
	resp, b = s.do(req{method: http.MethodPut, query: url.Values{"notification": {""}}, body: body})
	s.expectError(resp, b, http.StatusBadRequest, "InvalidArgument")
	resp, b = s.do(req{method: http.MethodDelete, query: url.Values{"notification": {""}}})
	s.expectStatus(resp, b, http.StatusNoContent)
	if hooks, _ := s.db.ListWebhooks(testBucket); len(hooks) != 0 {
		t.Fatal("hooks not deleted")
	}
}

func TestRouterMisc(t *testing.T) {
	s := newStack(t)
	// OPTIONS preflight without configured origins: 204, no CORS headers.
	r, _ := http.NewRequest(http.MethodOptions, s.srv.URL+"/"+testBucket+"/k", nil)
	r.Header.Set("Origin", "https://app.example")
	resp, err := s.client.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent || resp.Header.Get("Access-Control-Allow-Origin") != "" {
		t.Fatalf("OPTIONS %d %v", resp.StatusCode, resp.Header)
	}
	s.cfg.Server.CORSOrigins = []string{"https://app.example"}
	resp, _ = s.client.Do(r)
	resp.Body.Close()
	if resp.Header.Get("Access-Control-Allow-Origin") != "https://app.example" {
		t.Fatalf("CORS origin not echoed: %v", resp.Header)
	}
	// Every response carries request ids and nosniff.
	resp2, b := s.get("missing", nil)
	s.expectError(resp2, b, http.StatusNotFound, "NoSuchKey")
	if resp2.Header.Get("x-amz-request-id") == "" || resp2.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("headers %v", resp2.Header)
	}
	// Method not allowed on root.
	r, _ = http.NewRequest(http.MethodPost, s.srv.URL+"/", nil)
	resp, _ = s.client.Do(r)
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("POST / = %d", resp.StatusCode)
	}
	// Virtual-hosted style resolution.
	s.cfg.Server.VirtualHostDomains = []string{"s3.example.test"}
	s.mustPut("vh.txt", []byte("vhost"))
	u, _ := url.Parse(s.srv.URL)
	r, _ = http.NewRequest(http.MethodGet, u.String()+"/vh.txt", nil)
	r.Host = testBucket + ".s3.example.test"
	signHTTP(r, "/vh.txt", signOpts{accessKey: testAccessKey, secret: testSecret, region: testRegion, now: time.Now(), payloadHash: sha256Hex(nil)})
	resp, err = s.client.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	vb := new(bytes.Buffer)
	vb.ReadFrom(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || vb.String() != "vhost" {
		t.Fatalf("virtual-host GET %d %s", resp.StatusCode, vb.String())
	}
}
