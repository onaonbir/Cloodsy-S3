package main

import (
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"

	"github.com/onaonbir/Cloodsy-S3/admin"
	"github.com/onaonbir/Cloodsy-S3/cli"
	"github.com/onaonbir/Cloodsy-S3/config"
	"github.com/onaonbir/Cloodsy-S3/db"
	"github.com/onaonbir/Cloodsy-S3/handler"
	"github.com/onaonbir/Cloodsy-S3/httpx"
	applogger "github.com/onaonbir/Cloodsy-S3/logger"
	"github.com/onaonbir/Cloodsy-S3/server"
	"github.com/onaonbir/Cloodsy-S3/storage"
	"github.com/onaonbir/Cloodsy-S3/webdav"
	"github.com/pterm/pterm"
)

// Build-time variables (injected via -ldflags)
var (
	Version    = "dev"
	CommitHash = "unknown"
	BuildDate  = "unknown"
)

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
	}

	switch os.Args[1] {
	case "serve":
		runServe()
	case "bucket":
		runBucket()
	case "credential":
		runCredential()
	case "admin":
		runAdmin()
	case "update":
		runUpdate()
	case "version", "-v", "--version":
		printVersion()
	case "help", "-h", "--help":
		printUsage()
	default:
		pterm.Error.Printfln("Unknown command: %s", os.Args[1])
		printUsage()
		os.Exit(1)
	}
}

func printVersion() {
	pterm.Printf("Cloodsy S3 v%s\n", Version)
	pterm.Printf("  %s  %s\n", pterm.Gray("Commit:    "), CommitHash)
	pterm.Printf("  %s  %s\n", pterm.Gray("Build Date:"), BuildDate)
}

func printUsage() {
	fmt.Printf("Cloodsy S3 v%s - AWS SDK Compatible S3 Server\n\n", Version)
	fmt.Println(`Usage:
  cloodsys3 <command> [options]

Commands:
  serve                                         Start the S3 server
  bucket create <name> [--storage-dir=<path>]    Create a new bucket (optionally with custom storage)
  bucket list                                   List all buckets
  bucket delete <name>                          Delete a bucket
  bucket info <name>                            Show bucket details
  bucket storage <name> --dir=<path>             Move bucket storage to a new location
  bucket quota <name> <size>                    Set bucket quota (e.g. 10GB, 500MB, 0=unlimited)
  bucket versioning enable <name>               Enable bucket versioning
  bucket versioning suspend <name>              Suspend bucket versioning
  bucket versioning status <name>               Show versioning status
  bucket public-read <enable|disable|status> <name>  Toggle anonymous object read access
  bucket webdav <enable|disable|status> <name>  Toggle WebDAV mountability for a bucket
  bucket reprocess <name> [--prefix=<p>]        Regenerate optimized image variants
  bucket lifecycle set <name> --days=<N> [--prefix=<p>]  Set lifecycle rule
  bucket lifecycle get <name>                   Show lifecycle rules
  bucket lifecycle delete <name> [--prefix=<p>] Delete lifecycle rules
  bucket webhook add <name> --url=<url> [--events=<e>] [--secret=<s>]  Add webhook
  bucket webhook list <name>                    List webhooks
  bucket webhook delete <name> --id=<id>        Delete webhook
  credential create <bucket>                    Create read-write access/secret key pair
  credential create <bucket> --read-only        Create read-only access/secret key pair
  credential list <bucket>                      List access keys for a bucket
  credential delete <key>                       Delete an access key
  admin create <username> [--password=<pw>]      Create admin user (prompts when --password is omitted)
  admin list                                    List admin users
  admin delete <username>                       Delete admin user
  admin password <username> [--password=<pw>]   Reset admin password
  update                                        Update to latest version (verifies checksums)
  update --check                                Check for updates without installing
  version                                       Show version information

Options:
  -config <path>                Config file path (optional, uses defaults if not provided)

Environment overrides: CLOODSYS3_LISTEN, CLOODSYS3_ADMIN_LISTEN, CLOODSYS3_WEBDAV_LISTEN,
  CLOODSYS3_DB_PATH, CLOODSYS3_DATA_DIR, CLOODSYS3_LOG_LEVEL, CLOODSYS3_LOG_FORMAT,
  CLOODSYS3_TLS_ENABLED, CLOODSYS3_TLS_CERT, CLOODSYS3_TLS_KEY, CLOODSYS3_ADMIN_ENABLED,
  CLOODSYS3_WEBDAV_ENABLED, CLOODSYS3_UPDATE_CHECK`)
}

