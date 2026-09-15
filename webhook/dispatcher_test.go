package webhook

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/onaonbir/Cloodsy-S3/db"
)

func TestValidateURL(t *testing.T) {
	reject := []string{
		"ftp://example.com/hook",
		"file:///etc/passwd",
		"gopher://example.com",
		"http://localhost/hook",
		"http://LOCALHOST:8080/hook",
		"http://foo.localhost/hook",
		"http://127.0.0.1/hook",
		"http://127.0.0.1:9/x",
		"http://[::1]/hook",
		"http://169.254.169.254/latest/meta-data/",
		"http://10.0.0.1/hook",
		"http://172.16.5.5/hook",
		"http://192.168.0.1/hook",
		"http://0.0.0.0/hook",
		"http:///nohost",
		"not a url",
		"",
	}
	for _, u := range reject {
		if err := ValidateURL(u, false); err == nil {
			t.Errorf("ValidateURL(%q, false) accepted", u)
		}
	}
	// Scheme/host problems are rejected even when private targets are allowed.
	for _, u := range []string{"ftp://10.0.0.1/x", "http:///nohost", "://bad"} {
		if err := ValidateURL(u, true); err == nil {
			t.Errorf("ValidateURL(%q, true) accepted", u)
		}
	}
	// Private targets are fine when explicitly allowed.
	for _, u := range []string{"http://localhost/hook", "http://127.0.0.1:9/x", "http://169.254.169.254/", "http://10.0.0.1/hook"} {
		if err := ValidateURL(u, true); err != nil {
			t.Errorf("ValidateURL(%q, true) = %v", u, err)
		}
	}
	// Public literal IPs are accepted without DNS.
	if err := ValidateURL("http://8.8.8.8/hook", false); err != nil {
		t.Errorf("public ip rejected: %v", err)
	}
	// A name the resolver cannot even query (syntactically invalid label) must
	// be accepted: delivery simply fails later.
	if err := ValidateURL("https://a..b/hook", false); err != nil {
		t.Errorf("unresolvable host rejected: %v", err)
	}
	// example.com: accepted whether DNS works or not — unless the sandbox
	// resolver maps it to a private range, in which case rejection is right.
	if err := ValidateURL("https://example.com/hook", false); err != nil {
		ips, lerr := net.LookupIP("example.com")
		private := false
		for _, ip := range ips {
			if ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsUnspecified() {
				private = true
			}
		}
		if lerr != nil || !private {
			t.Errorf("ValidateURL(example.com) = %v (lookup: %v %v)", err, ips, lerr)
		}
	}
}

func TestMatchEvent(t *testing.T) {
	cases := []struct {
		pattern, event string
		want           bool
	}{
		{"*", "s3:ObjectCreated:Put", true},
		{"*", "anything", true},
		{"s3:ObjectCreated:Put", "s3:ObjectCreated:Put", true},
		{"s3:ObjectCreated:Put", "s3:ObjectCreated:Copy", false},
		{"s3:ObjectCreated:*", "s3:ObjectCreated:Copy", true},
		{"s3:ObjectCreated:*", "s3:ObjectRemoved:Delete", false},
		{"s3:ObjectRemoved:*", "s3:ObjectRemoved:DeleteMarkerCreated", true},
		{"s3:ObjectCreated:Put, s3:ObjectRemoved:*", "s3:ObjectRemoved:Delete", true},
		{"s3:ObjectCreated:Put,s3:ObjectCreated:Copy", "s3:ObjectCreated:CompleteMultipartUpload", false},
		{"", "s3:ObjectCreated:Put", false},
		{"s3:ObjectCreated", "s3:ObjectCreated:Put", false}, // no wildcard → exact only
		{"s3:*", "s3:ObjectCreated:Put", true},
	}
	for _, c := range cases {
		if got := matchEvent(c.pattern, c.event); got != c.want {
			t.Errorf("matchEvent(%q, %q) = %v want %v", c.pattern, c.event, got, c.want)
		}
	}
}

type delivery struct {
	header http.Header
	body   []byte
}

