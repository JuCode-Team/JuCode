package llm

import (
	"bytes"
	"context"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"

	"github.com/jucode/jucode/internal/biz"
	"github.com/jucode/jucode/internal/data/ent"
	"github.com/jucode/jucode/internal/data/ent/schema"
)

// Provider is the gateway's view of an upstream LLM endpoint. It is
// intentionally narrower than ent.UpstreamAccount: only what's needed to
// actually forward a request.
type Provider interface {
	ID() uuid.UUID
	Name() string
	Kind() string
	// Forward issues the upstream POST and returns the raw response so
	// the caller can either pipe SSE bytes (stream) or json-decode.
	// Caller must close resp.Body.
	Forward(ctx context.Context, path string, body []byte, stream bool, clientHeaders http.Header) (*http.Response, error)
}

// --- openai_compatible ---

type openaiProvider struct {
	id           uuid.UUID
	name         string
	baseURL      string
	apiKey       string
	organization string
	// format is the generation dialect (openai / dashscope / …), resolved
	// once at construction. Empty/"openai" ⇒ OpenAI-compatible passthrough
	// for the sync generation endpoints; other values select a native
	// adaptor (see provider_generation.go / image_native.go).
	format string
	// headerRules are the account's upstream header rewrites, applied last
	// on every request this provider sends.
	headerRules schema.HeaderRules
	hc          *http.Client
	// azureOpenAI turns on the Azure OpenAI request fixes.
	azureOpenAI bool
}

