package admin

import (
	"net/http"

	"github.com/onaonbir/Cloodsy-S3/auth"
	"github.com/onaonbir/Cloodsy-S3/httpx"
)

func validPermission(p string) bool {
	return p == "read-write" || p == "read-only"
}

func (h *Handler) handleListCredentials(w http.ResponseWriter, r *http.Request, bucketName string) {
	bucket, err := h.DB.GetBucket(bucketName)
	if err != nil || bucket == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "bucket not found"})
		return
	}

	creds, err := h.DB.ListCredentialsFull(bucket.ID)
	if err != nil {
		h.Logger.Error("list credentials error", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	result := make([]map[string]interface{}, 0, len(creds))
	for _, c := range creds {
		result = append(result, map[string]interface{}{
			"id":         c.ID,
			"name":       c.Name,
			"access_key": c.AccessKey,
			// SECURITY NOTE: the secret is returned in clear on purpose — the
			// Cloodsy Flutter admin GUI shows/copies it from this listing and
			// there is no separate "reveal" endpoint. Anyone holding an admin
			// session already has full control of every bucket, so this does
			// not widen the trust boundary, but the admin listener must be
			// TLS-protected or loopback-only (see admin.tls / trusted_proxies).
			"secret_key": c.SecretKey,
			"permission": c.Permission,
			"created_at": c.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"),
		})
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{"credentials": result})
}

func (h *Handler) handleCreateCredential(w http.ResponseWriter, r *http.Request, bucketName string) {
	bucket, err := h.DB.GetBucket(bucketName)
	if err != nil || bucket == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "bucket not found"})
		return
	}

	var req struct {
		Name       string `json:"name"`
		Permission string `json:"permission"`
	}
	// Both fields are optional so an empty body is accepted; a malformed body
	// is rejected instead of silently minting a read-write key.
	if !decodeJSON(w, r, &req, true) {
		return
	}
	if req.Permission == "" {
		req.Permission = "read-write"
	}
	if !validPermission(req.Permission) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "permission must be 'read-write' or 'read-only'"})
		return
	}
	if len(req.Name) > 128 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "name too long"})
		return
	}

	accessKey, err := auth.GenerateAccessKey()
	if err != nil {
		h.Logger.Error("generate access key error", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	secretKey, err := auth.GenerateSecretKey()
	if err != nil {
		h.Logger.Error("generate secret key error", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	_, err = h.DB.CreateCredential(bucket.ID, req.Name, accessKey, secretKey, req.Permission)
	if err != nil {
		h.Logger.Error("create credential error", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	h.Logger.Info("credential created via admin API", "bucket", bucketName, "name", httpx.SanitizeLog(req.Name), "access_key", maskKey(accessKey))
	writeJSON(w, http.StatusCreated, map[string]interface{}{
		"bucket":     bucketName,
		"name":       req.Name,
		"access_key": accessKey,
		"secret_key": secretKey,
		"permission": req.Permission,
	})
}

func (h *Handler) handleDeleteCredential(w http.ResponseWriter, r *http.Request, accessKey string) {
	cred, err := h.DB.GetCredentialByAccessKey(accessKey)
	if err != nil {
		h.Logger.Error("get credential error", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	if cred == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "credential not found"})
		return
	}

	if err := h.DB.DeleteCredential(accessKey); err != nil {
		h.Logger.Error("delete credential error", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	h.Logger.Info("credential deleted via admin API", "access_key", maskKey(accessKey))
	writeJSON(w, http.StatusOK, map[string]string{"message": "credential deleted"})
}

// maskKey keeps a short identifying prefix of an access key for logs. It is
// safe for keys shorter than the prefix (the old code sliced blindly).
func maskKey(k string) string {
	k = httpx.SanitizeLog(k)
	if len(k) <= 6 {
		return "***"
	}
	return k[:6] + "***"
}
