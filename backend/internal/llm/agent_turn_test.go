package llm

import (
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/jucode/jucode/internal/data/ent"
	"github.com/jucode/jucode/internal/middleware"
)

func TestAgentTurnIDFrom(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := func(header string, device bool) *gin.Context {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest("POST", "/v1/responses", nil)
		if header != "" {
			c.Request.Header.Set(agentTurnHeader, header)
		}
		if device {
			c.Set(middleware.CtxAuthz, &ent.OAuthAuthorization{ID: uuid.New()})
		}
		return c
	}
	cases := []struct {
		name, header string
		device       bool
		want         string
	}{
		{"device with valid turn", "turn_0123-abc", true, "turn_0123-abc"},
		{"api key caller ignored", "turn_0123-abc", false, ""},
		{"too short", "abc", true, ""},
		{"bad charset", "turn 0123abc", true, ""},
		{"absent", "", true, ""},
	}
	for _, tc := range cases {
		if got := agentTurnIDFrom(ctx(tc.header, tc.device)); got != tc.want {
			t.Errorf("%s: got %q want %q", tc.name, got, tc.want)
		}
	}
}
