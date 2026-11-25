// account_probe.go: admin-triggered one-shot health probe against a single
// account.
//
// model_probe.go measures customer-visible availability by calling our own
// gateway over loopback and letting routing pick the account. This probe
// answers the opposite question — "does THIS account, on THIS protocol,
// serve THIS model" — so it bypasses routing and talks to the account's own
// upstream directly, through the same Provider construction the request path
// uses: credential resolution, the auth scheme (Bearer vs X-API-Key,
// anthropic-version) and the account's endpoint_paths overrides.
//
// It is observational: nothing here touches health state, cooldowns or
// billing. The bill for the probe lands on the account's own upstream quota,
// as any request would.
package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/jucode/jucode/internal/biz"
	"github.com/jucode/jucode/internal/data/ent"
)

// accountProbeTimeout bounds the whole attempt. A ping should return in
// seconds; a first token slower than this is itself the answer the operator
// is looking for.
const accountProbeTimeout = 45 * time.Second

// probeDetailLimit bounds the upstream body kept for the response. Enough to
// carry the vendor's own error message; small enough that a misbehaving
// endpoint cannot fill the admin response with junk.
const probeDetailLimit = 600

// AccountProbeResult is one targeted probe's outcome. OK=false carries
// either a transport/credential error in Error or the upstream's status and
// truncated body in Detail.
type AccountProbeResult struct {
	OK        bool   `json:"ok"`
	Status    int    `json:"status"` // upstream HTTP status; 0 when nothing answered
	LatencyMS int64  `json:"latency_ms"`
	Model     string `json:"model"`    // model name actually sent upstream
	Protocol  string `json:"protocol"` // native protocol used
	URL       string `json:"url"`      // base_url + resolved path
	Error     string `json:"error,omitempty"`
	Detail    string `json:"detail,omitempty"`
	// Sample is the start of what the upstream answered on success, so the
	// operator sees real output rather than a bare status.
	Sample string `json:"sample,omitempty"`
}

// ProbeAccount sends one minimal generation request to acct's upstream.
// proto is a native protocol tag (chat_completions / responses / messages);
// model is the name to send (the caller resolves upstream_model bindings
// first). Malformed arguments return an error; everything downstream —
// missing credential, unsatisfiable egress, transport failure, non-2xx — is
// reported inside the result, since each of those IS a health verdict.
func ProbeAccount(ctx context.Context, accounts *biz.AccountPoolSvc, acct *ent.UpstreamAccount, proto, model string) (*AccountProbeResult, error) {
	dialect, reqProto, err := probeDialect(proto)
	if err != nil {
		return nil, err
	}
	model = strings.TrimSpace(model)
	if model == "" {
		return nil, fmt.Errorf("%w: model is required", biz.ErrAccountBadInput)
	}
	body, err := accountProbeBody(dialect, model)
	if err != nil {
		return nil, err
	}
	path := PathForDialect(dialect)
	if override := biz.EndpointPathFor(acct, proto); override != "" {
		path = override
	}
	result := &AccountProbeResult{
		Model:    model,
		Protocol: proto,
		URL:      strings.TrimRight(acct.BaseURL, "/") + path,
	}

	token, err := accounts.RequestToken(ctx, acct)
	if err != nil {
		result.Error = "credential: " + err.Error()
		return result, nil
	}
	client := &http.Client{Transport: newProviderTransport()}

	var prov Provider
	if reqProto == biz.RequestProtocolAnthropic {
		prov = NewAnthropicProviderWithClient(acct, token, client)
	} else {
		prov = NewOpenAIProviderWithClient(acct, token, client)
	}

	// clientHeaders stands in for a client request's headers.
	clientHeaders := http.Header{
		"Content-Type":      {"application/json"},
		"Anthropic-Version": {"2023-06-01"},
	}
	pctx, cancel := context.WithTimeout(ctx, accountProbeTimeout)
	defer cancel()
	started := time.Now()
	resp, err := prov.Forward(pctx, path, body, dialect == DialectCompatible, clientHeaders)
	result.LatencyMS = time.Since(started).Milliseconds()
	if err != nil {
		result.Error = err.Error()
		return result, nil
	}
	defer resp.Body.Close()
	payload, _ := io.ReadAll(io.LimitReader(resp.Body, probeDetailLimit))
	result.Status = resp.StatusCode
	result.OK = resp.StatusCode >= 200 && resp.StatusCode < 300
	if result.OK && strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream") {
		// A streamed chat can still fail inside the stream — the vendor's
		// verdict rides in the first data event, not the status line.
		if detail := streamErrorEvent(payload); detail != "" {
			result.OK = false
			result.Detail = detail
		}
	} else if !result.OK {
		result.Detail = strings.TrimSpace(string(payload))
	}
	if result.OK {
		result.Sample = strings.TrimSpace(string(payload))
	}
	return result, nil
}

// Web tool probes: one real call with a fixed input, so a pass means the key,
// egress and vendor all work end to end. The vendor bills it like any call.
const (
	webProbeQuery = "status check"
	webProbeURL   = "https://example.com"
	// webProbeSampleChars bounds the content summary in the result.
	webProbeSampleChars = 600
)

