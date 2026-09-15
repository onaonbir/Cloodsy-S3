package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeYAML(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// clearEnv makes sure ambient CLOODSYS3_* variables do not leak into tests.
func clearEnv(t *testing.T) {
	t.Helper()
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "CLOODSYS3_") {
			t.Setenv(kv[:strings.IndexByte(kv, '=')], "")
			os.Unsetenv(kv[:strings.IndexByte(kv, '=')])
		}
	}
}

func TestDefaultValidates(t *testing.T) {
	cfg := Default()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Default().Validate(): %v", err)
	}
	if cfg.Server.Listen != ":9000" || cfg.Admin.Listen != ":9001" || cfg.WebDAV.Listen != ":9002" {
		t.Fatalf("listen defaults %+v", cfg)
	}
	if cfg.Server.Region != "us-east-1" || cfg.Server.IdleTimeout != "60s" {
		t.Fatalf("server defaults %+v", cfg.Server)
	}
	if cfg.Update.CheckOnStart == nil || !*cfg.Update.CheckOnStart {
		t.Fatal("update.check_on_start should default to true")
	}
	if cfg.Image.DimensionStep != 16 || cfg.Image.Quality != 75 || cfg.Image.CacheMaxBytes != 1<<30 || cfg.Image.MaxConcurrent != 4 {
		t.Fatalf("image defaults %+v", cfg.Image)
	}
	if cfg.Admin.SessionTTL != "8h" || cfg.WebDAV.LockTimeout != "10m" || cfg.Storage.MultipartMaxAge != "24h" {
		t.Fatalf("duration defaults %+v %+v", cfg.Admin, cfg.WebDAV)
	}
	if cfg.Admin.TLS.Enabled || cfg.WebDAV.TLS.Enabled || cfg.Server.TLS.Enabled {
		t.Fatal("TLS must be off by default")
	}
}

func TestLoad_UnknownKeyRejected(t *testing.T) {
	clearEnv(t)
	p := writeYAML(t, "server:\n  listen: \":9000\"\n  lisen: \":9001\"\n")
	if _, err := Load(p); err == nil {
		t.Fatal("unknown key accepted")
	}
	p = writeYAML(t, "bogus_section:\n  x: 1\n")
	if _, err := Load(p); err == nil {
		t.Fatal("unknown top-level section accepted")
	}
	// Empty file → all defaults.
	p = writeYAML(t, "")
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("empty file: %v", err)
	}
	if cfg.Server.Listen != ":9000" {
		t.Fatalf("empty file listen %q", cfg.Server.Listen)
	}
	if _, err := Load(filepath.Join(t.TempDir(), "missing.yaml")); err == nil {
		t.Fatal("missing file accepted")
	}
}

func TestLoad_AppliesFileValues(t *testing.T) {
	clearEnv(t)
	p := writeYAML(t, `
server:
  listen: "127.0.0.1:19000"
  region: eu-central-1
  virtual_host_domains: [s3.local]
  cors_origins: ["https://app.example"]
  idle_timeout: 5m
admin:
  enabled: true
  listen: "127.0.0.1:19001"
  trusted_proxies: ["10.0.0.0/8", "192.168.1.1"]
image:
  enabled: true
  dimension_step: 1
  cache_max_bytes: -1
update:
  check_on_start: false
`)
	cfg, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Server.Listen != "127.0.0.1:19000" || cfg.Server.Region != "eu-central-1" || cfg.Server.IdleTimeout != "5m" {
		t.Fatalf("server %+v", cfg.Server)
	}
	if len(cfg.Server.VirtualHostDomains) != 1 || cfg.Server.VirtualHostDomains[0] != "s3.local" {
		t.Fatalf("vhost %v", cfg.Server.VirtualHostDomains)
	}
	if !cfg.Admin.Enabled || cfg.Admin.Listen != "127.0.0.1:19001" || len(cfg.Admin.TrustedProxies) != 2 {
		t.Fatalf("admin %+v", cfg.Admin)
	}
	if !cfg.Image.Enabled || cfg.Image.DimensionStep != 1 || cfg.Image.CacheMaxBytes != -1 {
		t.Fatalf("image %+v (explicit 1 / -1 must be preserved)", cfg.Image)
	}
	if cfg.Update.CheckOnStart == nil || *cfg.Update.CheckOnStart {
		t.Fatal("update.check_on_start=false not honored")
	}
}

