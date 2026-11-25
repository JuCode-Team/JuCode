package biz

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/jucode/jucode/internal/data"
)

// Agent usage: the JuCode Desktop agent's own turns (agent_turns) and the
// few settings it syncs through the cloud (client_settings). Both are
// reached with an OAuth device token; see api/oauth/agent_usage.go.

const (
	AgentUsageMaxTurns    = 200
	ClientSettingsMaxKeys = 100
	ClientSettingMaxBytes = 4096

	agentTurnSessionMaxLen = 128
	agentTurnChannelMaxLen = 64
	agentTurnModelMaxLen   = 128
	agentTurnGatewayWrite  = 10 * time.Second
)

var (
	ErrAgentUsageInvalid     = errors.New("invalid agent usage")
	ErrClientSettingsInvalid = errors.New("invalid client settings")

	// AgentTurnHeaderRe is the shape of X-JuCode-Turn. Shorter ids are not
	// accepted from the gateway so a stray header cannot collide by accident.
	AgentTurnHeaderRe     = regexp.MustCompile(`^[A-Za-z0-9_-]{8,64}$`)
	agentTurnIDRe         = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	clientSettingKeyRe    = regexp.MustCompile(`^[a-z][a-zA-Z0-9_.]{0,63}$`)
	agentTurnEngines      = map[string]bool{"jucode": true, "claude": true, "codex": true, "acp": true}
	agentTurnChannelKinds = map[string]bool{"jucode": true, "third_party": true, "local": true}
)

type AgentUsageSvc struct {
	d *data.Data
}

func NewAgentUsageSvc(d *data.Data) *AgentUsageSvc { return &AgentUsageSvc{d: d} }

// AgentTurnUpload is one client-reported turn. Times are unix milliseconds;
// 0 / absent means unknown.
type AgentTurnUpload struct {
	TurnID            string `json:"turn_id"`
	SessionID         string `json:"session_id"`
	Engine            string `json:"engine"`
	ChannelKind       string `json:"channel_kind"`
	Channel           string `json:"channel"`
	Model             string `json:"model"`
	InputTokens       int64  `json:"input_tokens"`
	CachedInputTokens int64  `json:"cached_input_tokens"`
	CacheWriteTokens  int64  `json:"cache_write_tokens"`
	OutputTokens      int64  `json:"output_tokens"`
	ReasoningTokens   int64  `json:"reasoning_tokens"`
	Requests          int64  `json:"requests"`
	StartedAt         int64  `json:"started_at"`
	EndedAt           int64  `json:"ended_at"`
}

// ValidateAgentTurns checks a whole upload batch; the first problem is
// returned wrapped in ErrAgentUsageInvalid.
func ValidateAgentTurns(turns []AgentTurnUpload) error {
	if len(turns) > AgentUsageMaxTurns {
		return fmt.Errorf("%w: at most %d turns per upload", ErrAgentUsageInvalid, AgentUsageMaxTurns)
	}
	for i, t := range turns {
		if err := validateAgentTurn(t); err != nil {
			return fmt.Errorf("%w: turns[%d]: %s", ErrAgentUsageInvalid, i, err.Error())
		}
	}
	return nil
}

func validateAgentTurn(t AgentTurnUpload) error {
	switch {
	case !agentTurnIDRe.MatchString(t.TurnID):
		return errors.New("turn_id must match [A-Za-z0-9_-]{1,64}")
	case !agentTurnEngines[t.Engine]:
		return errors.New("unknown engine")
	case !agentTurnChannelKinds[t.ChannelKind]:
		return errors.New("unknown channel_kind")
	case len(t.SessionID) > agentTurnSessionMaxLen:
		return errors.New("session_id too long")
	case len(t.Channel) > agentTurnChannelMaxLen:
		return errors.New("channel too long")
	case len(t.Model) > agentTurnModelMaxLen:
		return errors.New("model too long")
	case t.InputTokens < 0 || t.CachedInputTokens < 0 || t.CacheWriteTokens < 0 ||
		t.OutputTokens < 0 || t.ReasoningTokens < 0 || t.Requests < 0:
		return errors.New("token and request counts must be non-negative")
	case t.Requests > 1<<31-1:
		return errors.New("requests out of range")
	case t.StartedAt < 0 || t.EndedAt < 0:
		return errors.New("times must be unix milliseconds")
	}
	return nil
}

