package biz

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/jucode/jucode/internal/data"
	entbalancetx "github.com/jucode/jucode/internal/data/ent/balancetx"
	entorgbalancetx "github.com/jucode/jucode/internal/data/ent/organizationbalancetx"
)

// LedgerSummary is the period rollup for a personal or organization
// balance ledger: income (credits, excluding refund clawbacks) vs
// expense (debits). Days are calendar days in the business timezone and are
// zero-filled so the chart has a continuous X axis.
type LedgerSummary struct {
	From    time.Time         `json:"from"`
	To      time.Time         `json:"to"`
	Income  decimal.Decimal   `json:"income"`
	Expense decimal.Decimal   `json:"expense"`
	Days    []FinanceDayPoint `json:"days"`
}

func (s *Account) BillingSummary(ctx context.Context, userID uuid.UUID, from, to time.Time) (*LedgerSummary, error) {
	from, to = NormalizeLedgerRange(from, to, false, false)
	return queryLedgerSummary(ctx, s.d, entbalancetx.Table, entbalancetx.FieldUserID, userID, from, to, true)
}

func (s *OrganizationSvc) BillingSummary(ctx context.Context, orgID uuid.UUID, from, to time.Time) (*LedgerSummary, error) {
	from, to = NormalizeLedgerRange(from, to, false, false)
	return queryLedgerSummary(ctx, s.d, entorgbalancetx.Table, entorgbalancetx.FieldOrganizationID, orgID, from, to, false)
}

func queryLedgerSummary(
	ctx context.Context,
	d *data.Data,
	table, idCol string,
	id uuid.UUID,
	from, to time.Time,
	excludeRefund bool,
) (*LedgerSummary, error) {
	incomeRefund := ""
	if excludeRefund {
		// Positive refund rows (if any) are not "income". Negative
		// clawbacks must still count as expense so the chart matches
		// the balance change. Cast to text so org ledgers without a
		// refund enum still share this query.
		incomeRefund = " AND kind::text <> 'refund'"
	}
	q := fmt.Sprintf(`
		SELECT (created_at AT TIME ZONE $4)::date,
		       COALESCE(SUM(amount) FILTER (WHERE amount > 0%s), 0),
		       COALESCE(SUM(-amount) FILTER (WHERE amount < 0), 0)
		FROM %s
		WHERE %s = $1 AND created_at >= $2 AND created_at < $3
		GROUP BY 1
		ORDER BY 1
	`, incomeRefund, table, idCol)

	rows, err := d.DB().QueryContext(ctx, q, id, from, to, TimezoneName())
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	incomeByDay := map[string]decimal.Decimal{}
	expenseByDay := map[string]decimal.Decimal{}
	var income, expense decimal.Decimal
	for rows.Next() {
		var day time.Time
		var in, ex decimal.Decimal
		if err := rows.Scan(&day, &in, &ex); err != nil {
			return nil, err
		}
		key := day.Format("2006-01-02")
		incomeByDay[key] = in
		expenseByDay[key] = ex
		income = income.Add(in)
		expense = expense.Add(ex)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return &LedgerSummary{
		From:    from,
		To:      to,
		Income:  income,
		Expense: expense,
		Days:    fillFinanceDays(from, to, incomeByDay, expenseByDay),
	}, nil
}
