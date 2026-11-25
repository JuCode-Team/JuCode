package biz

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/jucode/jucode/internal/data/ent"
	entaccount "github.com/jucode/jucode/internal/data/ent/upstreamaccount"
)

// Remaining-quota tracking. Two sources feed the same snapshot:
//
//   - passive sampling: every upstream response carries the vendor's own
//     rate-limit headers, so the snapshot updates on ordinary traffic at no
//     extra cost. This is the primary source.
//   - probes: for the provider definitions that expose a usage endpoint
//     (Firecrawl credits), the leader loop asks directly so an idle account
//     still knows its balance.
//
// A snapshot that crosses the configured threshold parks the account until the
// window it belongs to resets — the same temp_unschedulable path an operator
// uses, so the admin UI explains it with no special case.

// QuotaWindow is one allowance window (5h / 7d / daily / monthly…).
type QuotaWindow struct {
	Name        string  `json:"name"`
	UsedPercent float64 `json:"used_percent,omitempty"`
	Used        float64 `json:"used,omitempty"`
	Limit       float64 `json:"limit,omitempty"`
	ResetAt     int64   `json:"reset_at,omitempty"`
	// Status of the window as the vendor reports it (e.g. "surpassed").
	Status string `json:"status,omitempty"`
}

// QuotaSnapshot is the stored form: which windows were observed, from where,
// and when.
type QuotaSnapshot struct {
	Source  string        `json:"source"`
	Windows []QuotaWindow `json:"windows,omitempty"`
	Balance *QuotaBalance `json:"balance,omitempty"`
	Plan    string        `json:"plan,omitempty"`
	At      int64         `json:"at"`
}

// QuotaBalance is a pay-as-you-go balance, when the vendor exposes one.
type QuotaBalance struct {
	Amount   float64 `json:"amount"`
	Currency string  `json:"currency,omitempty"`
}

// QuotaPolicy tunes the auto-pause for one account (or the site default).
type QuotaPolicy struct {
	// PauseAtPercent is the utilization that parks the account; 0 disables
	// auto-pause entirely (an operator may still use it manually).
	PauseAtPercent float64 `json:"pause_at_percent,omitempty"`
	// Windows lists the windows that participate; empty means all of them.
	Windows []string `json:"windows,omitempty"`
}

const (
	// quotaSampleThrottle bounds passive writes to one per account per window.
	quotaSampleThrottle = time.Minute
)

// SampleQuotaHeaders merges the vendor's rate-limit headers into the stored
// snapshot. Best-effort and throttled: it runs on the request path, so it must
// never block or fail a request.
func (s *AccountPoolSvc) SampleQuotaHeaders(ctx context.Context, acctID uuid.UUID, h http.Header) {
	if acctID == uuid.Nil || h == nil || s == nil || s.d == nil {
		return
	}
	now := time.Now()
	// In-process gate first: one sampling per account per window, before any
	// Redis or database work.
	if last, ok := s.quotaSampledAt.Load(acctID); ok {
		if since, ok := last.(time.Time); ok && now.Sub(since) < quotaSampleThrottle {
			return
		}
	}
	s.quotaSampledAt.Store(acctID, now)
	if s.d.Redis != nil {
		key := fmt.Sprintf("acct:quota:sampled:%s", acctID)
		if ok, err := s.d.Redis.SetNX(ctx, key, "1", quotaSampleThrottle).Result(); err == nil && !ok {
			return
		}
	}
	windows := parseQuotaHeaders(h, now)
	if len(windows) == 0 {
		return
	}
	acct, err := s.d.Ent.UpstreamAccount.Get(ctx, acctID)
	if err != nil {
		return
	}
	snapshot := mergeQuotaSnapshot(acct.QuotaSnapshot, QuotaSnapshot{
		Source:  "passive",
		Windows: windows,
		At:      now.Unix(),
	})
	if err := s.d.Ent.UpstreamAccount.UpdateOneID(acctID).
		SetQuotaSnapshot(snapshotMap(snapshot)).
		SetQuotaCheckedAt(now).
		Exec(ctx); err != nil {
		logAccountHealth("account.quota_sample_failed", acctID, "", err)
		return
	}
	s.MaybeAutoPauseForQuota(ctx, acctID)
}

