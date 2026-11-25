package biz

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/jucode/jucode/internal/data"
	"github.com/jucode/jucode/internal/data/ent"
	entplan "github.com/jucode/jucode/internal/data/ent/plan"
	entredeem "github.com/jucode/jucode/internal/data/ent/redeemcode"
	entredeemrec "github.com/jucode/jucode/internal/data/ent/redeemrecord"
	entuser "github.com/jucode/jucode/internal/data/ent/user"
)

var (
	ErrRedeemCodeNotFound  = errors.New("redeem code not found")
	ErrRedeemCodeInactive  = errors.New("redeem code is not active")
	ErrRedeemCodeExpired   = errors.New("redeem code expired")
	ErrRedeemCodeExhausted = errors.New("redeem code exhausted")
	ErrRedeemKindMismatch  = errors.New("redeem code misconfigured (kind/amount/plan)")
	// ErrRedeemAlreadyUsed = same (user, code) pair already has a record.
	// Returned even if the code's global max_uses still has headroom —
	// per-user uniqueness is enforced regardless of code mode.
	ErrRedeemAlreadyUsed = errors.New("redeem code already used by this user")
)

type RedeemSvc struct {
	d        *data.Data
	plans    *PlanSvc
	agents   *AgentSvc    // optional; nil disables agent settlement entries
	settings *SettingsSvc // kept for symmetry with other services; redemption math no longer needs the exchange rate (points = CNY 1:1)
	invites  *InviteSvc   // optional; re-evaluates signup rewards after redeem topups
}

func NewRedeemSvc(d *data.Data, plans *PlanSvc, settings *SettingsSvc) *RedeemSvc {
	return &RedeemSvc{d: d, plans: plans, settings: settings}
}

// SetAgentSvc wires the agent service in after construction. Without
// it, agent-attributed code redemptions still succeed — they just don't
// generate settlement entries, which would leave the agent uncompensated.
// main wires this up immediately after NewAgentSvc; tests can opt out.
func (s *RedeemSvc) SetAgentSvc(a *AgentSvc) { s.agents = a }

// SetInvites injects the invite service (late-wired; see Account.SetInvites).
func (s *RedeemSvc) SetInvites(inv *InviteSvc) { s.invites = inv }

// --- generation (admin) ---

type GenerateInput struct {
	Kind      string // balance | plan
	Amount    *decimal.Decimal
	Currency  string
	PlanID    *uuid.UUID
	Count     int
	MaxUses   int
	ExpiresAt *time.Time
	BatchID   string
	Note      string
	CreatedBy uuid.UUID
	// AgentID, when non-nil, attributes every generated code to that
	// agent — redemptions of those codes write AgentSettlementEntry
	// rows under that agent's name. Admin-initiated batches leave this
	// nil and skip settlement entries entirely.
	AgentID *uuid.UUID
}

func (s *RedeemSvc) GenerateBatch(ctx context.Context, in GenerateInput) ([]*ent.RedeemCode, error) {
	if in.Count <= 0 {
		in.Count = 1
	}
	if in.Count > 1000 {
		return nil, errors.New("count too large (max 1000 per batch)")
	}
	if in.MaxUses <= 0 {
		in.MaxUses = 1
	}
	switch in.Kind {
	case "balance":
		if in.Amount == nil || in.Amount.Sign() <= 0 {
			return nil, ErrRedeemKindMismatch
		}
	case "plan":
		if in.PlanID == nil {
			return nil, ErrRedeemKindMismatch
		}
		// Make sure the plan actually exists & is active.
		_, err := s.d.Ent.Plan.Query().
			Where(entplan.IDEQ(*in.PlanID), entplan.StatusEQ(entplan.StatusActive)).
			Only(ctx)
		if err != nil {
			return nil, fmt.Errorf("plan: %w", err)
		}
	default:
		return nil, ErrRedeemKindMismatch
	}
	if in.BatchID == "" {
		in.BatchID = "batch-" + shortRandom(8)
	}
	if in.Currency == "" {
		in.Currency = "CNY"
	}

	out := make([]*ent.RedeemCode, 0, in.Count)
	for i := 0; i < in.Count; i++ {
		code := newCodeString()
		q := s.d.Ent.RedeemCode.Create().
			SetCode(code).
			SetKind(entredeem.Kind(in.Kind)).
			SetCurrency(in.Currency).
			SetMaxUses(in.MaxUses).
			SetBatchID(in.BatchID).
			SetNote(in.Note).
			SetCreatedByUserID(in.CreatedBy)
		if in.Amount != nil {
			q = q.SetAmount(*in.Amount)
		}
		if in.PlanID != nil {
			q = q.SetPlanID(*in.PlanID)
		}
		if in.ExpiresAt != nil {
			q = q.SetExpiresAt(*in.ExpiresAt)
		}
		if in.AgentID != nil {
			q = q.SetAgentID(*in.AgentID)
		}
		rec, err := q.Save(ctx)
		if err != nil {
			return out, err
		}
		out = append(out, rec)
	}
	return out, nil
}

// --- listing / revocation ---

