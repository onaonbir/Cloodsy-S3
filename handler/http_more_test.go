package handler_test

import (
	"bytes"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/base64"
	"encoding/xml"
	"fmt"
	"hash/crc32"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"
)

func sha256B64(b []byte) string {
	s := sha256.Sum256(b)
	return base64.StdEncoding.EncodeToString(s[:])
}

func sha1Sum(b []byte) []byte {
	s := sha1.Sum(b)
	return s[:]
}

func crc32cB64(b []byte) string {
	c := crc32.Checksum(b, crc32.MakeTable(crc32.Castagnoli))
	return base64.StdEncoding.EncodeToString([]byte{byte(c >> 24), byte(c >> 16), byte(c >> 8), byte(c)})
}

func TestChecksumSHA256Header(t *testing.T) {
	s := newStack(t)
	body := []byte("sha256 checked payload")
	hdr := http.Header{}
	hdr.Set("x-amz-checksum-sha256", sha256B64(body))
	resp, b := s.put("sha-ok", body, hdr)
	s.expectStatus(resp, b, http.StatusOK)
	if resp.Header.Get("x-amz-checksum-sha256") != sha256B64(body) {
		t.Fatalf("checksum not echoed: %v", resp.Header)
	}
	resp, b = s.get("sha-ok", nil)
	s.expectStatus(resp, b, http.StatusOK)
	if !bytes.Equal(b, body) {
		t.Fatal("body mismatch")
	}

	hdr.Set("x-amz-checksum-sha256", sha256B64([]byte("something else")))
	resp, b = s.put("sha-bad", body, hdr)
	s.expectError(resp, b, http.StatusBadRequest, "BadDigest")
	if s.store.ObjectExists(testBucket, "sha-bad") {
		t.Fatal("bytes left on disk after BadDigest")
	}
	resp, b = s.get("sha-bad", nil)
	s.expectError(resp, b, http.StatusNotFound, "NoSuchKey")

	// Malformed base64 → InvalidDigest.
	hdr.Set("x-amz-checksum-sha256", "!!not-base64!!")
	resp, b = s.put("sha-bad2", body, hdr)
	s.expectError(resp, b, http.StatusBadRequest, "InvalidDigest")

	// sha1 and crc32c variants.
	hdr = http.Header{}
	hdr.Set("x-amz-checksum-sha1", base64.StdEncoding.EncodeToString(sha1Sum(body)))
	resp, b = s.put("sha1-ok", body, hdr)
	s.expectStatus(resp, b, http.StatusOK)
	hdr = http.Header{}
	hdr.Set("x-amz-checksum-crc32c", crc32cB64(body))
	resp, b = s.put("crc32c-ok", body, hdr)
	s.expectStatus(resp, b, http.StatusOK)
	if resp.Header.Get("x-amz-checksum-crc32c") != crc32cB64(body) {
		t.Fatalf("crc32c not echoed: %v", resp.Header)
	}
}

// encodeUnsignedChunkedBody produces a STREAMING-UNSIGNED-PAYLOAD-TRAILER body.
func encodeUnsignedChunkedBody(payload []byte, chunkSize int, trailers map[string]string) []byte {
	var b bytes.Buffer
	for i := 0; i < len(payload); i += chunkSize {
		end := i + chunkSize
		if end > len(payload) {
			end = len(payload)
		}
		fmt.Fprintf(&b, "%x\r\n", end-i)
		b.Write(payload[i:end])
		b.WriteString("\r\n")
	}
	b.WriteString("0\r\n")
	for k, v := range trailers {
		b.WriteString(k + ":" + v + "\r\n")
	}
	b.WriteString("\r\n")
	return b.Bytes()
}

func (s *stack) putUnsignedTrailer(key string, payload []byte, trailerName, trailerValue string) (*http.Response, []byte) {
	s.t.Helper()
	enc := encodeUnsignedChunkedBody(payload, 7, map[string]string{trailerName: trailerValue})
	hdr := http.Header{}
	hdr.Set("Content-Encoding", "aws-chunked")
	hdr.Set("X-Amz-Decoded-Content-Length", strconv.Itoa(len(payload)))
	hdr.Set("x-amz-trailer", trailerName)
	return s.do(req{method: http.MethodPut, key: key, body: enc, header: hdr, opts: signOpts{payloadHash: "STREAMING-UNSIGNED-PAYLOAD-TRAILER"}})
}

