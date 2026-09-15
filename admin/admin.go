package admin

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/onaonbir/Cloodsy-S3/config"
	"github.com/onaonbir/Cloodsy-S3/db"
	"github.com/onaonbir/Cloodsy-S3/httpx"
	"github.com/onaonbir/Cloodsy-S3/service"
	"github.com/onaonbir/Cloodsy-S3/storage"
	"golang.org/x/crypto/bcrypt"
)

const (
	defaultSessionTTL = 8 * time.Hour

	maxLoginAttempts  = 5
	loginLockDuration = 15 * time.Minute
	// maxLoginEntries caps the size of the failed-login table so an attacker
	// rotating source addresses or usernames cannot grow it without bound.
	maxLoginEntries = 10000

	minPasswordLen = 8
	// bcrypt silently ignores everything after 72 bytes; refuse instead of
	// pretending the tail is part of the secret.
	maxPasswordBytes = 72
	maxUsernameLen   = 128

	// maxJSONBody caps request bodies for the JSON endpoints.
	maxJSONBody = 1 << 20
)

// dummyPasswordHash is a valid cost-12 bcrypt hash that is compared against
// when the requested user does not exist, so that "unknown user" and "wrong
// password" take the same time.
const dummyPasswordHash = "$2a$12$4mFyUCH08Omoz829Wy8HfuYT0GZmT3fofSYWhvcx6w9trX.bEygNW"

type session struct {
	Username  string
	ExpiresAt time.Time
}

type loginAttempt struct {
	Count       int
	WindowStart time.Time // first failure of the current window
	LockedAt    time.Time
	LastSeen    time.Time
}

type Handler struct {
	DB      *db.DB
	Storage *storage.FileSystem
	Config  *config.Config
	Logger  *slog.Logger
	Version string
	// Objects is the shared object service. New() installs a private
	// instance; main replaces it with the one used by the S3 API so that
	// webhooks and image jobs are shared.
	Objects *service.Objects

	sessionTTL     time.Duration
	trustedProxies []*net.IPNet

	mu       sync.RWMutex
	sessions map[string]session // token -> session

	loginMu   sync.Mutex
	loginRate map[string]*loginAttempt // "ip:<addr>" / "user:<name>" -> attempt

	// busy tracks long-running per-bucket operations (storage move,
	// reprocess) so they never overlap.
	busyMu sync.Mutex
	busy   map[string]string // bucket -> operation
}

func New(database *db.DB, store *storage.FileSystem, cfg *config.Config, logger *slog.Logger) *Handler {
	h := &Handler{
		DB:         database,
		Storage:    store,
		Config:     cfg,
		Logger:     logger,
		Objects:    service.New(database, store, logger),
		sessionTTL: config.Duration(cfg.Admin.SessionTTL, defaultSessionTTL),
		sessions:   make(map[string]session),
		loginRate:  make(map[string]*loginAttempt),
		busy:       make(map[string]string),
	}
	h.trustedProxies = parseTrustedProxies(cfg.Admin.TrustedProxies, logger)

	// Warn about wildcard CORS in admin API
	for _, o := range cfg.Admin.CORSOrigins {
		if o == "*" {
			logger.Warn("Admin API CORS allows all origins ('*'). Consider restricting to specific origins in production.")
			break
		}
	}

	// Clean expired sessions and login attempts every hour
	go h.sessionCleaner()
	return h
}

// parseTrustedProxies turns IPs and CIDRs into networks. Invalid entries are
// logged and skipped (config validation should already have rejected them).
func parseTrustedProxies(list []string, logger *slog.Logger) []*net.IPNet {
	var out []*net.IPNet
	for _, p := range list {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if strings.Contains(p, "/") {
			_, n, err := net.ParseCIDR(p)
			if err != nil {
				if logger != nil {
					logger.Warn("ignoring invalid trusted proxy", "value", httpx.SanitizeLog(p), "error", err)
				}
				continue
			}
			out = append(out, n)
			continue
		}
		ip := net.ParseIP(p)
		if ip == nil {
			if logger != nil {
				logger.Warn("ignoring invalid trusted proxy", "value", httpx.SanitizeLog(p))
			}
			continue
		}
		bits := 32
		if ip.To4() == nil {
			bits = 128
		} else {
			ip = ip.To4()
		}
		out = append(out, &net.IPNet{IP: ip, Mask: net.CIDRMask(bits, bits)})
	}
	return out
}

