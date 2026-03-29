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
}

type AdminConfig struct {
	Enabled     bool     `yaml:"enabled"`
	Listen      string   `yaml:"listen"`
	CORSOrigins []string `yaml:"cors_origins"`
}

type ServerConfig struct {
	Listen string    `yaml:"listen"`
	Region string    `yaml:"region"`
	TLS    TLSConfig `yaml:"tls"`
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
