package biz

import (
	"context"

	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jucode/jucode/internal/data/ent"
	"github.com/jucode/jucode/internal/data/ent/schema"
)

func TestFetchUpstreamModels(t *testing.T) {
	tests := []struct {
		name      string
		base      string // appended to server URL via modelsListURL-style path
		path      string // path the server should see
		body      string
		wantIDs   []string
		wantIn    string // ref input cost of first model ("" = none)
		wantProto string // pin of first model ("" = none)
		wantErr   bool
	}{
		{
			name:    "openai envelope",
			path:    "/v1/models",
			body:    `{"data":[{"id":"a"},{"id":"b"},{"id":"a"}]}`,
			wantIDs: []string{"a", "b"},
		},
		{
			name:    "cost fields captured",
			path:    "/v1/models",
			body:    `{"data":[{"id":"a","cost":{"input":3,"output":15}}]}`,
			wantIDs: []string{"a"},
			wantIn:  "3",
		},
		{
			name:    "gemini-style models array",
			path:    "/v1/models",
			body:    `{"models":[{"name":"m1"},{"id":"m2"}]}`,
			wantIDs: []string{"m1", "m2"},
		},
		{
			name:    "plain string array under models",
			path:    "/v1/models",
			body:    `{"models":["x","y"]}`,
			wantIDs: []string{"x", "y"},
		},
		{
			name:    "nested vendor shape (agents list)",
			path:    "/v2/models",
			body:    `{"code":0,"data":{"agents":[{"name":"cli","models":["auto","glm-5.3","glm-5.2"]},{"name":"compact"}]}}`,
			wantIDs: []string{"auto", "glm-5.3", "glm-5.2"},
		},
		{
			name:      "single-endpoint model gets a protocol pin",
			path:      "/v1/models",
			body:      `{"data":[{"id":"claude-sonnet-4-6","supported_endpoints":["/messages"]},{"id":"ds","supported_endpoints":["/responses","/chat/completions"]}]}`,
			wantIDs:   []string{"claude-sonnet-4-6", "ds"},
			wantProto: "messages",
		},
		{
			name:    "non-2xx is an error",
			path:    "/v1/models",
			body:    `{"error":"nope"}`,
			wantErr: true,
		},
		{
			name:    "empty list is an error",
			path:    "/v1/models",
			body:    `{"data":[]}`,
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var gotPath, gotAuth string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotPath, gotAuth = r.URL.Path, r.Header.Get("Authorization")
				if tt.name == "non-2xx is an error" {
					w.WriteHeader(http.StatusBadGateway)
				}
				w.Write([]byte(tt.body))
			}))
			defer srv.Close()
			models, err := fetchUpstreamModels(context.Background(), srv.Client(), srv.URL+tt.path, "tok", schema.HeaderRules{})
			if tt.wantErr {
				if err == nil {
					t.Fatalf("want error, got %v", models)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if gotPath != tt.path {
				t.Fatalf("path=%s want=%s", gotPath, tt.path)
			}
			if gotAuth != "Bearer tok" {
				t.Fatalf("auth=%q", gotAuth)
			}
			if len(models) != len(tt.wantIDs) {
				t.Fatalf("ids=%v want=%v", models, tt.wantIDs)
			}
			for i := range models {
				if models[i].ID != tt.wantIDs[i] {
					t.Fatalf("ids=%v want=%v", models, tt.wantIDs)
				}
			}
			if tt.wantIn != "" {
				if models[0].InCost == nil || models[0].InCost.String() != tt.wantIn {
					t.Fatalf("in cost=%v want=%s", models[0].InCost, tt.wantIn)
				}
			}
			if tt.wantProto != "" && models[0].Protocol != tt.wantProto {
				t.Fatalf("proto=%q want=%q", models[0].Protocol, tt.wantProto)
			}
		})
	}
}

func TestResolvePlatformName(t *testing.T) {
	idx := newPlatformModelIndex([]string{
		"deepseek", "deepseek-v4.1", "deepseek-v4.1-flash", "gpt-5.2",
		"claude-opus-4", "claude-sonnet-4-5", "openai/gpt-5.3",
		"MiniMax/MiniMax-M3", "glm-5.3",
	})
	tests := []struct {
		remote string
		want   string
		exact  bool
	}{
		{"deepseek", "deepseek", true},                                 // verbatim
		{"clinepass/deepseek-v4.1-flash", "deepseek-v4.1-flash", true}, // vendor prefix + suffix exact
		{"deepseek/deepseek-v4.1-flash", "deepseek-v4.1-flash", true},
		{"clinepass/deepseek-v4.2-flash", "deepseek", false},       // only "deepseek" prefixes it
		{"aws/claude-opus-4", "claude-opus-4", true},               // suffix exact match
		{"anthropic/claude-sonnet-4.5", "claude-sonnet-4-5", true}, // "." vs "-" normalized
		{"anthropic/claude-sonnet-4.5:batch", "claude-sonnet-4-5", false},
		{"z-ai/glm-5.3", "glm-5.3", true},
		{"minimax-m3", "MiniMax/MiniMax-M3", true}, // platform name's own vendor prefix
		{"gpt-5.2", "gpt-5.2", true},
		{"openai/gpt-5.3", "openai/gpt-5.3", true},           // platform name keeps its own slash
		{"clinepass/openai/gpt-5.3", "openai/gpt-5.3", true}, // two-level vendor prefix
		{"deepseek-chat", "deepseek", false},                 // separator prefix
		{"deepseeks", "", false},                             // no boundary, no claim
		{"clinepass/mystery-1", "", false},                   // unmatched
	}
	for _, tt := range tests {
		got, exact := idx.resolve(tt.remote)
		if got != tt.want || exact != tt.exact {
			t.Errorf("resolve(%q)=(%q,%v) want=(%q,%v)", tt.remote, got, exact, tt.want, tt.exact)
		}
	}
}