func msTime(ms int64) *time.Time {
	if ms <= 0 {
		return nil
	}
	t := time.UnixMilli(ms).UTC()
	return &t
}

// Every column is supplied explicitly: the table's id / timestamps /
// gw_cost carry no database default (Go-side ent defaults only).
const agentTurnUploadSQL = `
INSERT INTO agent_turns (id, created_at, updated_at, user_id, turn_id, authorization_id,
	session_id, engine, channel_kind, channel, model,
	input_tokens, cached_input_tokens, cache_write_tokens, output_tokens, reasoning_tokens,
	requests, started_at, ended_at, reported, gw_cost, gw_requests, gw_group)
VALUES ($1, $2, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, true, 0, 0, '')
ON CONFLICT (user_id, turn_id) DO UPDATE SET
	updated_at = EXCLUDED.updated_at,
	authorization_id = COALESCE(agent_turns.authorization_id, EXCLUDED.authorization_id),
	session_id = EXCLUDED.session_id,
	engine = EXCLUDED.engine,
	channel_kind = EXCLUDED.channel_kind,
	channel = EXCLUDED.channel,
	model = EXCLUDED.model,
	input_tokens = EXCLUDED.input_tokens,
	cached_input_tokens = EXCLUDED.cached_input_tokens,
	cache_write_tokens = EXCLUDED.cache_write_tokens,
	output_tokens = EXCLUDED.output_tokens,
	reasoning_tokens = EXCLUDED.reasoning_tokens,
	requests = EXCLUDED.requests,
	started_at = EXCLUDED.started_at,
	ended_at = EXCLUDED.ended_at,
	reported = true`

// UploadTurns validates and upserts a batch of client-reported turns.
// Re-uploading a turn overwrites its client fields with the new values.
func (s *AgentUsageSvc) UploadTurns(ctx context.Context, userID uuid.UUID, authzID *uuid.UUID, turns []AgentTurnUpload) (int, error) {
	if err := ValidateAgentTurns(turns); err != nil {
		return 0, err
	}
	if len(turns) == 0 {
		return 0, nil
	}
	tx, err := s.d.DB().BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	now := time.Now()
	// One statement per turn: a multi-row ON CONFLICT DO UPDATE fails when
	// the batch repeats a turn_id; here the later entry simply wins.
	for _, t := range turns {
		if _, err := tx.ExecContext(ctx, agentTurnUploadSQL,
			uuid.New(), now, userID, t.TurnID, authzID,
			t.SessionID, t.Engine, t.ChannelKind, t.Channel, t.Model,
			t.InputTokens, t.CachedInputTokens, t.CacheWriteTokens, t.OutputTokens, t.ReasoningTokens,
			t.Requests, msTime(t.StartedAt), msTime(t.EndedAt),
		); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return len(turns), nil
}

const agentTurnGatewaySQL = `
INSERT INTO agent_turns (id, created_at, updated_at, user_id, turn_id, authorization_id,
	session_id, engine, channel_kind, channel, model,
	input_tokens, cached_input_tokens, cache_write_tokens, output_tokens, reasoning_tokens,
	requests, reported, gw_cost, gw_requests, gw_group)
VALUES ($1, $2, $2, $3, $4, $5, '', '', '', '', '', 0, 0, 0, 0, 0, 0, false, $6, 1, $7)
ON CONFLICT (user_id, turn_id) DO UPDATE SET
	updated_at = EXCLUDED.updated_at,
	authorization_id = COALESCE(agent_turns.authorization_id, EXCLUDED.authorization_id),
	gw_cost = agent_turns.gw_cost + EXCLUDED.gw_cost,
	gw_requests = agent_turns.gw_requests + 1,
	gw_group = CASE WHEN EXCLUDED.gw_group <> '' THEN EXCLUDED.gw_group ELSE agent_turns.gw_group END`

// RecordAgentTurnCharge adds one charged gateway request to its agent turn.
// Fire-and-forget: it returns immediately and only logs failures, so the
// billing path is never delayed or failed by it.
func (s *BillingSvc) RecordAgentTurnCharge(userID uuid.UUID, authzID uuid.UUID, turnID, group string, cost decimal.Decimal) {
	if s == nil || s.d == nil || turnID == "" {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), agentTurnGatewayWrite)
		defer cancel()
		if _, err := s.d.DB().ExecContext(ctx, agentTurnGatewaySQL,
			uuid.New(), time.Now(), userID, turnID, authzID, cost, group,
		); err != nil {
			slog.Warn("agent turn gateway upsert failed", "turn_id", turnID, "err", err)
		}
	}()
}

