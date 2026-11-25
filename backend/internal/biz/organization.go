package biz

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/jucode/jucode/internal/data"
	"github.com/jucode/jucode/internal/data/ent"
	entorg "github.com/jucode/jucode/internal/data/ent/organization"
	entorgtx "github.com/jucode/jucode/internal/data/ent/organizationbalancetx"
	entmember "github.com/jucode/jucode/internal/data/ent/organizationmember"
)

var (
	ErrOrganizationNotFound  = errors.New("organization not found")
	ErrOrganizationForbidden = errors.New("organization permission denied")
	ErrOrganizationLimit     = errors.New("organization limit exceeded")
	ErrMemberLimit           = errors.New("organization member limit exceeded")
	ErrInvalidAmount         = errors.New("amount must be positive")
)

type OrganizationSvc struct{ d *data.Data }

func NewOrganizationSvc(d *data.Data) *OrganizationSvc { return &OrganizationSvc{d: d} }

type CreateOrganizationInput struct {
	Name              string
	OwnerUserID       uuid.UUID
	MonthlySpendLimit decimal.Decimal
}

type UpdateOrganizationInput struct {
	Name              *string
	OwnerUserID       *uuid.UUID
	MonthlySpendLimit *decimal.Decimal
}

type OrganizationMemberInput struct {
	UserID            uuid.UUID
	Role              string
	MonthlySpendLimit decimal.Decimal
	TotalSpendLimit   decimal.Decimal
}

func (s *OrganizationSvc) Create(ctx context.Context, in CreateOrganizationInput) (*ent.Organization, error) {
	tx, err := s.d.Ent.Tx(ctx)
	if err != nil {
		return nil, err
	}
	org, err := tx.Organization.Create().
		SetName(strings.TrimSpace(in.Name)).
		SetOwnerUserID(in.OwnerUserID).
		SetMonthlySpendLimit(in.MonthlySpendLimit).
		Save(ctx)
	if err != nil {
		_ = tx.Rollback()
		return nil, err
	}
	if _, err := tx.OrganizationMember.Create().
		SetOrganizationID(org.ID).
		SetUserID(in.OwnerUserID).
		SetRole(entmember.RoleOwner).
		SetMonthlySpendLimit(decimal.Zero).
		Save(ctx); err != nil {
		_ = tx.Rollback()
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return org, nil
}

func (s *OrganizationSvc) List(ctx context.Context, q string, limit, offset int) ([]*ent.Organization, int, error) {
	query := s.d.Ent.Organization.Query()
	if q = strings.TrimSpace(q); q != "" {
		query = query.Where(entorg.NameContainsFold(q))
	}
	total, err := query.Clone().Count(ctx)
	if err != nil {
		return nil, 0, err
	}
	rows, err := query.Order(ent.Desc(entorg.FieldCreatedAt)).Limit(limit).Offset(offset).All(ctx)
	return rows, total, err
}

func (s *OrganizationSvc) Get(ctx context.Context, id uuid.UUID) (*ent.Organization, error) {
	org, err := s.d.Ent.Organization.Get(ctx, id)
	if err != nil {
		if ent.IsNotFound(err) {
			return nil, ErrOrganizationNotFound
		}
		return nil, err
	}
	return org, nil
}

// UserOrganization pairs an organization with the querying user's role in
// it, so the console can decide which manager-only tabs to show without a
// second round-trip.
type UserOrganization struct {
	Org  *ent.Organization
	Role string
}

func (s *OrganizationSvc) ListForUser(ctx context.Context, userID uuid.UUID) ([]UserOrganization, error) {
	members, err := s.d.Ent.OrganizationMember.Query().
		Where(entmember.UserIDEQ(userID)).
		Order(ent.Desc(entmember.FieldCreatedAt)).
		All(ctx)
	if err != nil {
		return nil, err
	}
	if len(members) == 0 {
		return []UserOrganization{}, nil
	}
	roleByOrg := make(map[uuid.UUID]string, len(members))
	ids := make([]uuid.UUID, 0, len(members))
	for _, m := range members {
		ids = append(ids, m.OrganizationID)
		roleByOrg[m.OrganizationID] = string(m.Role)
	}
	orgs, err := s.d.Ent.Organization.Query().
		Where(entorg.IDIn(ids...)).
		Order(ent.Asc(entorg.FieldName)).
		All(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]UserOrganization, 0, len(orgs))
	for _, org := range orgs {
		out = append(out, UserOrganization{Org: org, Role: roleByOrg[org.ID]})
	}
	return out, nil
}