func TestUnsignedPayloadTrailer(t *testing.T) {
	s := newStack(t)
	payload := []byte("trailer-checked payload, spanning several chunks")

	resp, b := s.putUnsignedTrailer("tr-ok", payload, "x-amz-checksum-crc32", crc32B64(payload))
	s.expectStatus(resp, b, http.StatusOK)
	if resp.Header.Get("ETag") != md5Quoted(payload) {
		t.Fatalf("ETag %q", resp.Header.Get("ETag"))
	}
	if resp.Header.Get("x-amz-checksum-crc32") != crc32B64(payload) {
		t.Fatalf("trailer checksum not echoed: %v", resp.Header)
	}
	resp, b = s.get("tr-ok", nil)
	s.expectStatus(resp, b, http.StatusOK)
	if !bytes.Equal(b, payload) {
		t.Fatalf("decoded body %q", b)
	}
	// Stored Content-Encoding must not carry the transport encoding.
	if strings.Contains(resp.Header.Get("Content-Encoding"), "aws-chunked") {
		t.Fatalf("aws-chunked leaked into stored Content-Encoding: %v", resp.Header)
	}

	// Wrong trailer checksum → BadDigest, nothing persisted.
	resp, b = s.putUnsignedTrailer("tr-bad", payload, "x-amz-checksum-crc32", crc32B64([]byte("other")))
	s.expectError(resp, b, http.StatusBadRequest, "BadDigest")
	if s.store.ObjectExists(testBucket, "tr-bad") {
		t.Fatal("bytes left on disk after trailer BadDigest")
	}
	// sha256 trailer works too.
	resp, b = s.putUnsignedTrailer("tr-sha", payload, "x-amz-checksum-sha256", sha256B64(payload))
	s.expectStatus(resp, b, http.StatusOK)
	if resp.Header.Get("x-amz-checksum-sha256") != sha256B64(payload) {
		t.Fatalf("sha256 trailer not echoed: %v", resp.Header)
	}
	// Decoded length mismatch → IncompleteBody.
	enc := encodeUnsignedChunkedBody(payload, 7, nil)
	hdr := http.Header{}
	hdr.Set("Content-Encoding", "aws-chunked")
	hdr.Set("X-Amz-Decoded-Content-Length", strconv.Itoa(len(payload)+1))
	resp, b = s.do(req{method: http.MethodPut, key: "tr-len", body: enc, header: hdr, opts: signOpts{payloadHash: "STREAMING-UNSIGNED-PAYLOAD-TRAILER"}})
	s.expectError(resp, b, http.StatusBadRequest, "IncompleteBody")
	if s.store.ObjectExists(testBucket, "tr-len") {
		t.Fatal("bytes left on disk after IncompleteBody")
	}
}

type listVersionsXML struct {
	XMLName             xml.Name `xml:"ListVersionsResult"`
	IsTruncated         bool     `xml:"IsTruncated"`
	NextKeyMarker       string   `xml:"NextKeyMarker"`
	NextVersionIdMarker string   `xml:"NextVersionIdMarker"`
	KeyMarker           string   `xml:"KeyMarker"`
	VersionIdMarker     string   `xml:"VersionIdMarker"`
	Versions            []struct {
		Key       string `xml:"Key"`
		VersionId string `xml:"VersionId"`
		IsLatest  bool   `xml:"IsLatest"`
		Size      int64  `xml:"Size"`
	} `xml:"Version"`
	DeleteMarkers []struct {
		Key       string `xml:"Key"`
		VersionId string `xml:"VersionId"`
		IsLatest  bool   `xml:"IsLatest"`
	} `xml:"DeleteMarker"`
	CommonPrefixes []struct {
		Prefix string `xml:"Prefix"`
	} `xml:"CommonPrefixes"`
}

func (s *stack) listVersions(q url.Values) listVersionsXML {
	s.t.Helper()
	q.Set("versions", "")
	resp, b := s.do(req{method: http.MethodGet, query: q})
	s.expectStatus(resp, b, http.StatusOK)
	var lv listVersionsXML
	if err := xml.Unmarshal(b, &lv); err != nil {
		s.t.Fatalf("%v\n%s", err, b)
	}
	return lv
}

