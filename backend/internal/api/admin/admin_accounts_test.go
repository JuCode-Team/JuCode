package admin

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/jucode/jucode/internal/biz"
	"github.com/jucode/jucode/internal/data/ent"
	entaccount "github.com/jucode/jucode/internal/data/ent/upstreamaccount"
)

func newTestCtx(method, target, body string) (*gin.Context, *httptest.ResponseRecorder) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(method, target, strings.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	return c, w
}

func TestAccountModelInputFromReq(t *testing.T) {
	t.Parallel()
	str := func(s string) *string { return &s }
	cases := []struct {
		name      string
		req       accountModelRequest
		wantOK    bool
		wantPrice bool
		wantInput bool
	}{
		{name: "none clears costs", req: accountModelRequest{ModelName: "m", CostMode: "none", PricePerRequestCost: str("1.5")}, wantOK: true},
		{name: "per_request needs price", req: accountModelRequest{ModelName: "m", CostMode: "per_request", PricePerRequestCost: str("1.5")}, wantOK: true, wantPrice: true},
		{name: "per_request without price rejected", req: accountModelRequest{ModelName: "m", CostMode: "per_request"}},
		{name: "per_usage needs input+output", req: accountModelRequest{ModelName: "m", CostMode: "per_usage", InputUnitCost: str("0.1"), OutputUnitCost: str("0.2")}, wantOK: true, wantInput: true},
		{name: "per_usage without output rejected", req: accountModelRequest{ModelName: "m", CostMode: "per_usage", InputUnitCost: str("0.1")}},
		{name: "unknown cost mode rejected", req: accountModelRequest{ModelName: "m", CostMode: "flat"}},
		{name: "bad decimal rejected", req: accountModelRequest{ModelName: "m", CostMode: "per_request", PricePerRequestCost: str("abc")}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c, w := newTestCtx(http.MethodPost, "/v1/admin/accounts/x/models", "{}")
			in, ok := accountModelInputFromReq(c, tc.req)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v (status %d)", ok, tc.wantOK, w.Code)
			}
			if !tc.wantOK {
				if w.Code != http.StatusBadRequest {
					t.Fatalf("want 400 on rejection, got %d", w.Code)
				}
				return
			}
			if (in.PricePerRequestCost != nil) != tc.wantPrice {
				t.Fatalf("price set = %v, want %v", in.PricePerRequestCost != nil, tc.wantPrice)
			}
			if (in.InputUnitCost != nil) != tc.wantInput {
				t.Fatalf("input set = %v, want %v", in.InputUnitCost != nil, tc.wantInput)
			}
		})
	}
}

func TestToProviderViewFromAccount(t *testing.T) {
	t.Parallel()
	legacy := uuid.New()
	acct := &ent.UpstreamAccount{
		ID:               uuid.New(),
		Name:             "claude",
		BaseURL:          "https://api.anthropic.com",
		APIFormat:        "anthropic-messages",
		NativeProtocols:  []string{biz.NativeProtocolMessages},
		Status:           entaccount.StatusActive,
		Schedulable:      true,
		Health:           entaccount.HealthHealthy,
		Weight:           3,
		CredentialEnc:    "enc-secret",
		LegacyProviderID: &legacy,
		CreatedAt:        time.Now(),
	}
	v := toProviderViewFromAccount(acct, 2)
	if v.ID != legacy.String() {
		t.Fatalf("id = %q, want legacy id %q", v.ID, legacy.String())
	}
	if v.LegacyProviderID != legacy.String() {
		t.Fatalf("legacy_provider_id = %q, want %q", v.LegacyProviderID, legacy.String())
	}
	if v.AccountID != acct.ID.String() {
		t.Fatalf("account_id = %q, want %q", v.AccountID, acct.ID.String())
	}
	if v.Kind != "anthropic" {
		t.Fatalf("kind = %q, want anthropic", v.Kind)
	}
	if len(v.Protocols) != 1 || v.Protocols[0] != "anthropic" {
		t.Fatalf("protocols = %v, want [anthropic]", v.Protocols)
	}
	if !v.APIKeySet {
		t.Fatal("api_key_set should be true when a credential is present")
	}
	if !v.Enabled {
		t.Fatal("enabled should be true for an active, schedulable account")
	}
	if v.SupportedModelCount != 2 {
		t.Fatalf("supported_model_count = %d, want 2", v.SupportedModelCount)
	}

	// A chat-only account maps onto the legacy openai dialect and, without a
	// legacy link, keeps its own id as the provider id.
	acct2 := &ent.UpstreamAccount{
		ID:              uuid.New(),
		NativeProtocols: []string{biz.NativeProtocolChat},
		Status:          entaccount.StatusActive,
		Schedulable:     true,
	}
	v2 := toProviderViewFromAccount(acct2, 0)
	if v2.Kind != "openai_compatible" {
		t.Fatalf("kind = %q, want openai_compatible", v2.Kind)
	}
	if v2.ID != acct2.ID.String() {
		t.Fatalf("id = %q, want own id %q", v2.ID, acct2.ID.String())
	}
	if v2.LegacyProviderID != "" {
		t.Fatalf("legacy_provider_id = %q, want empty", v2.LegacyProviderID)
	}
	if len(v2.Protocols) != 1 || v2.Protocols[0] != "openai" {
		t.Fatalf("protocols = %v, want [openai]", v2.Protocols)
	}
}

