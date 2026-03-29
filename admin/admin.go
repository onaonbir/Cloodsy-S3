package admin

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/onaonbir/Cloodsy-S3/config"
	"github.com/onaonbir/Cloodsy-S3/db"
	"github.com/onaonbir/Cloodsy-S3/storage"
	"golang.org/x/crypto/bcrypt"
)

const sessionTTL = 24 * time.Hour

type session struct {
	Username  string
	ExpiresAt time.Time
}

type Handler struct {
	DB      *db.DB
	Storage *storage.FileSystem
	Config  *config.Config
	Logger  *slog.Logger
	Version string

	mu       sync.RWMutex
	sessions map[string]session // token -> session
}

func New(database *db.DB, store *storage.FileSystem, cfg *config.Config, logger *slog.Logger) *Handler {
	h := &Handler{
		DB:       database,
		Storage:  store,
		Config:   cfg,
		Logger:   logger,
		sessions: make(map[string]session),
	}
	// Clean expired sessions every hour
	go h.sessionCleaner()
	return h
}

func (h *Handler) sessionCleaner() {
	ticker := time.NewTicker(1 * time.Hour)
	defer ticker.Stop()
	for range ticker.C {
		h.mu.Lock()
		now := time.Now()
		for token, s := range h.sessions {
			if now.After(s.ExpiresAt) {
				delete(h.sessions, token)
			}
		}
		h.mu.Unlock()
	}
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
	expiresAt := time.Now().Add(sessionTTL)
	h.mu.Lock()
	h.sessions[token] = session{Username: username, ExpiresAt: expiresAt}
	h.mu.Unlock()
	return token, expiresAt, nil
}

func (h *Handler) validateToken(token string) (string, bool) {
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

// ServeHTTP routes admin API requests.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// CORS
	origin := r.Header.Get("Origin")
	if origin != "" && h.corsAllowed(origin) {
		w.Header().Set("Access-Control-Allow-Origin", origin)
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
		w.Header().Set("Access-Control-Max-Age", "86400")
	}
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	w.Header().Set("Content-Type", "application/json")

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

	// DELETE /admin/buckets/{name}/objects/path/to/key
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
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}

	if req.Username == "" || req.Password == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "username and password required"})
		return
	}

	admin, err := h.DB.GetAdmin(req.Username)
	if err != nil {
		h.Logger.Error("admin login db error", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	if admin == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid credentials"})
		return
	}

	if err := bcrypt.CompareHashAndPassword([]byte(admin.PasswordHash), []byte(req.Password)); err != nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid credentials"})
		return
	}

	token, expiresAt, err := h.createSession(req.Username)
	if err != nil {
		h.Logger.Error("create session error", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	h.Logger.Info("admin login", "username", req.Username)
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"token":      token,
		"username":   req.Username,
		"expires_at": expiresAt.UTC().Format(time.RFC3339),
		"expires_in": int(sessionTTL.Seconds()),
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
	})
}

func writeJSON(w http.ResponseWriter, status int, data interface{}) {
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(data)
}