func TestPreVersioningObjects(t *testing.T) {
	s := newStack(t)
	s.mustPut("pv", []byte("before"))
	s.mustPut("only-null", []byte("null-only"))
	s.setVersioning("Enabled")
	resp, b := s.put("pv", []byte("after"), nil)
	s.expectStatus(resp, b, http.StatusOK)
	v2 := resp.Header.Get("x-amz-version-id")
	if v2 == "" || v2 == "null" {
		t.Fatalf("version id %q", v2)
	}

	// Listing: newest first, pre-versioning row reported as "null".
	lv := s.listVersions(url.Values{"prefix": {"pv"}})
	if len(lv.Versions) != 2 {
		t.Fatalf("versions %+v", lv.Versions)
	}
	if lv.Versions[0].VersionId != v2 || !lv.Versions[0].IsLatest || lv.Versions[0].Size != 5 {
		t.Fatalf("newest %+v", lv.Versions[0])
	}
	if lv.Versions[1].VersionId != "null" || lv.Versions[1].IsLatest || lv.Versions[1].Size != 6 {
		t.Fatalf("pre-versioning row %+v", lv.Versions[1])
	}

	// GET/HEAD ?versionId=null serves the pre-versioning bytes.
	resp, b = s.do(req{method: http.MethodGet, key: "pv", query: url.Values{"versionId": {"null"}}})
	s.expectStatus(resp, b, http.StatusOK)
	if string(b) != "before" || resp.Header.Get("x-amz-version-id") != "null" {
		t.Fatalf("versionId=null: %q %v", b, resp.Header)
	}
	resp, b = s.do(req{method: http.MethodHead, key: "pv", query: url.Values{"versionId": {"null"}}})
	s.expectStatus(resp, b, http.StatusOK)
	if resp.Header.Get("Content-Length") != "6" {
		t.Fatalf("HEAD null length %q", resp.Header.Get("Content-Length"))
	}
	// An object that only has a pre-versioning row reports version "null" on HEAD.
	resp, b = s.head("only-null", nil)
	s.expectStatus(resp, b, http.StatusOK)
	if resp.Header.Get("x-amz-version-id") != "null" {
		t.Fatalf("only-null version %q", resp.Header.Get("x-amz-version-id"))
	}
	resp, b = s.do(req{method: http.MethodGet, key: "only-null", query: url.Values{"versionId": {"null"}}})
	s.expectStatus(resp, b, http.StatusOK)

	// CopyObject from the null source version.
	h := http.Header{}
	h.Set("X-Amz-Copy-Source", "/"+testBucket+"/pv?versionId=null")
	resp, b = s.put("copy-of-null", nil, h)
	s.expectStatus(resp, b, http.StatusOK)
	if resp.Header.Get("x-amz-copy-source-version-id") != "null" {
		t.Fatalf("copy source version %v", resp.Header)
	}
	resp, b = s.get("copy-of-null", nil)
	s.expectStatus(resp, b, http.StatusOK)
	if string(b) != "before" {
		t.Fatalf("copied body %q", b)
	}
	// CopyObject from the newest explicit version.
	h.Set("X-Amz-Copy-Source", "/"+testBucket+"/pv?versionId="+v2)
	resp, b = s.put("copy-of-v2", nil, h)
	s.expectStatus(resp, b, http.StatusOK)
	if resp.Header.Get("x-amz-copy-source-version-id") != v2 {
		t.Fatalf("copy source version %v", resp.Header)
	}
	resp, b = s.get("copy-of-v2", nil)
	s.expectStatus(resp, b, http.StatusOK)
	if string(b) != "after" {
		t.Fatalf("copied body %q", b)
	}
	// Unknown source version → NoSuchVersion.
	h.Set("X-Amz-Copy-Source", "/"+testBucket+"/pv?versionId=ghost")
	resp, b = s.put("copy-of-ghost", nil, h)
	s.expectError(resp, b, http.StatusNotFound, "NoSuchVersion")

	// Deleting the null version by id removes it and keeps v2 current.
	resp, b = s.del("pv", url.Values{"versionId": {"null"}})
	s.expectStatus(resp, b, http.StatusNoContent)
	resp, b = s.do(req{method: http.MethodGet, key: "pv", query: url.Values{"versionId": {"null"}}})
	s.expectError(resp, b, http.StatusNotFound, "NoSuchVersion")
	resp, b = s.get("pv", nil)
	s.expectStatus(resp, b, http.StatusOK)
	if string(b) != "after" {
		t.Fatalf("after null delete %q", b)
	}
}

