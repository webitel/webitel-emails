// Package config contains service configuration loading and validation.
package config

import (
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/spf13/pflag"
	"github.com/webitel/webitel-go-kit/appconfig"
)

// Config contains all settings used by the service and migration commands.
type Config struct {
	Service  ServiceConfig      `mapstructure:"service"`
	OAuth    OAuthConfig        `mapstructure:"oauth"`
	Log      appconfig.Log      `mapstructure:"log"`
	Postgres appconfig.Postgres `mapstructure:"postgres"`
	Consul   appconfig.Consul   `mapstructure:"consul"`
	Pubsub   appconfig.Pubsub   `mapstructure:"pubsub"`

	LeaderElection      LeaderElectionConfig      `mapstructure:"leader_election"`
	ProfileDistribution ProfileDistributionConfig `mapstructure:"profile_distribution"`
	IMAPPolling         IMAPPollingConfig         `mapstructure:"imap_polling"`
}

// IMAPPollingConfig tunes how an instance polls the mailboxes assigned to it.
type IMAPPollingConfig struct {
	TickInterval    time.Duration `mapstructure:"tick_interval"`
	MaxConcurrency  int           `mapstructure:"max_concurrency"`
	ShutdownTimeout time.Duration `mapstructure:"shutdown_timeout"`
	FetchBatchSize  int           `mapstructure:"fetch_batch_size"`
	// Remaining messages are fetched by an immediate next poll.
	MaxMessagesPerPoll int `mapstructure:"max_messages_per_poll"`
	// Caps IMAP connections kept open (including idle, reused ones), separately from
	// MaxConcurrency, which only bounds polls running at the same time.
	MaxOpenConnections int `mapstructure:"max_open_connections"`
}

// LeaderElectionConfig tunes the Consul session used to elect the service leader.
type LeaderElectionConfig struct {
	SessionTTL      time.Duration `mapstructure:"session_ttl"`
	LockDelay       time.Duration `mapstructure:"lock_delay"`
	RetryInterval   time.Duration `mapstructure:"retry_interval"`
	ErrorCooldown   time.Duration `mapstructure:"error_cooldown"`
	MonitorInterval time.Duration `mapstructure:"monitor_interval"`
}

// ProfileDistributionConfig tunes how the leader assigns Email Profiles to instances.
type ProfileDistributionConfig struct {
	ReconcileInterval time.Duration `mapstructure:"reconcile_interval"`
}

// OAuthConfig contains deployment-specific OAuth settings shared by providers.
type OAuthConfig struct {
	RedirectURL string `mapstructure:"redirect_url"`
}

// ServiceConfig contains the gRPC listen address and TLS connection settings.
type ServiceConfig struct {
	Addr       string             `mapstructure:"addr"`
	Connection appconfig.GRPCConn `mapstructure:"conn"`
}

// LoadServerConfig loads configuration for the long-running service process,
// resolving values from CLI flags, then environment, config file and defaults.
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
	flags.String("oauth.redirect_url", "", "public OAuth callback URL exposed through API Gateway")

	flags.Duration("leader_election.session_ttl", 15*time.Second, "Consul leader session TTL (10s..24h)")
	flags.Duration("leader_election.lock_delay", time.Second, "delay before the leader key can be taken after a session dies")
	flags.Duration("leader_election.retry_interval", 10*time.Second, "how often a standby instance tries to become leader")
	flags.Duration("leader_election.error_cooldown", 5*time.Second, "pause after a failed Consul call or leader term")
	flags.Duration("leader_election.monitor_interval", 5*time.Second, "how often the leader checks that it still holds the key")
	flags.Duration("profile_distribution.reconcile_interval", 10*time.Second, "how often the leader rebalances Email Profiles")

	flags.Duration("imap_polling.tick_interval", time.Second, "how often an instance looks for mailboxes due for a check")
	flags.Int("imap_polling.max_concurrency", 50, "maximum mailboxes polled at the same time by one instance")
	flags.Duration("imap_polling.shutdown_timeout", 10*time.Second, "how long shutdown waits for running polls")
	flags.Int("imap_polling.fetch_batch_size", 50, "messages fetched by one IMAP FETCH command")
	flags.Int("imap_polling.max_messages_per_poll", 500, "messages handled by one poll of a mailbox")
	flags.Int("imap_polling.max_open_connections", 500, "maximum IMAP connections kept open at the same time by one instance")
}

func (c *Config) validate() error {
	if c.Service.Addr == "" {
		return fmt.Errorf("config: service.addr is required")
	}
	if err := appconfig.ValidateGRPCConn("service.conn", c.Service.Connection); err != nil {
		return err
	}
	if err := validateOAuthRedirectURL(c.OAuth.RedirectURL); err != nil {
		return err
	}
	if err := c.LeaderElection.validate(); err != nil {
		return err
	}
	if c.ProfileDistribution.ReconcileInterval <= 0 {
		return fmt.Errorf("config: profile_distribution.reconcile_interval must be positive")
	}
	if err := c.IMAPPolling.validate(); err != nil {
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

func (c LeaderElectionConfig) validate() error {
	// Consul accepts session TTLs only within this range.
	if c.SessionTTL < 10*time.Second || c.SessionTTL > 24*time.Hour {
		return fmt.Errorf("config: leader_election.session_ttl must be within 10s..24h")
	}
	// Zero would silently fall back to the Consul default of 15s.
	if c.LockDelay <= 0 {
		return fmt.Errorf("config: leader_election.lock_delay must be positive")
	}
	if c.RetryInterval <= 0 || c.ErrorCooldown <= 0 || c.MonitorInterval <= 0 {
		return fmt.Errorf("config: leader_election intervals must be positive")
	}

	return nil
}

func (c IMAPPollingConfig) validate() error {
	if c.TickInterval <= 0 {
		return fmt.Errorf("config: imap_polling.tick_interval must be positive")
	}
	if c.MaxConcurrency < 1 {
		return fmt.Errorf("config: imap_polling.max_concurrency must be at least 1")
	}
	// Must leave room for other stop hooks within the default fx stop timeout of 15s.
	if c.ShutdownTimeout <= 0 || c.ShutdownTimeout >= 15*time.Second {
		return fmt.Errorf("config: imap_polling.shutdown_timeout must be within (0, 15s)")
	}
	if c.FetchBatchSize < 1 || c.MaxMessagesPerPoll < c.FetchBatchSize {
		return fmt.Errorf("config: imap_polling.fetch_batch_size must be at least 1 and not above max_messages_per_poll")
	}
	if c.MaxOpenConnections < c.MaxConcurrency {
		return fmt.Errorf("config: imap_polling.max_open_connections must be at least max_concurrency")
	}

	return nil
}

func validateOAuthRedirectURL(rawURL string) error {
	if rawURL == "" {
		return nil
	}

	u, err := url.ParseRequestURI(rawURL)
	if err != nil {
		return fmt.Errorf("config: parse oauth.redirect_url: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("config: oauth.redirect_url must use http or https scheme")
	}
	if u.Host == "" {
		return fmt.Errorf("config: oauth.redirect_url must include a host")
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
