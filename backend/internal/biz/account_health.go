package biz

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/jucode/jucode/internal/data/ent"
	entaccount "github.com/jucode/jucode/internal/data/ent/upstreamaccount"
)

// Account health: one upstream failure is turned into a cooldown, a disable or
// nothing, according to the failure class and the pool's policy mode.
//
// Two kinds of state exist:
//
//   - time-based cooldowns (rate_limit_reset_at / overload_until /
//     temp_unschedulable_until / per-model cooldowns). They lift by themselves;
//     the scheduler simply skips the account until then, and nothing has to
//     clean them up.
//   - a hard disable (status=error), reserved for failures that will not heal
//     by waiting: a rejected credential, a deactivated organization, an
//     exhausted balance, or an error class that keeps repeating in a short
//     window. Recovery is explicit (admin clear or a successful account test).
//
// The runtime cooldown also lives in Redis so the very next request in another
// worker sees it; the database columns are the durable, admin-visible copy.

// PoolMode selects how aggressively an account is taken out of rotation.
type PoolMode string

const (
	// PoolModeBalanced cools by failure class and disables only on clear
	// terminal signals (default).
	PoolModeBalanced PoolMode = "balanced"
	// PoolModeConservative cools longer and disables sooner: for scarce or
	// expensive accounts where burning quota on retries hurts.
	PoolModeConservative PoolMode = "conservative"
	// PoolModeLenient never writes account state for transient upstream
	// failures and retries the same account instead (a pool of many
	// interchangeable keys, where upstream errors are expected). It
	// never disables: terminal-looking failures only cool for an hour.
	PoolModeLenient PoolMode = "lenient"
	// PoolModeObserve records everything for the admin UI and metrics but
	// never removes an account from rotation.
	PoolModeObserve PoolMode = "observe"

	// SettingKeyDefaultPoolMode is the site-wide fallback pool mode.
	SettingKeyDefaultPoolMode = "default_pool_mode"
)

// ParsePoolMode validates an admin-supplied mode. Empty means "inherit".
func ParsePoolMode(raw string) (PoolMode, bool) {
	switch PoolMode(strings.ToLower(strings.TrimSpace(raw))) {
	case "":
		return "", true
	case PoolModeBalanced:
		return PoolModeBalanced, true
	case PoolModeConservative:
		return PoolModeConservative, true
	case PoolModeLenient:
		return PoolModeLenient, true
	case PoolModeObserve:
		return PoolModeObserve, true
	default:
		return "", false
	}
}

// ResolvePoolMode returns the policy that applies to an account: its own
// override, else the site default, else balanced.
func (s *AccountPoolSvc) ResolvePoolMode(ctx context.Context, acct *ent.UpstreamAccount) PoolMode {
	if acct != nil && acct.PoolMode != "" {
		if mode, ok := ParsePoolMode(acct.PoolMode); ok && mode != "" {
			return mode
		}
	}
	if s != nil && s.settings != nil {
		if site, err := s.settings.GetSite(ctx); err == nil && site != nil && strings.TrimSpace(site.DefaultPoolMode) != "" {
			if parsed, ok := ParsePoolMode(site.DefaultPoolMode); ok && parsed != "" {
				return parsed
			}
		}
	}
	return PoolModeBalanced
}

// FailureAction is what the state machine decided to do with an account.
type FailureAction int

const (
	// ActionNone: the failure says nothing about account health (client
	// cancellation, request-level 4xx, upstream pool exhaustion, ...).
	ActionNone FailureAction = iota
	// ActionCooldown: skip the account until Until.
	ActionCooldown
	// ActionDisable: take the account out of rotation until an explicit
	// recovery.
	ActionDisable
)

// AccountFailure is everything the gateway knows about one upstream failure.
type AccountFailure struct {
	// Class is the gateway error class (rate_limit, auth, provider_failure,
	// timeout, pool_exhausted, cyber_block, user_request, client_canceled…).
	Class string
	// Status is the upstream HTTP status, 0 for transport/timeout failures.
	Status int
	// Reason is the gateway's strategy string (status_429, first_text_timeout…).
	Reason string
	// Headers carries the upstream response headers, used to read the reset
	// time a rate limit advertises.
	Headers http.Header
	// Body is the (bounded) upstream error body.
	Body string
	// Model is the model the request asked for, for model-scoped cooldowns.
	Model string
}