func TestListObjectVersionsPagination(t *testing.T) {
	s := newStack(t)
	s.setVersioning("Enabled")
	type entry struct{ key, version string }
	var want []entry
	for _, k := range []string{"p/a", "p/b", "p/c"} {
		var ids []string
		for i := 0; i < 2; i++ {
			resp, b := s.put(k, []byte(fmt.Sprintf("%s-%d", k, i)), nil)
			s.expectStatus(resp, b, http.StatusOK)
			ids = append(ids, resp.Header.Get("x-amz-version-id"))
		}
		// Newest first within a key.
		want = append(want, entry{k, ids[1]}, entry{k, ids[0]})
	}
	// Add a delete marker on p/b so the stream mixes markers and versions.
	resp, b := s.del("p/b", nil)
	s.expectStatus(resp, b, http.StatusNoContent)
	marker := resp.Header.Get("x-amz-version-id")
	want = append(want[:2], append([]entry{{"p/b", marker}}, want[2:]...)...)

	var got []entry
	km, vm := "", ""
	for page := 0; page < 20; page++ {
		q := url.Values{"prefix": {"p/"}, "max-keys": {"2"}}
		if km != "" {
			q.Set("key-marker", km)
			q.Set("version-id-marker", vm)
		}
		lv := s.listVersions(q)
		if lv.KeyMarker != km || lv.VersionIdMarker != vm {
			t.Fatalf("markers echoed %q/%q want %q/%q", lv.KeyMarker, lv.VersionIdMarker, km, vm)
		}
		n := len(lv.Versions) + len(lv.DeleteMarkers)
		if n > 2 {
			t.Fatalf("page %d has %d entries", page, n)
		}
		// Re-merge versions and markers in stream order: the XML groups them by
		// type, so reconstruct by key + version id.
		for _, v := range lv.Versions {
			got = append(got, entry{v.Key, v.VersionId})
		}
		for _, m := range lv.DeleteMarkers {
			got = append(got, entry{m.Key, m.VersionId})
		}
		if !lv.IsTruncated {
			if lv.NextKeyMarker != "" || lv.NextVersionIdMarker != "" {
				t.Fatalf("next markers on the last page: %q/%q", lv.NextKeyMarker, lv.NextVersionIdMarker)
			}
			break
		}
		if lv.NextKeyMarker == "" || lv.NextVersionIdMarker == "" {
			t.Fatalf("truncated page without next markers: %+v", lv)
		}
		km, vm = lv.NextKeyMarker, lv.NextVersionIdMarker
	}
	if len(got) != len(want) {
		t.Fatalf("got %d entries want %d\n%v\n%v", len(got), len(want), got, want)
	}
	// Order check is per page (types are grouped within a page), so compare
	// as an ordered set of pages: every wanted entry must appear exactly once
	// and keys must be non-decreasing.
	seen := map[entry]int{}
	for _, g := range got {
		seen[g]++
	}
	for _, w := range want {
		if seen[w] != 1 {
			t.Fatalf("entry %v seen %d times", w, seen[w])
		}
	}
	for i := 1; i < len(got); i++ {
		if got[i].key < got[i-1].key {
			t.Fatalf("keys out of order: %v", got)
		}
	}
	// The full single-page listing is newest-first per key.
	lv := s.listVersions(url.Values{"prefix": {"p/"}})
	if len(lv.Versions) != 6 || len(lv.DeleteMarkers) != 1 || lv.IsTruncated {
		t.Fatalf("full listing %+v", lv)
	}
	for i := 0; i < 6; i += 2 {
		if lv.Versions[i].Key != lv.Versions[i+1].Key {
			t.Fatalf("versions not grouped by key: %+v", lv.Versions)
		}
		// The first of each pair is the newer write (latest unless a marker sits on top).
		if lv.Versions[i].Key != "p/b" && !lv.Versions[i].IsLatest {
			t.Fatalf("first version of %s not latest", lv.Versions[i].Key)
		}
		if lv.Versions[i+1].IsLatest {
			t.Fatalf("older version of %s marked latest", lv.Versions[i].Key)
		}
	}
	if !lv.DeleteMarkers[0].IsLatest || lv.DeleteMarkers[0].VersionId != marker {
		t.Fatalf("marker %+v", lv.DeleteMarkers[0])
	}
	// Delimiter roll-up over versions.
	lv = s.listVersions(url.Values{"delimiter": {"/"}})
	if len(lv.CommonPrefixes) != 1 || lv.CommonPrefixes[0].Prefix != "p/" || len(lv.Versions) != 0 {
		t.Fatalf("delimiter listing %+v", lv)
	}
	// Invalid parameters.
	q := url.Values{"versions": {""}, "max-keys": {"x"}}
	resp, b = s.do(req{method: http.MethodGet, query: q})
	s.expectError(resp, b, http.StatusBadRequest, "InvalidArgument")
	q = url.Values{"versions": {""}, "encoding-type": {"weird"}}
	resp, b = s.do(req{method: http.MethodGet, query: q})
	s.expectError(resp, b, http.StatusBadRequest, "InvalidArgument")
}

func TestListMultipartUploadsPrefixUnderscore(t *testing.T) {
	s := newStack(t)
	ids := map[string]string{}
	for _, k := range []string{"a_b/1", "axb/1", "a_c/2", "b_/x"} {
		ids[k] = s.createUpload(k, nil)
	}
	resp, b := s.do(req{method: http.MethodGet, query: url.Values{"uploads": {""}, "prefix": {"a_"}}})
	s.expectStatus(resp, b, http.StatusOK)
	var lm struct {
		Prefix  string `xml:"Prefix"`
		Uploads []struct {
			Key      string `xml:"Key"`
			UploadId string `xml:"UploadId"`
		} `xml:"Upload"`
	}
	if err := xml.Unmarshal(b, &lm); err != nil {
		t.Fatalf("%v\n%s", err, b)
	}
	if lm.Prefix != "a_" || len(lm.Uploads) != 2 {
		t.Fatalf("uploads %+v\n%s", lm.Uploads, b)
	}
	if lm.Uploads[0].Key != "a_b/1" || lm.Uploads[0].UploadId != ids["a_b/1"] || lm.Uploads[1].Key != "a_c/2" {
		t.Fatalf("uploads %+v", lm.Uploads)
	}
	// "%" in the prefix is literal too.
	s.createUpload("pct%1", nil)
	s.createUpload("pctX1", nil)
	resp, b = s.do(req{method: http.MethodGet, query: url.Values{"uploads": {""}, "prefix": {"pct%"}}})
	s.expectStatus(resp, b, http.StatusOK)
	lm.Uploads = nil
	xml.Unmarshal(b, &lm)
	if len(lm.Uploads) != 1 || lm.Uploads[0].Key != "pct%1" {
		t.Fatalf("percent prefix uploads %+v", lm.Uploads)
	}
}

