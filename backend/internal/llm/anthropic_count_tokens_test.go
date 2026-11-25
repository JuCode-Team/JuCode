package llm

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/jucode/jucode/internal/biz"
	"github.com/jucode/jucode/internal/data/ent"
	entaccount "github.com/jucode/jucode/internal/data/ent/upstreamaccount"
	cryptopkg "github.com/jucode/jucode/internal/pkg/crypto"
	"github.com/jucode/jucode/internal/providerkit"
)

func TestEstimateAnthropicInputTokens(t *testing.T) {
	// Plain string content: 40 ASCII chars ≈ 10 tokens + overheads.
	var req anthropicRequest
	body := `{"model":"claude-3","messages":[{"role":"user","content":"` +
		"aaaaaaaaaabbbbbbbbbbccccccccccdddddddddd" + `"}]}`
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatal(err)
	}
	got := estimateAnthropicInputTokens(&req)
	want := countTokensBaseOverhead + countTokensMessageOverhead + 10
	if got != want {
		t.Fatalf("plain string: got %d want %d", got, want)
	}

	// Block content with system, an image (flat cost), a tool declaration
	// and CJK text (1 token per rune, not per 4 bytes).
	body = `{
		"model":"claude-3",
		"system":"be helpful",
		"tools":[{"name":"get_weather","description":"look up weather","input_schema":{"type":"object"}}],
		"messages":[
			{"role":"user","content":[
				{"type":"text","text":"你好世界"},
				{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AAAA"}}
			]}
		]
	}`
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatal(err)
	}
	got = estimateAnthropicInputTokens(&req)
	if got < countTokensImageFlatCost+4 {
		t.Fatalf("blocks: got %d, want at least image cost %d + 4 CJK tokens", got, countTokensImageFlatCost)
	}

	if estimateTokensFromText("你好世界") != 4 {
		t.Fatalf("CJK runes should count 1 token each, got %d", estimateTokensFromText("你好世界"))
	}
	if estimateTokensFromText("abcd") != 1 {
		t.Fatalf("4 ASCII chars should count 1 token, got %d", estimateTokensFromText("abcd"))
	}
	if estimateTokensFromText("abcde") != 2 {
		t.Fatalf("5 ASCII chars should round up to 2 tokens, got %d", estimateTokensFromText("abcde"))
	}
}

// countTokensTestEnv wires a Gateway around a fake Anthropic upstream. The
// account carries the endpoint and the static API-key credential, so the
// upstream client is built by the same path production uses.
func countTokensTestEnv(t *testing.T, upstream *httptest.Server) (*Gateway, *biz.Route) {
	t.Helper()
	cipher, err := cryptopkg.New(strings.Repeat("ab", 32))
	if err != nil {
		t.Fatalf("cipher: %v", err)
	}
	plain, err := providerkit.NewAPIKeyCredential("sk-test").Encode()
	if err != nil {
		t.Fatalf("encode credential: %v", err)
	}
	enc, err := cipher.Encrypt(plain)
	if err != nil {
		t.Fatalf("encrypt credential: %v", err)
	}
	acct := &ent.UpstreamAccount{
		ID:              uuid.New(),
		Name:            "fake-anthropic",
		BaseURL:         upstream.URL,
		NativeProtocols: []string{biz.NativeProtocolMessages},
		Status:          entaccount.StatusActive,
		Schedulable:     true,
		CredentialEnc:   enc,
	}
	// buildProviderForAttempt resolves the credential through the account pool
	// and reuses the account's cached upstream client, so wire both the way the
	// gateway does: a pool that can decode the static API key, and the client
	// stashed in accountClients.
	g := &Gateway{
		maxAttempts:    3,
		accounts:       biz.NewAccountPoolSvc(nil, cipher, nil),
		accountClients: map[uuid.UUID]accountClientEntry{acct.ID: {updatedAt: acct.UpdatedAt, client: upstream.Client()}},
	}
	route := &biz.Route{
		Model:     &ent.Model{Name: "claude-3"},
		Group:     &ent.ModelGroup{},
		Providers: []biz.RouteAccount{{Account: acct, UpstreamModel: "claude-3-upstream"}},
	}
	return g, route
}

func countTokensTestContext(t *testing.T, raw []byte) *gin.Context {
	t.Helper()
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens", bytes.NewReader(raw))
	return c
}

func TestUpstreamCountTokensRelay(t *testing.T) {
	var gotPath, gotModel string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		gotModel, _ = body["model"].(string)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"input_tokens":42}`))
	}))
	defer upstream.Close()

	g, route := countTokensTestEnv(t, upstream)
	raw := []byte(`{"model":"claude-3","messages":[{"role":"user","content":"hi"}]}`)
	out, ok := g.upstreamCountTokens(countTokensTestContext(t, raw), route, raw)
	if !ok {
		t.Fatal("expected upstream count to succeed")
	}
	var parsed struct {
		InputTokens int `json:"input_tokens"`
	}
	if err := json.Unmarshal(out, &parsed); err != nil || parsed.InputTokens != 42 {
		t.Fatalf("relayed body = %s (err=%v)", out, err)
	}
	if gotPath != "/v1/messages/count_tokens" {
		t.Fatalf("upstream path = %q", gotPath)
	}
	if gotModel != "claude-3-upstream" {
		t.Fatalf("upstream model = %q, want per-provider rewrite", gotModel)
	}
}

func TestUpstreamCountTokensFallsBackOnBadUpstream(t *testing.T) {
	for name, handler := range map[string]http.HandlerFunc{
		"5xx": func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		},
		"2xx without count": func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"unexpected":"shape"}`))
		},
	} {
		t.Run(name, func(t *testing.T) {
			upstream := httptest.NewServer(handler)
			defer upstream.Close()
			g, route := countTokensTestEnv(t, upstream)
			raw := []byte(`{"model":"claude-3","messages":[{"role":"user","content":"hi"}]}`)
			if _, ok := g.upstreamCountTokens(countTokensTestContext(t, raw), route, raw); ok {
				t.Fatal("expected fallback (ok=false)")
			}
		})
	}
}
