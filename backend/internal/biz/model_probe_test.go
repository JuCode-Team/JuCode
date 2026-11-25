package biz

import (
	"encoding/json"
	"reflect"
	"testing"
)

// The prober dials the address the server was told to listen on. Wildcard binds
// are the trap: ":8090" and "0.0.0.0:8090" are valid listen addresses but not
// valid destinations, and dialing them literally fails in ways that look like
// the gateway is down rather than like a config bug.
func TestLoopbackBaseURLRewritesWildcardBinds(t *testing.T) {
	cases := map[string]string{
		":8090":            "http://127.0.0.1:8090",
		"0.0.0.0:8090":     "http://127.0.0.1:8090",
		"[::]:8090":        "http://127.0.0.1:8090",
		"127.0.0.1:18080":  "http://127.0.0.1:18080",
		"192.168.1.5:8090": "http://192.168.1.5:8090",
	}
	for listen, want := range cases {
		if got := loopbackBaseURL(listen); got != want {
			t.Errorf("loopbackBaseURL(%q) = %q, want %q", listen, got, want)
		}
	}
}

// The probe body must stay minimal — every field here is billed on every tick
// against every enabled model. It also must remain a plain non-streaming chat
// completion: the gateway files its availability observation either way, but a
// stream would hold a connection open for the full idle timeout on a stalled
// channel instead of failing fast.
func TestProbeBodyIsMinimalAndNonStreaming(t *testing.T) {
	raw, err := probeBody("gpt-5.6-sol", false)
	if err != nil {
		t.Fatalf("probeBody: %v", err)
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("probeBody produced invalid json: %v", err)
	}

	if body["model"] != "gpt-5.6-sol" {
		t.Errorf("model = %v, want gpt-5.6-sol", body["model"])
	}
	if body["stream"] != false {
		t.Errorf("stream = %v, want false", body["stream"])
	}
	if body["max_tokens"] != float64(1) {
		t.Errorf("max_tokens = %v, want 1", body["max_tokens"])
	}
	msgs, ok := body["messages"].([]any)
	if !ok || len(msgs) != 1 {
		t.Fatalf("messages = %v, want exactly one", body["messages"])
	}
	for _, k := range []string{"thinking", "enable_thinking", "reasoning_effort", "reasoning"} {
		if _, present := body[k]; present {
			t.Errorf("%q must not be sent unless the link opted in — xAI answers 400 to it", k)
		}
	}
}

// Opting in adds every vendor's spelling at once. They coexist because vendors
// ignore keys they do not know; the risk is the vendor that validates strictly
// instead, which is why this is a per-link switch and not the default.
func TestProbeBodyDisableThinkingAddsEveryVendorSwitch(t *testing.T) {
	raw, err := probeBody("glm-5.2", true)
	if err != nil {
		t.Fatalf("probeBody: %v", err)
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("invalid json: %v", err)
	}

	if got := body["thinking"]; !reflect.DeepEqual(got, map[string]any{"type": "disabled"}) {
		t.Errorf("thinking = %v, want {type: disabled}", got)
	}
	if body["enable_thinking"] != false {
		t.Errorf("enable_thinking = %v, want false", body["enable_thinking"])
	}
	if body["reasoning_effort"] != "none" {
		t.Errorf("reasoning_effort = %v, want none", body["reasoning_effort"])
	}
	if _, ok := body["reasoning"].(map[string]any); !ok {
		t.Errorf("reasoning = %v, want an object", body["reasoning"])
	}
	// The minimal-request guarantees must survive the opt-in.
	if body["max_tokens"] != float64(1) || body["stream"] != false {
		t.Errorf("opting in changed the base request: max_tokens=%v stream=%v", body["max_tokens"], body["stream"])
	}
}

// A rejection from our own billing is not evidence about the channel. These
// three are the ways the probe account itself can be the problem — empty
// balance, a plan that excludes the group, a cost limit — and they must be
// reported as our fault so the silence on the page gets explained.
func TestProbeAccountFaultCoversOurOwnRejections(t *testing.T) {
	for _, status := range []int{402, 403, 429} {
		if !isProbeAccountFault(status) {
			t.Errorf("status %d should be treated as a probe-account fault", status)
		}
	}
	for _, status := range []int{200, 400, 404, 500, 502, 503, 504} {
		if isProbeAccountFault(status) {
			t.Errorf("status %d is about the channel, not the probe account", status)
		}
	}
}
