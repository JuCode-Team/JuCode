// invite.go: the invite/referral rebate engine (InviteSvc).
//
// Three record kinds, all in Postgres:
//
//   - InviteBinding: invitee→inviter, written once at registration
//   - InviteReward:  the (at most one) fixed signup reward per binding
//   - InviteRebate:  one row per rebated invitee TOPUP, unique on the
//     source order id (that constraint IS the idempotency). Rows with
//     source_kind=consume are the retired per-request model, kept for
//     history only.
//
// Money always moves through the same pattern as every other credit in this
// codebase: lock the user row, bump balance, write a BalanceTx
// (kind=invite_reward / invite_rebate) in one transaction.
//
// All entry points that other services call from their hot paths
// (EvaluateSignupAsync, ProcessTopupRebateAsync, ReconcileRefund) are
// nil-receiver-safe: an invite failure must never fail a registration, a
// topup, or a refund.
package biz

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/jucode/jucode/internal/data"
	"github.com/jucode/jucode/internal/data/ent"
	entbalancetx "github.com/jucode/jucode/internal/data/ent/balancetx"
	entbinding "github.com/jucode/jucode/internal/data/ent/invitebinding"
	entrebate "github.com/jucode/jucode/internal/data/ent/inviterebate"
	entreward "github.com/jucode/jucode/internal/data/ent/invitereward"
	entredeemcode "github.com/jucode/jucode/internal/data/ent/redeemcode"
	entredeemrecord "github.com/jucode/jucode/internal/data/ent/redeemrecord"
	entsiteorder "github.com/jucode/jucode/internal/data/ent/siteorder"
	entuser "github.com/jucode/jucode/internal/data/ent/user"
)

var (
	ErrInviteNotFound     = errors.New("invite record not found")
	ErrInviteInvalidState = errors.New("invite record is not in a state that allows this action")
	// ErrInviteCodeMalformed / ErrInviteCodeUnknown are the two ways a
	// user-supplied code can be unusable. They are separated because the
	// remedies differ: a malformed code is a typo the registrant can fix by
	// re-reading the code, an unknown one means the code itself is wrong.
	ErrInviteCodeMalformed = errors.New("invite code is malformed")
	ErrInviteCodeUnknown   = errors.New("invite code does not exist")
)

// inviteCodeAlphabet is uppercase alphanumerics minus the confusable 0/O/1/I.
// 32 symbols — 256 % 32 == 0, so byte-mod sampling has no modulo bias.
const (
	inviteCodeAlphabet = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"
	inviteCodeLength   = 8
)

type InviteSvc struct {
	d        *data.Data
	settings *SettingsSvc
}

func NewInviteSvc(d *data.Data, settings *SettingsSvc) *InviteSvc {
	return &InviteSvc{d: d, settings: settings}
}

// program returns the current invite configuration; any read failure
// degrades to the all-off defaults so a KV hiccup can never accidentally
// enable payouts.
func (s *InviteSvc) program(ctx context.Context) InviteProgramSettings {
	st, err := s.settings.GetSite(ctx)
	if err != nil || st == nil || st.InviteProgram == nil {
		return DefaultInviteProgram()
	}
	return *st.InviteProgram
}

// --- invite codes ---

func newInviteCode() (string, error) {
	buf := make([]byte, inviteCodeLength)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	out := make([]byte, inviteCodeLength)
	for i, b := range buf {
		out[i] = inviteCodeAlphabet[int(b)%len(inviteCodeAlphabet)]
	}
	return string(out), nil
}

// NormalizeInviteCode uppercases/trims a user-supplied code and returns ""
// when it cannot possibly be a valid code (wrong length or alphabet), so
// lookups skip the DB round-trip for garbage input.
func NormalizeInviteCode(raw string) string {
	code := strings.ToUpper(strings.TrimSpace(raw))
	if len(code) != inviteCodeLength {
		return ""
	}
	for i := 0; i < len(code); i++ {
		if !strings.ContainsRune(inviteCodeAlphabet, rune(code[i])) {
			return ""
		}
	}
	return code
}

// EnsureInviteCode returns the user's invite code, lazily generating one on
// first use (new Register, or first GET /v1/me/invite for pre-existing
// users). Concurrent callers race on a conditional update — whoever loses
// re-reads the winner's code; a unique-constraint collision with another
// user's code retries with a fresh one.
func (s *InviteSvc) EnsureInviteCode(ctx context.Context, userID uuid.UUID) (string, error) {
	u, err := s.d.Ent.User.Get(ctx, userID)
	if err != nil {
		return "", err
	}
	if u.InviteCode != nil && *u.InviteCode != "" {
		return *u.InviteCode, nil
	}
	for attempt := 0; attempt < 8; attempt++ {
		code, err := newInviteCode()
		if err != nil {
			return "", err
		}
		n, err := s.d.Ent.User.Update().
			Where(
				entuser.IDEQ(userID),
				entuser.Or(entuser.InviteCodeIsNil(), entuser.InviteCodeEQ("")),
			).
			SetInviteCode(code).
			Save(ctx)
		if err != nil {
			if ent.IsConstraintError(err) {
				continue // code taken by another user — retry with a new one
			}
			return "", err
		}
		if n == 0 {
			// Another request generated the code concurrently — use theirs.
			u, err := s.d.Ent.User.Get(ctx, userID)
			if err != nil {
				return "", err
			}
			if u.InviteCode != nil && *u.InviteCode != "" {
				return *u.InviteCode, nil
			}
			continue
		}
		return code, nil
	}
	return "", errors.New("invite: could not allocate a unique invite code")
}

// --- binding ---

// ResolveInviter validates a user-supplied invite code and returns the
// account that owns it, reporting ErrInviteCodeMalformed / ErrInviteCodeUnknown
// when it cannot be used. Callers that must not end up with an unusable
// binding use this to reject a bad code up front — invite_bindings.invitee_user_id
// is unique and a binding is only ever written at registration, so a dropped
// code is unrecoverable afterwards.
func (s *InviteSvc) ResolveInviter(ctx context.Context, rawCode string) (*ent.User, error) {
	code := NormalizeInviteCode(rawCode)
	if code == "" {
		return nil, ErrInviteCodeMalformed
	}
	inviter, err := s.d.Ent.User.Query().Where(entuser.InviteCodeEQ(code)).Only(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			return nil, ErrInviteCodeUnknown
		}
		return nil, err
	}
	return inviter, nil
}

// inviteConditionSnapshot is the JSON stored in invite_rewards.condition_snapshot.
type inviteConditionSnapshot struct {
	Condition      string          `json:"condition"`
	MinTopupAmount decimal.Decimal `json:"min_topup_amount"`
	AmountBasis    string          `json:"amount_basis,omitempty"`
}

