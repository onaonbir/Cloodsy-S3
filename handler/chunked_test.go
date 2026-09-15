package handler

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/onaonbir/Cloodsy-S3/auth"
)

// copyWithTimeout runs io.Copy in a goroutine and fails the test if it does
// not return within the deadline (a hung decoder is the bug we guard against).
func copyWithTimeout(t *testing.T, r io.Reader, d time.Duration) ([]byte, error) {
	t.Helper()
	type res struct {
		b   []byte
		err error
	}
	ch := make(chan res, 1)
	go func() {
		var buf bytes.Buffer
		_, err := io.Copy(&buf, r)
		ch <- res{buf.Bytes(), err}
	}()
	select {
	case r := <-ch:
		return r.b, r.err
	case <-time.After(d):
		t.Fatalf("io.Copy did not return within %v", d)
		return nil, nil
	}
}

// encodeUnsignedChunks produces an aws-chunked body without signatures and
// with optional trailers.
func encodeUnsignedChunks(chunks [][]byte, trailers map[string]string) []byte {
	var b bytes.Buffer
	for _, c := range chunks {
		fmt.Fprintf(&b, "%x\r\n", len(c))
		b.Write(c)
		b.WriteString("\r\n")
	}
	b.WriteString("0\r\n")
	for k, v := range trailers {
		b.WriteString(k + ":" + v + "\r\n")
	}
	b.WriteString("\r\n")
	return b.Bytes()
}

// testChunkSigner mirrors the client-side STREAMING-AWS4-HMAC-SHA256-PAYLOAD
// signing chain.
type testChunkSigner struct {
	key     []byte
	amzDate string
	scope   string
	prev    string
}

func newTestChunkSigner(secret, dateStamp, region, amzDate, seed string) *testChunkSigner {
	return &testChunkSigner{
		key:     auth.DeriveSigningKey(secret, dateStamp, region, "s3"),
		amzDate: amzDate,
		scope:   dateStamp + "/" + region + "/s3/aws4_request",
		prev:    seed,
	}
}

func (s *testChunkSigner) sign(chunk []byte) string {
	sum := sha256.Sum256(chunk)
	empty := sha256.Sum256(nil)
	sts := "AWS4-HMAC-SHA256-PAYLOAD\n" + s.amzDate + "\n" + s.scope + "\n" + s.prev + "\n" + hex.EncodeToString(empty[:]) + "\n" + hex.EncodeToString(sum[:])
	sig := hex.EncodeToString(auth.HMACSHA256(s.key, []byte(sts)))
	s.prev = sig
	return sig
}

func (s *testChunkSigner) signTrailer(canonical string) string {
	sum := sha256.Sum256([]byte(canonical))
	sts := "AWS4-HMAC-SHA256-TRAILER\n" + s.amzDate + "\n" + s.scope + "\n" + s.prev + "\n" + hex.EncodeToString(sum[:])
	return hex.EncodeToString(auth.HMACSHA256(s.key, []byte(sts)))
}

// encodeSignedChunks produces a signed aws-chunked body. trailers (in order)
// are appended after the final chunk with an x-amz-trailer-signature.
func encodeSignedChunks(s *testChunkSigner, chunks [][]byte, trailers [][2]string) []byte {
	var b bytes.Buffer
	for _, c := range chunks {
		fmt.Fprintf(&b, "%x;chunk-signature=%s\r\n", len(c), s.sign(c))
		b.Write(c)
		b.WriteString("\r\n")
	}
	fmt.Fprintf(&b, "0;chunk-signature=%s\r\n", s.sign(nil))
	if len(trailers) > 0 {
		var canonical strings.Builder
		for _, kv := range trailers {
			b.WriteString(kv[0] + ":" + kv[1] + "\r\n")
			canonical.WriteString(kv[0] + ":" + kv[1] + "\n")
		}
		b.WriteString("x-amz-trailer-signature:" + s.signTrailer(canonical.String()) + "\r\n")
	}
	b.WriteString("\r\n")
	return b.Bytes()
}

