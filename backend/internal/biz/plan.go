package biz

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/shopspring/decimal"

	"github.com/jucode/jucode/internal/data"
	"github.com/jucode/jucode/internal/data/ent"
	entplan "github.com/jucode/jucode/internal/data/ent/plan"
	entplanquotasnapshot "github.com/jucode/jucode/internal/data/ent/planquotasnapshot"
	entuserplan "github.com/jucode/jucode/internal/data/ent/userplan"
)

// quotaLuaSrc is the canonical source of the 3-window quota Lua. The
// duplicate copy at internal/data/redis/quota.lua exists for ops review
// / external tooling and MUST stay in sync.
// Unconditional 3-window increment. Overdraft is allowed by design: a
// single request can push `used` past `quota`, after which BillingSvc.PreCheck
// (which rejects on used >= quota) will block all subsequent requests. This
// avoids the old bug where Charge would refuse to write when cost would
// exceed quota, causing `used` to stay frozen and PreCheck to keep letting
// requests through.
// Cost arrives as a fixed-point int64 scaled by 1e8 (biz.moneyToFixed) and is
// accumulated with INCRBY. It used to be INCRBYFLOAT, which made these the one
// genuinely float-accumulated money figure in the system — and they gate plan
// quota, so the drift decided whether a user could keep making requests.
//
// Keys written before the switch hold a decimal string, so each one is
// rewritten in place on first touch. Versioning the key namespace instead
// would have been simpler but would zero every live window, handing every
// user on a plan a fresh 5h/weekly/monthly allowance.
const quotaLuaSrc = `
local function bump(key, cost, ttl)
  local cur = redis.call('GET', key)
  if cur and string.find(cur, '[.eE]') then
    local n = tonumber(cur) or 0
    local scaled = n * 1e8
    if scaled >= 0 then scaled = math.floor(scaled + 0.5) else scaled = -math.floor(-scaled + 0.5) end
    redis.call('SET', key, string.format('%d', scaled))
  end
  local v = redis.call('INCRBY', key, cost)
  redis.call('EXPIRE', key, ttl)
  return v
end

local cost = ARGV[1]
return {
  bump(KEYS[1], cost, tonumber(ARGV[5])),
  bump(KEYS[2], cost, tonumber(ARGV[6])),
  bump(KEYS[3], cost, tonumber(ARGV[7])),
}
`

var quotaLua = redis.NewScript(quotaLuaSrc)

// quotaOnceLuaSrc is the crash-safe variant used by reservation settlement.
// The idempotency marker and all three increments live in one Redis script:
// retrying after a process/DB failure observes the marker and returns the
// current counters without consuming the plan a second time.
const quotaOnceLuaSrc = `
local function current(key)
  local cur = redis.call('GET', key)
  if not cur then return '0' end
  return cur
end

if redis.call('EXISTS', KEYS[4]) == 1 then
  return {current(KEYS[1]), current(KEYS[2]), current(KEYS[3]), 0}
end

local function bump(key, cost, ttl)
  local cur = redis.call('GET', key)
  if cur and string.find(cur, '[.eE]') then
    local n = tonumber(cur) or 0
    local scaled = n * 1e8
    if scaled >= 0 then scaled = math.floor(scaled + 0.5) else scaled = -math.floor(-scaled + 0.5) end
    redis.call('SET', key, string.format('%d', scaled))
  end
  local v = redis.call('INCRBY', key, cost)
  redis.call('EXPIRE', key, ttl)
  return v
end

local cost = ARGV[1]
local result = {
  bump(KEYS[1], cost, tonumber(ARGV[2])),
  bump(KEYS[2], cost, tonumber(ARGV[3])),
  bump(KEYS[3], cost, tonumber(ARGV[4])),
}
redis.call('SET', KEYS[4], '1', 'EX', tonumber(ARGV[5]))
return {result[1], result[2], result[3], 1}
`

var quotaOnceLua = redis.NewScript(quotaOnceLuaSrc)

var (
	ErrPlanNotFound       = errors.New("plan not found")
	ErrNoActivePlan       = errors.New("user has no active plan")
	ErrPlanNotAllowed     = errors.New("active plan does not allow this model group")
	ErrPlanQuotaExhausted = errors.New("plan quota exceeded")
)

