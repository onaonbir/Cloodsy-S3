package httpx_test

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/onaonbir/Cloodsy-S3/httpx"
)

func TestSanitizeLog(t *testing.T) {
	cases := map[string]string{
		"plain/path":            "plain/path",
		"line1\nline2":          "line1?line2",
		"a\r\nb":                "a??b",
		"esc\x1b[31mred\x1b[0m": "esc?[31mred?[0m",
		"tab\there":             "tab?here",
		"nul\x00byte":           "nul?byte",
		"ünïcödé ok":            "ünïcödé ok",
		"":                      "",
		"\x7fdel":               "?del",
	}
	for in, want := range cases {
		if got := httpx.SanitizeLog(in); got != want {
			t.Errorf("SanitizeLog(%q) = %q want %q", in, got, want)
		}
	}
}

func TestIsLoopbackAddr(t *testing.T) {
	cases := map[string]bool{
		"127.0.0.1:9000":   true,
		"127.5.6.7:1":      true,
		"localhost:9001":   true,
		"[::1]:9000":       true,
		":9000":            false, // all interfaces
		"0.0.0.0:9000":     false,
		"192.168.1.10:9":   false,
		"[::]:9000":        false,
		"example.com:9000": false,
		"127.0.0.1":        false, // no port → unparsable
		"":                 false,
		"garbage":          false,
	}
	for in, want := range cases {
		if got := httpx.IsLoopbackAddr(in); got != want {
			t.Errorf("IsLoopbackAddr(%q) = %v want %v", in, got, want)
		}
	}
}

func TestMaxBody(t *testing.T) {
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, err := io.ReadAll(r.Body)
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			w.WriteHeader(http.StatusRequestEntityTooLarge)
			return
		}
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Write(b)
	})
	h := httpx.MaxBody(inner, 16)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"ok":true}`)))
	if rec.Code != http.StatusOK || rec.Body.String() != `{"ok":true}` {
		t.Fatalf("small body: %d %q", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"payload":"`+strings.Repeat("x", 100)+`"}`)))
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized body: %d", rec.Code)
	}
	// Exactly at the limit is fine.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/", strings.NewReader(strings.Repeat("y", 16))))
	if rec.Code != http.StatusOK {
		t.Fatalf("at-limit body: %d", rec.Code)
	}
	// GET without a body passes through.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("no body: %d", rec.Code)
	}
}

func TestRecover(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	calls := 0
	h := httpx.Recover(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path == "/boom" {
			panic("kaboom")
		}
		w.WriteHeader(http.StatusTeapot)
	}), logger)

	rec := httptest.NewRecorder()
	func() {
		defer func() {
			if rec := recover(); rec != nil {
				t.Fatalf("panic escaped Recover: %v", rec)
			}
		}()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/boom", nil))
	}()
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("panicking handler → %d want 500", rec.Code)
	}
	// The middleware keeps working for subsequent requests.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/fine", nil))
	if rec.Code != http.StatusTeapot || calls != 2 {
		t.Fatalf("after panic: %d calls=%d", rec.Code, calls)
	}
	// http.ErrAbortHandler is re-raised so net/http can abort the connection.
	abort := httpx.Recover(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic(http.ErrAbortHandler) }), logger)
	var got any
	func() {
		defer func() { got = recover() }()
		abort.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
	}()
	if got != http.ErrAbortHandler {
		t.Fatalf("ErrAbortHandler not propagated: %v", got)
	}

	// End-to-end: a real server survives a panicking handler.
	srv := httptest.NewServer(h)
	defer srv.Close()
	resp, err := srv.Client().Get(srv.URL + "/boom")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("live server: %d", resp.StatusCode)
	}
	resp, err = srv.Client().Get(srv.URL + "/fine")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusTeapot {
		t.Fatalf("live server after panic: %d", resp.StatusCode)
	}
}

func TestProgressTimeout_Disabled(t *testing.T) {
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) })
	if h := httpx.ProgressTimeout(inner, 0); h == nil {
		t.Fatal("nil handler")
	}
	rec := httptest.NewRecorder()
	httpx.ProgressTimeout(inner, -1).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("passthrough %d", rec.Code)
	}
}

// TestProgressTimeout_SlowButSteadyResponseSucceeds: a handler that keeps
// writing (one byte every 50ms for 500ms) must not be cut off by a 200ms idle
// timeout, even though the whole response takes longer than the idle window.
func TestProgressTimeout_SlowButSteadyResponseSucceeds(t *testing.T) {
	const ticks = 10
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		for i := 0; i < ticks; i++ {
			time.Sleep(50 * time.Millisecond)
			if _, err := w.Write([]byte("x")); err != nil {
				return
			}
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		}
	})
	srv := httptest.NewServer(httpx.ProgressTimeout(inner, 200*time.Millisecond))
	defer srv.Close()

	resp, err := srv.Client().Get(srv.URL)
	if err != nil {
		t.Fatalf("steady writer was cut off: %v", err)
	}
	b, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if resp.StatusCode != http.StatusOK || len(b) != ticks {
		t.Fatalf("status %d body %q", resp.StatusCode, b)
	}
}

