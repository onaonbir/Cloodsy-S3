package admin

import (
	"net/http"
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/onaonbir/Cloodsy-S3/httpx"
	applogger "github.com/onaonbir/Cloodsy-S3/logger"
	"github.com/onaonbir/Cloodsy-S3/metrics"
	"gopkg.in/yaml.v3"
)

const isoTime = "2006-01-02T15:04:05Z"

func fmtTime(t time.Time) string { return t.UTC().Format(isoTime) }

// --- status additions --------------------------------------------------------

func (h *Handler) statusS3() map[string]interface{} {
	c := h.Config
	domains := append([]string{}, c.Server.VirtualHostDomains...)
	return map[string]interface{}{
		"listen":               c.Server.Listen,
		"tls":                  c.Server.TLS.Enabled,
		"region":               c.Server.Region,
		"virtual_host_domains": domains,
	}
}

func statusHost() map[string]interface{} {
	hostname, _ := os.Hostname()
	return map[string]interface{}{
		"os":         runtime.GOOS,
		"hostname":   hostname,
		"wsl_distro": os.Getenv("WSL_DISTRO_NAME"),
	}
}

// --- sessions ----------------------------------------------------------------

// handleListSessions lists every live admin session (all users).
// GET /admin/sessions
func (h *Handler) handleListSessions(w http.ResponseWriter, r *http.Request, currentToken string) {
	now := time.Now()
	h.mu.RLock()
	list := make([]session, 0, len(h.sessions))
	currentID := ""
	for tok, s := range h.sessions {
		if now.After(s.ExpiresAt) {
			continue
		}
		if tok == currentToken {
			currentID = s.ID
		}
		list = append(list, s)
	}
	h.mu.RUnlock()

	sort.Slice(list, func(i, j int) bool { return list[i].CreatedAt.After(list[j].CreatedAt) })
	out := make([]map[string]interface{}, 0, len(list))
	for _, s := range list {
		out = append(out, map[string]interface{}{
			"id":           s.ID,
			"username":     s.Username,
			"created_at":   fmtTime(s.CreatedAt),
			"last_seen_at": fmtTime(s.LastSeenAt),
			"expires_at":   fmtTime(s.ExpiresAt),
			"ip":           s.IP,
			"user_agent":   s.UserAgent,
			"current":      s.ID == currentID,
		})
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"sessions": out})
}

// handleDeleteSession revokes one session by its public id. Revoking the
// caller's own session is a logout.
// DELETE /admin/sessions/{id}
func (h *Handler) handleDeleteSession(w http.ResponseWriter, r *http.Request, id, currentToken, actor string) {
	if id == "" || len(id) > 64 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid session id"})
		return
	}
	var owner string
	found, current := false, false
	h.mu.Lock()
	for tok, s := range h.sessions {
		if s.ID == id {
			owner = s.Username
			current = tok == currentToken
			delete(h.sessions, tok)
			found = true
			break
		}
	}
	h.mu.Unlock()
	if !found {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "session not found"})
		return
	}
	h.Logger.Info("admin session revoked", "id", id, "username", httpx.SanitizeLog(owner), "by", httpx.SanitizeLog(actor), "current", current)
	writeJSON(w, http.StatusOK, map[string]interface{}{"message": "session revoked", "current": current})
}

// --- logs --------------------------------------------------------------------

const (
	logsDefaultLimit = 200
	logsMaxLimit     = 2000
)

// handleLogs returns recent server log records from the in-memory ring.
// GET /admin/logs?since_id=&level=&limit=&q=
func (h *Handler) handleLogs(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	var since int64
	if v := q.Get("since_id"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid since_id"})
			return
		}
		since = n
	}
	level, ok := applogger.ParseLevel(q.Get("level"))
	if !ok {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid level (DEBUG, INFO, WARN, ERROR)"})
		return
	}
	limit := logsDefaultLimit
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid limit"})
			return
		}
		if n > logsMaxLimit {
			n = logsMaxLimit
		}
		limit = n
	}
	search := q.Get("q")
	if len(search) > 256 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "q too long"})
		return
	}

	ring := h.LogRing
	if ring == nil {
		ring = applogger.Default
	}
	entries, last := ring.Query(since, level, search, limit)
	out := make([]map[string]interface{}, 0, len(entries))
	for _, e := range entries {
		attrs := e.Attrs
		if attrs == nil {
			attrs = map[string]string{}
		}
		out = append(out, map[string]interface{}{
			"id":      e.ID,
			"time":    fmtTime(e.Time),
			"level":   applogger.LevelName(e.Level),
			"message": e.Message,
			"attrs":   attrs,
		})
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"entries": out, "last_id": last})
}

// --- config ------------------------------------------------------------------

const maskedValue = "••••"

// secretField reports whether a config key holds secret material. File
// paths (cert_file, key_file, ...) are not secrets.
func secretField(key string) bool {
	k := strings.ToLower(key)
	if strings.HasSuffix(k, "_file") || strings.HasSuffix(k, "_dir") || strings.HasSuffix(k, "_path") {
		return false
	}
	for _, s := range []string{"password", "secret", "token", "private_key", "key_material", "access_key", "credential"} {
		if strings.Contains(k, s) {
			return true
		}
	}
	return k == "key" || strings.HasSuffix(k, "_key")
}

func maskSecrets(v interface{}) interface{} {
	switch t := v.(type) {
	case map[string]interface{}:
		for k, val := range t {
			if secretField(k) {
				if val != nil && val != "" {
					t[k] = maskedValue
				}
				continue
			}
			t[k] = maskSecrets(val)
		}
		return t
	case []interface{}:
		for i := range t {
			t[i] = maskSecrets(t[i])
		}
		return t
	default:
		return v
	}
}

// handleConfig returns the effective configuration (YAML field names), with
// anything secret masked. Admin passwords and bucket credentials live in the
// database, never in this document.
// GET /admin/config
func (h *Handler) handleConfig(w http.ResponseWriter, r *http.Request) {
	raw, err := yaml.Marshal(h.Config)
	if err != nil {
		h.Logger.Error("config marshal error", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	var doc map[string]interface{}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		h.Logger.Error("config unmarshal error", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	if doc == nil {
		doc = map[string]interface{}{}
	}
	writeJSON(w, http.StatusOK, maskSecrets(doc))
}

// --- metrics -----------------------------------------------------------------

func metricsJSON(s metrics.Snapshot) map[string]interface{} {
	return map[string]interface{}{
		"requests":   s.Requests,
		"errors_4xx": s.Err4xx,
		"errors_5xx": s.Err5xx,
		"bytes_in":   s.BytesIn,
		"bytes_out":  s.BytesOut,
		"by_method":  s.ByMethod,
	}
}

// handleMetrics reports S3 listener counters plus a per-minute series for
// the last hour. The admin listener's own counters are under "admin".
// GET /admin/metrics
func (h *Handler) handleMetrics(w http.ResponseWriter, r *http.Request) {
	s3 := metrics.S3.Snapshot()
	series := make([]map[string]interface{}, 0, len(s3.Series))
	for _, p := range s3.Series {
		series = append(series, map[string]interface{}{
			"time":      fmtTime(p.Time),
			"requests":  p.Requests,
			"errors":    p.Errors,
			"bytes_in":  p.BytesIn,
			"bytes_out": p.BytesOut,
		})
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"since":  fmtTime(s3.Since),
		"s3":     metricsJSON(s3),
		"series": series,
		"admin":  metricsJSON(metrics.Admin.Snapshot()),
	})
}
