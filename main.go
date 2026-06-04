package main

import (
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"

	"github.com/onaonbir/Cloodsy-S3/admin"
	"github.com/onaonbir/Cloodsy-S3/cli"
	applogger "github.com/onaonbir/Cloodsy-S3/logger"
	"github.com/onaonbir/Cloodsy-S3/config"
	"github.com/onaonbir/Cloodsy-S3/db"
	"github.com/onaonbir/Cloodsy-S3/handler"
	"github.com/pterm/pterm"
	"github.com/onaonbir/Cloodsy-S3/server"
	"github.com/onaonbir/Cloodsy-S3/storage"
	"github.com/onaonbir/Cloodsy-S3/webdav"
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
		pterm.Error.Printfln("Unknown command: %s", command)
		printUsage()
		os.Exit(1)
	}
}

func printVersion() {
	pterm.Println()
	pterm.Printf("  %s %s\n", pterm.Bold.Sprint("Cloodsy S3"), pterm.Cyan("v"+Version))
	pterm.Printf("  %s  %s\n", pterm.Gray("Commit:    "), CommitHash)
	pterm.Printf("  %s  %s\n", pterm.Gray("Build Date:"), BuildDate)
	pterm.Println()
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
		pterm.Error.Printfln("Loading config '%s': %v", cfgPath, err)
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

	var h slog.Handler
	if cfg.Logging.Format == "json" {
		opts := &slog.HandlerOptions{Level: level}
		h = slog.NewJSONHandler(os.Stdout, opts)
	} else {
		h = applogger.NewPrettyHandler(os.Stdout, level)
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
		pterm.Error.Printfln("Initializing storage: %v", err)
		os.Exit(1)
	}

	dirs, err := database.GetAllBucketStorageDirs()
	if err != nil {
		pterm.Error.Printfln("Loading bucket storage dirs: %v", err)
		os.Exit(1)
	}
	store.LoadBucketDirs(dirs)

	h := handler.New(database, store, cfg, logger)

	// Print startup banner
	applogger.Banner(
		Version, CommitHash,
		cfg.Server.Listen, cfg.Admin.Listen,
		cfg.Server.Region, cfg.Storage.RootDir, cfg.Database.Path,
		cfg.Server.TLS.Enabled, cfg.Admin.Enabled,
	)

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

	// Start WebDAV server if enabled
	if cfg.WebDAV.Enabled {
		davSrv := webdav.RunServer(database, store, cfg, logger)
		defer webdav.StopServer(davSrv, logger)
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
		pterm.Info.Println( "Usage: cloodsys3 bucket <create|list|delete|info> [name]")
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
			pterm.Info.Println( "Usage: cloodsys3 bucket create <name> [--storage-dir=<path>]")
			os.Exit(1)
		}
		storageDir := getFlag(os.Args[4:], "--storage-dir")
		err = cli.RunBucketCreate(database, os.Args[3], cfg.Storage.RootDir, storageDir)
	case "list":
		err = cli.RunBucketList(database)
	case "delete":
		if len(os.Args) < 4 {
			pterm.Info.Println( "Usage: cloodsys3 bucket delete <name>")
			os.Exit(1)
		}
		err = cli.RunBucketDelete(database, os.Args[3], cfg.Storage.RootDir)
	case "info":
		if len(os.Args) < 4 {
			pterm.Info.Println( "Usage: cloodsys3 bucket info <name>")
			os.Exit(1)
		}
		err = cli.RunBucketInfo(database, os.Args[3], cfg.Storage.RootDir)
	case "storage":
		if len(os.Args) < 4 {
			pterm.Info.Println( "Usage: cloodsys3 bucket storage <name> --dir=<path>")
			pterm.Info.Println( "  Moves data and updates storage location. Use --dir= (empty) to reset to default.")
			os.Exit(1)
		}
		dir := getFlag(os.Args[4:], "--dir")
		err = cli.RunBucketStorageDir(database, os.Args[3], cfg.Storage.RootDir, dir)
	case "quota":
		if len(os.Args) < 5 {
			pterm.Info.Println( "Usage: cloodsys3 bucket quota <name> <size>")
			pterm.Info.Println( "  size: 10GB, 500MB, 1TB, 0 (unlimited)")
			os.Exit(1)
		}
		err = cli.RunBucketQuota(database, os.Args[3], os.Args[4])
	case "versioning":
		if len(os.Args) < 4 {
			pterm.Info.Println( "Usage: cloodsys3 bucket versioning <enable|suspend|status> <name>")
			os.Exit(1)
		}
		action := os.Args[3]
		if len(os.Args) < 5 {
			pterm.Info.Println( "Usage: cloodsys3 bucket versioning <enable|suspend|status> <name>")
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
			pterm.Error.Printfln("Unknown versioning action: %s\n", action)
			os.Exit(1)
		}
	case "public-read":
		if len(os.Args) < 5 {
			pterm.Info.Println( "Usage: cloodsys3 bucket public-read <enable|disable|status> <name>")
			os.Exit(1)
		}
		err = cli.RunBucketPublicRead(database, os.Args[4], os.Args[3])
	case "webdav":
		if len(os.Args) < 5 {
			pterm.Info.Println( "Usage: cloodsys3 bucket webdav <enable|disable|status> <name>")
			os.Exit(1)
		}
		err = cli.RunBucketWebDAV(database, os.Args[4], os.Args[3])
	case "reprocess":
		if len(os.Args) < 4 {
			pterm.Info.Println( "Usage: cloodsys3 bucket reprocess <name> [--prefix=<prefix>]")
			os.Exit(1)
		}
		prefix := getFlag(os.Args[4:], "--prefix")
		store, serr := storage.NewFileSystem(cfg.Storage.RootDir)
		if serr != nil {
			pterm.Error.Printfln("Initializing storage: %v", serr)
			os.Exit(1)
		}
		if dirs, derr := database.GetAllBucketStorageDirs(); derr == nil {
			store.LoadBucketDirs(dirs)
		}
		err = cli.RunBucketReprocess(database, store, cfg, os.Args[3], prefix)
	case "lifecycle":
		if len(os.Args) < 4 {
			pterm.Info.Println( "Usage: cloodsys3 bucket lifecycle <set|get|delete> <name> [options]")
			os.Exit(1)
		}
		action := os.Args[3]
		if len(os.Args) < 5 {
			pterm.Info.Println( "Usage: cloodsys3 bucket lifecycle <set|get|delete> <name> [options]")
			os.Exit(1)
		}
		name := os.Args[4]
		switch action {
		case "set":
			prefix := getFlag(os.Args[5:], "--prefix")
			daysStr := getFlag(os.Args[5:], "--days")
			if daysStr == "" {
				pterm.Info.Println( "Usage: cloodsys3 bucket lifecycle set <name> --days=<N> [--prefix=<prefix>]")
				os.Exit(1)
			}
			days, parseErr := strconv.Atoi(daysStr)
			if parseErr != nil {
				pterm.Error.Printfln("Invalid --days value: %s\n", daysStr)
				os.Exit(1)
			}
			err = cli.RunBucketLifecycleSet(database, name, prefix, days)
		case "get":
			err = cli.RunBucketLifecycleGet(database, name)
		case "delete":
			prefix := getFlag(os.Args[5:], "--prefix")
			err = cli.RunBucketLifecycleDelete(database, name, prefix)
		default:
			pterm.Error.Printfln("Unknown lifecycle action: %s\n", action)
			os.Exit(1)
		}
	case "webhook":
		if len(os.Args) < 4 {
			pterm.Info.Println( "Usage: cloodsys3 bucket webhook <add|list|delete> <name> [options]")
			os.Exit(1)
		}
		action := os.Args[3]
		if len(os.Args) < 5 {
			pterm.Info.Println( "Usage: cloodsys3 bucket webhook <add|list|delete> <name> [options]")
			os.Exit(1)
		}
		name := os.Args[4]
		switch action {
		case "add":
			url := getFlag(os.Args[5:], "--url")
			events := getFlag(os.Args[5:], "--events")
			secret := getFlag(os.Args[5:], "--secret")
			if url == "" {
				pterm.Info.Println( "Usage: cloodsys3 bucket webhook add <name> --url=<url> [--events=<events>] [--secret=<secret>]")
				os.Exit(1)
			}
			err = cli.RunBucketWebhookAdd(database, name, url, events, secret)
		case "list":
			err = cli.RunBucketWebhookList(database, name)
		case "delete":
			idStr := getFlag(os.Args[5:], "--id")
			if idStr == "" {
				pterm.Info.Println( "Usage: cloodsys3 bucket webhook delete <name> --id=<id>")
				os.Exit(1)
			}
			id, parseErr := strconv.ParseInt(idStr, 10, 64)
			if parseErr != nil {
				pterm.Error.Printfln("Invalid --id value: %s\n", idStr)
				os.Exit(1)
			}
			err = cli.RunBucketWebhookDelete(database, name, id)
		default:
			pterm.Error.Printfln("Unknown webhook action: %s\n", action)
			os.Exit(1)
		}
	default:
		pterm.Error.Printfln("Unknown bucket command: %s\n", subcommand)
		os.Exit(1)
	}

	if err != nil {
		pterm.Error.Printfln("%v", err)
		os.Exit(1)
	}
}