func TestWriteAccountErrorMapping(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"not found", biz.ErrAccountNotFound, http.StatusNotFound},
		{"bad input", biz.ErrAccountBadInput, http.StatusBadRequest},
		{"no credential", biz.ErrAccountNoCredential, http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c, w := newTestCtx(http.MethodGet, "/v1/admin/accounts/x", "")
			writeAccountError(c, tc.err)
			if w.Code != tc.want {
				t.Fatalf("status = %d, want %d", w.Code, tc.want)
			}
		})
	}
}

func TestParsePoolModeOrDefault(t *testing.T) {
	t.Parallel()
	if mode, ok := parsePoolModeOrDefault(""); !ok || string(mode) != "balanced" {
		t.Fatalf("empty mode = %q (ok=%v), want balanced", mode, ok)
	}
	if mode, ok := parsePoolModeOrDefault("conservative"); !ok || string(mode) != "conservative" {
		t.Fatalf("mode = %q (ok=%v), want conservative", mode, ok)
	}
	if _, ok := parsePoolModeOrDefault("bogus"); ok {
		t.Fatal("bogus mode should be rejected")
	}
}

// TestAccountRouteRegistration asserts the new account surfaces register
// without a gin route-tree conflict (a conflict panics at registration) and
// that the project add on a fresh engine. Services are zero values because
// registration never dereferences them.
func TestAccountRouteRegistration(t *testing.T) {
	t.Parallel()
	gin.SetMode(gin.TestMode)
	h := &Handler{accounts: &biz.AccountPoolSvc{}, users: &ent.Client{}}
	r := gin.New()
	g := r.Group("/v1/admin")
	h.mountAccounts(g)
	h.mountAccountPools(g)
	h.mountGroupAccounts(g)
	g.GET("/providers", h.listLegacyProviders)
	g.GET("/providers/:id", h.getLegacyProvider)
	h.mountProviderWriteGone(g)
	// The pre-existing model-group sub-resources must still coexist with the
	// new /model-groups/:id/accounts routes.
	g.GET("/model-groups/:id/models/:mid", func(c *gin.Context) {})
	g.POST("/model-groups/:id/providers/:pid", func(c *gin.Context) {})

	// A removed provider write endpoint answers 410 Gone pointing at accounts.
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/admin/providers", nil)
	r.ServeHTTP(w, req)
	if w.Code != http.StatusGone {
		t.Fatalf("POST /providers = %d, want 410", w.Code)
	}
	if !strings.Contains(w.Body.String(), "/v1/admin/accounts") {
		t.Fatalf("410 body should point at accounts, got %s", w.Body.String())
	}
}
