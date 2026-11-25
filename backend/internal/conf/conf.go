// Package conf loads runtime configuration from a YAML file with env-var
// overrides. Keep this file dependency-light so cmd/* can call it without
// pulling in the rest of the project.
package conf

import (
	"fmt"
	"strings"
	"time"

	"github.com/jucode/jucode/internal/region"
	"github.com/spf13/viper"
)

type Config struct {
	Server     ServerConfig   `mapstructure:"server"`
	Database   DatabaseConfig `mapstructure:"database"`
	Redis      RedisConfig    `mapstructure:"redis"`
	JWT        JWTConfig      `mapstructure:"jwt"`
	EncryptKey string         `mapstructure:"encrypt_key"`
	LLM        LLMConfig      `mapstructure:"llm"`
	Payments   PaymentsConfig `mapstructure:"payments"`
	Region     RegionConfig   `mapstructure:"region"`
	UsageLog   UsageLogConfig `mapstructure:"usage_log"`
	OAuth      OAuthConfig    `mapstructure:"oauth"`
}

// OAuthConfig configures the first-party OAuth clients (internal/api/oauth).
type OAuthConfig struct {
	// Clients are the accepted client ids; each may redirect to a loopback
	// address.
	Clients []string `mapstructure:"clients"`
	// WebClientID is the client that may also redirect to WebRedirectURIs.
	WebClientID string `mapstructure:"web_client_id"`
	// WebRedirectURIs are the exact callback URLs the web client may
	// redirect to after sign-in, e.g. "https://app.example.com/auth/callback".
	// Empty disables web sign-in except from loopback (local development).
	WebRedirectURIs []string `mapstructure:"web_redirect_uris"`
}

// UsageLogConfig controls how usage_logs / availability_observations rows
// reach Postgres.
type UsageLogConfig struct {
	// BatchEnabled routes writes through an async batch writer instead of
	// one transaction per request. Disabled keeps the original synchronous
	// path — this is the rollback switch.
	BatchEnabled bool `mapstructure:"batch_enabled"`
	// BufferSize is the queue depth. Rows are dropped (never blocked) when
	// it is full, so this is the tolerance for writer stalls.
	BufferSize int `mapstructure:"buffer_size"`
	// FlushMaxRows caps rows per INSERT. Clamped to 1400: usage_logs has 45
	// columns and Postgres allows 65535 bound parameters per statement.
	FlushMaxRows int `mapstructure:"flush_max_rows"`
	// FlushMaxAge bounds how long a partial batch waits. This is the lag
	// added to the admin log view, so keep it short.
	FlushMaxAge time.Duration `mapstructure:"flush_max_age"`
	// HourBucketsEnabled serves recent hourly stats windows from Redis hour
	// counters instead of scanning usage_logs. Disabled keeps every stats
	// query on SQL — the rollback switch. Counters are written regardless so
	// the ring is already warm when this is flipped on.
	HourBucketsEnabled bool `mapstructure:"hour_buckets_enabled"`
}

type ServerConfig struct {
	Listen       string `mapstructure:"listen"`
	MaxBodyBytes int64  `mapstructure:"max_body_bytes"`
	// Timezone is the IANA zone that defines calendar days and months for
	// reports, day buckets and daily/monthly spend limits.
	Timezone string `mapstructure:"timezone"`
	// TicketAttachmentDir stores ticket image uploads on disk. The JSON
	// on ticket_messages only keeps {name,type,id}; bytes never go in Postgres.
	TicketAttachmentDir string `mapstructure:"ticket_attachment_dir"`
	// TrustedProxyFetchURLs lists URLs returning newline-separated
	// CIDRs (one per line) to merge into Gin's SetTrustedProxies.
	// Defaults to Cloudflare's official v4 + v6 endpoints. Empty list
	// disables auto-fetch (use ExtraTrustedProxies + the ip_policies
	// DB table instead).
	TrustedProxyFetchURLs []string `mapstructure:"trusted_proxy_fetch_urls"`
	// ExtraTrustedProxies is a hard-coded fallback list applied when
	// the fetch above fails (network down at boot, CF unreachable).
	// Always combined with the auto-fetched list when both succeed.
	// 127.0.0.1 and ::1 are always trusted regardless of this list.
	ExtraTrustedProxies []string `mapstructure:"extra_trusted_proxies"`
}

type DatabaseConfig struct {
	DSN             string        `mapstructure:"dsn"`
	MaxOpenConns    int           `mapstructure:"max_open_conns"`
	MaxIdleConns    int           `mapstructure:"max_idle_conns"`
	ConnMaxLifetime time.Duration `mapstructure:"conn_max_lifetime"`
}