func newDB(t *testing.T) *db.DB {
	t.Helper()
	database, err := db.Open(filepath.Join(t.TempDir(), "wh.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	for _, b := range []string{"hooked", "other"} {
		if _, err := database.CreateBucket(b, ""); err != nil {
			t.Fatal(err)
		}
	}
	return database
}

func TestDispatcher_Delivery(t *testing.T) {
	database := newDB(t)
	got := make(chan delivery, 10)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got <- delivery{header: r.Header.Clone(), body: b}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	const secret = "s3cr3t"
	if _, err := database.CreateWebhook("hooked", "my-hook", srv.URL+"/hook", "s3:ObjectCreated:*", secret); err != nil {
		t.Fatal(err)
	}
	// A hook for another bucket and a hook with a non-matching pattern must
	// not receive the event.
	if _, err := database.CreateWebhook("other", "other", srv.URL+"/other", "*", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := database.CreateWebhook("hooked", "removed-only", srv.URL+"/removed", "s3:ObjectRemoved:*", ""); err != nil {
		t.Fatal(err)
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	d := NewDispatcher(database, 2, logger)
	d.SetRegion("eu-central-1")
	ts := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	d.Emit(Event{
		BucketName: "hooked", EventType: "s3:ObjectCreated:Put", Key: "dir/a b+c&d.txt",
		Size: 42, ETag: "\"abc123\"", VersionID: "v-1", Timestamp: ts,
	})

	var dl delivery
	select {
	case dl = <-got:
	case <-time.After(5 * time.Second):
		t.Fatal("webhook not delivered")
	}
	d.Stop()

	// Headers.
	if dl.header.Get("Content-Type") != "application/json" || dl.header.Get("X-Cloodsy-Event") != "s3:ObjectCreated:Put" {
		t.Fatalf("headers %v", dl.header)
	}
	if id := dl.header.Get("X-Cloodsy-Delivery"); len(id) != 32 {
		t.Fatalf("X-Cloodsy-Delivery %q", id)
	}
	if dl.header.Get("X-Cloodsy-Timestamp") != "1789473600" {
		t.Fatalf("X-Cloodsy-Timestamp %q", dl.header.Get("X-Cloodsy-Timestamp"))
	}
	if ua := dl.header.Get("User-Agent"); !strings.HasPrefix(ua, "Cloodsy-S3-Webhook/") {
		t.Fatalf("User-Agent %q", ua)
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(dl.body)
	if want := "sha256=" + hex.EncodeToString(mac.Sum(nil)); dl.header.Get("X-Cloodsy-Signature") != want {
		t.Fatalf("X-Cloodsy-Signature %q want %q", dl.header.Get("X-Cloodsy-Signature"), want)
	}
	mac2 := hmac.New(sha256.New, []byte(secret))
	mac2.Write([]byte("1789473600." + string(dl.body)))
	if want := "t=1789473600,v1=" + hex.EncodeToString(mac2.Sum(nil)); dl.header.Get("X-Cloodsy-Signature-256") != want {
		t.Fatalf("X-Cloodsy-Signature-256 %q want %q", dl.header.Get("X-Cloodsy-Signature-256"), want)
	}

	// Payload.
	var payload struct {
		Records []struct {
			EventVersion string `json:"eventVersion"`
			EventSource  string `json:"eventSource"`
			AwsRegion    string `json:"awsRegion"`
			EventTime    string `json:"eventTime"`
			EventName    string `json:"eventName"`
			S3           struct {
				SchemaVersion   string `json:"s3SchemaVersion"`
				ConfigurationID string `json:"configurationId"`
				Bucket          struct {
					Name string `json:"name"`
					ARN  string `json:"arn"`
				} `json:"bucket"`
				Object struct {
					Key           string `json:"key"`
					URLEncodedKey string `json:"urlEncodedKey"`
					Size          int64  `json:"size"`
					ETag          string `json:"eTag"`
					VersionID     string `json:"versionId"`
					Sequencer     string `json:"sequencer"`
				} `json:"object"`
			} `json:"s3"`
		} `json:"Records"`
	}
	if err := json.Unmarshal(dl.body, &payload); err != nil {
		t.Fatalf("payload %v\n%s", err, dl.body)
	}
	if len(payload.Records) != 1 {
		t.Fatalf("records %d", len(payload.Records))
	}
	r := payload.Records[0]
	if r.EventVersion != "2.1" || r.EventSource != "cloodsy:s3" || r.AwsRegion != "eu-central-1" || r.EventName != "s3:ObjectCreated:Put" || r.EventTime != "2026-09-15T12:00:00Z" {
		t.Fatalf("record %+v", r)
	}
	if r.S3.ConfigurationID != "my-hook" || r.S3.Bucket.Name != "hooked" || r.S3.Bucket.ARN != "arn:aws:s3:::hooked" || r.S3.SchemaVersion != "1.0" {
		t.Fatalf("s3 %+v", r.S3)
	}
	o := r.S3.Object
	if o.Key != "dir/a b+c&d.txt" {
		t.Fatalf("raw key %q", o.Key)
	}
	if o.URLEncodedKey != "dir/a+b%2Bc%26d.txt" {
		t.Fatalf("urlEncodedKey %q", o.URLEncodedKey)
	}
	if o.Size != 42 || o.ETag != "abc123" || o.VersionID != "v-1" || len(o.Sequencer) != 16 {
		t.Fatalf("object %+v", o)
	}

	// Nothing else was delivered (other bucket / non-matching pattern).
	select {
	case extra := <-got:
		t.Fatalf("unexpected extra delivery: %v", extra.header)
	case <-time.After(200 * time.Millisecond):
	}
}

func TestDispatcher_NoSecretNoSignature(t *testing.T) {
	database := newDB(t)
	got := make(chan http.Header, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got <- r.Header.Clone()
	}))
	defer srv.Close()
	database.CreateWebhook("hooked", "h", srv.URL, "*", "")
	d := NewDispatcher(database, 1, slog.New(slog.NewTextHandler(io.Discard, nil)))
	d.Emit(Event{BucketName: "hooked", EventType: "s3:ObjectRemoved:Delete", Key: "k", Timestamp: time.Now()})
	select {
	case h := <-got:
		if h.Get("X-Cloodsy-Signature") != "" || h.Get("X-Cloodsy-Signature-256") != "" {
			t.Fatalf("signature present without secret: %v", h)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("not delivered")
	}
	d.Stop()
}

func TestDispatcher_RetriesOn5xx(t *testing.T) {
	database := newDB(t)
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	database.CreateWebhook("hooked", "h", srv.URL, "*", "")
	d := NewDispatcher(database, 1, slog.New(slog.NewTextHandler(io.Discard, nil)))
	d.Emit(Event{BucketName: "hooked", EventType: "s3:ObjectCreated:Put", Key: "k", Timestamp: time.Now()})
	d.Stop() // waits for the worker, which retries after 1s
	if n := atomic.LoadInt32(&calls); n != 2 {
		t.Fatalf("calls %d want 2 (one failure + one retry)", n)
	}
}

func TestDispatcher_DoesNotFollowRedirects(t *testing.T) {
	database := newDB(t)
	var target int32
	mux := http.NewServeMux()
	mux.HandleFunc("/hook", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/internal", http.StatusTemporaryRedirect)
	})
	mux.HandleFunc("/internal", func(w http.ResponseWriter, r *http.Request) { atomic.AddInt32(&target, 1) })
	srv := httptest.NewServer(mux)
	defer srv.Close()
	database.CreateWebhook("hooked", "h", srv.URL+"/hook", "*", "")
	d := NewDispatcher(database, 1, slog.New(slog.NewTextHandler(io.Discard, nil)))
	d.Emit(Event{BucketName: "hooked", EventType: "s3:ObjectCreated:Put", Key: "k", Timestamp: time.Now()})
	d.Stop()
	if atomic.LoadInt32(&target) != 0 {
		t.Fatal("dispatcher followed a redirect")
	}
}

func TestDispatcher_StopThenEmitIsSafe(t *testing.T) {
	database := newDB(t)
	d := NewDispatcher(database, 0, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if d.workers != 4 {
		t.Fatalf("default workers %d", d.workers)
	}
	d.Stop()
	d.Stop() // idempotent
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("Emit after Stop panicked: %v", r)
		}
	}()
	d.Emit(Event{BucketName: "hooked", EventType: "s3:ObjectCreated:Put", Key: "k", Timestamp: time.Now()})
}