// TestProgressTimeout_ServerSideStallCompletes documents that the idle
// timeout is about client transfer progress, not handler latency: a handler
// that computes for longer than idle and then writes a small response still
// delivers it (the buffered write refreshes the deadline before the flush).
// This is what keeps long CompleteMultipartUpload assemblies from being cut
// off. The request context, however, is cancelled once the client has been
// silent for idle.
func TestProgressTimeout_ServerSideStallCompletes(t *testing.T) {
	ctxErr := make(chan error, 1)
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(500 * time.Millisecond)
		ctxErr <- r.Context().Err()
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("late but complete"))
	})
	srv := httptest.NewServer(httpx.ProgressTimeout(inner, 100*time.Millisecond))
	defer srv.Close()

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(srv.URL)
	if err != nil {
		t.Fatalf("small late response not delivered: %v", err)
	}
	b, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil || resp.StatusCode != http.StatusOK || string(b) != "late but complete" {
		t.Fatalf("status %d body %q err %v", resp.StatusCode, b, err)
	}
	if e := <-ctxErr; e == nil {
		t.Log("request context was still live after the idle window (background read did not time out)")
	}
}

// TestProgressTimeout_SlowUploadClientAborted: a client that sends headers and
// part of the body, then goes silent, must have its body read fail after idle
// (not hang for the whole declared Content-Length).
func TestProgressTimeout_SlowUploadClientAborted(t *testing.T) {
	readErr := make(chan error, 1)
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, err := io.ReadAll(r.Body)
		readErr <- err
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	srv := httptest.NewServer(httpx.ProgressTimeout(inner, 150*time.Millisecond))
	defer srv.Close()

	conn, err := net.Dial("tcp", srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	fmt.Fprintf(conn, "POST / HTTP/1.1\r\nHost: test\r\nContent-Length: 1000\r\n\r\npartial-body")
	// Now stall: never send the remaining bytes.
	start := time.Now()
	select {
	case err := <-readErr:
		if err == nil {
			t.Fatal("body read succeeded although the client stalled")
		}
		if d := time.Since(start); d > 3*time.Second {
			t.Fatalf("read aborted only after %v", d)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("stalled upload was not aborted")
	}
	// The server answers (or closes) promptly instead of waiting for the body.
	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	buf := make([]byte, 64)
	n, rerr := conn.Read(buf)
	if rerr != nil && !errors.Is(rerr, io.EOF) && !strings.Contains(rerr.Error(), "reset") {
		t.Fatalf("read response: %v", rerr)
	}
	if n > 0 && !strings.HasPrefix(string(buf[:n]), "HTTP/1.1 400") {
		t.Fatalf("unexpected response %q", buf[:n])
	}
}

// TestProgressTimeout_SlowDownloadClientAborted: a client that stops reading a
// large response makes the server's socket writes block; after idle the write
// fails and the handler stops streaming.
func TestProgressTimeout_SlowDownloadClientAborted(t *testing.T) {
	writeErr := make(chan error, 1)
	chunk := make([]byte, 64<<10)
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.WriteHeader(http.StatusOK)
		deadline := time.Now().Add(8 * time.Second)
		for time.Now().Before(deadline) {
			if _, err := w.Write(chunk); err != nil {
				writeErr <- err
				return
			}
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		}
		writeErr <- nil
	})
	srv := httptest.NewServer(httpx.ProgressTimeout(inner, 150*time.Millisecond))
	defer srv.Close()

	conn, err := net.Dial("tcp", srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	fmt.Fprintf(conn, "GET / HTTP/1.1\r\nHost: test\r\n\r\n")
	// Read just the status line, then stop reading entirely.
	buf := make([]byte, 16)
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(buf), "HTTP/1.1 200") {
		t.Fatalf("status %q", buf)
	}
	start := time.Now()
	select {
	case err := <-writeErr:
		if err == nil {
			t.Fatal("handler streamed for the full duration; stalled client was never aborted")
		}
		if d := time.Since(start); d > 5*time.Second {
			t.Fatalf("write aborted only after %v", d)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("stalled download was not aborted")
	}
}

// TestProgressTimeout_SlowUploadKeepsAlive: request-body reads also extend the
// deadline, so a trickling upload longer than the idle window succeeds.
func TestProgressTimeout_SlowUploadKeepsAlive(t *testing.T) {
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, err := io.ReadAll(r.Body)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.Write([]byte(strings.ToUpper(string(b))))
	})
	srv := httptest.NewServer(httpx.ProgressTimeout(inner, 200*time.Millisecond))
	defer srv.Close()

	pr, pw := io.Pipe()
	go func() {
		for i := 0; i < 8; i++ {
			time.Sleep(60 * time.Millisecond)
			pw.Write([]byte("a"))
		}
		pw.Close()
	}()
	req, _ := http.NewRequest(http.MethodPost, srv.URL, pr)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("trickling upload cut off: %v", err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(b) != "AAAAAAAA" {
		t.Fatalf("status %d body %q", resp.StatusCode, b)
	}
}

func TestListen(t *testing.T) {
	ln, err := httpx.Listen("127.0.0.1:0", "", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	ln.Close()
	if _, err := httpx.Listen("127.0.0.1:0", "/nonexistent/cert.pem", "/nonexistent/key.pem", 0); err == nil {
		t.Fatal("bad TLS files accepted")
	}
	ln, err = httpx.Listen("127.0.0.1:0", "", "", 1)
	if err != nil {
		t.Fatal(err)
	}
	ln.Close()
}
