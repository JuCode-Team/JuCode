// billing_reserve.go — the Reserve / Settle / Release primitive for user
// and organization cash balances.
//
// Lifecycle (see docs/billing-reserve-settle.md for the full state machine):
//
//	Reserve  — freeze funds BEFORE any upstream output is produced. After
//	           INSERT, lock the account and decideMonetaryReserve: at or
//	           above the 0.25 CNY water-line and able to cover the estimate,
//	           freeze the estimate (concurrent holds allowed). Below the
//	           water-line, or when the estimate exceeds available, take the
//	           last-request path — freeze all remaining cash and allow only
//	           one in-flight llm:/ws: hold. Rejects when available <= 0,
//	           when last-request would collide with another llm:/ws: hold,
//	           or when an organization/member limit would be exceeded by the
//	           actual freeze. Idempotent on request_key (an existing hold
//	           may be smaller than the retry's estimate after last-request).
//	Settle   — after the response (or the observed part of it), charge the
//	           actual cost and write ONE deduct ledger row. Exact settle
//	           charges CostFinal in full; the balance may go negative.
//	           CapAtHold (reconstructed usage on cancel) charges
//	           min(final, hold) and does not create reconstructed debt.
//	Release  — nothing billable happened; return the full amount.
//
// Plan-quota groups (billing_source=plan_only with a non-pay_as_you_go plan)
// keep their existing semantics — "the last request may cross the line, the
// next is rejected" — so their Reservation carries no monetary hold and
// Settle routes to the Redis quota consumption unchanged.
package biz

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/jucode/jucode/internal/data/ent"
	entapikey "github.com/jucode/jucode/internal/data/ent/apikey"
	entgroup "github.com/jucode/jucode/internal/data/ent/modelgroup"
	entusagelog "github.com/jucode/jucode/internal/data/ent/usagelog"
	"github.com/jucode/jucode/internal/metrics"
)

var (
	// ErrTaskAlreadyBilled is returned by SettleReservation when the settle
	// was asked to atomically claim a generation task's billed flag and the
	// flag was already taken — the charge is rolled back, nothing moved.
	ErrTaskAlreadyBilled = errors.New("generation task already billed")
	// ErrHoldFinished means the idempotency key belongs to a hold that has
	// already been settled or released — a retried Reserve must not create
	// a second hold for the same request.
	ErrHoldFinished = errors.New("billing hold already settled or released")
	// ErrHoldIdentityConflict means a duplicate key names a hold owned by a
	// different account or priced differently. Returning that hold would let
	// one request spend another request's reservation.
	ErrHoldIdentityConflict = errors.New("billing hold idempotency identity conflict")
)

// Reservation scopes. plan_quota reservations carry no frozen money.
const (
	reserveScopeUser         = "user"
	reserveScopeOrganization = "organization"
	reserveScopePlanQuota    = "plan_quota"
)

// Hold TTL defaults per hot path. A hold that outlives its TTL is returned
// to the balance by the reaper (crash safety); an in-flight request whose
// hold was reaped still settles via the released-hold fallback (direct
// conditional charge), so a generous TTL only delays crash recovery.
const (
	ReserveTTLRequest = 2 * time.Hour
	ReserveTTLWSTurn  = 1 * time.Hour
	ReserveTTLVideo   = 24 * time.Hour
)

// LowBalanceThreshold is the cash water-line (CNY). At or above this and
// able to cover the estimate, Reserve behaves as today (full estimate,
// concurrent). Below it, or when the estimate exceeds available, Reserve
// takes the last-request path: freeze all remaining cash and allow only
// one in-flight llm:/ws: hold.
var LowBalanceThreshold = decimal.RequireFromString("0.25")

// decideMonetaryReserve returns the amount to freeze from available cash.
// otherInteractive is the count of other held llm:/ws: reservations on the
// same account (this hold excluded).
func decideMonetaryReserve(available, estimate decimal.Decimal, otherInteractive int) (decimal.Decimal, error) {
	if !available.IsPositive() {
		return decimal.Zero, ErrInsufficientBalance
	}
	last := available.LessThan(estimate) || available.LessThan(LowBalanceThreshold)
	if last {
		if otherInteractive > 0 {
			return decimal.Zero, ErrInsufficientBalance
		}
		return available, nil
	}
	return estimate, nil
}

// Reservation is the in-memory handle for one hold (or one plan-quota
// pseudo-hold). It is reconstructable from the billing_holds row via
// LoadHeldReservation, which is how the video completion path finds its
// hold across process restarts.
type Reservation struct {
	HoldID         uuid.UUID
	Key            string
	Scope          string
	UserID         uuid.UUID
	OrganizationID *uuid.UUID
	UserPlanID     *uuid.UUID
	Source         entusagelog.BillingSource
	Amount         decimal.Decimal
	// PlanState is set for plan-quota reservations so Settle can consume
	// the Redis windows without a second ActiveUserPlan lookup.
	PlanState *PlanChargeState
}

