package llm

import (
	"testing"

	"github.com/jucode/jucode/internal/biz"
)

func TestProbeDialect(t *testing.T) {
	cases := []struct {
		proto    string
		dialect  Dialect
		request  biz.RequestProtocol
		wantPath string
	}{
		{biz.NativeProtocolChat, DialectCompatible, biz.RequestProtocolCompatible, "/v1/chat/completions"},
		{biz.NativeProtocolResponses, DialectOpenAI, biz.RequestProtocolResponses, "/v1/responses"},
		{biz.NativeProtocolMessages, DialectAnthropic, biz.RequestProtocolAnthropic, "/v1/messages"},
	}
	for _, tc := range cases {
		d, r, err := probeDialect(tc.proto)
		if err != nil {
			t.Fatalf("%s: %v", tc.proto, err)
		}
		if d != tc.dialect || r != tc.request {
			t.Fatalf("%s: got dialect %s request %s", tc.proto, d, r)
		}
		if got := PathForDialect(d); got != tc.wantPath {
			t.Fatalf("%s: path %s, want %s", tc.proto, got, tc.wantPath)
		}
	}
	if _, _, err := probeDialect("gemini"); err == nil {
		t.Fatal("unknown protocol accepted")
	}
}

func TestAccountProbeBody(t *testing.T) {
	chat := mustUnmarshal(t, mustBody(t, DialectCompatible, "m"))
	if chat["model"] != "m" || chat["max_tokens"] != float64(1) || chat["stream"] != true {
		t.Fatalf("chat body: %v", chat)
	}
	msgs, ok := chat["messages"].([]any)
	if !ok || len(msgs) != 1 {
		t.Fatalf("chat messages: %v", chat)
	}

	resp := mustUnmarshal(t, mustBody(t, DialectOpenAI, "m"))
	if resp["input"] != "ping" || resp["max_output_tokens"] != float64(16) {
		t.Fatalf("responses body: %v", resp)
	}

	ant := mustUnmarshal(t, mustBody(t, DialectAnthropic, "m"))
	if ant["max_tokens"] != float64(1) {
		t.Fatalf("anthropic body: %v", ant)
	}
	if _, hasStream := ant["stream"]; hasStream {
		t.Fatalf("anthropic body carries stream flag: %v", ant)
	}
	if _, hasMsgs := ant["messages"].([]any); !hasMsgs {
		t.Fatalf("anthropic messages: %v", ant)
	}
}

func TestStreamErrorEvent(t *testing.T) {
	cases := []struct {
		name    string
		payload string
		want    bool // true = an error event was found
	}{
		{"openai error object", `data: {"error":{"message":"bad key","type":"auth"}}`, true},
		{"vendor code envelope", `data: {"code":11101,"msg":"Non-stream chat request is currently not supported"}`, true},
		{"clean chunk", `data: {"id":"c1","choices":[{"delta":{"role":"assistant"}}]}`, false},
		{"done sentinel", `data: [DONE]`, false},
		{"zero code passes", `data: {"code":0,"choices":[]}`, false},
		{"unparseable then clean", "data: {broken\ndata: {\"choices\":[]}\n", false},
		{"empty", "", false},
	}
	for _, tc := range cases {
		got := streamErrorEvent([]byte(tc.payload)) != ""
		if got != tc.want {
			t.Fatalf("%s: streamErrorEvent = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func mustBody(t *testing.T, d Dialect, model string) []byte {
	t.Helper()
	body, err := accountProbeBody(d, model)
	if err != nil {
		t.Fatalf("accountProbeBody: %v", err)
	}
	return body
}