func TestObjectKeyPlusAndPercent(t *testing.T) {
	s := newStack(t)
	keys := []string{"plus+sign.txt", "pct%20literal.txt", "both+%2Bmixed%.txt", "q?mark&amp=1.txt"}
	for i, k := range keys {
		body := []byte(fmt.Sprintf("body-%d", i))
		s.mustPut(k, body)
		resp, b := s.get(k, nil)
		s.expectStatus(resp, b, http.StatusOK)
		if !bytes.Equal(b, body) {
			t.Fatalf("%q: body %q", k, b)
		}
		resp, b = s.head(k, nil)
		s.expectStatus(resp, b, http.StatusOK)
	}
	// Keys are stored literally: "pct%20literal.txt" is not "pct literal.txt".
	resp, b := s.get("pct literal.txt", nil)
	s.expectError(resp, b, http.StatusNotFound, "NoSuchKey")
	resp, b = s.get("plus sign.txt", nil)
	s.expectError(resp, b, http.StatusNotFound, "NoSuchKey")

	resp, b = s.do(req{method: http.MethodGet})
	s.expectStatus(resp, b, http.StatusOK)
	lr := parseList(t, b)
	listed := map[string]bool{}
	for _, c := range lr.Contents {
		listed[c.Key] = true
	}
	for _, k := range keys {
		if !listed[k] {
			t.Fatalf("%q missing from listing: %v", k, listed)
		}
	}
	// url encoding-type round trip.
	resp, b = s.do(req{method: http.MethodGet, query: url.Values{"encoding-type": {"url"}, "prefix": {"both"}}})
	s.expectStatus(resp, b, http.StatusOK)
	lr = parseList(t, b)
	if len(lr.Contents) != 1 || lr.Contents[0].Key != "both%2B%252Bmixed%25.txt" {
		t.Fatalf("encoded key %+v", lr.Contents)
	}
	for _, k := range keys {
		resp, b = s.del(k, nil)
		s.expectStatus(resp, b, http.StatusNoContent)
	}
}

func TestVirtualHostListing(t *testing.T) {
	s := newStack(t)
	s.cfg.Server.VirtualHostDomains = []string{"s3.local"}
	s.mustPut("vh/one", []byte("1"))
	s.mustPut("vh/two", []byte("2"))

	u, _ := url.Parse(s.srv.URL)
	r, _ := http.NewRequest(http.MethodGet, u.String()+"/?prefix=vh/", nil)
	r.Host = testBucket + ".s3.local"
	signHTTP(r, "/", signOpts{accessKey: testAccessKey, secret: testSecret, region: testRegion, now: time.Now(), payloadHash: sha256Hex(nil)})
	resp, err := s.client.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	buf := new(bytes.Buffer)
	buf.ReadFrom(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("vhost list %d %s", resp.StatusCode, buf.String())
	}
	lr := parseList(t, buf.Bytes())
	if len(lr.Contents) != 2 || lr.Contents[0].Key != "vh/one" || lr.Contents[1].Key != "vh/two" {
		t.Fatalf("vhost listing %+v\n%s", lr.Contents, buf.String())
	}
	if !strings.Contains(buf.String(), "<Name>"+testBucket+"</Name>") {
		t.Fatalf("bucket name missing: %s", buf.String())
	}
	// Host with a port and mixed case still resolves.
	r, _ = http.NewRequest(http.MethodGet, u.String()+"/", nil)
	r.Host = strings.ToUpper(testBucket) + ".S3.LOCAL:9000"
	signHTTP(r, "/", signOpts{accessKey: testAccessKey, secret: testSecret, region: testRegion, now: time.Now(), payloadHash: sha256Hex(nil)})
	resp, err = s.client.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("vhost with port: %d", resp.StatusCode)
	}
	// A host under the domain but with a dotted label is not a bucket → path style ListBuckets.
	r, _ = http.NewRequest(http.MethodGet, u.String()+"/", nil)
	r.Host = "a.b.s3.local"
	signHTTP(r, "/", signOpts{accessKey: testAccessKey, secret: testSecret, region: testRegion, now: time.Now(), payloadHash: sha256Hex(nil)})
	resp, err = s.client.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	buf.Reset()
	buf.ReadFrom(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(buf.String(), "ListAllMyBucketsResult") {
		t.Fatalf("dotted host: %d %s", resp.StatusCode, buf.String())
	}
}

