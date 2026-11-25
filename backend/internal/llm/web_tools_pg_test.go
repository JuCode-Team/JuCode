package llm

// Postgres-backed tests for the web tools, behind the same JUCODE_TEST_PG_DSN
// opt-in as the cross-group tests. They drive HandleWebSearch / HandleWebFetch
// through account choice, failover, the per-user limit and balance billing
// against httptest vendors.

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/jucode/jucode/internal/biz"
	"github.com/jucode/jucode/internal/data/ent"
	entaccount "github.com/jucode/jucode/internal/data/ent/upstreamaccount"
	entusagelog "github.com/jucode/jucode/internal/data/ent/usagelog"
	"github.com/jucode/jucode/internal/middleware"
	"github.com/jucode/jucode/internal/providerkit"
)

// newWebToolEnv is the cross-group harness with every web vendor account left
// by earlier runs disabled, so each test sees only its own accounts.
func newWebToolEnv(t *testing.T) *crossGroupEnv {
	e := newCrossGroupEnv(t, false, 0)
	if _, err := e.client.UpstreamAccount.Update().
		Where(entaccount.APIFormatIn("parallel", "brave", "jina", "firecrawl")).
		SetStatus(entaccount.StatusDisabled).
		Save(e.ctx); err != nil {
		t.Fatalf("disable old web accounts: %v", err)
	}
	return e
}

func (e *crossGroupEnv) addWebAccount(format, baseURL string, priority int) *ent.UpstreamAccount {
	e.t.Helper()
	plain, err := providerkit.NewAPIKeyCredential("vendor-key").Encode()
	if err != nil {
		e.t.Fatalf("encode credential: %v", err)
	}
	enc, err := e.cipher.Encrypt(plain)
	if err != nil {
		e.t.Fatalf("encrypt: %v", err)
	}
	acct, err := e.client.UpstreamAccount.Create().
		SetName(format + "-" + uuid.NewString()[:8]).
		SetBaseURL(baseURL).
		SetAPIFormat(format).
		SetCredentialEnc(enc).
		SetNativeProtocols([]string{biz.NativeProtocolWeb}).
		SetStatus(entaccount.StatusActive).
		SetSchedulable(true).
		SetPriority(priority).
		Save(e.ctx)
	if err != nil {
		e.t.Fatalf("create account: %v", err)
	}
	return acct
}

func (e *crossGroupEnv) doWeb(handler func(*gin.Context), requestID string, body map[string]any) *httptest.ResponseRecorder {
	e.t.Helper()
	gin.SetMode(gin.TestMode)
	raw, _ := json.Marshal(body)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	req := httptest.NewRequest(http.MethodPost, "/tools/v1/search", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Request-Id", requestID)
	c.Request = req
	c.Set(middleware.CtxUser, e.user)
	c.Set(middleware.CtxApiKey, e.apiKey)
	handler(c)
	return w
}

func vendorStub(t *testing.T, hits *int32, status int, header map[string]string, body string) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(hits, 1)
		for k, v := range header {
			w.Header().Set(k, v)
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(s.Close)
	return s
}

const parallelSearchOK = `{"search_id":"s","results":[{"url":"https://a.dev","title":"A","publish_date":null,"excerpts":["x"]}]}`

func TestWebSearchAutoFailsOverAndBillsOnce(t *testing.T) {
	e := newWebToolEnv(t)
	var braveHits, parallelHits int32
	brave := vendorStub(t, &braveHits, 422, nil,
		`{"type":"ErrorResponse","error":{"code":"SUBSCRIPTION_TOKEN_INVALID","detail":"invalid token","status":422}}`)
	parallel := vendorStub(t, &parallelHits, 200, nil, parallelSearchOK)
	e.addWebAccount("brave", brave.URL, 10)
	e.addWebAccount("parallel", parallel.URL, 20)

	before := e.balance()
	reqID := "web-" + uuid.NewString()
	w := e.doWeb(e.gw.HandleWebSearch, reqID, map[string]any{"query": "jucode"})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", w.Code, w.Body.String())
	}
	var out webSearchResponse
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil || out.Engine != "parallel" || len(out.Results) != 1 {
		t.Fatalf("body = %s", w.Body.String())
	}
	if braveHits != 1 || parallelHits != 1 {
		t.Fatalf("hits brave=%d parallel=%d, want brave first then parallel", braveHits, parallelHits)
	}
	if spent := before.Sub(e.balance()); !spent.Equal(decimal.RequireFromString("0.05")) {
		t.Fatalf("charged %s, want the search.auto price 0.05", spent)
	}
	row, err := e.client.UsageLog.Query().Where(entusagelog.RequestIDEQ(reqID)).Only(e.ctx)
	if err != nil {
		t.Fatalf("usage log: %v", err)
	}
	if row.Endpoint != entusagelog.EndpointWebSearch || row.Model != "search.auto" || row.UpstreamModel != "parallel" || row.GroupID != nil {
		t.Fatalf("usage row endpoint=%s model=%s upstream=%s group=%v", row.Endpoint, row.Model, row.UpstreamModel, row.GroupID)
	}
	if !row.CostFinal.Equal(decimal.RequireFromString("0.05")) || row.BillingSource != entusagelog.BillingSourceBalance {
		t.Fatalf("usage row cost=%s source=%s", row.CostFinal, row.BillingSource)
	}

	// The rejected key took its account out of rotation: the next call goes
	// straight to Parallel.
	w = e.doWeb(e.gw.HandleWebSearch, "web-"+uuid.NewString(), map[string]any{"query": "again"})
	if w.Code != http.StatusOK {
		t.Fatalf("second status = %d body = %s", w.Code, w.Body.String())
	}
	if braveHits != 1 || parallelHits != 2 {
		t.Fatalf("second call hits brave=%d parallel=%d, want the failed account skipped", braveHits, parallelHits)
	}
}

