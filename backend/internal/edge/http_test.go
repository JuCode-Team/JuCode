package edge

import (
	"bytes"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/jucode/jucode/internal/llm"
	"github.com/jucode/jucode/internal/region"
)

func TestRegionalEndpoint(t *testing.T) {
	for _, tc := range []struct {
		path string
		want string
	}{
		{"/v1/chat/completions", "chat_completions"},
		{"/v1/responses", "responses"},
		{"/v1/responses/compact", "responses"},
		{"/anthropic/v1/messages", "messages"},
		{"/v1/messages", "messages"},
		{"/v1/messages/count_tokens", "messages"},
	} {
		if got := regionalEndpoint(tc.path); got != tc.want {
			t.Fatalf("regionalEndpoint(%q) = %q; want %q", tc.path, got, tc.want)
		}
	}
}

func TestRequestProtocol(t *testing.T) {
	for _, tc := range []struct {
		path string
		want string
	}{
		{"/v1/chat/completions", "openai"},
		{"/v1/responses", "openai"},
		{"/v1/responses/compact", "openai"},
		{"/anthropic/v1/messages", "anthropic"},
		{"/v1/messages", "anthropic"},
	} {
		if got := requestProtocol(tc.path); got != tc.want {
			t.Fatalf("requestProtocol(%q) = %q; want %q", tc.path, got, tc.want)
		}
	}
}

func TestUpstreamKey(t *testing.T) {
	p := cachedUpstream{apiKey: "sk-openai", anthropicAPIKey: "sk-ant"}
	if got := p.upstreamKey("openai"); got != "sk-openai" {
		t.Fatalf("openai = %q, want sk-openai", got)
	}
	if got := p.upstreamKey("anthropic"); got != "sk-ant" {
		t.Fatalf("anthropic = %q, want sk-ant", got)
	}
	legacy := cachedUpstream{apiKey: "sk-shared"}
	if got := legacy.upstreamKey("anthropic"); got != "sk-shared" {
		t.Fatalf("legacy anthropic fallback = %q, want sk-shared", got)
	}
	claudeOnly := cachedUpstream{anthropicAPIKey: "sk-ant-only"}
	if got := claudeOnly.upstreamKey("anthropic"); got != "sk-ant-only" {
		t.Fatalf("anthropic override-only = %q, want sk-ant-only", got)
	}
	if got := claudeOnly.upstreamKey("openai"); got != "" {
		t.Fatalf("openai must not inherit the anthropic override, got %q", got)
	}
}

func TestJoinURL(t *testing.T) {
	for _, tc := range []struct{ base, request, want string }{
		{"https://api.example.com", "/v1/chat/completions", "https://api.example.com/v1/chat/completions"},
		{"https://api.example.com/v1", "/v1/chat/completions", "https://api.example.com/v1/chat/completions"},
		{"https://api.example.com/", "v1/responses", "https://api.example.com/v1/responses"},
	} {
		if got := joinURL(tc.base, tc.request); got != tc.want {
			t.Fatalf("joinURL(%q,%q) = %q; want %q", tc.base, tc.request, got, tc.want)
		}
	}
}

func TestResponseCost(t *testing.T) {
	s := &Service{}
	price := region.ModelPrice{BillingMode: "per_token", InputPer1K: "0.001", OutputPer1K: "0.002"}
	got, ok := s.responseCost(price, []byte(`{"usage":{"prompt_tokens":1000,"completion_tokens":500}}`))
	if !ok || got != 2_000 {
		t.Fatalf("cost = %d, %v; want 2000 units", got, ok)
	}
}

func TestResponseCostRoundsComputedMicroCNY(t *testing.T) {
	s := &Service{}
	price := region.ModelPrice{BillingMode: "per_token", InputPer1K: "0.00012", OutputPer1K: "0.00048"}
	got, ok := s.responseCost(price, []byte(`{"usage":{"prompt_tokens":14,"completion_tokens":130}}`))
	if !ok || got != 64 {
		t.Fatalf("cost = %d, %v; want 64 micro-CNY", got, ok)
	}
	_, in, out, ok := s.responseUsage(price, []byte(`{"usage":{"prompt_tokens":14,"completion_tokens":130}}`))
	if !ok || in != 14 || out != 130 {
		t.Fatalf("usage = %d, %d, %v; want 14, 130, true", in, out, ok)
	}
}

