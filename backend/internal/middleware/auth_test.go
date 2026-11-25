package middleware

import (
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestAuthOpenAIErrorIncludesRequestID(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Set(CtxRequestID, "req_123")

	got := openAIError(c, "invalid_api_key", "invalid api key")

	if got["request_id"] != "req_123" {
		t.Fatalf("top-level request_id = %v, want req_123", got["request_id"])
	}
	err, ok := got["error"].(gin.H)
	if !ok {
		t.Fatalf("error field has type %T", got["error"])
	}
	if err["request_id"] != "req_123" {
		t.Fatalf("nested request_id = %v, want req_123", err["request_id"])
	}
	if w.Header().Get("X-Request-Id") != "req_123" {
		t.Fatalf("X-Request-Id = %q, want req_123", w.Header().Get("X-Request-Id"))
	}
}
