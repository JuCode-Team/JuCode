package edge

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	entsql "entgo.io/ent/dialect/sql"

	"entgo.io/ent/dialect"
	"github.com/alicebob/miniredis/v2"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/shopspring/decimal"

	"github.com/jucode/jucode/internal/data/ent"
	entuser "github.com/jucode/jucode/internal/data/ent/user"
	cryptopkg "github.com/jucode/jucode/internal/pkg/crypto"
	"github.com/jucode/jucode/internal/region"
	"github.com/jucode/jucode/internal/region/control"
)

const (
	itestSyncKey = "000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f"
	itestToken   = "itest-service-token-0123456789abcdef0123456789"
	// Content that must never be stored anywhere: not in the outbox, not
	// in the control plane's tables.
	itestPrompt     = "PROMPT-CONTENT-MUST-NOT-BE-STORED"
	itestCompletion = "COMPLETION-CONTENT-MUST-NOT-BE-STORED"
)

type staticSnapshots struct {
	upstreams []region.ProviderSnapshot
	account   *region.AccountSnapshot
}

func (s staticSnapshots) UpstreamSnapshot(context.Context) ([]region.ProviderSnapshot, error) {
	return s.upstreams, nil
}

func (s staticSnapshots) AccountSnapshot(_ context.Context, hash string) (*region.AccountSnapshot, error) {
	if hash != s.account.APIKeyHash {
		return nil, region.ErrNotFound
	}
	return s.account, nil
}

