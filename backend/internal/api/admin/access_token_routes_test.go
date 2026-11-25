package admin

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/jucode/jucode/internal/middleware"
)

// A personal access token must not be able to hand out access that outlives
// it: a new admin, a password-reset mailbox, or SMTP pointed elsewhere would
// all survive revoking the token. The request is marked as token-authenticated
// the way middleware.JWT does, and must be refused before any handler work.
func TestAccessTokenCannotGrantLastingAccess(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	g := r.Group("/v1/admin", func(c *gin.Context) {
		c.Set(middleware.CtxUserID, uuid.New())
		c.Set(middleware.CtxAccessTokenID, uuid.New())
	})
	(&Handler{}).Mount(g)

	cases := []struct{ method, path, body string }{
		{http.MethodPut, "/v1/admin/settings", `{"smtp_host":"evil.example"}`},
		{http.MethodPatch, "/v1/admin/users/11111111-1111-1111-1111-111111111111", `{"role":"admin"}`},
		{http.MethodPatch, "/v1/admin/users/11111111-1111-1111-1111-111111111111", `{"email":"x@evil.example"}`},
		{http.MethodPatch, "/v1/admin/organizations/11111111-1111-1111-1111-111111111111", `{"owner_user_id":"22222222-2222-2222-2222-222222222222"}`},
		{http.MethodPut, "/v1/admin/organizations/11111111-1111-1111-1111-111111111111/members/22222222-2222-2222-2222-222222222222", `{"role":"owner"}`},
		{http.MethodDelete, "/v1/admin/organizations/11111111-1111-1111-1111-111111111111/members/22222222-2222-2222-2222-222222222222", ``},
	}
	for _, rt := range cases {
		t.Run(rt.method+" "+rt.path+" "+rt.body, func(t *testing.T) {
			w := httptest.NewRecorder()
			req := httptest.NewRequest(rt.method, rt.path, strings.NewReader(rt.body))
			req.Header.Set("Content-Type", "application/json")
			r.ServeHTTP(w, req)
			if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "session_required") {
				t.Fatalf("status %d body %s, want 403 session_required", w.Code, w.Body.String())
			}
		})
	}
}