// getFlag parses --key=value style flags from args.
func getFlag(args []string, name string) string {
	prefix := name + "="
	for _, arg := range args {
		if strings.HasPrefix(arg, prefix) {
			return strings.TrimPrefix(arg, prefix)
		}
	}
	return ""
}

func hasFlag(args []string, name string) bool {
	for _, arg := range args {
		if arg == name || strings.HasPrefix(arg, name+"=") {
			return true
		}
	}
	return false
}

func getConfigPath() string {
	for i, arg := range os.Args {
		if arg == "-config" && i+1 < len(os.Args) {
			return os.Args[i+1]
		}
	}
	return os.Getenv("CLOODSYS3_CONFIG")
}

func loadConfig() *config.Config {
	cfgPath := getConfigPath()
	var cfg *config.Config
	var err error
	if cfgPath == "" {
		cfg, err = config.LoadDefault()
	} else {
		cfg, err = config.Load(cfgPath)
	}
	if err != nil {
		if cfgPath != "" {
			pterm.Error.Printfln("Loading config '%s': %v", cfgPath, err)
		} else {
			pterm.Error.Printfln("Invalid configuration: %v", err)
		}
		os.Exit(1)
	}
	return cfg
}

func openDB(cfg *config.Config) *db.DB {
	database, err := db.OpenWithConfig(db.DBConfig{
		Path:        cfg.Database.Path,
		BusyTimeout: cfg.Database.BusyTimeout,
		CacheSize:   cfg.Database.CacheSize,
		MmapSize:    cfg.Database.MmapSize,
		MaxReaders:  cfg.Database.MaxReaders,
	})
	if err != nil {
		pterm.Error.Printfln("Opening database: %v", err)
		if os.IsPermission(unwrapErr(err)) {
			pterm.Info.Println("Hint: run the CLI as the same user the service runs as (e.g. sudo -u cloodsys3 ...) or fix ownership of the data directory.")
		}
		os.Exit(1)
	}
	return database
}

func unwrapErr(err error) error {
	for {
		u, ok := err.(interface{ Unwrap() error })
		if !ok || u.Unwrap() == nil {
			return err
		}
		err = u.Unwrap()
	}
}

func setupLogger(cfg *config.Config) *slog.Logger {
	level := slog.LevelInfo
	switch strings.ToLower(cfg.Logging.Level) {
	case "debug":
		level = slog.LevelDebug
	case "warn", "warning":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	}

	var h slog.Handler
	if strings.ToLower(cfg.Logging.Format) == "json" {
		h = slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level})
	} else {
		h = applogger.NewPrettyHandler(os.Stdout, level)
	}
	return slog.New(h)
}

func openStorage(cfg *config.Config, database *db.DB) *storage.FileSystem {
	store, err := storage.NewFileSystem(cfg.Storage.RootDir)
	if err != nil {
		pterm.Error.Printfln("Initializing storage: %v", err)
		os.Exit(1)
	}
	dirs, err := database.GetAllBucketStorageDirs()
	if err != nil {
		pterm.Error.Printfln("Loading bucket storage dirs: %v", err)
		os.Exit(1)
	}
	store.LoadBucketDirs(dirs)
	return store
}

// migrateStorageLayout moves objects whose keys need on-disk encoding (e.g.
// "dir/", "a//b", Windows-reserved names) from the legacy raw-key path to the
// encoded path. Runs once per database; safe to interrupt and re-run.
func migrateStorageLayout(database *db.DB, store *storage.FileSystem, logger *slog.Logger) {
	const layoutKey = "storage_layout_version"
	const target = "2"
	current, err := database.GetMeta(layoutKey)
	if err != nil {
		logger.Warn("could not read storage layout version", "error", err)
		return
	}
	if current == target {
		return
	}
	buckets := map[int64]string{}
	moved, failed := 0, 0
	err = database.IterateObjects(500, func(m db.ObjectMeta) error {
		if m.IsDeleteMarker {
			return nil
		}
		name, ok := buckets[m.BucketID]
		if !ok {
			name, _ = database.GetBucketNameByID(m.BucketID)
			buckets[m.BucketID] = name
		}
		if name == "" {
			return nil
		}
		cur, legacy := store.LegacyObjectPath(name, m.Key, m.VersionID)
		if legacy == "" {
			return nil
		}
		ok, merr := store.MoveLegacyObject(cur, legacy)
		if merr != nil {
			failed++
			logger.Error("storage layout migration: move failed", "bucket", name, "key", m.Key, "error", merr)
			return nil
		}
		if ok {
			moved++
		}
		return nil
	})
	if err != nil {
		logger.Error("storage layout migration aborted", "error", err)
		return
	}
	if failed == 0 {
		database.SetMeta(layoutKey, target)
	}
	if moved > 0 || failed > 0 {
		logger.Info("storage layout migration finished", "moved", moved, "failed", failed)
	}
}