func TestListObjectsEdgeCases(t *testing.T) {
	s := newStack(t)
	for _, k := range []string{"a/1", "a/2", "b/1", "c/1", "d"} {
		s.insertMeta(k)
	}
	// V2 max-keys=0 → KeyCount 0, IsTruncated true, no token needed to be valid.
	resp, b := s.do(req{method: http.MethodGet, query: url.Values{"list-type": {"2"}, "max-keys": {"0"}}})
	s.expectStatus(resp, b, http.StatusOK)
	lr := parseList(t, b)
	if lr.KeyCount != 0 || len(lr.Contents) != 0 || !lr.IsTruncated {
		t.Fatalf("v2 max-keys=0: %+v", lr)
	}
	// V1 marker equal to a common prefix continues after it.
	resp, b = s.do(req{method: http.MethodGet, query: url.Values{"delimiter": {"/"}, "marker": {"a/"}}})
	s.expectStatus(resp, b, http.StatusOK)
	lr = parseList(t, b)
	var prefixes []string
	for _, cp := range lr.CommonPrefixes {
		prefixes = append(prefixes, cp.Prefix)
	}
	if strings.Join(prefixes, ",") != "b/,c/" || len(lr.Contents) != 1 || lr.Contents[0].Key != "d" || lr.IsTruncated {
		t.Fatalf("marker=a/: prefixes %v contents %+v truncated=%v", prefixes, lr.Contents, lr.IsTruncated)
	}
	// A marker inside a folder (S3 semantics): remaining keys of that folder
	// still roll up, so "a/" appears again (a/2 > a/1).
	resp, b = s.do(req{method: http.MethodGet, query: url.Values{"delimiter": {"/"}, "marker": {"a/1"}}})
	lr = parseList(t, b)
	prefixes = nil
	for _, cp := range lr.CommonPrefixes {
		prefixes = append(prefixes, cp.Prefix)
	}
	if strings.Join(prefixes, ",") != "a/,b/,c/" {
		t.Fatalf("marker=a/1: prefixes %v", prefixes)
	}
	// max-keys=1 with a delimiter: first page is the "a/" prefix, NextMarker set.
	resp, b = s.do(req{method: http.MethodGet, query: url.Values{"delimiter": {"/"}, "max-keys": {"1"}}})
	lr = parseList(t, b)
	if len(lr.CommonPrefixes) != 1 || lr.CommonPrefixes[0].Prefix != "a/" || !lr.IsTruncated || lr.NextMarker != "a/" {
		t.Fatalf("first page %+v", lr)
	}
	// Prefix + delimiter inside a folder.
	resp, b = s.do(req{method: http.MethodGet, query: url.Values{"delimiter": {"/"}, "prefix": {"a/"}}})
	lr = parseList(t, b)
	if len(lr.Contents) != 2 || len(lr.CommonPrefixes) != 0 {
		t.Fatalf("prefix a/: %+v", lr)
	}
	// V2 start-after.
	resp, b = s.do(req{method: http.MethodGet, query: url.Values{"list-type": {"2"}, "start-after": {"b/1"}}})
	lr = parseList(t, b)
	if len(lr.Contents) != 2 || lr.Contents[0].Key != "c/1" || lr.Contents[1].Key != "d" {
		t.Fatalf("start-after %+v", lr.Contents)
	}
}

func TestMetadataTooLarge(t *testing.T) {
	s := newStack(t)
	// Single value over 256 bytes.
	hdr := http.Header{}
	hdr.Set("X-Amz-Meta-Big", strings.Repeat("v", 257))
	resp, b := s.put("meta-big", []byte("x"), hdr)
	s.expectError(resp, b, http.StatusBadRequest, "MetadataTooLarge")
	if s.store.ObjectExists(testBucket, "meta-big") {
		t.Fatal("object written despite MetadataTooLarge")
	}
	// Key over 128 bytes.
	hdr = http.Header{}
	hdr.Set("X-Amz-Meta-"+strings.Repeat("k", 129), "v")
	resp, b = s.put("meta-key", []byte("x"), hdr)
	s.expectError(resp, b, http.StatusBadRequest, "MetadataTooLarge")
	// Many small headers whose total exceeds 2 KB.
	hdr = http.Header{}
	for i := 0; i < 12; i++ {
		hdr.Set(fmt.Sprintf("X-Amz-Meta-K%02d", i), strings.Repeat("v", 200))
	}
	resp, b = s.put("meta-total", []byte("x"), hdr)
	s.expectError(resp, b, http.StatusBadRequest, "MetadataTooLarge")
	// Exactly at the value limit is fine.
	hdr = http.Header{}
	hdr.Set("X-Amz-Meta-Ok", strings.Repeat("v", 256))
	resp, b = s.put("meta-ok", []byte("x"), hdr)
	s.expectStatus(resp, b, http.StatusOK)
	resp, b = s.head("meta-ok", nil)
	s.expectStatus(resp, b, http.StatusOK)
	if len(resp.Header.Get("X-Amz-Meta-Ok")) != 256 {
		t.Fatalf("metadata not echoed: %v", resp.Header)
	}
	// CopyObject with REPLACE is subject to the same limit.
	h := http.Header{}
	h.Set("X-Amz-Copy-Source", "/"+testBucket+"/meta-ok")
	h.Set("x-amz-metadata-directive", "REPLACE")
	h.Set("X-Amz-Meta-Big", strings.Repeat("v", 300))
	resp, b = s.put("meta-copy", nil, h)
	s.expectError(resp, b, http.StatusBadRequest, "MetadataTooLarge")
	// CreateMultipartUpload too.
	resp, b = s.do(req{method: http.MethodPost, key: "meta-mp", query: url.Values{"uploads": {""}}, header: hdr2(map[string]string{"X-Amz-Meta-Big": strings.Repeat("v", 300)})})
	s.expectError(resp, b, http.StatusBadRequest, "MetadataTooLarge")
}