type ListFilter struct {
	Status  string
	Kind    string
	BatchID string
	Limit   int
}

func (s *RedeemSvc) List(ctx context.Context, f ListFilter) ([]*ent.RedeemCode, error) {
	q := s.d.Ent.RedeemCode.Query()
	if f.Status != "" {
		q = q.Where(entredeem.StatusEQ(entredeem.Status(f.Status)))
	}
	if f.Kind != "" {
		q = q.Where(entredeem.KindEQ(entredeem.Kind(f.Kind)))
	}
	if f.BatchID != "" {
		q = q.Where(entredeem.BatchIDEQ(f.BatchID))
	}
	if f.Limit <= 0 || f.Limit > 500 {
		f.Limit = 200
	}
	return q.Order(ent.Desc(entredeem.FieldCreatedAt)).Limit(f.Limit).All(ctx)
}

func (s *RedeemSvc) Revoke(ctx context.Context, id uuid.UUID) error {
	n, err := s.d.Ent.RedeemCode.Update().
		Where(entredeem.IDEQ(id), entredeem.StatusEQ(entredeem.StatusActive)).
		SetStatus(entredeem.StatusRevoked).
		Save(ctx)
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrRedeemCodeNotFound
	}
	return nil
}

// RevokeBatch revokes every active code in the given batch_id.
func (s *RedeemSvc) RevokeBatch(ctx context.Context, batchID string) (int, error) {
	return s.d.Ent.RedeemCode.Update().
		Where(entredeem.BatchIDEQ(batchID), entredeem.StatusEQ(entredeem.StatusActive)).
		SetStatus(entredeem.StatusRevoked).
		Save(ctx)
}

// --- redemption (user-facing) ---

type RedeemResult struct {
	Kind         string           `json:"kind"`
	Amount       *decimal.Decimal `json:"amount,omitempty"`
	Currency     string           `json:"currency,omitempty"`
	BalanceAfter *decimal.Decimal `json:"balance_after,omitempty"`
	UserPlanID   *uuid.UUID       `json:"user_plan_id,omitempty"`
	PlanName     string           `json:"plan_name,omitempty"`
	ExpireAt     *time.Time       `json:"expire_at,omitempty"`
}

