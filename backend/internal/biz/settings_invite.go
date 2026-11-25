package biz

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/shopspring/decimal"
)

// Signup-reward qualification conditions (InviteProgramSettings.SignupCondition).
// any_topup/min_topup are persisted identifiers; both now use net paid orders.
const (
	InviteConditionNone          = "none"
	InviteConditionEmailVerified = "email_verified"
	InviteConditionAnyTopup      = "any_topup"
	InviteConditionMinTopup      = "min_topup"
)

// InviteProgramSettings is the admin-facing invite/referral configuration,
// stored as one JSON value under the site_settings key "invite_program".
// Everything defaults to OFF / zero so merely deploying the feature changes
// nothing until an admin explicitly configures and enables it.
//
// Amounts and percentages ride as decimal so the JSON wire format is the
// quoted-string form ("0.5") the rest of the money API uses.
type InviteProgramSettings struct {
	Enabled bool `json:"enabled"`

	// Signup reward: fixed amount credited to the inviter once the invitee
	// meets SignupCondition.
	SignupRewardEnabled  bool            `json:"signup_reward_enabled"`
	SignupRewardAmount   decimal.Decimal `json:"signup_reward_amount"`
	SignupCondition      string          `json:"signup_condition"`
	SignupMinTopupAmount decimal.Decimal `json:"signup_min_topup_amount"`

	// Topup rebate: TopupRebateRatePct percent of each PAID balance order
	// the invitee places, credited to the inviter. One rebate per order.
	//
	// This replaced the per-request consumption rebate on 2026-09-03. That
	// model wrote one invite_rebates row AND one balance_txes row per
	// billed request, which grew both tables without bound and buried the
	// inviter's own ledger under sub-cent entries.
	TopupRebateEnabled bool            `json:"topup_rebate_enabled"`
	TopupRebateRatePct decimal.Decimal `json:"topup_rebate_rate_pct"`
	// 0 = unlimited window / cap.
	TopupRebateWindowDays    int             `json:"topup_rebate_window_days"`
	TopupRebateCapPerInvitee decimal.Decimal `json:"topup_rebate_cap_per_invitee"`

	// Risk controls. Caps of 0 mean unlimited; the *count* cap defaults to
	// 10/day so a fresh enable isn't instantly farmable. The checks always
	// RUN and always record their flags on the payout row; AutoHoldOnRisk
	// decides whether a flagged payout waits for a human or just pays with
	// the flags attached for later audit.
	SignupDailyCountCapPerInviter  int             `json:"signup_daily_count_cap_per_inviter"`
	SignupDailyAmountCapPerInviter decimal.Decimal `json:"signup_daily_amount_cap_per_inviter"`
	InviterMinAgeHours             int             `json:"inviter_min_age_hours"`
	SameIPDailyCap                 int             `json:"same_ip_daily_cap"`
	AutoHoldOnRisk                 bool            `json:"auto_hold_on_risk"`
	RequireInviterEmailVerified    bool            `json:"require_inviter_email_verified"`

	// ReviewAmountThreshold: any SINGLE payout (signup reward or one
	// topup rebate) whose amount is >= this value is held for manual
	// pay/reject. 0 = off. This is a money control, not a fraud heuristic:
	// it applies even when AutoHoldOnRisk is false, and it never reduces
	// the amount (it is not a cap).
	ReviewAmountThreshold decimal.Decimal `json:"review_amount_threshold"`
}

// InviteProgramPublic is the subset safe to show anonymous visitors on
// /v1/public/site-info and invitees on /v1/me/invite. Risk thresholds and
// caps are deliberately absent — publishing them is a how-to for gaming the
// risk engine.
type InviteProgramPublic struct {
	Enabled              bool            `json:"enabled"`
	SignupRewardEnabled  bool            `json:"signup_reward_enabled"`
	SignupRewardAmount   decimal.Decimal `json:"signup_reward_amount"`
	SignupCondition      string          `json:"signup_condition"`
	SignupMinTopupAmount decimal.Decimal `json:"signup_min_topup_amount"`
	TopupRebateEnabled   bool            `json:"topup_rebate_enabled"`
	TopupRebateRatePct   decimal.Decimal `json:"topup_rebate_rate_pct"`
	// Program terms, not risk thresholds: the inviter needs them to know
	// which payouts to expect. 0 = no window.
	TopupRebateWindowDays int `json:"topup_rebate_window_days"`
	// RiskReview: flagged payouts wait for an admin instead of paying out.
	// EmailVerificationHold: payouts made while the inviter's email is
	// unverified are among them.
	RiskReview            bool `json:"risk_review"`
	EmailVerificationHold bool `json:"email_verification_hold"`
}

