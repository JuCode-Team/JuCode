package biz

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/jucode/jucode/internal/data"
	"github.com/jucode/jucode/internal/data/ent"
	entuser "github.com/jucode/jucode/internal/data/ent/user"
	"github.com/jucode/jucode/internal/pkg/jwt"
)

func validTurn(id string) AgentTurnUpload {
	return AgentTurnUpload{
		TurnID: id, SessionID: "s1", Engine: "codex", ChannelKind: "jucode", Channel: "jucode", Model: "gpt-5",
		InputTokens: 100, CachedInputTokens: 40, CacheWriteTokens: 5, OutputTokens: 20, ReasoningTokens: 8,
		Requests: 2, StartedAt: 1_790_000_000_000, EndedAt: 1_790_000_005_000,
	}
}

func TestValidateAgentTurns(t *testing.T) {
	mut := func(f func(*AgentTurnUpload)) []AgentTurnUpload {
		turn := validTurn("turn_0001")
		f(&turn)
		return []AgentTurnUpload{turn}
	}
	ok := [][]AgentTurnUpload{
		nil,
		mut(func(*AgentTurnUpload) {}),
		mut(func(u *AgentTurnUpload) { u.ChannelKind, u.Engine = "local", "acp" }),
		mut(func(u *AgentTurnUpload) { u.StartedAt, u.EndedAt = 0, 0 }),
		mut(func(u *AgentTurnUpload) { u.TurnID = strings.Repeat("a", 64) }),
	}
	for i, turns := range ok {
		if err := ValidateAgentTurns(turns); err != nil {
			t.Errorf("ok[%d]: %v", i, err)
		}
	}
	bad := map[string][]AgentTurnUpload{
		"empty turn_id":     mut(func(u *AgentTurnUpload) { u.TurnID = "" }),
		"turn_id too long":  mut(func(u *AgentTurnUpload) { u.TurnID = strings.Repeat("a", 65) }),
		"turn_id charset":   mut(func(u *AgentTurnUpload) { u.TurnID = "a b" }),
		"unknown engine":    mut(func(u *AgentTurnUpload) { u.Engine = "cursor" }),
		"unknown kind":      mut(func(u *AgentTurnUpload) { u.ChannelKind = "jucode_cloud" }),
		"negative input":    mut(func(u *AgentTurnUpload) { u.InputTokens = -1 }),
		"negative reason":   mut(func(u *AgentTurnUpload) { u.ReasoningTokens = -1 }),
		"negative requests": mut(func(u *AgentTurnUpload) { u.Requests = -1 }),
		"negative time":     mut(func(u *AgentTurnUpload) { u.StartedAt = -5 }),
		"model too long":    mut(func(u *AgentTurnUpload) { u.Model = strings.Repeat("m", 129) }),
		"session too long":  mut(func(u *AgentTurnUpload) { u.SessionID = strings.Repeat("s", 129) }),
		"too many turns":    make([]AgentTurnUpload, AgentUsageMaxTurns+1),
	}
	for name, turns := range bad {
		if err := ValidateAgentTurns(turns); !errors.Is(err, ErrAgentUsageInvalid) {
			t.Errorf("%s: err = %v, want ErrAgentUsageInvalid", name, err)
		}
	}
}

func TestAgentTurnHeaderRe(t *testing.T) {
	for _, v := range []string{"abcdefgh", "turn_01H-XYZ", strings.Repeat("Z", 64)} {
		if !AgentTurnHeaderRe.MatchString(v) {
			t.Errorf("%q should match", v)
		}
	}
	for _, v := range []string{"", "abcdefg", strings.Repeat("Z", 65), "abcdefgh ", "abc/defgh", "abcdefgh\n"} {
		if AgentTurnHeaderRe.MatchString(v) {
			t.Errorf("%q should not match", v)
		}
	}
}

