package admin

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"

	"github.com/onaonbir/Cloodsy-S3/httpx"
	imageutil "github.com/onaonbir/Cloodsy-S3/image"
)

var validBucketName = regexp.MustCompile(`^[a-z0-9][a-z0-9\-]{1,61}[a-z0-9]$`)

type bucketResponse struct {
	ID            int64  `json:"id"`
	Name          string `json:"name"`
	QuotaBytes    int64  `json:"quota_bytes"`
	Versioning    string `json:"versioning"`
	StorageDir    string `json:"storage_dir"`
	PublicRead    bool   `json:"public_read"`
	WebDAVEnabled bool   `json:"webdav_enabled"`
	Objects       int64  `json:"objects"`
	UsageBytes    int64  `json:"usage_bytes"`
	Credentials   int    `json:"credentials"`
	CreatedAt     string `json:"created_at"`
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
			ID:            b.ID,
			Name:          b.Name,
			QuotaBytes:    b.QuotaBytes,
			Versioning:    b.Versioning,
			StorageDir:    b.StorageDir,
			PublicRead:    b.PublicRead,
			WebDAVEnabled: b.WebDAVEnabled,
			Objects:       objCount,
			UsageBytes:    usage,
			Credentials:   len(creds),
			CreatedAt:     b.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"),
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
		"id":             bucket.ID,
		"name":           bucket.Name,
		"quota_bytes":    bucket.QuotaBytes,
		"versioning":     bucket.Versioning,
		"storage_dir":    storageDir,
		"public_read":    bucket.PublicRead,
		"webdav_enabled": bucket.WebDAVEnabled,
		"storage_path":   storagePath,
		"objects":        objCount,
		"usage_bytes":    usage,
		"credentials":    len(creds),
		"created_at":     bucket.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"),
	})
}

// validateStorageDir checks a custom storage base directory supplied by the
// admin. It must be absolute and clean (no ".." tricks, no NUL).
func validateStorageDir(dir string) string {
	if dir == "" {
		return ""
	}
	if !filepath.IsAbs(dir) {
		return "storage_dir must be an absolute path"
	}
	if filepath.Clean(dir) != dir {
		return "storage_dir must be a clean absolute path"
	}
	for _, c := range dir {
		if c == 0 {
			return "storage_dir contains NUL"
		}
	}
	return ""
}

func (h *Handler) handleCreateBucket(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name       string `json:"name"`
		StorageDir string `json:"storage_dir"`
	}
	if !decodeJSON(w, r, &req, false) {
		return
	}

	if !validBucketName.MatchString(req.Name) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid bucket name: must be 3-63 lowercase alphanumeric chars and hyphens"})
		return
	}

	if msg := validateStorageDir(req.StorageDir); msg != "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": msg})
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
		if req.StorageDir != "" {
			h.Storage.RemoveBucketDir(req.Name)
		}
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to create storage directory"})
		return
	}

	h.Logger.Info("bucket created via admin API", "bucket", req.Name, "storage_dir", httpx.SanitizeLog(req.StorageDir))
	writeJSON(w, http.StatusCreated, map[string]interface{}{
		"id":          bucket.ID,
		"name":        bucket.Name,
		"storage_dir": bucket.StorageDir,
		"created_at":  bucket.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"),
	})
}