// Monetary reports whether settle/release move real balance (as opposed to
// the plan-quota pseudo-reservation).
func (r *Reservation) Monetary() bool {
	return r != nil && (r.Scope == reserveScopeUser || r.Scope == reserveScopeOrganization)
}

type ReserveInput struct {
	User   *ent.User
	ApiKey *ent.ApiKey
	Group  *ent.ModelGroup
	// PlanState is the snapshot from ResolvePlanState; resolved fresh when
	// nil and the group bills plan_only.
	PlanState *PlanChargeState
	// Key is the request-level idempotency key (gateway request id; WS
	// turns append ":turn<N>").
	Key    string
	Amount decimal.Decimal
	TTL    time.Duration
	Note   string
}

// Reserve freezes in.Amount from the account the group bills against.
// The caller is expected to have passed ResolvePlanState already (access +
// precheck); Reserve owns the money movement and the limit occupation.
func (s *BillingSvc) Reserve(ctx context.Context, in ReserveInput) (*Reservation, error) {
	res, err := s.reserve(ctx, in)
	result := "ok"
	switch {
	case err == nil:
	case errors.Is(err, ErrInsufficientBalance):
		result = "insufficient"
	case errors.Is(err, ErrOrganizationLimit), errors.Is(err, ErrMemberLimit):
		result = "limit"
	default:
		result = "error"
	}
	metrics.BillingReserveTotal.WithLabelValues(result).Inc()
	return res, err
}

func (s *BillingSvc) reserve(ctx context.Context, in ReserveInput) (*Reservation, error) {
	if in.User == nil || in.Group == nil {
		return nil, errors.New("reserve: missing user or group")
	}
	if in.Key == "" {
		return nil, errors.New("reserve: missing idempotency key")
	}
	if in.Amount.IsNegative() {
		return nil, errors.New("reserve: negative amount")
	}
	if in.TTL <= 0 {
		in.TTL = ReserveTTLRequest
	}
	switch in.Group.BillingSource {
	case entgroup.BillingSourceBalanceOnly:
		if in.ApiKey != nil && in.ApiKey.BillingScope == entapikey.BillingScopeOrganization {
			return s.reserveOrganization(ctx, in)
		}
		return s.reserveUser(ctx, in, nil, entusagelog.BillingSourceBalance)
	case entgroup.BillingSourcePlanOnly:
		state := in.PlanState
		if state == nil || state.UserPlan == nil || state.Plan == nil {
			up, plan, err := s.plans.ActiveUserPlan(ctx, in.User.ID)
			if err != nil {
				return nil, err
			}
			state = &PlanChargeState{UserPlan: up, Plan: plan}
		}
		if state.Plan.Type == "pay_as_you_go" {
			upID := state.UserPlan.ID
			return s.reserveUser(ctx, in, &upID, entusagelog.BillingSourcePlanBalance)
		}
		// Plan quota keeps its "last one may cross the line" contract: no
		// monetary hold, settle consumes the Redis windows as before.
		upID := state.UserPlan.ID
		return &Reservation{
			Key:        in.Key,
			Scope:      reserveScopePlanQuota,
			UserID:     in.User.ID,
			UserPlanID: &upID,
			Source:     entusagelog.BillingSourcePlan,
			PlanState:  state,
		}, nil
	default:
		return nil, ErrUnsupportedBillingSource
	}
}

