package logger

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"unicode"

	"github.com/pterm/pterm"
	"github.com/pterm/pterm/putils"
)

// PrettyHandler is a pterm-powered slog handler.
//
// Message and attribute values are sanitized before they reach the terminal:
// control characters (newlines, ANSI escape introducers, NUL, ...) are
// escaped, so a client-supplied string can neither forge log lines nor
// re-style the terminal. WithAttrs/WithGroup follow the slog contract, so
// logger.With(...) attributes and groups are rendered.
type PrettyHandler struct {
	w      io.Writer
	level  slog.Level
	mu     *sync.Mutex // shared by every handler derived from the same root
	attrs  []slog.Attr // pre-formatted attributes from WithAttrs (already group-qualified)
	groups []string    // open groups from WithGroup
}

func NewPrettyHandler(w io.Writer, level slog.Level) *PrettyHandler {
	return &PrettyHandler{w: w, level: level, mu: &sync.Mutex{}}
}

func (h *PrettyHandler) Enabled(_ context.Context, level slog.Level) bool {
	return level >= h.level
}

func (h *PrettyHandler) Handle(_ context.Context, r slog.Record) error {
	timeStr := pterm.Gray(r.Time.Format("15:04:05"))

	var prefix string
	switch {
	case r.Level >= slog.LevelError:
		prefix = pterm.Red("✖")
	case r.Level >= slog.LevelWarn:
		prefix = pterm.Yellow("▲")
	case r.Level >= slog.LevelInfo:
		prefix = pterm.Blue("●")
	default:
		prefix = pterm.Gray("○")
	}

	var msgColor func(a ...interface{}) string
	switch {
	case r.Level >= slog.LevelError:
		msgColor = pterm.Red
	case r.Level >= slog.LevelWarn:
		msgColor = pterm.Yellow
	case r.Level >= slog.LevelInfo:
		msgColor = pterm.White
	default:
		msgColor = pterm.Gray
	}

	var sb strings.Builder
	sb.WriteString(timeStr)
	sb.WriteByte(' ')
	sb.WriteString(prefix)
	sb.WriteByte(' ')
	sb.WriteString(msgColor(Sanitize(r.Message)))

	// Attributes fixed by WithAttrs come first, then the record's own.
	for _, a := range h.attrs {
		appendAttr(&sb, "", a)
	}
	groupPrefix := strings.Join(h.groups, ".")
	if groupPrefix != "" {
		groupPrefix += "."
	}
	r.Attrs(func(a slog.Attr) bool {
		appendAttr(&sb, groupPrefix, a)
		return true
	})

	h.mu.Lock()
	defer h.mu.Unlock()
	_, err := fmt.Fprintln(h.w, sb.String())
	return err
}

// appendAttr renders one attribute (flattening groups) onto sb.
func appendAttr(sb *strings.Builder, prefix string, a slog.Attr) {
	a.Value = a.Value.Resolve()
	if a.Equal(slog.Attr{}) {
		return // slog convention: zero Attr is ignored
	}
	if a.Value.Kind() == slog.KindGroup {
		attrs := a.Value.Group()
		if len(attrs) == 0 {
			return
		}
		p := prefix
		if a.Key != "" {
			p = prefix + a.Key + "."
		}
		for _, ga := range attrs {
			appendAttr(sb, p, ga)
		}
		return
	}
	key := Sanitize(prefix + a.Key)
	val := formatValue(a.Value)
	sb.WriteString(pterm.Gray(" " + key + "="))
	sb.WriteString(pterm.White(val))
}

// formatValue renders a value for the terminal. Strings with spaces or
// control characters are quoted so fields stay unambiguous; zero values are
// printed (an "0"/"false" is legitimate information, not noise).
func formatValue(v slog.Value) string {
	switch v.Kind() {
	case slog.KindString:
		return quoteIfNeeded(v.String())
	case slog.KindAny:
		if err, ok := v.Any().(error); ok {
			return quoteIfNeeded(err.Error())
		}
		return quoteIfNeeded(fmt.Sprint(v.Any()))
	default:
		return Sanitize(v.String())
	}
}

func quoteIfNeeded(s string) string {
	if s == "" {
		return `""`
	}
	needsQuote := false
	for _, r := range s {
		if unicode.IsControl(r) || r == '"' || r == '\\' || unicode.IsSpace(r) || r == unicode.ReplacementChar {
			needsQuote = true
			break
		}
	}
	if !needsQuote {
		return s
	}
	return strconv.Quote(s)
}

