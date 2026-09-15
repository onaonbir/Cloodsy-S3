package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"syscall"

	"github.com/onaonbir/Cloodsy-S3/auth"
	"github.com/onaonbir/Cloodsy-S3/config"
	"github.com/onaonbir/Cloodsy-S3/db"
	imageutil "github.com/onaonbir/Cloodsy-S3/image"
	"github.com/onaonbir/Cloodsy-S3/storage"
	"github.com/onaonbir/Cloodsy-S3/webhook"
	"github.com/pterm/pterm"
	"golang.org/x/crypto/bcrypt"
)

// restartWarning is printed after any CLI change the running server caches in
// memory (per-bucket storage directories).
const restartWarning = "A running server caches bucket storage locations: restart it (or make this change through the admin API) for the new location to take effect."

// validBucketName matches S3 bucket naming rules: 3-63 chars, lowercase alphanumeric + hyphens.
var validBucketName = regexp.MustCompile(`^[a-z0-9][a-z0-9\-]{1,61}[a-z0-9]$`)

func RunBucketCreate(database *db.DB, name, storageRoot, customStorageDir string) error {
	if !validBucketName.MatchString(name) {
		return fmt.Errorf("invalid bucket name '%s': must be 3-63 lowercase alphanumeric chars and hyphens", name)
	}

	if customStorageDir != "" && !filepath.IsAbs(customStorageDir) {
		return fmt.Errorf("--storage-dir must be an absolute path, got: %s", customStorageDir)
	}

	existing, err := database.GetBucket(name)
	if err != nil {
		return fmt.Errorf("check bucket: %w", err)
	}
	if existing != nil {
		return fmt.Errorf("bucket '%s' already exists", name)
	}

	bucket, err := database.CreateBucket(name, customStorageDir)
	if err != nil {
		return fmt.Errorf("create bucket: %w", err)
	}

	// Create storage directory with safe path joining and restrictive permissions
	base := storageRoot
	if customStorageDir != "" {
		base = customStorageDir
	}
	storagePath := filepath.Join(base, name)
	if err := os.MkdirAll(storagePath, 0700); err != nil {
		return fmt.Errorf("create storage dir: %w", err)
	}

	pterm.Success.Printfln("Bucket '%s' created (id=%d)", bucket.Name, bucket.ID)
	pterm.Info.Printfln("Storage: %s/", storagePath)
	if customStorageDir != "" {
		pterm.Info.Println("Custom storage directory")
		pterm.Warning.Println(restartWarning)
		pterm.Info.Println("systemd: add the directory to ReadWritePaths= in a drop-in, see cloodsys3.service.")
	}
	return nil
}

func RunBucketList(database *db.DB) error {
	buckets, err := database.ListBuckets()
	if err != nil {
		return fmt.Errorf("list buckets: %w", err)
	}

	if len(buckets) == 0 {
		pterm.Warning.Println("No buckets found.")
		return nil
	}

	tableData := pterm.TableData{{"ID", "NAME", "CREATED"}}
	for _, b := range buckets {
		tableData = append(tableData, []string{
			fmt.Sprintf("%d", b.ID), b.Name, b.CreatedAt.Format("2006-01-02 15:04:05"),
		})
	}
	pterm.DefaultTable.WithHasHeader().WithData(tableData).Render()
	return nil
}