func TestResponsesNestedUsage(t *testing.T) {
	s := &Service{}
	price := region.ModelPrice{BillingMode: "per_token", InputPer1K: "0.00012", OutputPer1K: "0.00048"}
	body := []byte(`{"type":"response.completed","response":{"usage":{"input_tokens":14,"output_tokens":130}}}`)
	if got, in, out, ok := s.responseUsage(price, body); !ok || got != 64 || in != 14 || out != 130 {
		t.Fatalf("Responses usage = %d, %d, %d, %v; want 64, 14, 130, true", got, in, out, ok)
	}
}

func TestAnthropicStreamUsageAndTTFT(t *testing.T) {
	s := &Service{}
	price := region.ModelPrice{BillingMode: "per_token", InputPer1K: "0.001", OutputPer1K: "0.002"}
	stream := strings.Join([]string{
		`data: {"type":"message_start","message":{"usage":{"input_tokens":1200,"cache_read_input_tokens":300,"output_tokens":1}}}`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hello"}}`,
		`data: {"type":"message_delta","usage":{"output_tokens":400}}`,
		`data: [DONE]`,
	}, "\n\n") + "\n\n"
	acc := &llm.Accumulator{}
	firstToken := 0
	if err := llm.PipeAndExtractOpts(&bytes.Buffer{}, strings.NewReader(stream), acc, llm.PipeOptions{OnFirstText: func() { firstToken++ }}); err != nil {
		t.Fatal(err)
	}
	if got, in, out, ok := s.accumulatedUsage(price, acc); !ok || got != 2300 || in != 1500 || out != 400 {
		t.Fatalf("Anthropic usage = %d, %d, %d, %v; want 2300, 1500, 400, true", got, in, out, ok)
	}
	if firstToken != 1 {
		t.Fatalf("TTFT callback count = %d; want 1", firstToken)
	}
}

func TestAccumulatedUsageUsesCachePrices(t *testing.T) {
	s := &Service{}
	price := region.ModelPrice{
		BillingMode: "per_token", InputPer1K: "1", OutputPer1K: "10",
		CacheReadPer1K: "0.1", CacheWritePer1K: "1.25",
	}
	acc := &llm.Accumulator{TokensIn: 100, CachedTokensIn: 800, CacheCreationTokensIn: 100, TokensOut: 50, HasUsage: true}
	if got, in, out, ok := s.accumulatedUsage(price, acc); !ok || got != 805000 || in != 1000 || out != 50 {
		t.Fatalf("cached usage = %d, %d, %d, %v; want 805000, 1000, 50, true", got, in, out, ok)
	}
}

func TestAccumulatedUsageSplitsCacheWriteTTL(t *testing.T) {
	s := &Service{}
	price := region.ModelPrice{
		BillingMode: "per_token", InputPer1K: "1", OutputPer1K: "10",
		CacheWritePer1K: "2", CacheWrite1hPer1K: "4",
	}
	acc := &llm.Accumulator{
		CacheCreationTokensIn: 1000, CacheCreation5mTokensIn: 400, CacheCreation1hTokensIn: 600,
		HasUsage: true,
	}
	// 400*2 + 600*4 = 3200 per 1K = 3.2 CNY
	if got, _, _, ok := s.accumulatedUsage(price, acc); !ok || got != 3_200_000 {
		t.Fatalf("5m/1h usage = %d, %v; want 3200000, true", got, ok)
	}
}

func TestAccumulatedUsageUsesAudioPrices(t *testing.T) {
	s := &Service{}
	price := region.ModelPrice{
		BillingMode: "per_token", InputPer1K: "1", OutputPer1K: "10",
		AudioInputPer1K: "8", AudioOutputPer1K: "80",
	}
	acc := &llm.Accumulator{TokensIn: 100, TokensOut: 50, AudioTokensIn: 900, AudioTokensOut: 200, HasUsage: true}
	// 100*1 + 50*10 + 900*8 + 200*80 = 23,800 per 1K = 23.8 CNY
	got, in, out, ok := s.accumulatedUsage(price, acc)
	if !ok || got != 23_800_000 || in != 1000 || out != 250 {
		t.Fatalf("audio usage = %d, %d, %d, %v; want 23800000, 1000, 250, true", got, in, out, ok)
	}
}

