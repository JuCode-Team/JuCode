package biz

import (
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/shopspring/decimal"
	"github.com/xuri/excelize/v2"
)

const maxOpsSpan = 3 * 366 * 24 * time.Hour

type OpsReport struct {
	From          time.Time       `json:"from"`
	To            time.Time       `json:"to"`
	Grain         string          `json:"grain"`
	KPI           OpsKPI          `json:"kpi"`
	Compare       OpsCompare      `json:"compare"`
	Funnel        OpsFunnel       `json:"funnel"`
	Orders        OpsOrderQuality `json:"orders"`
	Plans         OpsPlanSnap     `json:"plans"`
	Series        []OpsPoint      `json:"series"`
	Cohorts       []OpsCohort     `json:"cohorts"`
	TopPayers     []OpsPayer      `json:"top_payers"`
	KindSplit     []OpsKindRow    `json:"kind_split"`
	PlanMix       []OpsNamedCount `json:"plan_mix"`
	PlanSales     []OpsNamedAmt   `json:"plan_sales"`
	ConsumeScope  []OpsNamedAmt   `json:"consume_scope"`
	ConsumeSource []OpsNamedAmt   `json:"consume_source"`
}

type OpsKPI struct {
	GrossRevenue   string `json:"gross_revenue"`
	Refunds        string `json:"refunds"`
	NetRevenue     string `json:"net_revenue"`
	PayingUsers    int    `json:"paying_users"`
	NewPayingUsers int    `json:"new_paying_users"`
	PeriodActive   int    `json:"period_active"`
	ARPU           string `json:"arpu"`
	ARPPU          string `json:"arppu"`
	NewUsers       int    `json:"new_users"`
	PayConversion  string `json:"pay_conversion"`
	DAU            int    `json:"dau"`
	WAU            int    `json:"wau"`
	MAU            int    `json:"mau"`
	Stickiness     string `json:"stickiness"`
	Requests       int64  `json:"requests"`
	RepeatPayRate  string `json:"repeat_pay_rate"`
}

type OpsCompare struct {
	From            time.Time `json:"from"`
	To              time.Time `json:"to"`
	NetRevenue      string    `json:"net_revenue"`
	NewUsers        int       `json:"new_users"`
	PayingUsers     int       `json:"paying_users"`
	Requests        int64     `json:"requests"`
	NetRevenueDelta string    `json:"net_revenue_delta"`
	NewUsersDelta   string    `json:"new_users_delta"`
	PayingDelta     string    `json:"paying_users_delta"`
	RequestsDelta   string    `json:"requests_delta"`
}

type OpsFunnel struct {
	SignedUp int    `json:"signed_up"`
	Called   int    `json:"called"`
	Paid     int    `json:"paid"`
	CallRate string `json:"call_rate"`
	PayRate  string `json:"pay_rate"`
}

type OpsOrderQuality struct {
	PaidOrders     int    `json:"paid_orders"`
	AOV            string `json:"aov"`
	RefundRate     string `json:"refund_rate"`
	Created        int    `json:"created"`
	CreatedPaid    int    `json:"created_paid"`
	CreatedPending int    `json:"created_pending"`
	PayRate        string `json:"pay_rate"`
}

type OpsPlanSnap struct {
	Active   int `json:"active"`
	Opened   int `json:"opened"`
	Expired  int `json:"expired"`
	Canceled int `json:"canceled"`
}

type OpsPoint struct {
	Period      string `json:"period"`
	NewUsers    int    `json:"new_users"`
	ActiveUsers int    `json:"active_users"`
	PayingUsers int    `json:"paying_users"`
	Revenue     string `json:"revenue"`
	Refunds     string `json:"refunds"`
	Net         string `json:"net"`
	Requests    int64  `json:"requests"`
}

type OpsCohort struct {
	Month    string `json:"month"`
	NewUsers int    `json:"new_users"`
	Retained int    `json:"retained"`
	Rate     string `json:"rate"`
}

type OpsPayer struct {
	UserID string `json:"user_id"`
	Email  string `json:"email"`
	Orders int    `json:"orders"`
	Amount string `json:"amount"`
}

type OpsKindRow struct {
	Kind   string `json:"kind"`
	Orders int    `json:"orders"`
	Amount string `json:"amount"`
}

type OpsNamedCount struct {
	Key   string `json:"key"`
	Label string `json:"label"`
	Count int    `json:"count"`
}

type OpsNamedAmt struct {
	Key    string `json:"key"`
	Label  string `json:"label"`
	Count  int    `json:"count"`
	Amount string `json:"amount"`
}

