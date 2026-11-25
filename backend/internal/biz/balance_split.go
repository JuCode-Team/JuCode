package biz

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/jucode/jucode/internal/data/ent"
)

// BalanceSplit tells how much of a user's balance was paid for, which is
// the most a refund may take back. The balance holds three kinds of money:
//
//   - paid: site orders (balance top-ups and the initial balance of a
//     bought plan). Refundable.
//   - manual: admin top-ups, adjustments, compensations, redeem codes, agent
//     commission, and balance with no ledger source.
//   - gift: invite rewards and rebates.
//
// Nothing is stored per kind; the split is replayed from the ledger.
// Spending drains gifts first, then manual credits, then paid money, so a
// user keeps as much of their own payments as possible. Cash refunds
// ("site refund: ") return paid money and leave the rest alone.
type BalanceSplit struct {
	UserID     uuid.UUID       `json:"user_id"`
	Balance    decimal.Decimal `json:"balance"`
	Refundable decimal.Decimal `json:"refundable"`
}

const (
	// A positive "site refund: " row puts back a refund the payment channel
	// did not pay, so it is paid money too.
	sqlPaidCredit  = `(b.amount > 0 AND (b.note LIKE 'site order:%' OR b.note LIKE 'site refund:%' OR (b.note LIKE 'plan initial balance:%' AND b.note NOT LIKE 'plan initial balance: redeem %')))`
	sqlGiftCredit  = `(b.amount > 0 AND b.kind IN ('invite_rebate', 'invite_reward'))`
	sqlBucketDebit = `(b.amount < 0 AND b.note NOT LIKE 'site refund:%')`
)

// splitBalances computes the split for the given users. Each bucket is a
// running sum that its credits fill and every debit drains, floored at zero:
// its final level is S_end - min(0, min S). "gift" holds gifts; "free" holds
// everything that is not paid. Because debits drain gifts before manual
// credits, free - gift is what is left of the manual credits.
func (s *FinanceSvc) splitBalances(ctx context.Context, users []*ent.User) ([]BalanceSplit, error) {
	if len(users) == 0 {
		return nil, nil
	}
	idStrs := make([]string, len(users))
	for i, u := range users {
		idStrs[i] = u.ID.String()
	}

	rows, err := s.d.DB().QueryContext(ctx, `
		WITH u AS (
			SELECT unnest($1::uuid[]) AS id
		), t0 AS (
			SELECT b.user_id, min(b.created_at) AS t
			FROM balance_txes b JOIN u ON u.id = b.user_id
			WHERE b.amount > 0 AND NOT `+sqlPaidCredit+`
			GROUP BY b.user_id
		), ev AS (
			SELECT b.user_id, b.created_at, b.id,
				CASE WHEN `+sqlGiftCredit+` THEN b.amount
				     WHEN `+sqlBucketDebit+` THEN b.amount ELSE 0 END AS g,
				CASE WHEN b.amount > 0 AND NOT `+sqlPaidCredit+` THEN b.amount
				     WHEN `+sqlBucketDebit+` THEN b.amount ELSE 0 END AS f
			FROM balance_txes b
			JOIN t0 ON t0.user_id = b.user_id AND b.created_at >= t0.t
			JOIN u ON u.id = b.user_id
		), s AS (
			SELECT user_id, created_at, id,
				sum(g) OVER w AS gs, sum(f) OVER w AS fs
			FROM ev WINDOW w AS (PARTITION BY user_id ORDER BY created_at, id)
		), r AS (
			SELECT user_id, gs, fs,
				min(gs) OVER (PARTITION BY user_id) AS gmin,
				min(fs) OVER (PARTITION BY user_id) AS fmin,
				row_number() OVER (PARTITION BY user_id ORDER BY created_at DESC, id DESC) AS rn
			FROM s
		)
		SELECT user_id, gs, gmin, fs, fmin FROM r WHERE rn = 1
	`, idStrs)
	if err != nil {
		return nil, fmt.Errorf("balance split: %w", err)
	}
	defer rows.Close()
	type levels struct{ gift, free decimal.Decimal }
	got := make(map[uuid.UUID]levels, len(users))
	for rows.Next() {
		var id uuid.UUID
		var gs, gmin, fs, fmin decimal.Decimal
		if err := rows.Scan(&id, &gs, &gmin, &fs, &fmin); err != nil {
			return nil, err
		}
		got[id] = levels{
			gift: gs.Sub(decimal.Min(gmin, decimal.Zero)),
			free: fs.Sub(decimal.Min(fmin, decimal.Zero)),
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// What the ledger says is left of the paid money: everything it holds
	// minus the gift and manual buckets. Some balances were set without a
	// ledger row (migrations, direct edits); the part of the balance beyond
	// the ledger is unexplained and is not refunded as paid money.
	sumRows, err := s.d.DB().QueryContext(ctx, `
		SELECT b.user_id, sum(b.amount)
		FROM balance_txes b
		WHERE b.user_id = ANY($1::uuid[])
		GROUP BY b.user_id
	`, idStrs)
	if err != nil {
		return nil, fmt.Errorf("balance split ledger: %w", err)
	}
	defer sumRows.Close()
	ledger := make(map[uuid.UUID]decimal.Decimal, len(users))
	for sumRows.Next() {
		var id uuid.UUID
		var sum decimal.Decimal
		if err := sumRows.Scan(&id, &sum); err != nil {
			return nil, err
		}
		ledger[id] = sum
	}
	if err := sumRows.Err(); err != nil {
		return nil, err
	}

	out := make([]BalanceSplit, len(users))
	for i, u := range users {
		bal := decimal.Max(u.Balance, decimal.Zero)
		lv := got[u.ID]
		gift := decimal.Min(decimal.Max(lv.gift, decimal.Zero), bal)
		free := decimal.Min(decimal.Max(lv.free, gift), bal)
		paidLeft := ledger[u.ID].Sub(decimal.Max(lv.free, decimal.Zero))
		paid := decimal.Max(decimal.Min(bal.Sub(free), paidLeft), decimal.Zero)
		out[i] = BalanceSplit{UserID: u.ID, Balance: u.Balance, Refundable: paid}
	}
	return out, nil
}

// BalanceSplitFor is the split for one user.
func (s *FinanceSvc) BalanceSplitFor(ctx context.Context, u *ent.User) (BalanceSplit, error) {
	out, err := s.splitBalances(ctx, []*ent.User{u})
	if err != nil {
		return BalanceSplit{}, err
	}
	return out[0], nil
}