// newProviderTransport builds the upstream transport shared by every client
// for one provider. Streaming workload, thousands of
// concurrent long-lived connections per upstream host. Stdlib defaults
// would force a handshake on nearly every reuse.
// Bound connection establishment only — a dead/refused upstream
// (e.g. new-api down) fails fast instead of hanging. We deliberately
// do NOT set ResponseHeaderTimeout: this is a streaming relay and a
// slow first byte is handled by the stream first-text timeout, not by
// killing (and non-idempotently re-sending) the request here.
func newProviderTransport() *http.Transport {
	return &http.Transport{
		DialContext: (&net.Dialer{
			Timeout:   5 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		TLSHandshakeTimeout:   5 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		MaxConnsPerHost:       0,
		MaxIdleConns:          10000,
		MaxIdleConnsPerHost:   5000,
		IdleConnTimeout:       90 * time.Second,
		DisableCompression:    true,
		// A custom DialContext turns HTTP/2 off unless this is set. The
		// upstreams we stream from (Anthropic, OpenAI) negotiate HTTP/2, and
		// the HTTP/1.1 fallback holds the first body byte longer.
		ForceAttemptHTTP2: true,
	}
}

// NewOpenAIProvider takes the decrypted API key (and optional org).
func NewOpenAIProvider(a *ent.UpstreamAccount, apiKey string) Provider {
	return NewOpenAIProviderWithClient(a, apiKey, &http.Client{Transport: newProviderTransport()})
}

// NewOpenAIProviderWithClient is NewOpenAIProvider over a caller-supplied
// client, so account-backed requests reuse one connection pool per account
// while carrying a per-account credential.
func NewOpenAIProviderWithClient(a *ent.UpstreamAccount, apiKey string, client *http.Client) Provider {
	org := ""
	if a.Organization != nil {
		org = *a.Organization
	}
	return &openaiProvider{
		id:           a.ID,
		name:         a.Name,
		baseURL:      strings.TrimRight(a.BaseURL, "/"),
		apiKey:       apiKey,
		organization: org,
		format:       ProviderFormat(a),
		headerRules:  a.HeaderRules,
		hc:           client,
		azureOpenAI:  a.DefinitionID == biz.AzureOpenAIDefinitionID,
	}
}

func (o *openaiProvider) ID() uuid.UUID { return o.id }
func (o *openaiProvider) Name() string  { return o.name }
func (o *openaiProvider) Kind() string  { return "openai_compatible" }

func (o *openaiProvider) Forward(ctx context.Context, path string, body []byte, stream bool, clientHeaders http.Header) (*http.Response, error) {
	if o.azureOpenAI && strings.Contains(path, "/responses") {
		adapted, err := adaptAzureOpenAIResponsesBody(body)
		if err != nil {
			return nil, err
		}
		body = adapted
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+o.apiKey)
	if o.organization != "" {
		req.Header.Set("OpenAI-Organization", o.organization)
	}
	if stream {
		req.Header.Set("Accept", "text/event-stream")
	} else {
		req.Header.Set("Accept", "application/json")
	}
	ApplyOpenAIClientSessionHeaders(req.Header, clientHeaders)
	biz.ApplyHeaderRules(req.Header, o.headerRules, o.apiKey)
	return o.hc.Do(req)
}

// DialWS opens an upstream WebSocket (OpenAI Realtime). path should include
// any query string, e.g. "/v1/realtime?model=gpt-4o-realtime-preview".
func (o *openaiProvider) DialWS(ctx context.Context, path string, clientHeaders http.Header) (*websocket.Conn, *http.Response, error) {
	u := strings.TrimRight(o.baseURL, "/") + path
	u = strings.Replace(u, "https://", "wss://", 1)
	u = strings.Replace(u, "http://", "ws://", 1)
	hdr := http.Header{}
	hdr.Set("Authorization", "Bearer "+o.apiKey)
	if o.organization != "" {
		hdr.Set("OpenAI-Organization", o.organization)
	}
	ApplyOpenAIClientSessionHeaders(hdr, clientHeaders)
	if beta := openaiWSBeta(path, clientHeaders); beta != "" {
		hdr.Set("OpenAI-Beta", beta)
	}
	if proto := sanitizeUpstreamSubprotocols(clientHeaders.Get("Sec-WebSocket-Protocol")); proto != "" {
		hdr.Set("Sec-WebSocket-Protocol", proto)
	}
	biz.ApplyHeaderRules(hdr, o.headerRules, o.apiKey)
	d := websocket.Dialer{HandshakeTimeout: 15 * time.Second, EnableCompression: true}
	return d.DialContext(ctx, u, hdr)
}

// openaiWSBeta picks the OpenAI-Beta header for an upstream WebSocket dial.
// Realtime needs realtime=v1 when the client omitted it; Responses WS must
// not inherit that default — Codex treats it as the wrong product and the
// handshake fails, after which we silently fall back to the HTTP bridge.
func openaiWSBeta(path string, clientHeaders http.Header) string {
	if p := clientHeaders.Get("OpenAI-Beta"); p != "" {
		return p
	}
	if strings.Contains(path, "/realtime") {
		return "realtime=v1"
	}
	return ""
}

// wsInsecureKeyPrefix marks the subprotocol entry that carries a caller's key.
// Deliberately matched without the trailing dot and case-folded: stripping one
// entry too many costs a negotiation value, missing one hands a customer's key
// to a third-party operator. middleware.websocketCredential recognises the same
// marker and the two have to agree — an entry one accepts and the other does
// not strip is exactly the leak.
const wsInsecureKeyPrefix = "openai-insecure-api-key"

// sanitizeUpstreamSubprotocols filters a client's Sec-WebSocket-Protocol list
// before it is replayed to the upstream. Browser clients cannot set headers on
// a WebSocket, so the OpenAI convention smuggles the credential through the
// subprotocol list as "openai-insecure-api-key.<key>" — that entry holds the
// caller's JuCode key, which must never reach the upstream operator. Real
// negotiation values (e.g. "realtime", "openai-beta.realtime-v1") pass through.
func sanitizeUpstreamSubprotocols(raw string) string {
	if raw == "" {
		return ""
	}
	kept := make([]string, 0, 2)
	for _, p := range strings.Split(raw, ",") {
		p = strings.TrimSpace(p)
		// Matched case-insensitively: this is the last gate before a secret
		// leaves for a third party, so it must not depend on a client spelling
		// the marker the way we expect.
		if p == "" || strings.HasPrefix(strings.ToLower(p), wsInsecureKeyPrefix) {
			continue
		}
		kept = append(kept, p)
	}
	return strings.Join(kept, ", ")
}

// --- anthropic ---

type anthropicProvider struct {
	id          uuid.UUID
	name        string
	baseURL     string
	apiKey      string
	headerRules schema.HeaderRules
	hc          *http.Client
	// azureFoundry turns on the Claude-on-Foundry request fixes.
	azureFoundry bool
}

// NewAnthropicProvider takes the decrypted API key.
func NewAnthropicProvider(a *ent.UpstreamAccount, apiKey string) Provider {
	return NewAnthropicProviderWithClient(a, apiKey, &http.Client{Transport: newProviderTransport()})
}

// NewAnthropicProviderWithClient is NewAnthropicProvider over a caller-supplied
// client (shared connection pool per account, per-account credential).
func NewAnthropicProviderWithClient(a *ent.UpstreamAccount, apiKey string, client *http.Client) Provider {
	return &anthropicProvider{
		id:           a.ID,
		name:         a.Name,
		baseURL:      strings.TrimRight(a.BaseURL, "/"),
		apiKey:       apiKey,
		headerRules:  a.HeaderRules,
		hc:           client,
		azureFoundry: a.DefinitionID == biz.AzureFoundryClaudeDefinitionID,
	}
}

func (a *anthropicProvider) ID() uuid.UUID { return a.id }
func (a *anthropicProvider) Name() string  { return a.name }
func (a *anthropicProvider) Kind() string  { return "anthropic" }

// applyAuth writes the account's API key as X-API-Key. The caller's own
// credential is removed first so a JuCode key can never reach the upstream.
func (a *anthropicProvider) applyAuth(header http.Header) {
	header.Del("X-API-Key")
	header.Del("Authorization")
	header.Set("X-API-Key", a.apiKey)
}

// ApplyAnthropicClientHeaders copies the Anthropic headers that change
// upstream scheduling and prompt caching. Credentials are never copied.
func ApplyAnthropicClientHeaders(dst, src http.Header) {
	if dst == nil || src == nil {
		return
	}
	if v := strings.TrimSpace(src.Get("Anthropic-Version")); v != "" {
		dst.Set("Anthropic-Version", v)
	}
	if beta := joinHeaderList(src.Values("Anthropic-Beta")); beta != "" {
		if existing := joinHeaderList(dst.Values("Anthropic-Beta")); existing != "" {
			beta = joinHeaderList([]string{existing, beta})
		}
		dst.Set("Anthropic-Beta", beta)
	}
}

func joinHeaderList(vals []string) string {
	seen := make(map[string]struct{}, len(vals))
	parts := make([]string, 0, len(vals))
	for _, v := range vals {
		for _, part := range strings.Split(v, ",") {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			if _, ok := seen[part]; ok {
				continue
			}
			seen[part] = struct{}{}
			parts = append(parts, part)
		}
	}
	return strings.Join(parts, ",")
}

func (a *anthropicProvider) Forward(ctx context.Context, path string, body []byte, stream bool, clientHeaders http.Header) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	ApplyAnthropicClientHeaders(req.Header, clientHeaders)
	a.applyAuth(req.Header)
	if strings.TrimSpace(req.Header.Get("Anthropic-Version")) == "" {
		req.Header.Set("Anthropic-Version", "2023-06-01")
	}
	if stream {
		req.Header.Set("Accept", "text/event-stream")
	} else if strings.TrimSpace(req.Header.Get("Accept")) == "" {
		req.Header.Set("Accept", "application/json")
	}
	a.applyAzureFoundryBeta(req.Header, body)
	biz.ApplyHeaderRules(req.Header, a.headerRules, a.apiKey)
	return a.hc.Do(req)
}

func (a *anthropicProvider) applyAzureFoundryBeta(header http.Header, body []byte) {
	if !a.azureFoundry || !wantsThinkingDisplayUpdates(body) {
		return
	}
	header.Set("Anthropic-Beta", joinHeaderList(append(header.Values("Anthropic-Beta"), thinkingDisplayUpdatesBeta)))
}
