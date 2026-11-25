package biz

import (
	"context"
	"time"

	"github.com/shopspring/decimal"

	"github.com/jucode/jucode/internal/data"
	entuser "github.com/jucode/jucode/internal/data/ent/user"
)

type Overview struct {
	Users struct {
		Total  int `json:"total"`
		Active int `json:"active"`
		Banned int `json:"banned"`
		// DAU / MAU are usage-based: distinct users who actually made an LLM
		// API call (any usage_logs row) within the window, not a status flag.
		//   DAU: since start-of-day in the business timezone (aligned with the Today.* block)
		//   MAU: rolling trailing 30 days
		DAU int `json:"dau"`
		MAU int `json:"mau"`
	} `json:"users"`
	Today struct {
		Requests     int    `json:"requests"`
		CostUSD      string `json:"cost_usd"`
		Tokens       int    `json:"tokens"`
		Errors       int    `json:"errors"`
		AvgLatencyMs int    `json:"avg_latency_ms"`
	} `json:"today"`
}

type DashboardSvc struct{ d *data.Data }

func NewDashboardSvc(d *data.Data) *DashboardSvc { return &DashboardSvc{d: d} }

func (s *DashboardSvc) GetOverview(ctx context.Context) (*Overview, error) {
	var o Overview

	o.Users.Total, _ = s.d.Ent.User.Query().Count(ctx)
	o.Users.Active, _ = s.d.Ent.User.Query().Where(entuser.StatusEQ(entuser.StatusActive)).Count(ctx)
	o.Users.Banned, _ = s.d.Ent.User.Query().Where(entuser.StatusEQ(entuser.StatusBanned)).Count(ctx)

	startOfDay := startOfDay(time.Now())

	// DAU / MAU by real LLM API traffic: distinct users appearing in
	// usage_logs within each window (errors included — an errored call still
	// means the user was active). Scans the trailing 30d slice once, using the
	// (created_at) / (user_id, created_at) indexes.
	monthAgo := time.Now().AddDate(0, 0, -30)
	var dau, mau int64
	if err := s.d.DB().QueryRowContext(ctx, `
		SELECT
		  COUNT(DISTINCT user_id) FILTER (WHERE created_at >= $1)::bigint,
		  COUNT(DISTINCT user_id)::bigint
		FROM usage_logs
		WHERE created_at >= $2
	`, startOfDay, monthAgo).Scan(&dau, &mau); err == nil {
		o.Users.DAU = int(dau)
		o.Users.MAU = int(mau)
	}

	var requests, tokens, errors int64
	var avgLatency float64
	// cost as ::text, not ::float8 — SUM over numeric(20,8) is exact in
	// Postgres and casting to double throws that away before Go ever sees it.
	// Matches how stats.go reads the same column. Latency stays float: it is
	// a measurement, not money.
	var costSum string
	if err := s.d.DB().QueryRowContext(ctx, `
		SELECT
		  COUNT(*)::bigint,
		  COALESCE(SUM(cost_final), 0)::text,
		  COALESCE(SUM(tokens_in + tokens_out), 0)::bigint,
		  COUNT(*) FILTER (WHERE status <> 'ok')::bigint,
		  COALESCE(AVG(latency_ms), 0)::float8
		FROM usage_logs
		WHERE created_at >= $1
	`, startOfDay).Scan(&requests, &costSum, &tokens, &errors, &avgLatency); err == nil {
		o.Today.Requests = int(requests)
		cost, err := decimal.NewFromString(costSum)
		if err != nil {
			cost = decimal.Zero
		}
		o.Today.CostUSD = cost.StringFixed(6)
		o.Today.Tokens = int(tokens)
		o.Today.Errors = int(errors)
		o.Today.AvgLatencyMs = int(avgLatency)
	}

	return &o, nil
}