// parseQuotaHeaders reads the vendor rate-limit headers we know about.
func parseQuotaHeaders(h http.Header, now time.Time) []QuotaWindow {
	var windows []QuotaWindow
	add := func(name string, used float64, limit float64, resetAt int64, status string) {
		w := QuotaWindow{Name: name, ResetAt: resetAt, Status: status}
		switch {
		case limit > 0:
			w.Used, w.Limit = used, limit
			w.UsedPercent = used / limit * 100
		case used >= 0:
			w.UsedPercent = used
		}
		windows = append(windows, w)
	}

	// Anthropic unified windows: utilization headers are percentages (0-1 or
	// 0-100 depending on the surface), reset headers are unix seconds.
	for _, name := range []string{"5h", "7d"} {
		utilKey := http.CanonicalHeaderKey("anthropic-ratelimit-unified-" + name + "-utilization")
		resetKey := http.CanonicalHeaderKey("anthropic-ratelimit-unified-" + name + "-reset")
		surpassedKey := http.CanonicalHeaderKey("anthropic-ratelimit-unified-" + name + "-surpassed-threshold")
		used, ok := parsePercent(h.Get(utilKey))
		if !ok {
			if strings.EqualFold(strings.TrimSpace(h.Get(surpassedKey)), "true") {
				used, ok = 100, true
			}
		}
		if !ok {
			continue
		}
		var resetAt int64
		if t, ok := parseUnixOrMillis(h.Get(resetKey)); ok {
			resetAt = t.Unix()
		}
		add(name, used, 0, resetAt, "")
	}
	// When only the aggregate utilization/reset pair is present, treat it as
	// the 5h window (that is what Anthropic's aggregate covers).
	if len(windows) == 0 {
		if used, ok := parsePercent(h.Get("Anthropic-Ratelimit-Unified-Utilization")); ok {
			var resetAt int64
			if t, ok := parseUnixOrMillis(h.Get("Anthropic-Ratelimit-Unified-Reset")); ok {
				resetAt = t.Unix()
			}
			add("5h", used, 0, resetAt, "")
		}
	}

	// Codex/ChatGPT windows.
	for _, name := range []string{"primary", "secondary"} {
		usedKey := http.CanonicalHeaderKey("x-codex-" + name + "-used-percent")
		resetKey := http.CanonicalHeaderKey("x-codex-" + name + "-reset-at")
		resetAfterKey := http.CanonicalHeaderKey("x-codex-" + name + "-reset-after-seconds")
		used, ok := parsePercent(h.Get(usedKey))
		if !ok {
			continue
		}
		var resetAt int64
		if t, ok := parseUnixOrMillis(h.Get(resetKey)); ok {
			resetAt = t.Unix()
		} else if secs, err := strconv.ParseFloat(strings.TrimSpace(h.Get(resetAfterKey)), 64); err == nil {
			resetAt = now.Add(time.Duration(secs) * time.Second).Unix()
		}
		windowName := name
		if name == "primary" {
			windowName = "5h"
		} else {
			windowName = "7d"
		}
		add(windowName, used, 0, resetAt, "")
	}

	windows = append(windows, parsePolicyRateLimitWindows(h, now)...)
	return windows
}

// parsePolicyRateLimitWindows reads the list-valued RateLimit headers (Brave
// Search): X-RateLimit-Policy names the windows ("1;w=1, 15000;w=2592000") and
// Limit / Remaining / Reset carry one value per window in the same order,
// Reset in seconds from now. Only windows of a day or longer are kept — the
// per-second window is a rate limit, not a quota. A limit of 0 means
// unlimited and is skipped.
func parsePolicyRateLimitWindows(h http.Header, now time.Time) []QuotaWindow {
	policy := h.Get("X-Ratelimit-Policy")
	if policy == "" {
		return nil
	}
	split := func(raw string) []string {
		parts := strings.Split(raw, ",")
		for i := range parts {
			parts[i] = strings.TrimSpace(parts[i])
		}
		return parts
	}
	limits := split(h.Get("X-Ratelimit-Limit"))
	remaining := split(h.Get("X-Ratelimit-Remaining"))
	resets := split(h.Get("X-Ratelimit-Reset"))
	var windows []QuotaWindow
	for i, entry := range split(policy) {
		seconds := int64(0)
		for _, param := range strings.Split(entry, ";")[1:] {
			if value, ok := strings.CutPrefix(strings.TrimSpace(param), "w="); ok {
				seconds, _ = strconv.ParseInt(value, 10, 64)
			}
		}
		if seconds < 86400 || i >= len(limits) || i >= len(remaining) {
			continue
		}
		limit, errLimit := strconv.ParseFloat(limits[i], 64)
		left, errLeft := strconv.ParseFloat(remaining[i], 64)
		if errLimit != nil || errLeft != nil || limit <= 0 {
			continue
		}
		w := QuotaWindow{Name: fmt.Sprintf("%dd", seconds/86400), Limit: limit, Used: limit - left}
		w.UsedPercent = w.Used / limit * 100
		if i < len(resets) {
			if secs, err := strconv.ParseInt(resets[i], 10, 64); err == nil && secs > 0 {
				w.ResetAt = now.Add(time.Duration(secs) * time.Second).Unix()
			}
		}
		windows = append(windows, w)
	}
	return windows
}

