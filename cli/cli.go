package cli

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/onaonbir/Cloodsy-S3/auth"
	"github.com/onaonbir/Cloodsy-S3/db"
	"github.com/pterm/pterm"
	"golang.org/x/crypto/bcrypt"
)

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
		pterm.Info.Printfln("Custom storage directory")
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

func RunBucketDelete(database *db.DB, name, storageRoot string) error {
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

	hasObjects, err := database.BucketHasObjects(bucket.ID)
	if err != nil {
		return fmt.Errorf("check objects: %w", err)
	}
	if hasObjects {
		return fmt.Errorf("bucket '%s' is not empty", name)
	}

	if err := database.DeleteBucket(name); err != nil {
		return fmt.Errorf("delete bucket: %w", err)
	}

	// Remove storage directory using safe path join
	base := storageRoot
	if bucket.StorageDir != "" {
		base = bucket.StorageDir
	}
	storagePath := filepath.Join(base, name)
	os.RemoveAll(storagePath)

	// Also wipe the sibling multipart staging tree so abandoned parts don't
	// linger on disk. DB rows for active uploads are cascade-deleted along
	// with the bucket; this just cleans the matching files.
	multipartPath := filepath.Join(base, "."+name+"-multipart")
	os.RemoveAll(multipartPath)

	pterm.Success.Printfln("Bucket '%s' deleted.", name)
	return nil
}

func RunBucketStorageDir(database *db.DB, name, storageRoot, newDir string) error {
	if newDir != "" && !filepath.IsAbs(newDir) {
		return fmt.Errorf("--dir must be an absolute path, got: %s", newDir)
	}

	bucket, err := database.GetBucket(name)
	if err != nil {
		return fmt.Errorf("get bucket: %w", err)
	}
	if bucket == nil {
		return fmt.Errorf("bucket '%s' not found", name)
	}

	// Determine old and new base paths
	oldBase := storageRoot
	if bucket.StorageDir != "" {
		oldBase = bucket.StorageDir
	}
	newBase := storageRoot
	if newDir != "" {
		newBase = newDir
	}

	oldPath := filepath.Join(oldBase, name)
	newPath := filepath.Join(newBase, name)

	if oldPath == newPath {
		fmt.Printf("Storage directory is already '%s', nothing to change.\n", oldPath)
		return nil
	}

	// Ensure new parent directory exists
	if err := os.MkdirAll(newPath, 0700); err != nil {
		return fmt.Errorf("create new storage dir: %w", err)
	}

	// Move files from old to new location
	entries, err := os.ReadDir(oldPath)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("read old storage dir: %w", err)
	}

	movedCount := 0
	for _, entry := range entries {
		src := filepath.Join(oldPath, entry.Name())
		dst := filepath.Join(newPath, entry.Name())
		if err := os.Rename(src, dst); err != nil {
			return fmt.Errorf("move '%s': %w (partial move, %d files moved)", entry.Name(), err, movedCount)
		}
		movedCount++
	}

	// Remove old directory (now empty)
	os.RemoveAll(oldPath)

	// Move the sibling multipart staging tree, if any, so cleanup keeps finding it.
	oldMultipart := filepath.Join(oldBase, "."+name+"-multipart")
	newMultipart := filepath.Join(newBase, "."+name+"-multipart")
	if oldMultipart != newMultipart {
		if _, err := os.Stat(oldMultipart); err == nil {
			if err := os.Rename(oldMultipart, newMultipart); err != nil {
				return fmt.Errorf("move multipart staging: %w", err)
			}
		}
	}

	// Update DB
	if err := database.SetBucketStorageDir(name, newDir); err != nil {
		return fmt.Errorf("update database: %w", err)
	}

	if newDir != "" {
		pterm.Success.Printfln("Bucket '%s' storage moved to: %s/ (custom)", name, newPath)
	} else {
		pterm.Success.Printfln("Bucket '%s' storage moved back to default: %s/", name, newPath)
	}
	if movedCount > 0 {
		pterm.Info.Printfln("Moved %d items.", movedCount)
	}
	pterm.Warning.Println("Restart the server for changes to take effect.")
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

	storagePath := storageRoot + "/" + bucket.Name + "/"
	storageLabel := storagePath
	if bucket.StorageDir != "" {
		storagePath = bucket.StorageDir + "/" + bucket.Name + "/"
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
	tableData := pterm.TableData{{"PREFIX", "EXPIRATION DAYS", "CREATED"}}
	for _, r := range rules {
		prefix := r.Prefix
		if prefix == "" {
			prefix = "(all)"
		}
		tableData = append(tableData, []string{prefix, fmt.Sprintf("%d", r.ExpirationDays), r.CreatedAt.Format("2006-01-02 15:04:05")})
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
	hook, err := database.CreateWebhook(name, "", url, events, secret)
	if err != nil {
		return fmt.Errorf("create webhook: %w", err)
	}
	pterm.Success.Printfln("Webhook created for bucket '%s' (id=%d)", name, hook.ID)
	pterm.Info.Printfln("URL:    %s", hook.URL)
	pterm.Info.Printfln("Events: %s", hook.EventTypes)
	if secret != "" {
		pterm.Info.Printfln("Secret: %s", secret)
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

func generatePassword() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func RunAdminCreate(database *db.DB, username, customPassword string) error {
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

	password := customPassword
	if password == "" {
		password, err = generatePassword()
		if err != nil {
			return fmt.Errorf("generate password: %w", err)
		}
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(password), 12)
	if err != nil {
		return fmt.Errorf("hash password: %w", err)
	}

	_, err = database.CreateAdmin(username, string(hash))
	if err != nil {
		return fmt.Errorf("create admin: %w", err)
	}

	pterm.Success.Println("Admin user created")
	panel := pterm.DefaultBox.WithTitle("Admin Credentials").Sprint(
		pterm.Gray("Username: ") + pterm.Cyan(username) + "\n" +
			pterm.Gray("Password: ") + pterm.Cyan(password),
	)
	pterm.Println(panel)
	if customPassword == "" {
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

	if err := database.DeleteAdmin(username); err != nil {
		return fmt.Errorf("delete admin: %w", err)
	}

	pterm.Success.Printfln("Admin '%s' deleted.", username)
	return nil
}

func RunAdminPassword(database *db.DB, username, customPassword string) error {
	existing, err := database.GetAdmin(username)
	if err != nil {
		return fmt.Errorf("get admin: %w", err)
	}
	if existing == nil {
		return fmt.Errorf("admin '%s' not found", username)
	}

	password := customPassword
	if password == "" {
		password, err = generatePassword()
		if err != nil {
			return fmt.Errorf("generate password: %w", err)
		}
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(password), 12)
	if err != nil {
		return fmt.Errorf("hash password: %w", err)
	}

	if err := database.UpdateAdminPassword(username, string(hash)); err != nil {
		return fmt.Errorf("update password: %w", err)
	}

	pterm.Success.Printfln("Password updated for '%s'.", username)
	pterm.Info.Printfln("Password: %s", pterm.Cyan(password))
	if customPassword == "" {
		pterm.Warning.Println("Save the password now. It will not be shown again.")
	}
	return nil
}