// Redeem atomically validates a code and applies its effect to the user.
// Concurrency safety: SELECT FOR UPDATE locks the row so a single code
// shared by N users (max_uses > 1) is consumed correctly.
func (s *RedeemSvc) Redeem(ctx context.Context, userID uuid.UUID, code, ip string) (*RedeemResult, error) {
	code = strings.TrimSpace(code)
	if code == "" {
		return nil, ErrRedeemCodeNotFound
	}

	tx, err := s.d.Ent.Tx(ctx)
	if err != nil {
		return nil, err
	}
	rollback := func() { _ = tx.Rollback() }

	rc, err := tx.RedeemCode.Query().
		Where(entredeem.CodeEQ(code)).
		ForUpdate().
		Only(ctx)
	if err != nil {
		rollback()
		if ent.IsNotFound(err) {
			return nil, ErrRedeemCodeNotFound
		}
		return nil, err
	}
	if rc.Status != entredeem.StatusActive {
		rollback()
		return nil, ErrRedeemCodeInactive
	}
	if rc.ExpiresAt != nil && rc.ExpiresAt.Before(time.Now()) {
		// auto-flip status to expired for nicer admin views.
		_, _ = tx.RedeemCode.UpdateOneID(rc.ID).SetStatus(entredeem.StatusExpired).Save(ctx)
		_ = tx.Commit()
		return nil, ErrRedeemCodeExpired
	}
	if rc.UsedCount >= rc.MaxUses {
		rollback()
		return nil, ErrRedeemCodeExhausted
	}
	// Per-user uniqueness: no double-claim even if the code is multi-use.
	// We pre-check inside the same transaction (cheap query, friendlier
	// error than a 23505 unique-constraint surface), but the DB index
	// on (code_id, user_id) is the real source of truth — guards
	// against two concurrent redemption requests from the same user
	// landing in different goroutines.
	alreadyUsed, err := tx.RedeemRecord.Query().
		Where(entredeemrec.CodeIDEQ(rc.ID), entredeemrec.UserIDEQ(userID)).
		Exist(ctx)
	if err != nil {
		rollback()
		return nil, err
	}
	if alreadyUsed {
		rollback()
		return nil, ErrRedeemAlreadyUsed
	}

	// Internal accounting unit is points: 1 point = 1 CNY (see
	// internal/biz/currency.go). Code amounts and plan prices are entered
	// directly in points — every downstream row is written as CNY with no
	// conversion.
	result := &RedeemResult{Kind: string(rc.Kind), Currency: CurrencyCNY}

	switch rc.Kind {
	case entredeem.KindBalance:
		if rc.Amount == nil {
			rollback()
			return nil, ErrRedeemKindMismatch
		}
		amount := *rc.Amount
		// Lock the user row inside the same transaction so a concurrent
		// charge / topup / parallel-redeem doesn't lose an update.
		u, err := tx.User.Query().Where(entuser.IDEQ(userID)).ForUpdate().Only(ctx)
		if err != nil {
			rollback()
			return nil, err
		}
		newBal := u.Balance.Add(amount)
		if _, err := tx.User.UpdateOneID(u.ID).SetBalance(newBal).SetCurrency(CurrencyCNY).Save(ctx); err != nil {
			rollback()
			return nil, err
		}
		if _, err := tx.BalanceTx.Create().
			SetUserID(u.ID).SetKind("topup").
			SetAmount(amount).SetBalanceAfter(newBal).
			SetCurrency(CurrencyCNY).
			SetNote("redeem code: " + rc.Code).
			Save(ctx); err != nil {
			rollback()
			return nil, err
		}
		result.Amount = &amount
		result.BalanceAfter = &newBal

		balRec, err := tx.RedeemRecord.Create().
			SetCodeID(rc.ID).SetUserID(u.ID).
			SetKind(entredeemrec.Kind(string(rc.Kind))).
			SetAmount(amount).SetCurrency(CurrencyCNY).
			SetIP(ip).
			Save(ctx)
		if err != nil {
			rollback()
			return nil, err
		}
		if rc.AgentID != nil && s.agents != nil {
			if err := s.agents.RecordSale(ctx, tx, *rc.AgentID, rc.ID, balRec.ID, u.ID, amount, CurrencyCNY); err != nil {
				rollback()
				return nil, err
			}
		}

	case entredeem.KindPlan:
		if rc.PlanID == nil {
			rollback()
			return nil, ErrRedeemKindMismatch
		}
		plan, err := tx.Plan.Get(ctx, *rc.PlanID)
		if err != nil {
			rollback()
			return nil, err
		}
		start := time.Now()
		expire := start.AddDate(0, 0, plan.PeriodDays)
		if plan.PeriodDays <= 0 {
			expire = start.AddDate(0, 0, 90)
		}
		up, err := tx.UserPlan.Create().
			SetUserID(userID).SetPlanID(plan.ID).
			SetStartAt(start).SetExpireAt(expire).
			Save(ctx)
		if err != nil {
			rollback()
			return nil, err
		}
		u, err := tx.User.Query().Where(entuser.IDEQ(userID)).ForUpdate().Only(ctx)
		if err != nil {
			rollback()
			return nil, err
		}
		if err := grantPlanInitialBalance(ctx, tx, u, plan, up.ID, "plan initial balance: redeem "+rc.Code); err != nil {
			rollback()
			return nil, err
		}
		result.UserPlanID = &up.ID
		result.PlanName = plan.Name
		result.ExpireAt = &expire

		planRec, err := tx.RedeemRecord.Create().
			SetCodeID(rc.ID).SetUserID(userID).
			SetKind(entredeemrec.Kind(string(rc.Kind))).
			SetUserPlanID(up.ID).
			SetCurrency(CurrencyCNY).
			SetIP(ip).
			Save(ctx)
		if err != nil {
			rollback()
			return nil, err
		}
		if rc.AgentID != nil && s.agents != nil {
			// plan.Price is already in points (1 point = 1 CNY); agent
			// settlement entries record it as-is, no conversion.
			price := plan.Price
			if err := s.agents.RecordSale(ctx, tx, *rc.AgentID, rc.ID, planRec.ID, userID, price, CurrencyCNY); err != nil {
				rollback()
				return nil, err
			}
		}
	}

	// Bump used_count and flip status if exhausted.
	newUsed := rc.UsedCount + 1
	upd := tx.RedeemCode.UpdateOneID(rc.ID).SetUsedCount(newUsed)
	if newUsed >= rc.MaxUses {
		upd = upd.SetStatus(entredeem.StatusExhausted)
	}
	if _, err := upd.Save(ctx); err != nil {
		rollback()
		return nil, err
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}
	// Redemptions write topup-kind txs (balance codes and plan initial
	// balance) which may satisfy an any_topup/min_topup signup condition.
	s.invites.EvaluateSignupAsync(userID)
	return result, nil
}

// --- helpers ---

// newCodeString returns a human-friendly code like JUC-AB12CD-EF34GH.
// Alphabet excludes 0/O/1/I/L to avoid transcription errors.
const codeAlphabet = "ABCDEFGHJKMNPQRSTUVWXYZ23456789"

func newCodeString() string {
	buf := make([]byte, 12)
	if _, err := rand.Read(buf); err != nil {
		// fall back to time-based; collisions are extremely unlikely at MVP scale.
		t := time.Now().UnixNano()
		for i := range buf {
			buf[i] = byte(t >> uint(i*4))
		}
	}
	var b strings.Builder
	b.Grow(18)
	b.WriteString("JUC-")
	for i, c := range buf {
		if i == 6 {
			b.WriteByte('-')
		}
		b.WriteByte(codeAlphabet[int(c)%len(codeAlphabet)])
	}
	return b.String()
}

func shortRandom(n int) string {
	buf := make([]byte, n)
	_, _ = rand.Read(buf)
	var b strings.Builder
	b.Grow(n)
	for _, c := range buf {
		b.WriteByte(codeAlphabet[int(c)%len(codeAlphabet)])
	}
	return b.String()
}
