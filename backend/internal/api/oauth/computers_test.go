package oauth

import (
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

func send(r *gin.Engine, method, path, body string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)
	return w
}

// Validation failures are answered before any database access.
func TestComputersHandlerRejectsInvalidInput(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	rdb := redis.NewClient(&redis.Options{Addr: miniredis.RunT(t).Addr()})
	user := uuid.New()
	g := r.Group("/v1/oauth", func(c *gin.Context) { c.Set(middleware.CtxUserID, user) })
	NewComputers(biz.NewComputerSvc(nil), rdb).Mount(g)
	for _, body := range []string{`{}`, `not json`, `{"name":"` + strings.Repeat("名", 65) + `"}`} {
		if w := send(r, "PATCH", "/v1/oauth/computers/abc", body); w.Code != http.StatusBadRequest {
			t.Errorf("%.40s: status %d, want 400 (%s)", body, w.Code, w.Body.String())
		}
	}
}