func (s *BillingSvc) reserveUser(ctx context.Context, in ReserveInput, userPlanID *uuid.UUID, source entusagelog.BillingSource) (*Reservation, error) {
	holdID := uuid.New()
	expiresAt := time.Now().Add(in.TTL)
	tx, err := s.d.DB().BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	rollback := func(e error) (*Reservation, error) { _ = tx.Rollback(); return nil, e }
	var insertedID uuid.UUID
	if err := tx.QueryRowContext(ctx, `
		INSERT INTO billing_holds
		  (id, created_at, updated_at, request_key, user_id, scope, organization_id, user_plan_id,
		   billing_source, amount, status, expires_at, note)
		VALUES ($1,NOW(),NOW(),$2,$3,'user',NULL,$4,$5,$6,'held',$7,$8)
		ON CONFLICT (request_key) DO NOTHING
		RETURNING id`,
		holdID, in.Key, in.User.ID, nullableUUIDValue(userPlanID), string(source), in.Amount, expiresAt, in.Note,
	).Scan(&insertedID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			// Duplicate reserve for the same request: nothing was changed in
			// this transaction, hand back the existing hold.
			_ = tx.Rollback()
			return s.loadExistingHold(ctx, in, reserveScopeUser, nil, userPlanID, source)
		}
		return rollback(err)
	}
	var available decimal.Decimal
	if err := tx.QueryRowContext(ctx,
		`SELECT balance FROM users WHERE id=$1 FOR UPDATE`, in.User.ID).Scan(&available); err != nil {
		return rollback(err)
	}
	other, err := countInteractiveHolds(ctx, tx, in.User.ID, nil, insertedID)
	if err != nil {
		return rollback(err)
	}
	holdAmt, err := decideMonetaryReserve(available, in.Amount, other)
	if err != nil {
		return rollback(err)
	}
	newBalance := available.Sub(holdAmt)
	if _, err := tx.ExecContext(ctx,
		`UPDATE users SET balance=$1, updated_at=NOW() WHERE id=$2`,
		newBalance, in.User.ID); err != nil {
		return rollback(err)
	}
	if !holdAmt.Equal(in.Amount) {
		if _, err := tx.ExecContext(ctx,
			`UPDATE billing_holds SET amount=$1, updated_at=NOW() WHERE id=$2`,
			holdAmt, insertedID); err != nil {
			return rollback(err)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &Reservation{
		HoldID: insertedID, Key: in.Key, Scope: reserveScopeUser,
		UserID: in.User.ID, UserPlanID: userPlanID, Source: source, Amount: holdAmt,
		PlanState: in.PlanState,
	}, nil
}

func (s *BillingSvc) reserveOrganization(ctx context.Context, in ReserveInput) (*Reservation, error) {
	if in.ApiKey == nil || in.ApiKey.OrganizationID == nil {
		return nil, ErrOrganizationForbidden
	}
	orgID := *in.ApiKey.OrganizationID
	holdID := uuid.New()
	expiresAt := time.Now().Add(in.TTL)
	tx, err := s.d.DB().BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	rollback := func(e error) (*Reservation, error) { _ = tx.Rollback(); return nil, e }
	// Insert the hold before taking the account lock. Settle and Release both
	// lock hold -> account; using account -> hold here deadlocks when a
	// duplicate reserve waits on the same hold while settle waits on the org.
	var insertedID uuid.UUID
	if err := tx.QueryRowContext(ctx, `
		INSERT INTO billing_holds
		  (id, created_at, updated_at, request_key, user_id, scope, organization_id, user_plan_id,
		   billing_source, amount, status, expires_at, note)
		VALUES ($1,NOW(),NOW(),$2,$3,'organization',$4,NULL,$5,$6,'held',$7,$8)
		ON CONFLICT (request_key) DO NOTHING
		RETURNING id`,
		holdID, in.Key, in.User.ID, orgID, string(entusagelog.BillingSourceOrganizationBalance),
		in.Amount, expiresAt, in.Note,
	).Scan(&insertedID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			_ = tx.Rollback()
			return s.loadExistingHold(ctx, in, reserveScopeOrganization, &orgID, nil,
				entusagelog.BillingSourceOrganizationBalance)
		}
		return rollback(err)
	}
	// The organization row lock is the serialization point for all org
	// spend AND all org holds: the aggregates below cannot race another
	// reserve or charge for the same organization.
	var balance, orgMonthlyLimit decimal.Decimal
	if err := tx.QueryRowContext(ctx,
		`SELECT balance, monthly_spend_limit FROM organizations WHERE id=$1 FOR UPDATE`,
		orgID).Scan(&balance, &orgMonthlyLimit); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return rollback(ErrOrganizationNotFound)
		}
		return rollback(err)
	}
	var memberMonthlyLimit, memberTotalLimit decimal.Decimal
	if err := tx.QueryRowContext(ctx,
		`SELECT monthly_spend_limit, total_spend_limit FROM organization_members WHERE organization_id=$1 AND user_id=$2`,
		orgID, in.User.ID).Scan(&memberMonthlyLimit, &memberTotalLimit); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return rollback(ErrOrganizationForbidden)
		}
		return rollback(err)
	}
	other, err := countInteractiveHolds(ctx, tx, in.User.ID, &orgID, insertedID)
	if err != nil {
		return rollback(err)
	}
	holdAmt, err := decideMonetaryReserve(balance, in.Amount, other)
	if err != nil {
		return rollback(err)
	}
	// Limits occupy the actual freeze (last-request remaining, not the
	// original estimate) plus settled spend and other held amounts. Last-
	// request does not bypass spend caps.
	orgUsed, err := organizationMonthSpend(ctx, tx, orgID)
	if err != nil {
		return rollback(err)
	}
	orgHeld, err := heldOrganizationAmount(ctx, tx, orgID, nil, insertedID)
	if err != nil {
		return rollback(err)
	}
	if orgMonthlyLimit.Sign() > 0 && orgUsed.Add(orgHeld).Add(holdAmt).GreaterThan(orgMonthlyLimit) {
		return rollback(ErrOrganizationLimit)
	}
	memberMonth, memberTotal, err := organizationMemberSpend(ctx, tx, orgID, in.User.ID)
	if err != nil {
		return rollback(err)
	}
	memberHeld, err := heldOrganizationAmount(ctx, tx, orgID, &in.User.ID, insertedID)
	if err != nil {
		return rollback(err)
	}
	if memberMonthlyLimit.Sign() > 0 && memberMonth.Add(memberHeld).Add(holdAmt).GreaterThan(memberMonthlyLimit) {
		return rollback(memberLimitErr("monthly spend limit reached"))
	}
	if memberTotalLimit.Sign() > 0 && memberTotal.Add(memberHeld).Add(holdAmt).GreaterThan(memberTotalLimit) {
		return rollback(memberLimitErr("total spend limit reached"))
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE organizations SET balance = balance - $1, updated_at = NOW() WHERE id=$2`,
		holdAmt, orgID); err != nil {
		return rollback(err)
	}
	if !holdAmt.Equal(in.Amount) {
		if _, err := tx.ExecContext(ctx,
			`UPDATE billing_holds SET amount=$1, updated_at=NOW() WHERE id=$2`,
			holdAmt, insertedID); err != nil {
			return rollback(err)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &Reservation{
		HoldID: insertedID, Key: in.Key, Scope: reserveScopeOrganization,
		UserID: in.User.ID, OrganizationID: &orgID,
		Source: entusagelog.BillingSourceOrganizationBalance, Amount: holdAmt,
	}, nil
}

// heldOrganizationAmount sums the still-frozen estimates counting against an
// organization's (or one member's) limits, excluding the hold being created.
func heldOrganizationAmount(ctx context.Context, tx *sql.Tx, orgID uuid.UUID, userID *uuid.UUID, exclude uuid.UUID) (decimal.Decimal, error) {
	q := `
		SELECT COALESCE(SUM(amount), 0)
		FROM billing_holds
		WHERE organization_id=$1 AND scope='organization' AND status='held' AND id <> $2`
	args := []any{orgID, exclude}
	if userID != nil {
		q += ` AND user_id=$3`
		args = append(args, *userID)
	}
	var held decimal.Decimal
	err := tx.QueryRowContext(ctx, q, args...).Scan(&held)
	return held, err
}

func countInteractiveHolds(ctx context.Context, tx *sql.Tx, userID uuid.UUID, organizationID *uuid.UUID, exclude uuid.UUID) (int, error) {
	q := `
		SELECT COUNT(*)
		FROM billing_holds
		WHERE status='held' AND id <> $1
		  AND (request_key LIKE 'llm:%' OR request_key LIKE 'ws:%')`
	args := []any{exclude}
	if organizationID != nil {
		q += ` AND scope='organization' AND organization_id=$2`
		args = append(args, *organizationID)
	} else {
		q += ` AND scope='user' AND user_id=$2`
		args = append(args, userID)
	}
	var n int
	err := tx.QueryRowContext(ctx, q, args...).Scan(&n)
	return n, err
}

// loadExistingHold resolves an idempotency-key conflict: return the live
// hold only when it belongs to the same logical reservation; otherwise refuse
// to let the colliding request borrow another account's frozen funds.
func (s *BillingSvc) loadExistingHold(ctx context.Context, in ReserveInput, scope string, organizationID, userPlanID *uuid.UUID, source entusagelog.BillingSource) (*Reservation, error) {
	r, status, err := s.loadHoldByKey(ctx, in.Key)
	if err != nil {
		return nil, err
	}
	if r == nil {
		return nil, errors.New("billing hold conflict: row vanished")
	}
	if status != "held" {
		return nil, ErrHoldFinished
	}
	if r.UserID != in.User.ID ||
		r.Scope != scope ||
		!sameOptionalUUID(r.OrganizationID, organizationID) ||
		!sameOptionalUUID(r.UserPlanID, userPlanID) ||
		r.Source != source ||
		r.Amount.GreaterThan(in.Amount) {
		return nil, ErrHoldIdentityConflict
	}
	return r, nil
}

func sameOptionalUUID(a, b *uuid.UUID) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

// LoadHeldReservation returns the still-held reservation for an idempotency
// key, or nil when no hold exists or it already finished. Used by the video
// completion path, which may run in a different process than the submit.
func (s *BillingSvc) LoadHeldReservation(ctx context.Context, key string) (*Reservation, error) {
	r, status, err := s.loadHoldByKey(ctx, key)
	if err != nil {
		return nil, err
	}
	if r == nil || status != "held" {
		return nil, nil
	}
	return r, nil
}

func (s *BillingSvc) loadHoldByKey(ctx context.Context, key string) (*Reservation, string, error) {
	var (
		r          Reservation
		orgText    sql.NullString
		planText   sql.NullString
		sourceText string
		status     string
	)
	err := s.d.DB().QueryRowContext(ctx, `
		SELECT id, user_id, scope, organization_id, user_plan_id, billing_source, amount, status
		FROM billing_holds WHERE request_key=$1`, key).
		Scan(&r.HoldID, &r.UserID, &r.Scope, &orgText, &planText, &sourceText, &r.Amount, &status)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, "", nil
	}
	if err != nil {
		return nil, "", err
	}
	r.Key = key
	r.Source = entusagelog.BillingSource(sourceText)
	if orgText.Valid {
		id, err := uuid.Parse(orgText.String)
		if err != nil {
			return nil, "", err
		}
		r.OrganizationID = &id
	}
	if planText.Valid {
		id, err := uuid.Parse(planText.String)
		if err != nil {
			return nil, "", err
		}
		r.UserPlanID = &id
	}
	return &r, status, nil
}

// SettleOptions tunes one settle.
type SettleOptions struct {
	// Note lands on the deduct ledger row.
	Note string
	// CapAtHold caps the charge at the reserved amount. Used for
	// RECONSTRUCTED costs (client hung up mid-stream, usage estimated from
	// observed output) where charging beyond what we told the balance we
	// might take would over-collect on a guess. Exact upstream-reported
	// usage settles without the cap.
	CapAtHold bool
	// TaskID, when set, atomically claims generation_tasks.billed inside
	// the SAME transaction as the charge. If the flag was already taken the
	// whole settle rolls back with ErrTaskAlreadyBilled — this is the
	// "Charge success and Billed=true are one atom" guarantee for video.
	TaskID *uuid.UUID
}

// SettleReservation charges the actual cost against a reservation.
//
// Money flow for a monetary hold (status=held):
//
//	charged = min(final, amount)                      when CapAtHold
//	charged = final, extra taken from live balance    otherwise (may go negative)
//	balance += amount - charged
//	ledger  += one deduct row of `charged`
//
// Idempotent: settling an already-settled hold returns the recorded result
// without moving money. A hold the reaper released mid-flight falls back to
// a direct charge: exact usage may overdraft; CapAtHold still clamps at
// zero so reconstructed usage cannot create debt. CapAtHold shortfalls
// (live balance could not cover the capped want) are reported as a wrapped
// ErrInsufficientBalance alongside a non-nil result.
func (s *BillingSvc) SettleReservation(ctx context.Context, r *Reservation, base, final decimal.Decimal, opts SettleOptions) (*ChargeResult, error) {
	res, err := s.settleReservation(ctx, r, base, final, opts)
	result := "ok"
	if err != nil {
		result = "error"
	}
	metrics.BillingSettleTotal.WithLabelValues(result).Inc()
	return res, err
}

func (s *BillingSvc) settleReservation(ctx context.Context, r *Reservation, base, final decimal.Decimal, opts SettleOptions) (*ChargeResult, error) {
	if r == nil {
		return nil, errors.New("settle: nil reservation")
	}
	if final.IsNegative() {
		final = decimal.Zero
	}
	if r.Scope == reserveScopePlanQuota {
		if opts.TaskID != nil {
			return nil, errors.New("settle: plan-quota reservations cannot claim a task atomically")
		}
		if r.PlanState == nil || r.PlanState.UserPlan == nil || r.PlanState.Plan == nil {
			return nil, errors.New("settle: plan-quota reservation missing plan state")
		}
		if _, _, err := s.plans.ConsumeQuotaOnce(ctx, r.PlanState.UserPlan, r.PlanState.Plan, final, r.Key); err != nil {
			return nil, err
		}
		return &ChargeResult{
			BillingSource: entusagelog.BillingSourcePlan,
			UserPlanID:    r.UserPlanID,
			CostBase:      base, CostFinal: final, Currency: CurrencyCNY,
		}, nil
	}
	if !r.Monetary() {
		return nil, fmt.Errorf("settle: unknown reservation scope %q", r.Scope)
	}

	tx, err := s.d.DB().BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	rollback := func(e error) (*ChargeResult, error) { _ = tx.Rollback(); return nil, e }

	var (
		amount, settled decimal.Decimal
		settledNull     sql.NullString
		status          string
	)
	if err := tx.QueryRowContext(ctx, `
		SELECT amount, settled_amount, status FROM billing_holds WHERE id=$1 FOR UPDATE`,
		r.HoldID).Scan(&amount, &settledNull, &status); err != nil {
		return rollback(err)
	}
	if settledNull.Valid {
		if settled, err = decimal.NewFromString(settledNull.String); err != nil {
			return rollback(err)
		}
	}

	result := &ChargeResult{
		BillingSource:  r.Source,
		UserPlanID:     r.UserPlanID,
		OrganizationID: r.OrganizationID,
		CostBase:       base, CostFinal: final, Currency: CurrencyCNY,
	}

	claimTask := func() error {
		if opts.TaskID == nil {
			return nil
		}
		res, err := tx.ExecContext(ctx, `
			UPDATE generation_tasks SET billed=true, cost_final=$1, updated_at=NOW()
			WHERE id=$2 AND billed=false`, final, *opts.TaskID)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n == 0 {
			return ErrTaskAlreadyBilled
		}
		return nil
	}

	switch status {
	case "settled":
		// Retried settle: report what actually happened, move nothing.
		_ = tx.Rollback()
		res := *result
		res.CostFinal = settled
		return &res, nil
	case "released":
		// The reaper (or an explicit release) already returned the hold.
		// Delivered work still has to be paid for. Exact usage charges
		// `final` (may overdraft). CapAtHold charges at most the original
		// hold and never below zero so reconstructed usage cannot create
		// debt. Close the hold as settled in the same transaction so a
		// retry cannot run the fallback charge twice.
		want := final
		if opts.CapAtHold && want.GreaterThan(amount) {
			want = amount
		}
		charged, shortfall, err := s.chargeAccountClamped(ctx, tx, r, want, opts.Note, opts.CapAtHold)
		if err != nil {
			return rollback(err)
		}
		if err := claimTask(); err != nil {
			return rollback(err)
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE billing_holds
			SET status='settled', settled_amount=$1, closed_at=NOW(), updated_at=NOW()
			WHERE id=$2`, charged, r.HoldID); err != nil {
			return rollback(err)
		}
		if err := tx.Commit(); err != nil {
			return nil, err
		}
		result.CostFinal = charged
		if shortfall.Sign() > 0 {
			return result, fmt.Errorf("%w: settle after release short by %s", ErrInsufficientBalance, shortfall)
		}
		return result, nil
	case "held":
		// proceed
	default:
		return rollback(fmt.Errorf("settle: hold %s in unknown status %q", r.HoldID, status))
	}

	charged := final
	if opts.CapAtHold && charged.GreaterThan(amount) {
		charged = amount
	}

	// Account update: one row lock. Exact usage may overdraft; CapAtHold
	// already capped `charged` at the hold so returning unused hold cannot
	// create debt from reconstructed usage. Do not report a clamp shortfall.
	table, txTable, accountID := r.accountTables()
	var balance decimal.Decimal
	if err := tx.QueryRowContext(ctx,
		`SELECT balance FROM `+table+` WHERE id=$1 FOR UPDATE`, accountID).Scan(&balance); err != nil {
		return rollback(err)
	}
	newBalance := balance.Add(amount).Sub(charged)
	if _, err := tx.ExecContext(ctx,
		`UPDATE `+table+` SET balance=$1, currency='CNY', updated_at=NOW() WHERE id=$2`,
		newBalance, accountID); err != nil {
		return rollback(err)
	}
	if charged.Sign() > 0 {
		if err := insertDeductTx(ctx, tx, txTable, r, charged, newBalance, opts.Note); err != nil {
			return rollback(err)
		}
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE billing_holds
		SET status='settled', settled_amount=$1, closed_at=NOW(), updated_at=NOW()
		WHERE id=$2`, charged, r.HoldID); err != nil {
		return rollback(err)
	}
	if err := claimTask(); err != nil {
		return rollback(err)
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	result.CostFinal = charged
	return result, nil
}

// accountTables maps a reservation to (account table, ledger table, account id).
func (r *Reservation) accountTables() (string, string, uuid.UUID) {
	if r.Scope == reserveScopeOrganization && r.OrganizationID != nil {
		return "organizations", "organization_balance_txes", *r.OrganizationID
	}
	return "users", "balance_txes", r.UserID
}

func insertDeductTx(ctx context.Context, tx *sql.Tx, txTable string, r *Reservation, charged, balanceAfter decimal.Decimal, note string) error {
	if txTable == "organization_balance_txes" {
		_, err := tx.ExecContext(ctx, `
			INSERT INTO organization_balance_txes
			  (id, created_at, updated_at, organization_id, actor_user_id, kind, amount, balance_after, currency, note)
			VALUES ($1,NOW(),NOW(),$2,$3,'deduct',$4,$5,'CNY',$6)`,
			uuid.New(), *r.OrganizationID, r.UserID, charged.Neg(), balanceAfter, note)
		return err
	}
	_, err := tx.ExecContext(ctx, `
		INSERT INTO balance_txes
		  (id, created_at, updated_at, user_id, kind, amount, balance_after, currency, note)
		VALUES ($1,NOW(),NOW(),$2,'deduct',$3,$4,'CNY',$5)`,
		uuid.New(), r.UserID, charged.Neg(), balanceAfter, note)
	return err
}

// chargeAccountClamped is the released-hold fallback: charge `want` from the
// live balance and write the deduct row. When clampNonNegative is true
// (CapAtHold), never go below zero and return the uncollected remainder.
// Otherwise charge `want` in full; the balance may go negative and
// shortfall is 0.
func (s *BillingSvc) chargeAccountClamped(ctx context.Context, tx *sql.Tx, r *Reservation, want decimal.Decimal, note string, clampNonNegative bool) (charged, shortfall decimal.Decimal, err error) {
	table, txTable, accountID := r.accountTables()
	var balance decimal.Decimal
	if err := tx.QueryRowContext(ctx,
		`SELECT balance FROM `+table+` WHERE id=$1 FOR UPDATE`, accountID).Scan(&balance); err != nil {
		return decimal.Zero, decimal.Zero, err
	}
	if clampNonNegative {
		avail := balance
		if avail.IsNegative() {
			avail = decimal.Zero
		}
		charged = decimal.Min(want, avail)
		shortfall = want.Sub(charged)
	} else {
		charged = want
		shortfall = decimal.Zero
	}
	newBalance := balance.Sub(charged)
	if _, err := tx.ExecContext(ctx,
		`UPDATE `+table+` SET balance=$1, currency='CNY', updated_at=NOW() WHERE id=$2`,
		newBalance, accountID); err != nil {
		return decimal.Zero, decimal.Zero, err
	}
	if charged.Sign() > 0 {
		if err := insertDeductTx(ctx, tx, txTable, r, charged, newBalance, note); err != nil {
			return decimal.Zero, decimal.Zero, err
		}
	}
	return charged, shortfall, nil
}

// ReleaseReservation returns the full held amount — nothing billable
// happened. Idempotent; a no-op for plan-quota reservations and for holds
// that already finished.
func (s *BillingSvc) ReleaseReservation(ctx context.Context, r *Reservation) error {
	if r == nil || !r.Monetary() {
		return nil
	}
	return s.releaseHoldByID(ctx, r.HoldID)
}

func (s *BillingSvc) releaseHoldByID(ctx context.Context, holdID uuid.UUID) error {
	if holdID == uuid.Nil {
		return nil
	}
	tx, err := s.d.DB().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	rollback := func(e error) error { _ = tx.Rollback(); return e }
	var (
		userID  uuid.UUID
		scope   string
		orgText sql.NullString
		amount  decimal.Decimal
		status  string
	)
	if err := tx.QueryRowContext(ctx, `
		SELECT user_id, scope, organization_id, amount, status
		FROM billing_holds WHERE id=$1 FOR UPDATE`, holdID).
		Scan(&userID, &scope, &orgText, &amount, &status); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return rollback(nil)
		}
		return rollback(err)
	}
	if status != "held" {
		return tx.Commit()
	}
	table, accountID := "users", userID
	if scope == reserveScopeOrganization && orgText.Valid {
		orgID, parseErr := uuid.Parse(orgText.String)
		if parseErr != nil {
			return rollback(parseErr)
		}
		table, accountID = "organizations", orgID
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE `+table+` SET balance = balance + $1, updated_at = NOW() WHERE id=$2`,
		amount, accountID); err != nil {
		return rollback(err)
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE billing_holds
		SET status='released', closed_at=NOW(), updated_at=NOW()
		WHERE id=$1`, holdID); err != nil {
		return rollback(err)
	}
	return tx.Commit()
}

// ReleaseExpiredHolds is the crash-safety reaper: a hold whose owner died
// before settling is returned to its balance once expires_at passes. An
// in-flight request whose hold gets reaped still charges at settle time via
// the released-hold fallback, so reaping can only ever return money early —
// never lose a charge.
func (s *BillingSvc) ReleaseExpiredHolds(ctx context.Context, limit int) error {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.d.DB().QueryContext(ctx, `
		SELECT id FROM billing_holds
		WHERE status='held' AND expires_at <= NOW()
		ORDER BY expires_at LIMIT $1`, limit)
	if err != nil {
		return err
	}
	defer rows.Close()
	var ids []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, id := range ids {
		if err := s.releaseHoldByID(ctx, id); err != nil {
			return err
		}
	}
	return nil
}

// StartHoldCleanup runs the expired-hold reaper on the leader, mirroring
// control.SQLLedger.StartCleanup.
func (s *BillingSvc) StartHoldCleanup(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = 5 * time.Minute
	}
	go func() {
		cleanup := func() {
			if err := s.ReleaseExpiredHolds(ctx, 100); err != nil {
				slog.Warn("billing hold cleanup failed", "err", err)
			}
		}
		cleanup()
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				cleanup()
			}
		}
	}()
}

// --- conservative cost estimation ---

const (
	// defaultReserveOutputTokens is used when the client did not bound the
	// generation with max_tokens. Deliberately high — the product rule is
	// "reserve the maximum possible cost, refund the difference" — while
	// staying below pathological context sizes so a modest balance can
	// still start a request.
	defaultReserveOutputTokens = 32768
	// Input estimation: UTF-8 bytes / 2 over-counts English (~4 bytes per
	// token) about 2x and covers CJK (~3 bytes per char, roughly 1-1.5
	// tokens per char). Clamped so base64 image payloads can't inflate the
	// hold into the stratosphere.
	maxEstimatedInputTokens = 262144
	minEstimatedInputTokens = 64
	// defaultReserveVideoSeconds bounds a video hold when the client did
	// not request a duration.
	defaultReserveVideoSeconds = 15
)

// EstimateInputTokens converts a request-body size into a conservative
// (over-counting) input token estimate.
func EstimateInputTokens(bodyBytes int) int {
	t := bodyBytes / 2
	if t < minEstimatedInputTokens {
		return minEstimatedInputTokens
	}
	if t > maxEstimatedInputTokens {
		return maxEstimatedInputTokens
	}
	return t
}

// CostEstimateInput describes what is known about a request before it runs.
type CostEstimateInput struct {
	// BodyBytes is the raw request body size (input token estimation).
	BodyBytes int
	// MaxOutputTokens is the client-requested generation cap; <=0 falls
	// back to defaultReserveOutputTokens.
	MaxOutputTokens int
	// OutputSeconds is the requested media duration (video); <=0 falls
	// back to defaultReserveVideoSeconds when the model prices seconds.
	OutputSeconds int
}

// EstimateMaxCost returns a conservative upper bound on the request's final
// cost (post rate-multiplier, post pool-throttle): the amount Reserve should
// freeze. Estimation prefers OVER-reserving — the settle refunds the
// difference — and prices the whole input at the highest applicable input
// rate (cache writes cost more than plain input on some models).
//
// A model whose pricing is misconfigured (CalcCost would error) estimates
// zero rather than blocking traffic: the hold then guards nothing extra, and
// the settle-time CalcCost failure is reported exactly as before.
func (s *BillingSvc) EstimateMaxCost(ctx context.Context, m *ent.Model, group *ent.ModelGroup, in CostEstimateInput) decimal.Decimal {
	if m == nil || group == nil {
		return decimal.Zero
	}
	thousand := decimal.NewFromInt(1000)
	base := decimal.Zero
	switch m.BillingMode {
	case "per_request":
		if m.PricePerRequest != nil {
			base = *m.PricePerRequest
		}
	case "per_token":
		if m.InputUnitPrice == nil || m.OutputUnitPrice == nil {
			break
		}
		inTok := EstimateInputTokens(in.BodyBytes)
		outTok := in.MaxOutputTokens
		if outTok <= 0 {
			outTok = defaultReserveOutputTokens
		}
		inPrice := m.InputUnitPrice
		outPrice := m.OutputUnitPrice
		cacheWrite := m.CacheWriteInputPrice
		cacheWrite1h := m.CacheWrite1hInputPrice
		if tier := pickPriceTier(m.PriceTiers, int64(inTok)); tier != nil {
			if tier.InputUnitPrice != nil {
				inPrice = tier.InputUnitPrice
			}
			if tier.OutputUnitPrice != nil {
				outPrice = tier.OutputUnitPrice
			}
			if tier.CacheWriteInputPrice != nil {
				cacheWrite = tier.CacheWriteInputPrice
			}
			if tier.CacheWrite1hInputPrice != nil {
				cacheWrite1h = tier.CacheWrite1hInputPrice
			}
		}
		// Reserve at the higher write rate so a 1h cache create cannot
		// over-run a hold sized for 5m.
		if cacheWrite1h != nil && (cacheWrite == nil || cacheWrite1h.GreaterThan(*cacheWrite)) {
			cacheWrite = cacheWrite1h
		}
		if cacheWrite != nil && cacheWrite.GreaterThan(*inPrice) {
			inPrice = cacheWrite
		}
		base = inPrice.Mul(decimal.NewFromInt(int64(inTok))).Div(thousand).
			Add(outPrice.Mul(decimal.NewFromInt(int64(outTok))).Div(thousand))
	}
	if m.OutputSecondPrice != nil {
		secs := in.OutputSeconds
		if secs <= 0 {
			secs = defaultReserveVideoSeconds
		}
		base = base.Add(m.OutputSecondPrice.Mul(decimal.NewFromInt(int64(secs))))
	}
	final := base.Mul(decimal.NewFromFloat(group.RateMultiplier))
	if final.IsNegative() {
		return decimal.Zero
	}
	return final
}

func nullableUUIDValue(id *uuid.UUID) any {
	if id == nil || *id == uuid.Nil {
		return nil
	}
	return *id
}
