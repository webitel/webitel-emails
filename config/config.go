// Package config contains service configuration loading and validation.
package config

import (
	"fmt"
	"net/url"
	"strings"

	"github.com/spf13/pflag"
	"github.com/webitel/webitel-go-kit/appconfig"
)

type Config struct {
	Service  ServiceConfig      `mapstructure:"service"`
	Log      appconfig.Log      `mapstructure:"log"`
	Postgres appconfig.Postgres `mapstructure:"postgres"`
	Consul   appconfig.Consul   `mapstructure:"consul"`
	Pubsub   appconfig.Pubsub   `mapstructure:"pubsub"`
}

type ServiceConfig struct {
	Addr       string             `mapstructure:"addr"`
	Connection appconfig.GRPCConn `mapstructure:"conn"`
}

// LoadServerConfig loads configuration for the long-running service process.
// Values are resolved in this order: CLI flags, environment, config file,
// then defaults.
func LoadServerConfig(args []string) (*Config, error) {
	loader := appconfig.NewLoader(appconfig.Sections{
		Log:      true,
		Postgres: true,
		Consul:   true,
		Pubsub:   true,
	})

	flags := pflag.NewFlagSet("server", pflag.ContinueOnError)
	loader.RegisterFlags(flags)
	registerServiceFlags(flags)

	if err := flags.Parse(args); err != nil {
		return nil, fmt.Errorf("config: parse flags: %w", err)
	}
	if flags.NArg() != 0 {
		return nil, fmt.Errorf("config: unexpected arguments: %s", strings.Join(flags.Args(), " "))
	}

	cfg := &Config{}
	if err := loader.Load(flags, cfg); err != nil {
		return nil, err
	}
	if err := cfg.validate(); err != nil {
		return nil, err
	}

	return cfg, nil
}

// LoadMigrateConfig loads only the logging and PostgreSQL configuration
// required by the short-lived migration process.
func LoadMigrateConfig(args []string) (*Config, error) {
	loader := appconfig.NewLoader(appconfig.Sections{
		Log:      true,
		Postgres: true,
	})

	flags := pflag.NewFlagSet("migrate", pflag.ContinueOnError)
	loader.RegisterFlags(flags)

	if err := flags.Parse(args); err != nil {
		return nil, fmt.Errorf("config: parse flags: %w", err)
	}
	if flags.NArg() != 0 {
		return nil, fmt.Errorf("config: unexpected arguments: %s", strings.Join(flags.Args(), " "))
	}

	cfg := &Config{}
	if err := loader.Load(flags, cfg); err != nil {
		return nil, err
	}
	if err := cfg.validateMigrate(); err != nil {
		return nil, err
	}

	return cfg, nil
}

func registerServiceFlags(flags *pflag.FlagSet) {
	flags.String("service.addr", "localhost:8080", "gRPC listen and advertised address")
	appconfig.RegisterGRPCConnFlags(flags, "service.conn", true)
}

func (c *Config) validate() error {
	if c.Service.Addr == "" {
		return fmt.Errorf("config: service.addr is required")
	}
	if err := appconfig.ValidateGRPCConn("service.conn", c.Service.Connection); err != nil {
		return err
	}

	if err := c.validateMigrate(); err != nil {
		return err
	}
	if c.Consul.Addr == "" {
		return fmt.Errorf("config: consul.addr is required")
	}
	if c.Pubsub.Driver != "rabbitmq" {
		return fmt.Errorf("config: unsupported pubsub.driver %q", c.Pubsub.Driver)
	}
	if err := validateAMQPURL(c.Pubsub.URL); err != nil {
		return err
	}

	return nil
}

func (c *Config) validateMigrate() error {
	switch c.Log.Level {
	case "debug", "info", "warn", "error":
	case "":
		return fmt.Errorf("config: log.level is required")
	default:
		return fmt.Errorf("config: unsupported log.level %q", c.Log.Level)
	}

	if c.Postgres.DSN == "" {
		return fmt.Errorf("config: postgres.dsn is required (use --postgres.dsn or POSTGRES_DSN)")
	}

	return nil
}

func validateAMQPURL(rawURL string) error {
	if rawURL == "" {
		return fmt.Errorf("config: pubsub.url is required (use --pubsub.url or PUBSUB_URL)")
	}

	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("config: parse pubsub.url: %w", err)
	}
	if u.Scheme != "amqp" && u.Scheme != "amqps" {
		return fmt.Errorf("config: pubsub.url must use amqp or amqps scheme")
	}
	if u.Host == "" {
		return fmt.Errorf("config: pubsub.url must include a host")
	}

	return nil
}