func ParseOpsGrain(raw string, from, to time.Time) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "day", "month":
		return strings.ToLower(strings.TrimSpace(raw))
	}
	if to.Sub(from) > 90*24*time.Hour {
		return "month"
	}
	return "day"
}

func RatioString(num, den int) string {
	if den <= 0 {
		return "0"
	}
	return decimal.NewFromInt(int64(num)).Div(decimal.NewFromInt(int64(den))).StringFixed(4)
}

func PerUserAmount(amount decimal.Decimal, users int) string {
	if users <= 0 {
		return "0"
	}
	return amount.Div(decimal.NewFromInt(int64(users))).StringFixed(4)
}

func DeltaRatioDec(cur, prev decimal.Decimal) string {
	if prev.IsZero() {
		return ""
	}
	return cur.Sub(prev).Div(prev.Abs()).StringFixed(4)
}

func DeltaRatioInt(cur, prev int) string {
	if prev == 0 {
		return ""
	}
	return decimal.NewFromInt(int64(cur - prev)).Div(decimal.NewFromInt(int64(prev))).StringFixed(4)
}

func DeltaRatioInt64(cur, prev int64) string {
	if prev == 0 {
		return ""
	}
	return decimal.NewFromInt(cur - prev).Div(decimal.NewFromInt(prev)).StringFixed(4)
}

func NormalizeOpsRange(from, to time.Time, dateOnlyFrom, dateOnlyTo bool) (time.Time, time.Time) {
	return normalizeRange(from, to, dateOnlyFrom, dateOnlyTo, maxOpsSpan)
}

