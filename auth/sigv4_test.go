package auth_test

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/onaonbir/Cloodsy-S3/auth"
)

const (
	testAccessKey = "AKIATESTTESTTESTTEST"
	testSecret    = "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"
	testRegion    = "eu-central-1"
)

func sha256Hex(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// uriEncode mirrors the SigV4 rules (unreserved kept, "/" optionally kept).
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

// canonicalRequest builds the SigV4 canonical request string for r.
func canonicalRequest(r *http.Request, signed []string, payloadHash string, q url.Values) string {
	sorted := make([]string, len(signed))
	for i, h := range signed {
		sorted[i] = strings.ToLower(h)
	}
	sort.Strings(sorted)
	var hdrs strings.Builder
	for _, h := range sorted {
		var v string
		if h == "host" {
			v = r.Host
		} else {
			vals := r.Header.Values(h)
			for i := range vals {
				vals[i] = strings.TrimSpace(vals[i])
			}
			v = strings.Join(vals, ",")
		}
		hdrs.WriteString(h + ":" + strings.Join(strings.Fields(v), " ") + "\n")
	}
	path := r.URL.Path
	if path == "" {
		path = "/"
	}
	segs := strings.Split(path, "/")
	for i, s := range segs {
		segs[i] = uriEncode(s, false)
	}
	return r.Method + "\n" + strings.Join(segs, "/") + "\n" + canonicalQuery(q) + "\n" + hdrs.String() + "\n" + strings.Join(sorted, ";") + "\n" + payloadHash
}

// signRequest adds X-Amz-Date, X-Amz-Content-Sha256 and Authorization headers.
// extraSigned lists additional header names to include in SignedHeaders.
func signRequest(r *http.Request, accessKey, secret, region string, payloadHash string, now time.Time, extraSigned ...string) string {
	amzDate := now.UTC().Format("20060102T150405Z")
	dateStamp := amzDate[:8]
	r.Header.Set("X-Amz-Date", amzDate)
	r.Header.Set("X-Amz-Content-Sha256", payloadHash)
	signed := append([]string{"host", "x-amz-content-sha256", "x-amz-date"}, extraSigned...)
	for i := range signed {
		signed[i] = strings.ToLower(signed[i])
	}
	sort.Strings(signed)
	cr := canonicalRequest(r, signed, payloadHash, r.URL.Query())
	scope := dateStamp + "/" + region + "/s3/aws4_request"
	sts := "AWS4-HMAC-SHA256\n" + amzDate + "\n" + scope + "\n" + sha256Hex([]byte(cr))
	key := auth.DeriveSigningKey(secret, dateStamp, region, "s3")
	sig := hex.EncodeToString(auth.HMACSHA256(key, []byte(sts)))
	r.Header.Set("Authorization", fmt.Sprintf("AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s", accessKey, scope, strings.Join(signed, ";"), sig))
	return sig
}

// presign returns a presigned URL for method/path with the given expiry.
func presign(method, base, path string, query url.Values, accessKey, secret, region string, now time.Time, expires int) string {
	amzDate := now.UTC().Format("20060102T150405Z")
	dateStamp := amzDate[:8]
	scope := dateStamp + "/" + region + "/s3/aws4_request"
	q := url.Values{}
	for k, v := range query {
		q[k] = v
	}
	q.Set("X-Amz-Algorithm", "AWS4-HMAC-SHA256")
	q.Set("X-Amz-Credential", accessKey+"/"+scope)
	q.Set("X-Amz-Date", amzDate)
	q.Set("X-Amz-Expires", strconv.Itoa(expires))
	q.Set("X-Amz-SignedHeaders", "host")
	u, _ := url.Parse(base)
	r := &http.Request{Method: method, Host: u.Host, URL: &url.URL{Path: path}, Header: http.Header{}}
	cr := canonicalRequest(r, []string{"host"}, "UNSIGNED-PAYLOAD", q)
	sts := "AWS4-HMAC-SHA256\n" + amzDate + "\n" + scope + "\n" + sha256Hex([]byte(cr))
	key := auth.DeriveSigningKey(secret, dateStamp, region, "s3")
	q.Set("X-Amz-Signature", hex.EncodeToString(auth.HMACSHA256(key, []byte(sts))))
	return base + strings.Join(func() []string {
		segs := strings.Split(path, "/")
		for i, s := range segs {
			segs[i] = uriEncode(s, false)
		}
		return segs
	}(), "/") + "?" + q.Encode()
}

func newSignedRequest(t *testing.T, method, path string, body []byte, now time.Time, extra ...string) *http.Request {
	t.Helper()
	r, err := http.NewRequest(method, "http://s3.local:9000"+path, strings.NewReader(string(body)))
	if err != nil {
		t.Fatal(err)
	}
	signRequest(r, testAccessKey, testSecret, testRegion, sha256Hex(body), now, extra...)
	return r
}

func TestVerifySignature_OK(t *testing.T) {
	r := newSignedRequest(t, http.MethodPut, "/test-bucket/some%20key/ünï.txt", []byte("hello"), time.Now())
	parsed, err := auth.ParseAuthorizationHeader(r.Header.Get("Authorization"))
	if err != nil {
		t.Fatal(err)
	}
	if parsed.AccessKey != testAccessKey || parsed.Region != testRegion || parsed.Service != "s3" {
		t.Fatalf("parsed %+v", parsed)
	}
	if err := auth.VerifySignature(r, testSecret, testRegion, parsed); err != nil {
		t.Fatalf("verify: %v", err)
	}
	if parsed.AmzDate == "" || !strings.HasSuffix(parsed.Scope, "/"+testRegion+"/s3/aws4_request") {
		t.Fatalf("AmzDate/Scope not populated: %+v", parsed)
	}
	// nil auth re-parses the header.
	if err := auth.VerifySignature(r, testSecret, testRegion, nil); err != nil {
		t.Fatalf("verify(nil): %v", err)
	}
	// Query parameters are part of the signature.
	r2 := newSignedRequest(t, http.MethodGet, "/test-bucket", nil, time.Now())
	r2.URL.RawQuery = "prefix=a%2Fb&max-keys=10&list-type=2"
	signRequest(r2, testAccessKey, testSecret, testRegion, sha256Hex(nil), time.Now())
	if err := auth.VerifySignature(r2, testSecret, testRegion, nil); err != nil {
		t.Fatalf("verify with query: %v", err)
	}
	r2.URL.RawQuery = "prefix=a%2Fc&max-keys=10&list-type=2"
	if err := auth.VerifySignature(r2, testSecret, testRegion, nil); !errors.Is(err, auth.ErrSignatureMismatch) {
		t.Fatalf("tampered query: %v", err)
	}
}

func TestVerifySignature_Failures(t *testing.T) {
	now := time.Now()
	t.Run("tampered payload hash", func(t *testing.T) {
		r := newSignedRequest(t, http.MethodPut, "/b/k", []byte("hello"), now)
		r.Header.Set("X-Amz-Content-Sha256", sha256Hex([]byte("evil")))
		if err := auth.VerifySignature(r, testSecret, testRegion, nil); !errors.Is(err, auth.ErrSignatureMismatch) {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("wrong secret", func(t *testing.T) {
		r := newSignedRequest(t, http.MethodGet, "/b/k", nil, now)
		if err := auth.VerifySignature(r, "other", testRegion, nil); !errors.Is(err, auth.ErrSignatureMismatch) {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("time skew", func(t *testing.T) {
		r := newSignedRequest(t, http.MethodGet, "/b/k", nil, now.Add(-20*time.Minute))
		if err := auth.VerifySignature(r, testSecret, testRegion, nil); !errors.Is(err, auth.ErrTimeSkewed) {
			t.Fatalf("got %v", err)
		}
		r = newSignedRequest(t, http.MethodGet, "/b/k", nil, now.Add(20*time.Minute))
		if err := auth.VerifySignature(r, testSecret, testRegion, nil); !errors.Is(err, auth.ErrTimeSkewed) {
			t.Fatalf("future: got %v", err)
		}
		r = newSignedRequest(t, http.MethodGet, "/b/k", nil, now.Add(-10*time.Minute))
		if err := auth.VerifySignature(r, testSecret, testRegion, nil); err != nil {
			t.Fatalf("10 min old should verify: %v", err)
		}
	})
	t.Run("region mismatch", func(t *testing.T) {
		r := newSignedRequest(t, http.MethodGet, "/b/k", nil, now)
		if err := auth.VerifySignature(r, testSecret, "us-west-2", nil); !errors.Is(err, auth.ErrRegionMismatch) {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("date mismatch", func(t *testing.T) {
		r := newSignedRequest(t, http.MethodGet, "/b/k", nil, now)
		a, _ := auth.ParseAuthorizationHeader(r.Header.Get("Authorization"))
		a.Date = "20000101"
		if err := auth.VerifySignature(r, testSecret, testRegion, a); !errors.Is(err, auth.ErrDateMismatch) {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("missing date", func(t *testing.T) {
		r := newSignedRequest(t, http.MethodGet, "/b/k", nil, now)
		r.Header.Del("X-Amz-Date")
		if err := auth.VerifySignature(r, testSecret, testRegion, nil); !errors.Is(err, auth.ErrMissingDate) {
			t.Fatalf("got %v", err)
		}
		r.Header.Set("X-Amz-Date", "not-a-date")
		if err := auth.VerifySignature(r, testSecret, testRegion, nil); !errors.Is(err, auth.ErrInvalidDate) {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("missing content sha", func(t *testing.T) {
		r := newSignedRequest(t, http.MethodGet, "/b/k", nil, now)
		r.Header.Del("X-Amz-Content-Sha256")
		if err := auth.VerifySignature(r, testSecret, testRegion, nil); !errors.Is(err, auth.ErrMissingContentSHA) {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("missing auth", func(t *testing.T) {
		r, _ := http.NewRequest(http.MethodGet, "http://h/b", nil)
		if err := auth.VerifySignature(r, testSecret, testRegion, nil); !errors.Is(err, auth.ErrMissingAuth) {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("Date header fallback", func(t *testing.T) {
		r := newSignedRequest(t, http.MethodGet, "/b/k", nil, now)
		amz := r.Header.Get("X-Amz-Date")
		tm, _ := time.Parse("20060102T150405Z", amz)
		r.Header.Del("X-Amz-Date")
		r.Header.Set("Date", tm.UTC().Format(http.TimeFormat))
		// Signature covered x-amz-date which is now absent → mismatch, but the
		// date itself must be accepted (no ErrMissingDate).
		if err := auth.VerifySignature(r, testSecret, testRegion, nil); !errors.Is(err, auth.ErrSignatureMismatch) {
			t.Fatalf("got %v", err)
		}
	})
}

func TestParseAuthorizationHeader(t *testing.T) {
	compact := "AWS4-HMAC-SHA256 Credential=AK/20240101/eu-central-1/s3/aws4_request,SignedHeaders=host;x-amz-date,Signature=abc"
	a, err := auth.ParseAuthorizationHeader(compact)
	if err != nil {
		t.Fatal(err)
	}
	if a.AccessKey != "AK" || a.Date != "20240101" || a.Region != "eu-central-1" || a.Signature != "abc" || len(a.SignedHeaders) != 2 {
		t.Fatalf("parsed %+v", a)
	}
	spaced := "AWS4-HMAC-SHA256 Credential=AK/20240101/eu-central-1/s3/aws4_request , SignedHeaders=host;x-amz-date , Signature=abc"
	if _, err := auth.ParseAuthorizationHeader(spaced); err != nil {
		t.Fatalf("spaced: %v", err)
	}
	bad := []string{
		"",
		"AWS abc",
		"AWS4-HMAC-SHA256 Credential=AK/20240101/eu-central-1/s3/aws4_request, Signature=abc",                           // no signed headers
		"AWS4-HMAC-SHA256 Credential=AK/20240101/eu-central-1/s3/aws4_request, SignedHeaders=host",                      // no signature
		"AWS4-HMAC-SHA256 Credential=AK/20240101/eu-central-1/s3, SignedHeaders=host, Signature=abc",                    // short scope
		"AWS4-HMAC-SHA256 Credential=AK/20240101/eu-central-1/ec2/aws4_request, SignedHeaders=host, Signature=abc",      // service
		"AWS4-HMAC-SHA256 Credential=AK/20240101/eu-central-1/s3/aws4_request, SignedHeaders=x-amz-date, Signature=abc", // no host
	}
	for _, h := range bad {
		if _, err := auth.ParseAuthorizationHeader(h); err == nil {
			t.Errorf("accepted %q", h)
		}
	}
}

func TestVerifySignature_MultiValuedHeader(t *testing.T) {
	r, _ := http.NewRequest(http.MethodPut, "http://s3.local/b/k", strings.NewReader("x"))
	r.Header.Add("X-Amz-Meta-Foo", "a")
	r.Header.Add("X-Amz-Meta-Foo", " b ")
	signRequest(r, testAccessKey, testSecret, testRegion, sha256Hex([]byte("x")), time.Now(), "x-amz-meta-foo")
	if err := auth.VerifySignature(r, testSecret, testRegion, nil); err != nil {
		t.Fatalf("verify: %v", err)
	}
	// Signed as a single "a,b" value must produce the same signature.
	r2, _ := http.NewRequest(http.MethodPut, "http://s3.local/b/k", strings.NewReader("x"))
	r2.Header.Set("X-Amz-Meta-Foo", "a,b")
	r2.Header.Set("X-Amz-Date", r.Header.Get("X-Amz-Date"))
	r2.Header.Set("X-Amz-Content-Sha256", r.Header.Get("X-Amz-Content-Sha256"))
	r2.Header.Set("Authorization", r.Header.Get("Authorization"))
	if err := auth.VerifySignature(r2, testSecret, testRegion, nil); err != nil {
		t.Fatalf("verify joined: %v", err)
	}
	// Changing a signed header breaks the signature.
	r.Header.Set("X-Amz-Meta-Foo", "z")
	if err := auth.VerifySignature(r, testSecret, testRegion, nil); !errors.Is(err, auth.ErrSignatureMismatch) {
		t.Fatalf("tampered header: %v", err)
	}
	// Whitespace folding inside a header value is canonicalized.
	r3, _ := http.NewRequest(http.MethodPut, "http://s3.local/b/k", strings.NewReader("x"))
	r3.Header.Set("X-Amz-Meta-Foo", "  a    b  ")
	signRequest(r3, testAccessKey, testSecret, testRegion, sha256Hex([]byte("x")), time.Now(), "x-amz-meta-foo")
	if err := auth.VerifySignature(r3, testSecret, testRegion, nil); err != nil {
		t.Fatalf("folded whitespace: %v", err)
	}
}

func TestPresigned(t *testing.T) {
	now := time.Now()
	get := func(u string) *http.Request {
		r, err := http.NewRequest(http.MethodGet, u, nil)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	verify := func(r *http.Request) error {
		a, err := auth.ParsePresignedQuery(r.URL.Query())
		if err != nil {
			return err
		}
		return auth.VerifyPresignedSignature(r, testSecret, testRegion, a)
	}

	u := presign(http.MethodGet, "http://s3.local:9000", "/test-bucket/dir/my file+ü.txt", nil, testAccessKey, testSecret, testRegion, now, 300)
	if err := verify(get(u)); err != nil {
		t.Fatalf("presigned verify: %v", err)
	}
	// Extra response-* params are signed too.
	u2 := presign(http.MethodGet, "http://s3.local:9000", "/test-bucket/k", url.Values{"response-content-type": {"text/plain"}}, testAccessKey, testSecret, testRegion, now, 300)
	if err := verify(get(u2)); err != nil {
		t.Fatalf("presigned with extra query: %v", err)
	}
	if err := verify(get(u2 + "&extra=1")); !errors.Is(err, auth.ErrSignatureMismatch) {
		t.Fatalf("unsigned extra param: %v", err)
	}
	// Wrong secret.
	r := get(u)
	a, _ := auth.ParsePresignedQuery(r.URL.Query())
	if err := auth.VerifyPresignedSignature(r, "nope", testRegion, a); !errors.Is(err, auth.ErrSignatureMismatch) {
		t.Fatalf("wrong secret: %v", err)
	}
	// Wrong region.
	if err := auth.VerifyPresignedSignature(r, testSecret, "us-east-1", a); !errors.Is(err, auth.ErrRegionMismatch) {
		t.Fatalf("wrong region: %v", err)
	}
	// Not yet valid: dated one day ahead.
	uf := presign(http.MethodGet, "http://s3.local:9000", "/b/k", nil, testAccessKey, testSecret, testRegion, now.Add(24*time.Hour), 300)
	if err := verify(get(uf)); !errors.Is(err, auth.ErrPresignedNotYet) {
		t.Fatalf("future: %v", err)
	}
	// Slightly ahead (within skew) is fine.
	us := presign(http.MethodGet, "http://s3.local:9000", "/b/k", nil, testAccessKey, testSecret, testRegion, now.Add(5*time.Minute), 300)
	if err := verify(get(us)); err != nil {
		t.Fatalf("within skew: %v", err)
	}
	// Expired: dated 10 min ago with 60s expiry.
	ue := presign(http.MethodGet, "http://s3.local:9000", "/b/k", nil, testAccessKey, testSecret, testRegion, now.Add(-10*time.Minute), 60)
	if err := verify(get(ue)); !errors.Is(err, auth.ErrPresignedExpired) {
		t.Fatalf("expired: %v", err)
	}
	// Expires too large / invalid.
	for _, exp := range []int{604801, 0, -1} {
		ux := presign(http.MethodGet, "http://s3.local:9000", "/b/k", nil, testAccessKey, testSecret, testRegion, now, exp)
		err := verify(get(ux))
		if err == nil || !strings.Contains(err.Error(), "X-Amz-Expires") {
			t.Fatalf("expires=%d: %v", exp, err)
		}
	}
	// Max allowed expiry is accepted.
	um := presign(http.MethodGet, "http://s3.local:9000", "/b/k", nil, testAccessKey, testSecret, testRegion, now, 604800)
	if err := verify(get(um)); err != nil {
		t.Fatalf("max expiry: %v", err)
	}
	// Method is signed.
	rp := get(u)
	rp.Method = http.MethodDelete
	if err := verify(rp); !errors.Is(err, auth.ErrSignatureMismatch) {
		t.Fatalf("method change: %v", err)
	}
	// Parse failures.
	for _, q := range []string{
		"X-Amz-Algorithm=AWS4-HMAC-SHA512",
		"X-Amz-Algorithm=AWS4-HMAC-SHA256",
		"X-Amz-Algorithm=AWS4-HMAC-SHA256&X-Amz-Credential=a/b/c/s3/x&X-Amz-Signature=s",
		"X-Amz-Algorithm=AWS4-HMAC-SHA256&X-Amz-Credential=a/b/c/s3/x&X-Amz-Signature=s&X-Amz-SignedHeaders=x-amz-date&X-Amz-Date=20240101T000000Z&X-Amz-Expires=10",
	} {
		v, _ := url.ParseQuery(q)
		if _, err := auth.ParsePresignedQuery(v); err == nil {
			t.Errorf("accepted %q", q)
		}
	}
}

func TestKeyGeneration(t *testing.T) {
	ak, err := auth.GenerateAccessKey()
	if err != nil || len(ak) != 20 || !strings.HasPrefix(ak, "AK") {
		t.Fatalf("access key %q %v", ak, err)
	}
	sk, err := auth.GenerateSecretKey()
	if err != nil || len(sk) != 40 {
		t.Fatalf("secret key %q %v", sk, err)
	}
	if sk2, _ := auth.GenerateSecretKey(); sk2 == sk {
		t.Fatal("secret keys not random")
	}
	if auth.HashPayload(nil) != sha256Hex(nil) {
		t.Fatal("HashPayload(nil) should be the empty hash")
	}
	if auth.HashPayload(strings.NewReader("abc")) != sha256Hex([]byte("abc")) {
		t.Fatal("HashPayload mismatch")
	}
	if auth.HashSHA256Hex([]byte("abc")) != sha256Hex([]byte("abc")) {
		t.Fatal("HashSHA256Hex mismatch")
	}
}