// AccountFailureOutcome is the state machine's verdict, already applied.
type AccountFailureOutcome struct {
	Action FailureAction
	Until  time.Time
	Reason string
	// RetrySame tells the gateway to try the same account again (lenient mode)
	// instead of moving on.
	RetrySame bool
}

// Cooldown bounds: honouring a huge advertised reset could park an account for
// days, and a tiny one re-probes a saturated upstream immediately.
const (
	accountCooldownFloor = 5 * time.Second
	accountCooldownCeil  = 2 * time.Hour
	accountRateFallback  = 30 * time.Second
	// failure window counters
	failureWindowTTL = 180 * time.Minute
	// modelCooldownDefault parks one model (not the account) after a
	// model-not-found style rejection.
	modelCooldownDefault = 30 * time.Minute
)

// ReportAccountFailure classifies one upstream failure, applies the state it
// implies for this account, and returns what the caller should do next.
func (s *AccountPoolSvc) ReportAccountFailure(ctx context.Context, acct *ent.UpstreamAccount, f AccountFailure) AccountFailureOutcome {
	if acct == nil {
		return AccountFailureOutcome{Action: ActionNone}
	}
	mode := s.ResolvePoolMode(ctx, acct)
	reason := f.Reason
	if reason == "" {
		reason = f.Class
	}

	// Failures that say nothing about this account's health: upstream pool
	// exhaustion is global capacity, and the rest were caused by the request
	// itself. They are recorded for the admin view but must not count as
	// account failures (that counter feeds the "repeated 403" disable rule).
	switch f.Class {
	case "pool_exhausted", "client_canceled", "user_request", "user_policy", "cyber_block":
		s.recordAccountEvent(ctx, acct.ID, reason)
		return AccountFailureOutcome{Action: ActionNone}
	}

	// Observe mode never changes rotation, but still records what happened.
	if mode == PoolModeObserve {
		s.noteAccountFailure(ctx, acct, reason, false)
		return AccountFailureOutcome{Action: ActionNone}
	}

	if mode == PoolModeLenient {
		// Transient upstream trouble: keep the account eligible and let the
		// caller retry the same account.
		if !isTerminalAccountFailure(f) {
			outcome := AccountFailureOutcome{Action: ActionNone, RetrySame: true, Reason: reason}
			s.noteAccountFailure(ctx, acct, reason, false)
			return outcome
		}
	}

	switch f.Class {
	case "rate_limit":
		until := s.rateLimitReset(f)
		if mode == PoolModeConservative {
			if min := time.Now().Add(5 * time.Minute); until.Before(min) {
				until = min
			}
		}
		s.applyCooldown(ctx, acct, cooldownRateLimit, until, reason)
		return AccountFailureOutcome{Action: ActionCooldown, Until: until, Reason: reason}

	case "timeout":
		threshold := 3
		cooldown := 5 * time.Minute
		if mode == PoolModeConservative {
			threshold, cooldown = 2, 30*time.Minute
		}
		if s.bumpFailureWindow(ctx, acct.ID, "timeout") >= threshold {
			until := time.Now().Add(cooldown)
			s.applyCooldown(ctx, acct, cooldownOther, until, reason)
			return AccountFailureOutcome{Action: ActionCooldown, Until: until, Reason: reason}
		}
		s.noteAccountFailure(ctx, acct, reason, false)
		return AccountFailureOutcome{Action: ActionNone}

	case "provider_failure":
		if mode == PoolModeLenient {
			s.noteAccountFailure(ctx, acct, reason, false)
			return AccountFailureOutcome{Action: ActionNone, RetrySame: true, Reason: reason}
		}
		cooldown := time.Minute
		if mode == PoolModeConservative {
			cooldown = 5 * time.Minute
		}
		until := time.Now().Add(cooldown)
		s.applyCooldown(ctx, acct, cooldownOther, until, reason)
		return AccountFailureOutcome{Action: ActionCooldown, Until: until, Reason: reason}

	case "auth":
		outcome := s.handleAuthFailure(ctx, acct, f, mode, reason)
		s.auditAccountHealth(acct, f, mode, outcome)
		return outcome
	}

	s.noteAccountFailure(ctx, acct, reason, false)
	return AccountFailureOutcome{Action: ActionNone}
}

