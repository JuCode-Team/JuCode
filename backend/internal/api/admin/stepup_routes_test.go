package admin

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

// Every admin route that mints or moves money, grants privilege, or takes
// over an account must sit behind the TOTP step-up guard. The guard is only
// as strong as its weakest equivalent path: PATCH /users/:id once accepted a
// `balance` field, which was the same capability as the guarded topup
// endpoint and made the gate decorative.
//
// The stub guard aborts before the real handler runs, so a zero-value
// Handler with no services wired is enough — this asserts routing, not
// handler behavior.
func TestSensitiveAdminRoutesRequireStepUp(t *testing.T) {
	gin.SetMode(gin.TestMode)

	guarded := []struct{ method, path string }{
		{http.MethodPost, "/v1/admin/users/x/topup"},
		{http.MethodPost, "/v1/admin/users/x/plans"},
		{http.MethodPost, "/v1/admin/redeem-codes"},
		{http.MethodPost, "/v1/admin/finance/refunds/x/approve"},
		{http.MethodPost, "/v1/admin/finance/refunds/x/reject"},
		{http.MethodPost, "/v1/admin/organizations/x/topup"},
		{http.MethodPost, "/v1/admin/finance/orders/x/pay"},
		{http.MethodPost, "/v1/admin/agents/x/settle"},
		{http.MethodPost, "/v1/admin/finance/provider-settlements/x/confirm"},
		{http.MethodPost, "/v1/admin/users/x/reset-password"},
		{http.MethodPut, "/v1/admin/users/x/roles"},
	}

	const guardHit = http.StatusTeapot
	r := gin.New()
	h := &Handler{}
	h.SetStepUpGuard(func(c *gin.Context) { c.AbortWithStatus(guardHit) })
	h.Mount(r.Group("/v1/admin"))

	for _, rt := range guarded {
		t.Run(rt.method+" "+rt.path, func(t *testing.T) {
			w := httptest.NewRecorder()
			r.ServeHTTP(w, httptest.NewRequest(rt.method, rt.path, nil))
			if w.Code == http.StatusNotFound {
				t.Fatalf("route not registered")
			}
			if w.Code != guardHit {
				t.Fatalf("reached the handler without the step-up guard (status %d)", w.Code)
			}
		})
	}
}

// Setting a balance through the generic user-update endpoint is refused
// outright rather than silently dropped: a silent drop would leave the
// operator believing the number they typed was applied. Money moves through
// the guarded topup endpoint, which is transactional and writes a balance_tx.
func TestUpdateUserRejectsBalance(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	h := &Handler{}
	r.PATCH("/users/:id", h.updateUser)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPatch, "/users/11111111-1111-1111-1111-111111111111",
		strings.NewReader(`{"balance":"999999"}`))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 — balance must not be settable here", w.Code)
	}
}