// BindOnRegister links a fresh invitee to the owner of inviteCode and, when
// the signup reward is enabled with a positive amount, creates the pending
// InviteReward and runs the first qualification pass.
//
// Register resolves the code via ResolveInviter before the account exists, so
// by the time this runs the code is known to be usable; the forgiving
// empty/invalid/unknown branches here only cover the remaining race (the
// inviter's row disappearing in between) and callers that skip that check.
// A code owned by a banned or frozen account still binds — the risk engine
// holds the payout instead, so the evidence trail survives.
func (s *InviteSvc) BindOnRegister(ctx context.Context, inviteeID uuid.UUID, rawCode, registerIP, registerUA string) error {
	code := NormalizeInviteCode(rawCode)
	if code == "" {
		return nil
	}
	inviter, err := s.d.Ent.User.Query().Where(entuser.InviteCodeEQ(code)).Only(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			return nil // unknown code: register proceeds unbound
		}
		return err
	}
	if inviter.ID == inviteeID {
		return nil
	}
	program := s.program(ctx)

	tx, err := s.d.Ent.Tx(ctx)
	if err != nil {
		return err
	}
	rollback := func(e error) error { _ = tx.Rollback(); return e }
	// The invitee row serializes this decision with agent-code redemption.
	// If redemption commits first we observe its agent attribution; if this
	// bind commits first, any signup payout happened before the later agent
	// relationship took effect and is intentionally not clawed back.
	if _, err := tx.User.Query().
		Where(entuser.IDEQ(inviteeID)).
		ForUpdate().
		Only(ctx); err != nil {
		return rollback(err)
	}
	hasAgent, err := hasAgentAttribution(ctx, tx.Client(), inviteeID)
	if err != nil {
		return rollback(err)
	}
	// Serialize bindings for one inviter. Besides making the denormalized
	// relationship deterministic, this guarantees that a concurrent burst
	// cannot make every registration believe it is within the same-IP cap.
	if _, err := tx.User.Query().
		Where(entuser.IDEQ(inviter.ID)).
		ForUpdate().
		Only(ctx); err != nil {
		return rollback(err)
	}
	binding, err := tx.InviteBinding.Create().
		SetInviteeUserID(inviteeID).
		SetInviterUserID(inviter.ID).
		SetInviteCodeUsed(code).
		SetRegisterIP(registerIP).
		SetRegisterUserAgent(registerUA).
		Save(ctx)
	if err != nil {
		_ = tx.Rollback()
		if ent.IsConstraintError(err) {
			return nil // invitee already bound (unique invitee_user_id) — never re-bind
		}
		return err
	}
	if _, err := tx.User.UpdateOneID(inviteeID).SetInvitedByUserID(inviter.ID).Save(ctx); err != nil {
		return rollback(err)
	}
	// Reward row only when the program is live NOW — enabling the program
	// later is not retroactive for bindings created while it was off.
	// Agent-attributed customers may retain an invite binding as an audit
	// trail, but agent commission and invite rewards are mutually exclusive.
	if !hasAgent && program.Enabled && program.SignupRewardEnabled && program.SignupRewardAmount.Sign() > 0 {
		snapshot, _ := json.Marshal(inviteConditionSnapshot{
			Condition:      program.SignupCondition,
			MinTopupAmount: program.SignupMinTopupAmount,
			AmountBasis:    "paid_orders",
		})
		// Inviter IPs drift (last_login_ip); record the match seen at
		// signup so later evaluations keep it.
		selfFlags, err := selfReferralFlags(ctx, tx.Client(), binding, inviter)
		if err != nil {
			return rollback(err)
		}
		if _, err := tx.InviteReward.Create().
			SetRiskFlags(selfFlags).
			SetBindingID(binding.ID).
			SetInviterUserID(inviter.ID).
			SetInviteeUserID(inviteeID).
			SetAmount(program.SignupRewardAmount).
			SetCurrency(CurrencyCNY).
			SetConditionSnapshot(string(snapshot)).
			Save(ctx); err != nil {
			return rollback(err)
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if err := s.EvaluateSignup(ctx, inviteeID); err != nil {
		slog.Warn("invite: signup evaluation after bind failed", "invitee_id", inviteeID, "err", err.Error())
	}
	return nil
}

// --- signup qualification + risk ---

// EvaluateSignupAsync re-runs signup qualification off the caller's path.
// Safe on a nil receiver so hook sites don't need wiring guards.
func (s *InviteSvc) EvaluateSignupAsync(userID uuid.UUID) {
	if s == nil {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := s.EvaluateSignup(ctx, userID); err != nil {
			slog.Warn("invite: signup evaluation failed", "user_id", userID.String(), "err", err.Error())
		}
	}()
}

// InviteCouponConflict says which of the inviter's payouts a coupon on the
// invitee's next balance top-up would forfeit: coupon orders never rebate
// and never count toward the signup reward's top-up condition.
type InviteCouponConflict struct {
	TopupRebate  bool `json:"topup_rebate"`
	SignupReward bool `json:"signup_reward"`
}

// CouponConflict reports what a coupon would cost userID's inviter right
// now, so checkout can warn before the order is created. Zero value when
// the user has no live binding.
func (s *InviteSvc) CouponConflict(ctx context.Context, userID uuid.UUID) (InviteCouponConflict, error) {
	var out InviteCouponConflict
	program := s.program(ctx)
	if !program.Enabled {
		return out, nil
	}
	binding, err := s.d.Ent.InviteBinding.Query().
		Where(entbinding.InviteeUserIDEQ(userID), entbinding.StatusEQ(entbinding.StatusBound)).
		Only(ctx)
	if ent.IsNotFound(err) {
		return out, nil
	}
	if err != nil {
		return out, err
	}
	hasAgent, err := hasAgentAttribution(ctx, s.d.Ent, userID)
	if err != nil || hasAgent {
		return out, err
	}

	if program.TopupRebateEnabled && program.TopupRebateRatePct.Sign() > 0 &&
		(program.TopupRebateWindowDays <= 0 ||
			time.Now().Before(binding.CreatedAt.AddDate(0, 0, program.TopupRebateWindowDays))) {
		out.TopupRebate = true
		if program.TopupRebateCapPerInvitee.Sign() > 0 {
			used, err := rebateCommittedSum(ctx, s.d.Ent.InviteRebate.Query().
				Where(
					entrebate.InviteeUserIDEQ(userID),
					entrebate.SourceKindEQ(entrebate.SourceKindTopup),
				))
			if err != nil {
				return out, err
			}
			out.TopupRebate = used.LessThan(program.TopupRebateCapPerInvitee)
		}
	}

	reward, err := s.d.Ent.InviteReward.Query().
		Where(entreward.BindingIDEQ(binding.ID), entreward.StatusEQ(entreward.StatusPending)).
		Only(ctx)
	if err != nil && !ent.IsNotFound(err) {
		return out, err
	}
	if reward != nil && program.SignupRewardEnabled {
		var snap inviteConditionSnapshot
		_ = json.Unmarshal([]byte(reward.ConditionSnapshot), &snap)
		out.SignupReward = snap.Condition == InviteConditionAnyTopup || snap.Condition == InviteConditionMinTopup
	}
	return out, nil
}

// EvaluateSignup re-checks the invitee's signup reward against its
// snapshotted condition and either holds (risk) or pays it. Idempotent and
// cheap when there is nothing to do; called on every qualifying event
// (email verified / any topup landing / admin approval).
func (s *InviteSvc) EvaluateSignup(ctx context.Context, inviteeID uuid.UUID) error {
	binding, err := s.d.Ent.InviteBinding.Query().
		Where(entbinding.InviteeUserIDEQ(inviteeID)).
		Only(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			return nil
		}
		return err
	}
	if binding.Status != entbinding.StatusBound {
		return nil
	}
	hasAgent, err := hasAgentAttribution(ctx, s.d.Ent, inviteeID)
	if err != nil {
		return err
	}
	if hasAgent {
		return nil
	}
	reward, err := s.d.Ent.InviteReward.Query().
		Where(entreward.BindingIDEQ(binding.ID)).
		Only(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			return nil
		}
		return err
	}
	if reward.Status != entreward.StatusPending &&
		reward.Status != entreward.StatusHeld &&
		reward.Status != entreward.StatusQualified {
		return nil
	}
	program := s.program(ctx)
	// Program switched off after the reward was created: freeze in place —
	// no payout, no state churn. Turning it back on resumes evaluation.
	if !program.Enabled || !program.SignupRewardEnabled {
		return nil
	}
	ok, err := s.signupConditionMet(ctx, reward)
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}

	// Lock the reward before deciding its next state. Without this, an
	// evaluator that read "pending" could overwrite a concurrent pay/reject/
	// void terminal transition and make the same reward payable again.
	tx, err := s.d.Ent.Tx(ctx)
	if err != nil {
		return err
	}
	rollback := func(e error) error { _ = tx.Rollback(); return e }
	reward, err = tx.InviteReward.Query().
		Where(entreward.IDEQ(reward.ID)).
		ForUpdate().
		Only(ctx)
	if err != nil {
		return rollback(err)
	}
	if reward.Status != entreward.StatusPending &&
		reward.Status != entreward.StatusHeld &&
		reward.Status != entreward.StatusQualified {
		_ = tx.Rollback()
		return nil
	}
	binding, err = tx.InviteBinding.Get(ctx, reward.BindingID)
	if err != nil {
		return rollback(err)
	}
	if binding.Status != entbinding.StatusBound {
		_ = tx.Rollback()
		return nil
	}
	// The inviter row is the serialization point for all of this inviter's
	// qualification-cap decisions. A qualified row is committed before the
	// actual payout transaction, so the next evaluator counts it even while
	// payout is still in flight.
	inviter, err := tx.User.Query().
		Where(entuser.IDEQ(reward.InviterUserID)).
		ForUpdate().
		Only(ctx)
	if err != nil {
		return rollback(err)
	}
	flags, err := s.signupRiskFlags(ctx, tx.Client(), program, binding, reward, inviter)
	if err != nil {
		return rollback(err)
	}
	status := entreward.StatusQualified
	if len(flags) > 0 && program.AutoHoldOnRisk {
		status = entreward.StatusHeld
	}
	if overReviewThreshold(program, reward.Amount) {
		flags = append(flags, riskAmountOverThreshold)
		status = entreward.StatusHeld
	}
	var snap inviteConditionSnapshot
	if err := json.Unmarshal([]byte(reward.ConditionSnapshot), &snap); err != nil && reward.ConditionSnapshot != "" {
		return rollback(err)
	}
	if snap.Condition == InviteConditionAnyTopup || snap.Condition == InviteConditionMinTopup {
		snap.AmountBasis = "paid_orders"
		raw, err := json.Marshal(snap)
		if err != nil {
			return rollback(err)
		}
		reward.ConditionSnapshot = string(raw)
	}
	reward, err = tx.InviteReward.UpdateOneID(reward.ID).
		SetConditionSnapshot(reward.ConditionSnapshot).
		SetStatus(status).
		SetRiskFlags(flags).
		Save(ctx)
	if err != nil {
		return rollback(err)
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if status == entreward.StatusHeld {
		return nil
	}
	if _, err := s.PayReward(ctx, reward.ID, "auto payout on qualification"); err != nil {
		// Guard rejections (banned inviter, voided binding, ...) are not
		// engine errors: the row stays qualified/held for admin review.
		if errors.Is(err, ErrInviteInvalidState) {
			return nil
		}
		return err
	}
	return nil
}

// signupConditionMet evaluates the reward's SNAPSHOTTED condition — never
// the live settings — so an admin flipping the condition later cannot
// re-qualify or dis-qualify rewards already on the books.
func (s *InviteSvc) signupConditionMet(ctx context.Context, reward *ent.InviteReward) (bool, error) {
	snap := inviteConditionSnapshot{Condition: InviteConditionEmailVerified}
	if reward.ConditionSnapshot != "" {
		_ = json.Unmarshal([]byte(reward.ConditionSnapshot), &snap)
	}
	switch snap.Condition {
	case InviteConditionNone:
		return true, nil
	case InviteConditionEmailVerified:
		invitee, err := s.d.Ent.User.Get(ctx, reward.InviteeUserID)
		if err != nil {
			return false, err
		}
		return invitee.EmailVerified, nil
	case InviteConditionAnyTopup, InviteConditionMinTopup:
		total, err := s.signupPaidTotal(ctx, reward, snap)
		if err != nil {
			return false, err
		}
		if snap.Condition == InviteConditionAnyTopup {
			return total.Sign() > 0, nil
		}
		return total.GreaterThanOrEqual(snap.MinTopupAmount), nil
	default:
		return false, nil
	}
}

// Already-paid legacy rewards retain the ledger basis they qualified on.
// Unpaid rewards and rewards evaluated by this version use paid orders.
func (s *InviteSvc) signupPaidTotal(ctx context.Context, reward *ent.InviteReward, snap inviteConditionSnapshot) (decimal.Decimal, error) {
	if reward.Status != entreward.StatusPaid || snap.AmountBasis == "paid_orders" {
		return s.inviteePaidTotal(ctx, reward.InviteeUserID)
	}
	var total decimal.Decimal
	err := s.d.DB().QueryRowContext(ctx, `
		SELECT GREATEST(COALESCE(SUM(b.amount), 0), 0)
		FROM balance_txes b
		WHERE b.user_id = $1
		  AND ((b.kind = 'topup' AND b.amount > 0) OR b.kind = 'refund')
		  AND NOT EXISTS (
		    SELECT 1 FROM site_orders o WHERE o.user_id = $1
		    AND o.promotion_promoter_user_id IS NOT NULL
		    AND b.note IN ($2 || o.order_no, $3 || o.order_no))`,
		reward.InviteeUserID, siteOrderTopupNote, siteOrderRefundNote).Scan(&total)
	return total, err
}

// inviteePaidTotal counts net paid balance and plan orders. Order amount is
// cash paid, unlike credit_amount or a plan's gift balance. Approved refunds
// reduce it once; pending refunds and unpaid orders do not count. Coupon
// orders remain excluded because they already reward an agent.
// Existing any_topup/min_topup snapshots keep their thresholds and now use
// this paid-order basis too.
func (s *InviteSvc) inviteePaidTotal(ctx context.Context, userID uuid.UUID) (decimal.Decimal, error) {
	var total decimal.Decimal
	err := s.d.DB().QueryRowContext(ctx, `
		SELECT COALESCE(SUM(GREATEST(o.amount - COALESCE((
		  SELECT SUM(r.amount) FROM site_refunds r
		  WHERE r.order_id = o.id AND r.status = 'approved'
		), 0), 0)), 0)
		FROM site_orders o
		WHERE o.user_id = $1
		  AND o.status IN ('paid', 'partially_refunded')
		  AND o.promotion_promoter_user_id IS NULL`, userID).Scan(&total)
	if err != nil {
		return decimal.Zero, err
	}
	if total.Sign() < 0 {
		return decimal.Zero, nil
	}
	return total, nil
}

// Risk flag labels persisted to invite_rewards.risk_flags /
// invite_rebates.risk_flags. Stable strings — the admin UI matches on them.
const (
	riskInviterNotActive       = "inviter_not_active"
	riskInviterFrozen          = "inviter_frozen"
	riskInviterTooNew          = "inviter_account_too_new"
	riskInviterEmailUnverified = "inviter_email_unverified"
	riskIPMatchesInviter       = "invitee_ip_matches_inviter"
	riskPayerMatchesInviter    = "invitee_payer_matches_inviter"
	riskSameIPDailyCap         = "same_ip_daily_cap_exceeded"
	riskDailyCountCap          = "daily_signup_count_cap_exceeded"
	riskDailyAmountCap         = "daily_signup_amount_cap_exceeded"
	riskAmountOverThreshold    = "amount_over_threshold"
)