func runServe() {
	cfg := loadConfig()
	logger := setupLogger(cfg)
	database := openDB(cfg)
	defer database.Close()

	store := openStorage(cfg, database)
	migrateStorageLayout(database, store, logger)

	h := handler.New(database, store, cfg, logger)

	applogger.Banner(
		Version, CommitHash,
		cfg.Server.Listen, cfg.Admin.Listen,
		cfg.Server.Region, cfg.Storage.RootDir, cfg.Database.Path,
		cfg.Server.TLS.Enabled, cfg.Admin.Enabled,
	)

	if cfg.Update.CheckOnStart != nil && *cfg.Update.CheckOnStart {
		go cli.CheckUpdateInBackground(Version, func(format string, args ...any) {
			logger.Warn(fmt.Sprintf(format, args...))
		})
	}

	if cfg.Admin.Enabled {
		if !cfg.Admin.TLS.Enabled && !httpx.IsLoopbackAddr(cfg.Admin.Listen) {
			logger.Warn("admin API is reachable without TLS; credentials travel in cleartext. Bind admin.listen to 127.0.0.1, enable admin.tls, or put a TLS reverse proxy in front.", "listen", cfg.Admin.Listen)
		}
		adminHandler := admin.New(database, store, cfg, logger)
		adminHandler.Version = Version
		adminHandler.Objects = h.Objects
		adminSrv := admin.RunServer(adminHandler, cfg.Admin.Listen, logger)
		defer admin.StopServer(adminSrv, logger)
	}

	if cfg.WebDAV.Enabled {
		if !cfg.WebDAV.TLS.Enabled && !httpx.IsLoopbackAddr(cfg.WebDAV.Listen) {
			logger.Warn("WebDAV is reachable without TLS; Basic-auth secrets travel in cleartext (and Windows refuses Basic auth over HTTP by default). Enable webdav.tls or use a TLS reverse proxy.", "listen", cfg.WebDAV.Listen)
		}
		davSrv := webdav.RunServer(database, store, h.Objects, cfg, logger)
		defer webdav.StopServer(davSrv, logger)
	}

	server.Version = Version
	server.CommitHash = CommitHash

	if err := server.Run(cfg, h, logger); err != nil {
		logger.Error("server error", "error", err)
		os.Exit(1)
	}
}

