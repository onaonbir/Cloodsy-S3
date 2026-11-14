package logger

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"
)

// RingSize is the number of records kept by the default ring.
const RingSize = 2000

// Entry is one captured log record.
type Entry struct {
	ID      int64
	Time    time.Time
	Level   slog.Level
	Message string
	Attrs   map[string]string
}

// Ring is a fixed-size, concurrency-safe buffer of the most recent log
// records. IDs are monotonic so readers can poll with "since".
type Ring struct {
	mu      sync.Mutex
	entries []Entry
	next    int   // write position
	full    bool  // the buffer has wrapped
	lastID  int64 // id of the newest entry
	maxSize int
}

// NewRing creates a ring holding up to size entries.
func NewRing(size int) *Ring {
	if size <= 0 {
		size = RingSize
	}
	return &Ring{entries: make([]Entry, size), maxSize: size}
}

// Default is the process-wide ring fed by Tee in the server's logger.
var Default = NewRing(RingSize)

func (r *Ring) add(e Entry) {
	r.mu.Lock()
	r.lastID++
	e.ID = r.lastID
	r.entries[r.next] = e
	r.next++
	if r.next == r.maxSize {
		r.next = 0
		r.full = true
	}
	r.mu.Unlock()
}

// Query returns entries with ID > sinceID and level >= minLevel whose message
// or attributes contain q (case-insensitive), oldest first, at most limit
// (the newest matches are kept when more exist). It also returns the newest
// ID in the buffer.
func (r *Ring) Query(sinceID int64, minLevel slog.Level, q string, limit int) ([]Entry, int64) {
	q = strings.ToLower(q)
	r.mu.Lock()
	var snap []Entry
	if r.full {
		snap = make([]Entry, 0, r.maxSize)
		snap = append(snap, r.entries[r.next:]...)
		snap = append(snap, r.entries[:r.next]...)
	} else {
		snap = append([]Entry(nil), r.entries[:r.next]...)
	}
	last := r.lastID
	r.mu.Unlock()

	out := make([]Entry, 0, 16)
	for _, e := range snap {
		if e.ID <= sinceID || e.Level < minLevel {
			continue
		}
		if q != "" && !entryContains(e, q) {
			continue
		}
		out = append(out, e)
	}
	if limit > 0 && len(out) > limit {
		out = out[len(out)-limit:]
	}
	return out, last
}

func entryContains(e Entry, lowerQ string) bool {
	if strings.Contains(strings.ToLower(e.Message), lowerQ) {
		return true
	}
	for k, v := range e.Attrs {
		if strings.Contains(strings.ToLower(k), lowerQ) || strings.Contains(strings.ToLower(v), lowerQ) {
			return true
		}
	}
	return false
}

// TeeHandler forwards every record to the wrapped handler unchanged and also
// copies it (flattened, sanitized) into a Ring.
type TeeHandler struct {
	next   slog.Handler
	ring   *Ring
	attrs  []slog.Attr // already group-qualified keys
	prefix string      // open groups, "a.b."
}

// Tee wraps next so its records are also captured in ring.
func Tee(next slog.Handler, ring *Ring) slog.Handler {
	return &TeeHandler{next: next, ring: ring}
}

func (t *TeeHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return t.next.Enabled(ctx, l)
}

func (t *TeeHandler) Handle(ctx context.Context, rec slog.Record) error {
	attrs := make(map[string]string, len(t.attrs)+rec.NumAttrs())
	for _, a := range t.attrs {
		flattenAttr(attrs, "", a)
	}
	rec.Attrs(func(a slog.Attr) bool {
		flattenAttr(attrs, t.prefix, a)
		return true
	})
	t.ring.add(Entry{Time: rec.Time, Level: rec.Level, Message: Sanitize(truncateValue(rec.Message)), Attrs: attrs})
	return t.next.Handle(ctx, rec)
}

func flattenAttr(dst map[string]string, prefix string, a slog.Attr) {
	a.Value = a.Value.Resolve()
	if a.Equal(slog.Attr{}) {
		return
	}
	if a.Value.Kind() == slog.KindGroup {
		p := prefix
		if a.Key != "" {
			p = prefix + a.Key + "."
		}
		for _, ga := range a.Value.Group() {
			flattenAttr(dst, p, ga)
		}
		return
	}
	var v string
	switch a.Value.Kind() {
	case slog.KindString:
		v = a.Value.String()
	case slog.KindAny:
		if err, ok := a.Value.Any().(error); ok {
			v = err.Error()
		} else {
			v = fmt.Sprint(a.Value.Any())
		}
	default:
		v = a.Value.String()
	}
	dst[Sanitize(prefix+a.Key)] = Sanitize(truncateValue(v))
}

// maxValueLen caps the message and every attribute value kept in the ring
// (bytes, before sanitizing), so one huge log call cannot pin megabytes of
// memory per entry.
const maxValueLen = 4096

func truncateValue(v string) string {
	if len(v) <= maxValueLen {
		return v
	}
	return v[:maxValueLen] + "…"
}

func (t *TeeHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	if len(attrs) == 0 {
		return t
	}
	n := &TeeHandler{next: t.next.WithAttrs(attrs), ring: t.ring, prefix: t.prefix}
	n.attrs = append(append([]slog.Attr(nil), t.attrs...), qualify(attrs, t.prefix)...)
	return n
}

func qualify(attrs []slog.Attr, prefix string) []slog.Attr {
	if prefix == "" {
		return attrs
	}
	out := make([]slog.Attr, 0, len(attrs))
	for _, a := range attrs {
		a.Key = prefix + a.Key
		out = append(out, a)
	}
	return out
}

func (t *TeeHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return t
	}
	return &TeeHandler{next: t.next.WithGroup(name), ring: t.ring, attrs: t.attrs, prefix: t.prefix + name + "."}
}

// LevelName renders a level as DEBUG/INFO/WARN/ERROR.
func LevelName(l slog.Level) string {
	switch {
	case l >= slog.LevelError:
		return "ERROR"
	case l >= slog.LevelWarn:
		return "WARN"
	case l >= slog.LevelInfo:
		return "INFO"
	default:
		return "DEBUG"
	}
}

// ParseLevel maps DEBUG/INFO/WARN/ERROR (any case, "" = DEBUG) to a level.
func ParseLevel(s string) (slog.Level, bool) {
	switch strings.ToUpper(strings.TrimSpace(s)) {
	case "", "DEBUG":
		return slog.LevelDebug, true
	case "INFO":
		return slog.LevelInfo, true
	case "WARN", "WARNING":
		return slog.LevelWarn, true
	case "ERROR":
		return slog.LevelError, true
	}
	return 0, false
}