// RunBucketDelete removes a bucket. Unless force is set it refuses when any
// object row (current, noncurrent version or delete marker) still exists.
// On success the bucket directory and the sibling .<bucket>-multipart and
// .<bucket>-cache trees are removed from disk.
func RunBucketDelete(database *db.DB, name, storageRoot string, force bool) error {
	if !validBucketName.MatchString(name) {
		return fmt.Errorf("invalid bucket name '%s'", name)
	}

	bucket, err := database.GetBucket(name)
	if err != nil {
		return fmt.Errorf("get bucket: %w", err)
	}
	if bucket == nil {
		return fmt.Errorf("bucket '%s' not found", name)
	}

	hasRows, err := database.BucketHasAnyRows(bucket.ID)
	if err != nil {
		return fmt.Errorf("check objects: %w", err)
	}
	if hasRows && !force {
		return fmt.Errorf("bucket '%s' still contains objects, noncurrent versions or delete markers; re-run with --force to delete everything", name)
	}

	// Resolve the on-disk location before the row disappears.
	store, err := storage.NewFileSystem(storageRoot)
	if err != nil {
		return fmt.Errorf("open storage: %w", err)
	}
	if bucket.StorageDir != "" {
		store.LoadBucketDirs(map[string]string{name: bucket.StorageDir})
	}

	if err := database.DeleteBucket(name); err != nil {
		return fmt.Errorf("delete bucket: %w", err)
	}

	// Removes <base>/<bucket>, <base>/.<bucket>-multipart and <base>/.<bucket>-cache.
	if err := store.DeleteBucketDir(name); err != nil {
		pterm.Warning.Printfln("Bucket row deleted but storage cleanup failed: %v", err)
		pterm.Info.Printfln("Remove the directories manually under %s", storageBase(storageRoot, bucket.StorageDir))
		return nil
	}

	if hasRows {
		pterm.Success.Printfln("Bucket '%s' and all of its data deleted.", name)
	} else {
		pterm.Success.Printfln("Bucket '%s' deleted.", name)
	}
	return nil
}

// storageBase returns the effective base directory for a bucket.
func storageBase(storageRoot, custom string) string {
	if custom != "" {
		return custom
	}
	return storageRoot
}

// isCrossDevice reports whether a rename failed because source and target
// live on different filesystems.
func isCrossDevice(err error) bool {
	if errors.Is(err, syscall.EXDEV) {
		return true
	}
	var errno syscall.Errno
	if runtime.GOOS == "windows" && errors.As(err, &errno) && errno == 17 { // ERROR_NOT_SAME_DEVICE
		return true
	}
	return false
}

// moveTree renames src to dst when src exists. It returns (moved, error).
func moveTree(src, dst string) (bool, error) {
	if _, err := os.Stat(src); err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	if _, err := os.Stat(dst); err == nil {
		return false, fmt.Errorf("target '%s' already exists", dst)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0700); err != nil {
		return false, err
	}
	if err := os.Rename(src, dst); err != nil {
		return false, err
	}
	return true, nil
}

// RunBucketStorageDir moves a bucket (its data directory plus the sibling
// .<bucket>-multipart and .<bucket>-cache trees) to a new base directory and
// records the new location. Moves are done with rename and therefore must stay
// on one filesystem; cross-device moves are refused with manual instructions.
func RunBucketStorageDir(database *db.DB, name, storageRoot, newDir string) error {
	if newDir != "" && !filepath.IsAbs(newDir) {
		return fmt.Errorf("--dir must be an absolute path, got: %s", newDir)
	}
	if !validBucketName.MatchString(name) {
		return fmt.Errorf("invalid bucket name '%s'", name)
	}

	bucket, err := database.GetBucket(name)
	if err != nil {
		return fmt.Errorf("get bucket: %w", err)
	}
	if bucket == nil {
		return fmt.Errorf("bucket '%s' not found", name)
	}

	oldBase := storageBase(storageRoot, bucket.StorageDir)
	newBase := storageBase(storageRoot, newDir)
	if abs, err := filepath.Abs(oldBase); err == nil {
		oldBase = abs
	}
	if abs, err := filepath.Abs(newBase); err == nil {
		newBase = abs
	}
	if oldBase == newBase {
		pterm.Info.Printfln("Storage directory is already '%s', nothing to change.", filepath.Join(oldBase, name))
		return nil
	}

	trees := []string{name, "." + name + "-multipart", "." + name + "-cache"}
	var moved []string
	rollback := func() {
		for i := len(moved) - 1; i >= 0; i-- {
			os.Rename(filepath.Join(newBase, moved[i]), filepath.Join(oldBase, moved[i]))
		}
	}

	if err := os.MkdirAll(newBase, 0700); err != nil {
		return fmt.Errorf("create new storage dir: %w", err)
	}
	for _, t := range trees {
		src := filepath.Join(oldBase, t)
		dst := filepath.Join(newBase, t)
		ok, err := moveTree(src, dst)
		if err != nil {
			rollback()
			if isCrossDevice(err) {
				return fmt.Errorf("cannot move '%s' to '%s': source and target are on different filesystems.\n"+
					"Cross-filesystem moves are not done automatically. Stop the server, copy the trees manually, e.g.:\n"+
					"  cp -a '%s' '%s/'\n"+
					"  cp -a '%s' '%s/'   # if it exists\n"+
					"  cp -a '%s' '%s/'   # if it exists\n"+
					"verify the copy, delete the three old trees under '%s', then re-run this command to record the new location",
					src, dst,
					filepath.Join(oldBase, trees[0]), newBase,
					filepath.Join(oldBase, trees[1]), newBase,
					filepath.Join(oldBase, trees[2]), newBase,
					oldBase)
			}
			return fmt.Errorf("move '%s': %w (nothing changed)", t, err)
		}
		if ok {
			moved = append(moved, t)
		}
	}
	if len(moved) == 0 {
		// Nothing on disk yet: make sure the new data dir exists.
		if err := os.MkdirAll(filepath.Join(newBase, name), 0700); err != nil {
			return fmt.Errorf("create new storage dir: %w", err)
		}
	}

	if err := database.SetBucketStorageDir(name, newDir); err != nil {
		rollback()
		return fmt.Errorf("update database: %w (files moved back)", err)
	}

	newPath := filepath.Join(newBase, name)
	if newDir != "" {
		pterm.Success.Printfln("Bucket '%s' storage moved to: %s/ (custom)", name, newPath)
	} else {
		pterm.Success.Printfln("Bucket '%s' storage moved back to default: %s/", name, newPath)
	}
	if len(moved) > 0 {
		pterm.Info.Printfln("Moved: %s", strings.Join(moved, ", "))
	}
	pterm.Warning.Println(restartWarning)
	if newDir != "" {
		pterm.Info.Println("systemd: add the directory to ReadWritePaths= in a drop-in, see cloodsys3.service.")
	}
	return nil
}