func TestModelsListURL(t *testing.T) {
	svc := &AccountPoolSvc{}
	acct := acctWith(func(a *ent.UpstreamAccount) {
		a.BaseURL = "https://api.commandcode.ai/provider"
	})
	if got := svc.modelsListURL(context.Background(), acct); got != "https://api.commandcode.ai/provider/v1/models" {
		t.Fatalf("url=%s", got)
	}
	acct.BaseURL = "https://x.test/v1"
	if got := svc.modelsListURL(context.Background(), acct); got != "https://x.test/v1/models" {
		t.Fatalf("url=%s", got)
	}
}

func TestParseRemoteCatalog(t *testing.T) {
	body := []byte(`{
		"deepseek": {"name": "DeepSeek", "models": {
			"deepseek-chat": {"id": "deepseek-chat", "name": "DeepSeek Chat", "cost": {"input": 0.28, "output": 0.42}, "limit": {"context": 64000}}
		}},
		"openai": {"name": "OpenAI", "models": {
			"gpt-4o": {"id": "gpt-4o", "name": "GPT-4o", "cost": {"input": 2.5, "output": 10, "cache_read": 1.25}, "limit": {"context": 128000}},
			"gpt-4o-mini": {"id": "gpt-4o-mini", "name": "GPT-4o Mini", "limit": {"context": 128000}}
		}}
	}`)
	providers, byID, err := parseRemoteCatalog(body)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(providers) != 2 || providers[0].ID != "deepseek" || providers[0].Models != 1 {
		t.Fatalf("providers=%+v", providers)
	}
	models := byID["openai"]
	if len(models) != 2 || models[0].ID != "gpt-4o" {
		t.Fatalf("models=%+v", models)
	}
	if models[0].Cost == nil || models[0].Cost.Input != 2.5 || models[0].Cost.CacheRead != 1.25 {
		t.Fatalf("cost=%+v", models[0].Cost)
	}
	if models[0].Context != 128000 || models[1].Cost != nil {
		t.Fatalf("model=%+v", models[0])
	}
}

func TestCanonicalModels(t *testing.T) {
	svc := NewRemoteCatalogSvc()
	svc.fetched = time.Now()
	svc.byID = map[string][]RemoteCatalogModel{
		"deepseek": {
			{ID: "deepseek-v4.1-flash", Name: "DeepSeek V4.1 Flash", Cost: &RemoteModelCost{Input: 0.3, Output: 0.6}},
		},
		"openrouter": {
			{ID: "deepseek/deepseek-v4.1-flash", Name: "DeepSeek V4.1 Flash (OR)", Cost: &RemoteModelCost{Input: 9, Output: 9}},
			{ID: "qwen/qwen3-max", Name: "Qwen3 Max"},
		},
		"vercel": {
			{ID: "voyage/voyage-code-3", Name: "Voyage Code 3"},
		},
	}
	models, err := svc.CanonicalModels(context.Background())
	if err != nil {
		t.Fatalf("canonical: %v", err)
	}
	if len(models) != 3 {
		t.Fatalf("want 3 canonical models, got %+v", models)
	}
	var ds *RemoteCatalogModel
	for i := range models {
		if models[i].ID == "deepseek-v4.1-flash" {
			ds = &models[i]
		}
	}
	if ds == nil {
		t.Fatal("deepseek-v4.1-flash missing")
	}
	// The original-vendor listing (bare id) wins the metadata even though the
	// aggregator copy sorts earlier alphabetically.
	if ds.Name != "DeepSeek V4.1 Flash" || ds.Cost == nil || ds.Cost.Input != 0.3 {
		t.Fatalf("metadata picked wrong listing: %+v", ds)
	}
	if len(ds.Providers) != 2 || ds.Providers[0] != "deepseek" || ds.Providers[1] != "openrouter" {
		t.Fatalf("providers=%+v", ds.Providers)
	}
	if models[0].ID != "deepseek-v4.1-flash" {
		t.Fatalf("order=%v", models)
	}
}
