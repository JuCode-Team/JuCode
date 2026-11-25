package biz

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"
	"golang.org/x/sync/errgroup"

	"github.com/jucode/jucode/internal/data/ent"
	entprovider "github.com/jucode/jucode/internal/data/ent/provider"
	entticket "github.com/jucode/jucode/internal/data/ent/ticket"
)

// Finance windows are calendar days in the business timezone (see
// timezone.go) so a "30 天" picker matches the operator's date, not UTC
// midnight.

// maxFinanceSpan caps how far one summary query may scan usage_logs.
// The table is the largest in the DB; unbounded ranges are a full-table
// sequential scan dressed up as a dashboard.
const maxFinanceSpan = 366 * 24 * time.Hour

// maxLedgerSpan is for personal/org balance ledgers, which are tiny
// compared to usage_logs. Ten years is enough to keep "all history"
// reachable from the billing page without an unbounded scan.
const maxLedgerSpan = 10 * 366 * 24 * time.Hour

// usageCostExprFor renders the per-row channel cost formula against one price
// table. Callers pass the join alias:
//
//   - "am" = upstream_account_models, the ACCOUNT cost basis (current rows);
//   - "pm" = provider_models, the pre-migration channel basis, still the truth
//     for rows written before the account migration.
//
// The formula itself is the one the settlement path uses — keep them identical
// so the finance page and monthly 对账 agree. audio_tokens_* fold into the text
// terms: neither price table has an audio cost column, and those tokens used to
// live inside tokens_in/out.
func usageCostExprFor(alias string) string {
	return fmt.Sprintf(`
		COALESCE(%[1]s.price_per_request_cost, 0)
		+ (((ul.tokens_in + ul.audio_tokens_in)::numeric * COALESCE(%[1]s.input_unit_cost, 0))
		+ ((ul.tokens_out + ul.audio_tokens_out)::numeric * COALESCE(%[1]s.output_unit_cost, 0))
		+ (ul.cached_tokens_in::numeric * COALESCE(%[1]s.cache_read_input_cost, %[1]s.input_unit_cost, 0))
		+ ((CASE
			WHEN ul.cache_creation_5m_tokens_in = 0 AND ul.cache_creation_1h_tokens_in = 0
			THEN ul.cache_creation_tokens_in
			ELSE ul.cache_creation_5m_tokens_in
		END)::numeric * COALESCE(%[1]s.cache_write_input_cost, %[1]s.input_unit_cost, 0))
		+ (ul.cache_creation_1h_tokens_in::numeric * COALESCE(%[1]s.cache_write_1h_input_cost, %[1]s.cache_write_input_cost, %[1]s.input_unit_cost, 0))) / 1000
`, alias)
}

// fusedUsageCostExpr prices a usage row against the account's cost basis and
// falls back to the legacy channel basis when the row has no account binding.
// A pre-migration row (provider_account_id IS NULL) never joins `am`, so it is
// priced exactly as it was charged at the time — re-pricing history from the
// account table would silently rewrite the past.
func fusedUsageCostExpr() string {
	return fmt.Sprintf("CASE WHEN am.account_id IS NOT NULL THEN (%s) ELSE (%s) END", usageCostExprFor("am"), usageCostExprFor("pm"))
}

// usageAccountJoin resolves both the account a usage row belongs to and the
// price basis to charge it against:
//
//   - provider_account_id is the primary attribution and needs no resolution;
//   - a row written before the migration carries only provider_id, which is
//     mapped onto the account migrated from that channel (legacy_acct).
//     Rows whose channel was never migrated keep the provider id as their
//     dimension — they stay visible as historical, account-less rows.
//
// `acct` is the account by primary attribution, `legacy_acct` the account found
// through the legacy channel id, `am`/`pm` the two price bases.
const usageAccountJoin = `
	LEFT JOIN upstream_accounts acct ON acct.id = ul.provider_account_id
	LEFT JOIN upstream_accounts legacy_acct
	       ON ul.provider_account_id IS NULL AND legacy_acct.legacy_provider_id = ul.provider_id
	LEFT JOIN upstream_account_models am
	       ON am.account_id = ul.provider_account_id AND am.model_name = ul.model
	LEFT JOIN provider_models pm
	       ON pm.provider_id = ul.provider_id AND pm.model_name = ul.model
`