func (s *FinanceSvc) OpsReport(ctx context.Context, from, to time.Time, grain string) (*OpsReport, error) {
	from, to = NormalizeOpsRange(from, to, false, false)
	grain = ParseOpsGrain(grain, from, to)
	loc := Location()
	trunc := `(created_at AT TIME ZONE $3)`
	if grain == "month" {
		trunc = `date_trunc('month', created_at AT TIME ZONE $3)`
	}
	paidTrunc := strings.ReplaceAll(trunc, "created_at", "paid_at")
	refundTrunc := strings.ReplaceAll(trunc, "created_at", "reviewed_at")

	scanDec := func(q string, args ...any) (map[string]decimal.Decimal, map[string]int, error) {
		rows, err := s.d.DB().QueryContext(ctx, q, args...)
		if err != nil {
			return nil, nil, err
		}
		defer rows.Close()
		amts := map[string]decimal.Decimal{}
		counts := map[string]int{}
		for rows.Next() {
			var day time.Time
			var n int
			var amt decimal.Decimal
			if err := rows.Scan(&day, &n, &amt); err != nil {
				return nil, nil, err
			}
			key := formatOpsPeriod(day, grain, loc)
			amts[key] = amt
			counts[key] = n
		}
		return amts, counts, rows.Err()
	}
	scanInt := func(q string, args ...any) (map[string]int, error) {
		rows, err := s.d.DB().QueryContext(ctx, q, args...)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		out := map[string]int{}
		for rows.Next() {
			var day time.Time
			var n int
			if err := rows.Scan(&day, &n); err != nil {
				return nil, err
			}
			out[formatOpsPeriod(day, grain, loc)] = n
		}
		return out, rows.Err()
	}

	revAmt, payUsersByPeriod, err := scanDec(fmt.Sprintf(`
		SELECT %s::date, COUNT(DISTINCT user_id)::int, COALESCE(SUM(amount), 0)
		FROM site_orders
		WHERE status IN ('paid','partially_refunded','refunded')
		  AND paid_at >= $1 AND paid_at < $2
		GROUP BY 1 ORDER BY 1
	`, paidTrunc), from, to, TimezoneName())
	if err != nil {
		return nil, fmt.Errorf("ops revenue: %w", err)
	}
	refAmt, _, err := scanDec(fmt.Sprintf(`
		SELECT %s::date, COUNT(*)::int, COALESCE(SUM(amount), 0)
		FROM site_refunds
		WHERE status = 'approved' AND reviewed_at >= $1 AND reviewed_at < $2
		GROUP BY 1 ORDER BY 1
	`, refundTrunc), from, to, TimezoneName())
	if err != nil {
		return nil, fmt.Errorf("ops refunds: %w", err)
	}
	newUsers, err := scanInt(fmt.Sprintf(`
		SELECT %s::date, COUNT(*)::int FROM users
		WHERE created_at >= $1 AND created_at < $2 GROUP BY 1 ORDER BY 1
	`, trunc), from, to, TimezoneName())
	if err != nil {
		return nil, fmt.Errorf("ops new users: %w", err)
	}
	activeUsers := map[string]int{}
	reqByPeriod := map[string]int64{}
	usageRows, err := s.d.DB().QueryContext(ctx, fmt.Sprintf(`
		SELECT %s::date, COUNT(DISTINCT user_id)::int, COUNT(*)::bigint
		FROM usage_logs
		WHERE created_at >= $1 AND created_at < $2
		GROUP BY 1 ORDER BY 1
	`, trunc), from, to, TimezoneName())
	if err != nil {
		return nil, fmt.Errorf("ops usage series: %w", err)
	}
	for usageRows.Next() {
		var day time.Time
		var users int
		var reqs int64
		if err := usageRows.Scan(&day, &users, &reqs); err != nil {
			usageRows.Close()
			return nil, err
		}
		key := formatOpsPeriod(day, grain, loc)
		activeUsers[key] = users
		reqByPeriod[key] = reqs
	}
	err = usageRows.Err()
	usageRows.Close()
	if err != nil {
		return nil, err
	}

	var payingUsers, newPaying, repeatPayers, newUsersN, newUserPaid, newUserCalled, periodActive int
	var paidOrders, createdOrders, createdPaid, createdPending int
	var gross, refunds decimal.Decimal
	var requests int64
	if err := s.d.DB().QueryRowContext(ctx, `
		SELECT COALESCE(SUM(amt), 0), COUNT(*)::int, COALESCE(SUM(cnt), 0)::int,
		       COUNT(*) FILTER (WHERE cnt >= 2)::int
		FROM (
			SELECT user_id, SUM(amount) AS amt, COUNT(*) AS cnt
			FROM site_orders
			WHERE status IN ('paid','partially_refunded','refunded') AND paid_at >= $1 AND paid_at < $2
			GROUP BY user_id
		) t
	`, from, to).Scan(&gross, &payingUsers, &paidOrders, &repeatPayers); err != nil {
		return nil, err
	}
	if err := s.d.DB().QueryRowContext(ctx, `
		SELECT COALESCE(SUM(amount), 0) FROM site_refunds
		WHERE status = 'approved' AND reviewed_at >= $1 AND reviewed_at < $2
	`, from, to).Scan(&refunds); err != nil {
		return nil, err
	}
	if err := s.d.DB().QueryRowContext(ctx, `
		SELECT
		  COUNT(*)::int,
		  COUNT(*) FILTER (WHERE paid_at IS NOT NULL AND paid_at < $2)::int,
		  COUNT(*) FILTER (WHERE (paid_at IS NULL OR paid_at >= $2) AND status <> 'canceled')::int
		FROM site_orders
		WHERE created_at >= $1 AND created_at < $2
	`, from, to).Scan(&createdOrders, &createdPaid, &createdPending); err != nil {
		return nil, err
	}
	if err := s.d.DB().QueryRowContext(ctx, `
		SELECT COUNT(*) FROM (
			SELECT user_id, MIN(paid_at) AS first_paid FROM site_orders
			WHERE status IN ('paid','partially_refunded','refunded') AND paid_at IS NOT NULL
			GROUP BY user_id
		) t WHERE first_paid >= $1 AND first_paid < $2
	`, from, to).Scan(&newPaying); err != nil {
		return nil, err
	}
	if err := s.d.DB().QueryRowContext(ctx, `
		SELECT
		  COUNT(*)::int,
		  COUNT(*) FILTER (WHERE EXISTS (
		    SELECT 1 FROM usage_logs ul WHERE ul.user_id = u.id AND ul.created_at < $2
		  ))::int,
		  COUNT(*) FILTER (WHERE EXISTS (
		    SELECT 1 FROM site_orders o
		    WHERE o.user_id = u.id
		      AND o.status IN ('paid','partially_refunded','refunded')
		      AND o.paid_at IS NOT NULL AND o.paid_at < $2
		  ))::int
		FROM users u
		WHERE u.created_at >= $1 AND u.created_at < $2
	`, from, to).Scan(&newUsersN, &newUserCalled, &newUserPaid); err != nil {
		return nil, err
	}
	var consumeScope, consumeSource []OpsNamedAmt
	consumeScope, consumeSource, requests, periodActive, err = s.queryOpsUsageRollup(ctx, from, to)
	if err != nil {
		return nil, err
	}

	var dau, wau, mau int

	if err := s.d.DB().QueryRowContext(ctx, `
		SELECT
		  COUNT(DISTINCT user_id) FILTER (WHERE created_at >= $1)::int,
		  COUNT(DISTINCT user_id) FILTER (WHERE created_at >= $2)::int,
		  COUNT(DISTINCT user_id)::int
		FROM usage_logs
		WHERE created_at >= $3 AND created_at < $4
	`, to.Add(-24*time.Hour), to.Add(-7*24*time.Hour), to.Add(-30*24*time.Hour), to).Scan(&dau, &wau, &mau); err != nil {
		return nil, fmt.Errorf("ops dau: %w", err)
	}

	var plans OpsPlanSnap

	if err := s.d.DB().QueryRowContext(ctx, `
		SELECT
		  COUNT(*) FILTER (WHERE status = 'active' AND start_at < $2 AND expire_at > $2)::int,
		  COUNT(*) FILTER (WHERE start_at >= $1 AND start_at < $2)::int,
		  COUNT(*) FILTER (WHERE expire_at >= $1 AND expire_at < $2 AND status <> 'canceled')::int,
		  COUNT(*) FILTER (WHERE status = 'canceled' AND updated_at >= $1 AND updated_at < $2)::int
		FROM user_plans
	`, from, to).Scan(&plans.Active, &plans.Opened, &plans.Expired, &plans.Canceled); err != nil {
		return nil, fmt.Errorf("ops plans: %w", err)
	}

	net := gross.Sub(refunds)
	series := make([]OpsPoint, 0)
	for _, p := range fillOpsPeriods(from, to, grain, loc) {
		rev := revAmt[p]
		ref := refAmt[p]
		series = append(series, OpsPoint{
			Period: p, NewUsers: newUsers[p], ActiveUsers: activeUsers[p],
			PayingUsers: payUsersByPeriod[p],
			Revenue:     rev.StringFixed(4), Refunds: ref.StringFixed(4),
			Net: rev.Sub(ref).StringFixed(4), Requests: reqByPeriod[p],
		})
	}
	cohorts, err := s.queryOpsCohorts(ctx, from, to)
	if err != nil {
		return nil, err
	}
	payers, err := s.queryOpsTopPayers(ctx, from, to)
	if err != nil {
		return nil, err
	}
	kinds, err := s.queryOpsKindSplit(ctx, from, to)
	if err != nil {
		return nil, err
	}
	planMix, err := s.queryOpsPlanMix(ctx, to)
	if err != nil {
		return nil, err
	}
	planSales, err := s.queryOpsPlanSales(ctx, from, to)
	if err != nil {
		return nil, err
	}
	compare, err := s.queryOpsCompare(ctx, from, to, net, newUsersN, payingUsers, requests)
	if err != nil {
		return nil, err
	}

	return &OpsReport{
		From: from, To: to, Grain: grain,
		KPI: OpsKPI{
			GrossRevenue:   gross.StringFixed(4),
			Refunds:        refunds.StringFixed(4),
			NetRevenue:     net.StringFixed(4),
			PayingUsers:    payingUsers,
			NewPayingUsers: newPaying,
			PeriodActive:   periodActive,
			ARPU:           PerUserAmount(net, periodActive),
			ARPPU:          PerUserAmount(net, payingUsers),
			NewUsers:       newUsersN,
			PayConversion:  RatioString(newUserPaid, newUsersN),
			DAU:            dau,
			WAU:            wau,
			MAU:            mau,
			Stickiness:     RatioString(dau, mau),
			Requests:       requests,
			RepeatPayRate:  RatioString(repeatPayers, payingUsers),
		},
		Compare: compare,
		Funnel: OpsFunnel{
			SignedUp: newUsersN,
			Called:   newUserCalled,
			Paid:     newUserPaid,
			CallRate: RatioString(newUserCalled, newUsersN),
			PayRate:  RatioString(newUserPaid, newUsersN),
		},
		Orders: OpsOrderQuality{
			PaidOrders:     paidOrders,
			AOV:            PerUserAmount(gross, paidOrders),
			RefundRate:     refundRate(refunds, gross),
			Created:        createdOrders,
			CreatedPaid:    createdPaid,
			CreatedPending: createdPending,
			PayRate:        RatioString(createdPaid, createdOrders),
		},
		Plans:         plans,
		Series:        series,
		Cohorts:       cohorts,
		TopPayers:     payers,
		KindSplit:     kinds,
		PlanMix:       planMix,
		PlanSales:     planSales,
		ConsumeScope:  consumeScope,
		ConsumeSource: consumeSource,
	}, nil
}