func TestChunked_TruncatedChunkReturnsPromptly(t *testing.T) {
	cr := newAWSChunkedReader(strings.NewReader("100;chunk-signature=x\r\nhello"), nil)
	start := time.Now()
	got, err := copyWithTimeout(t, cr, time.Second)
	if !errors.Is(err, errIncompleteChunked) {
		t.Fatalf("want errIncompleteChunked, got %v (data %q)", err, got)
	}
	if time.Since(start) > time.Second {
		t.Fatal("decoder hung")
	}
	if cr.Complete() {
		t.Fatal("must not report complete")
	}
	// Subsequent reads keep returning the error.
	if _, err := cr.Read(make([]byte, 8)); !errors.Is(err, errIncompleteChunked) {
		t.Fatalf("sticky error: %v", err)
	}
}

func TestChunked_Malformed(t *testing.T) {
	cases := map[string]struct {
		body string
		want error
	}{
		"bad size":              {"ZZ;chunk-signature=x\r\nhi\r\n0\r\n\r\n", errMalformedChunk},
		"negative size":         {"-5\r\n\r\n0\r\n\r\n", errMalformedChunk},
		"header too long":       {strings.Repeat("a", 5000) + "\r\n", errMalformedChunk},
		"missing crlf":          {"2\r\nhixx0\r\n\r\n", errMalformedChunk},
		"missing final zero":    {"5\r\nhello\r\n", errIncompleteChunked},
		"empty body":            {"", errIncompleteChunked},
		"trailer without colon": {"0\r\nnocolon\r\n\r\n", errMalformedChunk},
		"chunk too large":       {"200000000000\r\n", errChunkTooLarge},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			cr := newAWSChunkedReader(strings.NewReader(c.body), nil)
			_, err := copyWithTimeout(t, cr, time.Second)
			if !errors.Is(err, c.want) {
				t.Fatalf("want %v, got %v", c.want, err)
			}
		})
	}
}

