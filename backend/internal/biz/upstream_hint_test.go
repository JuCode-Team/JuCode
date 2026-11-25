package biz

import (
	"strings"
	"testing"

	entavailability "github.com/jucode/jucode/internal/data/ent/availabilityobservation"
	entusagelog "github.com/jucode/jucode/internal/data/ent/usagelog"
)

func TestUpstreamErrorParamOnlyPassesStructuralPaths(t *testing.T) {
	cases := map[string]string{
		`{"error":{"message":"Missing required parameter: 'input[91].arguments'","type":"invalid_request_error"}}`:          "input[91].arguments",
		`{"error":{"message":"Invalid 'input[261].id': 'fc_08'. Expected an ID that begins with 'ctc'","type":"x"}}`:        "input[261].id",
		`{"error":{"message":"bad","param":"thinking","type":"invalid_request_error"}}`:                                     "thinking",
		`{"error":{"message":"Missing required parameter: 'see https://x.y/z now'","type":"invalid_request_error"}}`:        "",
		`{"error":{"message":"bad","param":"<script>alert(1)</script>","type":"invalid_request_error"}}`:                    "",
		`{"error":{"message":"Invalid schema for response_format 'codex_judge_output': x","type":"invalid_request_error"}}`: "",
		`not json`: "",
	}
	for body, want := range cases {
		if got := UpstreamErrorParam(body); got != want {
			t.Errorf("UpstreamErrorParam(%s) = %q, want %q", body, got, want)
		}
	}
}

func TestUsageReasonUsesUpstreamHints(t *testing.T) {
	raw := `{"error":{"message":"Function tools with reasoning_effort are not supported for gpt-6-luna in /v1/chat/completions. To use function tools, use /v1/responses or set reasoning_effort to 'none'.","type":"invalid_request_error"}}`
	r := ResolveUsageReason("upstream_error", "user_request", 400, raw, "")
	if r.Code != "tools_with_reasoning_unsupported" || !strings.Contains(r.Text, "/v1/responses") {
		t.Fatalf("reason = %+v", r)
	}
	// Older rows carry only the truncated message.
	r = ResolveUsageReason("upstream_error", "user_request", 400, "", `{"error":{"message":"Missing required parameter: 'input[91].input'","type":"invalid_request_error"}}`)
	if r.Code != "invalid_request" || !strings.Contains(r.Text, "input[91].input") {
		t.Fatalf("reason = %+v", r)
	}
	r = ResolveUsageReason("upstream_error", "upstream_config", 405, `{"error":{"message":"Method Not Allowed"}}`, "")
	if r.Code != "upstream_route_unsupported" {
		t.Fatalf("reason = %+v", r)
	}
}

func TestUpstreamConfigCountsAgainstAvailability(t *testing.T) {
	outcome, class := classifyAvailabilityOutcome(WriteUsageLogInput{
		Status: entusagelog.StatusUpstreamError, HTTPStatus: 405, ErrorClass: "upstream_config",
	})
	if outcome != entavailability.OutcomeBad || class != "upstream_config" {
		t.Fatalf("outcome = %s/%s, want bad/upstream_config", outcome, class)
	}
	outcome, _ = classifyAvailabilityOutcome(WriteUsageLogInput{
		Status: entusagelog.StatusUpstreamError, HTTPStatus: 400, ErrorClass: "user_request",
	})
	if outcome != entavailability.OutcomeExcluded {
		t.Fatalf("user_request outcome = %s, want excluded", outcome)
	}
	// The error monitor's filter treats upstream_config as a system error and
	// user_request as caller-side.
	if !isErrorAnomaly("upstream_error", "upstream_config") || isErrorAnomaly("upstream_error", "user_request") {
		t.Fatalf("anomaly classification of upstream_config/user_request is wrong")
	}
}
