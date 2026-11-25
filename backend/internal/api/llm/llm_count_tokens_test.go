package llm

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	llmpkg "github.com/jucode/jucode/internal/llm"
)

// TestCountTokensRoutesMounted verifies both surfaces expose the Anthropic
// count endpoint: the /v1 alias (MountGeneration) and the canonical
// /anthropic/v1 group (MountAnthropicMessages).
func TestCountTokensRoutesMounted(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := New(&llmpkg.Gateway{}, nil)
	r := gin.New()
	h.MountGeneration(r.Group("/v1"))
	h.MountAnthropicMessages(r.Group("/anthropic/v1"))

	want := map[string]bool{
		"/v1/messages/count_tokens":           false,
		"/anthropic/v1/messages/count_tokens": false,
	}
	for _, rt := range r.Routes() {
		if rt.Method == http.MethodPost {
			if _, ok := want[rt.Path]; ok {
				want[rt.Path] = true
			}
		}
	}
	for path, found := range want {
		if !found {
			t.Fatalf("POST %s not registered", path)
		}
	}
}

// TestCountTokensUnauthenticated: with no CtxUser in the context (i.e. the
// auth middleware did not resolve a credential) the handler must answer
// 401 in the Anthropic error envelope.
func TestCountTokensUnauthenticated(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := New(&llmpkg.Gateway{}, nil)
	r := gin.New()
	h.MountAnthropicMessages(r.Group("/anthropic/v1"))

	req := httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages/count_tokens",
		strings.NewReader(`{"model":"claude-3","messages":[{"role":"user","content":"hi"}]}`))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("not JSON: %s", w.Body.String())
	}
	if body["type"] != "error" {
		t.Fatalf("expected anthropic error envelope, got %s", w.Body.String())
	}
	errObj, _ := body["error"].(map[string]any)
	if errObj == nil || errObj["type"] != "authentication_error" {
		t.Fatalf("expected authentication_error, got %s", w.Body.String())
	}
}