// parsePercent accepts both 0-1 fractions and 0-100 percentages, which vendors
// mix freely across surfaces.
func parsePercent(raw string) (float64, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, false
	}
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return 0, false
	}
	if v <= 1 {
		v *= 100
	}
	if v < 0 {
		v = 0
	}
	return v, true
}

// mergeQuotaSnapshot keeps the last known value of every window: vendors report
// different windows on different endpoints, and losing one on every update
// would make the admin view flicker.
func mergeQuotaSnapshot(existing map[string]any, update QuotaSnapshot) QuotaSnapshot {
	var current QuotaSnapshot
	if len(existing) > 0 {
		if raw, err := json.Marshal(existing); err == nil {
			_ = json.Unmarshal(raw, &current)
		}
	}
	byName := make(map[string]QuotaWindow, len(current.Windows))
	order := make([]string, 0, len(current.Windows))
	for _, w := range current.Windows {
		if _, seen := byName[w.Name]; !seen {
			order = append(order, w.Name)
		}
		byName[w.Name] = w
	}
	for _, w := range update.Windows {
		if _, seen := byName[w.Name]; !seen {
			order = append(order, w.Name)
		}
		byName[w.Name] = w
	}
	out := QuotaSnapshot{
		Source: update.Source,
		Plan:   current.Plan,
		At:     update.At,
	}
	if update.Plan != "" {
		out.Plan = update.Plan
	}
	if update.Balance != nil {
		out.Balance = update.Balance
	} else {
		out.Balance = current.Balance
	}
	for _, name := range order {
		out.Windows = append(out.Windows, byName[name])
	}
	return out
}

func snapshotMap(s QuotaSnapshot) map[string]any {
	raw, err := json.Marshal(s)
	if err != nil {
		return map[string]any{}
	}
	out := map[string]any{}
	if err := json.Unmarshal(raw, &out); err != nil {
		return map[string]any{}
	}
	return out
}

// MaybeAutoPauseForQuota parks an account whose observed utilization crossed
// the threshold, until the window it belongs to resets. Returns true when the
// account was parked.
func (s *AccountPoolSvc) MaybeAutoPauseForQuota(ctx context.Context, acctID uuid.UUID) bool {
	if s == nil || s.d == nil {
		return false
	}
	acct, err := s.d.Ent.UpstreamAccount.Get(ctx, acctID)
	if err != nil {
		return false
	}
	policy := s.QuotaPolicyFor(ctx, acct)
	if policy.PauseAtPercent <= 0 {
		return false
	}
	var snapshot QuotaSnapshot
	if raw, err := json.Marshal(acct.QuotaSnapshot); err == nil {
		_ = json.Unmarshal(raw, &snapshot)
	}
	now := time.Now()
	for _, w := range snapshot.Windows {
		if !windowSelected(policy, w.Name) {
			continue
		}
		if w.UsedPercent < policy.PauseAtPercent {
			continue
		}
		until := time.Unix(w.ResetAt, 0)
		if w.ResetAt == 0 || !until.After(now) {
			// No reset advertised: park briefly and re-probe.
			until = now.Add(30 * time.Minute)
		}
		if acct.TempUnschedulableUntil != nil && acct.TempUnschedulableUntil.After(until) {
			return false // already parked at least as long
		}
		reason := fmt.Sprintf("quota_%s", w.Name)
		if err := s.d.Ent.UpstreamAccount.UpdateOneID(acctID).
			SetTempUnschedulableUntil(until).
			SetTempUnschedulableReason(reason).
			Exec(ctx); err != nil {
			logAccountHealth("account.quota_autopause_failed", acctID, reason, err)
			return false
		}
		if s.d.Redis != nil {
			_ = s.MarkCooldown(ctx, acctID, reason, time.Until(until))
		}
		return true
	}
	return false
}