// ProbeWebAccount runs one search or fetch (kind) through a web tool vendor
// account and reports the outcome like ProbeAccount does.
func ProbeWebAccount(ctx context.Context, accounts *biz.AccountPoolSvc, acct *ent.UpstreamAccount, kind string) (*AccountProbeResult, error) {
	engine, ok := biz.WebEngineByKind(kind, acct.APIFormat)
	if !ok {
		return nil, fmt.Errorf("%w: vendor %q does not serve web %s", biz.ErrAccountBadInput, acct.APIFormat, kind)
	}
	req := webToolRequest{engine: engine}
	if kind == biz.WebToolFetch {
		req.fetch = webFetchInput{URL: webProbeURL}
	} else {
		req.search = webSearchInput{Query: webProbeQuery, MaxResults: 3}
	}
	result := &AccountProbeResult{Model: engine.ID, Protocol: biz.NativeProtocolWeb, URL: strings.TrimRight(acct.BaseURL, "/")}
	token, err := accounts.RequestToken(ctx, acct)
	if err != nil {
		result.Error = "credential: " + err.Error()
		return result, nil
	}
	client := &http.Client{Transport: newProviderTransport()}
	pctx, cancel := context.WithTimeout(ctx, accountProbeTimeout)
	defer cancel()
	started := time.Now()
	res, err := forwardWebTool(pctx, NewOpenAIProviderWithClient(acct, token, client), req)
	result.LatencyMS = time.Since(started).Milliseconds()
	if err != nil {
		result.Error = err.Error()
		return result, nil
	}
	result.Status = res.StatusCode
	result.OK = res.StatusCode == http.StatusOK
	if !result.OK {
		result.Detail = truncate(strings.TrimSpace(string(res.Body)), probeDetailLimit)
		return result, nil
	}
	result.Sample = webProbeSample(kind, res.Body)
	return result, nil
}

// webProbeSample summarizes a successful web tool answer: the result count
// and titles for a search, the title and opening text for a fetch.
func webProbeSample(kind string, body []byte) string {
	if kind == biz.WebToolFetch {
		var out webFetchResponse
		if json.Unmarshal(body, &out) != nil {
			return truncate(string(body), webProbeSampleChars)
		}
		return truncate(fmt.Sprintf("%s\n%s", out.Title, strings.TrimSpace(out.Content)), webProbeSampleChars)
	}
	var out webSearchResponse
	if json.Unmarshal(body, &out) != nil {
		return truncate(string(body), webProbeSampleChars)
	}
	lines := []string{fmt.Sprintf("%d 条结果", len(out.Results))}
	for _, r := range out.Results {
		lines = append(lines, r.Title+" — "+r.URL)
	}
	return truncate(strings.Join(lines, "\n"), webProbeSampleChars)
}

// streamErrorEvent scans the first data event of an SSE payload for an error
// verdict — either an OpenAI-style {"error": {...}} object or a vendor
// envelope carrying a non-zero code. A clean first event (or no parseable
// event at all) means the stream started healthy.
func streamErrorEvent(payload []byte) string {
	for _, line := range strings.Split(string(payload), "\n") {
		data, ok := strings.CutPrefix(strings.TrimSpace(line), "data:")
		if !ok {
			continue
		}
		data = strings.TrimSpace(data)
		if data == "" || data == "[DONE]" {
			continue
		}
		var event map[string]any
		if err := json.Unmarshal([]byte(data), &event); err != nil {
			continue
		}
		if e, present := event["error"]; present && e != nil {
			return data
		}
		if code, ok := event["code"].(float64); ok && code != 0 {
			return data
		}
		return ""
	}
	return ""
}

// probeDialect maps the account-vocabulary protocol tag onto the gateway
// dialect it is spoken in.
func probeDialect(proto string) (Dialect, biz.RequestProtocol, error) {
	switch proto {
	case biz.NativeProtocolChat:
		return DialectCompatible, biz.RequestProtocolCompatible, nil
	case biz.NativeProtocolResponses:
		return DialectOpenAI, biz.RequestProtocolResponses, nil
	case biz.NativeProtocolMessages:
		return DialectAnthropic, biz.RequestProtocolAnthropic, nil
	}
	return "", "", fmt.Errorf("%w: unknown protocol %q", biz.ErrAccountBadInput, proto)
}

// accountProbeBody is the smallest request that still exercises the account
// end to end. Same discipline as model_probe.go's probeBody: one-token cap
// where the dialect honours it, no thinking flags — those are per-vendor and
// a vendor that rejects them would redden the result for the wrong reason.
func accountProbeBody(d Dialect, model string) ([]byte, error) {
	body := map[string]any{"model": model}
	switch d {
	case DialectAnthropic:
		body["max_tokens"] = 1
		body["messages"] = []map[string]string{{"role": "user", "content": "ping"}}
	case DialectOpenAI:
		body["input"] = "ping"
		// max_output_tokens has a 16-token floor on the Responses API; lower
		// values are rejected before the model is even consulted.
		body["max_output_tokens"] = 16
		body["stream"] = false
	default:
		body["max_tokens"] = 1
		// Chat pings stream: some vendors only serve streamed chat (the
		// non-stream shape is rejected outright), every compatible one
		// accepts it, and a streamed failure still surfaces via
		// streamErrorEvent.
		body["stream"] = true
		body["messages"] = []map[string]string{{"role": "user", "content": "ping"}}
	}
	return json.Marshal(body)
}