func TestChunked_UnsignedWithTrailer(t *testing.T) {
	payload := []byte("hello, chunked world!")
	crc := crc32.ChecksumIEEE(payload)
	crcB64 := base64.StdEncoding.EncodeToString([]byte{byte(crc >> 24), byte(crc >> 16), byte(crc >> 8), byte(crc)})
	body := encodeUnsignedChunks([][]byte{payload[:5], payload[5:12], payload[12:]}, map[string]string{"x-amz-checksum-crc32": crcB64})
	cr := newAWSChunkedReader(bytes.NewReader(body), nil)
	got, err := copyWithTimeout(t, cr, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("decoded %q", got)
	}
	if !cr.Complete() {
		t.Fatal("Complete() false")
	}
	if cr.Trailers["x-amz-checksum-crc32"] != crcB64 {
		t.Fatalf("trailers %v", cr.Trailers)
	}
	// Reading past the end yields EOF, not an error.
	if n, err := cr.Read(make([]byte, 4)); n != 0 || err != io.EOF {
		t.Fatalf("after end: n=%d err=%v", n, err)
	}
	// Empty payload with only the final chunk (no trailer section).
	cr = newAWSChunkedReader(strings.NewReader("0\r\n\r\n"), nil)
	got, err = copyWithTimeout(t, cr, time.Second)
	if err != nil || len(got) != 0 || !cr.Complete() {
		t.Fatalf("empty: %v %q complete=%v", err, got, cr.Complete())
	}
	// Bare LF line endings after data are tolerated; "0" with EOF right after it.
	cr = newAWSChunkedReader(strings.NewReader("3\r\nabc\n0\r\n"), nil)
	got, err = copyWithTimeout(t, cr, time.Second)
	if err != nil || string(got) != "abc" || !cr.Complete() {
		t.Fatalf("lf: %v %q complete=%v", err, got, cr.Complete())
	}
	// Small read buffers spanning chunk boundaries.
	cr = newAWSChunkedReader(bytes.NewReader(body), nil)
	var out []byte
	buf := make([]byte, 3)
	for {
		n, err := cr.Read(buf)
		out = append(out, buf[:n]...)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	if !bytes.Equal(out, payload) {
		t.Fatalf("small reads decoded %q", out)
	}
}

func TestChunked_Signed(t *testing.T) {
	const (
		secret    = "secret-key"
		dateStamp = "20240102"
		amzDate   = "20240102T030405Z"
		region    = "eu-west-1"
		seed      = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	)
	a := &auth.SigV4Auth{Date: dateStamp, Region: region, Signature: seed, AmzDate: amzDate, Scope: dateStamp + "/" + region + "/s3/aws4_request"}
	payload := bytes.Repeat([]byte("0123456789"), 2000)
	chunks := [][]byte{payload[:8192], payload[8192:16384], payload[16384:]}

	t.Run("valid chain", func(t *testing.T) {
		body := encodeSignedChunks(newTestChunkSigner(secret, dateStamp, region, amzDate, seed), chunks, nil)
		cr := newAWSChunkedReader(bytes.NewReader(body), newChunkSigner(secret, a))
		got, err := copyWithTimeout(t, cr, time.Second)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, payload) || !cr.Complete() {
			t.Fatalf("decoded %d bytes complete=%v", len(got), cr.Complete())
		}
	})
	t.Run("valid chain with signed trailer", func(t *testing.T) {
		body := encodeSignedChunks(newTestChunkSigner(secret, dateStamp, region, amzDate, seed), chunks, [][2]string{{"x-amz-checksum-sha256", "abc="}})
		cr := newAWSChunkedReader(bytes.NewReader(body), newChunkSigner(secret, a))
		if _, err := copyWithTimeout(t, cr, time.Second); err != nil {
			t.Fatal(err)
		}
		if cr.Trailers["x-amz-checksum-sha256"] != "abc=" {
			t.Fatalf("trailers %v", cr.Trailers)
		}
	})
	t.Run("tampered byte", func(t *testing.T) {
		body := encodeSignedChunks(newTestChunkSigner(secret, dateStamp, region, amzDate, seed), chunks, nil)
		// Flip a payload byte inside the second chunk.
		idx := bytes.Index(body, []byte("\r\n"+string(chunks[1][:16]))) + 2 + 5
		body[idx] ^= 0x01
		cr := newAWSChunkedReader(bytes.NewReader(body), newChunkSigner(secret, a))
		_, err := copyWithTimeout(t, cr, time.Second)
		if !errors.Is(err, errChunkSignature) {
			t.Fatalf("want errChunkSignature, got %v", err)
		}
		if cr.Complete() {
			t.Fatal("must not be complete")
		}
	})
	t.Run("tampered trailer", func(t *testing.T) {
		body := encodeSignedChunks(newTestChunkSigner(secret, dateStamp, region, amzDate, seed), chunks, [][2]string{{"x-amz-checksum-sha256", "abc="}})
		body = bytes.Replace(body, []byte("abc="), []byte("abd="), 1)
		cr := newAWSChunkedReader(bytes.NewReader(body), newChunkSigner(secret, a))
		if _, err := copyWithTimeout(t, cr, time.Second); !errors.Is(err, errChunkSignature) {
			t.Fatalf("want errChunkSignature, got %v", err)
		}
	})
	t.Run("wrong seed", func(t *testing.T) {
		body := encodeSignedChunks(newTestChunkSigner(secret, dateStamp, region, amzDate, "bbbb"), chunks, nil)
		cr := newAWSChunkedReader(bytes.NewReader(body), newChunkSigner(secret, a))
		if _, err := copyWithTimeout(t, cr, time.Second); !errors.Is(err, errChunkSignature) {
			t.Fatalf("want errChunkSignature, got %v", err)
		}
	})
	t.Run("missing chunk signature", func(t *testing.T) {
		body := encodeUnsignedChunks(chunks, nil)
		cr := newAWSChunkedReader(bytes.NewReader(body), newChunkSigner(secret, a))
		if _, err := copyWithTimeout(t, cr, time.Second); !errors.Is(err, errChunkSignature) {
			t.Fatalf("want errChunkSignature, got %v", err)
		}
	})
	t.Run("signer requires verified auth", func(t *testing.T) {
		if newChunkSigner(secret, nil) != nil {
			t.Fatal("nil auth")
		}
		if newChunkSigner(secret, &auth.SigV4Auth{Date: dateStamp}) != nil {
			t.Fatal("auth without AmzDate/Scope")
		}
	})
}