type PlanSvc struct {
	d        *data.Data
	settings *SettingsSvc // kept for symmetry with other services; quota math no longer needs the exchange rate (points = CNY 1:1)
}

func NewPlanSvc(d *data.Data, settings *SettingsSvc) *PlanSvc {
	return &PlanSvc{d: d, settings: settings}
}

// --- Plan CRUD (admin) ---

type CreatePlanInput struct {
	Name                string
	Price               decimal.Decimal
	Currency            string
	Type                string
	PeriodDays          int
	Quota5h             decimal.Decimal
	QuotaWeekly         decimal.Decimal
	QuotaMonthly        decimal.Decimal
	AllowedGroupIDs     []uuid.UUID
	Description         string
	ConcurrencyLimit    int
	RpmLimit            int
	TpmLimit            int
	MonthlySpendLimit   decimal.Decimal
	InitialBalance      decimal.Decimal
	AutoReloadEnabled   bool
	AutoReloadThreshold decimal.Decimal
	AutoReloadAmount    decimal.Decimal
}

func (s *PlanSvc) CreatePlan(ctx context.Context, in CreatePlanInput) (*ent.Plan, error) {
	q := s.d.Ent.Plan.Create().
		SetName(in.Name).
		SetPrice(in.Price).
		SetPeriodDays(in.PeriodDays).
		SetQuota5h(in.Quota5h).
		SetQuotaWeekly(in.QuotaWeekly).
		SetQuotaMonthly(in.QuotaMonthly).
		SetAllowedGroupIds(in.AllowedGroupIDs).
		SetDescription(in.Description).
		SetConcurrencyLimit(in.ConcurrencyLimit).
		SetRpmLimit(in.RpmLimit).
		SetTpmLimit(in.TpmLimit).
		SetMonthlySpendLimit(in.MonthlySpendLimit).
		SetInitialBalance(in.InitialBalance).
		SetAutoReloadEnabled(in.AutoReloadEnabled).
		SetAutoReloadThreshold(in.AutoReloadThreshold).
		SetAutoReloadAmount(in.AutoReloadAmount)
	if in.Currency != "" {
		q = q.SetCurrency(in.Currency)
	}
	if in.Type != "" {
		q = q.SetType(entplan.Type(in.Type))
	}
	return q.Save(ctx)
}

func (s *PlanSvc) ListPlans(ctx context.Context, onlyActive bool) ([]*ent.Plan, error) {
	q := s.d.Ent.Plan.Query()
	if onlyActive {
		q = q.Where(entplan.StatusEQ(entplan.StatusActive))
	}
	return q.Order(ent.Asc(entplan.FieldName)).All(ctx)
}

type UpdatePlanInput struct {
	Name                *string
	Price               *decimal.Decimal
	Currency            *string
	Type                *string
	PeriodDays          *int
	Quota5h             *decimal.Decimal
	QuotaWeekly         *decimal.Decimal
	QuotaMonthly        *decimal.Decimal
	AllowedGroupIDs     *[]uuid.UUID
	Description         *string
	Status              *string
	ConcurrencyLimit    *int
	RpmLimit            *int
	TpmLimit            *int
	MonthlySpendLimit   *decimal.Decimal
	InitialBalance      *decimal.Decimal
	AutoReloadEnabled   *bool
	AutoReloadThreshold *decimal.Decimal
	AutoReloadAmount    *decimal.Decimal
}