func hdr2(m map[string]string) http.Header {
	h := http.Header{}
	for k, v := range m {
		h.Set(k, v)
	}
	return h
}

func TestCORSPreflight(t *testing.T) {
	s := newStack(t)
	s.cfg.Server.CORSOrigins = []string{"https://app.example"}

	r, _ := http.NewRequest(http.MethodOptions, s.srv.URL+"/"+testBucket+"/k", nil)
	r.Header.Set("Origin", "https://app.example")
	r.Header.Set("Access-Control-Request-Method", "PUT")
	r.Header.Set("Access-Control-Request-Headers", "x-amz-meta-custom, content-type, x-amz-checksum-sha256")
	resp, err := s.client.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("preflight %d", resp.StatusCode)
	}
	h := resp.Header
	if h.Get("Access-Control-Allow-Origin") != "https://app.example" {
		t.Fatalf("allow-origin %q", h.Get("Access-Control-Allow-Origin"))
	}
	if h.Get("Access-Control-Allow-Headers") != "x-amz-meta-custom, content-type, x-amz-checksum-sha256" {
		t.Fatalf("requested headers not echoed: %q", h.Get("Access-Control-Allow-Headers"))
	}
	if !contains(h.Values("Vary"), "Origin") {
		t.Fatalf("Vary %v", h.Values("Vary"))
	}
	if !strings.Contains(h.Get("Access-Control-Allow-Methods"), "PUT") || h.Get("Access-Control-Max-Age") == "" {
		t.Fatalf("methods/max-age %v", h)
	}
	if !strings.Contains(h.Get("Access-Control-Expose-Headers"), "ETag") {
		t.Fatalf("expose headers %q", h.Get("Access-Control-Expose-Headers"))
	}
	// Without Access-Control-Request-Headers the default allow list is used.
	r.Header.Del("Access-Control-Request-Headers")
	resp, _ = s.client.Do(r)
	resp.Body.Close()
	if !strings.Contains(resp.Header.Get("Access-Control-Allow-Headers"), "Authorization") {
		t.Fatalf("default allow headers %q", resp.Header.Get("Access-Control-Allow-Headers"))
	}
	// Origin is matched case-insensitively but echoed as sent.
	r.Header.Set("Origin", "https://APP.example")
	resp, _ = s.client.Do(r)
	resp.Body.Close()
	if resp.Header.Get("Access-Control-Allow-Origin") != "https://APP.example" {
		t.Fatalf("case-insensitive origin: %v", resp.Header)
	}
	// Unlisted origin → no CORS headers but still 204.
	r.Header.Set("Origin", "https://evil.example")
	resp, _ = s.client.Do(r)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent || resp.Header.Get("Access-Control-Allow-Origin") != "" || contains(resp.Header.Values("Vary"), "Origin") {
		t.Fatalf("unlisted origin: %d %v", resp.StatusCode, resp.Header)
	}
	// Wildcard config echoes any origin.
	s.cfg.Server.CORSOrigins = []string{"*"}
	resp, _ = s.client.Do(r)
	resp.Body.Close()
	if resp.Header.Get("Access-Control-Allow-Origin") != "https://evil.example" {
		t.Fatalf("wildcard: %v", resp.Header)
	}
	// CORS headers also ride on real (signed) responses.
	hdr := http.Header{}
	hdr.Set("Origin", "https://app.example")
	s.cfg.Server.CORSOrigins = []string{"https://app.example"}
	resp2, b := s.do(req{method: http.MethodHead, header: hdr})
	s.expectStatus(resp2, b, http.StatusOK)
	if resp2.Header.Get("Access-Control-Allow-Origin") != "https://app.example" {
		t.Fatalf("CORS on HEAD bucket: %v", resp2.Header)
	}
}