func refundRate(refunds, gross decimal.Decimal) string {
	if gross.IsZero() {
		return "0"
	}
	return refunds.Div(gross).StringFixed(4)
}

func consumeScopeLabel(key string) string {
	if key == "organization" {
		return "组织"
	}
	return "个人"
}

func consumeSourceLabel(key string) string {
	switch key {
	case "plan":
		return "套餐额度"
	case "plan_balance":
		return "按量套餐"
	case "balance":
		return "个人余额"
	case "organization_balance":
		return "组织余额"
	case "none":
		return "未计费"
	default:
		return key
	}
}

func (s *FinanceSvc) queryOpsCompare(ctx context.Context, from, to time.Time, net decimal.Decimal, newUsers, payingUsers int, requests int64) (OpsCompare, error) {
	span := to.Sub(from)
	prevTo := from
	prevFrom := from.Add(-span)
	var prevGross, prevRefunds decimal.Decimal
	var prevNew, prevPaying int
	var prevReq int64
	if err := s.d.DB().QueryRowContext(ctx, `
		SELECT COALESCE(SUM(amount), 0) FROM site_orders
		WHERE status IN ('paid','partially_refunded','refunded') AND paid_at >= $1 AND paid_at < $2
	`, prevFrom, prevTo).Scan(&prevGross); err != nil {
		return OpsCompare{}, err
	}
	if err := s.d.DB().QueryRowContext(ctx, `
		SELECT COALESCE(SUM(amount), 0) FROM site_refunds
		WHERE status = 'approved' AND reviewed_at >= $1 AND reviewed_at < $2
	`, prevFrom, prevTo).Scan(&prevRefunds); err != nil {
		return OpsCompare{}, err
	}
	if err := s.d.DB().QueryRowContext(ctx, `
		SELECT COUNT(*) FROM users WHERE created_at >= $1 AND created_at < $2
	`, prevFrom, prevTo).Scan(&prevNew); err != nil {
		return OpsCompare{}, err
	}
	if err := s.d.DB().QueryRowContext(ctx, `
		SELECT COUNT(DISTINCT user_id) FROM site_orders
		WHERE status IN ('paid','partially_refunded','refunded') AND paid_at >= $1 AND paid_at < $2
	`, prevFrom, prevTo).Scan(&prevPaying); err != nil {
		return OpsCompare{}, err
	}
	if err := s.d.DB().QueryRowContext(ctx, `
		SELECT COUNT(*) FROM usage_logs WHERE created_at >= $1 AND created_at < $2
	`, prevFrom, prevTo).Scan(&prevReq); err != nil {
		return OpsCompare{}, err
	}
	prevNet := prevGross.Sub(prevRefunds)
	return OpsCompare{
		From:            prevFrom,
		To:              prevTo,
		NetRevenue:      prevNet.StringFixed(4),
		NewUsers:        prevNew,
		PayingUsers:     prevPaying,
		Requests:        prevReq,
		NetRevenueDelta: DeltaRatioDec(net, prevNet),
		NewUsersDelta:   DeltaRatioInt(newUsers, prevNew),
		PayingDelta:     DeltaRatioInt(payingUsers, prevPaying),
		RequestsDelta:   DeltaRatioInt64(requests, prevReq),
	}, nil
}