func windowSelected(policy QuotaPolicy, name string) bool {
	if len(policy.Windows) == 0 {
		// Default: the short window is the one that actually gates traffic;
		// the weekly one is informational unless an operator opts in.
		return name == "5h" || name == "daily"
	}
	for _, w := range policy.Windows {
		if strings.EqualFold(w, name) {
			return true
		}
	}
	return false
}

// QuotaPolicyFor resolves the auto-pause policy: the account override, else no
// auto-pause. Pay-per-token keys are not capped by a window, so a
// vendor-reported percentage only parks an account an operator opted in.
func (s *AccountPoolSvc) QuotaPolicyFor(ctx context.Context, acct *ent.UpstreamAccount) QuotaPolicy {
	policy := QuotaPolicy{}
	if acct == nil {
		return policy
	}
	if len(acct.QuotaPolicy) > 0 {
		raw, err := json.Marshal(acct.QuotaPolicy)
		if err == nil {
			var stored QuotaPolicy
			if err := json.Unmarshal(raw, &stored); err == nil {
				policy = stored
			}
		}
	}
	return policy
}

// SetQuotaPolicy stores an account's auto-pause override. A pause threshold of
// 0 disables auto-pause for this account.
func (s *AccountPoolSvc) SetQuotaPolicy(ctx context.Context, id uuid.UUID, policy QuotaPolicy) error {
	if policy.PauseAtPercent < 0 || policy.PauseAtPercent > 100 {
		return fmt.Errorf("%w: pause_at_percent must be between 0 and 100", ErrAccountBadInput)
	}
	for _, w := range policy.Windows {
		switch strings.ToLower(strings.TrimSpace(w)) {
		case "5h", "7d", "daily", "weekly", "monthly":
		default:
			return fmt.Errorf("%w: unknown quota window %q", ErrAccountBadInput, w)
		}
	}
	raw, err := json.Marshal(policy)
	if err != nil {
		return err
	}
	stored := map[string]any{}
	if err := json.Unmarshal(raw, &stored); err != nil {
		return err
	}
	return s.d.Ent.UpstreamAccount.UpdateOneID(id).SetQuotaPolicy(stored).Exec(ctx)
}

// --- probes ---

// quotaProbe fetches remaining quota from a vendor usage endpoint with the
// account's API key.
type quotaProbe func(ctx context.Context, s *AccountPoolSvc, token, baseURL string) (QuotaSnapshot, error)

// quotaProbes is keyed by provider definition id, so a probe only exists for
// vendors whose usage endpoint is documented.
var quotaProbes = map[string]quotaProbe{
	"firecrawl": fetchFirecrawlCredits,
}

// usageProbeSupported reports whether the account can be asked for its usage —
// the same check RefreshQuota makes before probing, so the admin view can
// advertise the capability instead of letting the operator discover it via a
// failed action.
func (s *AccountPoolSvc) usageProbeSupported(_ context.Context, acct *ent.UpstreamAccount) bool {
	_, ok := quotaProbes[strings.TrimSpace(acct.DefinitionID)]
	return ok
}

// RefreshQuota asks the vendor for the account's current usage. Accounts whose
// catalog has no usage endpoint keep their passively sampled snapshot.
func (s *AccountPoolSvc) RefreshQuota(ctx context.Context, id uuid.UUID) (*QuotaSnapshot, error) {
	acct, err := s.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	snapshot, err := s.fetchQuotaSnapshot(ctx, acct)
	if err != nil {
		return nil, err
	}
	snapshot.Source = "probe"
	snapshot.At = time.Now().Unix()
	stored := mergeQuotaSnapshot(acct.QuotaSnapshot, *snapshot)
	if err := s.d.Ent.UpstreamAccount.UpdateOneID(id).
		SetQuotaSnapshot(snapshotMap(stored)).
		SetQuotaCheckedAt(time.Now()).
		Exec(ctx); err != nil {
		return nil, err
	}
	s.MaybeAutoPauseForQuota(ctx, id)
	return &stored, nil
}

// fetchQuotaSnapshot gets the vendor's answer through the definition's probe.
func (s *AccountPoolSvc) fetchQuotaSnapshot(ctx context.Context, acct *ent.UpstreamAccount) (*QuotaSnapshot, error) {
	probe, ok := quotaProbes[strings.TrimSpace(acct.DefinitionID)]
	if !ok {
		name := strings.TrimSpace(acct.Name)
		if name == "" {
			name = acct.ID.String()
		}
		return nil, fmt.Errorf("%w: account %q has no usage endpoint", ErrAccountBadInput, name)
	}
	token, err := s.RequestToken(ctx, acct)
	if err != nil {
		return nil, err
	}
	snapshot, err := probe(ctx, s, token, acct.BaseURL)
	if err != nil {
		return nil, err
	}
	return &snapshot, nil
}