func TestResponseOverridesAndHeadRange(t *testing.T) {
	s := newStack(t)
	body := []byte("0123456789")
	s.mustPut("ovr", body)
	// Signed request: overrides applied.
	resp, b := s.do(req{method: http.MethodGet, key: "ovr", query: url.Values{
		"response-content-disposition": {"attachment; filename=\"dl.bin\""},
		"response-content-type":        {"application/x-custom"},
		"response-cache-control":       {"no-store"},
		"response-content-language":    {"tr"},
		"response-expires":             {"Thu, 01 Jan 2030 00:00:00 GMT"},
		"response-content-encoding":    {"identity"},
	}})
	s.expectStatus(resp, b, http.StatusOK)
	if resp.Header.Get("Content-Disposition") != "attachment; filename=\"dl.bin\"" || resp.Header.Get("Content-Type") != "application/x-custom" ||
		resp.Header.Get("Cache-Control") != "no-store" || resp.Header.Get("Content-Language") != "tr" || resp.Header.Get("Expires") != "Thu, 01 Jan 2030 00:00:00 GMT" {
		t.Fatalf("overrides not applied: %v", resp.Header)
	}
	// CRLF in an override is ignored.
	resp, b = s.do(req{method: http.MethodGet, key: "ovr", query: url.Values{"response-content-disposition": {"x\r\nInjected: 1"}}})
	s.expectStatus(resp, b, http.StatusOK)
	if resp.Header.Get("Content-Disposition") != "" || resp.Header.Get("Injected") != "" {
		t.Fatalf("header injection: %v", resp.Header)
	}
	// Anonymous public read ignores overrides.
	if err := s.db.SetBucketPublicRead(testBucket, true); err != nil {
		t.Fatal(err)
	}
	resp, b = s.do(req{method: http.MethodGet, key: "ovr", anon: true, query: url.Values{"response-content-disposition": {"attachment; filename=x"}}})
	s.expectStatus(resp, b, http.StatusOK)
	if resp.Header.Get("Content-Disposition") != "" {
		t.Fatalf("anonymous override honored: %v", resp.Header)
	}
	// HEAD + Range → 206 with Content-Range and no body; If-Range mismatch → 200.
	h := http.Header{}
	h.Set("Range", "bytes=3-6")
	resp, b = s.head("ovr", h)
	s.expectStatus(resp, b, http.StatusPartialContent)
	if resp.Header.Get("Content-Range") != "bytes 3-6/10" || resp.Header.Get("Content-Length") != "4" || len(b) != 0 {
		t.Fatalf("HEAD range %v body=%d", resp.Header, len(b))
	}
	h.Set("If-Range", "\"nope\"")
	resp, b = s.get("ovr", h)
	s.expectStatus(resp, b, http.StatusOK)
	if !bytes.Equal(b, body) || resp.Header.Get("Content-Range") != "" {
		t.Fatalf("If-Range mismatch: %q %v", b, resp.Header)
	}
	h.Set("If-Range", md5Quoted(body))
	resp, b = s.get("ovr", h)
	s.expectStatus(resp, b, http.StatusPartialContent)
	if string(b) != "3456" {
		t.Fatalf("If-Range match body %q", b)
	}
	// If-Range with a date after Last-Modified matches; before → full.
	h.Set("If-Range", time.Now().Add(time.Hour).UTC().Format(http.TimeFormat))
	resp, b = s.get("ovr", h)
	s.expectStatus(resp, b, http.StatusPartialContent)
	h.Set("If-Range", time.Now().Add(-time.Hour).UTC().Format(http.TimeFormat))
	resp, b = s.get("ovr", h)
	s.expectStatus(resp, b, http.StatusOK)
	// HEAD with an unsatisfiable range falls back to 200 (HEAD path).
	h = http.Header{}
	h.Set("Range", "bytes=50-60")
	resp, b = s.head("ovr", h)
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusRequestedRangeNotSatisfiable {
		t.Fatalf("HEAD unsatisfiable range: %d", resp.StatusCode)
	}
}

func TestDeleteNonexistentVersionAndKey(t *testing.T) {
	s := newStack(t)
	// Unversioned bucket: unknown key and unknown version both 204.
	resp, b := s.del("ghost", nil)
	s.expectStatus(resp, b, http.StatusNoContent)
	resp, b = s.del("ghost", url.Values{"versionId": {"v-none"}})
	s.expectStatus(resp, b, http.StatusNoContent)
	if resp.Header.Get("x-amz-version-id") != "" || resp.Header.Get("x-amz-delete-marker") != "" {
		t.Fatalf("headers on no-op delete: %v", resp.Header)
	}
	s.setVersioning("Enabled")
	s.mustPut("real", []byte("x"))
	resp, b = s.del("real", url.Values{"versionId": {"v-none"}})
	s.expectStatus(resp, b, http.StatusNoContent)
	resp, b = s.get("real", nil)
	s.expectStatus(resp, b, http.StatusOK)
	if string(b) != "x" {
		t.Fatal("object affected by deleting a ghost version")
	}
}