// usageAccountDimensionExpr is the fused account dimension of a usage row: the
// account id when the row has one (directly or through its migrated channel),
// otherwise the legacy provider id.
const usageAccountDimensionExpr = `COALESCE(ul.provider_account_id, legacy_acct.id, ul.provider_id)`

// NormalizeFinanceRange returns a half-open [from, to) window.
//
// dateOnlyTo means the caller sent a calendar date with no time: that
// day is inclusive, so to is advanced to the next midnight. RFC3339
// timestamps are used as-is (already exclusive if the client sent
// end-of-day + 1s, or exclusive-now).
func NormalizeFinanceRange(from, to time.Time, dateOnlyFrom, dateOnlyTo bool) (time.Time, time.Time) {
	return normalizeRange(from, to, dateOnlyFrom, dateOnlyTo, maxFinanceSpan)
}

func NormalizeLedgerRange(from, to time.Time, dateOnlyFrom, dateOnlyTo bool) (time.Time, time.Time) {
	return normalizeRange(from, to, dateOnlyFrom, dateOnlyTo, maxLedgerSpan)
}

func normalizeRange(from, to time.Time, dateOnlyFrom, dateOnlyTo bool, maxSpan time.Duration) (time.Time, time.Time) {
	loc := Location()
	now := time.Now().In(loc)
	if to.IsZero() {
		to = now
	} else {
		to = to.In(loc)
		if dateOnlyTo {
			to = time.Date(to.Year(), to.Month(), to.Day(), 0, 0, 0, 0, loc).AddDate(0, 0, 1)
		}
	}
	if from.IsZero() {
		from = to.AddDate(0, 0, -30)
	} else {
		from = from.In(loc)
		if dateOnlyFrom {
			from = time.Date(from.Year(), from.Month(), from.Day(), 0, 0, 0, 0, loc)
		}
	}
	if !to.After(from) {
		from = to.Add(-24 * time.Hour)
	}
	if maxSpan > 0 && to.Sub(from) > maxSpan {
		from = to.Add(-maxSpan)
	}
	return from, to
}

type FinanceSummary struct {
	From             time.Time           `json:"from"`
	To               time.Time           `json:"to"`
	AgentIncome      decimal.Decimal     `json:"agent_income"`
	SiteIncome       decimal.Decimal     `json:"site_income"`
	TotalIncome      decimal.Decimal     `json:"total_income"`
	FixedCost        decimal.Decimal     `json:"fixed_cost"`
	ManualCost       decimal.Decimal     `json:"manual_cost"`
	TotalCost        decimal.Decimal     `json:"total_cost"`
	NetProfit        decimal.Decimal     `json:"net_profit"`
	AccountBreakdown []AccountFinanceRow `json:"providers"`
	Days             []FinanceDayPoint   `json:"days"`
}

// AccountFinanceRow is one row of the cost breakdown, keyed by ACCOUNT.
//
// The dimension is fused: cost rows carry an account id (upstream_account_costs
// / usage_logs.provider_account_id), while rows written before the account
// migration carry a channel id, which is folded onto the account migrated from
// it. A historical row whose channel was never migrated has no account to fold
// into; it keeps its provider id as the dimension and is flagged
// LegacyProviderHistory so the page can label it as pre-migration history.
type AccountFinanceRow struct {
	// AccountID is the account id, or — when LegacyProviderHistory is set — the
	// legacy channel id of a row that predates the migration.
	AccountID uuid.UUID `json:"account_id"`
	// AccountName is the account's label; for a legacy-only row it falls back to
	// the channel's name, the one display name history still has.
	AccountName string          `json:"account_name"`
	FixedCost   decimal.Decimal `json:"fixed_cost"`
	ManualCost  decimal.Decimal `json:"manual_cost"`
	TotalCost   decimal.Decimal `json:"total_cost"`
	// LegacyProviderHistory marks a dimension that exists only in the
	// pre-migration provider_costs / usage_logs.provider_id history.
	LegacyProviderHistory bool `json:"legacy_provider_history,omitempty"`
}