func RunBucketInfo(database *db.DB, name, storageRoot string) error {
	bucket, err := database.GetBucket(name)
	if err != nil {
		return fmt.Errorf("get bucket: %w", err)
	}
	if bucket == nil {
		return fmt.Errorf("bucket '%s' not found", name)
	}

	objCount, err := database.CountObjects(bucket.ID)
	if err != nil {
		return fmt.Errorf("count objects: %w", err)
	}

	creds, err := database.ListCredentials(bucket.ID)
	if err != nil {
		return fmt.Errorf("list credentials: %w", err)
	}

	usage, err := database.GetBucketUsage(bucket.ID)
	if err != nil {
		return fmt.Errorf("get usage: %w", err)
	}

	storagePath := filepath.Join(storageRoot, bucket.Name) + string(filepath.Separator)
	storageLabel := storagePath
	if bucket.StorageDir != "" {
		storagePath = filepath.Join(bucket.StorageDir, bucket.Name) + string(filepath.Separator)
		storageLabel = storagePath + " (custom)"
	}

	quotaStr := pterm.Green("unlimited")
	if bucket.QuotaBytes > 0 {
		pct := float64(usage) / float64(bucket.QuotaBytes) * 100
		quotaStr = fmt.Sprintf("%s (%.1f%% used)", formatBytes(bucket.QuotaBytes), pct)
	}

	pterm.DefaultSection.Println(bucket.Name)
	bulletItems := []pterm.BulletListItem{
		{Level: 0, Text: pterm.Gray("ID:          ") + fmt.Sprintf("%d", bucket.ID)},
		{Level: 0, Text: pterm.Gray("Created:     ") + bucket.CreatedAt.Format("2006-01-02 15:04:05")},
		{Level: 0, Text: pterm.Gray("Storage:     ") + storageLabel},
		{Level: 0, Text: pterm.Gray("Objects:     ") + fmt.Sprintf("%d", objCount)},
		{Level: 0, Text: pterm.Gray("Usage:       ") + formatBytes(usage)},
		{Level: 0, Text: pterm.Gray("Quota:       ") + quotaStr},
		{Level: 0, Text: pterm.Gray("Credentials: ") + fmt.Sprintf("%d", len(creds))},
	}
	pterm.DefaultBulletList.WithItems(bulletItems).Render()

	if len(creds) > 0 {
		pterm.Println()
		tableData := pterm.TableData{{"ACCESS KEY", "PERMISSION", "CREATED"}}
		for _, c := range creds {
			tableData = append(tableData, []string{c.AccessKey, c.Permission, c.CreatedAt.Format("2006-01-02 15:04:05")})
		}
		pterm.DefaultTable.WithHasHeader().WithData(tableData).Render()
	}

	return nil
}