// handleAuthFailure covers the 4xx family: 400 credit/KYC, 401, 402, 403, 404.
func (s *AccountPoolSvc) handleAuthFailure(ctx context.Context, acct *ent.UpstreamAccount, f AccountFailure, mode PoolMode, reason string) AccountFailureOutcome {
	body := strings.ToLower(f.Body)

	// Terminal account states: the credential or the organization is gone.
	if looksLikeTerminalAuthFailure(body, f.Status) {
		if mode == PoolModeLenient {
			// Lenient pools tolerate ambiguous terminal-looking errors: the
			// message need not be about this key. Park it, never disable.
			until := time.Now().Add(time.Hour)
			s.applyCooldown(ctx, acct, cooldownOther, until, reason)
			return AccountFailureOutcome{Action: ActionCooldown, Until: until, Reason: reason}
		}
		return s.disableAccount(ctx, acct, reason)
	}

	switch f.Status {
	case http.StatusUnauthorized:
		if mode == PoolModeLenient {
			until := time.Now().Add(time.Hour)
			s.applyCooldown(ctx, acct, cooldownOther, until, reason)
			return AccountFailureOutcome{Action: ActionCooldown, Until: until, Reason: reason}
		}
		return s.disableAccount(ctx, acct, reason)

	case http.StatusPaymentRequired:
		// Balance depleted: recoverable once topped up.
		d := time.Hour
		if mode == PoolModeConservative {
			d = 6 * time.Hour
		}
		until := time.Now().Add(d)
		s.applyCooldown(ctx, acct, cooldownOther, until, reason)
		return AccountFailureOutcome{Action: ActionCooldown, Until: until, Reason: reason}

	case http.StatusForbidden:
		threshold := 3
		if mode == PoolModeConservative {
			threshold = 2
		}
		if s.bumpFailureWindow(ctx, acct.ID, "auth_403") >= threshold {
			return s.disableAccount(ctx, acct, reason)
		}
		d := 15 * time.Minute
		until := time.Now().Add(d)
		s.applyCooldown(ctx, acct, cooldownOther, until, reason)
		return AccountFailureOutcome{Action: ActionCooldown, Until: until, Reason: reason}

	case http.StatusNotFound:
		// Model not found on this upstream: park the model, not the account.
		if f.Model == "" {
			s.noteAccountFailure(ctx, acct, reason, false)
			return AccountFailureOutcome{Action: ActionNone}
		}
		d := modelCooldownDefault
		if mode == PoolModeConservative {
			d = 2 * modelCooldownDefault
		}
		if mode == PoolModeLenient {
			return AccountFailureOutcome{Action: ActionNone, RetrySame: true, Reason: reason}
		}
		s.setModelCooldown(ctx, acct.ID, f.Model, time.Now().Add(d), reason)
		return AccountFailureOutcome{Action: ActionCooldown, Until: time.Now().Add(d), Reason: reason}
	}

	// Remaining auth-class codes (408 and friends): short cooldown.
	until := time.Now().Add(time.Minute)
	s.applyCooldown(ctx, acct, cooldownOther, until, reason)
	return AccountFailureOutcome{Action: ActionCooldown, Until: until, Reason: reason}
}

// isTerminalAccountFailure reports failures that waiting cannot fix: they
// disable the account outside lenient mode.
func isTerminalAccountFailure(f AccountFailure) bool {
	if looksLikeTerminalAuthFailure(strings.ToLower(f.Body), f.Status) {
		return true
	}
	return f.Status == http.StatusUnauthorized
}

// looksLikeTerminalAuthFailure spots the upstream messages that mean "this
// account is finished" (not "try again later").
func looksLikeTerminalAuthFailure(lowerBody string, status int) bool {
	if lowerBody == "" {
		return false
	}
	for _, marker := range []string{
		"credit balance is too low",
		"insufficient balance",
		"organization has been disabled",
		"organization_disabled",
		"identity verification is required",
		"deactivated_workspace",
		"account has been deactivated",
		"token_invalidated",
		"token_revoked",
		"invalid_grant",
		"api key has been revoked",
		"key has been disabled",
	} {
		if strings.Contains(lowerBody, marker) {
			return true
		}
	}
	return false
}