// overReviewThreshold implements the per-payout manual-review amount
// threshold (ReviewAmountThreshold): 0 disables it; otherwise any single
// payout >= the threshold must be paid or rejected by a human. Unlike the
// AutoHoldOnRisk-gated flags this always holds — it is a money control,
// not a fraud heuristic — and it never reduces the amount.
func overReviewThreshold(program InviteProgramSettings, amount decimal.Decimal) bool {
	return program.ReviewAmountThreshold.Sign() > 0 &&
		amount.GreaterThanOrEqual(program.ReviewAmountThreshold)
}

// inviterRiskFlags are the inviter-account checks shared by signup rewards
// and consumption rebates.
func inviterRiskFlags(program InviteProgramSettings, inviter *ent.User, now time.Time) []string {
	var flags []string
	if inviter.Status != entuser.StatusActive {
		flags = append(flags, riskInviterNotActive)
	}
	if inviter.FrozenUntil != nil && inviter.FrozenUntil.After(now) {
		flags = append(flags, riskInviterFrozen)
	}
	if program.InviterMinAgeHours > 0 &&
		now.Sub(inviter.CreatedAt) < time.Duration(program.InviterMinAgeHours)*time.Hour {
		flags = append(flags, riskInviterTooNew)
	}
	if program.RequireInviterEmailVerified && !inviter.EmailVerified {
		flags = append(flags, riskInviterEmailUnverified)
	}
	return flags
}

// selfReferralFlags looks for evidence that invitee and inviter are the same
// person: a shared IP, or the same payment account on paid orders.
//
// IPs compared: the invitee's signup IP against the inviter's signup and
// latest login IP, plus the inviter's own binding IP when they were
// themselves invited (covers accounts registered before users.register_ip
// existed).
//
// Evidence is sticky: a self-referral flag already on one of the invitee's
// unpaid reward rows or held/rejected rebates is carried forward, so a later
// re-evaluation after the inviter's login IP changes cannot auto-release a
// payout that was held for review.
func selfReferralFlags(ctx context.Context, client *ent.Client, binding *ent.InviteBinding, inviter *ent.User) ([]string, error) {
	invitee, err := client.User.Get(ctx, binding.InviteeUserID)
	if err != nil {
		return nil, err
	}
	var flags []string

	inviterIPs := nonEmpty(inviter.RegisterIP, inviter.LastLoginIP)
	inviteeIPs := nonEmpty(binding.RegisterIP, invitee.RegisterIP)
	if len(inviteeIPs) > 0 {
		match := sharesAny(inviteeIPs, inviterIPs)
		if !match {
			match, err = client.InviteBinding.Query().
				Where(
					entbinding.InviteeUserIDEQ(inviter.ID),
					entbinding.RegisterIPIn(inviteeIPs...),
				).
				Exist(ctx)
			if err != nil {
				return nil, err
			}
		}
		if match {
			flags = append(flags, riskIPMatchesInviter)
		}
	}

	payers, err := client.SiteOrder.Query().
		Where(
			entsiteorder.UserIDEQ(invitee.ID),
			entsiteorder.PaymentPayerOpenidNEQ(""),
		).
		Select(entsiteorder.FieldPaymentPayerOpenid).
		Strings(ctx)
	if err != nil {
		return nil, err
	}
	// Alipay falls back to the masked login name ("159****5620") when it
	// returns no user id; masked values collide across people, so they are
	// not evidence.
	payers = slices.DeleteFunc(payers, func(p string) bool { return strings.Contains(p, "*") })
	if len(payers) > 0 {
		match, err := client.SiteOrder.Query().
			Where(
				entsiteorder.UserIDEQ(inviter.ID),
				entsiteorder.PaymentPayerOpenidIn(payers...),
			).
			Exist(ctx)
		if err != nil {
			return nil, err
		}
		if match {
			flags = append(flags, riskPayerMatchesInviter)
		}
	}

	rewards, err := client.InviteReward.Query().
		Where(
			entreward.InviteeUserIDEQ(invitee.ID),
			entreward.StatusIn(entreward.StatusPending, entreward.StatusQualified, entreward.StatusHeld, entreward.StatusRejected),
		).
		All(ctx)
	if err != nil {
		return nil, err
	}
	rebates, err := client.InviteRebate.Query().
		Where(
			entrebate.InviteeUserIDEQ(invitee.ID),
			entrebate.StatusIn(entrebate.StatusHeld, entrebate.StatusRejected),
		).
		All(ctx)
	if err != nil {
		return nil, err
	}
	var prior []string
	for _, r := range rewards {
		prior = append(prior, r.RiskFlags...)
	}
	for _, r := range rebates {
		prior = append(prior, r.RiskFlags...)
	}
	for _, f := range []string{riskIPMatchesInviter, riskPayerMatchesInviter} {
		if slices.Contains(prior, f) && !slices.Contains(flags, f) {
			flags = append(flags, f)
		}
	}
	return flags, nil
}

func nonEmpty(vals ...string) []string {
	out := make([]string, 0, len(vals))
	for _, v := range vals {
		if v != "" && !slices.Contains(out, v) {
			out = append(out, v)
		}
	}
	return out
}

func sharesAny(a, b []string) bool {
	for _, v := range a {
		if slices.Contains(b, v) {
			return true
		}
	}
	return false
}

func (s *InviteSvc) signupRiskFlags(ctx context.Context, client *ent.Client, program InviteProgramSettings, binding *ent.InviteBinding, reward *ent.InviteReward, inviter *ent.User) ([]string, error) {
	now := time.Now()
	flags := inviterRiskFlags(program, inviter, now)

	selfFlags, err := selfReferralFlags(ctx, client, binding, inviter)
	if err != nil {
		return nil, err
	}
	flags = append(flags, selfFlags...)

	if binding.RegisterIP != "" {
		// Same inviter + same IP crowding: the first SameIPDailyCap
		// registrations of a day pass, the overflow holds. Counted up to and
		// including this binding so re-evaluation is stable.
		if program.SameIPDailyCap > 0 {
			n, err := client.InviteBinding.Query().
				Where(
					entbinding.InviterUserIDEQ(binding.InviterUserID),
					entbinding.RegisterIPEQ(binding.RegisterIP),
					entbinding.CreatedAtGTE(startOfDay(binding.CreatedAt)),
					entbinding.CreatedAtLTE(binding.CreatedAt),
				).
				Count(ctx)
			if err != nil {
				return nil, err
			}
			if n > program.SameIPDailyCap {
				flags = append(flags, riskSameIPDailyCap)
			}
		}
	}

	// Daily payout caps: what has already been qualified/paid today, plus
	// this reward, must stay within the caps — the overflow holds.
	if program.SignupDailyCountCapPerInviter > 0 || program.SignupDailyAmountCapPerInviter.Sign() > 0 {
		count, amount, err := s.inviterSignupPayoutsToday(ctx, client, reward.InviterUserID, reward.ID, now)
		if err != nil {
			return nil, err
		}
		if program.SignupDailyCountCapPerInviter > 0 && count+1 > program.SignupDailyCountCapPerInviter {
			flags = append(flags, riskDailyCountCap)
		}
		if program.SignupDailyAmountCapPerInviter.Sign() > 0 &&
			amount.Add(reward.Amount).GreaterThan(program.SignupDailyAmountCapPerInviter) {
			flags = append(flags, riskDailyAmountCap)
		}
	}
	return flags, nil
}

// inviterSignupPayoutsToday counts today's already-committed signup payouts
// for an inviter: rewards paid today plus rewards currently in the
// (transient) qualified state, excluding the reward under evaluation.
func (s *InviteSvc) inviterSignupPayoutsToday(ctx context.Context, client *ent.Client, inviterID, excludeRewardID uuid.UUID, now time.Time) (int, decimal.Decimal, error) {
	dayStart := startOfDay(now)
	rows, err := client.InviteReward.Query().
		Where(
			entreward.InviterUserIDEQ(inviterID),
			entreward.IDNEQ(excludeRewardID),
			entreward.Or(
				entreward.And(entreward.StatusEQ(entreward.StatusPaid), entreward.PaidAtGTE(dayStart)),
				entreward.StatusEQ(entreward.StatusQualified),
			),
		).
		All(ctx)
	if err != nil {
		return 0, decimal.Zero, err
	}
	total := decimal.Zero
	for _, r := range rows {
		total = total.Add(r.Amount)
	}
	return len(rows), total, nil
}

// --- payouts ---

