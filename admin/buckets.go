package admin

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
)

var validBucketName = regexp.MustCompile(`^[a-z0-9][a-z0-9\-]{1,61}[a-z0-9]$`)

type bucketResponse struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	QuotaBytes  int64  `json:"quota_bytes"`
	Versioning  string `json:"versioning"`
	StorageDir  string `json:"storage_dir"`
	Objects     int64  `json:"objects"`
	UsageBytes  int64  `json:"usage_bytes"`
	Credentials int    `json:"credentials"`
	CreatedAt   string `json:"created_at"`
}

func (h *Handler) handleListBuckets(w http.ResponseWriter, r *http.Request) {
	buckets, err := h.DB.ListBuckets()
	if err != nil {
		h.Logger.Error("list buckets error", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	result := make([]bucketResponse, 0, len(buckets))
	for _, b := range buckets {
		objCount, _ := h.DB.CountObjects(b.ID)
		usage, _ := h.DB.GetBucketUsage(b.ID)
		creds, _ := h.DB.ListCredentials(b.ID)
		result = append(result, bucketResponse{
			ID:          b.ID,
			Name:        b.Name,
			QuotaBytes:  b.QuotaBytes,
			Versioning:  b.Versioning,
			StorageDir:  b.StorageDir,
			Objects:     objCount,
			UsageBytes:  usage,
			Credentials: len(creds),
			CreatedAt:   b.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"),
		})
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{"buckets": result})
}

func (h *Handler) handleGetBucket(w http.ResponseWriter, r *http.Request, name string) {
	bucket, err := h.DB.GetBucket(name)
	if err != nil {
		h.Logger.Error("get bucket error", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	if bucket == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "bucket not found"})
		return
	}

	objCount, _ := h.DB.CountObjects(bucket.ID)
	usage, _ := h.DB.GetBucketUsage(bucket.ID)
	creds, _ := h.DB.ListCredentials(bucket.ID)

	storageDir := bucket.StorageDir
	storagePath := filepath.Join(h.Config.Storage.RootDir, bucket.Name)
	if storageDir != "" {
		storagePath = filepath.Join(storageDir, bucket.Name)
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"id":           bucket.ID,
		"name":         bucket.Name,
		"quota_bytes":  bucket.QuotaBytes,
		"versioning":   bucket.Versioning,
		"storage_dir":  storageDir,
		"storage_path": storagePath,
		"objects":      objCount,
		"usage_bytes":  usage,
		"credentials":  len(creds),
		"created_at":   bucket.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"),
	})
}

func (h *Handler) handleCreateBucket(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name       string `json:"name"`
		StorageDir string `json:"storage_dir"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}

	if !validBucketName.MatchString(req.Name) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid bucket name: must be 3-63 lowercase alphanumeric chars and hyphens"})
		return
	}

	if req.StorageDir != "" && !filepath.IsAbs(req.StorageDir) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "storage_dir must be an absolute path"})
		return
	}

	existing, err := h.DB.GetBucket(req.Name)
	if err != nil {
		h.Logger.Error("check bucket error", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	if existing != nil {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "bucket already exists"})
		return
	}

	bucket, err := h.DB.CreateBucket(req.Name, req.StorageDir)
	if err != nil {
		h.Logger.Error("create bucket error", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	// Create storage directory
	base := h.Config.Storage.RootDir
	if req.StorageDir != "" {
		base = req.StorageDir
		h.Storage.SetBucketDir(req.Name, req.StorageDir)
	}
	storagePath := filepath.Join(base, req.Name)
	if err := os.MkdirAll(storagePath, 0700); err != nil {
		h.Logger.Error("create storage dir error", "error", err)
		h.DB.DeleteBucket(req.Name)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to create storage directory"})
		return
	}

	h.Logger.Info("bucket created via admin API", "bucket", req.Name, "storage_dir", req.StorageDir)
	writeJSON(w, http.StatusCreated, map[string]interface{}{
		"id":          bucket.ID,
		"name":        bucket.Name,
		"storage_dir": bucket.StorageDir,
		"created_at":  bucket.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"),
	})
}

func (h *Handler) handleDeleteBucket(w http.ResponseWriter, r *http.Request, name string) {
	bucket, err := h.DB.GetBucket(name)
	if err != nil {
		h.Logger.Error("get bucket error", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	if bucket == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "bucket not found"})
		return
	}

	hasObjects, err := h.DB.BucketHasObjects(bucket.ID)
	if err != nil {
		h.Logger.Error("check objects error", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	if hasObjects {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "bucket is not empty"})
		return
	}

	if err := h.DB.DeleteBucket(name); err != nil {
		h.Logger.Error("delete bucket error", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	// Delete storage directory
	if err := h.Storage.DeleteBucketDir(name); err != nil {
		h.Logger.Error("delete bucket dir error", "bucket", name, "error", err)
	}

	h.Logger.Info("bucket deleted via admin API", "bucket", name)
	writeJSON(w, http.StatusOK, map[string]string{"message": "bucket deleted"})
}

func (h *Handler) handleSetQuota(w http.ResponseWriter, r *http.Request, name string) {
	bucket, err := h.DB.GetBucket(name)
	if err != nil || bucket == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "bucket not found"})
		return
	}

	var req struct {
		QuotaBytes int64 `json:"quota_bytes"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}

	if err := h.DB.SetBucketQuota(name, req.QuotaBytes); err != nil {
		h.Logger.Error("set quota error", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"name":        name,
		"quota_bytes": req.QuotaBytes,
	})
}