// AgentUsageAgg is the numeric part shared by every summary bucket.
type AgentUsageAgg struct {
	InputTokens       int64  `json:"input_tokens"`
	CachedInputTokens int64  `json:"cached_input_tokens"`
	CacheWriteTokens  int64  `json:"cache_write_tokens"`
	OutputTokens      int64  `json:"output_tokens"`
	ReasoningTokens   int64  `json:"reasoning_tokens"`
	Cost              string `json:"cost"`
	Turns             int64  `json:"turns"`
}

type AgentUsageDay struct {
	Day string `json:"day"`
	AgentUsageAgg
}

type AgentUsageByChannel struct {
	ChannelKind string `json:"channel_kind"`
	Channel     string `json:"channel"`
	Group       string `json:"group"`
	AgentUsageAgg
}

type AgentUsageByModel struct {
	Model string `json:"model"`
	AgentUsageAgg
}

type AgentUsageByEngine struct {
	Engine string `json:"engine"`
	AgentUsageAgg
}

type AgentUsageByDevice struct {
	AuthorizationID *uuid.UUID `json:"authorization_id"`
	Name            string     `json:"name"`
	AgentUsageAgg
}

type AgentUsageSummary struct {
	Days      []AgentUsageDay       `json:"days"`
	Totals    AgentUsageAgg         `json:"totals"`
	ByChannel []AgentUsageByChannel `json:"by_channel"`
	ByModel   []AgentUsageByModel   `json:"by_model"`
	ByEngine  []AgentUsageByEngine  `json:"by_engine"`
	ByDevice  []AgentUsageByDevice  `json:"by_device"`
}

// AgentUsageWindowStart is the instant the local day (days-1) days before
// now began, for a client whose Date.getTimezoneOffset() is tzOffsetMin.
func AgentUsageWindowStart(now time.Time, days, tzOffsetMin int) time.Time {
	shift := -time.Duration(tzOffsetMin) * time.Minute
	local := now.UTC().Add(shift)
	dayStart := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, time.UTC)
	return dayStart.AddDate(0, 0, -(days - 1)).Add(-shift)
}

const agentUsageAggCols = `COALESCE(SUM(t.input_tokens),0), COALESCE(SUM(t.cached_input_tokens),0),
	COALESCE(SUM(t.cache_write_tokens),0), COALESCE(SUM(t.output_tokens),0),
	COALESCE(SUM(t.reasoning_tokens),0), COALESCE(SUM(t.gw_cost),0), COUNT(*)`

