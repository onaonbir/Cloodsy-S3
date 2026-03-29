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
	"text/tabwriter"

	"github.com/onaonbir/Cloodsy-S3/auth"
	"github.com/onaonbir/Cloodsy-S3/db"
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

	if customStorageDir != "" {
		fmt.Printf("Bucket '%s' created (id=%d). Storage: %s/ (custom)\n", bucket.Name, bucket.ID, storagePath)
	} else {
		fmt.Printf("Bucket '%s' created (id=%d). Storage: %s/\n", bucket.Name, bucket.ID, storagePath)
	}
	return nil
}

func RunBucketList(database *db.DB) error {
	buckets, err := database.ListBuckets()
	if err != nil {
		return fmt.Errorf("list buckets: %w", err)
	}

	if len(buckets) == 0 {
		fmt.Println("No buckets found.")
		return nil
	}

	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tNAME\tCREATED")
	for _, b := range buckets {
		fmt.Fprintf(tw, "%d\t%s\t%s\n", b.ID, b.Name, b.CreatedAt.Format("2006-01-02 15:04:05"))
	}
	tw.Flush()
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

	fmt.Printf("Bucket '%s' deleted.\n", name)
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

	// Update DB
	if err := database.SetBucketStorageDir(name, newDir); err != nil {
		return fmt.Errorf("update database: %w", err)
	}

	if newDir != "" {
		fmt.Printf("Bucket '%s' storage moved to: %s/ (custom)\n", name, newPath)
	} else {
		fmt.Printf("Bucket '%s' storage moved back to default: %s/\n", name, newPath)
	}
	if movedCount > 0 {
		fmt.Printf("Moved %d items.\n", movedCount)
	}
	fmt.Println("Note: Restart the server for changes to take effect.")
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

	fmt.Printf("Bucket:      %s\n", bucket.Name)
	fmt.Printf("ID:          %d\n", bucket.ID)
	fmt.Printf("Created:     %s\n", bucket.CreatedAt.Format("2006-01-02 15:04:05"))
	if bucket.StorageDir != "" {
		fmt.Printf("Storage:     %s/%s/ (custom)\n", bucket.StorageDir, bucket.Name)
	} else {
		fmt.Printf("Storage:     %s/%s/\n", storageRoot, bucket.Name)
	}
	fmt.Printf("Objects:     %d\n", objCount)
	fmt.Printf("Usage:       %s\n", formatBytes(usage))
	if bucket.QuotaBytes > 0 {
		pct := float64(usage) / float64(bucket.QuotaBytes) * 100
		fmt.Printf("Quota:       %s (%.1f%% used)\n", formatBytes(bucket.QuotaBytes), pct)
	} else {
		fmt.Printf("Quota:       unlimited\n")
	}
	fmt.Printf("Credentials: %d\n", len(creds))

	if len(creds) > 0 {
		fmt.Println("\nAccess Keys:")
		for _, c := range creds {
			fmt.Printf("  - %s [%s] (created: %s)\n", c.AccessKey, c.Permission, c.CreatedAt.Format("2006-01-02 15:04:05"))
		}
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
		fmt.Printf("Quota removed for bucket '%s'.\n", name)
	} else {
		fmt.Printf("Quota set for bucket '%s': %s\n", name, formatBytes(quotaBytes))
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

	fmt.Printf("Bucket:     %s\n", bucketName)
	fmt.Printf("Access Key: %s\n", accessKey)
	fmt.Printf("Secret Key: %s\n", secretKey)
	fmt.Printf("Permission: %s\n", permission)
	fmt.Printf("\nWarning: Save the secret key now. It will not be shown again.\n")

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
		fmt.Printf("No credentials for bucket '%s'.\n", bucketName)
		return nil
	}

	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "ACCESS KEY\tPERMISSION\tCREATED")
	for _, c := range creds {
		fmt.Fprintf(tw, "%s\t%s\t%s\n", c.AccessKey, c.Permission, c.CreatedAt.Format("2006-01-02 15:04:05"))
	}
	tw.Flush()
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

	fmt.Printf("Credential '%s' deleted.\n", accessKey)
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
	fmt.Printf("Versioning enabled for bucket '%s'.\n", name)
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
	fmt.Printf("Versioning suspended for bucket '%s'.\n", name)
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
	fmt.Printf("Bucket '%s' versioning: %s\n", name, status)
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
		fmt.Printf("Lifecycle rule set for bucket '%s': expire after %d days (all objects).\n", name, days)
	} else {
		fmt.Printf("Lifecycle rule set for bucket '%s': prefix='%s', expire after %d days.\n", name, prefix, days)
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
		fmt.Printf("No lifecycle rules for bucket '%s'.\n", name)
		return nil
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "PREFIX\tEXPIRATION DAYS\tCREATED")
	for _, r := range rules {
		prefix := r.Prefix
		if prefix == "" {
			prefix = "(all)"
		}
		fmt.Fprintf(tw, "%s\t%d\t%s\n", prefix, r.ExpirationDays, r.CreatedAt.Format("2006-01-02 15:04:05"))
	}
	tw.Flush()
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
		fmt.Printf("Lifecycle rule with prefix '%s' deleted for bucket '%s'.\n", prefix, name)
	} else {
		if err := database.DeleteLifecycleRules(name); err != nil {
			return fmt.Errorf("delete lifecycle rules: %w", err)
		}
		fmt.Printf("All lifecycle rules deleted for bucket '%s'.\n", name)
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
	fmt.Printf("Webhook created for bucket '%s' (id=%d).\n", name, hook.ID)
	fmt.Printf("  URL:    %s\n", hook.URL)
	fmt.Printf("  Events: %s\n", hook.EventTypes)
	if secret != "" {
		fmt.Printf("  Secret: %s\n", secret)
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
		fmt.Printf("No webhooks for bucket '%s'.\n", name)
		return nil
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tURL\tEVENTS\tACTIVE\tCREATED")
	for _, h := range hooks {
		fmt.Fprintf(tw, "%d\t%s\t%s\t%v\t%s\n", h.ID, h.URL, h.EventTypes, h.Active, h.CreatedAt.Format("2006-01-02 15:04:05"))
	}
	tw.Flush()
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
	fmt.Printf("Webhook %d deleted for bucket '%s'.\n", id, name)
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

	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return fmt.Errorf("hash password: %w", err)
	}

	_, err = database.CreateAdmin(username, string(hash))
	if err != nil {
		return fmt.Errorf("create admin: %w", err)
	}

	fmt.Printf("Admin user created.\n")
	fmt.Printf("Username: %s\n", username)
	fmt.Printf("Password: %s\n", password)
	if customPassword == "" {
		fmt.Printf("\nWarning: Save the password now. It will not be shown again.\n")
	}
	return nil
}

func RunAdminList(database *db.DB) error {
	admins, err := database.ListAdmins()
	if err != nil {
		return fmt.Errorf("list admins: %w", err)
	}

	if len(admins) == 0 {
		fmt.Println("No admin users found.")
		return nil
	}

	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tUSERNAME\tCREATED")
	for _, a := range admins {
		fmt.Fprintf(tw, "%d\t%s\t%s\n", a.ID, a.Username, a.CreatedAt.Format("2006-01-02 15:04:05"))
	}
	tw.Flush()
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

	fmt.Printf("Admin '%s' deleted.\n", username)
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

	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return fmt.Errorf("hash password: %w", err)
	}

	if err := database.UpdateAdminPassword(username, string(hash)); err != nil {
		return fmt.Errorf("update password: %w", err)
	}

	fmt.Printf("Password updated for '%s'.\n", username)
	fmt.Printf("Password: %s\n", password)
	if customPassword == "" {
		fmt.Printf("\nWarning: Save the password now. It will not be shown again.\n")
	}
	return nil
}
