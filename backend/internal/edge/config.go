package edge

import (
	"fmt"
	"strings"
	"time"

	"github.com/jucode/jucode/internal/biz"
	"github.com/jucode/jucode/internal/region"
	"github.com/spf13/viper"
)

// Config is jucode-edge's config file (configs/edge.example.yaml). Keys are
// noted next to each field; every key can also come from the environment as
// JUCODE_EDGE_<KEY>, dots replaced by underscores.
type Config struct {
	Listen string // listen
	// RegionID names this edge (region_id, required). It must equal the id
	// the control plane lists for this edge's token.
	RegionID string
	// Timezone decides calendar days and months for the API key spend
	// tripwire (timezone). Use the control plane's server.timezone.
	Timezone string
	// TrustForwardedFor takes the client IP for API key IP allowlists from
	// X-Forwarded-For (trust_forwarded_for). Only enable it when every
	// request reaches the edge through a proxy that sets the header.
	TrustForwardedFor bool
	RedisAddr         string
	RedisPassword     string
	RedisDB           int
	ControlURL        string // control.url
	ServiceToken      string // control.service_token
	SyncKey           string // control.sync_encrypt_key
	// ProvidersFile (upstreams.file) is a static upstream account snapshot
	// for standalone mode; AllowedProviderIDs (upstreams.allowed_ids) are the
	// upstream account ids this edge may forward to. The snapshot format is
	// region.ProviderSnapshotResponse.
	ProvidersFile      string
	AllowedProviderIDs []string
	OutboxDir          string
	LeaseCNY           string
	ReserveCNY         string
	OverdraftCNY       string
	RequestTTL         time.Duration
	AccountTTL         time.Duration
	SyncInterval       time.Duration
	MaxBodyBytes       int64
	RequestTimeout     time.Duration
}

func LoadConfig(path string) (Config, error) {
	v := viper.New()
	v.SetEnvPrefix("JUCODE_EDGE")
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	v.AutomaticEnv()
	v.SetDefault("listen", ":8091")
	v.SetDefault("timezone", biz.DefaultTimezone)
	v.SetDefault("trust_forwarded_for", false)
	v.SetDefault("redis.addr", "127.0.0.1:6379")
	v.SetDefault("redis.db", 1)
	v.SetDefault("outbox_dir", "./data/edge-outbox")
	v.SetDefault("lease_cny", "10")
	v.SetDefault("reserve_cny", "1")
	v.SetDefault("overdraft_cny", "10")
	v.SetDefault("request_ttl", 30*time.Minute)
	v.SetDefault("account_ttl", 10*time.Minute)
	v.SetDefault("sync_interval", 30*time.Second)
	v.SetDefault("max_body_bytes", 100<<20)
	v.SetDefault("request_timeout", 600*time.Second)
	if path != "" {
		v.SetConfigFile(path)
		if err := v.ReadInConfig(); err != nil {
			return Config{}, fmt.Errorf("read edge config %s: %w", path, err)
		}
	}
	var c Config
	c.Listen = v.GetString("listen")
	c.RedisAddr = v.GetString("redis.addr")
	c.RedisPassword = v.GetString("redis.password")
	c.RedisDB = v.GetInt("redis.db")
	c.RegionID = strings.TrimSpace(v.GetString("region_id"))
	c.Timezone = v.GetString("timezone")
	c.TrustForwardedFor = v.GetBool("trust_forwarded_for")
	c.ControlURL = strings.TrimRight(v.GetString("control.url"), "/")
	c.ServiceToken = v.GetString("control.service_token")
	c.SyncKey = v.GetString("control.sync_encrypt_key")
	c.ProvidersFile = v.GetString("upstreams.file")
	c.AllowedProviderIDs = v.GetStringSlice("upstreams.allowed_ids")
	c.OutboxDir = v.GetString("outbox_dir")
	c.LeaseCNY = v.GetString("lease_cny")
	c.ReserveCNY = v.GetString("reserve_cny")
	c.OverdraftCNY = v.GetString("overdraft_cny")
	c.RequestTTL = v.GetDuration("request_ttl")
	c.AccountTTL = v.GetDuration("account_ttl")
	c.SyncInterval = v.GetDuration("sync_interval")
	c.MaxBodyBytes = v.GetInt64("max_body_bytes")
	c.RequestTimeout = v.GetDuration("request_timeout")
	if c.RegionID == "" || c.RedisAddr == "" || c.OutboxDir == "" {
		return Config{}, fmt.Errorf("region_id, redis.addr, and outbox_dir are required")
	}
	if c.ControlURL == "" && c.ProvidersFile == "" {
		return Config{}, fmt.Errorf("one of control.url or upstreams.file is required")
	}
	if c.ControlURL != "" && len(c.AllowedProviderIDs) == 0 {
		return Config{}, fmt.Errorf("upstreams.allowed_ids is required when control.url is configured")
	}
	if c.ControlURL != "" && strings.TrimSpace(c.ServiceToken) == "" {
		return Config{}, fmt.Errorf("control.service_token is required when control.url is configured")
	}
	if len(c.SyncKey) != 64 {
		return Config{}, fmt.Errorf("control.sync_encrypt_key must be a 64-char hex key")
	}
	if _, err := time.LoadLocation(c.Timezone); err != nil {
		return Config{}, fmt.Errorf("timezone: %w", err)
	}
	if _, err := region.ParseUnits(c.ReserveCNY); err != nil {
		return Config{}, fmt.Errorf("reserve_cny: %w", err)
	}
	if lease, err := region.ParseUnits(c.LeaseCNY); err != nil || lease <= 0 {
		if err != nil {
			return Config{}, fmt.Errorf("lease_cny: %w", err)
		}
		return Config{}, fmt.Errorf("lease_cny must be positive")
	}
	if _, err := region.ParseUnits(c.OverdraftCNY); err != nil {
		return Config{}, fmt.Errorf("overdraft_cny: %w", err)
	}
	return c, nil
}