// PayReward credits a qualified/held/pending reward to the inviter.
// Idempotent: paying a paid reward returns it unchanged. Guards (voided
// binding, banned/rejected invitee, non-active inviter) return
// ErrInviteInvalidState and leave the row for admin review.
func (s *InviteSvc) PayReward(ctx context.Context, rewardID uuid.UUID, note string) (*ent.InviteReward, error) {
	tx, err := s.d.Ent.Tx(ctx)
	if err != nil {
		return nil, err
	}
	rollback := func(e error) (*ent.InviteReward, error) { _ = tx.Rollback(); return nil, e }
	reward, err := tx.InviteReward.Query().
		Where(entreward.IDEQ(rewardID)).
		ForUpdate().
		Only(ctx)
	if err != nil {
		_ = tx.Rollback()
		if ent.IsNotFound(err) {
			return nil, ErrInviteNotFound
		}
		return nil, err
	}
	if reward.Status == entreward.StatusPaid {
		_ = tx.Rollback()
		return reward, nil // already paid — idempotent success
	}
	if reward.Status != entreward.StatusPending &&
		reward.Status != entreward.StatusQualified &&
		reward.Status != entreward.StatusHeld {
		return rollback(fmt.Errorf("%w: reward is %s", ErrInviteInvalidState, reward.Status))
	}
	if reward.Amount.Sign() <= 0 {
		return rollback(fmt.Errorf("%w: reward amount is zero", ErrInviteInvalidState))
	}
	binding, err := tx.InviteBinding.Get(ctx, reward.BindingID)
	if err != nil {
		return rollback(err)
	}
	if binding.Status != entbinding.StatusBound {
		return rollback(fmt.Errorf("%w: binding voided", ErrInviteInvalidState))
	}
	invitee, err := tx.User.Query().
		Where(entuser.IDEQ(reward.InviteeUserID)).
		ForUpdate().
		Only(ctx)
	if err != nil {
		return rollback(err)
	}
	if invitee.Status == entuser.StatusBanned || invitee.Status == entuser.StatusRejected {
		return rollback(fmt.Errorf("%w: invitee is %s", ErrInviteInvalidState, invitee.Status))
	}
	hasAgent, err := hasAgentAttribution(ctx, tx.Client(), invitee.ID)
	if err != nil {
		return rollback(err)
	}
	if hasAgent {
		return rollback(fmt.Errorf("%w: invitee has agent attribution", ErrInviteInvalidState))
	}
	inviter, err := tx.User.Query().
		Where(entuser.IDEQ(reward.InviterUserID)).
		ForUpdate().
		Only(ctx)
	if err != nil {
		return rollback(err)
	}
	if inviter.Status != entuser.StatusActive {
		return rollback(fmt.Errorf("%w: inviter is %s", ErrInviteInvalidState, inviter.Status))
	}
	newBal := inviter.Balance.Add(reward.Amount)
	if _, err := tx.User.UpdateOneID(inviter.ID).SetBalance(newBal).SetCurrency(CurrencyCNY).Save(ctx); err != nil {
		return rollback(err)
	}
	btx, err := tx.BalanceTx.Create().
		SetUserID(inviter.ID).
		SetKind(entbalancetx.KindInviteReward).
		SetAmount(reward.Amount).
		SetBalanceAfter(newBal).
		SetCurrency(CurrencyCNY).
		SetNote("邀请注册奖励: " + maskEmail(invitee.Email)).
		Save(ctx)
	if err != nil {
		return rollback(err)
	}
	upd := tx.InviteReward.UpdateOneID(reward.ID).
		SetStatus(entreward.StatusPaid).
		SetPaidAt(time.Now()).
		SetPaidBalanceTxID(btx.ID)
	var snap inviteConditionSnapshot
	if reward.ConditionSnapshot != "" {
		if err := json.Unmarshal([]byte(reward.ConditionSnapshot), &snap); err != nil {
			return rollback(err)
		}
		if snap.Condition == InviteConditionAnyTopup || snap.Condition == InviteConditionMinTopup {
			snap.AmountBasis = "paid_orders"
			raw, err := json.Marshal(snap)
			if err != nil {
				return rollback(err)
			}
			upd.SetConditionSnapshot(string(raw))
		}
	}
	if note != "" {
		upd = upd.SetNote(note)
	}
	reward, err = upd.Save(ctx)
	if err != nil {
		return rollback(err)
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return reward, nil
}

// RejectReward marks an unpaid reward rejected. Paid rewards cannot be
// rejected (v1 has no clawback).
func (s *InviteSvc) RejectReward(ctx context.Context, rewardID uuid.UUID, note string) (*ent.InviteReward, error) {
	tx, err := s.d.Ent.Tx(ctx)
	if err != nil {
		return nil, err
	}
	rollback := func(e error) (*ent.InviteReward, error) { _ = tx.Rollback(); return nil, e }
	reward, err := tx.InviteReward.Query().
		Where(entreward.IDEQ(rewardID)).
		ForUpdate().
		Only(ctx)
	if err != nil {
		_ = tx.Rollback()
		if ent.IsNotFound(err) {
			return nil, ErrInviteNotFound
		}
		return nil, err
	}
	if reward.Status == entreward.StatusPaid || reward.Status == entreward.StatusVoided {
		return rollback(fmt.Errorf("%w: reward is %s", ErrInviteInvalidState, reward.Status))
	}
	if reward.Status == entreward.StatusRejected {
		_ = tx.Rollback()
		return reward, nil
	}
	upd := tx.InviteReward.UpdateOneID(rewardID).SetStatus(entreward.StatusRejected)
	if note != "" {
		upd = upd.SetNote(note)
	}
	reward, err = upd.Save(ctx)
	if err != nil {
		return rollback(err)
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return reward, nil
}

// --- email-verification release ---

// ReleaseEmailHoldsAsync runs ReleaseEmailHolds off the verification path.
// Nil-receiver safe.
func (s *InviteSvc) ReleaseEmailHoldsAsync(inviterID uuid.UUID) {
	if s == nil {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := s.ReleaseEmailHolds(ctx, inviterID); err != nil {
			slog.Warn("invite: email hold release failed", "inviter_id", inviterID.String(), "err", err.Error())
		}
	}()
}

// EmailHoldOnly reports whether flags hold a payout for nothing but the
// inviter's unverified email.
func EmailHoldOnly(flags []string) bool {
	return len(flags) == 1 && flags[0] == riskInviterEmailUnverified
}

// ReleaseEmailHolds re-checks the payouts held only because inviterID's
// email was unverified, once it is verified. Each goes back through the
// risk checks: a clean one is paid, anything else stays held with the
// flags that still apply.
func (s *InviteSvc) ReleaseEmailHolds(ctx context.Context, inviterID uuid.UUID) error {
	inviter, err := s.d.Ent.User.Get(ctx, inviterID)
	if err != nil {
		return err
	}
	if !inviter.EmailVerified || !s.program(ctx).Enabled {
		return nil
	}
	rewards, err := s.d.Ent.InviteReward.Query().
		Where(entreward.InviterUserIDEQ(inviterID), entreward.StatusEQ(entreward.StatusHeld)).
		All(ctx)
	if err != nil {
		return err
	}
	for _, r := range rewards {
		if EmailHoldOnly(r.RiskFlags) {
			if err := s.EvaluateSignup(ctx, r.InviteeUserID); err != nil {
				return err
			}
		}
	}
	rebates, err := s.d.Ent.InviteRebate.Query().
		Where(entrebate.InviterUserIDEQ(inviterID), entrebate.StatusEQ(entrebate.StatusHeld)).
		All(ctx)
	if err != nil {
		return err
	}
	for _, r := range rebates {
		if EmailHoldOnly(r.RiskFlags) {
			if err := s.releaseHeldRebate(ctx, r.ID); err != nil {
				return err
			}
		}
	}
	return nil
}

// releaseHeldRebate re-runs ProcessTopupRebate's hold decision on one
// email-held rebate and pays it when nothing holds it any more.
func (s *InviteSvc) releaseHeldRebate(ctx context.Context, rebateID uuid.UUID) error {
	program := s.program(ctx)
	tx, err := s.d.Ent.Tx(ctx)
	if err != nil {
		return err
	}
	rollback := func(e error) error { _ = tx.Rollback(); return e }
	rebate, err := tx.InviteRebate.Query().
		Where(entrebate.IDEQ(rebateID)).
		ForUpdate().
		Only(ctx)
	if err != nil {
		return rollback(err)
	}
	if rebate.Status != entrebate.StatusHeld || !EmailHoldOnly(rebate.RiskFlags) {
		_ = tx.Rollback()
		return nil
	}
	binding, err := tx.InviteBinding.Query().
		Where(entbinding.InviteeUserIDEQ(rebate.InviteeUserID)).
		Only(ctx)
	if err != nil {
		return rollback(err)
	}
	inviter, err := tx.User.Query().
		Where(entuser.IDEQ(rebate.InviterUserID)).
		ForUpdate().
		Only(ctx)
	if err != nil {
		return rollback(err)
	}
	flags := inviterRiskFlags(program, inviter, time.Now())
	selfFlags, err := selfReferralFlags(ctx, tx.Client(), binding, inviter)
	if err != nil {
		return rollback(err)
	}
	flags = append(flags, selfFlags...)
	hold := len(flags) > 0 && program.AutoHoldOnRisk
	if overReviewThreshold(program, rebate.RebateAmount) {
		flags = append(flags, riskAmountOverThreshold)
		hold = true
	}
	if hold {
		if _, err := tx.InviteRebate.UpdateOneID(rebate.ID).SetRiskFlags(flags).Save(ctx); err != nil {
			return rollback(err)
		}
		return tx.Commit()
	}
	_ = tx.Rollback()
	if _, err := s.PayRebate(ctx, rebateID, "auto payout after inviter email verified"); err != nil {
		if errors.Is(err, ErrInviteInvalidState) {
			return nil // guard held it back; row stays for admin review
		}
		return err
	}
	return nil
}

// --- topup rebate ---

// ProcessTopupRebateAsync runs ProcessTopupRebate off the order-payment
// path. Nil-receiver safe; never returns anything to the caller — a rebate
// failure must not fail (or slow) the topup that triggered it.
func (s *InviteSvc) ProcessTopupRebateAsync(inviteeID, sourceOrderID uuid.UUID, amount decimal.Decimal) {
	if s == nil {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := s.ProcessTopupRebate(ctx, inviteeID, sourceOrderID, amount); err != nil {
			slog.Warn("invite: topup rebate processing failed",
				"invitee_id", inviteeID.String(),
				"source_order_id", sourceOrderID.String(),
				"err", err.Error())
		}
	}()
}

// ProcessTopupRebate turns one PAID balance order into (at most) one inviter
// rebate. Idempotent on sourceOrderID: replays and duplicate callback
// deliveries hit the unique constraint and no-op. The row is inserted first
// and paid in a second transaction — if the payout fails the row survives as
// pending and the admin pay endpoint can retry it.
func (s *InviteSvc) ProcessTopupRebate(ctx context.Context, inviteeID, sourceOrderID uuid.UUID, amount decimal.Decimal) error {
	if sourceOrderID == uuid.Nil || amount.Sign() <= 0 {
		return nil
	}
	program := s.program(ctx)
	if !program.Enabled || !program.TopupRebateEnabled || program.TopupRebateRatePct.Sign() <= 0 {
		return nil
	}
	binding, err := s.d.Ent.InviteBinding.Query().
		Where(entbinding.InviteeUserIDEQ(inviteeID)).
		Only(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			return nil
		}
		return err
	}
	if binding.Status != entbinding.StatusBound {
		return nil
	}
	hasAgent, err := hasAgentAttribution(ctx, s.d.Ent, inviteeID)
	if err != nil {
		return err
	}
	if hasAgent {
		return nil
	}
	// One order pays at most one referrer: a coupon order already pays the
	// agent. Checked here rather than at the caller so every entry point
	// (payment, invite-backfill) gets it.
	couponOrder, err := s.d.Ent.SiteOrder.Query().
		Where(entsiteorder.IDEQ(sourceOrderID), entsiteorder.PromotionPromoterUserIDNotNil()).
		Exist(ctx)
	if err != nil {
		return err
	}
	if couponOrder {
		return nil
	}
	exclusive, err := hasExclusiveGroup(ctx, s.d.Ent, inviteeID)
	if err != nil {
		return err
	}
	if exclusive {
		return nil
	}
	if program.TopupRebateWindowDays > 0 &&
		time.Now().After(binding.CreatedAt.AddDate(0, 0, program.TopupRebateWindowDays)) {
		return nil
	}
	// Cheap idempotency pre-check; the unique constraint below is the
	// authoritative guard against races.
	exists, err := s.d.Ent.InviteRebate.Query().
		Where(entrebate.SourceOrderIDEQ(sourceOrderID)).
		Exist(ctx)
	if err != nil {
		return err
	}
	if exists {
		return nil
	}
	rebate := topupRebateAmount(amount, program.TopupRebateRatePct)
	if rebate.Sign() <= 0 {
		return nil
	}
	tx, err := s.d.Ent.Tx(ctx)
	if err != nil {
		return err
	}
	rollback := func(e error) error { _ = tx.Rollback(); return e }
	// Lock the binding against a concurrent void, then the inviter as the
	// serialization point shared by every rebate that can consume this
	// invitee's cap. All cap reads and the row insert happen while these
	// locks are held.
	binding, err = tx.InviteBinding.Query().
		Where(entbinding.IDEQ(binding.ID)).
		ForUpdate().
		Only(ctx)
	if err != nil {
		return rollback(err)
	}
	if binding.Status != entbinding.StatusBound {
		_ = tx.Rollback()
		return nil
	}
	// Serialize with agent-code redemption before the authoritative re-check.
	// This makes the attribution effective at transaction commit boundaries:
	// whichever side owns the user lock first is the earlier attribution.
	if _, err := tx.User.Query().
		Where(entuser.IDEQ(inviteeID)).
		ForUpdate().
		Only(ctx); err != nil {
		return rollback(err)
	}
	hasAgent, err = hasAgentAttribution(ctx, tx.Client(), inviteeID)
	if err != nil {
		return rollback(err)
	}
	if hasAgent {
		_ = tx.Rollback()
		return nil
	}
	inviter, err := tx.User.Query().
		Where(entuser.IDEQ(binding.InviterUserID)).
		ForUpdate().
		Only(ctx)
	if err != nil {
		return rollback(err)
	}
	exists, err = tx.InviteRebate.Query().
		Where(entrebate.SourceOrderIDEQ(sourceOrderID)).
		Exist(ctx)
	if err != nil {
		return rollback(err)
	}
	if exists {
		_ = tx.Rollback()
		return nil
	}
	// Per-invitee lifetime cap: pending+held+paid all count as committed,
	// net of anything already clawed back by a refund. The last slice is
	// clamped; later orders rebate nothing.
	if program.TopupRebateCapPerInvitee.Sign() > 0 {
		// Topup rows only: the retired consume rebates are historical rows and
		// must not eat a cap that was configured for this program.
		used, err := rebateCommittedSum(ctx, tx.InviteRebate.Query().
			Where(
				entrebate.InviteeUserIDEQ(inviteeID),
				entrebate.SourceKindEQ(entrebate.SourceKindTopup),
			))
		if err != nil {
			return rollback(err)
		}
		remaining := program.TopupRebateCapPerInvitee.Sub(used)
		if remaining.Sign() <= 0 {
			_ = tx.Rollback()
			return nil
		}
		if rebate.GreaterThan(remaining) {
			rebate = remaining
		}
	}
	flags := inviterRiskFlags(program, inviter, time.Now())
	selfFlags, err := selfReferralFlags(ctx, tx.Client(), binding, inviter)
	if err != nil {
		return rollback(err)
	}
	flags = append(flags, selfFlags...)
	status := entrebate.StatusPending
	if len(flags) > 0 && program.AutoHoldOnRisk {
		status = entrebate.StatusHeld
	}
	if overReviewThreshold(program, rebate) {
		flags = append(flags, riskAmountOverThreshold)
		status = entrebate.StatusHeld
	}
	row, err := tx.InviteRebate.Create().
		SetInviterUserID(binding.InviterUserID).
		SetInviteeUserID(inviteeID).
		SetSourceKind(entrebate.SourceKindTopup).
		SetSourceOrderID(sourceOrderID).
		SetSourceAmount(amount).
		SetRatePctSnapshot(program.TopupRebateRatePct).
		SetRebateAmount(rebate).
		SetCurrency(CurrencyCNY).
		SetStatus(status).
		SetRiskFlags(flags).
		Save(ctx)
	if err != nil {
		_ = tx.Rollback()
		if ent.IsConstraintError(err) {
			return nil // concurrent duplicate delivery — first writer wins
		}
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if status != entrebate.StatusPending {
		return nil
	}
	if _, err := s.PayRebate(ctx, row.ID, "auto payout"); err != nil {
		if errors.Is(err, ErrInviteInvalidState) {
			return nil // guard held it back; row stays for admin review
		}
		return err
	}
	return nil
}

// topupRebateAmount is rate% of the topup, truncated to whole cents.
// Truncation (not rounding) is deliberate: it can never pay out more than
// the configured rate, and a rebate the inviter sees in their ledger should
// be a clean cent figure rather than the 8-decimal residue the per-request
// consumption rebate used to produce.
func topupRebateAmount(amount, ratePct decimal.Decimal) decimal.Decimal {
	return amount.Mul(ratePct).Div(decimal.NewFromInt(100)).RoundDown(2)
}

// rebateCommittedSum sums the NET committed rebate (rebate_amount minus
// anything a refund has already taken back) over pending+held+paid rows
// matching the supplied transactional query. Two aggregates rather than one
// expression so it stays inside the caller's transaction without needing
// ent's raw-modifier feature.
func rebateCommittedSum(ctx context.Context, q *ent.InviteRebateQuery) (decimal.Decimal, error) {
	q = q.Where(entrebate.StatusIn(entrebate.StatusPending, entrebate.StatusHeld, entrebate.StatusPaid))
	gross, err := rebateDecimalSum(ctx, q.Clone(), entrebate.FieldRebateAmount)
	if err != nil {
		return decimal.Zero, err
	}
	clawed, err := rebateDecimalSum(ctx, q.Clone(), entrebate.FieldClawedBackAmount)
	if err != nil {
		return decimal.Zero, err
	}
	return gross.Sub(clawed), nil
}

func rebateDecimalSum(ctx context.Context, q *ent.InviteRebateQuery, field string) (decimal.Decimal, error) {
	var sums []struct {
		Sum decimal.NullDecimal `json:"sum"`
	}
	if err := q.Aggregate(ent.Sum(field)).Scan(ctx, &sums); err != nil {
		return decimal.Zero, err
	}
	if len(sums) == 0 || !sums[0].Sum.Valid {
		return decimal.Zero, nil
	}
	return sums[0].Sum.Decimal, nil
}

// --- refund reconciliation ---

// ReconcileRefund is the single invite-side hook for an approved refund on
// a balance or plan order. It runs after the refund has committed, so it never
// fails the refund itself; both halves are idempotent and derive their
// target from the order's CUMULATIVE approved refunds, which means a
// partial refund followed by another one lands on the right total rather
// than double-charging the inviter.
//
// refundedTotal is the sum of all approved refunds on the order.
func (s *InviteSvc) ReconcileRefund(ctx context.Context, inviteeID, orderID uuid.UUID, refundedTotal decimal.Decimal) {
	if s == nil {
		return
	}
	if err := s.clawbackTopupRebate(ctx, orderID, refundedTotal); err != nil {
		slog.Error("invite: topup rebate clawback failed",
			"order_id", orderID.String(), "err", err.Error())
	}
	if err := s.reverseSignupRewardIfUnqualified(ctx, inviteeID); err != nil {
		slog.Error("invite: signup reward re-check after refund failed",
			"invitee_id", inviteeID.String(), "err", err.Error())
	}
}

// clawbackTopupRebate reconciles the (at most one) rebate written for an
// order against how much of that order has been refunded.
//
//	entitled = rebate_amount * (source_amount - refunded) / source_amount
//
// An unpaid row is simply reduced to `entitled` (voided when that reaches
// zero). A paid row keeps its original rebate_amount — payout history is
// not rewritten — and instead accumulates clawed_back_amount, with the
// difference debited from the inviter in one balance_txes row.
//
// The inviter's balance MAY go negative: they were credited real money for
// a topup that has since been returned, and the debt is honest. Their next
// topup absorbs it.
func (s *InviteSvc) clawbackTopupRebate(ctx context.Context, orderID uuid.UUID, refundedTotal decimal.Decimal) error {
	if orderID == uuid.Nil || refundedTotal.Sign() <= 0 {
		return nil
	}
	tx, err := s.d.Ent.Tx(ctx)
	if err != nil {
		return err
	}
	rollback := func(e error) error { _ = tx.Rollback(); return e }
	rebate, err := tx.InviteRebate.Query().
		Where(entrebate.SourceOrderIDEQ(orderID)).
		ForUpdate().
		Only(ctx)
	if err != nil {
		_ = tx.Rollback()
		if ent.IsNotFound(err) {
			return nil // no rebate was ever written for this order
		}
		return err
	}
	if rebate.Status == entrebate.StatusRejected || rebate.Status == entrebate.StatusVoided {
		_ = tx.Rollback()
		return nil
	}
	// What the inviter is still owed: the snapshotted rate applied to the
	// SURVIVING topup, never a fraction of rebate_amount. Deriving it from
	// rebate_amount would be wrong twice over — a row clamped by the
	// per-invitee cap has rebate_amount < rate x source_amount, so scaling
	// that down again claws back money the cap says was earned; and it
	// would compound across successive partial refunds.
	entitled := decimal.Zero
	if remaining := rebate.SourceAmount.Sub(refundedTotal); remaining.Sign() > 0 {
		entitled = topupRebateAmount(remaining, rebate.RatePctSnapshot)
		if entitled.GreaterThan(rebate.RebateAmount) {
			entitled = rebate.RebateAmount
		}
	}
	// clawed_back_amount is the single accumulator for BOTH branches, so
	// rebate_amount stays the immutable payout snapshot and every replay
	// lands on the same target for a given cumulative refund total.
	target := rebate.RebateAmount.Sub(entitled)
	delta := target.Sub(rebate.ClawedBackAmount)
	if delta.Sign() <= 0 {
		_ = tx.Rollback()
		return nil // already reconciled to this refund total
	}

	if rebate.Status != entrebate.StatusPaid {
		// Never credited, so no money moves — PayRebate pays
		// rebate_amount minus what is recorded here.
		upd := tx.InviteRebate.UpdateOneID(rebate.ID).
			SetClawedBackAmount(target).
			SetNote(refundNote(rebate.Note, "订单退款，可发放返利调整为 "+entitled.String()))
		if entitled.Sign() <= 0 {
			upd = upd.SetStatus(entrebate.StatusVoided)
		}
		if _, err := upd.Save(ctx); err != nil {
			return rollback(err)
		}
		return tx.Commit()
	}

	inviter, err := tx.User.Query().
		Where(entuser.IDEQ(rebate.InviterUserID)).
		ForUpdate().
		Only(ctx)
	if err != nil {
		return rollback(err)
	}
	invitee, err := tx.User.Get(ctx, rebate.InviteeUserID)
	if err != nil {
		return rollback(err)
	}
	newBal := inviter.Balance.Sub(delta)
	if _, err := tx.User.UpdateOneID(inviter.ID).SetBalance(newBal).SetCurrency(CurrencyCNY).Save(ctx); err != nil {
		return rollback(err)
	}
	if _, err := tx.BalanceTx.Create().
		SetUserID(inviter.ID).
		SetKind(entbalancetx.KindInviteRebate).
		SetAmount(delta.Neg()).
		SetBalanceAfter(newBal).
		SetCurrency(CurrencyCNY).
		SetNote("邀请充值返利退回: " + maskEmail(invitee.Email)).
		Save(ctx); err != nil {
		return rollback(err)
	}
	if _, err := tx.InviteRebate.UpdateOneID(rebate.ID).
		SetClawedBackAmount(target).
		SetNote(refundNote(rebate.Note, "订单退款，已退回返利 "+target.String())).
		Save(ctx); err != nil {
		return rollback(err)
	}
	return tx.Commit()
}

// reverseSignupRewardIfUnqualified undoes a signup reward whose qualifying
// condition no longer holds once refunds are netted out of the invitee's
// topup total. Only the topup-based conditions can flip this way;
// email_verified and none are unaffected by a refund.
//
//	paid              -> voided, and the amount debited from the inviter.
//	                     Voided rather than returned to pending so a later
//	                     topup cannot silently re-pay it; paid_balance_tx_id
//	                     stays on the row as evidence of what was reversed.
//	qualified / held   -> back to pending. No money moved, so there is
//	                     nothing to reverse — but leaving it payable would
//	                     let an admin hit "发放" on a reward the invitee
//	                     refunded their way out of (PayReward does not
//	                     re-check the condition).
func (s *InviteSvc) reverseSignupRewardIfUnqualified(ctx context.Context, inviteeID uuid.UUID) error {
	binding, err := s.d.Ent.InviteBinding.Query().
		Where(entbinding.InviteeUserIDEQ(inviteeID)).
		Only(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			return nil
		}
		return err
	}
	reward, err := s.d.Ent.InviteReward.Query().
		Where(entreward.BindingIDEQ(binding.ID)).
		Only(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			return nil
		}
		return err
	}
	if reward.Status != entreward.StatusPaid &&
		reward.Status != entreward.StatusQualified &&
		reward.Status != entreward.StatusHeld {
		return nil
	}
	if reward.Amount.Sign() <= 0 {
		return nil
	}
	var snap inviteConditionSnapshot
	if reward.ConditionSnapshot != "" {
		_ = json.Unmarshal([]byte(reward.ConditionSnapshot), &snap)
	}
	if snap.Condition != InviteConditionAnyTopup && snap.Condition != InviteConditionMinTopup {
		return nil
	}
	stillMet, err := s.signupConditionMet(ctx, reward)
	if err != nil {
		return err
	}
	if stillMet {
		return nil
	}

	tx, err := s.d.Ent.Tx(ctx)
	if err != nil {
		return err
	}
	rollback := func(e error) error { _ = tx.Rollback(); return e }
	reward, err = tx.InviteReward.Query().
		Where(entreward.IDEQ(reward.ID)).
		ForUpdate().
		Only(ctx)
	if err != nil {
		return rollback(err)
	}
	if reward.Status == entreward.StatusQualified || reward.Status == entreward.StatusHeld {
		// Nothing was credited: just make it unpayable again.
		if _, err := tx.InviteReward.UpdateOneID(reward.ID).
			SetStatus(entreward.StatusPending).
			SetNote(refundNote(reward.Note, "订单退款后不再满足达标条件，已退回待达标")).
			Save(ctx); err != nil {
			return rollback(err)
		}
		return tx.Commit()
	}
	if reward.Status != entreward.StatusPaid {
		_ = tx.Rollback()
		return nil // a concurrent reconcile already reversed it
	}
	inviter, err := tx.User.Query().
		Where(entuser.IDEQ(reward.InviterUserID)).
		ForUpdate().
		Only(ctx)
	if err != nil {
		return rollback(err)
	}
	invitee, err := tx.User.Get(ctx, reward.InviteeUserID)
	if err != nil {
		return rollback(err)
	}
	newBal := inviter.Balance.Sub(reward.Amount)
	if _, err := tx.User.UpdateOneID(inviter.ID).SetBalance(newBal).SetCurrency(CurrencyCNY).Save(ctx); err != nil {
		return rollback(err)
	}
	if _, err := tx.BalanceTx.Create().
		SetUserID(inviter.ID).
		SetKind(entbalancetx.KindInviteReward).
		SetAmount(reward.Amount.Neg()).
		SetBalanceAfter(newBal).
		SetCurrency(CurrencyCNY).
		SetNote("邀请注册奖励退回: " + maskEmail(invitee.Email)).
		Save(ctx); err != nil {
		return rollback(err)
	}
	if _, err := tx.InviteReward.UpdateOneID(reward.ID).
		SetStatus(entreward.StatusVoided).
		SetNote(refundNote(reward.Note, "订单退款后不再满足达标条件，奖励已退回")).
		Save(ctx); err != nil {
		return rollback(err)
	}
	return tx.Commit()
}