func RunBucketQuota(database *db.DB, name, sizeStr string) error {
	bucket, err := database.GetBucket(name)
	if err != nil {
		return fmt.Errorf("get bucket: %w", err)
	}
	if bucket == nil {
		return fmt.Errorf("bucket '%s' not found", name)
	}

	quotaBytes, err := parseSize(sizeStr)
	if err != nil {
		return fmt.Errorf("invalid size '%s': %w", sizeStr, err)
	}

	if err := database.SetBucketQuota(name, quotaBytes); err != nil {
		return fmt.Errorf("set quota: %w", err)
	}

	if quotaBytes == 0 {
		pterm.Success.Printfln("Quota removed for bucket '%s'.", name)
	} else {
		pterm.Success.Printfln("Quota set for bucket '%s': %s", name, formatBytes(quotaBytes))
	}
	return nil
}

func RunCredentialCreate(database *db.DB, bucketName string, readOnly bool) error {
	bucket, err := database.GetBucket(bucketName)
	if err != nil {
		return fmt.Errorf("get bucket: %w", err)
	}
	if bucket == nil {
		return fmt.Errorf("bucket '%s' not found", bucketName)
	}

	accessKey, err := auth.GenerateAccessKey()
	if err != nil {
		return fmt.Errorf("generate access key: %w", err)
	}
	secretKey, err := auth.GenerateSecretKey()
	if err != nil {
		return fmt.Errorf("generate secret key: %w", err)
	}

	permission := "read-write"
	if readOnly {
		permission = "read-only"
	}

	_, err = database.CreateCredential(bucket.ID, "", accessKey, secretKey, permission)
	if err != nil {
		return fmt.Errorf("create credential: %w", err)
	}

	pterm.Success.Println("Credential created")
	panel := pterm.DefaultBox.WithTitle("Credentials").Sprint(
		pterm.Gray("Bucket:     ") + bucketName + "\n" +
			pterm.Gray("Access Key: ") + pterm.Cyan(accessKey) + "\n" +
			pterm.Gray("Secret Key: ") + pterm.Cyan(secretKey) + "\n" +
			pterm.Gray("Permission: ") + permission,
	)
	pterm.Println(panel)
	pterm.Warning.Println("Save the secret key now. It will not be shown again.")

	return nil
}

func RunCredentialList(database *db.DB, bucketName string) error {
	bucket, err := database.GetBucket(bucketName)
	if err != nil {
		return fmt.Errorf("get bucket: %w", err)
	}
	if bucket == nil {
		return fmt.Errorf("bucket '%s' not found", bucketName)
	}

	creds, err := database.ListCredentials(bucket.ID)
	if err != nil {
		return fmt.Errorf("list credentials: %w", err)
	}

	if len(creds) == 0 {
		pterm.Warning.Printfln("No credentials for bucket '%s'.", bucketName)
		return nil
	}

	tableData := pterm.TableData{{"ACCESS KEY", "PERMISSION", "CREATED"}}
	for _, c := range creds {
		tableData = append(tableData, []string{c.AccessKey, c.Permission, c.CreatedAt.Format("2006-01-02 15:04:05")})
	}
	pterm.DefaultTable.WithHasHeader().WithData(tableData).Render()
	return nil
}

func RunCredentialDelete(database *db.DB, accessKey string) error {
	cred, err := database.GetCredentialByAccessKey(accessKey)
	if err != nil {
		return fmt.Errorf("get credential: %w", err)
	}
	if cred == nil {
		return fmt.Errorf("access key '%s' not found", accessKey)
	}

	if err := database.DeleteCredential(accessKey); err != nil {
		return fmt.Errorf("delete credential: %w", err)
	}

	pterm.Success.Printfln("Credential '%s' deleted.", accessKey)
	return nil
}

// Versioning subcommands

func RunBucketVersioningEnable(database *db.DB, name string) error {
	bucket, err := database.GetBucket(name)
	if err != nil {
		return fmt.Errorf("get bucket: %w", err)
	}
	if bucket == nil {
		return fmt.Errorf("bucket '%s' not found", name)
	}
	if err := database.SetBucketVersioning(name, "Enabled"); err != nil {
		return fmt.Errorf("set versioning: %w", err)
	}
	pterm.Success.Printfln("Versioning enabled for bucket '%s'.", name)
	return nil
}