type FinanceDayPoint struct {
	Date    string          `json:"date"`
	Income  decimal.Decimal `json:"income"`
	Expense decimal.Decimal `json:"expense"`
}

type dayCurrencyAmount struct {
	day      string
	currency string
	amount   decimal.Decimal
}

// dayAccountCurrencyAmount is one day × one account dimension × one currency of
// channel cost, read from usage_logs (fixed/usage cost) or from the two manual
// cost tables.
type dayAccountCurrencyAmount struct {
	day      string
	account  accountCostDimension
	currency string
	amount   decimal.Decimal
}

// accountCostDimension is the fusion key every cost row is bucketed under: the
// account id, or — when the row predates the migration and its channel was
// never migrated — the legacy channel id, flagged so reports can label it.
type accountCostDimension struct {
	ID     uuid.UUID
	Legacy bool
}

// accountDimension folds an attribution id onto the account it belongs to.
// nameByAccount is the set of accounts that exist in this deployment (including
// soft-deleted ones, so history keeps its label): an id found there IS an
// account id; anything else is a pre-migration channel id.
func accountDimension(id uuid.UUID, nameByAccount map[uuid.UUID]string) accountCostDimension {
	if _, ok := nameByAccount[id]; ok {
		return accountCostDimension{ID: id}
	}
	return accountCostDimension{ID: id, Legacy: true}
}

func (d accountCostDimension) name(nameByAccount map[uuid.UUID]string, nameByLegacyProvider map[uuid.UUID]string) string {
	if !d.Legacy {
		return nameByAccount[d.ID]
	}
	return nameByLegacyProvider[d.ID]
}

type dayAmount struct {
	day    string
	amount decimal.Decimal
}