type RedisConfig struct {
	Addr     string `mapstructure:"addr"`
	Password string `mapstructure:"password"`
	DB       int    `mapstructure:"db"`
}

type JWTConfig struct {
	Secret     string        `mapstructure:"secret"`
	AccessTTL  time.Duration `mapstructure:"access_ttl"`
	RefreshTTL time.Duration `mapstructure:"refresh_ttl"`
}

type LLMConfig struct {
	MaxAttempts    int           `mapstructure:"max_attempts"`
	RequestTimeout time.Duration `mapstructure:"request_timeout"`
	// RateLimitMode governs the per-user RPM/TPM caps on the generation
	// endpoints: "off" (default), "shadow" (evaluate and log, never reject),
	// or "enforce". Start in shadow — these are the first rate caps on these
	// routes, so limits set from guesswork will otherwise reject real traffic.
	RateLimitMode string `mapstructure:"rate_limit_mode"`
}

// RegionConfig turns this server into the control plane of the
// EXPERIMENTAL regional edge feature (see docs/edge.md). Disabled, the
// /internal/region/v1 routes are not mounted and no lease loop runs. The
// edge process has its own config file and does not use this struct.
type RegionConfig struct {
	Enabled bool `mapstructure:"enabled"`
	// SyncEncryptKey encrypts upstream credentials in the snapshots sent to
	// edges. 64 hex chars; never the same value as encrypt_key.
	SyncEncryptKey string `mapstructure:"sync_encrypt_key"`
	// LeaseCNY caps one credit lease, OverdraftCNY is how far below zero a
	// balance may go when a lease is frozen, LeaseTTL is a lease's lifetime.
	LeaseCNY     string        `mapstructure:"lease_cny"`
	OverdraftCNY string        `mapstructure:"overdraft_cny"`
	LeaseTTL     time.Duration `mapstructure:"lease_ttl"`
	// Edges lists the edge nodes allowed to call the control plane. Each
	// has its own token, and the token decides which region_id it may use.
	Edges []RegionEdgeConfig `mapstructure:"edges"`
}

type RegionEdgeConfig struct {
	// ID is the edge's region_id (the same value as region_id in its
	// edge.yaml).
	ID           string `mapstructure:"id"`
	ServiceToken string `mapstructure:"service_token"`
}

type PaymentsConfig struct {
	Alipay AlipayConfig `mapstructure:"alipay"`
}

type AlipayConfig struct {
	Enabled             bool   `mapstructure:"enabled"`
	AppID               string `mapstructure:"appid"`
	GatewayURL          string `mapstructure:"gateway_url"`
	PrivateKeyFile      string `mapstructure:"private_key_file"`
	AlipayPublicKeyFile string `mapstructure:"alipay_public_key_file"`
	EncryptKey          string `mapstructure:"encrypt_key"`
	NotifyURL           string `mapstructure:"notify_url"`
}