func RunBucketVersioningSuspend(database *db.DB, name string) error {
	bucket, err := database.GetBucket(name)
	if err != nil {
		return fmt.Errorf("get bucket: %w", err)
	}
	if bucket == nil {
		return fmt.Errorf("bucket '%s' not found", name)
	}
	if err := database.SetBucketVersioning(name, "Suspended"); err != nil {
		return fmt.Errorf("set versioning: %w", err)
	}
	pterm.Success.Printfln("Versioning suspended for bucket '%s'.", name)
	return nil
}

func RunBucketVersioningStatus(database *db.DB, name string) error {
	bucket, err := database.GetBucket(name)
	if err != nil {
		return fmt.Errorf("get bucket: %w", err)
	}
	if bucket == nil {
		return fmt.Errorf("bucket '%s' not found", name)
	}
	status := bucket.Versioning
	if status == "" {
		status = "Disabled"
	}
	pterm.Info.Printfln("Bucket '%s' versioning: %s", name, pterm.Cyan(status))
	return nil
}

// RunBucketPublicRead toggles anonymous object read access for a bucket.
// action is one of "enable", "disable", "status".
func RunBucketPublicRead(database *db.DB, name, action string) error {
	bucket, err := database.GetBucket(name)
	if err != nil {
		return fmt.Errorf("get bucket: %w", err)
	}
	if bucket == nil {
		return fmt.Errorf("bucket '%s' not found", name)
	}

	switch action {
	case "enable":
		if err := database.SetBucketPublicRead(name, true); err != nil {
			return fmt.Errorf("set public-read: %w", err)
		}
		pterm.Success.Printfln("Public read enabled for bucket '%s'. Anonymous GET/HEAD is now allowed.", name)
	case "disable":
		if err := database.SetBucketPublicRead(name, false); err != nil {
			return fmt.Errorf("set public-read: %w", err)
		}
		pterm.Success.Printfln("Public read disabled for bucket '%s'.", name)
	case "status":
		status := "Disabled"
		if bucket.PublicRead {
			status = "Enabled"
		}
		pterm.Info.Printfln("Bucket '%s' public-read: %s", name, pterm.Cyan(status))
	default:
		return fmt.Errorf("unknown public-read action: %s (use enable|disable|status)", action)
	}
	return nil
}

// RunBucketReprocess (re)generates optimized image variants for existing
// objects in a bucket. Originals are never modified. Useful after enabling
// image optimization on a bucket that already has content.
func RunBucketReprocess(database *db.DB, store storage.Backend, cfg *config.Config, name, prefix string) error {
	bucket, err := database.GetBucket(name)
	if err != nil {
		return fmt.Errorf("get bucket: %w", err)
	}
	if bucket == nil {
		return fmt.Errorf("bucket '%s' not found", name)
	}

	quality := cfg.Image.Quality
	if quality <= 0 || quality > 100 {
		quality = 75
	}

	var processed, skipped, failed int
	marker := ""
	for {
		// No delimiter → flat listing of all keys under the prefix.
		objects, _, truncated, next, err := database.ListObjectsMeta(bucket.ID, prefix, marker, "", 1000)
		if err != nil {
			return fmt.Errorf("list objects: %w", err)
		}
		for i := range objects {
			m := objects[i]
			if m.IsDeleteMarker || !imageutil.IsImageContentType(m.ContentType) {
				skipped++
				continue
			}
			job := imageutil.Job{
				Bucket:      name,
				Key:         m.Key,
				VersionID:   m.VersionID,
				ETag:        m.ETag,
				ContentType: m.ContentType,
			}
			if err := imageutil.OptimizeOne(store, job, quality, cfg.Image.MaxSourceBytes); err != nil {
				failed++
				pterm.Warning.Printfln("  %s: %v", m.Key, err)
				continue
			}
			processed++
		}
		if !truncated {
			break
		}
		marker = next
	}

	pterm.Success.Printfln("Reprocess complete for '%s': %d optimized, %d skipped, %d failed.", name, processed, skipped, failed)
	return nil
}