func (s *FinanceSvc) queryOpsCohorts(ctx context.Context, from, to time.Time) ([]OpsCohort, error) {
	rows, err := s.d.DB().QueryContext(ctx, `
		WITH new_u AS (
			SELECT id, date_trunc('month', created_at AT TIME ZONE $3)::date AS cohort
			FROM users WHERE created_at >= $1 AND created_at < $2
		)
		SELECT n.cohort, COUNT(*)::int,
		       COUNT(*) FILTER (WHERE EXISTS (
		         SELECT 1 FROM usage_logs ul
		         WHERE ul.user_id = n.id
		           AND ul.created_at >= ((n.cohort + INTERVAL '1 month') AT TIME ZONE $3)
		           AND ul.created_at < ((n.cohort + INTERVAL '2 month') AT TIME ZONE $3)
		       ))::int
		FROM new_u n
		WHERE ((n.cohort + INTERVAL '2 month') AT TIME ZONE $3) <= $2
		GROUP BY 1 ORDER BY 1
	`, from, to, TimezoneName())
	if err != nil {
		return nil, fmt.Errorf("ops cohorts: %w", err)
	}
	defer rows.Close()
	var out []OpsCohort
	for rows.Next() {
		var day time.Time
		var neu, ret int
		if err := rows.Scan(&day, &neu, &ret); err != nil {
			return nil, err
		}
		out = append(out, OpsCohort{Month: day.Format("2006-01"), NewUsers: neu, Retained: ret, Rate: RatioString(ret, neu)})
	}
	return out, rows.Err()
}

