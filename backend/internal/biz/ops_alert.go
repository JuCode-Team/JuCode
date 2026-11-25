package biz

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/jucode/jucode/internal/data"
)

const (
	OpsAlertWebhookGeneric  = "generic"
	OpsAlertWebhookDingTalk = "dingtalk"
	OpsAlertWebhookFeishu   = "feishu"

	opsAlertKeyPrefix = "ops-alert:"
)

type OpsAlertConfig struct {
	Enabled       bool
	WebhookType   string
	WebhookURL    string
	SigningSecret string
	Timeout       time.Duration
	MaxRetries    int
	SilenceWindow time.Duration
}

type OpsAlertEvent struct {
	Event         string    `json:"event"`
	Title         string    `json:"title"`
	Severity      string    `json:"severity"`
	ResourceType  string    `json:"resource_type"`
	ResourceName  string    `json:"resource_name"`
	State         string    `json:"state"`
	PreviousState string    `json:"previous_state,omitempty"`
	Message       string    `json:"message"`
	OccurredAt    time.Time `json:"occurred_at"`

	// ResourceKey is used only for deduplication and never leaves the process.
	ResourceKey string `json:"-"`
}

type opsAlertConfigProvider interface {
	GetOpsAlertConfig(context.Context) (OpsAlertConfig, error)
}

// OpsAlertSvc sends deliberately small, sanitized operational events. It does
// not accept arbitrary incident payloads, request bodies, user identifiers, or
// upstream error text.
type OpsAlertSvc struct {
	settings opsAlertConfigProvider
	redis    *redis.Client
	client   *http.Client
	logger   *slog.Logger

	mu          sync.Mutex
	localDedup  map[string]time.Time
	localStates map[string]string
	now         func() time.Time
}

func NewOpsAlertSvc(d *data.Data, settings *SettingsSvc, logger *slog.Logger) *OpsAlertSvc {
	var rdb *redis.Client
	if d != nil {
		rdb = d.Redis
	}
	return newOpsAlertSvc(settings, rdb, http.DefaultClient, logger)
}

func newOpsAlertSvc(settings opsAlertConfigProvider, rdb *redis.Client, client *http.Client, logger *slog.Logger) *OpsAlertSvc {
	if client == nil {
		client = http.DefaultClient
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &OpsAlertSvc{
		settings:    settings,
		redis:       rdb,
		client:      client,
		logger:      logger,
		localDedup:  make(map[string]time.Time),
		localStates: make(map[string]string),
		now:         time.Now,
	}
}

func normalizeOpsAlertWebhookType(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case OpsAlertWebhookDingTalk:
		return OpsAlertWebhookDingTalk
	case OpsAlertWebhookFeishu:
		return OpsAlertWebhookFeishu
	default:
		return OpsAlertWebhookGeneric
	}
}

func normalizeOpsAlertConfig(cfg OpsAlertConfig) OpsAlertConfig {
	cfg.WebhookType = normalizeOpsAlertWebhookType(cfg.WebhookType)
	cfg.WebhookURL = strings.TrimSpace(cfg.WebhookURL)
	if cfg.Timeout < time.Second || cfg.Timeout > 30*time.Second {
		cfg.Timeout = 5 * time.Second
	}
	if cfg.MaxRetries < 0 || cfg.MaxRetries > 5 {
		cfg.MaxRetries = 2
	}
	if cfg.SilenceWindow < 30*time.Second || cfg.SilenceWindow > 24*time.Hour {
		cfg.SilenceWindow = 5 * time.Minute
	}
	return cfg
}

func (s *OpsAlertSvc) config(ctx context.Context) (OpsAlertConfig, error) {
	if s == nil || s.settings == nil {
		return OpsAlertConfig{}, nil
	}
	cfg, err := s.settings.GetOpsAlertConfig(ctx)
	if err != nil {
		return OpsAlertConfig{}, err
	}
	return normalizeOpsAlertConfig(cfg), nil
}

func (s *OpsAlertSvc) Enabled(ctx context.Context) (bool, error) {
	cfg, err := s.config(ctx)
	return cfg.Enabled && cfg.WebhookURL != "", err
}

// Notify returns sent=false with no error when alerts are disabled,
// unconfigured, or suppressed by the silence window.
func (s *OpsAlertSvc) Notify(ctx context.Context, event OpsAlertEvent) (sent bool, err error) {
	cfg, err := s.config(ctx)
	if err != nil {
		return false, err
	}
	return s.notifyWithConfig(ctx, cfg, event)
}