// Load reads YAML at path (if non-empty) and applies env-var overrides.
// Env vars use prefix JUCODE_ and underscores for nested keys, e.g.
// JUCODE_DATABASE_DSN, JUCODE_JWT_SECRET.
func Load(path string) (*Config, error) {
	v := viper.New()
	v.SetEnvPrefix("JUCODE")
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	v.AutomaticEnv()

	setDefaults(v)

	if path != "" {
		v.SetConfigFile(path)
		if err := v.ReadInConfig(); err != nil {
			return nil, fmt.Errorf("read config %s: %w", path, err)
		}
	}

	var c Config
	if err := v.Unmarshal(&c); err != nil {
		return nil, fmt.Errorf("unmarshal config: %w", err)
	}
	if err := c.validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

func setDefaults(v *viper.Viper) {
	v.SetDefault("server.listen", ":8090")
	v.SetDefault("server.max_body_bytes", 100<<20)
	v.SetDefault("server.timezone", "Asia/Shanghai")
	v.SetDefault("server.ticket_attachment_dir", "./data/ticket-attachments")
	v.SetDefault("server.trusted_proxy_fetch_urls", []string{
		"https://www.cloudflare.com/ips-v4",
		"https://www.cloudflare.com/ips-v6",
	})
	v.SetDefault("database.max_open_conns", 32)
	v.SetDefault("database.max_idle_conns", 8)
	v.SetDefault("database.conn_max_lifetime", 30*time.Minute)
	v.SetDefault("redis.addr", "localhost:6379")
	v.SetDefault("redis.db", 0)
	v.SetDefault("jwt.access_ttl", 1*time.Hour)
	v.SetDefault("jwt.refresh_ttl", 30*24*time.Hour)
	v.SetDefault("llm.max_attempts", 3)
	v.SetDefault("llm.request_timeout", 120*time.Second)
	v.SetDefault("llm.rate_limit_mode", "off")
	v.SetDefault("region.enabled", false)
	v.SetDefault("region.lease_cny", "10")
	v.SetDefault("region.overdraft_cny", "10")
	v.SetDefault("region.lease_ttl", 24*time.Hour)
	// Off by default: enable per-environment after watching the drop counter.
	v.SetDefault("usage_log.batch_enabled", false)
	v.SetDefault("usage_log.buffer_size", 8192)
	v.SetDefault("usage_log.flush_max_rows", 500)
	v.SetDefault("usage_log.flush_max_age", time.Second)
	v.SetDefault("usage_log.hour_buckets_enabled", false)
	v.SetDefault("oauth.clients", []string{"jucode-cli", "jucode-desktop", "jucode-web"})
	v.SetDefault("oauth.web_client_id", "jucode-web")
	// Registered so JUCODE_OAUTH_WEB_REDIRECT_URIS (comma-separated) binds
	// without a YAML entry.
	v.SetDefault("oauth.web_redirect_uris", []string{})
	v.SetDefault("payments.alipay.enabled", false)
	v.SetDefault("payments.alipay.gateway_url", "https://openapi.alipay.com/gateway.do")
}

func (c *Config) validate() error {
	// The env form is comma-separated ("a, b"); ids and callbacks are matched
	// exactly, so stray spaces would reject every entry after the first.
	c.OAuth.Clients = trimList(c.OAuth.Clients)
	c.OAuth.WebClientID = strings.TrimSpace(c.OAuth.WebClientID)
	c.OAuth.WebRedirectURIs = trimList(c.OAuth.WebRedirectURIs)
	if len(c.OAuth.Clients) == 0 {
		return fmt.Errorf("oauth.clients must list at least one client id")
	}
	if c.Database.DSN == "" {
		return fmt.Errorf("database.dsn is required")
	}
	if c.EncryptKey == "" {
		return fmt.Errorf("encrypt_key is required (64 hex chars = 32 bytes AES-256)")
	}
	if len(c.EncryptKey) != 64 {
		return fmt.Errorf("encrypt_key must be 64 hex chars (32 bytes); got %d", len(c.EncryptKey))
	}
	if c.Region.Enabled {
		if err := c.Region.validate(); err != nil {
			return err
		}
	}
	return nil
}

func trimList(in []string) []string {
	out := in[:0]
	for _, v := range in {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	return out
}

// minServiceTokenLen keeps edge tokens out of guessing range.
const minServiceTokenLen = 32

func (r *RegionConfig) validate() error {
	if len(strings.TrimSpace(r.SyncEncryptKey)) != 64 {
		return fmt.Errorf("region.sync_encrypt_key must be 64 hex chars when enabled")
	}
	if r.LeaseTTL <= 0 {
		return fmt.Errorf("region.lease_ttl must be positive")
	}
	if lease, err := region.ParseUnits(r.LeaseCNY); err != nil || lease <= 0 {
		if err != nil {
			return fmt.Errorf("region.lease_cny: %w", err)
		}
		return fmt.Errorf("region.lease_cny must be positive")
	}
	if overdraft, err := region.ParseUnits(r.OverdraftCNY); err != nil || overdraft < 0 {
		if err != nil {
			return fmt.Errorf("region.overdraft_cny: %w", err)
		}
		return fmt.Errorf("region.overdraft_cny must be non-negative")
	}
	if len(r.Edges) == 0 {
		return fmt.Errorf("region.edges must list at least one edge when enabled")
	}
	ids := map[string]bool{}
	tokens := map[string]bool{}
	for i := range r.Edges {
		e := &r.Edges[i]
		e.ID = strings.TrimSpace(e.ID)
		e.ServiceToken = strings.TrimSpace(e.ServiceToken)
		if e.ID == "" {
			return fmt.Errorf("region.edges[%d].id is required", i)
		}
		if len(e.ServiceToken) < minServiceTokenLen {
			return fmt.Errorf("region.edges[%d].service_token must be at least %d characters", i, minServiceTokenLen)
		}
		if ids[e.ID] || tokens[e.ServiceToken] {
			return fmt.Errorf("region.edges[%d]: id and service_token must be unique", i)
		}
		ids[e.ID], tokens[e.ServiceToken] = true, true
	}
	return nil
}
