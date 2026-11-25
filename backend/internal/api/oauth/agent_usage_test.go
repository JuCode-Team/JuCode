package oauth

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"

	"github.com/jucode/jucode/internal/biz"
	"github.com/jucode/jucode/internal/middleware"
)

// Validation failures are answered before any database access, so a
// service without a database is enough here.
func TestDeviceHandlerRejectsInvalidInput(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	rdb := redis.NewClient(&redis.Options{Addr: miniredis.RunT(t).Addr()})
	g := r.Group("/v1/oauth", func(c *gin.Context) { c.Set(middleware.CtxUserID, uuid.New()) })
	NewDevice(biz.NewAgentUsageSvc(nil), rdb).Mount(g)
	New(rdb, nil, nil, Clients{}).MountPublic(r.Group("/v1/oauth"))

	turn := `{"turn_id":"t1","engine":"codex","channel_kind":"%s","input_tokens":%s}`
	cases := []struct {
		method, path, body string
	}{
		{"POST", "/v1/oauth/agent-usage", `{"turns":[` + fmt.Sprintf(turn, "cloud", "1") + `]}`},
		{"POST", "/v1/oauth/agent-usage", `{"turns":[` + fmt.Sprintf(turn, "jucode", "-1") + `]}`},
		{"POST", "/v1/oauth/agent-usage", `{"turns":"x"}`},
		{"GET", "/v1/oauth/agent-usage/summary?days=0", ""},
		{"GET", "/v1/oauth/agent-usage/summary?days=401", ""},
		{"GET", "/v1/oauth/agent-usage/summary?tz_offset=900", ""},
		{"GET", "/v1/oauth/agent-usage/recent?limit=101", ""},
		{"POST", "/v1/oauth/agent-usage/charges", `{}`},
		{"POST", "/v1/oauth/agent-usage/charges", `{"turn_ids":["invalid id"]}`},
		{"POST", "/v1/oauth/agent-usage/charges", `{"turn_ids":"x"}`},
		{"PUT", "/v1/oauth/settings", `{}`},
		{"PUT", "/v1/oauth/settings", `{"settings":{"Bad":1}}`},
		{"PUT", "/v1/oauth/settings", `{"settings":{"big":"` + strings.Repeat("x", 4100) + `"}}`},
		{"POST", "/v1/oauth/revoke", `{}`},
		{"POST", "/v1/oauth/revoke", `{"refresh_token":"  "}`},
	}
	for _, tc := range cases {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
		req.Header.Set("Content-Type", "application/json")
		r.ServeHTTP(w, req)
		if w.Code != http.StatusBadRequest {
			t.Errorf("%s %s %s: status %d, want 400 (%s)", tc.method, tc.path, tc.body, w.Code, w.Body.String())
		}
	}
}

// Each endpoint family has its own per-user cap, and an oversized body is
// refused before it is buffered.
func TestDeviceHandlerLimits(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	rdb := redis.NewClient(&redis.Options{Addr: miniredis.RunT(t).Addr()})
	user := uuid.New()
	g := r.Group("/v1/oauth", func(c *gin.Context) { c.Set(middleware.CtxUserID, user) })
	NewDevice(biz.NewAgentUsageSvc(nil), rdb).Mount(g)
	New(rdb, nil, nil, Clients{}).MountPublic(r.Group("/v1/oauth"))

	send := func(method, path, body string) int {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		r.ServeHTTP(w, req)
		return w.Code
	}
	// Invalid bodies still count; the cap answers 429 once spent.
	for i := 0; i < agentUploadPerMinute; i++ {
		if code := send("POST", "/v1/oauth/agent-usage", `{"turns":"x"}`); code != http.StatusBadRequest {
			t.Fatalf("upload %d: status %d, want 400", i, code)
		}
	}
	if code := send("POST", "/v1/oauth/agent-usage", `{"turns":"x"}`); code != http.StatusTooManyRequests {
		t.Fatalf("upload past the cap: status %d, want 429", code)
	}
	// Reads count separately from uploads.
	if code := send("GET", "/v1/oauth/agent-usage/summary?days=0", ""); code != http.StatusBadRequest {
		t.Fatalf("summary after uploads: status %d, want 400", code)
	}
	big := `{"settings":{"k":"` + strings.Repeat("x", settingsUploadMaxBytes) + `"}}`
	if code := send("PUT", "/v1/oauth/settings", big); code != http.StatusBadRequest {
		t.Fatalf("oversized settings: status %d, want 400", code)
	}
	for i := 0; i < 20; i++ {
		send("POST", "/v1/oauth/revoke", `{}`)
	}
	if code := send("POST", "/v1/oauth/revoke", `{}`); code != http.StatusTooManyRequests {
		t.Fatalf("revoke past the cap: status %d, want 429", code)
	}
}