func (s *OpsAlertSvc) notifyWithConfig(ctx context.Context, cfg OpsAlertConfig, event OpsAlertEvent) (bool, error) {
	if !cfg.Enabled || cfg.WebhookURL == "" {
		return false, nil
	}
	if event.OccurredAt.IsZero() {
		event.OccurredAt = s.now().UTC()
	} else {
		event.OccurredAt = event.OccurredAt.UTC()
	}
	dedupKey := s.dedupKey(event)
	claimed, err := s.claimDedup(ctx, dedupKey, cfg.SilenceWindow)
	if err != nil {
		return false, err
	}
	if !claimed {
		return false, nil
	}
	defer func() {
		if err != nil {
			s.releaseDedup(context.Background(), dedupKey)
		}
	}()

	target, body, headers, err := buildOpsAlertRequest(cfg, event)
	if err != nil {
		return false, err
	}
	for attempt := 0; attempt <= cfg.MaxRetries; attempt++ {
		retry, sendErr := s.sendAttempt(ctx, cfg.Timeout, target, body, headers)
		if sendErr == nil {
			return true, nil
		}
		err = sendErr
		if !retry || attempt == cfg.MaxRetries {
			return false, err
		}
		delay := 200 * time.Millisecond * time.Duration(1<<attempt)
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return false, ctx.Err()
		case <-timer.C:
		}
	}
	return false, err
}

func (s *OpsAlertSvc) sendAttempt(ctx context.Context, timeout time.Duration, target string, body []byte, headers http.Header) (bool, error) {
	attemptCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(attemptCtx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return false, err
	}
	req.Header = headers.Clone()
	resp, err := s.client.Do(req)
	if err != nil {
		// net/http errors commonly embed the full request URL. Webhook URLs
		// contain bearer-like bot tokens, so never return that error to logs.
		return true, errors.New("ops alert webhook request failed")
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	_ = resp.Body.Close()
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return false, nil
	}
	retry := resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500
	return retry, fmt.Errorf("ops alert webhook returned HTTP %d", resp.StatusCode)
}

func buildOpsAlertRequest(cfg OpsAlertConfig, event OpsAlertEvent) (string, []byte, http.Header, error) {
	parsed, err := url.Parse(cfg.WebhookURL)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || parsed.User != nil {
		return "", nil, nil, errors.New("ops alert webhook URL must be an http(s) URL without userinfo")
	}
	headers := make(http.Header)
	headers.Set("Content-Type", "application/json")
	timestamp := event.OccurredAt.Unix()

	var payload any
	switch cfg.WebhookType {
	case OpsAlertWebhookDingTalk:
		payload = map[string]any{
			"msgtype": "markdown",
			"markdown": map[string]string{
				"title": event.Title,
				"text":  opsAlertText(event, true),
			},
		}
		if cfg.SigningSecret != "" {
			ms := timestamp * 1000
			signingText := strconv.FormatInt(ms, 10) + "\n" + cfg.SigningSecret
			mac := hmac.New(sha256.New, []byte(cfg.SigningSecret))
			_, _ = mac.Write([]byte(signingText))
			query := parsed.Query()
			query.Set("timestamp", strconv.FormatInt(ms, 10))
			query.Set("sign", base64.StdEncoding.EncodeToString(mac.Sum(nil)))
			parsed.RawQuery = query.Encode()
		}
	case OpsAlertWebhookFeishu:
		feishu := map[string]any{
			"msg_type": "text",
			"content":  map[string]string{"text": opsAlertText(event, false)},
		}
		if cfg.SigningSecret != "" {
			signingKey := strconv.FormatInt(timestamp, 10) + "\n" + cfg.SigningSecret
			mac := hmac.New(sha256.New, []byte(signingKey))
			feishu["timestamp"] = strconv.FormatInt(timestamp, 10)
			feishu["sign"] = base64.StdEncoding.EncodeToString(mac.Sum(nil))
		}
		payload = feishu
	default:
		payload = struct {
			Version string `json:"version"`
			OpsAlertEvent
		}{Version: "1", OpsAlertEvent: event}
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return "", nil, nil, err
	}
	if cfg.WebhookType == OpsAlertWebhookGeneric && cfg.SigningSecret != "" {
		ts := strconv.FormatInt(timestamp, 10)
		mac := hmac.New(sha256.New, []byte(cfg.SigningSecret))
		_, _ = mac.Write([]byte(ts + "\n"))
		_, _ = mac.Write(body)
		headers.Set("X-JuCode-Alert-Timestamp", ts)
		headers.Set("X-JuCode-Alert-Signature", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	}
	return parsed.String(), body, headers, nil
}

func opsAlertText(event OpsAlertEvent, markdown bool) string {
	lines := []string{event.Title, "类型: " + event.Event, "资源: " + event.ResourceName, "状态: " + event.State}
	if event.PreviousState != "" {
		lines = append(lines, "原状态: "+event.PreviousState)
	}
	lines = append(lines, "时间: "+event.OccurredAt.Format(time.RFC3339), event.Message)
	if markdown {
		lines[0] = "### " + lines[0]
		for i := 1; i < len(lines); i++ {
			lines[i] = "- " + lines[i]
		}
	}
	return strings.Join(lines, "\n")
}

func (s *OpsAlertSvc) dedupKey(event OpsAlertEvent) string {
	resource := event.ResourceKey
	if resource == "" {
		resource = event.ResourceType + ":" + event.ResourceName
	}
	sum := sha256.Sum256([]byte(event.Event + "\x00" + resource + "\x00" + event.State))
	return opsAlertKeyPrefix + "dedup:" + hex.EncodeToString(sum[:])
}

func (s *OpsAlertSvc) claimDedup(ctx context.Context, key string, ttl time.Duration) (bool, error) {
	if s.redis != nil {
		claimed, err := s.redis.SetNX(ctx, key, "1", ttl).Result()
		if err == nil {
			return claimed, nil
		}
		s.logger.Warn("ops_alert.redis_dedup_failed", "err", err)
	}
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	if expires, ok := s.localDedup[key]; ok && expires.After(now) {
		return false, nil
	}
	s.localDedup[key] = now.Add(ttl)
	return true, nil
}

func (s *OpsAlertSvc) releaseDedup(ctx context.Context, key string) {
	if s.redis != nil {
		_ = s.redis.Del(ctx, key).Err()
	}
	s.mu.Lock()
	delete(s.localDedup, key)
	s.mu.Unlock()
}

// ReconcileStatusIncidentsAsync detects the open/closed edge for each model
// group. The state is shared in Redis when available and falls back to process
// memory; notification retries remain off the status-page request path.
func (s *OpsAlertSvc) ReconcileStatusIncidentsAsync(groups []GroupStatus) {
	if s == nil {
		return
	}
	snapshot := append([]GroupStatus(nil), groups...)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
		defer cancel()
		cfg, err := s.config(ctx)
		if err != nil || !cfg.Enabled || cfg.WebhookURL == "" {
			if err != nil {
				s.logger.Warn("ops_alert.config_failed", "err", err)
			}
			return
		}
		for _, group := range snapshot {
			current := "closed"
			if isIncidentState(group.State) {
				current = "open"
			}
			stateKey := opsAlertKeyPrefix + "incident-state:" + group.ID
			previous, found := s.loadState(ctx, stateKey)
			if !found && current == "closed" {
				s.storeState(ctx, stateKey, current)
				continue
			}
			if found && previous == current {
				continue
			}
			event := statusIncidentAlertEvent(group, current)
			sent, sendErr := s.notifyWithConfig(ctx, cfg, event)
			if sendErr != nil {
				s.logger.Warn("ops_alert.incident_delivery_failed", "group", group.Name, "err", sendErr)
				continue
			}
			if sent {
				s.storeState(ctx, stateKey, current)
			}
		}
	}()
}

