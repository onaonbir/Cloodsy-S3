// Package metrics keeps cheap, lock-free request counters for the HTTP
// listeners (totals plus a per-minute ring for the last hour).
package metrics

import (
	"bufio"
	"errors"
	"io"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

// Methods counted individually; anything else is reported as "OTHER".
var methods = [...]string{"GET", "PUT", "POST", "DELETE", "HEAD", "OPTIONS", "OTHER"}

func methodIndex(m string) int {
	for i, x := range methods[:len(methods)-1] {
		if m == x {
			return i
		}
	}
	return len(methods) - 1
}

// SeriesLen is the number of per-minute slots kept.
const SeriesLen = 60

type slot struct {
	minute   atomic.Int64 // unix minute this slot currently holds
	requests atomic.Int64
	errors   atomic.Int64
	bytesIn  atomic.Int64
	bytesOut atomic.Int64
}

// Registry accumulates counters for one listener.
type Registry struct {
	since    time.Time
	requests atomic.Int64
	err4xx   atomic.Int64
	err5xx   atomic.Int64
	bytesIn  atomic.Int64
	bytesOut atomic.Int64
	byMethod [len(methods)]atomic.Int64

	rollMu sync.Mutex
	series [SeriesLen]slot
	now    func() time.Time
}

// New creates an empty registry.
func New() *Registry {
	return &Registry{since: time.Now().UTC(), now: time.Now}
}

// Process-wide registries for the S3 and Admin listeners.
var (
	S3    = New()
	Admin = New()
)

func (r *Registry) slotFor(minute int64) *slot {
	s := &r.series[minute%SeriesLen]
	if s.minute.Load() != minute {
		r.rollMu.Lock()
		if s.minute.Load() != minute {
			s.requests.Store(0)
			s.errors.Store(0)
			s.bytesIn.Store(0)
			s.bytesOut.Store(0)
			s.minute.Store(minute)
		}
		r.rollMu.Unlock()
	}
	return s
}

// Record adds one finished request.
func (r *Registry) Record(method string, status int, in, out int64) {
	r.requests.Add(1)
	r.byMethod[methodIndex(method)].Add(1)
	r.bytesIn.Add(in)
	r.bytesOut.Add(out)
	isErr := false
	switch {
	case status >= 500:
		r.err5xx.Add(1)
		isErr = true
	case status >= 400:
		r.err4xx.Add(1)
		isErr = true
	}
	s := r.slotFor(r.now().Unix() / 60)
	s.requests.Add(1)
	if isErr {
		s.errors.Add(1)
	}
	s.bytesIn.Add(in)
	s.bytesOut.Add(out)
}

// Point is one minute of the series.
type Point struct {
	Time     time.Time
	Requests int64
	Errors   int64
	BytesIn  int64
	BytesOut int64
}

// Snapshot is a consistent-enough copy of the counters.
type Snapshot struct {
	Since    time.Time
	Requests int64
	Err4xx   int64
	Err5xx   int64
	BytesIn  int64
	BytesOut int64
	ByMethod map[string]int64
	Series   []Point // oldest first, exactly SeriesLen minutes ending now
}

// Snapshot reads the counters.
func (r *Registry) Snapshot() Snapshot {
	s := Snapshot{
		Since:    r.since,
		Requests: r.requests.Load(),
		Err4xx:   r.err4xx.Load(),
		Err5xx:   r.err5xx.Load(),
		BytesIn:  r.bytesIn.Load(),
		BytesOut: r.bytesOut.Load(),
		ByMethod: make(map[string]int64, len(methods)),
		Series:   make([]Point, 0, SeriesLen),
	}
	for i, m := range methods {
		s.ByMethod[m] = r.byMethod[i].Load()
	}
	cur := r.now().Unix() / 60
	for m := cur - SeriesLen + 1; m <= cur; m++ {
		p := Point{Time: time.Unix(m*60, 0).UTC()}
		sl := &r.series[m%SeriesLen]
		if sl.minute.Load() == m {
			p.Requests = sl.requests.Load()
			p.Errors = sl.errors.Load()
			p.BytesIn = sl.bytesIn.Load()
			p.BytesOut = sl.bytesOut.Load()
		}
		s.Series = append(s.Series, p)
	}
	return s
}

// Middleware counts every request served by next: method, final status and
// request/response body bytes. The wrapped writer keeps http.Flusher,
// http.Hijacker and Unwrap (for http.ResponseController).
func Middleware(next http.Handler, reg *Registry) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cw := &countingWriter{ResponseWriter: w}
		var cb *countingBody
		if r.Body != nil && r.Body != http.NoBody {
			cb = &countingBody{ReadCloser: r.Body}
			r.Body = cb
		}
		defer func() {
			status := cw.status
			if status == 0 {
				status = http.StatusOK
			}
			if rec := recover(); rec != nil {
				if status < 500 && !cw.wrote {
					status = http.StatusInternalServerError
				}
				reg.Record(r.Method, status, cb.count(), cw.n)
				panic(rec)
			}
			reg.Record(r.Method, status, cb.count(), cw.n)
		}()
		next.ServeHTTP(cw, r)
	})
}

type countingBody struct {
	io.ReadCloser
	n atomic.Int64
}

func (b *countingBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if n > 0 {
		b.n.Add(int64(n))
	}
	return n, err
}

func (b *countingBody) count() int64 {
	if b == nil {
		return 0
	}
	return b.n.Load()
}

type countingWriter struct {
	http.ResponseWriter
	status int
	wrote  bool
	n      int64
}

func (w *countingWriter) WriteHeader(code int) {
	if !w.wrote && code >= 200 {
		w.status = code
		w.wrote = true
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *countingWriter) Write(p []byte) (int, error) {
	if !w.wrote {
		w.status = http.StatusOK
		w.wrote = true
	}
	n, err := w.ResponseWriter.Write(p)
	w.n += int64(n)
	return n, err
}

func (w *countingWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (w *countingWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *countingWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if h, ok := w.ResponseWriter.(http.Hijacker); ok {
		return h.Hijack()
	}
	return nil, nil, errors.New("hijack not supported")
}
