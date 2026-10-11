package acme

import (
	"context"
	"crypto/tls"
	"io"
	"log/slog"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func freePort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	return addr
}

func TestNewValidation(t *testing.T) {
	dir := t.TempDir()
	cases := map[string]Config{
		"no tos":     {Domains: []string{"a.example.com"}, CacheDir: dir},
		"no domains": {AcceptTOS: true, CacheDir: dir},
		"no cache":   {AcceptTOS: true, Domains: []string{"a.example.com"}},
	}
	for name, c := range cases {
		if _, err := New(c, testLogger()); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
	m, err := New(Config{AcceptTOS: true, Domains: []string{"A.Example.com"}, CacheDir: filepath.Join(dir, "acme")}, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	tc := m.TLSConfig()
	if tc.GetCertificate == nil {
		t.Fatal("GetCertificate must be set")
	}
	if tc.MinVersion != tls.VersionTLS12 {
		t.Fatalf("min version %x", tc.MinVersion)
	}
	if len(tc.NextProtos) < 3 || tc.NextProtos[0] != "h2" || tc.NextProtos[1] != "http/1.1" {
		t.Fatalf("NextProtos %v", tc.NextProtos)
	}
	hasALPN := false
	for _, p := range tc.NextProtos {
		if p == "acme-tls/1" {
			hasALPN = true
		}
	}
	if !hasALPN {
		t.Fatalf("acme-tls/1 ALPN missing: %v", tc.NextProtos)
	}
}

func TestHostPolicyRejectsUnknownDomain(t *testing.T) {
	m, err := New(Config{AcceptTOS: true, Domains: []string{"ok.example.com"}, CacheDir: t.TempDir(), DirectoryURL: "https://127.0.0.1:1/directory"}, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	// Unknown host must be refused by the host policy before any CA contact.
	_, err = m.TLSConfig().GetCertificate(&tls.ClientHelloInfo{ServerName: "evil.example.com"})
	if err == nil || !strings.Contains(err.Error(), "not configured") {
		t.Fatalf("expected host policy rejection, got %v", err)
	}
}

func TestHTTPListenerRedirectsAndServesChallengePath(t *testing.T) {
	addr := freePort(t)
	m, err := New(Config{AcceptTOS: true, Domains: []string{"s3.example.com"}, CacheDir: t.TempDir(), HTTPListen: addr, RedirectTarget: "https://s3.example.com:9000"}, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	if err := m.StartHTTP(); err != nil {
		t.Fatal(err)
	}
	defer m.Stop()

	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }, Timeout: 5 * time.Second}
	resp, err := client.Get("http://" + addr + "/bucket/key?x=1")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMovedPermanently {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != "https://s3.example.com:9000/bucket/key?x=1" {
		t.Fatalf("location %q", loc)
	}

	// The challenge path is handled by autocert (404 for an unknown token,
	// never a redirect).
	resp, err = client.Get("http://" + addr + "/.well-known/acme-challenge/unknown-token")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode == http.StatusMovedPermanently {
		t.Fatal("challenge path must not redirect")
	}
}

func TestHTTPListenerDisabled(t *testing.T) {
	m, err := New(Config{AcceptTOS: true, Domains: []string{"s3.example.com"}, CacheDir: t.TempDir()}, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	if err := m.StartHTTP(); err != nil {
		t.Fatal(err)
	}
	m.Stop() // no-op without a listener
}

func TestWarmDoesNotBlockOnCancelledContext(t *testing.T) {
	m, err := New(Config{AcceptTOS: true, Domains: []string{"s3.example.com"}, CacheDir: t.TempDir(), DirectoryURL: "https://127.0.0.1:1/directory"}, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	done := make(chan struct{})
	go func() { m.Warm(ctx); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Warm did not return promptly on cancelled context")
	}
}
