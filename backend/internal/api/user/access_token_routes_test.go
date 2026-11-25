package user

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/jucode/jucode/internal/middleware"
)

// A personal access token must not manage tokens or change the owner's
// password, email or two-factor: a leaked token could otherwise mint more
// tokens or lock the owner out.
func TestAccessTokenCannotManageAccountSecurity(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	g := r.Group("/v1/me", func(c *gin.Context) {
		c.Set(middleware.CtxUserID, uuid.New())
		c.Set(middleware.CtxAccessTokenID, uuid.New())
	})
	(&Handler{}).Mount(g)

	for _, rt := range []struct{ method, path string }{
		{http.MethodPost, "/v1/me/change-password"},
		{http.MethodPost, "/v1/me/change-email"},
		{http.MethodPost, "/v1/me/send-email-verification"},
		{http.MethodPost, "/v1/me/totp/setup"},
		{http.MethodPost, "/v1/me/totp/confirm"},
		{http.MethodPost, "/v1/me/totp/disable"},
		{http.MethodGet, "/v1/me/access-tokens"},
		{http.MethodPost, "/v1/me/access-tokens"},
		{http.MethodDelete, "/v1/me/access-tokens/x"},
		{http.MethodPut, "/v1/me/organizations/x/members/y"},
		{http.MethodDelete, "/v1/me/organizations/x/members/y"},
	} {
		t.Run(rt.method+" "+rt.path, func(t *testing.T) {
			w := httptest.NewRecorder()
			r.ServeHTTP(w, httptest.NewRequest(rt.method, rt.path, nil))
			if w.Code != http.StatusForbidden {
				t.Fatalf("status %d, want 403 session_required", w.Code)
			}
		})
	}
}