// RunBucketWebDAV toggles WebDAV mountability for a bucket. Effective only when
// the global WebDAV server is enabled in config. action: enable|disable|status.
func RunBucketWebDAV(database *db.DB, name, action string) error {
	bucket, err := database.GetBucket(name)
	if err != nil {
		return fmt.Errorf("get bucket: %w", err)
	}
	if bucket == nil {
		return fmt.Errorf("bucket '%s' not found", name)
	}

	switch action {
	case "enable":
		if err := database.SetBucketWebDAVEnabled(name, true); err != nil {
			return fmt.Errorf("set webdav: %w", err)
		}
		pterm.Success.Printfln("WebDAV enabled for bucket '%s' (requires global webdav.enabled).", name)
	case "disable":
		if err := database.SetBucketWebDAVEnabled(name, false); err != nil {
			return fmt.Errorf("set webdav: %w", err)
		}
		pterm.Success.Printfln("WebDAV disabled for bucket '%s'.", name)
	case "status":
		status := "Disabled"
		if bucket.WebDAVEnabled {
			status = "Enabled"
		}
		pterm.Info.Printfln("Bucket '%s' webdav: %s", name, pterm.Cyan(status))
	default:
		return fmt.Errorf("unknown webdav action: %s (use enable|disable|status)", action)
	}
	return nil
}

// Lifecycle subcommands

func RunBucketLifecycleSet(database *db.DB, name, prefix string, days int) error {
	bucket, err := database.GetBucket(name)
	if err != nil {
		return fmt.Errorf("get bucket: %w", err)
	}
	if bucket == nil {
		return fmt.Errorf("bucket '%s' not found", name)
	}
	if days <= 0 {
		return fmt.Errorf("expiration days must be positive")
	}
	if err := database.PutLifecycleRule(name, "", prefix, days); err != nil {
		return fmt.Errorf("set lifecycle rule: %w", err)
	}
	if prefix == "" {
		pterm.Success.Printfln("Lifecycle rule set for bucket '%s': expire after %d days (all objects).", name, days)
	} else {
		pterm.Success.Printfln("Lifecycle rule set for bucket '%s': prefix='%s', expire after %d days.", name, prefix, days)
	}
	return nil
}

func RunBucketLifecycleGet(database *db.DB, name string) error {
	bucket, err := database.GetBucket(name)
	if err != nil {
		return fmt.Errorf("get bucket: %w", err)
	}
	if bucket == nil {
		return fmt.Errorf("bucket '%s' not found", name)
	}
	rules, err := database.GetLifecycleRules(name)
	if err != nil {
		return fmt.Errorf("get lifecycle rules: %w", err)
	}
	if len(rules) == 0 {
		pterm.Warning.Printfln("No lifecycle rules for bucket '%s'.", name)
		return nil
	}
	tableData := pterm.TableData{{"ID", "PREFIX", "STATUS", "EXPIRE (DAYS)", "NONCURRENT (DAYS)", "ABORT MPU (DAYS)", "EXPIRE MARKERS", "CREATED"}}
	dash := func(n int) string {
		if n <= 0 {
			return "-"
		}
		return strconv.Itoa(n)
	}
	for _, r := range rules {
		prefix := r.Prefix
		if prefix == "" {
			prefix = "(all)"
		}
		status := r.Status
		if status == "" {
			status = "Enabled"
		}
		if status == "Enabled" {
			status = pterm.Green(status)
		} else {
			status = pterm.Yellow(status)
		}
		id := r.Name
		if id == "" {
			id = "-"
		}
		markers := "no"
		if r.ExpireDeleteMarkers {
			markers = "yes"
		}
		tableData = append(tableData, []string{
			id, prefix, status, dash(r.ExpirationDays), dash(r.NoncurrentDays), dash(r.AbortMultipartDays), markers,
			r.CreatedAt.Format("2006-01-02 15:04:05"),
		})
	}
	pterm.DefaultTable.WithHasHeader().WithData(tableData).Render()
	return nil
}

func RunBucketLifecycleDelete(database *db.DB, name, prefix string) error {
	bucket, err := database.GetBucket(name)
	if err != nil {
		return fmt.Errorf("get bucket: %w", err)
	}
	if bucket == nil {
		return fmt.Errorf("bucket '%s' not found", name)
	}
	if prefix != "" {
		if err := database.DeleteLifecycleRuleByPrefix(name, prefix); err != nil {
			return fmt.Errorf("delete lifecycle rule: %w", err)
		}
		pterm.Success.Printfln("Lifecycle rule with prefix '%s' deleted for bucket '%s'.", prefix, name)
	} else {
		if err := database.DeleteLifecycleRules(name); err != nil {
			return fmt.Errorf("delete lifecycle rules: %w", err)
		}
		pterm.Success.Printfln("All lifecycle rules deleted for bucket '%s'.", name)
	}
	return nil
}