func (h *Handler) handleSetStorage(w http.ResponseWriter, r *http.Request, name string) {
	bucket, err := h.DB.GetBucket(name)
	if err != nil || bucket == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "bucket not found"})
		return
	}

	var req struct {
		StorageDir string `json:"storage_dir"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}

	if req.StorageDir != "" && !filepath.IsAbs(req.StorageDir) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "storage_dir must be an absolute path"})
		return
	}

	// Determine old and new paths
	oldBase := h.Config.Storage.RootDir
	if bucket.StorageDir != "" {
		oldBase = bucket.StorageDir
	}
	newBase := h.Config.Storage.RootDir
	if req.StorageDir != "" {
		newBase = req.StorageDir
	}

	oldPath := filepath.Join(oldBase, name)
	newPath := filepath.Join(newBase, name)

	if oldPath == newPath {
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"name":         name,
			"storage_dir":  req.StorageDir,
			"storage_path": newPath,
			"moved":        0,
		})
		return
	}

	// Create new dir
	if err := os.MkdirAll(newPath, 0700); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to create new storage directory"})
		return
	}

	// Move files
	entries, err := os.ReadDir(oldPath)
	if err != nil && !os.IsNotExist(err) {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to read old storage directory"})
		return
	}

	movedCount := 0
	for _, entry := range entries {
		src := filepath.Join(oldPath, entry.Name())
		dst := filepath.Join(newPath, entry.Name())
		if err := os.Rename(src, dst); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{
				"error": "move failed at " + entry.Name() + ": " + err.Error(),
				"moved": strconv.Itoa(movedCount),
			})
			return
		}
		movedCount++
	}
	os.RemoveAll(oldPath)

	// Update DB and in-memory registry
	if err := h.DB.SetBucketStorageDir(name, req.StorageDir); err != nil {
		h.Logger.Error("set storage dir error", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	if req.StorageDir != "" {
		h.Storage.SetBucketDir(name, req.StorageDir)
	} else {
		h.Storage.RemoveBucketDir(name)
	}

	h.Logger.Info("bucket storage moved via admin API", "bucket", name, "new_dir", req.StorageDir, "moved", movedCount)
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"name":         name,
		"storage_dir":  req.StorageDir,
		"storage_path": newPath,
		"moved":        movedCount,
	})
}

func (h *Handler) handleGetVersioning(w http.ResponseWriter, r *http.Request, name string) {
	bucket, err := h.DB.GetBucket(name)
	if err != nil || bucket == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "bucket not found"})
		return
	}

	status := bucket.Versioning
	if status == "" {
		status = "Disabled"
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"name":       name,
		"versioning": status,
	})
}

func (h *Handler) handleSetVersioning(w http.ResponseWriter, r *http.Request, name string) {
	bucket, err := h.DB.GetBucket(name)
	if err != nil || bucket == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "bucket not found"})
		return
	}

	var req struct {
		Versioning string `json:"versioning"` // "Enabled" or "Suspended"
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}

	if req.Versioning != "Enabled" && req.Versioning != "Suspended" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "versioning must be 'Enabled' or 'Suspended'"})
		return
	}

	if err := h.DB.SetBucketVersioning(name, req.Versioning); err != nil {
		h.Logger.Error("set versioning error", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"name":       name,
		"versioning": req.Versioning,
	})
}
