package metrics

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHandlerServesRegisteredMetrics(t *testing.T) {
	// Record something so the families have at least one series.
	HTTPRequestsTotal.WithLabelValues("GET", "/test", "200").Inc()
	LLMAttemptsTotal.WithLabelValues("prov-1", "chat_completions", "ok").Inc()
	BillingTokensTotal.WithLabelValues("prov-1", "gpt-5", "in").Add(42)

	rec := httptest.NewRecorder()
	Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/internal/metrics", nil))
	body := rec.Body.String()

	for _, want := range []string{
		"http_requests_total",
		"llm_upstream_attempts_total",
		"billing_tokens_total",
		"go_goroutines", // default process collector
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("metrics output missing %q", want)
		}
	}
}