func runCredential() {
	if len(os.Args) < 3 {
		pterm.Info.Println( "Usage: cloodsys3 credential <create|list|delete> [bucket|key]")
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
			pterm.Info.Println( "Usage: cloodsys3 credential create <bucket-name> [--read-only]")
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
			pterm.Info.Println( "Usage: cloodsys3 credential list <bucket-name>")
			os.Exit(1)
		}
		err = cli.RunCredentialList(database, os.Args[3])
	case "delete":
		if len(os.Args) < 4 {
			pterm.Info.Println( "Usage: cloodsys3 credential delete <access-key>")
			os.Exit(1)
		}
		err = cli.RunCredentialDelete(database, os.Args[3])
	default:
		pterm.Error.Printfln("Unknown credential command: %s\n", subcommand)
		os.Exit(1)
	}

	if err != nil {
		pterm.Error.Printfln("%v", err)
		os.Exit(1)
	}
}

func runAdmin() {
	if len(os.Args) < 3 {
		pterm.Info.Println( "Usage: cloodsys3 admin <create|list|delete> [username]")
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
			pterm.Info.Println( "Usage: cloodsys3 admin create <username> [--password=<password>]")
			os.Exit(1)
		}
		password := getFlag(os.Args[4:], "--password")
		err = cli.RunAdminCreate(database, os.Args[3], password)
	case "list":
		err = cli.RunAdminList(database)
	case "delete":
		if len(os.Args) < 4 {
			pterm.Info.Println( "Usage: cloodsys3 admin delete <username>")
			os.Exit(1)
		}
		err = cli.RunAdminDelete(database, os.Args[3])
	case "password":
		if len(os.Args) < 4 {
			pterm.Info.Println( "Usage: cloodsys3 admin password <username> [--password=<password>]")
			os.Exit(1)
		}
		password := getFlag(os.Args[4:], "--password")
		err = cli.RunAdminPassword(database, os.Args[3], password)
	default:
		pterm.Error.Printfln("Unknown admin command: %s\n", subcommand)
		os.Exit(1)
	}

	if err != nil {
		pterm.Error.Printfln("%v", err)
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
		pterm.Error.Printfln("%v", err)
		os.Exit(1)
	}
}
