// Package acme provides automatic TLS certificates through the ACME protocol
// (Let's Encrypt by default) using golang.org/x/crypto/acme/autocert.
//
// One Manager serves every listener (S3, Admin API, WebDAV); certificates are
// obtained on the first TLS handshake for an allowed hostname, cached on disk
// and renewed automatically. The HTTP-01 challenge is answered on a plain
// HTTP listener (port 80 by default) which otherwise redirects to HTTPS; the
// TLS-ALPN-01 challenge is answered on the TLS listeners themselves.
package acme

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"golang.org/x/crypto/acme"
	"golang.org/x/crypto/acme/autocert"
)

// Config mirrors config.ACMEConfig without importing the config package.
type Config struct {
	Domains      []string
	Email        string
	AcceptTOS    bool
	CacheDir     string
	DirectoryURL string
	// HTTPListen is the HTTP-01 challenge / redirect listener ("" disables).
	HTTPListen string
	// RedirectTarget is the public HTTPS origin http:// requests are sent to,
	// e.g. "https://s3.example.com". Empty keeps the request host.
	RedirectTarget string
}

// Manager wraps autocert.Manager with the listener plumbing the server needs.
type Manager struct {
	m      *autocert.Manager
	cfg    Config
	logger *slog.Logger
	httpLn net.Listener
	httpSv *http.Server
}

// New builds the manager and prepares the cache directory (0700).
func New(cfg Config, logger *slog.Logger) (*Manager, error) {
	if !cfg.AcceptTOS {
		return nil, errors.New("acme: terms of service not accepted")
	}
	if len(cfg.Domains) == 0 {
		return nil, errors.New("acme: no domains configured")
	}
	if cfg.CacheDir == "" {
		return nil, errors.New("acme: cache dir is required")
	}
	if err := os.MkdirAll(cfg.CacheDir, 0700); err != nil {
		return nil, fmt.Errorf("acme: create cache dir: %w", err)
	}
	hosts := make([]string, 0, len(cfg.Domains))
	for _, d := range cfg.Domains {
		hosts = append(hosts, strings.ToLower(strings.TrimSpace(d)))
	}
	m := &autocert.Manager{
		Prompt:      autocert.AcceptTOS,
		Cache:       autocert.DirCache(cfg.CacheDir),
		HostPolicy:  autocert.HostWhitelist(hosts...),
		Email:       cfg.Email,
		RenewBefore: 30 * 24 * time.Hour,
	}
	if cfg.DirectoryURL != "" {
		m.Client = &acme.Client{DirectoryURL: cfg.DirectoryURL}
	}
	return &Manager{m: m, cfg: cfg, logger: logger}, nil
}

// TLSConfig returns a *tls.Config that obtains certificates on demand and
// answers TLS-ALPN-01 challenges. Safe to share across listeners.
func (mg *Manager) TLSConfig() *tls.Config {
	c := mg.m.TLSConfig()
	c.MinVersion = tls.VersionTLS12
	// Keep h2/http1.1 in front of the ACME ALPN protocol autocert adds.
	c.NextProtos = append([]string{"h2", "http/1.1"}, c.NextProtos...)
	return c
}

// StartHTTP opens the plain-HTTP listener for HTTP-01 challenges. Every other
// request is redirected to HTTPS with a permanent redirect. It is a no-op when
// HTTPListen is empty.
func (mg *Manager) StartHTTP() error {
	if mg.cfg.HTTPListen == "" {
		return nil
	}
	ln, err := net.Listen("tcp", mg.cfg.HTTPListen)
	if err != nil {
		return fmt.Errorf("acme: listen %s: %w", mg.cfg.HTTPListen, err)
	}
	redirect := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		target := mg.cfg.RedirectTarget
		if target == "" {
			host := r.Host
			if h, _, err := net.SplitHostPort(host); err == nil {
				host = h
			}
			target = "https://" + host
		}
		http.Redirect(w, r, strings.TrimSuffix(target, "/")+r.URL.RequestURI(), http.StatusMovedPermanently)
	})
	mg.httpLn = ln
	mg.httpSv = &http.Server{
		Handler:           mg.m.HTTPHandler(redirect),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    16 << 10,
	}
	go func() {
		mg.logger.Info("ACME HTTP-01 listener on " + mg.cfg.HTTPListen + " (redirects to HTTPS)")
		if err := mg.httpSv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			mg.logger.Error("acme http listener error", "error", err)
		}
	}()
	return nil
}

// Stop shuts the HTTP-01 listener down.
func (mg *Manager) Stop() {
	if mg.httpSv == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	mg.httpSv.Shutdown(ctx)
}

// Warm requests certificates for every configured domain so the first real
// client does not pay the issuance latency. autocert bounds each request to
// five minutes. Failures are logged, never fatal: issuance is retried on the
// next handshake. Returns once every domain has been attempted or ctx ends.
func (mg *Manager) Warm(ctx context.Context) {
	for _, d := range mg.cfg.Domains {
		if ctx.Err() != nil {
			return
		}
		host := strings.ToLower(strings.TrimSpace(d))
		if _, err := mg.m.GetCertificate(&tls.ClientHelloInfo{ServerName: host}); err != nil {
			mg.logger.Warn("acme: certificate not ready yet", "domain", host, "error", err)
			continue
		}
		mg.logger.Info("acme: certificate ready", "domain", host)
	}
}
