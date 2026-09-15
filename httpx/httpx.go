// Package httpx holds HTTP server middleware shared by the S3, Admin and
// WebDAV listeners.
package httpx

import (
	"bufio"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"runtime/debug"
	"strings"
	"time"
	"unicode"

	"golang.org/x/net/netutil"
)

// ProgressTimeout aborts a request only when no bytes have been transferred
// in either direction for idle. Unlike http.Server.ReadTimeout/WriteTimeout it
// never caps the total duration of a large upload or download.
func ProgressTimeout(next http.Handler, idle time.Duration) http.Handler {
	if idle <= 0 {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rc := http.NewResponseController(w)
		extend := func() {
			deadline := time.Now().Add(idle)
			// Errors mean the underlying connection does not support deadlines
			// (e.g. HTTP/2 streams); ignore.
			_ = rc.SetReadDeadline(deadline)
			_ = rc.SetWriteDeadline(deadline)
		}
		extend()
		if r.Body != nil && r.Body != http.NoBody {
			r.Body = &progressBody{ReadCloser: r.Body, extend: extend}
		}
		next.ServeHTTP(&progressWriter{ResponseWriter: w, extend: extend}, r)
	})
}

type progressBody struct {
	io.ReadCloser
	extend func()
}

func (b *progressBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if n > 0 {
		b.extend()
	}
	return n, err
}

type progressWriter struct {
	http.ResponseWriter
	extend func()
}

func (w *progressWriter) Write(p []byte) (int, error) {
	n, err := w.ResponseWriter.Write(p)
	if n > 0 {
		w.extend()
	}
	return n, err
}

func (w *progressWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (w *progressWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *progressWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if h, ok := w.ResponseWriter.(http.Hijacker); ok {
		return h.Hijack()
	}
	return nil, nil, errors.New("hijack not supported")
}

// Recover converts panics into 500 responses and logs the stack, so a bug in
// a background-style goroutine started by a handler cannot take the process
// down through an unhandled panic in the request path.
func Recover(next http.Handler, logger *slog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				if rec == http.ErrAbortHandler {
					panic(rec)
				}
				logger.Error("panic in request handler", "method", r.Method, "path", SanitizeLog(r.URL.Path), "panic", fmt.Sprint(rec), "stack", string(debug.Stack()))
				http.Error(w, "internal server error", http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// MaxBody caps request bodies (for JSON/XML APIs, not object uploads).
func MaxBody(next http.Handler, n int64) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Body != nil {
			r.Body = http.MaxBytesReader(w, r.Body, n)
		}
		next.ServeHTTP(w, r)
	})
}

// Listen opens a TCP listener, optionally TLS-wrapped and connection-capped.
func Listen(addr string, tlsCert, tlsKey string, maxConns int) (net.Listener, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	if maxConns > 0 {
		ln = netutil.LimitListener(ln, maxConns)
	}
	if tlsCert != "" && tlsKey != "" {
		cert, err := tls.LoadX509KeyPair(tlsCert, tlsKey)
		if err != nil {
			ln.Close()
			return nil, fmt.Errorf("load TLS key pair: %w", err)
		}
		ln = tls.NewListener(ln, &tls.Config{
			Certificates: []tls.Certificate{cert},
			MinVersion:   tls.VersionTLS12,
			NextProtos:   []string{"h2", "http/1.1"},
		})
	}
	return ln, nil
}

// SanitizeLog strips control characters (newlines, ANSI escapes) from values
// that originate from clients before they reach a text log.
func SanitizeLog(s string) string {
	if !strings.ContainsFunc(s, func(r rune) bool { return unicode.IsControl(r) }) {
		return s
	}
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return '?'
		}
		return r
	}, s)
}

// IsLoopbackAddr reports whether a listen address binds only to loopback.
func IsLoopbackAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