// Sanitize escapes control characters (including newlines and ESC) so a
// string can be written to a terminal log as a single, inert line.
func Sanitize(s string) string {
	if !strings.ContainsFunc(s, unicode.IsControl) {
		return s
	}
	var sb strings.Builder
	sb.Grow(len(s) + 8)
	for _, r := range s {
		if unicode.IsControl(r) {
			q := strconv.QuoteRune(r) // e.g. '\n', '\x1b'
			sb.WriteString(q[1 : len(q)-1])
			continue
		}
		sb.WriteRune(r)
	}
	return sb.String()
}

// WithAttrs returns a handler that always logs attrs (qualified by the open
// groups) in addition to each record's own attributes.
func (h *PrettyHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	if len(attrs) == 0 {
		return h
	}
	n := h.clone()
	prefix := strings.Join(h.groups, ".")
	if prefix != "" {
		prefix += "."
	}
	for _, a := range attrs {
		a.Value = a.Value.Resolve()
		if a.Equal(slog.Attr{}) {
			continue
		}
		if prefix != "" && a.Value.Kind() != slog.KindGroup {
			a.Key = prefix + a.Key
		} else if prefix != "" {
			// Wrap the group so its members get the qualified path.
			a = slog.Attr{Key: strings.TrimSuffix(prefix, ".") + "." + a.Key, Value: a.Value}
		}
		n.attrs = append(n.attrs, a)
	}
	return n
}

// WithGroup returns a handler that qualifies subsequent attribute keys with
// name (rendered as "name.key").
func (h *PrettyHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	n := h.clone()
	n.groups = append(n.groups, name)
	return n
}

func (h *PrettyHandler) clone() *PrettyHandler {
	n := &PrettyHandler{
		w:      h.w,
		level:  h.level,
		mu:     h.mu,
		attrs:  make([]slog.Attr, len(h.attrs), len(h.attrs)+4),
		groups: make([]string, len(h.groups), len(h.groups)+1),
	}
	copy(n.attrs, h.attrs)
	copy(n.groups, h.groups)
	return n
}

// Banner prints the startup banner with server info.
func Banner(version, commit, listen, adminListen, region, storageDir, dbPath string, tlsEnabled, adminEnabled bool) {
	// Big text header
	s, _ := pterm.DefaultBigText.WithLetters(
		putils.LettersFromStringWithStyle("Cloodsy", pterm.NewStyle(pterm.FgCyan, pterm.Bold)),
		putils.LettersFromStringWithStyle(" - ", pterm.NewStyle(pterm.FgGray, pterm.Bold)),
		putils.LettersFromStringWithStyle("S3", pterm.NewStyle(pterm.FgWhite, pterm.Bold)),
	).Srender()
	pterm.Println(s)

	// Version line
	versionStr := pterm.Bold.Sprintf("Cloodsy S3") + " " + pterm.Green("v"+version)
	if commit != "unknown" {
		versionStr += pterm.Gray(" (" + commit + ")")
	}
	pterm.Println("  " + versionStr)
	pterm.Println()

	// Info table
	scheme := "http"
	if tlsEnabled {
		scheme = "https"
	}
	s3Addr := formatAddr(listen, scheme)

	tableData := [][]string{
		{"S3 API", s3Addr},
		{"Region", region},
		{"Storage", storageDir},
		{"Database", dbPath},
		{"Platform", runtime.GOOS + "/" + runtime.GOARCH},
	}

	if adminEnabled {
		// Insert after S3 API
		adminAddr := formatAddr(adminListen, "http")
		tableData = append([][]string{tableData[0], {"Admin API", adminAddr}}, tableData[1:]...)
	}

	if tlsEnabled {
		tableData = append(tableData, []string{"TLS", pterm.Green("enabled")})
	} else {
		tableData = append(tableData, []string{"TLS", pterm.Yellow("disabled")})
	}

	for _, row := range tableData {
		pterm.Printf("  %s  %s\n", pterm.Gray(padRight(row[0], 12)), pterm.White(row[1]))
	}

	pterm.Println()
	pterm.Println("  " + pterm.Cyan("cloodsy.com") + pterm.Gray(" • ") + pterm.Cyan("onaonbir.com"))
	pterm.Println()
}

func formatAddr(listen, scheme string) string {
	if len(listen) > 0 && listen[0] == ':' {
		return fmt.Sprintf("%s://0.0.0.0%s", scheme, listen)
	}
	return fmt.Sprintf("%s://%s", scheme, listen)
}

func padRight(s string, length int) string {
	if len(s) >= length {
		return s
	}
	return s + strings.Repeat(" ", length-len(s))
}