// Webhook subcommands

func RunBucketWebhookAdd(database *db.DB, name, url, events, secret string) error {
	bucket, err := database.GetBucket(name)
	if err != nil {
		return fmt.Errorf("get bucket: %w", err)
	}
	if bucket == nil {
		return fmt.Errorf("bucket '%s' not found", name)
	}
	if url == "" {
		return fmt.Errorf("--url is required")
	}
	// The CLI runs on the host itself, so private/loopback targets are allowed here.
	if err := webhook.ValidateURL(url, true); err != nil {
		return err
	}
	if secret != "" {
		warnArgvSecret("--secret")
	}
	hook, err := database.CreateWebhook(name, "", url, events, secret)
	if err != nil {
		return fmt.Errorf("create webhook: %w", err)
	}
	pterm.Success.Printfln("Webhook created for bucket '%s' (id=%d)", name, hook.ID)
	pterm.Info.Printfln("URL:    %s", hook.URL)
	pterm.Info.Printfln("Events: %s", hook.EventTypes)
	if secret != "" {
		pterm.Info.Println("Secret: (set, not shown)")
	}
	return nil
}

func RunBucketWebhookList(database *db.DB, name string) error {
	bucket, err := database.GetBucket(name)
	if err != nil {
		return fmt.Errorf("get bucket: %w", err)
	}
	if bucket == nil {
		return fmt.Errorf("bucket '%s' not found", name)
	}
	hooks, err := database.ListWebhooks(name)
	if err != nil {
		return fmt.Errorf("list webhooks: %w", err)
	}
	if len(hooks) == 0 {
		pterm.Warning.Printfln("No webhooks for bucket '%s'.", name)
		return nil
	}
	tableData := pterm.TableData{{"ID", "URL", "EVENTS", "ACTIVE", "CREATED"}}
	for _, h := range hooks {
		tableData = append(tableData, []string{
			fmt.Sprintf("%d", h.ID), h.URL, h.EventTypes, fmt.Sprintf("%v", h.Active), h.CreatedAt.Format("2006-01-02 15:04:05"),
		})
	}
	pterm.DefaultTable.WithHasHeader().WithData(tableData).Render()
	return nil
}

func RunBucketWebhookDelete(database *db.DB, name string, id int64) error {
	bucket, err := database.GetBucket(name)
	if err != nil {
		return fmt.Errorf("get bucket: %w", err)
	}
	if bucket == nil {
		return fmt.Errorf("bucket '%s' not found", name)
	}
	if err := database.DeleteWebhook(id); err != nil {
		return fmt.Errorf("delete webhook: %w", err)
	}
	pterm.Success.Printfln("Webhook %d deleted for bucket '%s'.", id, name)
	return nil
}

// parseSize parses a human-readable size string (e.g., "10GB", "500MB", "0") into bytes.
func parseSize(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "0" {
		return 0, nil
	}

	s = strings.ToUpper(s)
	multiplier := int64(1)

	switch {
	case strings.HasSuffix(s, "TB"):
		multiplier = 1024 * 1024 * 1024 * 1024
		s = strings.TrimSuffix(s, "TB")
	case strings.HasSuffix(s, "GB"):
		multiplier = 1024 * 1024 * 1024
		s = strings.TrimSuffix(s, "GB")
	case strings.HasSuffix(s, "MB"):
		multiplier = 1024 * 1024
		s = strings.TrimSuffix(s, "MB")
	case strings.HasSuffix(s, "KB"):
		multiplier = 1024
		s = strings.TrimSuffix(s, "KB")
	}

	val, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil {
		return 0, fmt.Errorf("invalid number: %s", s)
	}
	if val < 0 {
		return 0, fmt.Errorf("size cannot be negative")
	}
	return int64(val * float64(multiplier)), nil
}