// TestEdgeControlRoundTrip runs one request through the whole regional
// path over real HTTP: the edge fetches its upstream and account snapshots,
// gets a credit lease frozen on the Postgres balance, forwards the request,
// writes the usage event to its outbox, pushes it, and the control plane
// books it against the lease. Releasing the lease returns the rest.
func TestEdgeControlRoundTrip(t *testing.T) {
	dsn := os.Getenv("JUCODE_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("JUCODE_TEST_PG_DSN not set; skipping the edge/control round trip")
	}
	ctx := context.Background()
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	client := ent.NewClient(ent.Driver(entsql.OpenDB(dialect.Postgres, db)))
	t.Cleanup(func() { _ = client.Close() })
	if err := client.Schema.Create(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if _, err := db.ExecContext(ctx, control.SchemaSQL); err != nil {
		t.Fatalf("region schema: %v", err)
	}

	user, err := client.User.Create().SetEmail(uuid.NewString() + "@test.invalid").
		SetPasswordHash("x").SetStatus(entuser.StatusActive).
		SetBalance(decimal.RequireFromString("20")).Save(ctx)
	if err != nil {
		t.Fatal(err)
	}
	secret := "sk-itest-" + uuid.NewString()
	key, err := client.ApiKey.Create().SetUserID(user.ID).SetPrefix("sk-itest").
		SetHash(hashSecret(secret)).SetName("itest").Save(ctx)
	if err != nil {
		t.Fatal(err)
	}

	upstreamCalls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls++
		if r.Header.Get("Authorization") != "Bearer sk-upstream-itest" {
			t.Errorf("upstream auth = %q", r.Header.Get("Authorization"))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"x","choices":[{"message":{"role":"assistant","content":"` + itestCompletion + `"}}],"usage":{"prompt_tokens":10,"completion_tokens":10}}`))
	}))
	t.Cleanup(upstream.Close)

	cipher, err := cryptopkg.New(itestSyncKey)
	if err != nil {
		t.Fatal(err)
	}
	envelope, err := cipher.Encrypt("sk-upstream-itest")
	if err != nil {
		t.Fatal(err)
	}
	upstreamID := uuid.New()
	regionID := "itest-" + uuid.NewString()[:8]
	snapshots := staticSnapshots{
		upstreams: []region.ProviderSnapshot{{
			ID: upstreamID, Name: "itest", Kind: "openai", BaseURL: upstream.URL,
			APIKeyEnvelope: envelope, Enabled: true, Models: []string{"itest-model"},
		}},
		account: &region.AccountSnapshot{
			UserID: user.ID, APIKeyID: key.ID, APIKeyHash: key.Hash, APIKeyName: key.Name,
			BillingScope: region.BillingScopePersonal, Status: "active",
			AllowedModels: []string{"itest-model"},
			ModelPrices:   map[string]region.ModelPrice{"itest-model": {BillingMode: "per_request", PerRequestCNY: "0.5"}},
			BalanceCNY:    "20",
		},
	}
	ledger := control.NewSQLLedger(db, control.FixedLeasePolicy{
		MaxCNY: decimal.RequireFromString("5"), OverdraftCNY: decimal.Zero, TTL: time.Hour,
	})
	gin.SetMode(gin.TestMode)
	router := gin.New()
	control.NewHandler(control.TokenAuth{itestToken: regionID}, snapshots, ledger).Mount(router.Group(region.APIPrefix))
	controlSrv := httptest.NewServer(router)
	t.Cleanup(controlSrv.Close)

	mr := miniredis.RunT(t)
	outboxDir := filepath.Join(t.TempDir(), "outbox")
	cfg := Config{
		Listen: ":0", RegionID: regionID, RedisAddr: mr.Addr(),
		ControlURL: controlSrv.URL, ServiceToken: itestToken, SyncKey: itestSyncKey,
		AllowedProviderIDs: []string{upstreamID.String()}, OutboxDir: outboxDir,
		LeaseCNY: "10", ReserveCNY: "1", OverdraftCNY: "0",
		RequestTTL: time.Minute, AccountTTL: time.Minute,
		// Long enough that nothing but the explicit flush below pushes usage.
		SyncInterval: time.Hour, MaxBodyBytes: 1 << 20, RequestTimeout: 10 * time.Second,
	}
	svc, err := NewService(cfg, NewControlPlane(cfg))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(svc.Close)
	runCtx, cancel := context.WithCancel(ctx)
	t.Cleanup(cancel)
	svc.Start(runCtx)
	edgeSrv := httptest.NewServer(svc.Handler())
	t.Cleanup(edgeSrv.Close)

	deadline := time.Now().Add(5 * time.Second)
	for {
		resp, err := http.Get(edgeSrv.URL + "/readyz")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("edge never loaded its upstream snapshot")
		}
		time.Sleep(20 * time.Millisecond)
	}

	req, _ := http.NewRequest(http.MethodPost, edgeSrv.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"itest-model","messages":[{"role":"user","content":"`+itestPrompt+`"}]}`))
	req.Header.Set("Authorization", "Bearer "+secret)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || upstreamCalls != 1 {
		t.Fatalf("edge request = %d, upstream calls %d", resp.StatusCode, upstreamCalls)
	}

	// The lease froze 5 (the control plane's cap, not the 10 asked for).
	var leaseID uuid.UUID
	var granted, spent decimal.Decimal
	if err := db.QueryRowContext(ctx, `SELECT id, granted_cny, spent_cny FROM regional_credit_leases WHERE region_id=$1`, regionID).
		Scan(&leaseID, &granted, &spent); err != nil {
		t.Fatalf("lease row: %v", err)
	}
	if !granted.Equal(decimal.NewFromInt(5)) || !spent.IsZero() {
		t.Fatalf("lease granted %s spent %s before usage; want 5 and 0", granted, spent)
	}
	assertBalance(t, db, user.ID, "15")

	// The pending event is metadata only.
	files, _ := os.ReadDir(outboxDir)
	if len(files) != 1 {
		t.Fatalf("outbox holds %d files; want 1", len(files))
	}
	raw, _ := os.ReadFile(filepath.Join(outboxDir, files[0].Name()))
	if strings.Contains(string(raw), itestPrompt) || strings.Contains(string(raw), itestCompletion) {
		t.Fatalf("outbox event carries request or response content: %s", raw)
	}

	svc.flushOutbox(ctx)
	if files, _ := os.ReadDir(outboxDir); len(files) != 0 {
		t.Fatalf("outbox still holds %d files after a flush", len(files))
	}
	if err := db.QueryRowContext(ctx, `SELECT spent_cny FROM regional_credit_leases WHERE id=$1`, leaseID).Scan(&spent); err != nil {
		t.Fatal(err)
	}
	if !spent.Equal(decimal.RequireFromString("0.5")) {
		t.Fatalf("lease spent = %s; want 0.5", spent)
	}
	var logCost decimal.Decimal
	var logAccount uuid.UUID
	if err := db.QueryRowContext(ctx, `SELECT cost_final, provider_account_id FROM usage_logs WHERE user_id=$1`, user.ID).
		Scan(&logCost, &logAccount); err != nil {
		t.Fatalf("usage log: %v", err)
	}
	if !logCost.Equal(decimal.RequireFromString("0.5")) || logAccount != upstreamID {
		t.Fatalf("usage log cost %s account %s", logCost, logAccount)
	}
	for _, table := range []string{"usage_logs", "regional_usage_events"} {
		var leaked int
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM `+table+` t WHERE t.user_id=$1 AND (row_to_json(t)::text LIKE '%CONTENT-MUST-NOT%')`, user.ID).Scan(&leaked); err != nil {
			t.Fatal(err)
		}
		if leaked != 0 {
			t.Fatalf("%s stored request or response content", table)
		}
	}

	// Releasing the lease refunds what it did not spend: 20 - 0.5.
	if err := region.NewClient(controlSrv.URL, itestToken, nil).ReleaseLease(ctx, leaseID); err != nil {
		t.Fatalf("release: %v", err)
	}
	assertBalance(t, db, user.ID, "19.5")
}

func assertBalance(t *testing.T, db *sql.DB, userID uuid.UUID, want string) {
	t.Helper()
	var balance decimal.Decimal
	if err := db.QueryRow(`SELECT balance FROM users WHERE id=$1`, userID).Scan(&balance); err != nil {
		t.Fatal(err)
	}
	if !balance.Equal(decimal.RequireFromString(want)) {
		t.Fatalf("balance = %s; want %s", balance, want)
	}
}