// RefreshStaleQuotas probes the accounts whose quota snapshot is older than
// stale (or missing), bounded to concurrency workers. Runs on the leader loop.
func (s *AccountPoolSvc) RefreshStaleQuotas(ctx context.Context, stale time.Duration, concurrency int) (int, error) {
	if s == nil || s.d == nil {
		return 0, nil
	}
	if stale <= 0 {
		stale = 15 * time.Minute
	}
	if concurrency <= 0 {
		concurrency = 4
	}
	cutoff := time.Now().Add(-stale)
	accounts, err := s.d.Ent.UpstreamAccount.Query().
		Where(
			entaccount.DeletedAtIsNil(),
			entaccount.DefinitionIDIn(quotaProbeDefinitionIDs()...),
			entaccount.Or(
				entaccount.QuotaCheckedAtIsNil(),
				entaccount.QuotaCheckedAtLT(cutoff),
			),
		).
		Limit(200).
		All(ctx)
	if err != nil {
		return 0, err
	}
	if len(accounts) == 0 {
		return 0, nil
	}
	sem := make(chan struct{}, concurrency)
	var (
		wg     sync.WaitGroup
		mu     sync.Mutex
		probed int
	)
	for _, acct := range accounts {
		select {
		case <-ctx.Done():
			wg.Wait()
			return probed, ctx.Err()
		case sem <- struct{}{}:
		}
		wg.Add(1)
		go func(id uuid.UUID) {
			defer wg.Done()
			defer func() { <-sem }()
			probeCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
			defer cancel()
			if _, err := s.RefreshQuota(probeCtx, id); err != nil {
				slog.Debug("provider_account.quota_probe_skipped", "account_id", id.String(), "err", err.Error())
				// Stamp the attempt so a provider without a usage endpoint is
				// not re-probed on every tick.
				_ = s.d.Ent.UpstreamAccount.UpdateOneID(id).SetQuotaCheckedAt(time.Now()).Exec(probeCtx)
				return
			}
			mu.Lock()
			probed++
			mu.Unlock()
		}(acct.ID)
	}
	wg.Wait()
	return probed, nil
}

func quotaProbeDefinitionIDs() []string {
	ids := make([]string, 0, len(quotaProbes))
	for id := range quotaProbes {
		ids = append(ids, id)
	}
	return ids
}

// fetchFirecrawlCredits reads the team's remaining credits from the documented
// GET /v2/team/credit-usage
// (docs.firecrawl.dev/api-reference/endpoint/credit-usage).
func fetchFirecrawlCredits(ctx context.Context, s *AccountPoolSvc, token, baseURL string) (QuotaSnapshot, error) {
	base := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if base == "" {
		base = "https://api.firecrawl.dev"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/v2/team/credit-usage", nil)
	if err != nil {
		return QuotaSnapshot{}, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	resp, err := s.probeClient().Do(req)
	if err != nil {
		return QuotaSnapshot{}, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode >= 400 {
		return QuotaSnapshot{}, fmt.Errorf("usage endpoint %d: %s", resp.StatusCode, truncateForError(string(body)))
	}
	return parseFirecrawlCreditUsage(body)
}

func parseFirecrawlCreditUsage(body []byte) (QuotaSnapshot, error) {
	var payload struct {
		Data struct {
			RemainingCredits *float64 `json:"remainingCredits"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return QuotaSnapshot{}, fmt.Errorf("usage body is not JSON: %w", err)
	}
	if payload.Data.RemainingCredits == nil {
		return QuotaSnapshot{}, fmt.Errorf("usage body has no remainingCredits")
	}
	return QuotaSnapshot{Balance: &QuotaBalance{Amount: *payload.Data.RemainingCredits, Currency: "credits"}}, nil
}

// quotaProbeClient is the HTTP client used for usage probes. A short timeout
// keeps a stuck vendor endpoint from holding a background probe slot.
var quotaProbeClient = &http.Client{Timeout: 15 * time.Second}

func (s *AccountPoolSvc) probeClient() *http.Client { return quotaProbeClient }

func truncateForError(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 200 {
		return s[:200]
	}
	return s
}