// formatBytes formats a byte count into a human-readable string.
func formatBytes(b int64) string {
	const (
		kb = 1024
		mb = kb * 1024
		gb = mb * 1024
		tb = gb * 1024
	)
	switch {
	case b >= tb:
		return fmt.Sprintf("%.2f TB", float64(b)/float64(tb))
	case b >= gb:
		return fmt.Sprintf("%.2f GB", float64(b)/float64(gb))
	case b >= mb:
		return fmt.Sprintf("%.2f MB", float64(b)/float64(mb))
	case b >= kb:
		return fmt.Sprintf("%.2f KB", float64(b)/float64(kb))
	default:
		return fmt.Sprintf("%d B", b)
	}
}

// Admin CLI commands

// RunAdminCreate creates an admin user. The password comes from --password
// (discouraged: visible in argv), --generate (printed once), or an
// interactive no-echo prompt when neither is given.
func RunAdminCreate(database *db.DB, username, customPassword string, generate bool) error {
	if username == "" {
		return fmt.Errorf("username is required")
	}

	existing, err := database.GetAdmin(username)
	if err != nil {
		return fmt.Errorf("check admin: %w", err)
	}
	if existing != nil {
		return fmt.Errorf("admin '%s' already exists", username)
	}

	password, generated, err := resolvePassword(customPassword, generate)
	if err != nil {
		return err
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(password), 12)
	if err != nil {
		return fmt.Errorf("hash password: %w", err)
	}

	if _, err := database.CreateAdmin(username, string(hash)); err != nil {
		return fmt.Errorf("create admin: %w", err)
	}

	pterm.Success.Printfln("Admin user '%s' created.", username)
	if generated {
		panel := pterm.DefaultBox.WithTitle("Admin Credentials").Sprint(
			pterm.Gray("Username: ") + pterm.Cyan(username) + "\n" +
				pterm.Gray("Password: ") + pterm.Cyan(password),
		)
		pterm.Println(panel)
		pterm.Warning.Println("Save the password now. It will not be shown again.")
	}
	return nil
}

func RunAdminList(database *db.DB) error {
	admins, err := database.ListAdmins()
	if err != nil {
		return fmt.Errorf("list admins: %w", err)
	}

	if len(admins) == 0 {
		pterm.Warning.Println("No admin users found.")
		return nil
	}

	tableData := pterm.TableData{{"ID", "USERNAME", "CREATED"}}
	for _, a := range admins {
		tableData = append(tableData, []string{
			fmt.Sprintf("%d", a.ID), a.Username, a.CreatedAt.Format("2006-01-02 15:04:05"),
		})
	}
	pterm.DefaultTable.WithHasHeader().WithData(tableData).Render()
	return nil
}

func RunAdminDelete(database *db.DB, username string) error {
	existing, err := database.GetAdmin(username)
	if err != nil {
		return fmt.Errorf("get admin: %w", err)
	}
	if existing == nil {
		return fmt.Errorf("admin '%s' not found", username)
	}

	count, err := database.CountAdmins()
	if err != nil {
		return fmt.Errorf("count admins: %w", err)
	}
	if count <= 1 {
		return fmt.Errorf("cannot delete the last admin")
	}

	if err := database.DeleteAdmin(username); err != nil {
		return fmt.Errorf("delete admin: %w", err)
	}

	pterm.Success.Printfln("Admin '%s' deleted.", username)
	return nil
}

// RunAdminPassword resets an admin's password; see RunAdminCreate for the
// password sources. A custom password is never echoed back.
func RunAdminPassword(database *db.DB, username, customPassword string, generate bool) error {
	existing, err := database.GetAdmin(username)
	if err != nil {
		return fmt.Errorf("get admin: %w", err)
	}
	if existing == nil {
		return fmt.Errorf("admin '%s' not found", username)
	}

	password, generated, err := resolvePassword(customPassword, generate)
	if err != nil {
		return err
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(password), 12)
	if err != nil {
		return fmt.Errorf("hash password: %w", err)
	}

	if err := database.UpdateAdminPassword(username, string(hash)); err != nil {
		return fmt.Errorf("update password: %w", err)
	}

	pterm.Success.Printfln("Password updated for '%s'.", username)
	if generated {
		pterm.Info.Printfln("Password: %s", pterm.Cyan(password))
		pterm.Warning.Println("Save the password now. It will not be shown again.")
	}
	return nil
}