func (s *PlanSvc) UpdatePlan(ctx context.Context, id uuid.UUID, in UpdatePlanInput) (*ent.Plan, error) {
	q := s.d.Ent.Plan.UpdateOneID(id)
	if in.Name != nil {
		q = q.SetName(*in.Name)
	}
	if in.Price != nil {
		q = q.SetPrice(*in.Price)
	}
	if in.Currency != nil {
		q = q.SetCurrency(*in.Currency)
	}
	if in.Type != nil {
		q = q.SetType(entplan.Type(*in.Type))
	}
	if in.PeriodDays != nil {
		q = q.SetPeriodDays(*in.PeriodDays)
	}
	if in.Quota5h != nil {
		q = q.SetQuota5h(*in.Quota5h)
	}
	if in.QuotaWeekly != nil {
		q = q.SetQuotaWeekly(*in.QuotaWeekly)
	}
	if in.QuotaMonthly != nil {
		q = q.SetQuotaMonthly(*in.QuotaMonthly)
	}
	if in.AllowedGroupIDs != nil {
		q = q.SetAllowedGroupIds(*in.AllowedGroupIDs)
	}
	if in.Description != nil {
		q = q.SetDescription(*in.Description)
	}
	if in.Status != nil {
		q = q.SetStatus(entplan.Status(*in.Status))
	}
	if in.ConcurrencyLimit != nil {
		q = q.SetConcurrencyLimit(*in.ConcurrencyLimit)
	}
	if in.RpmLimit != nil {
		q = q.SetRpmLimit(*in.RpmLimit)
	}
	if in.TpmLimit != nil {
		q = q.SetTpmLimit(*in.TpmLimit)
	}
	if in.MonthlySpendLimit != nil {
		q = q.SetMonthlySpendLimit(*in.MonthlySpendLimit)
	}
	if in.InitialBalance != nil {
		q = q.SetInitialBalance(*in.InitialBalance)
	}
	if in.AutoReloadEnabled != nil {
		q = q.SetAutoReloadEnabled(*in.AutoReloadEnabled)
	}
	if in.AutoReloadThreshold != nil {
		q = q.SetAutoReloadThreshold(*in.AutoReloadThreshold)
	}
	if in.AutoReloadAmount != nil {
		q = q.SetAutoReloadAmount(*in.AutoReloadAmount)
	}
	p, err := q.Save(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			return nil, ErrPlanNotFound
		}
		return nil, err
	}
	return p, nil
}

// PayAsYouGoUsageThisMonth sums plan_balance spend in the plan's current
// 30-day window, which is counted from start_at like the quota windows.
func (s *PlanSvc) PayAsYouGoUsageThisMonth(ctx context.Context, up *ent.UserPlan) (decimal.Decimal, error) {
	start := QuotaWindowsAt(up, time.Now())[2].Start
	var total decimal.Decimal
	err := s.d.DB().QueryRowContext(ctx, `
		SELECT COALESCE(SUM(cost_final), 0)
		FROM usage_logs
		WHERE user_plan_id = $1
		  AND billing_source = 'plan_balance'
		  AND status = 'ok'
		  AND created_at >= $2
	`, up.ID, start).Scan(&total)
	return total, err
}

func (s *PlanSvc) PayAsYouGoUsageViewThisMonth(ctx context.Context, up *ent.UserPlan, plan *ent.Plan) (decimal.Decimal, error) {
	// Usage logs and plan limits are both denominated in points
	// (1 point = 1 CNY) — return the summed usage as-is, no conversion.
	return s.PayAsYouGoUsageThisMonth(ctx, up)
}

// HardDeletePlanResult reports what was purged so callers (admin UI) can
// confirm impact after the fact.
type HardDeletePlanResult struct {
	UserPlansDeleted      int
	QuotaSnapshotsDeleted int
}