func (s *OrganizationSvc) Members(ctx context.Context, orgID uuid.UUID) ([]*ent.OrganizationMember, error) {
	return s.d.Ent.OrganizationMember.Query().
		Where(entmember.OrganizationIDEQ(orgID)).
		WithUser().
		Order(ent.Asc(entmember.FieldRole), ent.Desc(entmember.FieldCreatedAt)).
		All(ctx)
}

func (s *OrganizationSvc) Member(ctx context.Context, orgID, userID uuid.UUID) (*ent.OrganizationMember, error) {
	m, err := s.d.Ent.OrganizationMember.Query().
		Where(entmember.OrganizationIDEQ(orgID), entmember.UserIDEQ(userID)).
		Only(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			return nil, ErrOrganizationForbidden
		}
		return nil, err
	}
	return m, nil
}

func (s *OrganizationSvc) CanManage(ctx context.Context, orgID, userID uuid.UUID) (bool, error) {
	m, err := s.Member(ctx, orgID, userID)
	if err != nil {
		if errors.Is(err, ErrOrganizationForbidden) {
			return false, nil
		}
		return false, err
	}
	return m.Role == entmember.RoleOwner || m.Role == entmember.RoleAdmin, nil
}

func (s *OrganizationSvc) Update(ctx context.Context, id uuid.UUID, in UpdateOrganizationInput) (*ent.Organization, error) {
	q := s.d.Ent.Organization.UpdateOneID(id)
	if in.Name != nil {
		q = q.SetName(strings.TrimSpace(*in.Name))
	}
	if in.OwnerUserID != nil {
		q = q.SetOwnerUserID(*in.OwnerUserID)
	}
	if in.MonthlySpendLimit != nil {
		q = q.SetMonthlySpendLimit(*in.MonthlySpendLimit)
	}
	org, err := q.Save(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			return nil, ErrOrganizationNotFound
		}
		return nil, err
	}
	if in.OwnerUserID != nil {
		if err := s.UpsertMember(ctx, id, OrganizationMemberInput{
			UserID: *in.OwnerUserID,
			Role:   "owner",
		}); err != nil {
			return nil, err
		}
	}
	return org, nil
}

func (s *OrganizationSvc) UpsertMember(ctx context.Context, orgID uuid.UUID, in OrganizationMemberInput) error {
	var role entmember.Role
	switch in.Role {
	case "owner":
		role = entmember.RoleOwner
	case "admin":
		role = entmember.RoleAdmin
	case "", "member":
		role = entmember.RoleMember
	default:
		return errors.New("organization role must be owner|admin|member")
	}
	if in.MonthlySpendLimit.Sign() < 0 || in.TotalSpendLimit.Sign() < 0 {
		return ErrInvalidAmount
	}
	return s.d.Ent.OrganizationMember.Create().
		SetOrganizationID(orgID).
		SetUserID(in.UserID).
		SetRole(role).
		SetMonthlySpendLimit(in.MonthlySpendLimit).
		SetTotalSpendLimit(in.TotalSpendLimit).
		OnConflictColumns("organization_id", "user_id").
		UpdateNewValues().
		Exec(ctx)
}

// MemberSpend is one member's spend against their caps, for the console.
type MemberSpend struct {
	Month decimal.Decimal
	Total decimal.Decimal
}