// refundNote appends a reconciliation line to an existing note without
// losing what an operator wrote earlier.
func refundNote(existing, add string) string {
	existing = strings.TrimSpace(existing)
	if existing == "" {
		return add
	}
	return existing + "\n" + add
}

// PayRebate credits a pending/held rebate to the inviter. Same idempotency
// and guard semantics as PayReward.
func (s *InviteSvc) PayRebate(ctx context.Context, rebateID uuid.UUID, note string) (*ent.InviteRebate, error) {
	tx, err := s.d.Ent.Tx(ctx)
	if err != nil {
		return nil, err
	}
	rollback := func(e error) (*ent.InviteRebate, error) { _ = tx.Rollback(); return nil, e }
	rebate, err := tx.InviteRebate.Query().
		Where(entrebate.IDEQ(rebateID)).
		ForUpdate().
		Only(ctx)
	if err != nil {
		_ = tx.Rollback()
		if ent.IsNotFound(err) {
			return nil, ErrInviteNotFound
		}
		return nil, err
	}
	if rebate.Status == entrebate.StatusPaid {
		_ = tx.Rollback()
		return rebate, nil // idempotent success
	}
	if rebate.Status != entrebate.StatusPending && rebate.Status != entrebate.StatusHeld {
		return rollback(fmt.Errorf("%w: rebate is %s", ErrInviteInvalidState, rebate.Status))
	}
	// A refund may have reduced this row before anyone paid it; credit
	// only what survives.
	payable := rebate.RebateAmount.Sub(rebate.ClawedBackAmount)
	if payable.Sign() <= 0 {
		return rollback(fmt.Errorf("%w: rebate amount is zero", ErrInviteInvalidState))
	}
	binding, err := tx.InviteBinding.Query().
		Where(entbinding.InviteeUserIDEQ(rebate.InviteeUserID)).
		Only(ctx)
	if err != nil {
		return rollback(err)
	}
	if binding.Status != entbinding.StatusBound {
		return rollback(fmt.Errorf("%w: binding voided", ErrInviteInvalidState))
	}
	invitee, err := tx.User.Query().
		Where(entuser.IDEQ(rebate.InviteeUserID)).
		ForUpdate().
		Only(ctx)
	if err != nil {
		return rollback(err)
	}
	if invitee.Status == entuser.StatusBanned || invitee.Status == entuser.StatusRejected {
		return rollback(fmt.Errorf("%w: invitee is %s", ErrInviteInvalidState, invitee.Status))
	}
	hasAgent, err := hasAgentAttribution(ctx, tx.Client(), invitee.ID)
	if err != nil {
		return rollback(err)
	}
	if hasAgent {
		return rollback(fmt.Errorf("%w: invitee has agent attribution", ErrInviteInvalidState))
	}
	inviter, err := tx.User.Query().
		Where(entuser.IDEQ(rebate.InviterUserID)).
		ForUpdate().
		Only(ctx)
	if err != nil {
		return rollback(err)
	}
	if inviter.Status != entuser.StatusActive {
		return rollback(fmt.Errorf("%w: inviter is %s", ErrInviteInvalidState, inviter.Status))
	}
	newBal := inviter.Balance.Add(payable)
	if _, err := tx.User.UpdateOneID(inviter.ID).SetBalance(newBal).SetCurrency(CurrencyCNY).Save(ctx); err != nil {
		return rollback(err)
	}
	label := "邀请充值返利: "
	if rebate.SourceKind == entrebate.SourceKindConsume {
		label = "邀请消费返利: "
	}
	btxc := tx.BalanceTx.Create().
		SetUserID(inviter.ID).
		SetKind(entbalancetx.KindInviteRebate).
		SetAmount(payable).
		SetBalanceAfter(newBal).
		SetCurrency(CurrencyCNY).
		SetNote(label + maskEmail(invitee.Email))
	// Retired consume rebates point at a usage log; topup rebates have no
	// usage log to point at (their source is a site order).
	if rebate.SourceUsageLogID != nil {
		btxc = btxc.SetRelatedUsageLogID(*rebate.SourceUsageLogID)
	}
	btx, err := btxc.Save(ctx)
	if err != nil {
		return rollback(err)
	}
	upd := tx.InviteRebate.UpdateOneID(rebate.ID).
		SetStatus(entrebate.StatusPaid).
		SetPaidAt(time.Now()).
		SetPaidBalanceTxID(btx.ID)
	if note != "" {
		upd = upd.SetNote(note)
	}
	rebate, err = upd.Save(ctx)
	if err != nil {
		return rollback(err)
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return rebate, nil
}

// RejectRebate marks an unpaid rebate rejected.
func (s *InviteSvc) RejectRebate(ctx context.Context, rebateID uuid.UUID, note string) (*ent.InviteRebate, error) {
	tx, err := s.d.Ent.Tx(ctx)
	if err != nil {
		return nil, err
	}
	rollback := func(e error) (*ent.InviteRebate, error) { _ = tx.Rollback(); return nil, e }
	rebate, err := tx.InviteRebate.Query().
		Where(entrebate.IDEQ(rebateID)).
		ForUpdate().
		Only(ctx)
	if err != nil {
		_ = tx.Rollback()
		if ent.IsNotFound(err) {
			return nil, ErrInviteNotFound
		}
		return nil, err
	}
	if rebate.Status == entrebate.StatusPaid || rebate.Status == entrebate.StatusVoided {
		return rollback(fmt.Errorf("%w: rebate is %s", ErrInviteInvalidState, rebate.Status))
	}
	if rebate.Status == entrebate.StatusRejected {
		_ = tx.Rollback()
		return rebate, nil
	}
	upd := tx.InviteRebate.UpdateOneID(rebateID).SetStatus(entrebate.StatusRejected)
	if note != "" {
		upd = upd.SetNote(note)
	}
	rebate, err = upd.Save(ctx)
	if err != nil {
		return rollback(err)
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return rebate, nil
}

// hasAgentAttribution reports whether the user has completed a redemption of
// an agent-issued code. Main currently has no users.agent_id/customer-binding
// column; the immutable redeem_records -> redeem_codes.agent_id relation is
// therefore the authoritative evidence that this customer produced an agent
// sale and belongs to the agent channel. Code/profile status changes after the
// sale do not erase that attribution.
func hasAgentAttribution(ctx context.Context, client *ent.Client, userID uuid.UUID) (bool, error) {
	return client.RedeemRecord.Query().
		Where(
			entredeemrecord.UserIDEQ(userID),
			entredeemrecord.HasCodeWith(entredeemcode.AgentIDNotNil()),
		).
		Exist(ctx)
}

// VoidBinding voids a binding and every unpaid reward/rebate under it. Paid
// amounts are NOT clawed back (v1 contract); future spend by the invitee
// stops rebating because ProcessRebate checks binding status.
func (s *InviteSvc) VoidBinding(ctx context.Context, bindingID uuid.UUID, note string) (*ent.InviteBinding, error) {
	tx, err := s.d.Ent.Tx(ctx)
	if err != nil {
		return nil, err
	}
	rollback := func(e error) (*ent.InviteBinding, error) { _ = tx.Rollback(); return nil, e }
	binding, err := tx.InviteBinding.Query().
		Where(entbinding.IDEQ(bindingID)).
		ForUpdate().
		Only(ctx)
	if err != nil {
		_ = tx.Rollback()
		if ent.IsNotFound(err) {
			return nil, ErrInviteNotFound
		}
		return nil, err
	}
	if binding.Status == entbinding.StatusVoided {
		_ = tx.Rollback()
		return binding, nil // idempotent
	}
	binding, err = tx.InviteBinding.UpdateOneID(bindingID).
		SetStatus(entbinding.StatusVoided).
		Save(ctx)
	if err != nil {
		return rollback(err)
	}
	rewardUpd := tx.InviteReward.Update().
		Where(
			entreward.BindingIDEQ(bindingID),
			entreward.StatusIn(entreward.StatusPending, entreward.StatusQualified, entreward.StatusHeld, entreward.StatusRejected),
		).
		SetStatus(entreward.StatusVoided)
	if note != "" {
		rewardUpd = rewardUpd.SetNote(note)
	}
	if _, err := rewardUpd.Save(ctx); err != nil {
		return rollback(err)
	}
	rebateUpd := tx.InviteRebate.Update().
		Where(
			entrebate.InviteeUserIDEQ(binding.InviteeUserID),
			entrebate.StatusIn(entrebate.StatusPending, entrebate.StatusHeld, entrebate.StatusRejected),
		).
		SetStatus(entrebate.StatusVoided)
	if note != "" {
		rebateUpd = rebateUpd.SetNote(note)
	}
	if _, err := rebateUpd.Save(ctx); err != nil {
		return rollback(err)
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return binding, nil
}

// --- user-facing dashboard (/v1/me/invite) ---

type MyInviteStats struct {
	InvitedCount     int             `json:"invited_count"`
	QualifiedCount   int             `json:"qualified_count"`
	PendingCount     int             `json:"pending_count"`
	HeldCount        int             `json:"held_count"`
	SignupRewardPaid decimal.Decimal `json:"signup_reward_paid"`
	// TopupRebatePaid is the NET rebate credited to this inviter: paid
	// amounts minus anything a refund has taken back. It spans both the
	// live topup rebates and the retired consume rebates, so a long-time
	// inviter's total does not appear to drop when the program changed.
	TopupRebatePaid decimal.Decimal `json:"topup_rebate_paid"`
}

type MyInvitee struct {
	UserID                          uuid.UUID       `json:"user_id"`
	EmailMasked                     string          `json:"email_masked"`
	CreatedAt                       time.Time       `json:"created_at"`
	SignupRewardStatus              string          `json:"signup_reward_status"`
	SignupRewardAmount              decimal.Decimal `json:"signup_reward_amount"`
	RebatePaid                      decimal.Decimal `json:"rebate_paid"`
	SignupCondition                 string          `json:"signup_condition"`
	SignupPaidAmount                decimal.Decimal `json:"signup_paid_amount"`
	SignupRequiredAmount            decimal.Decimal `json:"signup_required_amount"`
	SignupEmailVerified             bool            `json:"signup_email_verified"`
	SignupEmailVerificationRequired bool            `json:"signup_email_verification_required"`
	RebateHeld                      decimal.Decimal `json:"rebate_held"`
	RebateEmailVerificationRequired bool            `json:"rebate_email_verification_required"`
	// Why held payouts are held, as heldReasons codes.
	SignupRewardHeldReasons []string `json:"signup_reward_held_reasons"`
	RebateHeldReasons       []string `json:"rebate_held_reasons"`
}

// heldReasons turns risk flags into the reason codes the inviter sees.
// The self-referral signals collapse into one code so the page does not
// say which check matched.
func heldReasons(dst []string, flags []string) []string {
	for _, f := range flags {
		var code string
		switch f {
		case riskInviterEmailUnverified:
			code = "inviter_email_unverified"
		case riskInviterTooNew:
			code = "inviter_account_too_new"
		case riskInviterNotActive, riskInviterFrozen:
			code = "inviter_account_restricted"
		case riskIPMatchesInviter, riskPayerMatchesInviter:
			code = "self_referral_suspected"
		case riskSameIPDailyCap, riskDailyCountCap, riskDailyAmountCap:
			code = "daily_limit"
		case riskAmountOverThreshold:
			code = "amount_review"
		default:
			continue
		}
		if !slices.Contains(dst, code) {
			dst = append(dst, code)
		}
	}
	return dst
}

type MyInviteReward struct {
	ID          uuid.UUID       `json:"id"`
	EmailMasked string          `json:"email_masked"`
	Amount      decimal.Decimal `json:"amount"`
	Currency    string          `json:"currency"`
	Status      string          `json:"status"`
	CreatedAt   time.Time       `json:"created_at"`
	PaidAt      *time.Time      `json:"paid_at,omitempty"`
}

type MyInviteRebate struct {
	ID          uuid.UUID `json:"id"`
	EmailMasked string    `json:"email_masked"`
	// SourceKind is "topup" for the live program and "consume" for the
	// retired per-request rebates that predate 2026-09-03.
	SourceKind       string          `json:"source_kind"`
	SourceAmount     decimal.Decimal `json:"source_amount"`
	RebateAmount     decimal.Decimal `json:"rebate_amount"`
	ClawedBackAmount decimal.Decimal `json:"clawed_back_amount"`
	Currency         string          `json:"currency"`
	Status           string          `json:"status"`
	CreatedAt        time.Time       `json:"created_at"`
	PaidAt           *time.Time      `json:"paid_at,omitempty"`
}

type MyInviteView struct {
	InviteCode                       string              `json:"invite_code"`
	InviteURL                        string              `json:"invite_url"`
	Program                          InviteProgramPublic `json:"program"`
	Stats                            MyInviteStats       `json:"stats"`
	Invitees                         []MyInvitee         `json:"invitees"`
	Rewards                          []MyInviteReward    `json:"rewards"`
	Rebates                          []MyInviteRebate    `json:"rebates"`
	Total                            int                 `json:"total"`
	Limit                            int                 `json:"limit"`
	Offset                           int                 `json:"offset"`
	InviterEmailVerificationRequired bool                `json:"inviter_email_verification_required"`
}

// MyInvite assembles the whole self-service invite dashboard in one call.
// limit/offset page the invitees list (and bound the rewards/rebates lists);
// Total is the invitee total.
func (s *InviteSvc) MyInvite(ctx context.Context, userID uuid.UUID, limit, offset int) (*MyInviteView, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	if offset < 0 {
		offset = 0
	}
	code, err := s.EnsureInviteCode(ctx, userID)
	if err != nil {
		return nil, err
	}
	st, err := s.settings.GetSite(ctx)
	if err != nil {
		return nil, err
	}
	program := DefaultInviteProgram()
	if st.InviteProgram != nil {
		program = *st.InviteProgram
	}
	inviter, err := s.d.Ent.User.Get(ctx, userID)
	if err != nil {
		return nil, err
	}
	view := &MyInviteView{
		InviteCode: code,
		InviteURL:  inviteURL(st.SiteURL, code),
		Program:    program.Public(),
		Stats: MyInviteStats{
			SignupRewardPaid: decimal.Zero,
			TopupRebatePaid:  decimal.Zero,
		},
		Invitees: []MyInvitee{},
		Rewards:  []MyInviteReward{},
		Rebates:  []MyInviteRebate{},
		Limit:    limit,
		Offset:   offset,
		InviterEmailVerificationRequired: program.Enabled && program.AutoHoldOnRisk &&
			program.RequireInviterEmailVerified && !inviter.EmailVerified,
	}

	base := s.d.Ent.InviteBinding.Query().
		Where(entbinding.InviterUserIDEQ(userID), entbinding.StatusEQ(entbinding.StatusBound))
	total, err := base.Clone().Count(ctx)
	if err != nil {
		return nil, err
	}
	view.Stats.InvitedCount = total
	view.Total = total
	bindings, err := base.
		Order(ent.Desc(entbinding.FieldCreatedAt)).
		Limit(limit).Offset(offset).
		All(ctx)
	if err != nil {
		return nil, err
	}

	// Reward counters come from a grouped aggregate rather than loading
	// every reward row: an inviter with thousands of invitees would
	// otherwise pull all of them into memory to compute five numbers.
	rewardStatRows, err := s.d.DB().QueryContext(ctx, `
		SELECT status, COUNT(*), COALESCE(SUM(amount), 0)
		FROM invite_rewards
		WHERE inviter_user_id = $1
		GROUP BY status`, userID)
	if err != nil {
		return nil, err
	}
	for rewardStatRows.Next() {
		var status string
		var count int
		var sum decimal.Decimal
		if err := rewardStatRows.Scan(&status, &count, &sum); err != nil {
			return nil, err
		}
		switch entreward.Status(status) {
		case entreward.StatusPending:
			view.Stats.PendingCount += count
		case entreward.StatusHeld:
			view.Stats.HeldCount += count
		case entreward.StatusQualified:
			view.Stats.QualifiedCount += count
		case entreward.StatusPaid:
			view.Stats.QualifiedCount += count
			view.Stats.SignupRewardPaid = view.Stats.SignupRewardPaid.Add(sum)
		}
	}
	if err := rewardStatRows.Err(); err != nil {
		_ = rewardStatRows.Close()
		return nil, err
	}
	// Closed eagerly: MyInvite runs several more queries below and must not
	// pin a pooled connection for the whole call.
	if err := rewardStatRows.Close(); err != nil {
		return nil, err
	}
	if err := s.d.DB().QueryRowContext(ctx, `
		SELECT COALESCE(SUM(rebate_amount - clawed_back_amount), 0)
		FROM invite_rebates
		WHERE inviter_user_id = $1 AND status = 'paid'`, userID).
		Scan(&view.Stats.TopupRebatePaid); err != nil {
		return nil, err
	}
	// Rewards for the invitees on THIS page only.
	bindingIDs := make([]uuid.UUID, 0, len(bindings))
	for _, b := range bindings {
		bindingIDs = append(bindingIDs, b.ID)
	}
	rewardByBinding := make(map[uuid.UUID]*ent.InviteReward, len(bindingIDs))
	if len(bindingIDs) > 0 {
		pageRewards, err := s.d.Ent.InviteReward.Query().
			Where(entreward.BindingIDIn(bindingIDs...)).
			All(ctx)
		if err != nil {
			return nil, err
		}
		for _, r := range pageRewards {
			rewardByBinding[r.BindingID] = r
		}
	}

	// Emails + per-invitee paid rebate sums for the current page only.
	inviteeIDs := make([]uuid.UUID, 0, len(bindings))
	for _, b := range bindings {
		inviteeIDs = append(inviteeIDs, b.InviteeUserID)
	}
	emailByID := map[uuid.UUID]string{}
	emailVerifiedByID := map[uuid.UUID]bool{}
	rebatePaidByInvitee := map[uuid.UUID]decimal.Decimal{}
	rebateHeldByInvitee := map[uuid.UUID]decimal.Decimal{}
	rebateEmailRequiredByInvitee := map[uuid.UUID]bool{}
	rebateReasonsByInvitee := map[uuid.UUID][]string{}
	if len(inviteeIDs) > 0 {
		users, err := s.d.Ent.User.Query().Where(entuser.IDIn(inviteeIDs...)).All(ctx)
		if err != nil {
			return nil, err
		}
		for _, u := range users {
			emailByID[u.ID] = u.Email
			emailVerifiedByID[u.ID] = u.EmailVerified
		}
		rows, err := s.d.Ent.InviteRebate.Query().
			Where(
				entrebate.InviterUserIDEQ(userID),
				entrebate.InviteeUserIDIn(inviteeIDs...),
				entrebate.StatusIn(entrebate.StatusPaid, entrebate.StatusHeld),
			).
			All(ctx)
		if err != nil {
			return nil, err
		}
		for _, r := range rows {
			net := r.RebateAmount.Sub(r.ClawedBackAmount)
			if r.Status == entrebate.StatusPaid {
				rebatePaidByInvitee[r.InviteeUserID] = rebatePaidByInvitee[r.InviteeUserID].Add(net)
			} else {
				rebateHeldByInvitee[r.InviteeUserID] = rebateHeldByInvitee[r.InviteeUserID].Add(net)
				rebateReasonsByInvitee[r.InviteeUserID] = heldReasons(rebateReasonsByInvitee[r.InviteeUserID], r.RiskFlags)
				if !inviter.EmailVerified && slices.Contains(r.RiskFlags, riskInviterEmailUnverified) {
					rebateEmailRequiredByInvitee[r.InviteeUserID] = true
				}
			}
		}
	}
	for _, b := range bindings {
		item := MyInvitee{
			UserID:                          b.InviteeUserID,
			EmailMasked:                     maskEmail(emailByID[b.InviteeUserID]),
			CreatedAt:                       b.CreatedAt,
			SignupRewardStatus:              "none",
			SignupRewardAmount:              decimal.Zero,
			RebatePaid:                      rebatePaidByInvitee[b.InviteeUserID],
			RebateHeld:                      rebateHeldByInvitee[b.InviteeUserID],
			RebateEmailVerificationRequired: rebateEmailRequiredByInvitee[b.InviteeUserID],
			SignupRewardHeldReasons:         []string{},
			RebateHeldReasons:               append([]string{}, rebateReasonsByInvitee[b.InviteeUserID]...),
			SignupPaidAmount:                decimal.Zero,
			SignupRequiredAmount:            decimal.Zero,
			SignupEmailVerified:             emailVerifiedByID[b.InviteeUserID],
		}
		if r, ok := rewardByBinding[b.ID]; ok {
			item.SignupRewardStatus = string(r.Status)
			item.SignupRewardAmount = r.Amount
			snap := inviteConditionSnapshot{Condition: InviteConditionEmailVerified}
			if r.ConditionSnapshot != "" {
				if err := json.Unmarshal([]byte(r.ConditionSnapshot), &snap); err != nil {
					return nil, fmt.Errorf("invite reward condition: %w", err)
				}
			}
			item.SignupCondition = snap.Condition
			if snap.Condition == InviteConditionAnyTopup || snap.Condition == InviteConditionMinTopup {
				item.SignupPaidAmount, err = s.inviteePaidTotal(ctx, b.InviteeUserID)
				if err != nil {
					return nil, err
				}
				item.SignupRequiredAmount = snap.MinTopupAmount
			}
			item.SignupEmailVerificationRequired = !inviter.EmailVerified && r.Status == entreward.StatusHeld &&
				slices.Contains(r.RiskFlags, riskInviterEmailUnverified)
			if r.Status == entreward.StatusHeld {
				item.SignupRewardHeldReasons = heldReasons(item.SignupRewardHeldReasons, r.RiskFlags)
			}
		}
		view.Invitees = append(view.Invitees, item)
	}

	// Recent rewards/rebates, bounded by the same page size. Extra email
	// lookups cover invitees outside the current bindings page.
	extraIDs := map[uuid.UUID]bool{}
	rewardsPage, err := s.d.Ent.InviteReward.Query().
		Where(entreward.InviterUserIDEQ(userID)).
		Order(ent.Desc(entreward.FieldCreatedAt)).
		Limit(limit).Offset(offset).
		All(ctx)
	if err != nil {
		return nil, err
	}
	for _, r := range rewardsPage {
		if _, ok := emailByID[r.InviteeUserID]; !ok {
			extraIDs[r.InviteeUserID] = true
		}
	}
	rebateRows, err := s.d.Ent.InviteRebate.Query().
		Where(entrebate.InviterUserIDEQ(userID)).
		Order(ent.Desc(entrebate.FieldCreatedAt)).
		Limit(limit).Offset(offset).
		All(ctx)
	if err != nil {
		return nil, err
	}
	for _, r := range rebateRows {
		if _, ok := emailByID[r.InviteeUserID]; !ok {
			extraIDs[r.InviteeUserID] = true
		}
	}
	if len(extraIDs) > 0 {
		ids := make([]uuid.UUID, 0, len(extraIDs))
		for id := range extraIDs {
			ids = append(ids, id)
		}
		users, err := s.d.Ent.User.Query().Where(entuser.IDIn(ids...)).All(ctx)
		if err != nil {
			return nil, err
		}
		for _, u := range users {
			emailByID[u.ID] = u.Email
		}
	}
	for _, r := range rewardsPage {
		view.Rewards = append(view.Rewards, MyInviteReward{
			ID:          r.ID,
			EmailMasked: maskEmail(emailByID[r.InviteeUserID]),
			Amount:      r.Amount,
			Currency:    r.Currency,
			Status:      string(r.Status),
			CreatedAt:   r.CreatedAt,
			PaidAt:      r.PaidAt,
		})
	}
	for _, r := range rebateRows {
		view.Rebates = append(view.Rebates, MyInviteRebate{
			ID:               r.ID,
			EmailMasked:      maskEmail(emailByID[r.InviteeUserID]),
			SourceKind:       string(r.SourceKind),
			SourceAmount:     r.SourceAmount,
			RebateAmount:     r.RebateAmount,
			ClawedBackAmount: r.ClawedBackAmount,
			Currency:         r.Currency,
			Status:           string(r.Status),
			CreatedAt:        r.CreatedAt,
			PaidAt:           r.PaidAt,
		})
	}
	return view, nil
}

func inviteURL(siteURL, code string) string {
	base := strings.TrimRight(strings.TrimSpace(siteURL), "/")
	return base + "/register?code=" + code
}

// maskEmail keeps the first rune and the domain: a***@example.com. Never
// returns the raw address — invite views show OTHER people's emails.
func maskEmail(email string) string {
	at := strings.Index(email, "@")
	if at <= 0 {
		return "***"
	}
	return email[:1] + "***" + email[at:]
}

// --- admin listing ---

type InviteAdminFilter struct {
	Q         string // bindings only: matches invite code / register IP
	Status    string
	InviterID *uuid.UUID
	InviteeID *uuid.UUID
	Limit     int
	Offset    int
}

func normalizeInviteFilter(f InviteAdminFilter) InviteAdminFilter {
	if f.Limit <= 0 || f.Limit > 200 {
		f.Limit = 50
	}
	if f.Offset < 0 {
		f.Offset = 0
	}
	return f
}

type InviteAdminBinding struct {
	Binding      *ent.InviteBinding
	InviterEmail string
	InviteeEmail string
}

func (s *InviteSvc) ListBindings(ctx context.Context, f InviteAdminFilter) ([]InviteAdminBinding, int, error) {
	f = normalizeInviteFilter(f)
	q := s.d.Ent.InviteBinding.Query()
	if f.Status != "" {
		q = q.Where(entbinding.StatusEQ(entbinding.Status(f.Status)))
	}
	if f.InviterID != nil {
		q = q.Where(entbinding.InviterUserIDEQ(*f.InviterID))
	}
	if f.InviteeID != nil {
		q = q.Where(entbinding.InviteeUserIDEQ(*f.InviteeID))
	}
	if term := strings.TrimSpace(f.Q); term != "" {
		q = q.Where(entbinding.Or(
			entbinding.InviteCodeUsedContainsFold(term),
			entbinding.RegisterIPContainsFold(term),
		))
	}
	total, err := q.Clone().Count(ctx)
	if err != nil {
		return nil, 0, err
	}
	rows, err := q.Order(ent.Desc(entbinding.FieldCreatedAt)).
		Limit(f.Limit).Offset(f.Offset).
		All(ctx)
	if err != nil {
		return nil, 0, err
	}
	ids := make([]uuid.UUID, 0, len(rows)*2)
	for _, b := range rows {
		ids = append(ids, b.InviterUserID, b.InviteeUserID)
	}
	emails, err := s.emailsByID(ctx, ids)
	if err != nil {
		return nil, 0, err
	}
	out := make([]InviteAdminBinding, 0, len(rows))
	for _, b := range rows {
		out = append(out, InviteAdminBinding{
			Binding:      b,
			InviterEmail: emails[b.InviterUserID],
			InviteeEmail: emails[b.InviteeUserID],
		})
	}
	return out, total, nil
}

type InviteAdminReward struct {
	Reward       *ent.InviteReward
	InviterEmail string
	InviteeEmail string
}

func (s *InviteSvc) ListRewards(ctx context.Context, f InviteAdminFilter) ([]InviteAdminReward, int, error) {
	f = normalizeInviteFilter(f)
	q := s.d.Ent.InviteReward.Query()
	if f.Status != "" {
		q = q.Where(entreward.StatusEQ(entreward.Status(f.Status)))
	}
	if f.InviterID != nil {
		q = q.Where(entreward.InviterUserIDEQ(*f.InviterID))
	}
	if f.InviteeID != nil {
		q = q.Where(entreward.InviteeUserIDEQ(*f.InviteeID))
	}
	total, err := q.Clone().Count(ctx)
	if err != nil {
		return nil, 0, err
	}
	rows, err := q.Order(ent.Desc(entreward.FieldCreatedAt)).
		Limit(f.Limit).Offset(f.Offset).
		All(ctx)
	if err != nil {
		return nil, 0, err
	}
	ids := make([]uuid.UUID, 0, len(rows)*2)
	for _, r := range rows {
		ids = append(ids, r.InviterUserID, r.InviteeUserID)
	}
	emails, err := s.emailsByID(ctx, ids)
	if err != nil {
		return nil, 0, err
	}
	out := make([]InviteAdminReward, 0, len(rows))
	for _, r := range rows {
		out = append(out, InviteAdminReward{
			Reward:       r,
			InviterEmail: emails[r.InviterUserID],
			InviteeEmail: emails[r.InviteeUserID],
		})
	}
	return out, total, nil
}

type InviteAdminRebate struct {
	Rebate       *ent.InviteRebate
	InviterEmail string
	InviteeEmail string
}

func (s *InviteSvc) ListRebates(ctx context.Context, f InviteAdminFilter) ([]InviteAdminRebate, int, error) {
	f = normalizeInviteFilter(f)
	q := s.d.Ent.InviteRebate.Query()
	if f.Status != "" {
		q = q.Where(entrebate.StatusEQ(entrebate.Status(f.Status)))
	}
	if f.InviterID != nil {
		q = q.Where(entrebate.InviterUserIDEQ(*f.InviterID))
	}
	if f.InviteeID != nil {
		q = q.Where(entrebate.InviteeUserIDEQ(*f.InviteeID))
	}
	total, err := q.Clone().Count(ctx)
	if err != nil {
		return nil, 0, err
	}
	rows, err := q.Order(ent.Desc(entrebate.FieldCreatedAt)).
		Limit(f.Limit).Offset(f.Offset).
		All(ctx)
	if err != nil {
		return nil, 0, err
	}
	ids := make([]uuid.UUID, 0, len(rows)*2)
	for _, r := range rows {
		ids = append(ids, r.InviterUserID, r.InviteeUserID)
	}
	emails, err := s.emailsByID(ctx, ids)
	if err != nil {
		return nil, 0, err
	}
	out := make([]InviteAdminRebate, 0, len(rows))
	for _, r := range rows {
		out = append(out, InviteAdminRebate{
			Rebate:       r,
			InviterEmail: emails[r.InviterUserID],
			InviteeEmail: emails[r.InviteeUserID],
		})
	}
	return out, total, nil
}

func (s *InviteSvc) emailsByID(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]string, error) {
	out := map[uuid.UUID]string{}
	if len(ids) == 0 {
		return out, nil
	}
	seen := map[uuid.UUID]bool{}
	uniq := make([]uuid.UUID, 0, len(ids))
	for _, id := range ids {
		if !seen[id] {
			seen[id] = true
			uniq = append(uniq, id)
		}
	}
	users, err := s.d.Ent.User.Query().Where(entuser.IDIn(uniq...)).All(ctx)
	if err != nil {
		return nil, err
	}
	for _, u := range users {
		out[u.ID] = u.Email
	}
	return out, nil
}