func TestPayloadChecker(t *testing.T) {
	// Trailer checksum wiring: expectedSum empty, algo from trailer.
	payload := []byte("abc")
	crc := crc32.ChecksumIEEE(payload)
	crcB64 := base64.StdEncoding.EncodeToString([]byte{byte(crc >> 24), byte(crc >> 16), byte(crc >> 8), byte(crc)})
	body := encodeUnsignedChunks([][]byte{payload}, map[string]string{"x-amz-checksum-crc32": crcB64})
	cr := newAWSChunkedReader(bytes.NewReader(body), nil)
	pc := &payloadChecker{chunked: cr, checksumAlgo: "crc32", checksumHash: crc32.NewIEEE(), declared: 3}
	if _, err := io.Copy(io.Discard, pc.wrap(cr)); err != nil {
		t.Fatal(err)
	}
	if err := pc.verify(3, nil); err != nil {
		t.Fatalf("verify: %v", err)
	}
	if err := pc.verify(2, nil); !errors.Is(err, errIncompleteBody) {
		t.Fatalf("declared mismatch: %v", err)
	}
	// Incomplete chunked stream is rejected at verify time.
	cr2 := newAWSChunkedReader(strings.NewReader("3\r\nabc\r\n"), nil)
	pc2 := &payloadChecker{chunked: cr2, declared: -1}
	io.Copy(io.Discard, pc2.wrap(cr2))
	if err := pc2.verify(3, nil); !errors.Is(err, errIncompleteChunked) {
		t.Fatalf("incomplete: %v", err)
	}
	// Bad base64 in checksum header.
	pc3 := &payloadChecker{checksumAlgo: "crc32", checksumHash: crc32.NewIEEE(), expectedSum: "!!!", declared: -1}
	if err := pc3.verify(0, nil); !errors.Is(err, errInvalidDigest) {
		t.Fatalf("bad base64: %v", err)
	}
}

func TestParseRange(t *testing.T) {
	cases := []struct {
		hdr        string
		size       int64
		start, end int64
		ok         bool
	}{
		{"bytes=0-4", 100, 0, 4, true},
		{"bytes=-5", 100, 95, 99, true},
		{"bytes=95-", 100, 95, 99, true},
		{"bytes=0-", 100, 0, 99, true},
		{"bytes=50-1000", 100, 50, 99, true},
		{"bytes=-1000", 100, 0, 99, true},
		{"bytes=0-4,10-20", 100, 0, 0, false},
		{"bytes=100-", 100, -1, 0, false},
		{"bytes=200-300", 100, -1, 0, false},
		{"bytes=-0", 100, -1, 0, false},
		{"bytes=-5", 0, -1, 0, false},
		{"bytes=5-2", 100, 0, 0, false},
		{"items=0-4", 100, 0, 0, false},
		{"bytes=abc", 100, 0, 0, false},
		{" bytes=1-2 ", 100, 1, 2, true},
	}
	for _, c := range cases {
		s, e, ok := parseRange(c.hdr, c.size)
		if s != c.start || e != c.end || ok != c.ok {
			t.Errorf("parseRange(%q,%d) = %d,%d,%v want %d,%d,%v", c.hdr, c.size, s, e, ok, c.start, c.end, c.ok)
		}
	}
}