// rateLimitReset reads the reset time a rate-limited upstream advertises:
// explicit reset headers first (they are the only exact signal), then the body,
// then the caller's Retry-After, and finally a short fallback so the account is
// re-probed soon.
func (s *AccountPoolSvc) rateLimitReset(f AccountFailure) time.Time {
	now := time.Now()
	until := now.Add(accountRateFallback)

	if h := f.Headers; h != nil {
		// Aggregate reset (Anthropic unified windows): unix seconds.
		for _, key := range []string{
			"Anthropic-Ratelimit-Unified-Reset",
			"Anthropic-Ratelimit-Unified-7d-Reset",
			"Anthropic-Ratelimit-Unified-5h-Reset",
		} {
			if t, ok := parseUnixOrMillis(h.Get(key)); ok {
				return clampAccountUntil(t, now)
			}
		}
		// Codex windows.
		for _, key := range []string{
			"X-Codex-Rate-Limit-Reset",
			"X-Codex-Primary-Reset-At",
			"X-Codex-Secondary-Reset-At",
		} {
			if t, ok := parseUnixOrMillis(h.Get(key)); ok {
				return clampAccountUntil(t, now)
			}
		}
		if ra, ok := parseRetryAfterHeader(h.Get("Retry-After")); ok {
			return clampAccountUntil(now.Add(ra), now)
		}
	}

	// Body: {"resets_at": <unix>} / {"resets_in_seconds": n} /
	// {"error":{"resets_at":...}} — vendors differ, so scan the raw body.
	if secs, ok := findResetInBody(f.Body); ok {
		if secs > 1e9 { // already an absolute timestamp
			return clampAccountUntil(time.Unix(int64(secs), 0), now)
		}
		return clampAccountUntil(now.Add(time.Duration(secs)*time.Second), now)
	}
	return until
}

func clampAccountUntil(t, now time.Time) time.Time {
	if t.Before(now.Add(accountCooldownFloor)) {
		return now.Add(accountCooldownFloor)
	}
	if t.After(now.Add(accountCooldownCeil)) {
		return now.Add(accountCooldownCeil)
	}
	return t
}

func parseUnixOrMillis(raw string) (time.Time, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}, false
	}
	if v, err := strconv.ParseFloat(raw, 64); err == nil {
		return unixLoose(v), true
	}
	// HTTP date form is also legal in these headers.
	if t, err := http.ParseTime(raw); err == nil {
		return t, true
	}
	return time.Time{}, false
}

func unixLoose(v float64) time.Time {
	if v > 1e12 { // milliseconds
		return time.UnixMilli(int64(v))
	}
	return time.Unix(int64(v), 0)
}

func parseRetryAfterHeader(raw string) (time.Duration, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, false
	}
	if secs, err := strconv.Atoi(raw); err == nil {
		if secs < 0 {
			return 0, false
		}
		return time.Duration(secs) * time.Second, true
	}
	if t, err := http.ParseTime(raw); err == nil {
		d := time.Until(t)
		if d < 0 {
			return 0, false
		}
		return d, true
	}
	return 0, false
}

// findResetInBody scans an upstream error body for a reset hint. Bodies are
// small and vendor-specific, so a shape-tolerant scan beats a strict decode.
func findResetInBody(body string) (float64, bool) {
	if len(body) == 0 {
		return 0, false
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(body), &payload); err == nil {
		if v, ok := pluckReset(payload); ok {
			return v, true
		}
	}
	// Anthropic-style prose: "resets in 3h 20m" is not machine readable; only
	// accept explicit numbers here.
	return 0, false
}

func pluckReset(m map[string]any) (float64, bool) {
	for _, key := range []string{"resets_at", "reset_at", "resets_in_seconds", "reset_after_seconds", "retry_after_seconds"} {
		if v, ok := numericValue(m[key]); ok {
			return v, true
		}
	}
	for _, nested := range []string{"error", "detail", "rate_limit", "quota", "usage"} {
		if child, ok := m[nested].(map[string]any); ok {
			if v, ok := pluckReset(child); ok {
				return v, true
			}
		}
	}
	return 0, false
}

func numericValue(v any) (float64, bool) {
	switch t := v.(type) {
	case float64:
		return t, true
	case json.Number:
		f, err := t.Float64()
		return f, err == nil
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(t), 64)
		return f, err == nil
	}
	return 0, false
}