// Summary aggregates reported turns started in the last `days` local days.
func (s *AgentUsageSvc) Summary(ctx context.Context, userID uuid.UUID, days, tzOffsetMin int) (*AgentUsageSummary, error) {
	since := AgentUsageWindowStart(time.Now(), days, tzOffsetMin)
	where := `FROM agent_turns t WHERE t.user_id = $1 AND t.reported AND COALESCE(t.started_at, t.created_at) >= $2`
	out := &AgentUsageSummary{
		Days: []AgentUsageDay{}, ByChannel: []AgentUsageByChannel{}, ByModel: []AgentUsageByModel{},
		ByEngine: []AgentUsageByEngine{}, ByDevice: []AgentUsageByDevice{},
	}

	dayExpr := `to_char((COALESCE(t.started_at, t.created_at) AT TIME ZONE 'UTC') - make_interval(mins => $3), 'YYYY-MM-DD')`
	if err := s.groupRows(ctx, `SELECT `+dayExpr+`, `+agentUsageAggCols+` `+where+` GROUP BY 1 ORDER BY 1`,
		[]any{userID, since, tzOffsetMin}, func(scan func(keys ...any) (AgentUsageAgg, error)) error {
			var r AgentUsageDay
			agg, err := scan(&r.Day)
			r.AgentUsageAgg = agg
			out.Days = append(out.Days, r)
			return err
		}); err != nil {
		return nil, err
	}
	if err := s.groupRows(ctx, `SELECT `+agentUsageAggCols+` `+where, []any{userID, since},
		func(scan func(keys ...any) (AgentUsageAgg, error)) error {
			agg, err := scan()
			out.Totals = agg
			return err
		}); err != nil {
		return nil, err
	}
	if err := s.groupRows(ctx, `SELECT t.channel_kind, t.channel, t.gw_group, `+agentUsageAggCols+` `+where+
		` GROUP BY 1, 2, 3 ORDER BY 9 DESC, 1, 2, 3`, []any{userID, since},
		func(scan func(keys ...any) (AgentUsageAgg, error)) error {
			var r AgentUsageByChannel
			agg, err := scan(&r.ChannelKind, &r.Channel, &r.Group)
			r.AgentUsageAgg = agg
			out.ByChannel = append(out.ByChannel, r)
			return err
		}); err != nil {
		return nil, err
	}
	if err := s.groupRows(ctx, `SELECT t.model, `+agentUsageAggCols+` `+where+` GROUP BY 1 ORDER BY 7 DESC, 1`,
		[]any{userID, since}, func(scan func(keys ...any) (AgentUsageAgg, error)) error {
			var r AgentUsageByModel
			agg, err := scan(&r.Model)
			r.AgentUsageAgg = agg
			out.ByModel = append(out.ByModel, r)
			return err
		}); err != nil {
		return nil, err
	}
	if err := s.groupRows(ctx, `SELECT t.engine, `+agentUsageAggCols+` `+where+` GROUP BY 1 ORDER BY 7 DESC, 1`,
		[]any{userID, since}, func(scan func(keys ...any) (AgentUsageAgg, error)) error {
			var r AgentUsageByEngine
			agg, err := scan(&r.Engine)
			r.AgentUsageAgg = agg
			out.ByEngine = append(out.ByEngine, r)
			return err
		}); err != nil {
		return nil, err
	}
	// Revoked devices keep their name: the join does not filter revoked_at.
	deviceSQL := `SELECT t.authorization_id, COALESCE(MAX(a.name), ''), ` + agentUsageAggCols + ` ` +
		strings.Replace(where, `FROM agent_turns t`, `FROM agent_turns t LEFT JOIN oauth_authorizations a ON a.id = t.authorization_id`, 1) +
		` GROUP BY 1 ORDER BY 8 DESC, 2`
	if err := s.groupRows(ctx, deviceSQL, []any{userID, since},
		func(scan func(keys ...any) (AgentUsageAgg, error)) error {
			var r AgentUsageByDevice
			var id uuid.NullUUID
			agg, err := scan(&id, &r.Name)
			if id.Valid {
				r.AuthorizationID = &id.UUID
			}
			r.AgentUsageAgg = agg
			out.ByDevice = append(out.ByDevice, r)
			return err
		}); err != nil {
		return nil, err
	}
	return out, nil
}

// groupRows runs q and hands each row to fn with a scanner that reads the
// row's leading key columns followed by the agentUsageAggCols block.
func (s *AgentUsageSvc) groupRows(ctx context.Context, q string, args []any, fn func(scan func(keys ...any) (AgentUsageAgg, error)) error) error {
	rows, err := s.d.DB().QueryContext(ctx, q, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		scan := func(keys ...any) (AgentUsageAgg, error) {
			var a AgentUsageAgg
			var cost decimal.Decimal
			dest := append(keys, &a.InputTokens, &a.CachedInputTokens, &a.CacheWriteTokens,
				&a.OutputTokens, &a.ReasoningTokens, &cost, &a.Turns)
			if err := rows.Scan(dest...); err != nil {
				return a, err
			}
			a.Cost = cost.String()
			return a, nil
		}
		if err := fn(scan); err != nil {
			return err
		}
	}
	return rows.Err()
}