// handleDeleteBucket removes a bucket. It refuses while any object row exists
// (current objects, non-current versions or delete markers) unless
// ?force=true is given, in which case the rows are dropped with the bucket
// (ON DELETE CASCADE) and the data directories are removed.
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

	force, _ := strconv.ParseBool(r.URL.Query().Get("force"))

	if ok, op := h.tryLockBucket(name, "delete"); !ok {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "bucket is busy: " + op + " in progress"})
		return
	}
	defer h.unlockBucket(name)

	hasRows, err := h.DB.BucketHasAnyRows(bucket.ID)
	if err != nil {
		h.Logger.Error("check objects error", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	if hasRows && !force {
		hasObjects, _ := h.DB.BucketHasObjects(bucket.ID)
		msg := "bucket is not empty"
		if !hasObjects {
			msg = "bucket still holds object versions or delete markers; pass ?force=true to delete them"
		}
		writeJSON(w, http.StatusConflict, map[string]string{"error": msg})
		return
	}

	if err := h.DB.DeleteBucket(name); err != nil {
		h.Logger.Error("delete bucket error", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	// Delete storage directory (and the sibling multipart/cache trees).
	if err := h.Storage.DeleteBucketDir(name); err != nil {
		h.Logger.Error("delete bucket dir error", "bucket", name, "error", err)
	}

	h.Logger.Info("bucket deleted via admin API", "bucket", name, "force", force, "had_rows", hasRows)
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
	if !decodeJSON(w, r, &req, false) {
		return
	}
	if req.QuotaBytes < 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "quota_bytes must be >= 0"})
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

// movedEntry records one rename so a failed move can be undone.
type movedEntry struct {
	src, dst string
}

// moveTree renames every entry of src into dst (creating dst). It appends the
// renames to *moved so the caller can roll back. A missing src is not an error.
func moveTree(src, dst string, moved *[]movedEntry, created *[]string) (int, error) {
	entries, err := os.ReadDir(src)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("read %s: %w", src, err)
	}
	if len(entries) == 0 {
		return 0, nil
	}
	if _, err := os.Stat(dst); os.IsNotExist(err) {
		if err := os.MkdirAll(dst, 0700); err != nil {
			return 0, fmt.Errorf("create %s: %w", dst, err)
		}
		*created = append(*created, dst)
	}
	n := 0
	for _, e := range entries {
		s := filepath.Join(src, e.Name())
		d := filepath.Join(dst, e.Name())
		if _, err := os.Lstat(d); err == nil {
			return n, fmt.Errorf("destination already has %s", e.Name())
		}
		if err := os.Rename(s, d); err != nil {
			return n, fmt.Errorf("move %s: %w", e.Name(), err)
		}
		*moved = append(*moved, movedEntry{src: s, dst: d})
		n++
	}
	return n, nil
}

// rollbackMoves renames entries back in reverse order and removes the
// directories this move created. Errors are logged; there is nothing better
// to do with them.
func (h *Handler) rollbackMoves(moved []movedEntry, created []string) {
	for i := len(moved) - 1; i >= 0; i-- {
		if err := os.Rename(moved[i].dst, moved[i].src); err != nil {
			h.Logger.Error("storage move rollback failed", "from", moved[i].dst, "to", moved[i].src, "error", err)
		}
	}
	for i := len(created) - 1; i >= 0; i-- {
		os.Remove(created[i]) // only succeeds when empty, which is the point
	}
}

// handleSetStorage relocates a bucket's data (objects, multipart staging and
// variant cache) to another base directory on the same filesystem and then
// records the new location. Any failure mid-way moves everything back.
func (h *Handler) handleSetStorage(w http.ResponseWriter, r *http.Request, name string) {
	bucket, err := h.DB.GetBucket(name)
	if err != nil || bucket == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "bucket not found"})
		return
	}

	var req struct {
		StorageDir string `json:"storage_dir"`
	}
	if !decodeJSON(w, r, &req, false) {
		return
	}

	if msg := validateStorageDir(req.StorageDir); msg != "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": msg})
		return
	}

	// Determine old and new base paths
	oldBase := h.Config.Storage.RootDir
	if bucket.StorageDir != "" {
		oldBase = bucket.StorageDir
	}
	newBase := h.Config.Storage.RootDir
	if req.StorageDir != "" {
		newBase = req.StorageDir
	}
	oldBase, _ = filepath.Abs(oldBase)
	newBase, _ = filepath.Abs(newBase)

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

	// One long-running operation per bucket at a time.
	if ok, op := h.tryLockBucket(name, "storage move"); !ok {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "bucket is busy: " + op + " in progress"})
		return
	}
	defer h.unlockBucket(name)

	// The new base must exist and live on the same device: os.Rename cannot
	// cross filesystems and a copy would leave two half-states around.
	if err := os.MkdirAll(newBase, 0700); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to create new storage directory"})
		return
	}
	if err := os.MkdirAll(oldPath, 0700); err != nil { // ensure the source exists for the device probe
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to access current storage directory"})
		return
	}
	same, err := sameDevice(oldPath, newBase)
	if err != nil {
		h.Logger.Error("storage move device check failed", "bucket", name, "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to compare storage devices"})
		return
	}
	if !same {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "new storage_dir is on a different filesystem; cross-device moves are not supported (copy the data offline and update storage_dir afterwards)",
		})
		return
	}

	// Move the object tree plus the sibling staging/cache trees.
	pairs := []movedEntry{
		{src: oldPath, dst: newPath},
		{src: filepath.Join(oldBase, "."+name+"-multipart"), dst: filepath.Join(newBase, "."+name+"-multipart")},
		{src: filepath.Join(oldBase, "."+name+"-cache"), dst: filepath.Join(newBase, "."+name+"-cache")},
	}
	var moved []movedEntry
	var created []string
	movedCount := 0
	if _, err := os.Stat(newPath); os.IsNotExist(err) {
		if err := os.MkdirAll(newPath, 0700); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to create new storage directory"})
			return
		}
		created = append(created, newPath)
	}
	for _, p := range pairs {
		n, err := moveTree(p.src, p.dst, &moved, &created)
		movedCount += n
		if err != nil {
			h.Logger.Error("bucket storage move failed, rolling back", "bucket", name, "error", err, "moved", movedCount)
			h.rollbackMoves(moved, created)
			writeJSON(w, http.StatusInternalServerError, map[string]string{
				"error": "move failed: " + err.Error(),
				"moved": "0",
			})
			return
		}
	}

	// Persist only after every byte is in place.
	if err := h.DB.SetBucketStorageDir(name, req.StorageDir); err != nil {
		h.Logger.Error("set storage dir error, rolling back move", "bucket", name, "error", err)
		h.rollbackMoves(moved, created)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	if req.StorageDir != "" {
		h.Storage.SetBucketDir(name, req.StorageDir)
	} else {
		h.Storage.RemoveBucketDir(name)
	}

	// Old directories are empty now; remove them (never RemoveAll — anything
	// that appeared concurrently is left alone).
	for _, p := range pairs {
		os.Remove(p.src)
	}

	h.Logger.Info("bucket storage moved via admin API", "bucket", name, "new_dir", httpx.SanitizeLog(req.StorageDir), "moved", movedCount)
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
	if !decodeJSON(w, r, &req, false) {
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

func (h *Handler) handleSetPublicRead(w http.ResponseWriter, r *http.Request, name string) {
	bucket, err := h.DB.GetBucket(name)
	if err != nil || bucket == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "bucket not found"})
		return
	}

	var req struct {
		PublicRead bool `json:"public_read"`
	}
	if !decodeJSON(w, r, &req, false) {
		return
	}

	if err := h.DB.SetBucketPublicRead(name, req.PublicRead); err != nil {
		h.Logger.Error("set public-read error", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	h.Logger.Info("bucket public-read changed via admin API", "bucket", name, "public_read", req.PublicRead)
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"name":        name,
		"public_read": req.PublicRead,
	})
}

