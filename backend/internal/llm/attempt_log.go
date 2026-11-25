package llm

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/jucode/jucode/internal/biz"
	"github.com/jucode/jucode/internal/metrics"
)

const usageAttemptPrefix = "attempt:"

type usageAttemptLog struct {
	Attempt          int            `json:"attempt"`
	ProviderID       string         `json:"provider_id"`
	ProviderName     string         `json:"provider_name,omitempty"`
	ProviderKind     string         `json:"provider_kind,omitempty"`
	UpstreamModel    string         `json:"upstream_model,omitempty"`
	StrategyPriority int            `json:"strategy_priority"`
	HTTPStatus       int            `json:"http_status,omitempty"`
	ErrorMessage     string         `json:"error_message,omitempty"`
	ErrorClass       string         `json:"error_class,omitempty"`
	SwitchStrategy   string         `json:"switch_strategy,omitempty"`
	LatencyMs        map[string]int `json:"latency_ms,omitempty"`
	// ImagesOmitted counts older images replaced by a placeholder to fit the
	// account's image cap (see image_limit.go).
	ImagesOmitted int  `json:"images_omitted,omitempty"`
	Final         bool `json:"final,omitempty"`
}
type usageAttemptRecorder struct {
	endpoint string
	// group is the model-group label for the retry counter, set at
	// construction by the caller that owns the route.
	group string
	items []usageAttemptLog
}

func (r *usageAttemptRecorder) Start(rp biz.RouteAccount) int {
	// Starting an attempt on top of a previous one IS a failover retry, so
	// the counter is incremented here rather than at each of the loop's
	// continue sites. The reason is the previous attempt's structured class
	// — the same value that drove the decision to retry.
	if n := len(r.items); n > 0 {
		metrics.LLMRetriesTotal.WithLabelValues(r.group, r.items[n-1].ErrorClass).Inc()
	}
	a := usageAttemptLog{
		Attempt:          len(r.items) + 1,
		ProviderID:       metricOwner(rp),
		ProviderName:     routeAccountName(rp),
		ProviderKind:     routeAccountKind(rp),
		UpstreamModel:    rp.UpstreamModel,
		StrategyPriority: rp.Priority,
	}
	r.items = append(r.items, a)
	return len(r.items) - 1
}

func (r *usageAttemptRecorder) SetResult(idx, httpStatus int, errMsg, switchStrategy string) {
	if idx < 0 || idx >= len(r.items) {
		return
	}
	r.items[idx].HTTPStatus = httpStatus
	// The chain is stored on the usage row: keep only a redacted copy of
	// whatever the upstream wrote.
	r.items[idx].ErrorMessage = biz.SanitizeUpstreamReply(httpStatus, errMsg)
	r.items[idx].SwitchStrategy = switchStrategy
	cls := deriveErrorClass(switchStrategy, httpStatus, errMsg)
	r.items[idx].ErrorClass = string(cls)
	metrics.LLMAttemptsTotal.WithLabelValues(r.items[idx].ProviderID, r.endpoint, string(cls)).Inc()
}

// FinalClass returns the error class of the last recorded attempt — the
// one whose outcome the request ultimately ended on. Empty if no attempt
// has a result yet.
func (r *usageAttemptRecorder) FinalClass() errorClass {
	if len(r.items) == 0 {
		return ""
	}
	return errorClass(r.items[len(r.items)-1].ErrorClass)
}

func (r *usageAttemptRecorder) SetLatency(idx int, key string, d time.Duration) {
	if idx < 0 || idx >= len(r.items) || key == "" {
		return
	}
	if r.items[idx].LatencyMs == nil {
		r.items[idx].LatencyMs = make(map[string]int)
	}
	r.items[idx].LatencyMs[key] = int(d.Milliseconds())
}

func (r *usageAttemptRecorder) SetImagesOmitted(idx, n int) {
	if idx < 0 || idx >= len(r.items) {
		return
	}
	r.items[idx].ImagesOmitted = n
}

func (r *usageAttemptRecorder) MarkFinal(idx int) {
	if idx < 0 || idx >= len(r.items) {
		return
	}
	r.items[idx].Final = true
}

func (r *usageAttemptRecorder) RetryCount() int {
	if len(r.items) == 0 {
		return 0
	}
	return len(r.items) - 1
}

func (r *usageAttemptRecorder) Chain() []string {
	if len(r.items) == 0 {
		return nil
	}
	out := make([]string, 0, len(r.items))
	for _, a := range r.items {
		b, err := json.Marshal(a)
		if err != nil {
			out = append(out, a.ProviderID)
			continue
		}
		out = append(out, usageAttemptPrefix+string(b))
	}
	return out
}

func switchStrategyForStatus(status int) string {
	switch {
	case status == 429:
		return "status_429"
	case status >= 500:
		return fmt.Sprintf("status_%d", status)
	case status >= 400:
		return fmt.Sprintf("client_status_%d", status)
	default:
		return ""
	}
}