// Summary is one range scan of usage_logs (grouped by day + account) plus a
// handful of tiny ledger aggregations, run in parallel. The old path issued a
// usage_logs scan per provider; with N providers that was N full
// created_at-range scans of the largest table.
//
// Every cost dimension here is an ACCOUNT. The two retired channel tables
// (provider_costs / provider_models / usage_logs.provider_id) are read too, but
// only as history, fused onto the account migrated from that channel; a channel
// that was never migrated keeps its own row and is flagged as such.
func (s *FinanceSvc) Summary(ctx context.Context, from, to time.Time) (*FinanceSummary, error) {
	from, to = NormalizeFinanceRange(from, to, false, false)

	accounts, err := s.d.Ent.UpstreamAccount.Query().All(ctx)
	if err != nil {
		return nil, err
	}
	nameByAccount := make(map[uuid.UUID]string, len(accounts))
	for _, acct := range accounts {
		nameByAccount[acct.ID] = acct.Name
	}
	// The legacy channel table stays READ-ONLY history: it is only ever used to
	// resolve a display name for account-less pre-migration rows.
	legacyProviders, err := s.d.Ent.Provider.Query().Order(ent.Asc(entprovider.FieldName)).All(ctx)
	if err != nil {
		return nil, err
	}
	nameByLegacyProvider := make(map[uuid.UUID]string, len(legacyProviders))
	for _, p := range legacyProviders {
		nameByLegacyProvider[p.ID] = p.Name
	}

	var (
		usageRows  []dayAccountCurrencyAmount
		manualRows []dayAccountCurrencyAmount
		agentDays  []dayAmount
		orderDays  []dayCurrencyAmount
		refundDays []dayCurrencyAmount
	)

	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() error {
		rows, err := s.queryUsageCostByDayAccount(gctx, from, to, nameByAccount)
		if err != nil {
			return fmt.Errorf("usage cost: %w", err)
		}
		usageRows = rows
		return nil
	})
	g.Go(func() error {
		rows, err := s.queryManualCostByDayAccount(gctx, from, to, nameByAccount)
		if err != nil {
			return fmt.Errorf("manual cost: %w", err)
		}
		manualRows = rows
		return nil
	})
	g.Go(func() error {
		rows, err := s.queryDayAmount(gctx, `
			SELECT (created_at AT TIME ZONE $3)::date, COALESCE(SUM(platform_payable), 0)
			FROM agent_settlement_entries
			WHERE created_at >= $1 AND created_at < $2
			  -- A withdrawal pays an existing rebate into balance; the rebate
			  -- row already counted the cost.
			  AND kind <> 'balance_withdraw'
			GROUP BY 1
		`, from, to)
		if err != nil {
			return fmt.Errorf("agent income: %w", err)
		}
		agentDays = rows
		return nil
	})
	g.Go(func() error {
		rows, err := s.queryDayCurrencyAmount(gctx, `
			SELECT (paid_at AT TIME ZONE $3)::date,
			       COALESCE(NULLIF(UPPER(currency), ''), 'CNY'),
			       COALESCE(SUM(amount), 0)
			FROM site_orders
			WHERE status IN ('paid','partially_refunded','refunded')
			  AND paid_at >= $1 AND paid_at < $2
			GROUP BY 1, 2
		`, from, to)
		if err != nil {
			return fmt.Errorf("site orders: %w", err)
		}
		orderDays = rows
		return nil
	})
	g.Go(func() error {
		rows, err := s.queryDayCurrencyAmount(gctx, `
			SELECT (reviewed_at AT TIME ZONE $3)::date,
			       COALESCE(NULLIF(UPPER(currency), ''), 'CNY'),
			       COALESCE(SUM(amount), 0)
			FROM site_refunds
			WHERE status = 'approved' AND reviewed_at >= $1 AND reviewed_at < $2
			GROUP BY 1, 2
		`, from, to)
		if err != nil {
			return fmt.Errorf("site refunds: %w", err)
		}
		refundDays = rows
		return nil
	})
	if err := g.Wait(); err != nil {
		return nil, err
	}

	rate := s.cnyRate()
	fixedByAccount := map[accountCostDimension]decimal.Decimal{}
	manualByAccount := map[accountCostDimension]decimal.Decimal{}
	incomeByDay := map[string]decimal.Decimal{}
	expenseByDay := map[string]decimal.Decimal{}

	addDay := func(dst map[string]decimal.Decimal, day string, v decimal.Decimal) {
		dst[day] = dst[day].Add(v)
	}

	var fixedCost, manualCost decimal.Decimal
	for _, row := range usageRows {
		amt := ToCNY(row.amount, row.currency, rate)
		fixedByAccount[row.account] = fixedByAccount[row.account].Add(amt)
		fixedCost = fixedCost.Add(amt)
		addDay(expenseByDay, row.day, amt)
	}
	for _, row := range manualRows {
		amt := ToCNY(row.amount, row.currency, rate)
		manualByAccount[row.account] = manualByAccount[row.account].Add(amt)
		manualCost = manualCost.Add(amt)
		addDay(expenseByDay, row.day, amt)
	}

	var agentIncome decimal.Decimal
	for _, row := range agentDays {
		agentIncome = agentIncome.Add(row.amount)
		addDay(incomeByDay, row.day, row.amount)
	}

	var siteOrders, siteRefunds decimal.Decimal
	for _, row := range orderDays {
		amt := ToCNY(row.amount, row.currency, rate)
		siteOrders = siteOrders.Add(amt)
		addDay(incomeByDay, row.day, amt)
	}
	for _, row := range refundDays {
		amt := ToCNY(row.amount, row.currency, rate)
		siteRefunds = siteRefunds.Add(amt)
		addDay(incomeByDay, row.day, amt.Neg())
	}

	siteIncome := siteOrders.Sub(siteRefunds)
	totalIncome := agentIncome.Add(siteIncome)
	totalCost := fixedCost.Add(manualCost)

	// Accounts with a name are listed first (declaration order from the account
	// query); a legacy-only dimension is appended after them so history whose
	// channel was never migrated stays visible instead of silently vanishing.
	breakdown := make([]AccountFinanceRow, 0, len(accounts))
	seen := make(map[accountCostDimension]struct{}, len(accounts))
	appendRow := func(key accountCostDimension) {
		fixed, manual := fixedByAccount[key], manualByAccount[key]
		total := fixed.Add(manual)
		if total.Sign() == 0 {
			return
		}
		seen[key] = struct{}{}
		breakdown = append(breakdown, AccountFinanceRow{
			AccountID:             key.ID,
			AccountName:           key.name(nameByAccount, nameByLegacyProvider),
			FixedCost:             fixed,
			ManualCost:            manual,
			TotalCost:             total,
			LegacyProviderHistory: key.Legacy,
		})
	}
	for _, acct := range accounts {
		appendRow(accountCostDimension{ID: acct.ID})
	}
	remaining := make(map[accountCostDimension]struct{})
	for key := range fixedByAccount {
		if _, ok := seen[key]; !ok {
			remaining[key] = struct{}{}
		}
	}
	for key := range manualByAccount {
		if _, ok := seen[key]; !ok {
			remaining[key] = struct{}{}
		}
	}
	legacyKeys := make([]accountCostDimension, 0, len(remaining))
	for key := range remaining {
		legacyKeys = append(legacyKeys, key)
	}
	sort.Slice(legacyKeys, func(i, j int) bool {
		return legacyKeys[i].name(nameByAccount, nameByLegacyProvider) < legacyKeys[j].name(nameByAccount, nameByLegacyProvider)
	})
	for _, key := range legacyKeys {
		appendRow(key)
	}

	out := &FinanceSummary{
		From:             from,
		To:               to,
		AgentIncome:      agentIncome,
		SiteIncome:       siteIncome,
		TotalIncome:      totalIncome,
		FixedCost:        fixedCost,
		ManualCost:       manualCost,
		TotalCost:        totalCost,
		NetProfit:        totalIncome.Sub(totalCost),
		AccountBreakdown: breakdown,
		Days:             fillFinanceDays(from, to, incomeByDay, expenseByDay),
	}
	return out, nil
}