// Public projects the admin config down to the publishable subset.
func (p InviteProgramSettings) Public() InviteProgramPublic {
	return InviteProgramPublic{
		Enabled:               p.Enabled,
		SignupRewardEnabled:   p.SignupRewardEnabled,
		SignupRewardAmount:    p.SignupRewardAmount,
		SignupCondition:       p.SignupCondition,
		SignupMinTopupAmount:  p.SignupMinTopupAmount,
		TopupRebateEnabled:    p.TopupRebateEnabled,
		TopupRebateRatePct:    p.TopupRebateRatePct,
		TopupRebateWindowDays: p.TopupRebateWindowDays,
		RiskReview:            p.AutoHoldOnRisk,
		EmailVerificationHold: p.AutoHoldOnRisk && p.RequireInviterEmailVerified,
	}
}

// DefaultInviteProgram returns the everything-off baseline. Fresh installs
// and unparseable stored values both resolve to this.
func DefaultInviteProgram() InviteProgramSettings {
	return InviteProgramSettings{
		Enabled:                        false,
		SignupRewardEnabled:            false,
		SignupRewardAmount:             decimal.Zero,
		SignupCondition:                InviteConditionEmailVerified,
		SignupMinTopupAmount:           decimal.Zero,
		TopupRebateEnabled:             false,
		TopupRebateRatePct:             decimal.Zero,
		TopupRebateWindowDays:          0,
		TopupRebateCapPerInvitee:       decimal.Zero,
		SignupDailyCountCapPerInviter:  10,
		SignupDailyAmountCapPerInviter: decimal.Zero,
		InviterMinAgeHours:             24,
		SameIPDailyCap:                 3,
		// Manual review is OFF by default: risk flags are recorded on the
		// payout row either way, but a flagged payout pays straight through
		// unless an operator deliberately turns holding on.
		AutoHoldOnRisk:              false,
		RequireInviterEmailVerified: true,
		ReviewAmountThreshold:       decimal.Zero,
	}
}

// NormalizeInviteProgram validates an admin-submitted config. Unlike most
// site settings (which silently clamp), invalid invite values are rejected:
// a mistyped rebate rate is real money.
func NormalizeInviteProgram(in InviteProgramSettings) (InviteProgramSettings, error) {
	in.SignupCondition = strings.TrimSpace(in.SignupCondition)
	if in.SignupCondition == "" {
		in.SignupCondition = InviteConditionEmailVerified
	}
	switch in.SignupCondition {
	case InviteConditionNone, InviteConditionEmailVerified, InviteConditionAnyTopup, InviteConditionMinTopup:
	default:
		return in, fmt.Errorf("invite_program: invalid signup_condition %q", in.SignupCondition)
	}
	if in.SignupRewardAmount.Sign() < 0 {
		return in, fmt.Errorf("invite_program: signup_reward_amount must be >= 0")
	}
	if in.SignupMinTopupAmount.Sign() < 0 {
		return in, fmt.Errorf("invite_program: signup_min_topup_amount must be >= 0")
	}
	if in.TopupRebateRatePct.Sign() < 0 || in.TopupRebateRatePct.GreaterThan(decimal.NewFromInt(100)) {
		return in, fmt.Errorf("invite_program: topup_rebate_rate_pct must be within 0-100")
	}
	if in.TopupRebateWindowDays < 0 {
		return in, fmt.Errorf("invite_program: topup_rebate_window_days must be >= 0")
	}
	if in.TopupRebateCapPerInvitee.Sign() < 0 {
		return in, fmt.Errorf("invite_program: topup_rebate_cap_per_invitee must be >= 0")
	}
	if in.SignupDailyCountCapPerInviter < 0 {
		return in, fmt.Errorf("invite_program: signup_daily_count_cap_per_inviter must be >= 0")
	}
	if in.SignupDailyAmountCapPerInviter.Sign() < 0 {
		return in, fmt.Errorf("invite_program: signup_daily_amount_cap_per_inviter must be >= 0")
	}
	if in.InviterMinAgeHours < 0 {
		return in, fmt.Errorf("invite_program: inviter_min_age_hours must be >= 0")
	}
	if in.SameIPDailyCap < 0 {
		return in, fmt.Errorf("invite_program: same_ip_daily_cap must be >= 0")
	}
	if in.ReviewAmountThreshold.Sign() < 0 {
		return in, fmt.Errorf("invite_program: review_amount_threshold must be >= 0")
	}
	return in, nil
}

// parseInviteProgram reads the stored JSON, falling back to the defaults on
// any parse error (a corrupt KV row must not disable settings loading).
func parseInviteProgram(v string) InviteProgramSettings {
	out := DefaultInviteProgram()
	v = strings.TrimSpace(v)
	if v == "" || v == "null" {
		return out
	}
	// Overlay stored values on the defaults so older/partial JSON does not
	// silently turn off newly-added risk controls.
	parsed := DefaultInviteProgram()
	if err := json.Unmarshal([]byte(v), &parsed); err != nil {
		return out
	}
	normalized, err := NormalizeInviteProgram(parsed)
	if err != nil {
		return out
	}
	return normalized
}