func (h *Handler) handleSetWebDAV(w http.ResponseWriter, r *http.Request, name string) {
	bucket, err := h.DB.GetBucket(name)
	if err != nil || bucket == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "bucket not found"})
		return
	}

	var req struct {
		WebDAVEnabled bool `json:"webdav_enabled"`
	}
	if !decodeJSON(w, r, &req, false) {
		return
	}

	if err := h.DB.SetBucketWebDAVEnabled(name, req.WebDAVEnabled); err != nil {
		h.Logger.Error("set webdav error", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	h.Logger.Info("bucket webdav changed via admin API", "bucket", name, "webdav_enabled", req.WebDAVEnabled)
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"name":           name,
		"webdav_enabled": req.WebDAVEnabled,
	})
}

// handleReprocess regenerates optimized image variants for a bucket's existing
// objects. It runs in the background and returns immediately so the GUI stays
// responsive. Originals are never modified. Only one reprocess (or other
// long-running operation) may run per bucket; a second request gets 409.
func (h *Handler) handleReprocess(w http.ResponseWriter, r *http.Request, name string) {
	bucket, err := h.DB.GetBucket(name)
	if err != nil || bucket == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "bucket not found"})
		return
	}

	quality := h.Config.Image.Quality
	if quality <= 0 || quality > 100 {
		quality = 75
	}
	maxSource := h.Config.Image.MaxSourceBytes

	if ok, op := h.tryLockBucket(name, "reprocess"); !ok {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "bucket is busy: " + op + " in progress"})
		return
	}

	go func(bucketID int64) {
		defer h.unlockBucket(name)
		defer func() {
			if rec := recover(); rec != nil {
				h.Logger.Error("reprocess panic", "bucket", name, "panic", fmt.Sprint(rec))
			}
		}()
		// Images are decoded one at a time here; the S3 GET transform path
		// has its own limiter, so a reprocess adds at most one decode to the
		// process-wide concurrency.
		marker := ""
		processed, failed := 0, 0
		for {
			objects, _, truncated, next, err := h.DB.ListObjectsMeta(bucketID, "", marker, "", 1000)
			if err != nil {
				h.Logger.Error("reprocess list error", "bucket", name, "error", err)
				return
			}
			for i := range objects {
				m := objects[i]
				if m.IsDeleteMarker || !imageutil.IsImageContentType(m.ContentType) {
					continue
				}
				if maxSource > 0 && m.Size > maxSource {
					continue
				}
				job := imageutil.Job{Bucket: name, Key: m.Key, VersionID: m.VersionID, ETag: m.ETag, ContentType: m.ContentType}
				if err := optimizeSafely(h.Storage, job, quality, maxSource); err != nil {
					h.Logger.Debug("reprocess optimize failed", "key", httpx.SanitizeLog(m.Key), "error", err)
					failed++
					continue
				}
				processed++
			}
			if !truncated || next == "" || next == marker {
				break
			}
			marker = next
		}
		h.Logger.Info("reprocess complete", "bucket", name, "optimized", processed, "failed", failed)
	}(bucket.ID)

	writeJSON(w, http.StatusAccepted, map[string]interface{}{
		"name":   name,
		"status": "reprocessing started",
	})
}

// optimizeSafely contains a decoder panic to the single job.
func optimizeSafely(store imageutil.VariantStore, job imageutil.Job, quality int, maxSource int64) (err error) {
	defer func() {
		if rec := recover(); rec != nil {
			err = errors.New(fmt.Sprint("decoder panic: ", rec))
		}
	}()
	return imageutil.OptimizeOne(store, job, quality, maxSource)
}
