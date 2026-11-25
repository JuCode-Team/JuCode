package biz

import (
	"strings"
	"testing"
)

const secretPrompt = "my secret prompt text"

func TestSanitizeUpstreamErrorDropsFreeText(t *testing.T) {
	raw := `{"error":{"type":"invalid_request_error","code":"context_length_exceeded","param":"messages","message":"This model's maximum context length is 8192 tokens. Your input: '` + secretPrompt + `'"}}`
	got := SanitizeUpstreamError(raw)
	if strings.Contains(got, secretPrompt) {
		t.Fatalf("request text survived: %s", got)
	}
	for _, want := range []string{"invalid_request_error", "context_length_exceeded", `"param":"messages"`, "maximum context"} {
		if !strings.Contains(got, want) {
			t.Errorf("summary %s lacks %q", got, want)
		}
	}
	// The stored summary resolves to the same reason as the raw body.
	rawReason := ResolveUsageReason("upstream_error", "user_request", 400, raw, "")
	storedReason := ResolveUsageReason("upstream_error", "user_request", 400, got, "")
	if rawReason != storedReason {
		t.Errorf("reason from summary = %+v, from raw = %+v", storedReason, rawReason)
	}
}

func TestSanitizeUpstreamErrorRejectsUnsafeTokens(t *testing.T) {
	raw := `{"error":{"type":"` + secretPrompt + `","param":"` + secretPrompt + `","message":"Invalid 'input[3].content': '` + secretPrompt + `'"}}`
	got := SanitizeUpstreamError(raw)
	if strings.Contains(got, secretPrompt) {
		t.Fatalf("request text survived: %s", got)
	}
	if !strings.Contains(got, "input[3].content") {
		t.Errorf("summary %s lost the parameter path", got)
	}
	if SanitizeUpstreamError("<html>"+secretPrompt+"</html>") != "" {
		t.Error("an unknown body should leave nothing")
	}
}

func TestSanitizeErrorMessage(t *testing.T) {
	cases := map[string]string{
		"billing reserve: insufficient balance":                         "billing reserve: insufficient balance",
		"upstream 400: plain text " + secretPrompt:                      "upstream 400",
		`{"error":{"message":"` + secretPrompt + `"}}`:                  "",
		"upstream 413: {\"error\":{\"message\":\"payload too large\"}}": `upstream 413 {"error":{"message":"payload too large"}}`,
	}
	for in, want := range cases {
		if got := SanitizeErrorMessage(in); got != want {
			t.Errorf("SanitizeErrorMessage(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSanitizeUpstreamReply(t *testing.T) {
	if got := SanitizeUpstreamReply(422, "output_policy_block:violence"); got != "output_policy_block:violence" {
		t.Errorf("gateway token rewritten: %q", got)
	}
	if got := SanitizeUpstreamReply(400, "Bad request: "+secretPrompt); strings.Contains(got, secretPrompt) {
		t.Errorf("upstream text survived: %q", got)
	}
	if got := SanitizeUpstreamReply(0, "dial tcp 10.0.0.1:443: i/o timeout"); got != "dial tcp 10.0.0.1:443: i/o timeout" {
		t.Errorf("transport error rewritten: %q", got)
	}
}

func TestSanitizeTaskError(t *testing.T) {
	if got := SanitizeTaskError("InvalidParameter"); got != "InvalidParameter" {
		t.Errorf("code rewritten: %q", got)
	}
	if got := SanitizeTaskError("prompt '" + secretPrompt + "' rejected"); got != "upstream task failed" {
		t.Errorf("free text kept: %q", got)
	}
}