func runBucket() {
	if len(os.Args) < 3 {
		pterm.Info.Println("Usage: cloodsys3 bucket <create|list|delete|info|storage|quota|versioning|public-read|webdav|reprocess|lifecycle|webhook> ...")
		os.Exit(1)
	}

	cfg := loadConfig()
	database := openDB(cfg)
	defer database.Close()

	subcommand := os.Args[2]
	var err error

	switch subcommand {
	case "create":
		if len(os.Args) < 4 {
			pterm.Info.Println("Usage: cloodsys3 bucket create <name> [--storage-dir=<path>]")
			os.Exit(1)
		}
		storageDir := getFlag(os.Args[4:], "--storage-dir")
		err = cli.RunBucketCreate(database, os.Args[3], cfg.Storage.RootDir, storageDir)
	case "list":
		err = cli.RunBucketList(database)
	case "delete":
		if len(os.Args) < 4 {
			pterm.Info.Println("Usage: cloodsys3 bucket delete <name> [--force]")
			os.Exit(1)
		}
		err = cli.RunBucketDelete(database, os.Args[3], cfg.Storage.RootDir, hasFlag(os.Args[4:], "--force"))
	case "info":
		if len(os.Args) < 4 {
			pterm.Info.Println("Usage: cloodsys3 bucket info <name>")
			os.Exit(1)
		}
		err = cli.RunBucketInfo(database, os.Args[3], cfg.Storage.RootDir)
	case "storage":
		if len(os.Args) < 4 {
			pterm.Info.Println("Usage: cloodsys3 bucket storage <name> --dir=<path>")
			pterm.Info.Println("  Moves data and updates storage location. Use --dir= (empty) to reset to default.")
			os.Exit(1)
		}
		dir := getFlag(os.Args[4:], "--dir")
		err = cli.RunBucketStorageDir(database, os.Args[3], cfg.Storage.RootDir, dir)
	case "quota":
		if len(os.Args) < 5 {
			pterm.Info.Println("Usage: cloodsys3 bucket quota <name> <size>")
			pterm.Info.Println("  size: 10GB, 500MB, 1TB, 0 (unlimited)")
			os.Exit(1)
		}
		err = cli.RunBucketQuota(database, os.Args[3], os.Args[4])
	case "versioning":
		if len(os.Args) < 5 {
			pterm.Info.Println("Usage: cloodsys3 bucket versioning <enable|suspend|status> <name>")
			os.Exit(1)
		}
		action, name := os.Args[3], os.Args[4]
		switch action {
		case "enable":
			err = cli.RunBucketVersioningEnable(database, name)
		case "suspend":
			err = cli.RunBucketVersioningSuspend(database, name)
		case "status":
			err = cli.RunBucketVersioningStatus(database, name)
		default:
			pterm.Error.Printfln("Unknown versioning action: %s", action)
			os.Exit(1)
		}
	case "public-read":
		if len(os.Args) < 5 {
			pterm.Info.Println("Usage: cloodsys3 bucket public-read <enable|disable|status> <name>")
			os.Exit(1)
		}
		err = cli.RunBucketPublicRead(database, os.Args[4], os.Args[3])
	case "webdav":
		if len(os.Args) < 5 {
			pterm.Info.Println("Usage: cloodsys3 bucket webdav <enable|disable|status> <name>")
			os.Exit(1)
		}
		err = cli.RunBucketWebDAV(database, os.Args[4], os.Args[3])
	case "reprocess":
		if len(os.Args) < 4 {
			pterm.Info.Println("Usage: cloodsys3 bucket reprocess <name> [--prefix=<prefix>]")
			os.Exit(1)
		}
		prefix := getFlag(os.Args[4:], "--prefix")
		store := openStorage(cfg, database)
		err = cli.RunBucketReprocess(database, store, cfg, os.Args[3], prefix)
	case "lifecycle":
		if len(os.Args) < 5 {
			pterm.Info.Println("Usage: cloodsys3 bucket lifecycle <set|get|delete> <name> [options]")
			os.Exit(1)
		}
		action, name := os.Args[3], os.Args[4]
		switch action {
		case "set":
			prefix := getFlag(os.Args[5:], "--prefix")
			daysStr := getFlag(os.Args[5:], "--days")
			if daysStr == "" {
				pterm.Info.Println("Usage: cloodsys3 bucket lifecycle set <name> --days=<N> [--prefix=<prefix>]")
				os.Exit(1)
			}
			days, parseErr := strconv.Atoi(daysStr)
			if parseErr != nil || days <= 0 {
				pterm.Error.Printfln("Invalid --days value: %s", daysStr)
				os.Exit(1)
			}
			err = cli.RunBucketLifecycleSet(database, name, prefix, days)
		case "get":
			err = cli.RunBucketLifecycleGet(database, name)
		case "delete":
			prefix := getFlag(os.Args[5:], "--prefix")
			err = cli.RunBucketLifecycleDelete(database, name, prefix)
		default:
			pterm.Error.Printfln("Unknown lifecycle action: %s", action)
			os.Exit(1)
		}
	case "webhook":
		if len(os.Args) < 5 {
			pterm.Info.Println("Usage: cloodsys3 bucket webhook <add|list|delete> <name> [options]")
			os.Exit(1)
		}
		action, name := os.Args[3], os.Args[4]
		switch action {
		case "add":
			url := getFlag(os.Args[5:], "--url")
			events := getFlag(os.Args[5:], "--events")
			secret := getFlag(os.Args[5:], "--secret")
			if url == "" {
				pterm.Info.Println("Usage: cloodsys3 bucket webhook add <name> --url=<url> [--events=<events>] [--secret=<secret>]")
				os.Exit(1)
			}
			err = cli.RunBucketWebhookAdd(database, name, url, events, secret)
		case "list":
			err = cli.RunBucketWebhookList(database, name)
		case "delete":
			idStr := getFlag(os.Args[5:], "--id")
			if idStr == "" {
				pterm.Info.Println("Usage: cloodsys3 bucket webhook delete <name> --id=<id>")
				os.Exit(1)
			}
			id, parseErr := strconv.ParseInt(idStr, 10, 64)
			if parseErr != nil {
				pterm.Error.Printfln("Invalid --id value: %s", idStr)
				os.Exit(1)
			}
			err = cli.RunBucketWebhookDelete(database, name, id)
		default:
			pterm.Error.Printfln("Unknown webhook action: %s", action)
			os.Exit(1)
		}
	default:
		pterm.Error.Printfln("Unknown bucket command: %s", subcommand)
		os.Exit(1)
	}

	if err != nil {
		pterm.Error.Printfln("%v", err)
		os.Exit(1)
	}
}