func fillFinanceDays(from, to time.Time, income, expense map[string]decimal.Decimal) []FinanceDayPoint {
	if income == nil {
		income = map[string]decimal.Decimal{}
	}
	if expense == nil {
		expense = map[string]decimal.Decimal{}
	}
	loc := Location()
	start := time.Date(from.In(loc).Year(), from.In(loc).Month(), from.In(loc).Day(), 0, 0, 0, 0, loc)
	end := to.In(loc)
	days := make([]FinanceDayPoint, 0, int(end.Sub(start).Hours()/24)+1)
	for d := start; d.Before(end); d = d.AddDate(0, 0, 1) {
		key := d.Format("2006-01-02")
		days = append(days, FinanceDayPoint{
			Date:    key,
			Income:  income[key],
			Expense: expense[key],
		})
	}
	return days
}

// queryUsageCostByDayAccount is one scan of usage_logs grouped by day, account
// dimension and currency. The dimension is the account id the row is attributed
// to (provider_account_id, or the account migrated from the row's legacy
// provider_id); a row whose channel was never migrated keeps its provider id and
// is flagged as legacy history by the caller.
func (s *FinanceSvc) queryUsageCostByDayAccount(ctx context.Context, from, to time.Time, nameByAccount map[uuid.UUID]string) ([]dayAccountCurrencyAmount, error) {
	q := fmt.Sprintf(`
		SELECT (ul.created_at AT TIME ZONE $3)::date,
		       %s,
		       COALESCE(NULLIF(UPPER(am.currency), ''), NULLIF(UPPER(pm.currency), ''), 'CNY'),
		       COALESCE(SUM(%s), 0)
		FROM usage_logs ul
		%s
		WHERE ul.created_at >= $1 AND ul.created_at < $2
		  AND %s IS NOT NULL
		GROUP BY 1, 2, 3
	`, usageAccountDimensionExpr, fusedUsageCostExpr(), usageAccountJoin, usageAccountDimensionExpr)
	rows, err := s.d.DB().QueryContext(ctx, q, from, to, TimezoneName())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]dayAccountCurrencyAmount, 0, 64)
	for rows.Next() {
		var day time.Time
		var accountID uuid.UUID
		var row dayAccountCurrencyAmount
		if err := rows.Scan(&day, &accountID, &row.currency, &row.amount); err != nil {
			return nil, err
		}
		row.day = day.Format("2006-01-02")
		row.account = accountDimension(accountID, nameByAccount)
		out = append(out, row)
	}
	return out, rows.Err()
}

