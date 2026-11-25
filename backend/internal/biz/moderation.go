// Package biz — async content moderation via the OpenAI Moderation API.
//
// ModerationSvc adds a high-precision layer on top of the cheap keyword
// scanner WITHOUT touching the request hot path: every review runs in a
// detached goroutine after the gateway has already decided what to do with
// the request. It calls an OpenAI-compatible Moderation endpoint
// (omni-moderation-latest) which returns per-category scores in [0,1]; a
// request is treated as a violation when any score meets the configured
// threshold ("审查强度").
//
// All runtime config (enable, base_url, api_key, model, threshold, sample
// rate, rate cap) is read from SiteSettings so an admin can change it from
// the console with no redeploy; the API key is stored encrypted-at-rest.
//
// Cost control: clean traffic is only sampled at SampleRate; a per-minute
// token bucket caps upstream calls; identical payloads reuse a cached verdict.
// Confirmed high+ violations feed SanctionSvc — the only path to account
// freezes/bans.
package biz

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"

	entincident "github.com/jucode/jucode/internal/data/ent/userincident"
)

const moderationMaxChars = 8000

type ModerationSvc struct {
	settings  *SettingsSvc
	rdb       *redis.Client
	incidents *IncidentSvc
	sanctions *SanctionSvc
	http      *http.Client
	log       *slog.Logger
}

// ModerationRequest is the gateway's hand-off. Text is the already
// image-redacted request body; Scan is the keyword scan result.
type ModerationRequest struct {
	UserID    uuid.UUID
	ApiKeyID  *uuid.UUID
	Model     string
	Endpoint  string
	RequestID string
	IP        string
	Text      string
	Scan      PolicyScanResult
}

func NewModerationSvc(settings *SettingsSvc, rdb *redis.Client, incidents *IncidentSvc, sanctions *SanctionSvc, log *slog.Logger) *ModerationSvc {
	if log == nil {
		log = slog.Default()
	}
	return &ModerationSvc{
		settings:  settings,
		rdb:       rdb,
		incidents: incidents,
		sanctions: sanctions,
		http:      &http.Client{Timeout: 15 * time.Second},
		log:       log,
	}
}

// MaybeReview decides whether req warrants a moderation check and, if so, runs
// it asynchronously. Safe on a nil receiver and never blocks the caller.
func (m *ModerationSvc) MaybeReview(req ModerationRequest) {
	if m == nil || m.settings == nil || req.Text == "" {
		return
	}
	st, err := m.settings.GetSite(context.Background())
	if err != nil || st == nil || !st.ModerationEnabled || st.ModerationBaseURL == "" || st.ModerationModel == "" {
		return
	}
	flagged := len(req.Scan.Hits) > 0 || len(req.Scan.Suspects) > 0
	if !flagged && !sampleHit(req.UserID, st.ModerationSampleRate) {
		return
	}
	go m.review(req, flagged, st)
}

// sampleHit returns true for a `rate` fraction of inputs, deterministic per
// user+minute so a given user isn't sampled repeatedly within a minute.
func sampleHit(uid uuid.UUID, rate float64) bool {
	if rate <= 0 {
		return false
	}
	if rate >= 1 {
		return true
	}
	h := sha256.Sum256([]byte(uid.String() + time.Now().Format("200601021504")))
	v := float64(uint32(h[0])<<24|uint32(h[1])<<16|uint32(h[2])<<8|uint32(h[3])) / float64(1<<32)
	return v < rate
}

func (m *ModerationSvc) review(req ModerationRequest, flagged bool, st *SiteSettings) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	key, err := m.settings.GetModerationSecret(ctx)
	if err != nil || key == "" {
		m.log.Warn("moderation.no_key", "request_id", req.RequestID, "err", errStr(err))
		return
	}
	if !m.allow(ctx, st.ModerationMaxPerMin) {
		m.log.Warn("moderation.rate_limited", "request_id", req.RequestID)
		return
	}

	text := req.Text
	if len(text) > moderationMaxChars {
		text = text[:moderationMaxChars]
	}
	sum := sha256.Sum256(fmt.Appendf(nil, "%s|%g|%s", st.ModerationModel, st.ModerationThreshold, text))
	cacheKey := "mod:verdict:" + hex.EncodeToString(sum[:])

	v, ok := m.cachedVerdict(ctx, cacheKey)
	if !ok {
		scores, cerr := m.callModeration(ctx, st.ModerationBaseURL, key, st.ModerationModel, text)
		if cerr != nil {
			m.log.Warn("moderation.api_failed", "request_id", req.RequestID, "err", cerr.Error())
			return
		}
		v = evaluateScores(scores, st.ModerationThreshold)
		m.cacheVerdict(ctx, cacheKey, v)
	}
	m.apply(ctx, req, v, flagged)
}