func (h *Handler) sessionCleaner() {
	defer func() {
		if rec := recover(); rec != nil {
			h.Logger.Error("admin session cleaner panic", "panic", rec)
		}
	}()
	ticker := time.NewTicker(1 * time.Hour)
	defer ticker.Stop()
	for range ticker.C {
		h.cleanupOnce(time.Now())
	}
}

// cleanupOnce drops expired sessions and stale login counters.
func (h *Handler) cleanupOnce(now time.Time) {
	h.mu.Lock()
	for token, s := range h.sessions {
		if now.After(s.ExpiresAt) {
			delete(h.sessions, token)
		}
	}
	h.mu.Unlock()

	h.loginMu.Lock()
	for k, a := range h.loginRate {
		if attemptExpired(a, now) {
			delete(h.loginRate, k)
		}
	}
	h.loginMu.Unlock()
}

// attemptExpired reports whether a login counter carries no live state: the
// lock has lapsed, the failure window has lapsed, or nothing was counted.
func attemptExpired(a *loginAttempt, now time.Time) bool {
	if !a.LockedAt.IsZero() {
		return now.After(a.LockedAt.Add(loginLockDuration))
	}
	if a.Count > 0 {
		return now.After(a.WindowStart.Add(loginLockDuration))
	}
	return true
}

func loginKeys(ip, username string) []string {
	keys := []string{"ip:" + ip}
	if username != "" {
		keys = append(keys, "user:"+username)
	}
	return keys
}

// checkLoginRate reports whether a login may proceed for the given keys.
func (h *Handler) checkLoginRate(keys ...string) bool {
	now := time.Now()
	h.loginMu.Lock()
	defer h.loginMu.Unlock()
	allowed := true
	for _, k := range keys {
		a, ok := h.loginRate[k]
		if !ok {
			continue
		}
		if attemptExpired(a, now) {
			delete(h.loginRate, k)
			continue
		}
		if !a.LockedAt.IsZero() {
			allowed = false // still locked
		}
	}
	return allowed
}

func (h *Handler) recordLoginFailure(keys ...string) {
	now := time.Now()
	h.loginMu.Lock()
	defer h.loginMu.Unlock()
	for _, k := range keys {
		a, ok := h.loginRate[k]
		if ok && attemptExpired(a, now) {
			delete(h.loginRate, k)
			ok = false
		}
		if !ok {
			if len(h.loginRate) >= maxLoginEntries {
				h.evictOldestLocked()
			}
			a = &loginAttempt{WindowStart: now}
			h.loginRate[k] = a
		}
		a.Count++
		a.LastSeen = now
		if a.Count >= maxLoginAttempts {
			a.LockedAt = now
		}
	}
}

// evictOldestLocked removes the least recently touched counter. Caller holds loginMu.
func (h *Handler) evictOldestLocked() {
	var oldestKey string
	var oldest time.Time
	first := true
	for k, a := range h.loginRate {
		if first || a.LastSeen.Before(oldest) {
			oldestKey, oldest, first = k, a.LastSeen, false
		}
	}
	if !first {
		delete(h.loginRate, oldestKey)
	}
}

func (h *Handler) resetLoginAttempts(keys ...string) {
	h.loginMu.Lock()
	defer h.loginMu.Unlock()
	for _, k := range keys {
		delete(h.loginRate, k)
	}
}

// remoteIP extracts the IP from r.RemoteAddr (IPv6 safe). It returns the raw
// value when it is not host:port (tests, unix sockets).
func remoteIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = strings.TrimSpace(r.RemoteAddr)
	}
	host = strings.TrimPrefix(strings.TrimSuffix(host, "]"), "[")
	if ip := net.ParseIP(host); ip != nil {
		return ip.String()
	}
	return host
}

// clientIP returns the address used for rate limiting and audit logs. The
// X-Forwarded-For header is honored (first hop) only when the direct peer is
// one of the configured trusted proxies; otherwise any client could spoof it
// and bypass the login limiter.
func clientIP(r *http.Request, trusted []*net.IPNet) string {
	remote := remoteIP(r)
	if len(trusted) == 0 {
		return remote
	}
	rip := net.ParseIP(remote)
	if rip == nil || !ipInNets(rip, trusted) {
		return remote
	}
	fwd := r.Header.Get("X-Forwarded-For")
	if fwd == "" {
		return remote
	}
	first, _, _ := strings.Cut(fwd, ",")
	first = strings.TrimSpace(first)
	// Tolerate "ip:port" and "[v6]:port" forms some proxies emit.
	if hostPart, _, err := net.SplitHostPort(first); err == nil {
		first = hostPart
	}
	first = strings.TrimPrefix(strings.TrimSuffix(first, "]"), "[")
	if ip := net.ParseIP(first); ip != nil {
		return ip.String()
	}
	return remote
}

