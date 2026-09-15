package config

import (
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

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
	Update   UpdateConfig   `yaml:"update"`
}

// UpdateConfig controls the self-update checks.
type UpdateConfig struct {
	// CheckOnStart makes `serve` query GitHub once at startup for a newer
	// release. Set to false for air-gapped or privacy-sensitive deployments.
	CheckOnStart *bool `yaml:"check_on_start"`
}

// WebDAVConfig exposes buckets over WebDAV so clients can mount them as network
// drives. Disabled by default. Auth maps to bucket credentials (access key as
// username, secret key as password); one credential = one bucket = the mount.
type WebDAVConfig struct {
	Enabled bool      `yaml:"enabled"` // master switch, default false
	Listen  string    `yaml:"listen"`  // listen address, default ":9002"
	Prefix  string    `yaml:"prefix"`  // URL path prefix, default "/"
	TLS     TLSConfig `yaml:"tls"`     // per-listener TLS; reuses server.tls cert/key when enabled without its own
	// LockTimeout caps WebDAV lock lifetimes (clients that omit Timeout would
	// otherwise hold infinite locks). Duration string, default "10m".
	LockTimeout string `yaml:"lock_timeout"`
}

// ImageConfig controls automatic image optimization on upload and
// transform-on-access (?w=&h=&q=). Optimization produces a separate variant
// alongside the original (the original bytes are never modified). Disabled by
// default so existing deployments are unaffected.
type ImageConfig struct {
	Enabled      bool  `yaml:"enabled"`        // master switch, default false
	SyncMaxBytes int64 `yaml:"sync_max_bytes"` // <= => optimize inline; > => async. default 2_000_000
	Quality      int   `yaml:"quality"`        // JPEG quality 1-100, default 75
	Workers      int   `yaml:"workers"`        // async worker pool size, default 2
	QueueSize    int   `yaml:"queue_size"`     // async queue buffer, default 256
	// MaxSourceBytes: objects larger than this are never decoded (default 32 MB).
	MaxSourceBytes int64 `yaml:"max_source_bytes"`
	// MaxConcurrent caps simultaneous decodes across GET transforms and the
	// upload optimizer (default 4).
	MaxConcurrent int `yaml:"max_concurrent"`
	// CacheMaxBytes bounds the per-bucket variant cache; oldest entries are
	// evicted when exceeded (default 1 GB, 0 = unlimited).
	CacheMaxBytes int64 `yaml:"cache_max_bytes"`
	// DimensionStep rounds requested widths/heights up to a multiple of this
	// value so the number of distinct variants per object stays bounded
	// (default 16, 1 = exact sizes).
	DimensionStep int `yaml:"dimension_step"`
}

type AdminConfig struct {
	Enabled     bool      `yaml:"enabled"`
	Listen      string    `yaml:"listen"`
	CORSOrigins []string  `yaml:"cors_origins"`
	TLS         TLSConfig `yaml:"tls"` // per-listener TLS; reuses server.tls cert/key when enabled without its own
	// TrustedProxies lists CIDRs/IPs whose X-Forwarded-For header is honored
	// for rate limiting. Empty = never trust the header.
	TrustedProxies []string `yaml:"trusted_proxies"`
	// SessionTTL is the admin session lifetime (duration string, default "8h").
	SessionTTL string `yaml:"session_ttl"`
}

type ServerConfig struct {
	Listen      string    `yaml:"listen"`
	Region      string    `yaml:"region"`
	TLS         TLSConfig `yaml:"tls"`
	CORSOrigins []string  `yaml:"cors_origins"`
	// VirtualHostDomains enables virtual-hosted-style requests: a request to
	// "<bucket>.<domain>" is treated as bucket "<bucket>".
	VirtualHostDomains []string `yaml:"virtual_host_domains"`
	// MaxConnections caps simultaneous S3 connections (0 = unlimited).
	MaxConnections int `yaml:"max_connections"`
	// IdleTimeout is how long a request may stall without transferring any
	// bytes before it is aborted (duration string, default "60s"). It bounds
	// slow clients without capping the total transfer time of large objects.
	IdleTimeout string `yaml:"idle_timeout"`
	// RequirePayloadSignature rejects UNSIGNED-PAYLOAD header-auth requests
	// (presigned URLs are unaffected). Default false.
	RequirePayloadSignature bool `yaml:"require_payload_signature"`
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
	RootDir           string `yaml:"root_dir"`
	MultipartMaxAge   string `yaml:"multipart_max_age"`  // Duration string, e.g. "24h"
	LifecycleInterval string `yaml:"lifecycle_interval"` // Duration string, e.g. "1h"
}

type LoggingConfig struct {
	Level  string `yaml:"level"`
	Format string `yaml:"format"`
}