// isIncidentState is shared with sla_monitor.go (same package).

func statusIncidentAlertEvent(group GroupStatus, transition string) OpsAlertEvent {
	if transition == "open" {
		return OpsAlertEvent{
			Event: "incident.opened", Title: "服务异常已打开", Severity: alertSeverity(group.State),
			ResourceType: "model_group", ResourceName: group.Name, ResourceKey: group.ID,
			State: string(group.State), Message: "状态页模型组进入异常状态。",
		}
	}
	return OpsAlertEvent{
		Event: "incident.closed", Title: "服务异常已恢复", Severity: "info",
		ResourceType: "model_group", ResourceName: group.Name, ResourceKey: group.ID,
		State: string(StatusHealthy), PreviousState: "incident_open",
		Message: "状态页模型组已恢复健康。",
	}
}

func alertSeverity(state StatusState) string {
	switch state {
	case StatusDown:
		return "critical"
	case StatusSerious:
		return "high"
	case StatusDegraded:
		return "warning"
	default:
		return "info"
	}
}

func (s *OpsAlertSvc) loadState(ctx context.Context, key string) (string, bool) {
	if s.redis != nil {
		value, err := s.redis.Get(ctx, key).Result()
		if err == nil {
			return value, true
		}
		if !errors.Is(err, redis.Nil) {
			s.logger.Warn("ops_alert.redis_state_read_failed", "err", err)
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	value, ok := s.localStates[key]
	return value, ok
}

func (s *OpsAlertSvc) storeState(ctx context.Context, key, state string) {
	if s.redis != nil {
		if err := s.redis.Set(ctx, key, state, 0).Err(); err == nil {
			return
		} else {
			s.logger.Warn("ops_alert.redis_state_write_failed", "err", err)
		}
	}
	s.mu.Lock()
	s.localStates[key] = state
	s.mu.Unlock()
}
