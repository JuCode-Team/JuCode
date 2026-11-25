package user

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/jucode/jucode/internal/biz"
	"github.com/jucode/jucode/internal/data"
	"github.com/jucode/jucode/internal/middleware"
	jwtpkg "github.com/jucode/jucode/internal/pkg/jwt"
)

// End-to-end over the real middleware chain, because the bug this guards
// against lived exactly in the seam between them and neither side was wrong on
// its own.
//
// handleMyRate first shipped reading the caller from middleware.UserFrom
// (CtxUser). That key is populated by the API-key and OAuth paths, but /v1/me
// is guarded by middleware.JWT, which sets CtxUserID only. So the handler
// returned 401 to every browser session; the widget polls every 5s; the
// frontend's 401 interceptor concluded the session was dead and signed people
// out seconds after they opened the usage page.
//
// Unit-testing the handler with a hand-built context would have passed while
// production stayed broken — the context has to come from the real middleware.
func TestMyRateRouteAcceptsAJWTSession(t *testing.T) {
	gin.SetMode(gin.TestMode)

	const secret = "test-secret-that-is-long-enough-for-the-signer-32b"
	signer := jwtpkg.NewSigner(secret, time.Hour, 24*time.Hour)
	uid := uuid.New()
	token, _, _, err := signer.IssueAccess(uid, "user")
	if err != nil {
		t.Fatalf("issue access token: %v", err)
	}

	// stats with no Redis reports zeros rather than failing, and a nil ent
	// client makes the caps lookup a no-op — neither is what is under test.
	h := &Handler{stats: biz.NewStatsSvc(&data.Data{})}

	r := gin.New()
	grp := r.Group("/v1/me")
	grp.Use(middleware.JWT(signer, nil, func(context.Context, uuid.UUID) (bool, error) { return true, nil }))
	grp.GET("/stats/rate", h.handleMyRate)

	req := httptest.NewRequest(http.MethodGet, "/v1/me/stats/rate", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code == http.StatusUnauthorized {
		t.Fatalf("authenticated session got 401 (%s) — this is the logout bug", w.Body.String())
	}
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}

	var got biz.UsageRate
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("response is not a UsageRate: %v (%s)", err, w.Body.String())
	}
	if got.RPM != 0 || got.TPM != 0 {
		t.Fatalf("idle scope should report zeros, got %+v", got)
	}
}

// A request with no credentials must still be rejected — the fix must not have
// turned the endpoint into an unauthenticated one.
func TestMyRateRouteStillRejectsAnonymous(t *testing.T) {
	gin.SetMode(gin.TestMode)

	const secret = "test-secret-that-is-long-enough-for-the-signer-32b"
	signer := jwtpkg.NewSigner(secret, time.Hour, 24*time.Hour)
	h := &Handler{stats: biz.NewStatsSvc(&data.Data{})}

	r := gin.New()
	grp := r.Group("/v1/me")
	grp.Use(middleware.JWT(signer, nil, func(context.Context, uuid.UUID) (bool, error) { return true, nil }))
	grp.GET("/stats/rate", h.handleMyRate)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/me/stats/rate", nil))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous request got %d, want 401", w.Code)
	}
}
