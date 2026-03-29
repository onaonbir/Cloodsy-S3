package admin

import (
	"encoding/json"
	"net/http"

	"github.com/onaonbir/Cloodsy-S3/auth"
)

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
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		req.Permission = "read-write"
	}
	if req.Permission == "" {
		req.Permission = "read-write"
	}
	if req.Permission != "read-write" && req.Permission != "read-only" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "permission must be 'read-write' or 'read-only'"})
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

	h.Logger.Info("credential created via admin API", "bucket", bucketName, "name", req.Name, "access_key", accessKey)
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

	h.Logger.Info("credential deleted via admin API", "access_key", accessKey)
	writeJSON(w, http.StatusOK, map[string]string{"message": "credential deleted"})
}