// An edge node with no audio prices must still bill the tokens — the split
// moved them out of TokensOut, so ignoring them would make voice free.
func TestAccumulatedUsageAudioFallsBackToTextPrices(t *testing.T) {
	s := &Service{}
	price := region.ModelPrice{BillingMode: "per_token", InputPer1K: "1", OutputPer1K: "10"}
	acc := &llm.Accumulator{AudioTokensIn: 1000, AudioTokensOut: 1000, HasUsage: true}
	got, _, _, ok := s.accumulatedUsage(price, acc)
	if !ok || got != 11_000_000 {
		t.Fatalf("audio fallback cost = %d, %v; want 11000000, true", got, ok)
	}
}

func TestVisibleModels(t *testing.T) {
	s := &Service{upstreams: []cachedUpstream{
		{snapshot: region.ProviderSnapshot{Models: []string{"deepseek-v4-flash", "kimi-k3"}}},
		{snapshot: region.ProviderSnapshot{Models: []string{"gpt-5.5"}}},
	}}
	got := s.visibleModels([]string{"gpt-5.5", "not-configured", "deepseek-v4-flash", "gpt-5.5"})
	want := []string{"deepseek-v4-flash", "gpt-5.5"}
	if len(got) != len(want) {
		t.Fatalf("visible models = %#v; want %#v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("visible models = %#v; want %#v", got, want)
		}
	}
}

func TestVisibleModelsWithUnrestrictedProvider(t *testing.T) {
	s := &Service{upstreams: []cachedUpstream{
		{snapshot: region.ProviderSnapshot{}},
	}}
	got := s.visibleModels([]string{"kimi-k3", "deepseek-v4-flash"})
	want := []string{"deepseek-v4-flash", "kimi-k3"}
	if len(got) != len(want) {
		t.Fatalf("visible models = %#v; want %#v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("visible models = %#v; want %#v", got, want)
		}
	}
}

func TestForwardPassesOpenAISessionHeaders(t *testing.T) {
	var gotReq *http.Request
	s := &Service{
		hc: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			gotReq = r.Clone(r.Context())
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader(`{}`)),
				Request:    r,
			}, nil
		})},
	}
	in, err := http.NewRequest(http.MethodPost, "http://edge.local/v1/responses", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	in.Header.Set("User-Agent", "codex_cli_rs/0.87.0")
	in.Header.Set("session_id", "sess-abc")
	in.Header.Set("session-id", "thread-xyz")
	in.Header.Set("x-codex-parent-thread-id", "parent-1")
	in.Header.Set("Authorization", "Bearer sk-juc-client")
	in.Header.Set("Cookie", "session=secret")

	resp, err := s.forward(in, cachedUpstream{
		snapshot: region.ProviderSnapshot{BaseURL: "https://upstream.example"},
		apiKey:   "provider-key",
	}, "/v1/responses", []byte(`{}`), false)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if gotReq == nil {
		t.Fatal("request was not sent")
	}
	if gotReq.Header.Get("Authorization") != "Bearer provider-key" {
		t.Fatalf("Authorization = %q", gotReq.Header.Get("Authorization"))
	}
	if gotReq.Header.Get("Cookie") != "" {
		t.Fatalf("Cookie leaked: %q", gotReq.Header.Get("Cookie"))
	}
	if gotReq.Header.Get("User-Agent") != "codex_cli_rs/0.87.0" {
		t.Fatalf("User-Agent = %q", gotReq.Header.Get("User-Agent"))
	}
	if gotReq.Header.Get("session_id") != "sess-abc" {
		t.Fatalf("session_id = %q", gotReq.Header.Get("session_id"))
	}
	if gotReq.Header.Get("session-id") != "thread-xyz" {
		t.Fatalf("session-id = %q", gotReq.Header.Get("session-id"))
	}
	if gotReq.Header.Get("x-codex-parent-thread-id") != "parent-1" {
		t.Fatalf("x-codex-parent-thread-id = %q", gotReq.Header.Get("x-codex-parent-thread-id"))
	}
}