func runCredential() {
	if len(os.Args) < 3 {
		pterm.Info.Println("Usage: cloodsys3 credential <create|list|delete> [bucket|key]")
		os.Exit(1)
	}

	cfg := loadConfig()
	database := openDB(cfg)
	defer database.Close()

	subcommand := os.Args[2]
	var err error

	switch subcommand {
	case "create":
		if len(os.Args) < 4 {
			pterm.Info.Println("Usage: cloodsys3 credential create <bucket-name> [--read-only]")
			os.Exit(1)
		}
		err = cli.RunCredentialCreate(database, os.Args[3], hasFlag(os.Args[4:], "--read-only"))
	case "list":
		if len(os.Args) < 4 {
			pterm.Info.Println("Usage: cloodsys3 credential list <bucket-name>")
			os.Exit(1)
		}
		err = cli.RunCredentialList(database, os.Args[3])
	case "delete":
		if len(os.Args) < 4 {
			pterm.Info.Println("Usage: cloodsys3 credential delete <access-key>")
			os.Exit(1)
		}
		err = cli.RunCredentialDelete(database, os.Args[3])
	default:
		pterm.Error.Printfln("Unknown credential command: %s", subcommand)
		os.Exit(1)
	}

	if err != nil {
		pterm.Error.Printfln("%v", err)
		os.Exit(1)
	}
}

func runAdmin() {
	if len(os.Args) < 3 {
		pterm.Info.Println("Usage: cloodsys3 admin <create|list|delete|password> [username]")
		os.Exit(1)
	}

	cfg := loadConfig()
	database := openDB(cfg)
	defer database.Close()

	subcommand := os.Args[2]
	var err error

	switch subcommand {
	case "create":
		if len(os.Args) < 4 {
			pterm.Info.Println("Usage: cloodsys3 admin create <username> [--password=<password>|--generate]")
			os.Exit(1)
		}
		err = cli.RunAdminCreate(database, os.Args[3], getFlag(os.Args[4:], "--password"), hasFlag(os.Args[4:], "--generate"))
	case "list":
		err = cli.RunAdminList(database)
	case "delete":
		if len(os.Args) < 4 {
			pterm.Info.Println("Usage: cloodsys3 admin delete <username>")
			os.Exit(1)
		}
		err = cli.RunAdminDelete(database, os.Args[3])
	case "password":
		if len(os.Args) < 4 {
			pterm.Info.Println("Usage: cloodsys3 admin password <username> [--password=<password>|--generate]")
			os.Exit(1)
		}
		err = cli.RunAdminPassword(database, os.Args[3], getFlag(os.Args[4:], "--password"), hasFlag(os.Args[4:], "--generate"))
	default:
		pterm.Error.Printfln("Unknown admin command: %s", subcommand)
		os.Exit(1)
	}

	if err != nil {
		pterm.Error.Printfln("%v", err)
		os.Exit(1)
	}
}

func runUpdate() {
	checkOnly := hasFlag(os.Args[2:], "--check")
	var err error
	if checkOnly {
		err = cli.RunUpdateCheck(Version)
	} else {
		err = cli.RunUpdate(Version)
	}
	if err != nil {
		pterm.Error.Printfln("%v", err)
		os.Exit(1)
	}
}
