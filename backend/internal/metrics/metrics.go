// Package metrics registers the process's Prometheus collectors and
// exposes the helpers callers use to record them. All metrics are
// registered on the default registry so promhttp.Handler() serves them
// without extra wiring.
//
// Label discipline: account/model/endpoint/result only. Never user_id,
// api_key_id, request_id, or anything else unbounded — a label that can
// take thousands of values turns a metrics endpoint into a memory leak.
//
// The upstream dimension label is `account`: the routing entity is an
// ent.UpstreamAccount, and callers pass its id. Metric NAMES that still say
// "provider" (provider_cooldown_total) are kept deliberately as the legacy
// anchor a dashboard panel is keyed on — only the label vocabulary moved.
package metrics

import (
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// ---- HTTP layer ----

var HTTPRequestsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
	Name: "http_requests_total",
	Help: "HTTP requests by route pattern, method, and status code.",
}, []string{"method", "path", "status"})

var HTTPRequestDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
	Name:    "http_request_duration_seconds",
	Help:    "HTTP request latency by route pattern.",
	Buckets: []float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10, 30, 60},
}, []string{"method", "path"})

// ---- LLM upstream ----

// LLMAttemptsTotal counts every upstream attempt, labeled by the
// structured outcome class (error_class.go is the source of truth).
var LLMAttemptsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
	Name: "llm_upstream_attempts_total",
	Help: "Upstream LLM attempts by account, endpoint, and outcome class.",
}, []string{"account", "endpoint", "result"})

var LLMAttemptDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
	Name:    "llm_upstream_duration_seconds",
	Help:    "Upstream attempt latency (connect + first response) by account.",
	Buckets: []float64{.1, .25, .5, 1, 2.5, 5, 10, 30, 60, 120},
}, []string{"account", "endpoint"})

var LLMTTFB = promauto.NewHistogramVec(prometheus.HistogramOpts{
	Name:    "llm_upstream_ttfb_seconds",
	Help:    "Time to first downstream byte for streamed responses.",
	Buckets: []float64{.1, .25, .5, 1, 2, 5, 10, 30},
}, []string{"account", "endpoint"})

var LLMRetriesTotal = promauto.NewCounterVec(prometheus.CounterOpts{
	Name: "llm_retry_total",
	Help: "Failover retries by model group and trigger reason.",
}, []string{"group", "reason"})

var LLMStreamIdleTimeout = promauto.NewCounterVec(prometheus.CounterOpts{
	Name: "llm_stream_idle_timeout_total",
	Help: "Stream idle-timeout aborts by account.",
}, []string{"account"})

var LLMStreamFirstTextTimeout = promauto.NewCounterVec(prometheus.CounterOpts{
	Name: "llm_stream_first_text_timeout_total",
	Help: "First-text timeout aborts by account.",
}, []string{"account"})

// ---- Account health ----

// AccountCooldownTotal counts runtime cooldown marks by account and reason.
// The metric NAME keeps its historical `provider_cooldown_total` spelling so a
// dashboard panel keyed on it keeps rendering; the label carries the account id.
var AccountCooldownTotal = promauto.NewCounterVec(prometheus.CounterOpts{
	Name: "provider_cooldown_total",
	Help: "Runtime cooldown marks by account id and reason.",
}, []string{"account", "reason"})

// ---- Billing ----

var BillingReserveTotal = promauto.NewCounterVec(prometheus.CounterOpts{
	Name: "billing_reserve_total",
	Help: "Billing hold attempts by outcome.",
}, []string{"result"}) // ok | insufficient | limit | error

var BillingSettleTotal = promauto.NewCounterVec(prometheus.CounterOpts{
	Name: "billing_settle_total",
	Help: "Billing hold settlements by outcome.",
}, []string{"result"}) // ok | error

var BillingTokensTotal = promauto.NewCounterVec(prometheus.CounterOpts{
	Name: "billing_tokens_total",
	Help: "Billed token throughput by account, model, and direction.",
}, []string{"account", "model", "direction"}) // direction: in | out | cached

// Handler returns the Prometheus scrape handler for /internal/metrics.
func Handler() http.Handler { return promhttp.Handler() }