// applyCooldown records a time-based cooldown: Redis for immediate effect
// across workers, the database columns for durability and the admin UI.
func (s *AccountPoolSvc) applyCooldown(ctx context.Context, acct *ent.UpstreamAccount, kind string, until time.Time, reason string) {
	ttl := time.Until(until)
	if ttl <= 0 {
		return
	}
	if s.hasAccountStore() {
		if err := s.MarkCooldown(ctx, acct.ID, reason, ttl); err != nil {
			logAccountHealth("account.cooldown_mark_failed", acct.ID, reason, err)
		}
	}
	s.persistHealthState(ctx, acct.ID, func(u *ent.UpstreamAccountUpdateOne) *ent.UpstreamAccountUpdateOne {
		switch kind {
		case cooldownRateLimit:
			return u.SetRateLimitResetAt(until)
		case cooldownOverload:
			return u.SetOverloadUntil(until)
		default:
			return u.SetNillableTempUnschedulableUntil(&until).SetTempUnschedulableReason(reason)
		}
	}, reason, false)
}

// disableAccount takes an account out of rotation until an explicit recovery.
func (s *AccountPoolSvc) disableAccount(ctx context.Context, acct *ent.UpstreamAccount, reason string) AccountFailureOutcome {
	if s.hasAccountStore() {
		if err := s.MarkCooldown(ctx, acct.ID, reason, 0); err != nil {
			logAccountHealth("account.disable_mark_failed", acct.ID, reason, err)
		}
	}
	s.persistHealthState(ctx, acct.ID, func(u *ent.UpstreamAccountUpdateOne) *ent.UpstreamAccountUpdateOne {
		return u.SetStatus(entaccount.StatusError).SetSchedulable(false)
	}, reason, true)
	return AccountFailureOutcome{Action: ActionDisable, Reason: reason}
}

// auditAccountHealth keeps a durable record of the auth-class verdicts: every
// disable and every cooldown after a 401/402/403/404. The account row only
// keeps the latest error class, which the next event overwrites, so without
// this nothing tells afterwards why an account was taken out of rotation. The
// rows land in admin_audit_logs under the system actor (uuid.Nil), with the
// same "id=<account>" target the admin routes use.
func (s *AccountPoolSvc) auditAccountHealth(acct *ent.UpstreamAccount, f AccountFailure, mode PoolMode, outcome AccountFailureOutcome) {
	if s == nil || s.d == nil || outcome.Action == ActionNone {
		return
	}
	action := "account.health.cooldown"
	if outcome.Action == ActionDisable {
		action = "account.health.disable"
	}
	payload := map[string]any{
		"account":   acct.Name,
		"pool_mode": string(mode),
		"class":     f.Class,
		"status":    f.Status,
		"reason":    outcome.Reason,
		"model":     f.Model,
		// Redacted: the body answered a user's request and can quote it.
		"error": SanitizeUpstreamError(f.Body),
	}
	if !outcome.Until.IsZero() {
		payload["until"] = outcome.Until
	}
	NewAuditSvc(s.d).RecordAsync(AuditEntry{
		ActorUserID: uuid.Nil,
		Action:      action,
		Target:      "id=" + acct.ID.String(),
		Payload:     payload,
	})
}

// noteAccountFailure records a real account failure: it stamps the class and
// grows the consecutive-failure counter without changing rotation on its own.
func (s *AccountPoolSvc) noteAccountFailure(ctx context.Context, acct *ent.UpstreamAccount, reason string, disabled bool) {
	s.persistHealthState(ctx, acct.ID, nil, reason, disabled)
}

// recordAccountEvent records that something happened to the account without
// counting it as an account fault.
func (s *AccountPoolSvc) recordAccountEvent(ctx context.Context, id uuid.UUID, reason string) {
	if s == nil || s.d == nil {
		return
	}
	if err := s.d.Ent.UpstreamAccount.UpdateOneID(id).
		SetLastErrorClass(reason).
		SetLastErrorAt(time.Now()).
		Exec(ctx); err != nil {
		logAccountHealth("account.health_event_write_failed", id, reason, err)
	}
}

