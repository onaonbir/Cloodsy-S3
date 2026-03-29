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
	"github.com/onaonbir/Cloodsy-S3/server"
	"github.com/onaonbir/Cloodsy-S3/storage"
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

	command := os.Args[1]

	switch command {
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
		fmt.Fprintf(os.Stderr, "Unknown command: %s\n\n", command)
		printUsage()
		os.Exit(1)
	}
}

func printVersion() {
	fmt.Printf("Cloodsy S3 v%s\n", Version)
	fmt.Printf("Commit:     %s\n", CommitHash)
	fmt.Printf("Build Date: %s\n", BuildDate)
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
  admin create <username> [--password=<pw>]      Create admin user (auto-generate or custom password)
  admin list                                    List admin users
  admin delete <username>                       Delete admin user
  admin password <username> [--password=<pw>]   Reset admin password
  update                                        Update to latest version
  update --check                                Check for updates without installing
  version                                       Show version information

Options:
  -config <path>                Config file path (optional, uses defaults if not provided)`)
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

func getConfigPath() string {
	for i, arg := range os.Args {
		if arg == "-config" && i+1 < len(os.Args) {
			return os.Args[i+1]
		}
	}
	return ""
}

func loadConfig() *config.Config {
	cfgPath := getConfigPath()
	if cfgPath == "" {
		return config.Default()
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error loading config '%s': %v\n", cfgPath, err)
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
		fmt.Fprintf(os.Stderr, "Error opening database: %v\n", err)
		os.Exit(1)
	}
	return database
}

func setupLogger(cfg *config.Config) *slog.Logger {
	level := slog.LevelInfo
	switch strings.ToLower(cfg.Logging.Level) {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	}

	opts := &slog.HandlerOptions{Level: level}
	var h slog.Handler
	if cfg.Logging.Format == "json" {
		h = slog.NewJSONHandler(os.Stdout, opts)
	} else {
		h = slog.NewTextHandler(os.Stdout, opts)
	}
	return slog.New(h)
}

func runServe() {
	cfg := loadConfig()
	logger := setupLogger(cfg)
	database := openDB(cfg)
	defer database.Close()

	store, err := storage.NewFileSystem(cfg.Storage.RootDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error initializing storage: %v\n", err)
		os.Exit(1)
	}

	dirs, err := database.GetAllBucketStorageDirs()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error loading bucket storage dirs: %v\n", err)
		os.Exit(1)
	}
	store.LoadBucketDirs(dirs)

	h := handler.New(database, store, cfg, logger)

	// Check for updates in background
	go cli.CheckUpdateInBackground(Version, func(format string, args ...any) {
		logger.Warn(fmt.Sprintf(format, args...))
	})

	// Start admin API if enabled
	if cfg.Admin.Enabled {
		adminHandler := admin.New(database, store, cfg, logger)
		adminHandler.Version = Version
		adminSrv := admin.RunServer(adminHandler, cfg.Admin.Listen, logger)
		defer admin.StopServer(adminSrv, logger)
	}

	// Pass version info to server package for startup logging
	server.Version = Version
	server.CommitHash = CommitHash

	if err := server.Run(cfg, h, logger); err != nil {
		logger.Error("server error", "error", err)
		os.Exit(1)
	}
}

func runBucket() {
	if len(os.Args) < 3 {
		fmt.Fprintln(os.Stderr, "Usage: cloodsys3 bucket <create|list|delete|info> [name]")
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
			fmt.Fprintln(os.Stderr, "Usage: cloodsys3 bucket create <name> [--storage-dir=<path>]")
			os.Exit(1)
		}
		storageDir := getFlag(os.Args[4:], "--storage-dir")
		err = cli.RunBucketCreate(database, os.Args[3], cfg.Storage.RootDir, storageDir)
	case "list":
		err = cli.RunBucketList(database)
	case "delete":
		if len(os.Args) < 4 {
			fmt.Fprintln(os.Stderr, "Usage: cloodsys3 bucket delete <name>")
			os.Exit(1)
		}
		err = cli.RunBucketDelete(database, os.Args[3], cfg.Storage.RootDir)
	case "info":
		if len(os.Args) < 4 {
			fmt.Fprintln(os.Stderr, "Usage: cloodsys3 bucket info <name>")
			os.Exit(1)
		}
		err = cli.RunBucketInfo(database, os.Args[3], cfg.Storage.RootDir)
	case "storage":
		if len(os.Args) < 4 {
			fmt.Fprintln(os.Stderr, "Usage: cloodsys3 bucket storage <name> --dir=<path>")
			fmt.Fprintln(os.Stderr, "  Moves data and updates storage location. Use --dir= (empty) to reset to default.")
			os.Exit(1)
		}
		dir := getFlag(os.Args[4:], "--dir")
		err = cli.RunBucketStorageDir(database, os.Args[3], cfg.Storage.RootDir, dir)
	case "quota":
		if len(os.Args) < 5 {
			fmt.Fprintln(os.Stderr, "Usage: cloodsys3 bucket quota <name> <size>")
			fmt.Fprintln(os.Stderr, "  size: 10GB, 500MB, 1TB, 0 (unlimited)")
			os.Exit(1)
		}
		err = cli.RunBucketQuota(database, os.Args[3], os.Args[4])
	case "versioning":
		if len(os.Args) < 4 {
			fmt.Fprintln(os.Stderr, "Usage: cloodsys3 bucket versioning <enable|suspend|status> <name>")
			os.Exit(1)
		}
		action := os.Args[3]
		if len(os.Args) < 5 {
			fmt.Fprintln(os.Stderr, "Usage: cloodsys3 bucket versioning <enable|suspend|status> <name>")
			os.Exit(1)
		}
		name := os.Args[4]
		switch action {
		case "enable":
			err = cli.RunBucketVersioningEnable(database, name)
		case "suspend":
			err = cli.RunBucketVersioningSuspend(database, name)
		case "status":
			err = cli.RunBucketVersioningStatus(database, name)
		default:
			fmt.Fprintf(os.Stderr, "Unknown versioning action: %s\n", action)
			os.Exit(1)
		}
	case "lifecycle":
		if len(os.Args) < 4 {
			fmt.Fprintln(os.Stderr, "Usage: cloodsys3 bucket lifecycle <set|get|delete> <name> [options]")
			os.Exit(1)
		}
		action := os.Args[3]
		if len(os.Args) < 5 {
			fmt.Fprintln(os.Stderr, "Usage: cloodsys3 bucket lifecycle <set|get|delete> <name> [options]")
			os.Exit(1)
		}
		name := os.Args[4]
		switch action {
		case "set":
			prefix := getFlag(os.Args[5:], "--prefix")
			daysStr := getFlag(os.Args[5:], "--days")
			if daysStr == "" {
				fmt.Fprintln(os.Stderr, "Usage: cloodsys3 bucket lifecycle set <name> --days=<N> [--prefix=<prefix>]")
				os.Exit(1)
			}
			days, parseErr := strconv.Atoi(daysStr)
			if parseErr != nil {
				fmt.Fprintf(os.Stderr, "Invalid --days value: %s\n", daysStr)
				os.Exit(1)
			}
			err = cli.RunBucketLifecycleSet(database, name, prefix, days)
		case "get":
			err = cli.RunBucketLifecycleGet(database, name)
		case "delete":
			prefix := getFlag(os.Args[5:], "--prefix")
			err = cli.RunBucketLifecycleDelete(database, name, prefix)
		default:
			fmt.Fprintf(os.Stderr, "Unknown lifecycle action: %s\n", action)
			os.Exit(1)
		}
	case "webhook":
		if len(os.Args) < 4 {
			fmt.Fprintln(os.Stderr, "Usage: cloodsys3 bucket webhook <add|list|delete> <name> [options]")
			os.Exit(1)
		}
		action := os.Args[3]
		if len(os.Args) < 5 {
			fmt.Fprintln(os.Stderr, "Usage: cloodsys3 bucket webhook <add|list|delete> <name> [options]")
			os.Exit(1)
		}
		name := os.Args[4]
		switch action {
		case "add":
			url := getFlag(os.Args[5:], "--url")
			events := getFlag(os.Args[5:], "--events")
			secret := getFlag(os.Args[5:], "--secret")
			if url == "" {
				fmt.Fprintln(os.Stderr, "Usage: cloodsys3 bucket webhook add <name> --url=<url> [--events=<events>] [--secret=<secret>]")
				os.Exit(1)
			}
			err = cli.RunBucketWebhookAdd(database, name, url, events, secret)
		case "list":
			err = cli.RunBucketWebhookList(database, name)
		case "delete":
			idStr := getFlag(os.Args[5:], "--id")
			if idStr == "" {
				fmt.Fprintln(os.Stderr, "Usage: cloodsys3 bucket webhook delete <name> --id=<id>")
				os.Exit(1)
			}
			id, parseErr := strconv.ParseInt(idStr, 10, 64)
			if parseErr != nil {
				fmt.Fprintf(os.Stderr, "Invalid --id value: %s\n", idStr)
				os.Exit(1)
			}
			err = cli.RunBucketWebhookDelete(database, name, id)
		default:
			fmt.Fprintf(os.Stderr, "Unknown webhook action: %s\n", action)
			os.Exit(1)
		}
	default:
		fmt.Fprintf(os.Stderr, "Unknown bucket command: %s\n", subcommand)
		os.Exit(1)
	}

	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

func runCredential() {
	if len(os.Args) < 3 {
		fmt.Fprintln(os.Stderr, "Usage: cloodsys3 credential <create|list|delete> [bucket|key]")
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
			fmt.Fprintln(os.Stderr, "Usage: cloodsys3 credential create <bucket-name> [--read-only]")
			os.Exit(1)
		}
		readOnly := false
		for _, arg := range os.Args[4:] {
			if arg == "--read-only" {
				readOnly = true
			}
		}
		err = cli.RunCredentialCreate(database, os.Args[3], readOnly)
	case "list":
		if len(os.Args) < 4 {
			fmt.Fprintln(os.Stderr, "Usage: cloodsys3 credential list <bucket-name>")
			os.Exit(1)
		}
		err = cli.RunCredentialList(database, os.Args[3])
	case "delete":
		if len(os.Args) < 4 {
			fmt.Fprintln(os.Stderr, "Usage: cloodsys3 credential delete <access-key>")
			os.Exit(1)
		}
		err = cli.RunCredentialDelete(database, os.Args[3])
	default:
		fmt.Fprintf(os.Stderr, "Unknown credential command: %s\n", subcommand)
		os.Exit(1)
	}

	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

func runAdmin() {
	if len(os.Args) < 3 {
		fmt.Fprintln(os.Stderr, "Usage: cloodsys3 admin <create|list|delete> [username]")
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
			fmt.Fprintln(os.Stderr, "Usage: cloodsys3 admin create <username> [--password=<password>]")
			os.Exit(1)
		}
		password := getFlag(os.Args[4:], "--password")
		err = cli.RunAdminCreate(database, os.Args[3], password)
	case "list":
		err = cli.RunAdminList(database)
	case "delete":
		if len(os.Args) < 4 {
			fmt.Fprintln(os.Stderr, "Usage: cloodsys3 admin delete <username>")
			os.Exit(1)
		}
		err = cli.RunAdminDelete(database, os.Args[3])
	case "password":
		if len(os.Args) < 4 {
			fmt.Fprintln(os.Stderr, "Usage: cloodsys3 admin password <username> [--password=<password>]")
			os.Exit(1)
		}
		password := getFlag(os.Args[4:], "--password")
		err = cli.RunAdminPassword(database, os.Args[3], password)
	default:
		fmt.Fprintf(os.Stderr, "Unknown admin command: %s\n", subcommand)
		os.Exit(1)
	}

	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

func runUpdate() {
	checkOnly := false
	for _, arg := range os.Args[2:] {
		if arg == "--check" {
			checkOnly = true
		}
	}

	var err error
	if checkOnly {
		err = cli.RunUpdateCheck(Version)
	} else {
		err = cli.RunUpdate(Version)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}
