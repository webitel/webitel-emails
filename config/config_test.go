package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/webitel/webitel-go-kit/appconfig"
)

func TestLoadServerConfigDefaults(t *testing.T) {
	clearConfigEnvironment(t)

	cfg, err := LoadServerConfig([]string{
		"--service.conn.verify_certs=false",
		"--postgres.dsn=postgres://user:pass@localhost/webitel",
		"--pubsub.url=amqp://user:pass@localhost/",
	})
	if err != nil {
		t.Fatalf("LoadServerConfig: %v", err)
	}

	if cfg.Service.Addr != "localhost:8080" {
		t.Errorf("service.addr = %q, want localhost:8080", cfg.Service.Addr)
	}
	if cfg.Log.Level != "info" {
		t.Errorf("log.level = %q, want info", cfg.Log.Level)
	}
	if !cfg.Log.Console {
		t.Error("log.console = false, want true")
	}
	if cfg.Consul.Addr != "localhost:8500" {
		t.Errorf("consul.addr = %q, want localhost:8500", cfg.Consul.Addr)
	}
	if cfg.Pubsub.Driver != "rabbitmq" {
		t.Errorf("pubsub.driver = %q, want rabbitmq", cfg.Pubsub.Driver)
	}
}

func TestLoadServerConfigPrecedence(t *testing.T) {
	clearConfigEnvironment(t)

	path := filepath.Join(t.TempDir(), "config.yml")
	yaml := []byte(`
service:
  addr: yaml:8080
  conn:
    verify_certs: false
oauth:
  redirect_url: https://yaml.example.com/oauth/callback
log:
  level: error
postgres:
  dsn: postgres://yaml@localhost/webitel
consul:
  addr: yaml-consul:8500
pubsub:
  url: amqp://yaml@localhost/
  driver: rabbitmq
`)
	if err := os.WriteFile(path, yaml, 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	t.Setenv("SERVICE_ADDR", "env:8080")
	t.Setenv("OAUTH_REDIRECT_URL", "https://env.example.com/oauth/callback")
	t.Setenv("POSTGRES_DSN", "postgres://env@localhost/webitel")
	t.Setenv("LOG_LEVEL", "warn")

	cfg, err := LoadServerConfig([]string{
		"--config_file=" + path,
		"--service.addr=cli:8080",
		"--oauth.redirect_url=https://cli.example.com/oauth/callback",
		"--log.level=debug",
	})
	if err != nil {
		t.Fatalf("LoadServerConfig: %v", err)
	}

	if cfg.Service.Addr != "cli:8080" {
		t.Errorf("service.addr = %q, want CLI value", cfg.Service.Addr)
	}
	if cfg.Log.Level != "debug" {
		t.Errorf("log.level = %q, want CLI value", cfg.Log.Level)
	}
	if cfg.OAuth.RedirectURL != "https://cli.example.com/oauth/callback" {
		t.Errorf("oauth.redirect_url = %q, want CLI value", cfg.OAuth.RedirectURL)
	}
	if cfg.Postgres.DSN != "postgres://env@localhost/webitel" {
		t.Errorf("postgres.dsn = %q, want environment value", cfg.Postgres.DSN)
	}
	if cfg.Consul.Addr != "yaml-consul:8500" {
		t.Errorf("consul.addr = %q, want config file value", cfg.Consul.Addr)
	}
}

func TestLoadMigrateConfigUsesOnlyLogAndPostgres(t *testing.T) {
	clearConfigEnvironment(t)

	cfg, err := LoadMigrateConfig([]string{
		"--postgres.dsn=postgres://user:pass@localhost/webitel",
		"--log.level=debug",
	})
	if err != nil {
		t.Fatalf("LoadMigrateConfig: %v", err)
	}

	if cfg.Postgres.DSN != "postgres://user:pass@localhost/webitel" {
		t.Errorf("postgres.dsn = %q, want CLI value", cfg.Postgres.DSN)
	}
	if cfg.Log.Level != "debug" {
		t.Errorf("log.level = %q, want debug", cfg.Log.Level)
	}
	if cfg.Consul.Addr != "" {
		t.Errorf("consul.addr = %q, want empty for migrate config", cfg.Consul.Addr)
	}
	if cfg.Pubsub.URL != "" {
		t.Errorf("pubsub.url = %q, want empty for migrate config", cfg.Pubsub.URL)
	}
}

func TestLoadMigrateConfigPrecedence(t *testing.T) {
	clearConfigEnvironment(t)

	path := filepath.Join(t.TempDir(), "config.yml")
	yaml := []byte(`
log:
  level: error
postgres:
  dsn: postgres://yaml@localhost/webitel
`)
	if err := os.WriteFile(path, yaml, 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	t.Setenv("POSTGRES_DSN", "postgres://env@localhost/webitel")

	cfg, err := LoadMigrateConfig([]string{
		"--config_file=" + path,
		"--postgres.dsn=postgres://cli@localhost/webitel",
	})
	if err != nil {
		t.Fatalf("LoadMigrateConfig: %v", err)
	}

	if cfg.Postgres.DSN != "postgres://cli@localhost/webitel" {
		t.Errorf("postgres.dsn = %q, want CLI value", cfg.Postgres.DSN)
	}
	if cfg.Log.Level != "error" {
		t.Errorf("log.level = %q, want config file value", cfg.Log.Level)
	}
}

func TestConfigValidate(t *testing.T) {
	valid := Config{
		Service:  ServiceConfig{Addr: "localhost:8080"},
		Log:      appconfig.Log{Level: "info", Console: true},
		Postgres: appconfig.Postgres{DSN: "postgres://localhost/webitel"},
		Consul:   appconfig.Consul{Addr: "localhost:8500"},
		Pubsub:   appconfig.Pubsub{URL: "amqp://localhost/", Driver: "rabbitmq"},
	}

	tests := []struct {
		name    string
		mutate  func(*Config)
		wantErr string
	}{
		{name: "valid", mutate: func(*Config) {}},
		{name: "missing service address", mutate: func(c *Config) { c.Service.Addr = "" }, wantErr: "service.addr"},
		{name: "invalid OAuth redirect URL", mutate: func(c *Config) { c.OAuth.RedirectURL = "ftp://example.com/callback" }, wantErr: "oauth.redirect_url"},
		{name: "unsupported log level", mutate: func(c *Config) { c.Log.Level = "trace" }, wantErr: "log.level"},
		{name: "missing postgres DSN", mutate: func(c *Config) { c.Postgres.DSN = "" }, wantErr: "postgres.dsn"},
		{name: "missing consul address", mutate: func(c *Config) { c.Consul.Addr = "" }, wantErr: "consul.addr"},
		{name: "unsupported pubsub driver", mutate: func(c *Config) { c.Pubsub.Driver = "kafka" }, wantErr: "pubsub.driver"},
		{name: "invalid pubsub scheme", mutate: func(c *Config) { c.Pubsub.URL = "https://localhost" }, wantErr: "amqp or amqps"},
		{name: "missing pubsub host", mutate: func(c *Config) { c.Pubsub.URL = "amqp:///" }, wantErr: "include a host"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := valid
			tt.mutate(&cfg)

			err := cfg.validate()
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("validate: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("validate error = %v, want mention of %q", err, tt.wantErr)
			}
		})
	}
}

func clearConfigEnvironment(t *testing.T) {
	t.Helper()

	for _, name := range []string{
		"SERVICE_ADDR",
		"SERVICE_CONN_VERIFY_CERTS",
		"SERVICE_CONN_CA",
		"SERVICE_CONN_CERT",
		"SERVICE_CONN_KEY",
		"SERVICE_CONN_CLIENT_CA",
		"SERVICE_CONN_CLIENT_CERT",
		"SERVICE_CONN_CLIENT_KEY",
		"OAUTH_REDIRECT_URL",
		"LOG_LEVEL",
		"LOG_JSON",
		"LOG_OTEL",
		"LOG_FILE",
		"LOG_CONSOLE",
		"POSTGRES_DSN",
		"POSTGRES_MAX_OPEN_CONNS",
		"POSTGRES_MAX_IDLE_CONNS",
		"POSTGRES_CONN_MAX_IDLE_TIME",
		"POSTGRES_CONN_MAX_LIFETIME",
		"CONSUL",
		"CONSUL_ADDR",
		"PUBSUB_URL",
		"PUBSUB_DRIVER",
	} {
		t.Setenv(name, "")
	}
}