func TestValidateClientSettings(t *testing.T) {
	ok := map[string]json.RawMessage{
		"theme":                       json.RawMessage(`"dark"`),
		"editor.fontSize":             json.RawMessage(`14`),
		"a":                           json.RawMessage(`null`),
		"x" + strings.Repeat("y", 63): json.RawMessage(`{}`),
		"big":                         json.RawMessage(`"` + strings.Repeat("v", ClientSettingMaxBytes-2) + `"`),
	}
	if err := ValidateClientSettings(ok); err != nil {
		t.Fatalf("valid payload rejected: %v", err)
	}
	bad := map[string]map[string]json.RawMessage{
		"uppercase start": {"Theme": json.RawMessage(`1`)},
		"digit start":     {"1theme": json.RawMessage(`1`)},
		"dash":            {"the-me": json.RawMessage(`1`)},
		"too long key":    {"x" + strings.Repeat("y", 64): json.RawMessage(`1`)},
		"empty key":       {"": json.RawMessage(`1`)},
		"value too big":   {"big": json.RawMessage(`"` + strings.Repeat("v", ClientSettingMaxBytes-1) + `"`)},
	}
	for name, in := range bad {
		if err := ValidateClientSettings(in); !errors.Is(err, ErrClientSettingsInvalid) {
			t.Errorf("%s: err = %v, want ErrClientSettingsInvalid", name, err)
		}
	}
	tooMany := map[string]json.RawMessage{}
	for i := 0; i <= ClientSettingsMaxKeys; i++ {
		tooMany[fmt.Sprintf("k%d", i)] = json.RawMessage(`1`)
	}
	if err := ValidateClientSettings(tooMany); !errors.Is(err, ErrClientSettingsInvalid) {
		t.Errorf("101 keys: err = %v", err)
	}
}

func TestAgentUsageWindowStart(t *testing.T) {
	// 2026-10-01 01:30 UTC is 09:30 on 10-01 in UTC+8 (offset -480).
	now := time.Date(2026, 10, 1, 1, 30, 0, 0, time.UTC)
	got := AgentUsageWindowStart(now, 1, -480)
	if want := time.Date(2026, 9, 30, 16, 0, 0, 0, time.UTC); !got.Equal(want) {
		t.Fatalf("UTC+8 1 day: got %v want %v", got, want)
	}
	// Same instant is 2026-09-30 18:30 in UTC-7 (offset 420); 3 days back.
	got = AgentUsageWindowStart(now, 3, 420)
	if want := time.Date(2026, 9, 28, 7, 0, 0, 0, time.UTC); !got.Equal(want) {
		t.Fatalf("UTC-7 3 days: got %v want %v", got, want)
	}
}