func (s *AccountPoolSvc) persistHealthState(ctx context.Context, id uuid.UUID, mutate func(*ent.UpstreamAccountUpdateOne) *ent.UpstreamAccountUpdateOne, reason string, disabled bool) {
	if s == nil || s.d == nil {
		return
	}
	update := s.d.Ent.UpstreamAccount.UpdateOneID(id).
		SetLastErrorClass(reason).
		SetLastErrorAt(time.Now())
	if !disabled {
		update = update.AddConsecutiveFailures(1)
	}
	if mutate != nil {
		update = mutate(update)
	}
	if err := update.Exec(ctx); err != nil {
		logAccountHealth("account.health_state_write_failed", id, reason, err)
	}
}

// bumpFailureWindow counts failures of one kind inside a rolling window and
// returns the current count. Falls back to the DB counter when Redis is not
// available, so repeated 403s still disable an account.
func (s *AccountPoolSvc) bumpFailureWindow(ctx context.Context, id uuid.UUID, kind string) int {
	if s == nil || s.d == nil {
		// Without storage there is no window to count against: treat every
		// failure as the first one so the caller keeps its cooldown branch.
		return 1
	}
	if s.hasAccountStore() {
		key := fmt.Sprintf("acct:failwin:%d:%s", id, kind)
		res := s.d.Redis.Incr(ctx, key)
		if err := res.Err(); err == nil {
			if res.Val() == 1 {
				_ = s.d.Redis.Expire(ctx, key, failureWindowTTL)
			}
			return int(res.Val())
		}
	}
	acct, err := s.d.Ent.UpstreamAccount.Get(ctx, id)
	if err != nil {
		return 0
	}
	return acct.ConsecutiveFailures + 1
}

// clearFailureWindow drops the rolling counters for an account (called on
// recovery and on any successful request).
func (s *AccountPoolSvc) clearFailureWindow(ctx context.Context, id uuid.UUID) {
	if !s.hasAccountStore() {
		return
	}
	for _, kind := range []string{"timeout", "auth_403"} {
		_ = s.d.Redis.Del(ctx, fmt.Sprintf("acct:failwin:%d:%s", id, kind))
	}
}

// setModelCooldown parks one model on one account without touching the rest.
func (s *AccountPoolSvc) setModelCooldown(ctx context.Context, id uuid.UUID, model string, until time.Time, reason string) {
	if s == nil || s.d == nil {
		return
	}
	acct, err := s.d.Ent.UpstreamAccount.Get(ctx, id)
	if err != nil {
		return
	}
	cooldowns := map[string]any{}
	if len(acct.ModelCooldowns) > 0 {
		if raw, err := json.Marshal(acct.ModelCooldowns); err == nil {
			_ = json.Unmarshal(raw, &cooldowns)
		}
	}
	cooldowns[model] = map[string]any{
		"until_ms": until.UnixMilli(),
		"reason":   reason,
	}
	if err := s.d.Ent.UpstreamAccount.UpdateOneID(id).SetModelCooldowns(cooldowns).Exec(ctx); err != nil {
		logAccountHealth("account.model_cooldown_write_failed", id, reason, err)
	}
}

// ClearModelCooldown drops every model-scoped cooldown of an account.
func (s *AccountPoolSvc) ClearModelCooldown(ctx context.Context, id uuid.UUID) error {
	return s.d.Ent.UpstreamAccount.UpdateOneID(id).
		SetModelCooldowns(map[string]any{}).
		Exec(ctx)
}

// ModelCooled reports whether one model is parked on this account right now.
func ModelCooled(acct *ent.UpstreamAccount, model string, now time.Time) bool {
	if acct == nil || model == "" || len(acct.ModelCooldowns) == 0 {
		return false
	}
	raw, ok := acct.ModelCooldowns[model]
	if !ok {
		return false
	}
	entry, ok := raw.(map[string]any)
	if !ok {
		return false
	}
	untilMS, ok := numericValue(entry["until_ms"])
	if !ok {
		return false
	}
	return now.Before(time.UnixMilli(int64(untilMS)))
}