func TestEtagMatchAndCopySource(t *testing.T) {
	if !etagMatch("\"abc\"", "\"abc\"") || !etagMatch("W/\"abc\"", "\"abc\"") || !etagMatch("*", "\"x\"") || !etagMatch("\"x\", \"abc\"", "\"abc\"") {
		t.Fatal("etagMatch positives")
	}
	if etagMatch("\"abd\"", "\"abc\"") {
		t.Fatal("etagMatch negative")
	}
	b, k, v, err := parseCopySource("/src-bucket/dir/my%20file%2B%C3%BC.txt?versionId=v1")
	if err != nil || b != "src-bucket" || k != "dir/my file+ü.txt" || v != "v1" {
		t.Fatalf("parseCopySource: %q %q %q %v", b, k, v, err)
	}
	if _, _, _, err := parseCopySource("bucketonly"); err == nil {
		t.Fatal("missing key accepted")
	}
	if _, _, _, err := parseCopySource("/b/%zz"); err == nil {
		t.Fatal("bad escape accepted")
	}
}

func TestEncodeKeyIfNeeded(t *testing.T) {
	if got := EncodeKeyIfNeeded("a b+c/d&e.txt", "url"); got != "a%20b%2Bc/d%26e.txt" {
		t.Fatalf("got %q", got)
	}
	if got := EncodeKeyIfNeeded("a b", ""); got != "a b" {
		t.Fatalf("no encoding: %q", got)
	}
}

func TestIsValidObjectKey(t *testing.T) {
	good := []string{"a", "report..final.pdf", "dir/", "a b/ü.txt", "..hidden", "x/..y", strings.Repeat("k", 1024)}
	for _, k := range good {
		if !isValidObjectKey(k) {
			t.Errorf("%q should be valid", k)
		}
	}
	bad := []string{"", "../x", "a/../b", "a/./b", ".", "..", "a\x00b", strings.Repeat("k", 1025)}
	for _, k := range bad {
		if isValidObjectKey(k) {
			t.Errorf("%q should be invalid", k)
		}
	}
}

func TestCollectMetadata(t *testing.T) {
	r, _ := http.NewRequest("PUT", "/b/k", nil)
	r.Header.Set("X-Amz-Meta-Foo", "bar")
	r.Header.Add("X-Amz-Meta-Multi", "a")
	r.Header.Add("X-Amz-Meta-Multi", "b")
	r.Header.Set("Cache-Control", "max-age=60")
	r.Header.Set("Content-Encoding", "aws-chunked, gzip")
	r.Header.Set("Content-Disposition", "inline")
	m, err := collectMetadata(r)
	if err != nil {
		t.Fatal(err)
	}
	parsed := parseMetadata(m)
	if parsed["foo"] != "bar" || parsed["multi"] != "a,b" {
		t.Fatalf("user meta %v", parsed)
	}
	if parsed["$cache-control"] != "max-age=60" || parsed["$content-encoding"] != "gzip" || parsed["$content-disposition"] != "inline" {
		t.Fatalf("system meta %v", parsed)
	}
	r2, _ := http.NewRequest("PUT", "/b/k", nil)
	r2.Header.Set("Content-Encoding", "aws-chunked")
	m2, _ := collectMetadata(r2)
	if m2 != "{}" {
		t.Fatalf("bare aws-chunked must be dropped: %s", m2)
	}
	r3, _ := http.NewRequest("PUT", "/b/k", nil)
	r3.Header.Set("X-Amz-Meta-Big", strings.Repeat("v", 300))
	if _, err := collectMetadata(r3); err == nil {
		t.Fatal("oversized value accepted")
	}
	r4, _ := http.NewRequest("PUT", "/b/k", nil)
	for i := 0; i < 20; i++ {
		r4.Header.Set(fmt.Sprintf("X-Amz-Meta-K%d", i), strings.Repeat("v", 200))
	}
	if _, err := collectMetadata(r4); err == nil {
		t.Fatal("total size limit not enforced")
	}
}