func ipInNets(ip net.IP, nets []*net.IPNet) bool {
	for _, n := range nets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

func generateToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "cks_" + hex.EncodeToString(b), nil
}

func (h *Handler) createSession(username string) (string, time.Time, error) {
	token, err := generateToken()
	if err != nil {
		return "", time.Time{}, err
	}
	expiresAt := time.Now().Add(h.sessionTTL)
	h.mu.Lock()
	h.sessions[token] = session{Username: username, ExpiresAt: expiresAt}
	h.mu.Unlock()
	return token, expiresAt, nil
}

func (h *Handler) validateToken(token string) (string, bool) {
	if token == "" {
		return "", false
	}
	h.mu.RLock()
	s, ok := h.sessions[token]
	h.mu.RUnlock()
	if !ok || time.Now().After(s.ExpiresAt) {
		return "", false
	}
	return s.Username, true
}

func (h *Handler) deleteSession(token string) {
	h.mu.Lock()
	delete(h.sessions, token)
	h.mu.Unlock()
}

// revokeSessions drops every session belonging to username (password change,
// account deletion). It returns the number of sessions removed.
func (h *Handler) revokeSessions(username string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for token, s := range h.sessions {
		if s.Username == username {
			delete(h.sessions, token)
			n++
		}
	}
	return n
}

// tryLockBucket marks a bucket as busy with op. It returns false (and the
// current op) when another long-running operation already owns the bucket.
func (h *Handler) tryLockBucket(bucket, op string) (bool, string) {
	h.busyMu.Lock()
	defer h.busyMu.Unlock()
	if cur, ok := h.busy[bucket]; ok {
		return false, cur
	}
	h.busy[bucket] = op
	return true, ""
}

func (h *Handler) unlockBucket(bucket string) {
	h.busyMu.Lock()
	delete(h.busy, bucket)
	h.busyMu.Unlock()
}