// HardDeletePlan removes the plan row AND every UserPlan referencing it,
// inside one transaction. NOT idempotent in the same sense as archiving a plan —
// this is the "I really mean to evaporate this plan" path.
//
// Order matters because the FKs are RESTRICT (NO ACTION):
//  1. plan_quota_snapshots  → references user_plans.id
//  2. user_plans            → references plans.id
//  3. plans                 → finally
//
// usage_logs / balance_txes / redeem_records keep a bare user_plan_id
// column without an FK to user_plans, so historical audit rows continue
// to mention the now-deleted user_plan id. That's intentional: tearing
// up the ledger to "clean" a delete would break compliance.
//
// Redis quota counters keyed by user_plan_id are not actively purged —
// they expire on their own TTL and the now-orphaned keys are harmless.
func (s *PlanSvc) HardDeletePlan(ctx context.Context, id uuid.UUID) (*HardDeletePlanResult, error) {
	tx, err := s.d.Ent.Tx(ctx)
	if err != nil {
		return nil, err
	}
	// Pre-flight: the plan must exist. UpdateOneID would silently treat
	// a missing row as no-op; we want a clear ErrPlanNotFound for the
	// caller.
	if _, err := tx.Plan.Get(ctx, id); err != nil {
		_ = tx.Rollback()
		if ent.IsNotFound(err) {
			return nil, ErrPlanNotFound
		}
		return nil, err
	}

	upIDs, err := tx.UserPlan.Query().
		Where(entuserplan.PlanIDEQ(id)).
		IDs(ctx)
	if err != nil {
		_ = tx.Rollback()
		return nil, err
	}

	snapDeleted := 0
	if len(upIDs) > 0 {
		n, err := tx.PlanQuotaSnapshot.Delete().
			Where(entplanquotasnapshot.UserPlanIDIn(upIDs...)).
			Exec(ctx)
		if err != nil {
			_ = tx.Rollback()
			return nil, fmt.Errorf("delete quota snapshots: %w", err)
		}
		snapDeleted = n
	}

	upDeleted, err := tx.UserPlan.Delete().
		Where(entuserplan.PlanIDEQ(id)).
		Exec(ctx)
	if err != nil {
		_ = tx.Rollback()
		return nil, fmt.Errorf("delete user_plans: %w", err)
	}

	if err := tx.Plan.DeleteOneID(id).Exec(ctx); err != nil {
		_ = tx.Rollback()
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &HardDeletePlanResult{UserPlansDeleted: upDeleted, QuotaSnapshotsDeleted: snapDeleted}, nil
}

func (s *PlanSvc) DeletePlan(ctx context.Context, id uuid.UUID) error {
	// archive rather than hard-delete so existing UserPlans still resolve.
	_, err := s.d.Ent.Plan.UpdateOneID(id).SetStatus(entplan.StatusArchived).Save(ctx)
	if err != nil && ent.IsNotFound(err) {
		return ErrPlanNotFound
	}
	return err
}

// ActiveUserPlan returns the user's current plan for plan-only billing, or
// nil + ErrNoActivePlan. See activeUserPlan for how it chooses.
func (s *PlanSvc) ActiveUserPlan(ctx context.Context, userID uuid.UUID) (*ent.UserPlan, *ent.Plan, error) {
	return activeUserPlan(ctx, s.d.Ent, userID)
}

// activeUserPlan picks among the user's non-expired active plans. An active
// quota_subscription always wins over a pay_as_you_go credit pack, newest
// first within each type. A credit pack only grants balance, so it must not
// displace a subscription: picking purely by start_at let a subscriber who
// later bought a pack lose the subscription's quota, with plan-group traffic
// billed from the wallet instead.
func activeUserPlan(ctx context.Context, client *ent.Client, userID uuid.UUID) (*ent.UserPlan, *ent.Plan, error) {
	ups, err := client.UserPlan.Query().
		Where(
			entuserplan.UserIDEQ(userID),
			entuserplan.StatusEQ(entuserplan.StatusActive),
			entuserplan.ExpireAtGT(time.Now()),
		).
		Order(ent.Desc(entuserplan.FieldStartAt)).
		All(ctx)
	if err != nil {
		return nil, nil, err
	}
	if len(ups) == 0 {
		return nil, nil, ErrNoActivePlan
	}
	ids := make([]uuid.UUID, 0, len(ups))
	for _, up := range ups {
		ids = append(ids, up.PlanID)
	}
	plans, err := client.Plan.Query().Where(entplan.IDIn(ids...)).All(ctx)
	if err != nil {
		return nil, nil, err
	}
	byID := make(map[uuid.UUID]*ent.Plan, len(plans))
	for _, p := range plans {
		byID[p.ID] = p
	}
	var fallback *ent.UserPlan
	for _, up := range ups {
		p := byID[up.PlanID]
		if p == nil {
			continue
		}
		if IsSubscriptionPlan(p) {
			return up, p, nil
		}
		if fallback == nil {
			fallback = up
		}
	}
	if fallback == nil {
		return nil, nil, ErrNoActivePlan
	}
	return fallback, byID[fallback.PlanID], nil
}

// IsSubscriptionPlan reports whether plan is a real subscription with its
// own quota windows and plan-only groups. A pay_as_you_go "额度套餐" only
// grants balance and is the same thing as the wallet, so it never counts.
func IsSubscriptionPlan(plan *ent.Plan) bool {
	return plan != nil && plan.Type == entplan.TypeQuotaSubscription
}

// ConsumeQuota runs the 3-window Lua atomically.
// cost must be the FINAL (post rate-multiplier) decimal amount in points
// (1 point = 1 CNY). Quota windows and plan.Quota* are points too, so the
// Lua decrements/compares with no currency conversion.
func (s *PlanSvc) ConsumeQuota(ctx context.Context, up *ent.UserPlan, plan *ent.Plan, cost decimal.Decimal) (UsageView, error) {
	costFixed, ok := moneyToFixed(cost)
	if !ok {
		return UsageView{}, fmt.Errorf("plan quota: cost %s does not fit the fixed-point counter", cost)
	}
	keys := quotaKeys(up, time.Now())
	args := []any{
		costFixed,
		plan.Quota5h.String(),
		plan.QuotaWeekly.String(),
		plan.QuotaMonthly.String(),
		int(quotaWindow5h.Seconds()), // bucket TTL — a key never outlives its window
		int(quotaWindowWeekly.Seconds()),
		int(quotaWindowMonthly.Seconds()),
	}
	res, err := quotaLua.Run(ctx, s.d.Redis, keys, args...).Result()
	if err != nil {
		return UsageView{}, err
	}
	arr, ok := res.([]any)
	if !ok || len(arr) != 3 {
		return UsageView{}, fmt.Errorf("unexpected lua reply: %v", res)
	}
	// INCRBY replies are Lua integers, which go-redis surfaces as int64.
	parse := func(v any) decimal.Decimal {
		n, _ := v.(int64)
		return moneyFromFixed(n)
	}
	return UsageView{
		Used5h:      parse(arr[0]),
		UsedWeekly:  parse(arr[1]),
		UsedMonthly: parse(arr[2]),
	}, nil
}

// ConsumeQuotaOnce consumes all quota windows at most once for key. The marker
// outlives the longest quota window; callers may therefore safely retry after
// a crash between Redis consumption and their own durable completion flag.
func (s *PlanSvc) ConsumeQuotaOnce(ctx context.Context, up *ent.UserPlan, plan *ent.Plan, cost decimal.Decimal, key string) (UsageView, bool, error) {
	if key == "" {
		return UsageView{}, false, errors.New("plan quota: missing idempotency key")
	}
	costFixed, ok := moneyToFixed(cost)
	if !ok {
		return UsageView{}, false, fmt.Errorf("plan quota: cost %s does not fit the fixed-point counter", cost)
	}
	keys := append(quotaKeys(up, time.Now()), quotaOnceKey(up.ID, key))
	args := []any{
		costFixed,
		int(quotaWindow5h.Seconds()),
		int(quotaWindowWeekly.Seconds()),
		int(quotaWindowMonthly.Seconds()),
		int((36 * 24 * time.Hour).Seconds()),
	}
	res, err := quotaOnceLua.Run(ctx, s.d.Redis, keys, args...).Result()
	if err != nil {
		return UsageView{}, false, err
	}
	arr, ok := res.([]any)
	if !ok || len(arr) != 4 {
		return UsageView{}, false, fmt.Errorf("unexpected idempotent lua reply: %v", res)
	}
	parse := func(v any) decimal.Decimal {
		switch value := v.(type) {
		case int64:
			return moneyFromFixed(value)
		case string:
			if n, err := strconv.ParseInt(value, 10, 64); err == nil {
				return moneyFromFixed(n)
			}
			// A duplicate may encounter a legacy float counter that has not
			// been touched by this settlement. Preserve its visible value.
			if d, err := decimal.NewFromString(value); err == nil {
				return d
			}
		}
		return decimal.Zero
	}
	consumed, _ := arr[3].(int64)
	return UsageView{
		Used5h:      parse(arr[0]),
		UsedWeekly:  parse(arr[1]),
		UsedMonthly: parse(arr[2]),
	}, consumed == 1, nil
}

func quotaOnceKey(userPlanID uuid.UUID, key string) string {
	sum := sha256.Sum256([]byte(key))
	return fmt.Sprintf("planquota:once:%s:%x", userPlanID, sum[:])
}

// PeekQuota returns the current usage without incrementing.
func (s *PlanSvc) PeekQuota(ctx context.Context, up *ent.UserPlan) (UsageView, error) {
	keys := quotaKeys(up, time.Now())
	vals, err := s.d.Redis.MGet(ctx, keys...).Result()
	if err != nil {
		return UsageView{}, err
	}
	parse := func(v any) decimal.Decimal {
		s, ok := v.(string)
		if !ok || s == "" {
			return decimal.Zero
		}
		if n, err := strconv.ParseInt(s, 10, 64); err == nil {
			return moneyFromFixed(n)
		}
		// Legacy INCRBYFLOAT key that ConsumeQuota has not rewritten yet.
		// decimal parses the scientific notation those could carry directly,
		// so this no longer detours through float64.
		if d, err := decimal.NewFromString(s); err == nil {
			return d
		}
		return decimal.Zero
	}
	return UsageView{
		Used5h:      parse(vals[0]),
		UsedWeekly:  parse(vals[1]),
		UsedMonthly: parse(vals[2]),
	}, nil
}

type UsageView struct {
	Used5h      decimal.Decimal
	UsedWeekly  decimal.Decimal
	UsedMonthly decimal.Decimal
}

// Quota windows are counted from the user plan's start_at, not the calendar:
// a plan bought on the 28th gets a full 30-day month and a full week, and
// every window restarts exactly one window length after the previous one.
const (
	quotaWindow5h      = 5 * time.Hour
	quotaWindowWeekly  = 7 * 24 * time.Hour
	quotaWindowMonthly = 30 * 24 * time.Hour
)

// PlanQuotaWindow is one quota window of a user plan: its Redis counter and the
// span [Start, End) it covers.
type PlanQuotaWindow struct {
	Key        string
	Start, End time.Time
}

// QuotaWindowsAt returns the 5h, weekly and monthly windows of up that
// contain now, in that order. Each counts from its own anchor once an admin
// has brought that window's next reset forward, else from start_at. An
// anchored window carries the anchor in its key: a request priced before the
// reset may still settle into the old counter and refresh its TTL, and that
// must never be read as the new cycle's window.
func QuotaWindowsAt(up *ent.UserPlan, now time.Time) [3]PlanQuotaWindow {
	window := func(name string, size time.Duration, anchorAt *time.Time) PlanQuotaWindow {
		anchor, prefix := up.StartAt, "qta:"+up.ID.String()
		if anchorAt != nil {
			anchor = *anchorAt
			prefix += fmt.Sprintf(":a%d", anchor.UnixMilli())
		}
		var n int64
		if now.After(anchor) {
			n = int64(now.Sub(anchor) / size)
		}
		start := anchor.Add(time.Duration(n) * size)
		return PlanQuotaWindow{
			Key:   fmt.Sprintf("%s:%s:%d", prefix, name, n),
			Start: start,
			End:   start.Add(size),
		}
	}
	return [3]PlanQuotaWindow{
		window("5h", quotaWindow5h, up.QuotaAnchor5hAt),
		window("weekly", quotaWindowWeekly, up.QuotaAnchorWeeklyAt),
		window("monthly", quotaWindowMonthly, up.QuotaAnchorMonthlyAt),
	}
}

func quotaKeys(up *ent.UserPlan, now time.Time) []string {
	w := QuotaWindowsAt(up, now)
	return []string{w[0].Key, w[1].Key, w[2].Key}
}

// AllowsGroup reports whether the plan's allowed_group_ids contains the
// given group. Empty allowed_group_ids means "all groups allowed" so
// admins can ship a permissive plan quickly.
func PlanAllowsGroup(plan *ent.Plan, groupID uuid.UUID) bool {
	if len(plan.AllowedGroupIds) == 0 {
		return true
	}
	for _, g := range plan.AllowedGroupIds {
		if g == groupID {
			return true
		}
	}
	return false
}