// Default returns a Config with all default values.
func Default() *Config {
	cfg := &Config{}
	applyDefaults(cfg)
	return cfg
}

func applyDefaults(cfg *Config) {
	if cfg.Server.Listen == "" {
		cfg.Server.Listen = ":9000"
	}
	if cfg.Server.Region == "" {
		cfg.Server.Region = "us-east-1"
	}
	if cfg.Server.IdleTimeout == "" {
		cfg.Server.IdleTimeout = "60s"
	}
	if cfg.Database.Path == "" {
		cfg.Database.Path = "./.cloodsys3/cloodsys3.db"
	}
	if cfg.Database.BusyTimeout <= 0 {
		cfg.Database.BusyTimeout = 5000
	}
	if cfg.Database.CacheSize <= 0 {
		cfg.Database.CacheSize = 64000
	}
	if cfg.Database.MmapSize <= 0 {
		cfg.Database.MmapSize = 134217728
	}
	if cfg.Database.MaxReaders <= 0 {
		cfg.Database.MaxReaders = 4
	}
	if cfg.Storage.RootDir == "" {
		cfg.Storage.RootDir = "./.cloodsys3/data"
	}
	if cfg.Storage.MultipartMaxAge == "" {
		cfg.Storage.MultipartMaxAge = "24h"
	}
	if cfg.Storage.LifecycleInterval == "" {
		cfg.Storage.LifecycleInterval = "1h"
	}
	if cfg.Logging.Level == "" {
		cfg.Logging.Level = "info"
	}
	if cfg.Logging.Format == "" {
		cfg.Logging.Format = "text"
	}
	if cfg.Admin.Listen == "" {
		cfg.Admin.Listen = ":9001"
	}
	if cfg.Admin.SessionTTL == "" {
		cfg.Admin.SessionTTL = "8h"
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
	if cfg.Image.MaxSourceBytes <= 0 {
		cfg.Image.MaxSourceBytes = 32 << 20
	}
	if cfg.Image.MaxConcurrent <= 0 {
		cfg.Image.MaxConcurrent = 4
	}
	if cfg.Image.CacheMaxBytes == 0 {
		cfg.Image.CacheMaxBytes = 1 << 30
	}
	if cfg.Image.DimensionStep <= 0 {
		cfg.Image.DimensionStep = 16
	}
	// WebDAV defaults (only meaningful when WebDAV.Enabled).
	if cfg.WebDAV.Listen == "" {
		cfg.WebDAV.Listen = ":9002"
	}
	if cfg.WebDAV.Prefix == "" {
		cfg.WebDAV.Prefix = "/"
	}
	if cfg.WebDAV.LockTimeout == "" {
		cfg.WebDAV.LockTimeout = "10m"
	}
	if cfg.Update.CheckOnStart == nil {
		t := true
		cfg.Update.CheckOnStart = &t
	}
	// Secondary listeners reuse the S3 listener's certificate when TLS is
	// switched on for them without their own cert/key. TLS is never enabled
	// implicitly so existing GUI/WebDAV clients keep working after an upgrade.
	if cfg.Admin.TLS.Enabled && cfg.Admin.TLS.CertFile == "" && cfg.Admin.TLS.KeyFile == "" {
		cfg.Admin.TLS.CertFile, cfg.Admin.TLS.KeyFile = cfg.Server.TLS.CertFile, cfg.Server.TLS.KeyFile
	}
	if cfg.WebDAV.TLS.Enabled && cfg.WebDAV.TLS.CertFile == "" && cfg.WebDAV.TLS.KeyFile == "" {
		cfg.WebDAV.TLS.CertFile, cfg.WebDAV.TLS.KeyFile = cfg.Server.TLS.CertFile, cfg.Server.TLS.KeyFile
	}
}

// applyEnv lets deployments override the most common settings without a
// config file (useful under systemd/containers).
func applyEnv(cfg *Config) {
	set := func(env string, dst *string) {
		if v, ok := os.LookupEnv(env); ok && v != "" {
			*dst = v
		}
	}
	set("CLOODSYS3_LISTEN", &cfg.Server.Listen)
	set("CLOODSYS3_REGION", &cfg.Server.Region)
	set("CLOODSYS3_DB_PATH", &cfg.Database.Path)
	set("CLOODSYS3_DATA_DIR", &cfg.Storage.RootDir)
	set("CLOODSYS3_LOG_LEVEL", &cfg.Logging.Level)
	set("CLOODSYS3_LOG_FORMAT", &cfg.Logging.Format)
	set("CLOODSYS3_ADMIN_LISTEN", &cfg.Admin.Listen)
	set("CLOODSYS3_WEBDAV_LISTEN", &cfg.WebDAV.Listen)
	set("CLOODSYS3_TLS_CERT", &cfg.Server.TLS.CertFile)
	set("CLOODSYS3_TLS_KEY", &cfg.Server.TLS.KeyFile)
	if v, ok := os.LookupEnv("CLOODSYS3_TLS_ENABLED"); ok {
		cfg.Server.TLS.Enabled, _ = strconv.ParseBool(v)
	}
	if v, ok := os.LookupEnv("CLOODSYS3_ADMIN_ENABLED"); ok {
		cfg.Admin.Enabled, _ = strconv.ParseBool(v)
	}
	if v, ok := os.LookupEnv("CLOODSYS3_WEBDAV_ENABLED"); ok {
		cfg.WebDAV.Enabled, _ = strconv.ParseBool(v)
	}
	if v, ok := os.LookupEnv("CLOODSYS3_UPDATE_CHECK"); ok {
		b, _ := strconv.ParseBool(v)
		cfg.Update.CheckOnStart = &b
	}
}

// Validate returns an error describing the first invalid setting.
func (cfg *Config) Validate() error {
	for name, addr := range map[string]string{"server.listen": cfg.Server.Listen, "admin.listen": cfg.Admin.Listen, "webdav.listen": cfg.WebDAV.Listen} {
		if _, _, err := net.SplitHostPort(addr); err != nil {
			return fmt.Errorf("%s: invalid listen address %q: %w", name, addr, err)
		}
	}
	if cfg.Admin.Enabled && cfg.Admin.Listen == cfg.Server.Listen {
		return fmt.Errorf("admin.listen must differ from server.listen")
	}
	if cfg.WebDAV.Enabled && (cfg.WebDAV.Listen == cfg.Server.Listen || (cfg.Admin.Enabled && cfg.WebDAV.Listen == cfg.Admin.Listen)) {
		return fmt.Errorf("webdav.listen must differ from the other listeners")
	}
	for name, t := range map[string]TLSConfig{"server.tls": cfg.Server.TLS, "admin.tls": cfg.Admin.TLS, "webdav.tls": cfg.WebDAV.TLS} {
		if !t.Enabled {
			continue
		}
		if t.CertFile == "" || t.KeyFile == "" {
			return fmt.Errorf("%s: cert_file and key_file are required when enabled", name)
		}
		if _, err := os.Stat(t.CertFile); err != nil {
			return fmt.Errorf("%s.cert_file: %w", name, err)
		}
		if _, err := os.Stat(t.KeyFile); err != nil {
			return fmt.Errorf("%s.key_file: %w", name, err)
		}
	}
	switch strings.ToLower(cfg.Logging.Level) {
	case "debug", "info", "warn", "warning", "error":
	default:
		return fmt.Errorf("logging.level: unknown level %q (debug|info|warn|error)", cfg.Logging.Level)
	}
	switch strings.ToLower(cfg.Logging.Format) {
	case "text", "json":
	default:
		return fmt.Errorf("logging.format: unknown format %q (text|json)", cfg.Logging.Format)
	}
	for name, d := range map[string]string{
		"storage.multipart_max_age":  cfg.Storage.MultipartMaxAge,
		"storage.lifecycle_interval": cfg.Storage.LifecycleInterval,
		"server.idle_timeout":        cfg.Server.IdleTimeout,
		"admin.session_ttl":          cfg.Admin.SessionTTL,
		"webdav.lock_timeout":        cfg.WebDAV.LockTimeout,
	} {
		if _, err := time.ParseDuration(d); err != nil {
			return fmt.Errorf("%s: invalid duration %q", name, d)
		}
	}
	for _, p := range cfg.Admin.TrustedProxies {
		if strings.Contains(p, "/") {
			if _, _, err := net.ParseCIDR(p); err != nil {
				return fmt.Errorf("admin.trusted_proxies: invalid CIDR %q", p)
			}
		} else if net.ParseIP(p) == nil {
			return fmt.Errorf("admin.trusted_proxies: invalid IP %q", p)
		}
	}
	if cfg.Server.MaxConnections < 0 {
		return fmt.Errorf("server.max_connections must be >= 0")
	}
	return nil
}

// Duration parses a validated duration field, falling back to def.
func Duration(s string, def time.Duration) time.Duration {
	if d, err := time.ParseDuration(s); err == nil && d > 0 {
		return d
	}
	return def
}

// Load reads a YAML config file and applies env overrides and defaults.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	cfg := &Config{}
	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	dec.KnownFields(true)
	if err := dec.Decode(cfg); err != nil && err.Error() != "EOF" {
		return nil, err
	}
	applyEnv(cfg)
	applyDefaults(cfg)
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// LoadDefault builds the default config with env overrides applied.
func LoadDefault() (*Config, error) {
	cfg := &Config{}
	applyEnv(cfg)
	applyDefaults(cfg)
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}