func (s *FinanceSvc) queryOpsTopPayers(ctx context.Context, from, to time.Time) ([]OpsPayer, error) {
	rows, err := s.d.DB().QueryContext(ctx, `
		SELECT o.user_id::text, COALESCE(u.email, ''), COUNT(*)::int, COALESCE(SUM(o.amount), 0)
		FROM site_orders o
		LEFT JOIN users u ON u.id = o.user_id
		WHERE o.status IN ('paid','partially_refunded','refunded')
		  AND o.paid_at >= $1 AND o.paid_at < $2
		GROUP BY 1, 2 ORDER BY 4 DESC LIMIT 20
	`, from, to)
	if err != nil {
		return nil, fmt.Errorf("ops payers: %w", err)
	}
	defer rows.Close()
	var out []OpsPayer
	for rows.Next() {
		var id, email string
		var n int
		var amt decimal.Decimal
		if err := rows.Scan(&id, &email, &n, &amt); err != nil {
			return nil, err
		}
		out = append(out, OpsPayer{UserID: id, Email: email, Orders: n, Amount: amt.StringFixed(4)})
	}
	return out, rows.Err()
}

func (s *FinanceSvc) queryOpsKindSplit(ctx context.Context, from, to time.Time) ([]OpsKindRow, error) {
	rows, err := s.d.DB().QueryContext(ctx, `
		SELECT kind::text, COUNT(*)::int, COALESCE(SUM(amount), 0)
		FROM site_orders
		WHERE status IN ('paid','partially_refunded','refunded')
		  AND paid_at >= $1 AND paid_at < $2
		GROUP BY 1 ORDER BY 3 DESC
	`, from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []OpsKindRow
	for rows.Next() {
		var kind string
		var n int
		var amt decimal.Decimal
		if err := rows.Scan(&kind, &n, &amt); err != nil {
			return nil, err
		}
		out = append(out, OpsKindRow{Kind: kind, Orders: n, Amount: amt.StringFixed(4)})
	}
	return out, rows.Err()
}

func (s *FinanceSvc) queryOpsPlanMix(ctx context.Context, at time.Time) ([]OpsNamedCount, error) {
	rows, err := s.d.DB().QueryContext(ctx, `
		SELECT p.id::text, p.name, COUNT(*)::int
		FROM user_plans up
		JOIN plans p ON p.id = up.plan_id
		WHERE up.status = 'active' AND up.start_at < $1 AND up.expire_at > $1
		GROUP BY 1, 2 ORDER BY 3 DESC
	`, at)
	if err != nil {
		return nil, fmt.Errorf("ops plan mix: %w", err)
	}
	defer rows.Close()
	var out []OpsNamedCount
	for rows.Next() {
		var key, label string
		var n int
		if err := rows.Scan(&key, &label, &n); err != nil {
			return nil, err
		}
		out = append(out, OpsNamedCount{Key: key, Label: label, Count: n})
	}
	return out, rows.Err()
}

func (s *FinanceSvc) queryOpsPlanSales(ctx context.Context, from, to time.Time) ([]OpsNamedAmt, error) {
	rows, err := s.d.DB().QueryContext(ctx, `
		SELECT COALESCE(o.plan_id::text, ''), COALESCE(p.name, '(无套餐)'),
		       COUNT(*)::int, COALESCE(SUM(o.amount), 0)
		FROM site_orders o
		LEFT JOIN plans p ON p.id = o.plan_id
		WHERE o.kind = 'plan'
		  AND o.status IN ('paid','partially_refunded','refunded')
		  AND o.paid_at >= $1 AND o.paid_at < $2
		GROUP BY 1, 2 ORDER BY 4 DESC
	`, from, to)
	if err != nil {
		return nil, fmt.Errorf("ops plan sales: %w", err)
	}
	defer rows.Close()
	var out []OpsNamedAmt
	for rows.Next() {
		var key, label string
		var n int
		var amt decimal.Decimal
		if err := rows.Scan(&key, &label, &n, &amt); err != nil {
			return nil, err
		}
		out = append(out, OpsNamedAmt{Key: key, Label: label, Count: n, Amount: amt.StringFixed(4)})
	}
	return out, rows.Err()
}

func (s *FinanceSvc) queryOpsUsageRollup(ctx context.Context, from, to time.Time) (scope, source []OpsNamedAmt, requests int64, active int, err error) {
	rows, err := s.d.DB().QueryContext(ctx, `
		SELECT
		  CASE
		    WHEN grouping(scope_key) = 0 THEN 'scope'
		    WHEN grouping(src) = 0 THEN 'source'
		    ELSE 'total'
		  END,
		  COALESCE(scope_key, src, ''),
		  COUNT(*)::bigint,
		  COUNT(DISTINCT user_id)::int,
		  COALESCE(SUM(cost_final), 0)
		FROM (
		  SELECT
		    CASE WHEN organization_id IS NULL THEN 'personal' ELSE 'organization' END AS scope_key,
		    billing_source::text AS src,
		    user_id,
		    cost_final
		  FROM usage_logs
		  WHERE created_at >= $1 AND created_at < $2
		) t
		GROUP BY GROUPING SETS ((scope_key), (src), ())
	`, from, to)
	if err != nil {
		return nil, nil, 0, 0, fmt.Errorf("ops usage rollup: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var kind, key string
		var n int64
		var users int
		var amt decimal.Decimal
		if err := rows.Scan(&kind, &key, &n, &users, &amt); err != nil {
			return nil, nil, 0, 0, err
		}
		switch kind {
		case "total":
			requests = n
			active = users
		case "scope":
			scope = append(scope, OpsNamedAmt{Key: key, Label: consumeScopeLabel(key), Count: int(n), Amount: amt.StringFixed(4)})
		case "source":
			source = append(source, OpsNamedAmt{Key: key, Label: consumeSourceLabel(key), Count: int(n), Amount: amt.StringFixed(4)})
		}
	}
	return scope, source, requests, active, rows.Err()
}

func formatOpsPeriod(day time.Time, grain string, loc *time.Location) string {
	d := day.In(loc)
	if grain == "month" {
		return d.Format("2006-01")
	}
	return d.Format("2006-01-02")
}

func fillOpsPeriods(from, to time.Time, grain string, loc *time.Location) []string {
	start := from.In(loc)
	end := to.In(loc)
	var out []string
	if grain == "month" {
		cur := time.Date(start.Year(), start.Month(), 1, 0, 0, 0, 0, loc)
		for cur.Before(end) {
			out = append(out, cur.Format("2006-01"))
			cur = cur.AddDate(0, 1, 0)
		}
		return out
	}
	cur := time.Date(start.Year(), start.Month(), start.Day(), 0, 0, 0, 0, loc)
	for cur.Before(end) {
		out = append(out, cur.Format("2006-01-02"))
		cur = cur.AddDate(0, 0, 1)
	}
	return out
}

func writeOpsSheet(f *excelize.File, name string, header []string, rows [][]any) {
	_, _ = f.NewSheet(name)
	_ = f.SetSheetRow(name, "A1", &header)
	for i, row := range rows {
		cell, _ := excelize.CoordinatesToCellName(1, i+2)
		_ = f.SetSheetRow(name, cell, &row)
	}
}

func WriteOpsXLSX(w io.Writer, report *OpsReport) error {
	f := excelize.NewFile()
	defer f.Close()
	_ = f.SetSheetName("Sheet1", "指标")
	kpi := [][]string{
		{"区间起", report.From.Format(time.RFC3339)},
		{"区间止", report.To.Format(time.RFC3339)},
		{"粒度", report.Grain},
		{"成交额", report.KPI.GrossRevenue},
		{"退款", report.KPI.Refunds},
		{"净收入", report.KPI.NetRevenue},
		{"付费用户", strconv.Itoa(report.KPI.PayingUsers)},
		{"新付费用户", strconv.Itoa(report.KPI.NewPayingUsers)},
		{"区间活跃", strconv.Itoa(report.KPI.PeriodActive)},
		{"ARPU(净收入/区间活跃)", report.KPI.ARPU},
		{"ARPPU(净收入/付费用户)", report.KPI.ARPPU},
		{"新注册", strconv.Itoa(report.KPI.NewUsers)},
		{"新注册付费率", report.KPI.PayConversion},
		{"DAU", strconv.Itoa(report.KPI.DAU)},
		{"WAU", strconv.Itoa(report.KPI.WAU)},
		{"MAU", strconv.Itoa(report.KPI.MAU)},
		{"黏性 DAU/MAU", report.KPI.Stickiness},
		{"请求数", strconv.FormatInt(report.KPI.Requests, 10)},
		{"复购率", report.KPI.RepeatPayRate},
		{"客单价", report.Orders.AOV},
		{"退款率", report.Orders.RefundRate},
		{"下单数", strconv.Itoa(report.Orders.Created)},
		{"下单后已支付", strconv.Itoa(report.Orders.CreatedPaid)},
		{"待支付", strconv.Itoa(report.Orders.CreatedPending)},
		{"下单支付率", report.Orders.PayRate},
		{"漏斗注册", strconv.Itoa(report.Funnel.SignedUp)},
		{"漏斗已调用", strconv.Itoa(report.Funnel.Called)},
		{"漏斗已付费", strconv.Itoa(report.Funnel.Paid)},
		{"在约套餐", strconv.Itoa(report.Plans.Active)},
		{"新开套餐", strconv.Itoa(report.Plans.Opened)},
		{"过期套餐", strconv.Itoa(report.Plans.Expired)},
		{"取消套餐", strconv.Itoa(report.Plans.Canceled)},
		{"上期净收入", report.Compare.NetRevenue},
		{"净收入环比", report.Compare.NetRevenueDelta},
		{"新注册环比", report.Compare.NewUsersDelta},
		{"付费人数环比", report.Compare.PayingDelta},
		{"请求环比", report.Compare.RequestsDelta},
	}
	for i, row := range kpi {
		cell, _ := excelize.CoordinatesToCellName(1, i+1)
		_ = f.SetSheetRow("指标", cell, &row)
	}
	seriesRows := make([][]any, 0, len(report.Series))
	for _, p := range report.Series {
		seriesRows = append(seriesRows, []any{p.Period, p.NewUsers, p.ActiveUsers, p.PayingUsers, p.Revenue, p.Refunds, p.Net, p.Requests})
	}
	writeOpsSheet(f, "时间序列", []string{"周期", "新注册", "活跃", "付费用户", "成交额", "退款", "净收入", "请求数"}, seriesRows)
	cohortRows := make([][]any, 0, len(report.Cohorts))
	for _, c := range report.Cohorts {
		cohortRows = append(cohortRows, []any{c.Month, c.NewUsers, c.Retained, c.Rate})
	}
	writeOpsSheet(f, "次月留存", []string{"注册月", "新用户", "次月仍有调用", "留存率"}, cohortRows)
	payerRows := make([][]any, 0, len(report.TopPayers))
	for _, p := range report.TopPayers {
		payerRows = append(payerRows, []any{p.UserID, p.Email, p.Orders, p.Amount})
	}
	writeOpsSheet(f, "头部付费", []string{"用户ID", "邮箱", "订单数", "金额"}, payerRows)
	kindRows := make([][]any, 0, len(report.KindSplit))
	for _, k := range report.KindSplit {
		kindRows = append(kindRows, []any{k.Kind, k.Orders, k.Amount})
	}
	writeOpsSheet(f, "订单类型", []string{"类型", "订单数", "金额"}, kindRows)
	mixRows := make([][]any, 0, len(report.PlanMix))
	for _, p := range report.PlanMix {
		mixRows = append(mixRows, []any{p.Label, p.Count})
	}
	writeOpsSheet(f, "在约套餐", []string{"套餐", "人数"}, mixRows)
	saleRows := make([][]any, 0, len(report.PlanSales))
	for _, p := range report.PlanSales {
		saleRows = append(saleRows, []any{p.Label, p.Count, p.Amount})
	}
	writeOpsSheet(f, "套餐成交", []string{"套餐", "订单数", "金额"}, saleRows)
	scopeRows := make([][]any, 0, len(report.ConsumeScope))
	for _, p := range report.ConsumeScope {
		scopeRows = append(scopeRows, []any{p.Label, p.Count, p.Amount})
	}
	writeOpsSheet(f, "消耗归属", []string{"范围", "请求数", "消耗"}, scopeRows)
	srcRows := make([][]any, 0, len(report.ConsumeSource))
	for _, p := range report.ConsumeSource {
		srcRows = append(srcRows, []any{p.Label, p.Count, p.Amount})
	}
	writeOpsSheet(f, "计费来源", []string{"来源", "请求数", "消耗"}, srcRows)
	_, err := f.WriteTo(w)
	return err
}