// queryManualCostByDayAccount fuses the two manual-cost tables by account:
// upstream_account_costs (the account table, written by CreateAccountCost) and
// provider_costs (READ-ONLY history, folded onto the account migrated from its
// channel). Rows the fusion cannot place stay keyed by provider id.
func (s *FinanceSvc) queryManualCostByDayAccount(ctx context.Context, from, to time.Time, nameByAccount map[uuid.UUID]string) ([]dayAccountCurrencyAmount, error) {
	out := make([]dayAccountCurrencyAmount, 0, 16)
	accountRows, err := s.d.DB().QueryContext(ctx, `
		SELECT (occurred_at AT TIME ZONE $3)::date,
		       account_id,
		       COALESCE(NULLIF(UPPER(currency), ''), 'CNY'),
		       COALESCE(SUM(amount), 0)
		FROM upstream_account_costs
		WHERE occurred_at >= $1 AND occurred_at < $2
		GROUP BY 1, 2, 3
	`, from, to, TimezoneName())
	if err != nil {
		return nil, err
	}
	defer accountRows.Close()
	for accountRows.Next() {
		var day time.Time
		var accountID uuid.UUID
		var row dayAccountCurrencyAmount
		if err := accountRows.Scan(&day, &accountID, &row.currency, &row.amount); err != nil {
			return nil, err
		}
		row.day = day.Format("2006-01-02")
		row.account = accountDimension(accountID, nameByAccount)
		out = append(out, row)
	}
	if err := accountRows.Err(); err != nil {
		return nil, err
	}

	legacyRows, err := s.d.DB().QueryContext(ctx, `
		SELECT (pc.occurred_at AT TIME ZONE $3)::date,
		       COALESCE(acct.id, pc.provider_id),
		       COALESCE(NULLIF(UPPER(pc.currency), ''), 'CNY'),
		       COALESCE(SUM(pc.amount), 0)
		FROM provider_costs pc
		LEFT JOIN upstream_accounts acct ON acct.legacy_provider_id = pc.provider_id
		WHERE pc.occurred_at >= $1 AND pc.occurred_at < $2
		GROUP BY 1, 2, 3
	`, from, to, TimezoneName())
	if err != nil {
		return nil, err
	}
	defer legacyRows.Close()
	for legacyRows.Next() {
		var day time.Time
		var accountID uuid.UUID
		var row dayAccountCurrencyAmount
		if err := legacyRows.Scan(&day, &accountID, &row.currency, &row.amount); err != nil {
			return nil, err
		}
		row.day = day.Format("2006-01-02")
		row.account = accountDimension(accountID, nameByAccount)
		out = append(out, row)
	}
	return out, legacyRows.Err()
}

func (s *FinanceSvc) queryDayAmount(ctx context.Context, query string, from, to time.Time) ([]dayAmount, error) {
	rows, err := s.d.DB().QueryContext(ctx, query, from, to, TimezoneName())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]dayAmount, 0, 32)
	for rows.Next() {
		var day time.Time
		var row dayAmount
		if err := rows.Scan(&day, &row.amount); err != nil {
			return nil, err
		}
		row.day = day.Format("2006-01-02")
		out = append(out, row)
	}
	return out, rows.Err()
}

func (s *FinanceSvc) queryDayCurrencyAmount(ctx context.Context, query string, from, to time.Time) ([]dayCurrencyAmount, error) {
	rows, err := s.d.DB().QueryContext(ctx, query, from, to, TimezoneName())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]dayCurrencyAmount, 0, 32)
	for rows.Next() {
		var day time.Time
		var row dayCurrencyAmount
		if err := rows.Scan(&day, &row.currency, &row.amount); err != nil {
			return nil, err
		}
		row.day = day.Format("2006-01-02")
		out = append(out, row)
	}
	return out, rows.Err()
}

func (s *FinanceSvc) resolveRefundTickets(ctx context.Context, orderID uuid.UUID) {
	_, _ = s.d.Ent.Ticket.Update().
		Where(
			entticket.OrderIDEQ(orderID),
			entticket.CategoryEQ(entticket.CategoryRefund),
			entticket.StatusIn(
				entticket.StatusOpen,
				entticket.StatusInProgress,
				entticket.StatusWaitingUser,
			),
		).
		SetStatus(entticket.StatusResolved).
		Save(ctx)
}