func TestForwardPassesAnthropicBeta(t *testing.T) {
	var gotReq *http.Request
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		gotReq = r.Clone(r.Context())
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(`{}`)),
			Request:    r,
		}, nil
	})}
	s := &Service{hc: client, streamHC: client}
	in, err := http.NewRequest(http.MethodPost, "http://edge.local/v1/messages", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	in.Header.Set("Anthropic-Beta", "prompt-caching-2024-07-31,fine-grained-tool-streaming-2025-05-14")
	in.Header.Set("Anthropic-Version", "2023-06-01")
	in.Header.Set("X-API-Key", "sk-juc-client")
	in.Header.Set("Authorization", "Bearer sk-juc-client")

	resp, err := s.forward(in, cachedUpstream{
		snapshot: region.ProviderSnapshot{BaseURL: "https://upstream.example"},
		apiKey:   "provider-key",
	}, "/v1/messages", []byte(`{}`), true)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if gotReq.Header.Get("X-API-Key") != "provider-key" {
		t.Fatalf("X-API-Key = %q", gotReq.Header.Get("X-API-Key"))
	}
	if strings.Contains(gotReq.Header.Get("Authorization"), "sk-juc-client") || strings.Contains(gotReq.Header.Get("X-API-Key"), "sk-juc-client") {
		t.Fatalf("caller credential leaked: %v", gotReq.Header)
	}
	beta := gotReq.Header.Get("Anthropic-Beta")
	for _, want := range []string{"prompt-caching-2024-07-31", "fine-grained-tool-streaming-2025-05-14"} {
		if !strings.Contains(beta, want) {
			t.Fatalf("Anthropic-Beta = %q, missing %s", beta, want)
		}
	}
	if gotReq.Header.Get("Anthropic-Version") != "2023-06-01" {
		t.Fatalf("Anthropic-Version = %q", gotReq.Header.Get("Anthropic-Version"))
	}
}

func TestEdgeTransportAttemptsHTTP2(t *testing.T) {
	tr, ok := edgeTransport().(*http.Transport)
	if !ok || !tr.ForceAttemptHTTP2 {
		t.Fatal("edge transport is not attempting HTTP/2")
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func TestLedgerAccountIDUsesOrganizationScope(t *testing.T) {
	userID := uuid.New()
	organizationID := uuid.New()

	personal, err := ledgerAccountID(&region.AccountSnapshot{UserID: userID, BillingScope: region.BillingScopePersonal})
	if err != nil || personal != userID {
		t.Fatalf("personal ledger account = %s, %v; want user %s", personal, err, userID)
	}
	organization, err := ledgerAccountID(&region.AccountSnapshot{
		UserID: userID, BillingScope: region.BillingScopeOrganization, OrganizationID: &organizationID,
	})
	if err != nil || organization != organizationID {
		t.Fatalf("organization ledger account = %s, %v; want organization %s", organization, err, organizationID)
	}
	if _, err := ledgerAccountID(&region.AccountSnapshot{UserID: userID, BillingScope: region.BillingScopeOrganization}); err == nil {
		t.Fatal("organization account without organization id should fail")
	}
}

// A lease the control plane refuses is the client's problem (no balance, a
// cost limit), not an outage: it must not surface as 503.
func TestReservationErrorMapsControlPlaneRefusals(t *testing.T) {
	cases := []struct {
		err    error
		status int
		code   string
	}{
		{ErrInsufficientCredit, http.StatusPaymentRequired, "insufficient_balance"},
		{&region.Error{Status: 402, Code: region.CodeInsufficientBalance}, http.StatusPaymentRequired, "insufficient_balance"},
		{&region.Error{Status: 402, Code: region.CodeAPIKeyCostLimit}, http.StatusForbidden, "cost_limit_exceeded"},
		{&region.Error{Status: 402, Code: region.CodeMemberLimit}, http.StatusForbidden, "organization_limit_exceeded"},
		{&region.Error{Status: 403, Code: region.CodeOrganizationForbidden}, http.StatusForbidden, "organization_forbidden"},
		{&region.Error{Status: 500, Code: region.CodeInternal}, http.StatusServiceUnavailable, "billing_unavailable"},
	}
	for _, tc := range cases {
		status, code, _ := reservationError(tc.err)
		if status != tc.status || code != tc.code {
			t.Errorf("reservationError(%v) = %d %s; want %d %s", tc.err, status, code, tc.status, tc.code)
		}
	}
}