// ServeHTTP routes admin API requests.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// CORS
	origin := r.Header.Get("Origin")
	if origin != "" && h.corsAllowed(origin) {
		w.Header().Set("Access-Control-Allow-Origin", origin)
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
		w.Header().Set("Access-Control-Max-Age", "86400")
		w.Header().Add("Vary", "Origin")
	}
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-store")

	path := strings.TrimPrefix(r.URL.Path, "/admin")
	path = strings.TrimSuffix(path, "/")

	// Public endpoints (no auth required)
	if path == "/login" && r.Method == http.MethodPost {
		h.handleLogin(w, r)
		return
	}

	// All other endpoints require auth
	token := extractToken(r)
	username, ok := h.validateToken(token)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	// Route to handler
	parts := strings.Split(strings.TrimPrefix(path, "/"), "/")

	switch {
	// GET /admin/status
	case path == "/status" && r.Method == http.MethodGet:
		h.handleStatus(w, r)

	// POST /admin/logout
	case path == "/logout" && r.Method == http.MethodPost:
		h.deleteSession(token)
		writeJSON(w, http.StatusOK, map[string]string{"message": "logged out"})

	// GET /admin/admins
	case path == "/admins" && r.Method == http.MethodGet:
		h.handleListAdmins(w, r)

	// POST /admin/admins
	case path == "/admins" && r.Method == http.MethodPost:
		h.handleCreateAdmin(w, r)

	// DELETE /admin/admins/{username}
	case len(parts) == 2 && parts[0] == "admins" && r.Method == http.MethodDelete:
		h.handleDeleteAdmin(w, r, parts[1], username)

	// PUT /admin/admins/{username}/password
	case len(parts) == 3 && parts[0] == "admins" && parts[2] == "password" && r.Method == http.MethodPut:
		h.handleUpdateAdminPassword(w, r, parts[1])

	// GET /admin/buckets
	case path == "/buckets" && r.Method == http.MethodGet:
		h.handleListBuckets(w, r)

	// POST /admin/buckets
	case path == "/buckets" && r.Method == http.MethodPost:
		h.handleCreateBucket(w, r)

	// GET /admin/buckets/{name}
	case len(parts) == 2 && parts[0] == "buckets" && r.Method == http.MethodGet:
		h.handleGetBucket(w, r, parts[1])

	// DELETE /admin/buckets/{name}
	case len(parts) == 2 && parts[0] == "buckets" && r.Method == http.MethodDelete:
		h.handleDeleteBucket(w, r, parts[1])

	// PUT /admin/buckets/{name}/quota
	case len(parts) == 3 && parts[0] == "buckets" && parts[2] == "quota" && r.Method == http.MethodPut:
		h.handleSetQuota(w, r, parts[1])

	// PUT /admin/buckets/{name}/storage
	case len(parts) == 3 && parts[0] == "buckets" && parts[2] == "storage" && r.Method == http.MethodPut:
		h.handleSetStorage(w, r, parts[1])

	// GET/PUT /admin/buckets/{name}/versioning
	case len(parts) == 3 && parts[0] == "buckets" && parts[2] == "versioning" && r.Method == http.MethodGet:
		h.handleGetVersioning(w, r, parts[1])
	case len(parts) == 3 && parts[0] == "buckets" && parts[2] == "versioning" && r.Method == http.MethodPut:
		h.handleSetVersioning(w, r, parts[1])

	// PUT /admin/buckets/{name}/public-read
	case len(parts) == 3 && parts[0] == "buckets" && parts[2] == "public-read" && r.Method == http.MethodPut:
		h.handleSetPublicRead(w, r, parts[1])

	// PUT /admin/buckets/{name}/webdav
	case len(parts) == 3 && parts[0] == "buckets" && parts[2] == "webdav" && r.Method == http.MethodPut:
		h.handleSetWebDAV(w, r, parts[1])

	// POST /admin/buckets/{name}/reprocess
	case len(parts) == 3 && parts[0] == "buckets" && parts[2] == "reprocess" && r.Method == http.MethodPost:
		h.handleReprocess(w, r, parts[1])

	// GET/POST /admin/buckets/{name}/credentials
	case len(parts) == 3 && parts[0] == "buckets" && parts[2] == "credentials" && r.Method == http.MethodGet:
		h.handleListCredentials(w, r, parts[1])
	case len(parts) == 3 && parts[0] == "buckets" && parts[2] == "credentials" && r.Method == http.MethodPost:
		h.handleCreateCredential(w, r, parts[1])

	// DELETE /admin/credentials/{accessKey}
	case len(parts) == 2 && parts[0] == "credentials" && r.Method == http.MethodDelete:
		h.handleDeleteCredential(w, r, parts[1])

	// GET/POST /admin/buckets/{name}/lifecycle
	case len(parts) == 3 && parts[0] == "buckets" && parts[2] == "lifecycle" && r.Method == http.MethodGet:
		h.handleGetLifecycle(w, r, parts[1])
	case len(parts) == 3 && parts[0] == "buckets" && parts[2] == "lifecycle" && r.Method == http.MethodPost:
		h.handleSetLifecycle(w, r, parts[1])
	case len(parts) == 3 && parts[0] == "buckets" && parts[2] == "lifecycle" && r.Method == http.MethodDelete:
		h.handleDeleteLifecycle(w, r, parts[1])

	// GET/POST /admin/buckets/{name}/webhooks
	case len(parts) == 3 && parts[0] == "buckets" && parts[2] == "webhooks" && r.Method == http.MethodGet:
		h.handleListWebhooks(w, r, parts[1])
	case len(parts) == 3 && parts[0] == "buckets" && parts[2] == "webhooks" && r.Method == http.MethodPost:
		h.handleCreateWebhook(w, r, parts[1])

	// DELETE /admin/webhooks/{id}
	case len(parts) == 2 && parts[0] == "webhooks" && r.Method == http.MethodDelete:
		h.handleDeleteWebhook(w, r, parts[1])

	// GET /admin/buckets/{name}/objects?prefix=&delimiter=/
	case len(parts) >= 3 && parts[0] == "buckets" && parts[2] == "objects" && r.Method == http.MethodGet:
		h.handleListObjects(w, r, parts[1])

	// POST /admin/buckets/{name}/objects/delete-prefix (folder delete)
	case len(parts) == 4 && parts[0] == "buckets" && parts[2] == "objects" && parts[3] == "delete-prefix" && r.Method == http.MethodPost:
		h.handleDeletePrefix(w, r, parts[1])

	// DELETE /admin/buckets/{name}/objects/path/to/key[?versionId=]
	case len(parts) >= 4 && parts[0] == "buckets" && parts[2] == "objects" && r.Method == http.MethodDelete:
		key := extractObjectKey(parts)
		if key == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "object key required"})
		} else {
			h.handleDeleteObject(w, r, parts[1], key)
		}

	default:
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
	}
}

func (h *Handler) corsAllowed(origin string) bool {
	for _, o := range h.Config.Admin.CORSOrigins {
		if o == "*" || o == origin {
			return true
		}
	}
	return false
}