// DB-backed: run against a scratch database, e.g.
//
//	JUCODE_TEST_PG_DSN='postgres://postgres:test@127.0.0.1:5499/jucode_test?sslmode=disable' \
//	go test ./internal/biz -run 'AgentUsage|ClientSettings|OAuthRefresh'
func agentUsageTestData(t *testing.T) (*data.Data, *ent.User) {
	t.Helper()
	dsn := os.Getenv("JUCODE_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("JUCODE_TEST_PG_DSN not set")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	client := ent.NewClient(ent.Driver(entsql.OpenDB(dialect.Postgres, db)))
	ctx := context.Background()
	if err := client.Schema.Create(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	u, err := client.User.Create().SetEmail(uuid.NewString() + "@test.invalid").SetPasswordHash("x").SetStatus(entuser.StatusActive).Save(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	return data.NewWith(client, nil, db), u
}

func TestAgentUsageUploadGatewayAndSummaryPG(t *testing.T) {
	d, u := agentUsageTestData(t)
	ctx := context.Background()
	svc := NewAgentUsageSvc(d)
	billing := &BillingSvc{d: d}

	dev, err := d.Ent.OAuthAuthorization.Create().SetUserID(u.ID).SetClientID("jucode-desktop").SetName("Mac Studio").
		SetRefreshTokenHash(uuid.NewString()).SetRefreshExpiresAt(time.Now().Add(time.Hour)).
		SetRevokedAt(time.Now()). // revoked devices still show their name
		Save(ctx)
	if err != nil {
		t.Fatalf("create device: %v", err)
	}

	// Gateway charges arrive before the client upload.
	billing.RecordAgentTurnCharge(u.ID, dev.ID, "turn_aaaa1", "Claude · Example", decimal.RequireFromString("0.1"))
	billing.RecordAgentTurnCharge(u.ID, dev.ID, "turn_aaaa1", "", decimal.RequireFromString("0.023456"))
	waitFor(t, func() bool {
		var n int
		_ = d.DB().QueryRow(`SELECT gw_requests FROM agent_turns WHERE user_id=$1 AND turn_id='turn_aaaa1'`, u.ID).Scan(&n)
		return n == 2
	})

	// Not reported yet: invisible to summary / recent.
	if items, err := svc.Recent(ctx, u.ID, 10); err != nil || len(items) != 0 {
		t.Fatalf("recent before upload = %v, %v", items, err)
	}

	now := time.Now()
	t1 := validTurn("turn_aaaa1")
	t1.StartedAt, t1.EndedAt = now.Add(-time.Minute).UnixMilli(), now.UnixMilli()
	t2 := validTurn("turn_bbbb2")
	t2.Engine, t2.ChannelKind, t2.Channel, t2.Model = "claude", "third_party", "openrouter", "claude-x"
	t2.StartedAt = now.Add(-2 * time.Minute).UnixMilli()
	otherDev := uuid.New()
	for i := 0; i < 2; i++ { // re-upload is idempotent
		n, err := svc.UploadTurns(ctx, u.ID, &otherDev, []AgentTurnUpload{t1, t2})
		if err != nil || n != 2 {
			t.Fatalf("upload #%d: n=%d err=%v", i, n, err)
		}
	}
	// A batch repeating a turn id: later entry wins, no ON CONFLICT error.
	t2b := t2
	t2b.OutputTokens = 99
	if _, err := svc.UploadTurns(ctx, u.ID, &otherDev, []AgentTurnUpload{t2, t2b}); err != nil {
		t.Fatalf("duplicate in batch: %v", err)
	}
	if _, err := svc.UploadTurns(ctx, u.ID, nil, []AgentTurnUpload{{TurnID: "x", Engine: "nope", ChannelKind: "local"}}); !errors.Is(err, ErrAgentUsageInvalid) {
		t.Fatalf("invalid upload err = %v", err)
	}

	var rows int
	_ = d.DB().QueryRow(`SELECT COUNT(*) FROM agent_turns WHERE user_id=$1`, u.ID).Scan(&rows)
	if rows != 2 {
		t.Fatalf("rows = %d, want 2", rows)
	}

	items, err := svc.Recent(ctx, u.ID, 10)
	if err != nil || len(items) != 2 {
		t.Fatalf("recent = %+v, %v", items, err)
	}
	first := items[0]
	if first.TurnID != "turn_aaaa1" || first.Cost != "0.123456" || first.Group != "Claude · Example" ||
		first.InputTokens != 100 || first.Requests != 2 || first.Device == nil || first.Device.ID != dev.ID ||
		first.Device.Name != "Mac Studio" || first.StartedAt == nil || *first.StartedAt != t1.StartedAt {
		t.Fatalf("turn 1 = %+v (device %+v)", first, first.Device)
	}
	if second := items[1]; second.OutputTokens != 99 || second.Cost != "0" || second.Device == nil || second.Device.ID != otherDev {
		t.Fatalf("turn 2 = %+v", second)
	}

	sum, err := svc.Summary(ctx, u.ID, 30, -480)
	if err != nil {
		t.Fatalf("summary: %v", err)
	}
	if sum.Totals.Turns != 2 || sum.Totals.InputTokens != 200 || sum.Totals.OutputTokens != 119 || sum.Totals.Cost != "0.123456" {
		t.Fatalf("totals = %+v", sum.Totals)
	}
	if len(sum.Days) == 0 || len(sum.ByChannel) != 2 || len(sum.ByModel) != 2 || len(sum.ByEngine) != 2 || len(sum.ByDevice) != 2 {
		t.Fatalf("summary shape = %+v", sum)
	}
	if c := sum.ByChannel[0]; c.ChannelKind != "jucode" || c.Group != "Claude · Example" || c.Cost != "0.123456" {
		t.Fatalf("by_channel[0] = %+v", c)
	}
	names := map[string]bool{}
	for _, dv := range sum.ByDevice {
		names[dv.Name] = true
	}
	if !names["Mac Studio"] {
		t.Fatalf("by_device = %+v", sum.ByDevice)
	}
	if empty, err := svc.Summary(ctx, uuid.New(), 30, 0); err != nil || empty.Totals.Cost != "0" || empty.Days == nil {
		t.Fatalf("empty summary = %+v, %v", empty, err)
	}
}

func TestAgentUsageChargesPG(t *testing.T) {
	d, u := agentUsageTestData(t)
	ctx := context.Background()
	svc := NewAgentUsageSvc(d)
	dev, err := d.Ent.OAuthAuthorization.Create().SetUserID(u.ID).SetClientID("jucode-desktop").
		SetRefreshTokenHash(uuid.NewString()).SetRefreshExpiresAt(time.Now().Add(time.Hour)).Save(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// The real price calculation applies the group multiplier once.
	input, output := decimal.RequireFromString("0.01"), decimal.RequireFromString("0.02")
	model := &ent.Model{BillingMode: "per_token", InputUnitPrice: &input, OutputUnitPrice: &output}
	billing := &BillingSvc{d: d}
	_, charged, err := billing.CalcCost(model, &ent.ModelGroup{RateMultiplier: 2.5}, TokenUsage{TokensIn: 1000, TokensOut: 500})
	if err != nil {
		t.Fatal(err)
	}
	billing.RecordAgentTurnCharge(u.ID, dev.ID, "scheduled_turn", "paid", charged)
	waitFor(t, func() bool {
		rows, err := svc.Charges(ctx, u.ID, []string{"scheduled_turn"})
		return err == nil && len(rows) == 1
	})
	rows, err := svc.Charges(ctx, u.ID, []string{"scheduled_turn", "scheduled_turn", "missing_turn"})
	if err != nil || len(rows) != 1 || rows[0].Cost != "0.05" || rows[0].Requests != 1 || rows[0].Group != "paid" {
		t.Fatalf("charges = %+v, %v", rows, err)
	}
	// IDs from another account reveal neither its cost nor its existence.
	rows, err = svc.Charges(ctx, uuid.New(), []string{"scheduled_turn"})
	if err != nil || len(rows) != 0 {
		t.Fatalf("other account = %+v, %v", rows, err)
	}
	for _, ids := range [][]string{nil, {"bad id"}, make([]string, 2001)} {
		if _, err := svc.Charges(ctx, u.ID, ids); !errors.Is(err, ErrAgentUsageInvalid) {
			t.Fatalf("invalid request: %v", err)
		}
	}
}

func TestClientSettingsPG(t *testing.T) {
	d, u := agentUsageTestData(t)
	ctx := context.Background()
	svc := NewAgentUsageSvc(d)

	got, err := svc.PutSettings(ctx, u.ID, map[string]json.RawMessage{
		"theme": json.RawMessage(`"dark"`), "editor.fontSize": json.RawMessage(`14`),
	})
	if err != nil || len(got) != 2 || string(got["theme"].Value) != `"dark"` || got["theme"].UpdatedAt == 0 {
		t.Fatalf("put = %+v, %v", got, err)
	}
	got, err = svc.PutSettings(ctx, u.ID, map[string]json.RawMessage{"theme": json.RawMessage(`null`)})
	if err != nil || len(got) != 1 || string(got["editor.fontSize"].Value) != `14` {
		t.Fatalf("delete = %+v, %v", got, err)
	}

	batch := map[string]json.RawMessage{}
	for i := 0; i < ClientSettingsMaxKeys; i++ {
		batch[fmt.Sprintf("k%d", i)] = json.RawMessage(`1`)
	}
	// 100 new keys + the existing one = 101 > limit; nothing is applied.
	if _, err := svc.PutSettings(ctx, u.ID, batch); !errors.Is(err, ErrClientSettingsInvalid) {
		t.Fatalf("over limit err = %v", err)
	}
	if all, _ := svc.GetSettings(ctx, u.ID); len(all) != 1 {
		t.Fatalf("over-limit put leaked %d keys", len(all))
	}
}

func TestOAuthRefreshRotatesOncePG(t *testing.T) {
	d, u := agentUsageTestData(t)
	ctx := context.Background()
	svc := NewOAuthSvc(d, jwt.NewSigner("test-secret-test-secret-test-secret!", time.Hour, 24*time.Hour))
	_, bundle, err := svc.CreateDevice(ctx, u.ID, "jucode-desktop", "dev", "", "")
	if err != nil {
		t.Fatalf("create device: %v", err)
	}

	const n = 8
	var wg sync.WaitGroup
	var mu sync.Mutex
	var wins []*TokenBundle
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			b, err := svc.Refresh(ctx, bundle.RefreshToken, "", "")
			if err == nil {
				mu.Lock()
				wins = append(wins, b)
				mu.Unlock()
			} else if !errors.Is(err, ErrOAuthRefreshInvalid) {
				t.Errorf("refresh: %v", err)
			}
		}()
	}
	wg.Wait()
	if len(wins) != 1 {
		t.Fatalf("%d concurrent refreshes succeeded, want exactly 1", len(wins))
	}
	// The winner's token is the live one.
	if _, err := svc.Refresh(ctx, wins[0].RefreshToken, "", ""); err != nil {
		t.Fatalf("winner token rejected: %v", err)
	}

	// RFC 7009 revoke: unknown token is a no-op, a known one revokes the device.
	if err := svc.RevokeByRefreshToken(ctx, "juc-rt-unknown"); err != nil {
		t.Fatalf("revoke unknown: %v", err)
	}
	b, err := svc.Refresh(ctx, mustRefresh(t, svc, u.ID), "", "")
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if err := svc.RevokeByRefreshToken(ctx, b.RefreshToken); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if _, err := svc.Refresh(ctx, b.RefreshToken, "", ""); !errors.Is(err, ErrOAuthRefreshInvalid) {
		t.Fatalf("refresh after revoke err = %v", err)
	}
}

func mustRefresh(t *testing.T, svc *OAuthSvc, userID uuid.UUID) string {
	t.Helper()
	_, b, err := svc.CreateDevice(context.Background(), userID, "jucode-cli", "dev2", "", "")
	if err != nil {
		t.Fatalf("create device: %v", err)
	}
	return b.RefreshToken
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met within 5s")
		}
		time.Sleep(20 * time.Millisecond)
	}
}