// AccountSchedulable reports whether the account may serve a request right now.
// Time-based cooldowns lift by themselves; a disable (status=error, or the
// schedulable switch turned off) needs an explicit recovery.
func AccountSchedulable(acct *ent.UpstreamAccount, model string, now time.Time) bool {
	if acct == nil || acct.DeletedAt != nil {
		return false
	}
	if acct.Status != entaccount.StatusActive || !acct.Schedulable {
		return false
	}
	if acct.RateLimitResetAt != nil && now.Before(*acct.RateLimitResetAt) {
		return false
	}
	if acct.OverloadUntil != nil && now.Before(*acct.OverloadUntil) {
		return false
	}
	if acct.TempUnschedulableUntil != nil && now.Before(*acct.TempUnschedulableUntil) {
		return false
	}
	if ModelCooled(acct, model, now) {
		return false
	}
	return true
}

// AccountAssignable reports whether an account can remain the session's
// channel. A disable is serious. A time-based cooldown is not:
// the session keeps the account, and this request may still try another
// account in the same group.
func AccountAssignable(acct *ent.UpstreamAccount, now time.Time) bool {
	if acct == nil || acct.DeletedAt != nil {
		return false
	}
	return acct.Status == entaccount.StatusActive && acct.Schedulable
}

// RecoverAccountState clears every runtime health signal so the account is
// eligible again: used by the admin "clear error" action and after a
// successful account test.
func (s *AccountPoolSvc) RecoverAccountState(ctx context.Context, id uuid.UUID) error {
	if _, err := s.d.Ent.UpstreamAccount.UpdateOneID(id).
		ClearRateLimitResetAt().
		ClearOverloadUntil().
		ClearTempUnschedulableUntil().
		SetTempUnschedulableReason("").
		SetModelCooldowns(map[string]any{}).
		SetConsecutiveFailures(0).
		SetLastErrorClass("").
		SetSchedulable(true).
		SetStatus(entaccount.StatusActive).
		Save(ctx); err != nil {
		return err
	}
	s.clearFailureWindow(ctx, id)
	return s.ClearCooldown(ctx, id)
}

// RecordAccountSuccess clears the transient counters after a served request.
func (s *AccountPoolSvc) RecordAccountSuccess(ctx context.Context, acct *ent.UpstreamAccount) {
	if acct == nil {
		return
	}
	s.clearFailureWindow(ctx, acct.ID)
	if acct.ConsecutiveFailures > 0 {
		if err := s.d.Ent.UpstreamAccount.UpdateOneID(acct.ID).
			SetConsecutiveFailures(0).
			SetLastErrorClass("").
			Exec(ctx); err != nil {
			logAccountHealth("account.success_reset_failed", acct.ID, "", err)
		}
	}
	// A recovered account also drops time-based cooldowns that were only ever
	// optimistic guesses (a successful call is proof the account works).
	if acct.RateLimitResetAt != nil || acct.OverloadUntil != nil || acct.TempUnschedulableUntil != nil {
		if err := s.d.Ent.UpstreamAccount.UpdateOneID(acct.ID).
			ClearRateLimitResetAt().
			ClearOverloadUntil().
			ClearTempUnschedulableUntil().
			SetTempUnschedulableReason("").
			Exec(ctx); err != nil {
			logAccountHealth("account.cooldown_clear_failed", acct.ID, "", err)
		}
		if s.hasAccountStore() {
			_ = s.ClearCooldown(ctx, acct.ID)
		}
	}
}

// SetSchedulable is the admin pause switch (status stays untouched so the
// operator can tell "parked" from "broken").
func (s *AccountPoolSvc) SetSchedulable(ctx context.Context, id uuid.UUID, schedulable bool) error {
	update := s.d.Ent.UpstreamAccount.UpdateOneID(id).SetSchedulable(schedulable)
	if schedulable {
		// Unparking also clears the timers, otherwise the account would stay
		// invisible until they expire and the UI would look broken.
		update = update.ClearRateLimitResetAt().ClearOverloadUntil().ClearTempUnschedulableUntil()
	}
	return update.Exec(ctx)
}

func (s *AccountPoolSvc) hasAccountStore() bool {
	return s != nil && s.d != nil && s.d.Redis != nil
}

// cooldown kinds (kept as strings so the DB column chosen stays obvious).
const (
	cooldownRateLimit = "rate_limit"
	cooldownOverload  = "overload"
	cooldownOther     = "cooldown"
)

func logAccountHealth(msg string, id uuid.UUID, reason string, err error) {
	if err == nil {
		return
	}
	slog.Debug(msg, "account_id", id.String(), "reason", reason, "err", err.Error())
}