func extractToken(r *http.Request) string {
	auth := r.Header.Get("Authorization")
	if strings.HasPrefix(auth, "Bearer ") {
		return strings.TrimPrefix(auth, "Bearer ")
	}
	return ""
}

func (h *Handler) handleLogin(w http.ResponseWriter, r *http.Request) {
	ip := clientIP(r, h.trustedProxies)

	// Per-IP check first so a locked address is turned away before we spend
	// time parsing anything it sends.
	if !h.checkLoginRate(loginKeys(ip, "")...) {
		h.Logger.Warn("login rate limited", "ip", ip)
		writeJSON(w, http.StatusTooManyRequests, map[string]string{
			"error": "too many login attempts, try again later",
		})
		return
	}

	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if !decodeJSON(w, r, &req, false) {
		return
	}

	if req.Username == "" || req.Password == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "username and password required"})
		return
	}
	if len(req.Username) > maxUsernameLen || len(req.Password) > maxPasswordBytes {
		// Nothing valid can be that long; do not spend a bcrypt round on it.
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid credentials"})
		return
	}
	safeUser := httpx.SanitizeLog(req.Username)
	keys := loginKeys(ip, req.Username)

	if !h.checkLoginRate(keys...) {
		h.Logger.Warn("login rate limited", "username", safeUser, "ip", ip)
		writeJSON(w, http.StatusTooManyRequests, map[string]string{
			"error": "too many login attempts, try again later",
		})
		return
	}

	admin, err := h.DB.GetAdmin(req.Username)
	if err != nil {
		h.Logger.Error("admin login db error", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	// Always run a bcrypt comparison so an unknown username costs the same
	// wall-clock time as a wrong password.
	hash := dummyPasswordHash
	if admin != nil {
		hash = admin.PasswordHash
	}
	cmpErr := bcrypt.CompareHashAndPassword([]byte(hash), []byte(req.Password))
	if admin == nil || cmpErr != nil {
		h.recordLoginFailure(keys...)
		h.Logger.Warn("failed login attempt", "username", safeUser, "ip", ip)
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid credentials"})
		return
	}

	// Success — reset attempts
	h.resetLoginAttempts(keys...)

	token, expiresAt, err := h.createSession(admin.Username)
	if err != nil {
		h.Logger.Error("create session error", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	h.Logger.Info("admin login", "username", safeUser, "ip", ip)
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"token":      token,
		"username":   admin.Username,
		"expires_at": expiresAt.UTC().Format(time.RFC3339),
		"expires_in": int(h.sessionTTL.Seconds()),
	})
}

func (h *Handler) handleStatus(w http.ResponseWriter, r *http.Request) {
	buckets, _ := h.DB.ListBuckets()
	hasAdmin, _ := h.DB.AdminExists()
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"status":       "ok",
		"version":      h.Version,
		"buckets":      len(buckets),
		"admin_exists": hasAdmin,
		"webdav": map[string]interface{}{
			"enabled": h.Config.WebDAV.Enabled,
			"listen":  h.Config.WebDAV.Listen,
		},
	})
}

// decodeJSON parses the request body into v. On failure it writes the error
// response and returns false, so handlers never continue with zero-valued
// defaults after a malformed body. allowEmpty treats a completely empty body
// as "no fields given" (for endpoints whose every field is optional).
func decodeJSON(w http.ResponseWriter, r *http.Request, v interface{}, allowEmpty bool) bool {
	if r.Body == nil {
		if allowEmpty {
			return true
		}
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "request body required"})
		return false
	}
	dec := json.NewDecoder(r.Body)
	err := dec.Decode(v)
	if err == nil {
		// Reject trailing garbage after the first JSON value.
		if _, extra := dec.Token(); extra != io.EOF {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body: unexpected trailing data"})
			return false
		}
		return true
	}
	if errors.Is(err, io.EOF) && allowEmpty {
		return true
	}
	var tooBig *http.MaxBytesError
	if errors.As(err, &tooBig) {
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "request body too large"})
		return false
	}
	if errors.Is(err, io.EOF) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "request body required"})
		return false
	}
	writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
	return false
}

// validatePassword enforces the shared password policy. It returns a
// client-facing message, or "" when the password is acceptable.
func validatePassword(p string) string {
	if len(p) < minPasswordLen {
		return "password must be at least 8 characters"
	}
	if len(p) > maxPasswordBytes {
		return "password must be at most 72 bytes (bcrypt limit)"
	}
	return ""
}

func writeJSON(w http.ResponseWriter, status int, data interface{}) {
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(data)
}
