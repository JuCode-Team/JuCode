package cs

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/jucode/jucode/internal/middleware"
)

// Setting another user's password yields a login that outlives the token
// used to set it, so a personal access token must be refused here.
func TestAccessTokenCannotResetPasswords(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	g := r.Group("/v1/cs", func(c *gin.Context) {
		c.Set(middleware.CtxUserID, uuid.New())
		c.Set(middleware.CtxAccessTokenID, uuid.New())
	})
	(&Handler{}).Mount(g)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/cs/users/11111111-1111-1111-1111-111111111111/reset-password", strings.NewReader(`{"password":"x"}`))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "session_required") {
		t.Fatalf("status %d body %s, want 403 session_required", w.Code, w.Body.String())
	}
}