func TestWebFetchFailureIsNotBilled(t *testing.T) {
	e := newWebToolEnv(t)
	var hits int32
	parallel := vendorStub(t, &hits, 200, nil,
		`{"extract_id":"e","results":[],"errors":[{"url":"https://gone.dev","error_type":"fetch_error","http_status_code":404,"content":"gone"}]}`)
	e.addWebAccount("parallel", parallel.URL, 10)

	before := e.balance()
	w := e.doWeb(e.gw.HandleWebFetch, "web-"+uuid.NewString(), map[string]any{"url": "https://gone.dev", "engine": "parallel"})
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d body = %s", w.Code, w.Body.String())
	}
	if hits != 1 {
		t.Fatalf("a page that failed to fetch must not be retried on other accounts: hits=%d", hits)
	}
	if !e.balance().Equal(before) {
		t.Fatalf("balance moved from %s to %s on a failed fetch", before, e.balance())
	}
}

func TestWebToolDisabledEngineAndUserLimit(t *testing.T) {
	e := newWebToolEnv(t)
	var hits int32
	parallel := vendorStub(t, &hits, 200, nil, parallelSearchOK)
	e.addWebAccount("parallel", parallel.URL, 10)
	settings := biz.DefaultWebToolSettings()
	settings.Engines["search.brave"] = biz.WebEngineSetting{Enabled: false, Price: decimal.RequireFromString("0.05")}
	settings.UserRPM = 1
	if _, err := e.gw.webTools.Update(e.ctx, settings); err != nil {
		t.Fatalf("update settings: %v", err)
	}
	t.Cleanup(func() { _, _ = e.gw.webTools.Update(e.ctx, biz.DefaultWebToolSettings()) })

	w := e.doWeb(e.gw.HandleWebSearch, "web-"+uuid.NewString(), map[string]any{"query": "x", "engine": "brave"})
	if w.Code != http.StatusBadRequest || !bytes.Contains(w.Body.Bytes(), []byte("engine_disabled")) {
		t.Fatalf("disabled engine: status = %d body = %s", w.Code, w.Body.String())
	}
	if w = e.doWeb(e.gw.HandleWebSearch, "web-"+uuid.NewString(), map[string]any{"query": "x"}); w.Code != http.StatusOK {
		t.Fatalf("first call: status = %d body = %s", w.Code, w.Body.String())
	}
	w = e.doWeb(e.gw.HandleWebSearch, "web-"+uuid.NewString(), map[string]any{"query": "x"})
	if w.Code != http.StatusTooManyRequests || !bytes.Contains(w.Body.Bytes(), []byte("rpm_limit")) {
		t.Fatalf("over the per-user limit: status = %d body = %s", w.Code, w.Body.String())
	}
}

func TestWebSearchRecordsBraveMonthlyQuota(t *testing.T) {
	e := newWebToolEnv(t)
	var hits int32
	brave := vendorStub(t, &hits, 200, map[string]string{
		"X-RateLimit-Policy":    "1;w=1, 2000;w=2592000",
		"X-RateLimit-Limit":     "1, 2000",
		"X-RateLimit-Remaining": "0, 1500",
		"X-RateLimit-Reset":     "1, 3600",
	}, `{"web":{"results":[]}}`)
	acct := e.addWebAccount("brave", brave.URL, 10)

	w := e.doWeb(e.gw.HandleWebSearch, "web-"+uuid.NewString(), map[string]any{"query": "x", "engine": "brave"})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", w.Code, w.Body.String())
	}
	// The sample is written off the request path.
	deadline := time.Now().Add(3 * time.Second)
	for {
		got, err := e.client.UpstreamAccount.Get(e.ctx, acct.ID)
		if err != nil {
			t.Fatal(err)
		}
		if windows, ok := got.QuotaSnapshot["windows"].([]any); ok && len(windows) == 1 {
			window := windows[0].(map[string]any)
			if window["name"] != "30d" || window["used"].(float64) != 500 || window["limit"].(float64) != 2000 {
				t.Fatalf("window = %v", window)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("quota snapshot never recorded: %v", got.QuotaSnapshot)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestStaleQuotaLoopProbesFirecrawlCredits(t *testing.T) {
	e := newWebToolEnv(t)
	var hits int32
	firecrawl := vendorStub(t, &hits, 200, nil,
		`{"success":true,"data":{"remainingCredits":1398,"planCredits":1000,"billingPeriodEnd":"2026-10-29T02:43:51.802Z"}}`)
	acct := e.addWebAccount("firecrawl", firecrawl.URL, 10)
	if err := e.client.UpstreamAccount.UpdateOneID(acct.ID).SetDefinitionID("firecrawl").Exec(e.ctx); err != nil {
		t.Fatal(err)
	}

	if _, err := e.gw.accounts.RefreshStaleQuotas(e.ctx, 15*time.Minute, 4); err != nil {
		t.Fatalf("refresh stale quotas: %v", err)
	}
	got, err := e.client.UpstreamAccount.Get(e.ctx, acct.ID)
	if err != nil {
		t.Fatal(err)
	}
	if hits != 1 || got.QuotaCheckedAt == nil || got.QuotaSnapshot["source"] != "probe" {
		t.Fatalf("an API-key account with a quota operation was not probed: hits=%d snapshot=%v", hits, got.QuotaSnapshot)
	}
}