func (m *ModerationSvc) apply(ctx context.Context, req ModerationRequest, v modVerdict, flagged bool) {
	action := "logged"
	msg := "moderation: clean"
	if v.Violation {
		msg = fmt.Sprintf("moderation: violation %s (max %.2f)", strings.Join(v.Categories, ","), v.MaxScore)
	}
	uid := req.UserID
	payload := map[string]any{
		"violation":  v.Violation,
		"categories": v.Categories,
		"max_score":  v.MaxScore,
		"flagged":    flagged,
		"sampled":    !flagged,
	}
	m.incidents.RecordAsync(IncidentInput{
		UserID:    &uid,
		ApiKeyID:  req.ApiKeyID,
		Kind:      string(entincident.KindLlmModeration),
		Severity:  v.Severity,
		Action:    action,
		Model:     req.Model,
		Endpoint:  req.Endpoint,
		RequestID: req.RequestID,
		IP:        req.IP,
		Message:   msg,
		Payload:   payload,
	})
	if v.Violation && severityRankBiz(v.Severity) >= severityRankBiz("high") {
		m.sanctions.RegisterAbuse(ctx, uid, v.Severity, "moderation-confirmed: "+strings.Join(v.Categories, ","))
	}
}

// --- OpenAI Moderation API ---

type modVerdict struct {
	Violation  bool     `json:"violation"`
	Severity   string   `json:"severity"`
	Categories []string `json:"categories"`
	MaxScore   float64  `json:"max_score"`
}

// callModeration posts to {base}/moderations and returns the per-category
// scores from the first result.
func (m *ModerationSvc) callModeration(ctx context.Context, baseURL, apiKey, model, text string) (map[string]float64, error) {
	body, _ := json.Marshal(map[string]any{"model": model, "input": text})
	url := strings.TrimRight(baseURL, "/") + "/moderations"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+apiKey)

	resp, err := m.http.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("moderation upstream status %d: %s", resp.StatusCode, string(raw))
	}
	var parsed struct {
		Results []struct {
			CategoryScores map[string]float64 `json:"category_scores"`
		} `json:"results"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, err
	}
	if len(parsed.Results) == 0 {
		return nil, fmt.Errorf("moderation upstream returned no results")
	}
	return parsed.Results[0].CategoryScores, nil
}

// evaluateScores turns category scores into a verdict given the action
// threshold. Violation = any category at/above the threshold. Severity scales
// with the top score; sexual/minors is forced to critical regardless.
func evaluateScores(scores map[string]float64, threshold float64) modVerdict {
	if threshold <= 0 {
		threshold = 0.5
	}
	v := modVerdict{Severity: "low"}
	type cs struct {
		name  string
		score float64
	}
	var over []cs
	for name, score := range scores {
		if score > v.MaxScore {
			v.MaxScore = score
		}
		if score >= threshold {
			over = append(over, cs{name, score})
		}
	}
	if len(over) == 0 {
		return v
	}
	// Highest-scoring categories first, capped to keep the incident concise.
	sort.Slice(over, func(i, j int) bool { return over[i].score > over[j].score })
	v.Violation = true
	forceCritical := false
	for i, c := range over {
		if i < 8 {
			v.Categories = append(v.Categories, c.name)
		}
		if c.name == "sexual/minors" {
			forceCritical = true
		}
	}
	switch {
	case forceCritical || v.MaxScore >= 0.95:
		v.Severity = "critical"
	case v.MaxScore >= 0.85:
		v.Severity = "high"
	case v.MaxScore >= 0.70:
		v.Severity = "medium"
	default:
		v.Severity = "low"
	}
	return v
}

// --- Redis: token bucket + verdict cache ---

// allow consumes one token from a per-minute fixed-window bucket. maxPerMin<=0
// disables the cap. Fails OPEN on Redis errors (the cap is a cost guard).
func (m *ModerationSvc) allow(ctx context.Context, maxPerMin int) bool {
	if m.rdb == nil || maxPerMin <= 0 {
		return true
	}
	key := "mod:rl:" + time.Now().Format("200601021504")
	n, err := m.rdb.Incr(ctx, key).Result()
	if err != nil {
		return true
	}
	if n == 1 {
		_ = m.rdb.Expire(ctx, key, 70*time.Second).Err()
	}
	return n <= int64(maxPerMin)
}

func (m *ModerationSvc) cachedVerdict(ctx context.Context, key string) (modVerdict, bool) {
	if m.rdb == nil {
		return modVerdict{}, false
	}
	raw, err := m.rdb.Get(ctx, key).Bytes()
	if err != nil {
		return modVerdict{}, false
	}
	var v modVerdict
	if json.Unmarshal(raw, &v) != nil {
		return modVerdict{}, false
	}
	return v, true
}

func (m *ModerationSvc) cacheVerdict(ctx context.Context, key string, v modVerdict) {
	if m.rdb == nil {
		return
	}
	if b, err := json.Marshal(v); err == nil {
		_ = m.rdb.Set(ctx, key, b, 24*time.Hour).Err()
	}
}

func errStr(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