// MemberSpendFor reports a member's month-to-date and lifetime spend from the
// organization ledger — the same numbers the charge path tests limits against.
func (s *OrganizationSvc) MemberSpendFor(ctx context.Context, orgID, userID uuid.UUID) (MemberSpend, error) {
	month, total, err := organizationMemberSpend(ctx, s.d.DB(), orgID, userID)
	if err != nil {
		return MemberSpend{}, err
	}
	return MemberSpend{Month: month, Total: total}, nil
}

func (s *OrganizationSvc) RemoveMember(ctx context.Context, orgID, userID uuid.UUID) error {
	org, err := s.Get(ctx, orgID)
	if err != nil {
		return err
	}
	if org.OwnerUserID == userID {
		return errors.New("cannot remove organization owner")
	}
	_, err = s.d.Ent.OrganizationMember.Delete().
		Where(entmember.OrganizationIDEQ(orgID), entmember.UserIDEQ(userID)).
		Exec(ctx)
	return err
}

// TransferFromMember moves `amount` from a member's personal balance into the
// organization's balance, atomically.
//
// This is how an organization gets funded from the console. It is a transfer,
// not a grant: the money has to already exist in someone's personal balance
// (put there by the normal payment flow), because an endpoint that simply adds
// to organizations.balance lets any org manager mint currency — which is what
// this used to do.
//
// Lock order is users → organizations, matching every other path that touches
// both: the charge path locks only the organization row, so no cycle is
// possible.
func (s *OrganizationSvc) TransferFromMember(ctx context.Context, orgID, userID uuid.UUID, amount decimal.Decimal, note string) (orgBalance, userBalance decimal.Decimal, err error) {
	if amount.Sign() <= 0 {
		return decimal.Zero, decimal.Zero, ErrInvalidAmount
	}
	tx, err := s.d.DB().BeginTx(ctx, nil)
	if err != nil {
		return decimal.Zero, decimal.Zero, err
	}
	rollback := func(e error) (decimal.Decimal, decimal.Decimal, error) {
		_ = tx.Rollback()
		return decimal.Zero, decimal.Zero, e
	}

	var personal decimal.Decimal
	if err := tx.QueryRowContext(ctx,
		`SELECT balance FROM users WHERE id=$1 FOR UPDATE`, userID).Scan(&personal); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return rollback(ErrOrganizationForbidden)
		}
		return rollback(err)
	}
	if personal.LessThan(amount) {
		return rollback(ErrInsufficientBalance)
	}

	var org decimal.Decimal
	if err := tx.QueryRowContext(ctx,
		`SELECT balance FROM organizations WHERE id=$1 FOR UPDATE`, orgID).Scan(&org); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return rollback(ErrOrganizationNotFound)
		}
		return rollback(err)
	}

	newPersonal := personal.Sub(amount)
	newOrg := org.Add(amount)

	if _, err := tx.ExecContext(ctx,
		`UPDATE users SET balance=$1, currency='CNY', updated_at=NOW() WHERE id=$2`,
		newPersonal, userID); err != nil {
		return rollback(err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO balance_txes
		  (id, created_at, updated_at, user_id, kind, amount, balance_after, currency, note)
		VALUES ($1,NOW(),NOW(),$2,'transfer_out',$3,$4,'CNY',$5)`,
		uuid.New(), userID, amount.Neg(), newPersonal, note); err != nil {
		return rollback(err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE organizations SET balance=$1, currency='CNY', updated_at=NOW() WHERE id=$2`,
		newOrg, orgID); err != nil {
		return rollback(err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO organization_balance_txes
		  (id, created_at, updated_at, organization_id, actor_user_id, kind, amount, balance_after, currency, note)
		VALUES ($1,NOW(),NOW(),$2,$3,'topup',$4,$5,'CNY',$6)`,
		uuid.New(), orgID, userID, amount, newOrg, note); err != nil {
		return rollback(err)
	}
	if err := tx.Commit(); err != nil {
		return decimal.Zero, decimal.Zero, err
	}
	return newOrg, newPersonal, nil
}

// Grant credits an organization without a funding source. ADMIN ONLY — it
// creates money. Kept because operators already have the equivalent power over
// personal balances (admin user topup / balance override); the user-facing
// path is TransferFromMember.
func (s *OrganizationSvc) Grant(ctx context.Context, orgID uuid.UUID, actorID *uuid.UUID, amount decimal.Decimal, note string) (decimal.Decimal, error) {
	tx, err := s.d.Ent.Tx(ctx)
	if err != nil {
		return decimal.Zero, err
	}
	org, err := tx.Organization.Query().Where(entorg.IDEQ(orgID)).ForUpdate().Only(ctx)
	if err != nil {
		_ = tx.Rollback()
		if ent.IsNotFound(err) {
			return decimal.Zero, ErrOrganizationNotFound
		}
		return decimal.Zero, err
	}
	next := org.Balance.Add(amount)
	if _, err := tx.Organization.UpdateOneID(orgID).SetBalance(next).SetCurrency(CurrencyCNY).Save(ctx); err != nil {
		_ = tx.Rollback()
		return decimal.Zero, err
	}
	q := tx.OrganizationBalanceTx.Create().
		SetOrganizationID(orgID).
		SetKind(entorgtx.KindTopup).
		SetAmount(amount).
		SetBalanceAfter(next).
		SetCurrency(CurrencyCNY).
		SetNote(note)
	if actorID != nil {
		q = q.SetActorUserID(*actorID)
	}
	if _, err := q.Save(ctx); err != nil {
		_ = tx.Rollback()
		return decimal.Zero, err
	}
	if err := tx.Commit(); err != nil {
		return decimal.Zero, err
	}
	return next, nil
}

type OrgBalanceTxFilter struct {
	Kind   string // topup | deduct | adjust; empty = all
	Q      string // substring match on note
	From   time.Time
	To     time.Time
	Limit  int
	Offset int
}

// ListBalanceTxs pages through one organization's balance ledger, newest
// first. Callers gate access — the ledger exposes balance_after (the org's
// running balance), so it is manager-only on the user-facing API.
func (s *OrganizationSvc) ListBalanceTxs(ctx context.Context, orgID uuid.UUID, f OrgBalanceTxFilter) ([]*ent.OrganizationBalanceTx, int, error) {
	query := s.d.Ent.OrganizationBalanceTx.Query().
		Where(entorgtx.OrganizationIDEQ(orgID))
	if kind := strings.TrimSpace(f.Kind); kind != "" {
		query = query.Where(entorgtx.KindEQ(entorgtx.Kind(kind)))
	}
	if q := strings.TrimSpace(f.Q); q != "" {
		query = query.Where(entorgtx.NoteContainsFold(q))
	}
	if !f.From.IsZero() {
		query = query.Where(entorgtx.CreatedAtGTE(f.From))
	}
	if !f.To.IsZero() {
		query = query.Where(entorgtx.CreatedAtLT(f.To))
	}
	total, err := query.Clone().Count(ctx)
	if err != nil {
		return nil, 0, err
	}
	rows, err := query.
		Order(ent.Desc(entorgtx.FieldCreatedAt)).
		Limit(f.Limit).
		Offset(f.Offset).
		All(ctx)
	return rows, total, err
}

// MonthlyDeducted is the console's read of month-to-date spend. Same SQL
// aggregate the charge path uses, so the number a manager sees is the number
// the limit is tested against.
func (s *OrganizationSvc) MonthlyDeducted(ctx context.Context, orgID uuid.UUID, userID *uuid.UUID) (decimal.Decimal, error) {
	if userID != nil {
		month, _, err := organizationMemberSpend(ctx, s.d.DB(), orgID, *userID)
		return month, err
	}
	return organizationMonthSpend(ctx, s.d.DB(), orgID)
}

func monthStartUTC() time.Time {
	now := time.Now().UTC()
	return time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
}