func TestValidate_Errors(t *testing.T) {
	cases := []struct {
		name string
		mut  func(*Config)
		want string
	}{
		{"invalid server listen", func(c *Config) { c.Server.Listen = "nope" }, "server.listen"},
		{"invalid admin listen", func(c *Config) { c.Admin.Listen = "127.0.0.1" }, "admin.listen"},
		{"invalid webdav listen", func(c *Config) { c.WebDAV.Listen = "::1:9002" }, "webdav.listen"},
		{"admin same as server", func(c *Config) { c.Admin.Enabled = true; c.Admin.Listen = c.Server.Listen }, "admin.listen must differ"},
		{"webdav same as server", func(c *Config) { c.WebDAV.Enabled = true; c.WebDAV.Listen = c.Server.Listen }, "webdav.listen must differ"},
		{"webdav same as admin", func(c *Config) { c.WebDAV.Enabled = true; c.Admin.Enabled = true; c.WebDAV.Listen = c.Admin.Listen }, "webdav.listen must differ"},
		{"bad idle timeout", func(c *Config) { c.Server.IdleTimeout = "soon" }, "server.idle_timeout"},
		{"bad lifecycle interval", func(c *Config) { c.Storage.LifecycleInterval = "1 hour" }, "storage.lifecycle_interval"},
		{"bad multipart age", func(c *Config) { c.Storage.MultipartMaxAge = "x" }, "storage.multipart_max_age"},
		{"bad session ttl", func(c *Config) { c.Admin.SessionTTL = "8" }, "admin.session_ttl"},
		{"bad lock timeout", func(c *Config) { c.WebDAV.LockTimeout = "" }, "webdav.lock_timeout"},
		{"server tls without files", func(c *Config) { c.Server.TLS.Enabled = true }, "cert_file and key_file are required"},
		{"server tls missing cert file", func(c *Config) {
			c.Server.TLS = TLSConfig{Enabled: true, CertFile: "/nonexistent/c.pem", KeyFile: "/nonexistent/k.pem"}
		}, "server.tls.cert_file"},
		{"admin tls without files", func(c *Config) { c.Admin.TLS.Enabled = true }, "admin.tls"},
		{"webdav tls without files", func(c *Config) { c.WebDAV.TLS.Enabled = true }, "webdav.tls"},
		{"bad log level", func(c *Config) { c.Logging.Level = "verbose" }, "logging.level"},
		{"bad log format", func(c *Config) { c.Logging.Format = "xml" }, "logging.format"},
		{"bad trusted proxy cidr", func(c *Config) { c.Admin.TrustedProxies = []string{"10.0.0.0/99"} }, "trusted_proxies"},
		{"bad trusted proxy ip", func(c *Config) { c.Admin.TrustedProxies = []string{"not-an-ip"} }, "trusted_proxies"},
		{"negative max connections", func(c *Config) { c.Server.MaxConnections = -1 }, "max_connections"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := Default()
			c.mut(cfg)
			err := cfg.Validate()
			if err == nil {
				t.Fatalf("no error")
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Fatalf("error %q does not mention %q", err, c.want)
			}
		})
	}
	// Disabled listeners may share addresses and skip TLS file checks.
	cfg := Default()
	cfg.Admin.Listen = cfg.Server.Listen
	cfg.WebDAV.Listen = cfg.Server.Listen
	cfg.Logging.Level = "WARNING"
	cfg.Logging.Format = "JSON"
	if err := cfg.Validate(); err != nil {
		t.Fatalf("disabled listeners: %v", err)
	}
}