type AgentTurnDevice struct {
	ID   uuid.UUID `json:"id"`
	Name string    `json:"name"`
}

// Charges are gateway-settled amounts, never client estimates. They can be
// read before the daemon uploads its token counts for the same turns.
type AgentTurnCharge struct {
	TurnID   string `json:"turn_id"`
	Cost     string `json:"cost"`
	Requests int64  `json:"requests"`
	Group    string `json:"group"`
}

func (s *AgentUsageSvc) Charges(ctx context.Context, userID uuid.UUID, turns []string) ([]AgentTurnCharge, error) {
	if len(turns) == 0 || len(turns) > 2000 {
		return nil, fmt.Errorf("%w: supply 1..2000 turn_ids", ErrAgentUsageInvalid)
	}
	for _, turn := range turns {
		if !agentTurnIDRe.MatchString(turn) {
			return nil, fmt.Errorf("%w: invalid turn_id", ErrAgentUsageInvalid)
		}
	}
	rows, err := s.d.DB().QueryContext(ctx, `SELECT turn_id, gw_cost, gw_requests, gw_group
FROM agent_turns WHERE user_id = $1 AND turn_id = ANY($2) AND gw_requests > 0`, userID, turns)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AgentTurnCharge{}
	for rows.Next() {
		var row AgentTurnCharge
		var cost decimal.Decimal
		if err := rows.Scan(&row.TurnID, &cost, &row.Requests, &row.Group); err != nil {
			return nil, err
		}
		row.Cost = cost.String()
		out = append(out, row)
	}
	return out, rows.Err()
}

type AgentTurnItem struct {
	TurnID            string           `json:"turn_id"`
	SessionID         string           `json:"session_id"`
	Engine            string           `json:"engine"`
	ChannelKind       string           `json:"channel_kind"`
	Channel           string           `json:"channel"`
	Group             string           `json:"group"`
	Model             string           `json:"model"`
	InputTokens       int64            `json:"input_tokens"`
	CachedInputTokens int64            `json:"cached_input_tokens"`
	CacheWriteTokens  int64            `json:"cache_write_tokens"`
	OutputTokens      int64            `json:"output_tokens"`
	ReasoningTokens   int64            `json:"reasoning_tokens"`
	Requests          int64            `json:"requests"`
	Cost              string           `json:"cost"`
	Device            *AgentTurnDevice `json:"device"`
	StartedAt         *int64           `json:"started_at"`
	EndedAt           *int64           `json:"ended_at"`
}

