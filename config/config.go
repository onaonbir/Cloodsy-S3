package config

import (
	"os"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Server   ServerConfig   `yaml:"server"`
	Database DatabaseConfig `yaml:"database"`
	Storage  StorageConfig  `yaml:"storage"`
	Logging  LoggingConfig  `yaml:"logging"`
	Admin    AdminConfig    `yaml:"admin"`
	Image    ImageConfig    `yaml:"image"`
	WebDAV   WebDAVConfig   `yaml:"webdav"`
}

// WebDAVConfig exposes buckets over WebDAV so clients can mount them as network
// drives. Disabled by default. Auth maps to bucket credentials (access key as
// username, secret key as password); one credential = one bucket = the mount.
type WebDAVConfig struct {
	Enabled bool   `yaml:"enabled"` // master switch, default false
	Listen  string `yaml:"listen"`  // listen address, default ":9002"
	Prefix  string `yaml:"prefix"`  // URL path prefix, default "/"
}

// ImageConfig controls automatic image optimization on upload. Optimization
// produces a separate, optimized variant alongside the original (the original
// bytes are never modified). Disabled by default so existing deployments are
// unaffected.
type ImageConfig struct {
	Enabled      bool  `yaml:"enabled"`        // master switch, default false
	SyncMaxBytes int64 `yaml:"sync_max_bytes"` // <= => optimize inline; > => async. default 2_000_000
	Quality      int   `yaml:"quality"`        // JPEG quality 1-100, default 75
	Workers      int   `yaml:"workers"`        // async worker pool size, default 2
	QueueSize    int   `yaml:"queue_size"`     // async queue buffer, default 256
}

type AdminConfig struct {
	Enabled     bool     `yaml:"enabled"`
	Listen      string   `yaml:"listen"`
	CORSOrigins []string `yaml:"cors_origins"`
}

type ServerConfig struct {
	Listen      string   `yaml:"listen"`
	Region      string   `yaml:"region"`
	TLS         TLSConfig `yaml:"tls"`
	CORSOrigins []string `yaml:"cors_origins"`
}

type TLSConfig struct {
	Enabled  bool   `yaml:"enabled"`
	CertFile string `yaml:"cert_file"`
	KeyFile  string `yaml:"key_file"`
}

type DatabaseConfig struct {
	Path        string `yaml:"path"`
	BusyTimeout int    `yaml:"busy_timeout"` // ms, default 5000
	CacheSize   int    `yaml:"cache_size"`   // KB, default 64000 (64MB)
	MmapSize    int    `yaml:"mmap_size"`    // bytes, default 134217728 (128MB)
	MaxReaders  int    `yaml:"max_readers"`  // default 4
}

type StorageConfig struct {
	RootDir            string `yaml:"root_dir"`
	MultipartMaxAge    string `yaml:"multipart_max_age"`    // Duration string, e.g. "24h"
	LifecycleInterval  string `yaml:"lifecycle_interval"`   // Duration string, e.g. "1h"
}

type LoggingConfig struct {
	Level  string `yaml:"level"`
	Format string `yaml:"format"`
}

// Default returns a Config with all default values.
func Default() *Config {
	return &Config{
		Server: ServerConfig{
			Listen: ":9000",
			Region: "us-east-1",
		},
		Database: DatabaseConfig{
			Path:        "./.cloodsys3/cloodsys3.db",
			BusyTimeout: 5000,
			CacheSize:   64000,
			MmapSize:    134217728,
			MaxReaders:  4,
		},
		Storage: StorageConfig{
			RootDir:         "./.cloodsys3/data",
			MultipartMaxAge: "24h",
		},
		Logging: LoggingConfig{
			Level:  "info",
			Format: "text",
		},
	}
}

func applyDefaults(cfg *Config) {
	d := Default()
	if cfg.Server.Listen == "" {
		cfg.Server.Listen = d.Server.Listen
	}
	if cfg.Server.Region == "" {
		cfg.Server.Region = d.Server.Region
	}
	if cfg.Database.Path == "" {
		cfg.Database.Path = d.Database.Path
	}
	if cfg.Database.BusyTimeout <= 0 {
		cfg.Database.BusyTimeout = d.Database.BusyTimeout
	}
	if cfg.Database.CacheSize <= 0 {
		cfg.Database.CacheSize = d.Database.CacheSize
	}
	if cfg.Database.MmapSize <= 0 {
		cfg.Database.MmapSize = d.Database.MmapSize
	}
	if cfg.Database.MaxReaders <= 0 {
		cfg.Database.MaxReaders = d.Database.MaxReaders
	}
	if cfg.Storage.RootDir == "" {
		cfg.Storage.RootDir = d.Storage.RootDir
	}
	if cfg.Storage.MultipartMaxAge == "" {
		cfg.Storage.MultipartMaxAge = d.Storage.MultipartMaxAge
	}
	if cfg.Storage.LifecycleInterval == "" {
		cfg.Storage.LifecycleInterval = "1h"
	}
	if cfg.Logging.Level == "" {
		cfg.Logging.Level = d.Logging.Level
	}
	if cfg.Logging.Format == "" {
		cfg.Logging.Format = d.Logging.Format
	}
	if cfg.Admin.Listen == "" {
		cfg.Admin.Listen = ":9001"
	}
	// Image optimization defaults (only meaningful when Image.Enabled).
	if cfg.Image.SyncMaxBytes <= 0 {
		cfg.Image.SyncMaxBytes = 2_000_000
	}
	if cfg.Image.Quality <= 0 || cfg.Image.Quality > 100 {
		cfg.Image.Quality = 75
	}
	if cfg.Image.Workers <= 0 {
		cfg.Image.Workers = 2
	}
	if cfg.Image.QueueSize <= 0 {
		cfg.Image.QueueSize = 256
	}
	// WebDAV defaults (only meaningful when WebDAV.Enabled).
	if cfg.WebDAV.Listen == "" {
		cfg.WebDAV.Listen = ":9002"
	}
	if cfg.WebDAV.Prefix == "" {
		cfg.WebDAV.Prefix = "/"
	}
}

// Load reads a YAML config file and applies defaults for missing fields.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	cfg := &Config{}
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, err
	}

	applyDefaults(cfg)
	return cfg, nil
}