func TestLoad_ValidatesFile(t *testing.T) {
	clearEnv(t)
	for name, body := range map[string]string{
		"invalid listen":   "server:\n  listen: nope\n",
		"admin==server":    "server:\n  listen: \":9000\"\nadmin:\n  enabled: true\n  listen: \":9000\"\n",
		"bad duration":     "storage:\n  lifecycle_interval: yearly\n",
		"tls without file": "server:\n  tls:\n    enabled: true\n",
		"bad yaml":         "server: [\n",
	} {
		if _, err := Load(writeYAML(t, body)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestEnvOverrides(t *testing.T) {
	clearEnv(t)
	t.Setenv("CLOODSYS3_LISTEN", "127.0.0.1:29000")
	t.Setenv("CLOODSYS3_REGION", "ap-south-1")
	t.Setenv("CLOODSYS3_DB_PATH", "/tmp/x.db")
	t.Setenv("CLOODSYS3_DATA_DIR", "/tmp/data")
	t.Setenv("CLOODSYS3_LOG_LEVEL", "debug")
	t.Setenv("CLOODSYS3_LOG_FORMAT", "json")
	t.Setenv("CLOODSYS3_ADMIN_LISTEN", "127.0.0.1:29001")
	t.Setenv("CLOODSYS3_ADMIN_ENABLED", "true")
	t.Setenv("CLOODSYS3_WEBDAV_LISTEN", "127.0.0.1:29002")
	t.Setenv("CLOODSYS3_WEBDAV_ENABLED", "1")
	t.Setenv("CLOODSYS3_UPDATE_CHECK", "false")

	cfg, err := LoadDefault()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Server.Listen != "127.0.0.1:29000" || cfg.Server.Region != "ap-south-1" {
		t.Fatalf("server %+v", cfg.Server)
	}
	if cfg.Database.Path != "/tmp/x.db" || cfg.Storage.RootDir != "/tmp/data" {
		t.Fatalf("paths %+v %+v", cfg.Database, cfg.Storage)
	}
	if cfg.Logging.Level != "debug" || cfg.Logging.Format != "json" {
		t.Fatalf("logging %+v", cfg.Logging)
	}
	if !cfg.Admin.Enabled || cfg.Admin.Listen != "127.0.0.1:29001" || !cfg.WebDAV.Enabled || cfg.WebDAV.Listen != "127.0.0.1:29002" {
		t.Fatalf("admin/webdav %+v %+v", cfg.Admin, cfg.WebDAV)
	}
	if cfg.Update.CheckOnStart == nil || *cfg.Update.CheckOnStart {
		t.Fatal("CLOODSYS3_UPDATE_CHECK=false not applied")
	}

	// Env wins over the file.
	p := writeYAML(t, "server:\n  listen: \":9000\"\n  region: us-east-1\nupdate:\n  check_on_start: true\n")
	cfg, err = Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Server.Listen != "127.0.0.1:29000" || cfg.Server.Region != "ap-south-1" || *cfg.Update.CheckOnStart {
		t.Fatalf("env did not override file: %+v", cfg.Server)
	}
	// Empty env values are ignored; unparsable booleans mean false.
	t.Setenv("CLOODSYS3_LISTEN", "")
	t.Setenv("CLOODSYS3_ADMIN_ENABLED", "maybe")
	cfg, err = LoadDefault()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Server.Listen != ":9000" || cfg.Admin.Enabled {
		t.Fatalf("empty/invalid env handling %+v %+v", cfg.Server, cfg.Admin)
	}
}

func TestEnvTLS(t *testing.T) {
	clearEnv(t)
	dir := t.TempDir()
	cert := filepath.Join(dir, "cert.pem")
	key := filepath.Join(dir, "key.pem")
	os.WriteFile(cert, []byte("cert"), 0o600)
	os.WriteFile(key, []byte("key"), 0o600)
	t.Setenv("CLOODSYS3_TLS_ENABLED", "true")
	t.Setenv("CLOODSYS3_TLS_CERT", cert)
	t.Setenv("CLOODSYS3_TLS_KEY", key)
	cfg, err := LoadDefault()
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Server.TLS.Enabled || cfg.Server.TLS.CertFile != cert || cfg.Server.TLS.KeyFile != key {
		t.Fatalf("tls %+v", cfg.Server.TLS)
	}
	t.Setenv("CLOODSYS3_TLS_KEY", filepath.Join(dir, "missing.pem"))
	if _, err := LoadDefault(); err == nil {
		t.Fatal("missing key file accepted")
	}
}

func TestSecondaryListenersReuseServerTLS(t *testing.T) {
	clearEnv(t)
	dir := t.TempDir()
	cert := filepath.Join(dir, "cert.pem")
	key := filepath.Join(dir, "key.pem")
	os.WriteFile(cert, []byte("cert"), 0o600)
	os.WriteFile(key, []byte("key"), 0o600)
	own := filepath.Join(dir, "own.pem")
	os.WriteFile(own, []byte("own"), 0o600)

	p := writeYAML(t, "server:\n  tls:\n    enabled: true\n    cert_file: "+cert+"\n    key_file: "+key+"\nadmin:\n  enabled: true\n  tls:\n    enabled: true\nwebdav:\n  enabled: true\n  tls:\n    enabled: true\n    cert_file: "+own+"\n    key_file: "+own+"\n")
	cfg, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Admin.TLS.CertFile != cert || cfg.Admin.TLS.KeyFile != key {
		t.Fatalf("admin tls did not inherit server cert: %+v", cfg.Admin.TLS)
	}
	if cfg.WebDAV.TLS.CertFile != own || cfg.WebDAV.TLS.KeyFile != own {
		t.Fatalf("webdav own cert overwritten: %+v", cfg.WebDAV.TLS)
	}
	// Admin TLS is never enabled implicitly by server TLS.
	p = writeYAML(t, "server:\n  tls:\n    enabled: true\n    cert_file: "+cert+"\n    key_file: "+key+"\nadmin:\n  enabled: true\n")
	cfg, err = Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Admin.TLS.Enabled || cfg.WebDAV.TLS.Enabled {
		t.Fatal("secondary TLS enabled implicitly")
	}
	// Admin TLS on with no server cert to inherit → validation error.
	p = writeYAML(t, "admin:\n  enabled: true\n  tls:\n    enabled: true\n")
	if _, err := Load(p); err == nil || !strings.Contains(err.Error(), "admin.tls") {
		t.Fatalf("want admin.tls error, got %v", err)
	}
}

func TestDuration(t *testing.T) {
	if Duration("90s", time.Minute) != 90*time.Second {
		t.Fatal("parse")
	}
	if Duration("bad", time.Minute) != time.Minute || Duration("", time.Minute) != time.Minute {
		t.Fatal("fallback")
	}
	if Duration("0s", time.Minute) != time.Minute || Duration("-1s", time.Minute) != time.Minute {
		t.Fatal("non-positive should fall back")
	}
}
