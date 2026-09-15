package handler_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/onaonbir/Cloodsy-S3/auth"
	"github.com/onaonbir/Cloodsy-S3/config"
	"github.com/onaonbir/Cloodsy-S3/db"
	"github.com/onaonbir/Cloodsy-S3/handler"
	"github.com/onaonbir/Cloodsy-S3/server"
	"github.com/onaonbir/Cloodsy-S3/storage"
)

const (
	testRegion    = "eu-central-1"
	testAccessKey = "AKIATESTTESTTESTTEST"
	testSecret    = "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"
	testBucket    = "test-bucket"
)

// stack is a fully wired S3 server backed by a temp dir.
type stack struct {
	t      *testing.T
	srv    *httptest.Server
	db     *db.DB
	store  *storage.FileSystem
	cfg    *config.Config
	h      *handler.Handler
	bucket *db.Bucket
	cred   *db.BucketCredential
	client *http.Client
}

func newStack(t *testing.T) *stack {
	t.Helper()
	dir := t.TempDir()
	database, err := db.Open(filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	store, err := storage.NewFileSystem(filepath.Join(dir, "data"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.Server.Region = testRegion
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := handler.New(database, store, cfg, logger)
	router := server.NewRouter(h, logger)
	srv := httptest.NewServer(router)

	bucket, err := database.CreateBucket(testBucket, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CreateBucketDir(testBucket); err != nil {
		t.Fatal(err)
	}
	cred, err := database.CreateCredential(bucket.ID, "n", testAccessKey, testSecret, "read-write")
	if err != nil {
		t.Fatal(err)
	}
	s := &stack{t: t, srv: srv, db: database, store: store, cfg: cfg, h: h, bucket: bucket, cred: cred, client: srv.Client()}
	t.Cleanup(func() {
		srv.Close()
		database.Close()
	})
	return s
}

// refreshBucket reloads the bucket row (e.g. after versioning/quota changes).
func (s *stack) refreshBucket() *db.Bucket {
	b, err := s.db.GetBucket(testBucket)
	if err != nil || b == nil {
		s.t.Fatalf("refresh bucket: %v", err)
	}
	s.bucket = b
	return b
}

// --- SigV4 client-side helpers ---------------------------------------------

func sha256Hex(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func uriEncode(s string, encodeSlash bool) string {
	var buf strings.Builder
	for _, b := range []byte(s) {
		switch {
		case (b >= 'A' && b <= 'Z') || (b >= 'a' && b <= 'z') || (b >= '0' && b <= '9') || b == '-' || b == '.' || b == '_' || b == '~':
			buf.WriteByte(b)
		case b == '/' && !encodeSlash:
			buf.WriteByte(b)
		default:
			fmt.Fprintf(&buf, "%%%02X", b)
		}
	}
	return buf.String()
}

func canonicalQuery(q url.Values) string {
	keys := make([]string, 0, len(q))
	for k := range q {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var pairs []string
	for _, k := range keys {
		vals := append([]string(nil), q[k]...)
		sort.Strings(vals)
		for _, v := range vals {
			pairs = append(pairs, uriEncode(k, true)+"="+uriEncode(v, true))
		}
	}
	return strings.Join(pairs, "&")
}

func canonicalRequest(method, host, path string, hdr http.Header, signed []string, payloadHash string, q url.Values) string {
	sorted := make([]string, len(signed))
	for i, h := range signed {
		sorted[i] = strings.ToLower(h)
	}
	sort.Strings(sorted)
	var hdrs strings.Builder
	for _, h := range sorted {
		var v string
		if h == "host" {
			v = host
		} else {
			vals := hdr.Values(h)
			for i := range vals {
				vals[i] = strings.TrimSpace(vals[i])
			}
			v = strings.Join(vals, ",")
		}
		hdrs.WriteString(h + ":" + strings.Join(strings.Fields(v), " ") + "\n")
	}
	if path == "" {
		path = "/"
	}
	segs := strings.Split(path, "/")
	for i, s := range segs {
		segs[i] = uriEncode(s, false)
	}
	return method + "\n" + strings.Join(segs, "/") + "\n" + canonicalQuery(q) + "\n" + hdrs.String() + "\n" + strings.Join(sorted, ";") + "\n" + payloadHash
}

// signOpts tweaks how a request is signed.
type signOpts struct {
	accessKey   string
	secret      string
	region      string
	now         time.Time
	payloadHash string   // "" → sha256 of body
	extraSigned []string // additional headers to sign
}

// req describes an S3 request; helper methods fill the signing headers.
type req struct {
	method string
	key    string // object key ("" for bucket ops)
	bucket string // "" → testBucket
	query  url.Values
	header http.Header
	body   []byte
	opts   signOpts
	anon   bool
}

// do signs and sends the request, returning the response with its body read.
func (s *stack) do(r req) (*http.Response, []byte) {
	s.t.Helper()
	bucket := r.bucket
	if bucket == "" {
		bucket = testBucket
	}
	path := "/" + bucket
	if r.key != "" {
		path += "/" + r.key
	}
	u, _ := url.Parse(s.srv.URL)
	u.Path = path
	if r.query != nil {
		u.RawQuery = r.query.Encode()
	}
	httpReq, err := http.NewRequest(r.method, u.String(), bytes.NewReader(r.body))
	if err != nil {
		s.t.Fatal(err)
	}
	httpReq.ContentLength = int64(len(r.body))
	for k, vs := range r.header {
		for _, v := range vs {
			httpReq.Header.Add(k, v)
		}
	}
	if !r.anon {
		o := r.opts
		if o.accessKey == "" {
			o.accessKey = testAccessKey
		}
		if o.secret == "" {
			o.secret = testSecret
		}
		if o.region == "" {
			o.region = testRegion
		}
		if o.now.IsZero() {
			o.now = time.Now()
		}
		if o.payloadHash == "" {
			o.payloadHash = sha256Hex(r.body)
		}
		signHTTP(httpReq, path, o)
	}
	resp, err := s.client.Do(httpReq)
	if err != nil {
		s.t.Fatalf("%s %s: %v", r.method, path, err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, body
}

// signHTTP signs an already-built request in place. path must be the decoded
// path the server will see (r.URL.Path), which is what the canonical URI is
// derived from.
func signHTTP(httpReq *http.Request, path string, o signOpts) string {
	amzDate := o.now.UTC().Format("20060102T150405Z")
	dateStamp := amzDate[:8]
	httpReq.Header.Set("X-Amz-Date", amzDate)
	httpReq.Header.Set("X-Amz-Content-Sha256", o.payloadHash)
	signed := append([]string{"host", "x-amz-content-sha256", "x-amz-date"}, o.extraSigned...)
	for i := range signed {
		signed[i] = strings.ToLower(signed[i])
	}
	sort.Strings(signed)
	cr := canonicalRequest(httpReq.Method, httpReq.Host, path, httpReq.Header, signed, o.payloadHash, httpReq.URL.Query())
	scope := dateStamp + "/" + o.region + "/s3/aws4_request"
	sts := "AWS4-HMAC-SHA256\n" + amzDate + "\n" + scope + "\n" + sha256Hex([]byte(cr))
	key := auth.DeriveSigningKey(o.secret, dateStamp, o.region, "s3")
	sig := hex.EncodeToString(auth.HMACSHA256(key, []byte(sts)))
	httpReq.Header.Set("Authorization", fmt.Sprintf("AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s", o.accessKey, scope, strings.Join(signed, ";"), sig))
	return sig
}

// presignURL builds a presigned URL for an object.
func (s *stack) presignURL(method, key string, extra url.Values, now time.Time, expires int) string {
	u, _ := url.Parse(s.srv.URL)
	path := "/" + testBucket + "/" + key
	amzDate := now.UTC().Format("20060102T150405Z")
	dateStamp := amzDate[:8]
	scope := dateStamp + "/" + testRegion + "/s3/aws4_request"
	q := url.Values{}
	for k, v := range extra {
		q[k] = v
	}
	q.Set("X-Amz-Algorithm", "AWS4-HMAC-SHA256")
	q.Set("X-Amz-Credential", testAccessKey+"/"+scope)
	q.Set("X-Amz-Date", amzDate)
	q.Set("X-Amz-Expires", strconv.Itoa(expires))
	q.Set("X-Amz-SignedHeaders", "host")
	cr := canonicalRequest(method, u.Host, path, http.Header{}, []string{"host"}, "UNSIGNED-PAYLOAD", q)
	sts := "AWS4-HMAC-SHA256\n" + amzDate + "\n" + scope + "\n" + sha256Hex([]byte(cr))
	key2 := auth.DeriveSigningKey(testSecret, dateStamp, testRegion, "s3")
	q.Set("X-Amz-Signature", hex.EncodeToString(auth.HMACSHA256(key2, []byte(sts))))
	u.Path = path
	u.RawQuery = q.Encode()
	return u.String()
}

// --- aws-chunked client encoder --------------------------------------------

type chunkSignerT struct {
	key     []byte
	amzDate string
	scope   string
	prev    string
}

func (c *chunkSignerT) sign(chunk []byte) string {
	sum := sha256.Sum256(chunk)
	empty := sha256.Sum256(nil)
	sts := "AWS4-HMAC-SHA256-PAYLOAD\n" + c.amzDate + "\n" + c.scope + "\n" + c.prev + "\n" + hex.EncodeToString(empty[:]) + "\n" + hex.EncodeToString(sum[:])
	sig := hex.EncodeToString(auth.HMACSHA256(c.key, []byte(sts)))
	c.prev = sig
	return sig
}

// putChunked sends a STREAMING-AWS4-HMAC-SHA256-PAYLOAD PUT of payload split
// into chunkSize pieces. tamper, if non-nil, mutates the encoded body before
// sending (after signing).
func (s *stack) putChunked(key string, payload []byte, chunkSize int, extraHeaders http.Header, tamper func([]byte)) (*http.Response, []byte) {
	s.t.Helper()
	path := "/" + testBucket + "/" + key
	u, _ := url.Parse(s.srv.URL)
	u.Path = path
	now := time.Now()
	amzDate := now.UTC().Format("20060102T150405Z")
	dateStamp := amzDate[:8]
	scope := dateStamp + "/" + testRegion + "/s3/aws4_request"

	// Pre-compute the encoded length: each chunk header + data + CRLFs.
	var chunks [][]byte
	for i := 0; i < len(payload); i += chunkSize {
		end := i + chunkSize
		if end > len(payload) {
			end = len(payload)
		}
		chunks = append(chunks, payload[i:end])
	}
	const sigLen = 64
	encodedLen := 0
	for _, c := range chunks {
		encodedLen += len(fmt.Sprintf("%x", len(c))) + len(";chunk-signature=") + sigLen + 2 + len(c) + 2
	}
	encodedLen += 1 + len(";chunk-signature=") + sigLen + 2 + 2

	httpReq, _ := http.NewRequest(http.MethodPut, u.String(), nil)
	for k, vs := range extraHeaders {
		for _, v := range vs {
			httpReq.Header.Add(k, v)
		}
	}
	httpReq.Header.Set("Content-Encoding", "aws-chunked")
	httpReq.Header.Set("X-Amz-Decoded-Content-Length", strconv.Itoa(len(payload)))
	httpReq.Header.Set("Content-Length", strconv.Itoa(encodedLen))
	seed := signHTTP(httpReq, path, signOpts{
		accessKey: testAccessKey, secret: testSecret, region: testRegion, now: now,
		payloadHash: "STREAMING-AWS4-HMAC-SHA256-PAYLOAD",
		extraSigned: []string{"content-encoding", "x-amz-decoded-content-length", "content-length"},
	})
	cs := &chunkSignerT{key: auth.DeriveSigningKey(testSecret, dateStamp, testRegion, "s3"), amzDate: amzDate, scope: scope, prev: seed}
	var body bytes.Buffer
	for _, c := range chunks {
		fmt.Fprintf(&body, "%x;chunk-signature=%s\r\n", len(c), cs.sign(c))
		body.Write(c)
		body.WriteString("\r\n")
	}
	fmt.Fprintf(&body, "0;chunk-signature=%s\r\n\r\n", cs.sign(nil))
	enc := body.Bytes()
	if len(enc) != encodedLen {
		s.t.Fatalf("encoded length mismatch: %d vs %d", len(enc), encodedLen)
	}
	if tamper != nil {
		tamper(enc)
	}
	httpReq.Body = io.NopCloser(bytes.NewReader(enc))
	httpReq.ContentLength = int64(len(enc))
	resp, err := s.client.Do(httpReq)
	if err != nil {
		s.t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, b
}

// --- assertions --------------------------------------------------------------

type s3ErrorXML struct {
	XMLName xml.Name `xml:"Error"`
	Code    string   `xml:"Code"`
	Message string   `xml:"Message"`
	Region  string   `xml:"Region"`
}

func errCode(body []byte) string {
	var e s3ErrorXML
	if err := xml.Unmarshal(body, &e); err != nil {
		return ""
	}
	return e.Code
}

func (s *stack) expectStatus(resp *http.Response, body []byte, status int) {
	s.t.Helper()
	if resp.StatusCode != status {
		s.t.Fatalf("%s %s: status %d want %d\n%s", resp.Request.Method, resp.Request.URL.Path, resp.StatusCode, status, body)
	}
}

func (s *stack) expectError(resp *http.Response, body []byte, status int, code string) {
	s.t.Helper()
	s.expectStatus(resp, body, status)
	if got := errCode(body); got != code {
		s.t.Fatalf("%s %s: error code %q want %q\n%s", resp.Request.Method, resp.Request.URL.Path, got, code, body)
	}
}

// convenience wrappers
func (s *stack) put(key string, body []byte, hdr http.Header) (*http.Response, []byte) {
	return s.do(req{method: http.MethodPut, key: key, body: body, header: hdr})
}

func (s *stack) get(key string, hdr http.Header) (*http.Response, []byte) {
	return s.do(req{method: http.MethodGet, key: key, header: hdr})
}

func (s *stack) head(key string, hdr http.Header) (*http.Response, []byte) {
	return s.do(req{method: http.MethodHead, key: key, header: hdr})
}

func (s *stack) del(key string, q url.Values) (*http.Response, []byte) {
	return s.do(req{method: http.MethodDelete, key: key, query: q})
}

func (s *stack) mustPut(key string, body []byte) string {
	s.t.Helper()
	resp, b := s.put(key, body, nil)
	s.expectStatus(resp, b, http.StatusOK)
	return resp.Header.Get("ETag")
}

func (s *stack) setVersioning(status string) {
	s.t.Helper()
	body := []byte(`<VersioningConfiguration xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><Status>` + status + `</Status></VersioningConfiguration>`)
	resp, b := s.do(req{method: http.MethodPut, query: url.Values{"versioning": {""}}, body: body})
	s.expectStatus(resp, b, http.StatusOK)
	s.refreshBucket()
}

// insertMeta writes an object row directly (listing tests don't need bytes).
func (s *stack) insertMeta(key string) {
	s.t.Helper()
	if err := s.db.PutObjectMeta(&db.ObjectMeta{BucketID: s.bucket.ID, Key: key, Size: 1, ETag: "\"e\"", ContentType: "text/plain", LastModified: time.Now(), Metadata: "{}", IsLatest: true}); err != nil {
		s.t.Fatal(err)
	}
}

type listResult struct {
	XMLName               xml.Name `xml:"ListBucketResult"`
	Prefix                string   `xml:"Prefix"`
	Marker                string   `xml:"Marker"`
	NextMarker            string   `xml:"NextMarker"`
	IsTruncated           bool     `xml:"IsTruncated"`
	EncodingType          string   `xml:"EncodingType"`
	KeyCount              int      `xml:"KeyCount"`
	NextContinuationToken string   `xml:"NextContinuationToken"`
	Contents              []struct {
		Key  string `xml:"Key"`
		ETag string `xml:"ETag"`
		Size int64  `xml:"Size"`
	} `xml:"Contents"`
	CommonPrefixes []struct {
		Prefix string `xml:"Prefix"`
	} `xml:"CommonPrefixes"`
}

func parseList(t *testing.T, body []byte) listResult {
	t.Helper()
	var lr listResult
	if err := xml.Unmarshal(body, &lr); err != nil {
		t.Fatalf("parse list: %v\n%s", err, body)
	}
	return lr
}