// Recent returns the newest reported turns.
func (s *AgentUsageSvc) Recent(ctx context.Context, userID uuid.UUID, limit int) ([]AgentTurnItem, error) {
	rows, err := s.d.DB().QueryContext(ctx, `
SELECT t.turn_id, t.session_id, t.engine, t.channel_kind, t.channel, t.gw_group, t.model,
	t.input_tokens, t.cached_input_tokens, t.cache_write_tokens, t.output_tokens, t.reasoning_tokens,
	t.requests, t.gw_cost, t.authorization_id, COALESCE(a.name, ''), t.started_at, t.ended_at
FROM agent_turns t LEFT JOIN oauth_authorizations a ON a.id = t.authorization_id
WHERE t.user_id = $1 AND t.reported
ORDER BY COALESCE(t.started_at, t.created_at) DESC, t.id
LIMIT $2`, userID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AgentTurnItem{}
	for rows.Next() {
		var it AgentTurnItem
		var cost decimal.Decimal
		var authz uuid.NullUUID
		var deviceName string
		var started, ended sql.NullTime
		if err := rows.Scan(&it.TurnID, &it.SessionID, &it.Engine, &it.ChannelKind, &it.Channel, &it.Group, &it.Model,
			&it.InputTokens, &it.CachedInputTokens, &it.CacheWriteTokens, &it.OutputTokens, &it.ReasoningTokens,
			&it.Requests, &cost, &authz, &deviceName, &started, &ended); err != nil {
			return nil, err
		}
		it.Cost = cost.String()
		if authz.Valid {
			it.Device = &AgentTurnDevice{ID: authz.UUID, Name: deviceName}
		}
		it.StartedAt = nullTimeMs(started)
		it.EndedAt = nullTimeMs(ended)
		out = append(out, it)
	}
	return out, rows.Err()
}

func nullTimeMs(t sql.NullTime) *int64 {
	if !t.Valid {
		return nil
	}
	ms := t.Time.UnixMilli()
	return &ms
}

type ClientSettingValue struct {
	Value     json.RawMessage `json:"value"`
	UpdatedAt int64           `json:"updated_at"`
}

// ValidateClientSettings checks a PUT payload: key shape and value size.
// A nil / JSON-null value means delete.
func ValidateClientSettings(in map[string]json.RawMessage) error {
	if len(in) > ClientSettingsMaxKeys {
		return fmt.Errorf("%w: at most %d keys", ErrClientSettingsInvalid, ClientSettingsMaxKeys)
	}
	for k, v := range in {
		if !clientSettingKeyRe.MatchString(k) {
			return fmt.Errorf("%w: key %q must match ^[a-z][a-zA-Z0-9_.]{0,63}$", ErrClientSettingsInvalid, k)
		}
		if len(v) > ClientSettingMaxBytes {
			return fmt.Errorf("%w: value of %q exceeds %d bytes", ErrClientSettingsInvalid, k, ClientSettingMaxBytes)
		}
	}
	return nil
}

func isJSONNull(v json.RawMessage) bool {
	return len(v) == 0 || strings.TrimSpace(string(v)) == "null"
}

func (s *AgentUsageSvc) GetSettings(ctx context.Context, userID uuid.UUID) (map[string]ClientSettingValue, error) {
	return getClientSettings(ctx, s.d.DB(), userID)
}

type sqlQuerier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

func getClientSettings(ctx context.Context, q sqlQuerier, userID uuid.UUID) (map[string]ClientSettingValue, error) {
	rows, err := q.QueryContext(ctx, `SELECT key, value, updated_at FROM client_settings WHERE user_id = $1`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]ClientSettingValue{}
	for rows.Next() {
		var k string
		var v []byte
		var at time.Time
		if err := rows.Scan(&k, &v, &at); err != nil {
			return nil, err
		}
		out[k] = ClientSettingValue{Value: json.RawMessage(v), UpdatedAt: at.UnixMilli()}
	}
	return out, rows.Err()
}

// PutSettings applies a partial update (null deletes) and returns the full
// set. Fails with ErrClientSettingsInvalid when the user would end up with
// more than ClientSettingsMaxKeys keys.
func (s *AgentUsageSvc) PutSettings(ctx context.Context, userID uuid.UUID, in map[string]json.RawMessage) (map[string]ClientSettingValue, error) {
	if err := ValidateClientSettings(in); err != nil {
		return nil, err
	}
	tx, err := s.d.DB().BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	now := time.Now()
	for k, v := range in {
		if isJSONNull(v) {
			if _, err := tx.ExecContext(ctx, `DELETE FROM client_settings WHERE user_id = $1 AND key = $2`, userID, k); err != nil {
				return nil, err
			}
			continue
		}
		if _, err := tx.ExecContext(ctx, `
INSERT INTO client_settings (id, created_at, updated_at, user_id, key, value)
VALUES ($1, $2, $2, $3, $4, $5::jsonb)
ON CONFLICT (user_id, key) DO UPDATE SET value = EXCLUDED.value, updated_at = EXCLUDED.updated_at`,
			uuid.New(), now, userID, k, string(v)); err != nil {
			return nil, err
		}
	}
	var n int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM client_settings WHERE user_id = $1`, userID).Scan(&n); err != nil {
		return nil, err
	}
	if n > ClientSettingsMaxKeys {
		return nil, fmt.Errorf("%w: at most %d keys per user", ErrClientSettingsInvalid, ClientSettingsMaxKeys)
	}
	out, err := getClientSettings(ctx, tx, userID)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return out, nil
}
