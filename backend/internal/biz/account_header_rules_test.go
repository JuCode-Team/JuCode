package biz

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/jucode/jucode/internal/data/ent/schema"
)

func TestNormalizeHeaderRules(t *testing.T) {
	got, err := NormalizeHeaderRules(&schema.HeaderRules{
		Set:        map[string]string{"api-key": " {api_key} "},
		Remove:     []string{"x-api-key", " "},
		DropValues: map[string][]string{"anthropic-beta": {"advisor-tool-*", "", "*"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Set["Api-Key"] != "{api_key}" {
		t.Fatalf("set = %v, want canonical name and trimmed value", got.Set)
	}
	if len(got.Remove) != 1 || got.Remove[0] != "X-Api-Key" {
		t.Fatalf("remove = %v", got.Remove)
	}
	if v := got.DropValues["Anthropic-Beta"]; len(v) != 1 || v[0] != "advisor-tool-*" {
		t.Fatalf("drop_values = %v, want blank and bare * entries dropped", got.DropValues)
	}

	for name, in := range map[string]schema.HeaderRules{
		"host":        {Set: map[string]string{"Host": "evil"}},
		"framing":     {Remove: []string{"content-length"}},
		"bad name":    {Set: map[string]string{"a b": "x"}},
		"line break":  {Set: map[string]string{"X-A": "x\r\nX-B: y"}},
		"list in one": {DropValues: map[string][]string{"Anthropic-Beta": {"a,b"}}},
	} {
		if _, err := NormalizeHeaderRules(&in); !errors.Is(err, ErrAccountBadInput) {
			t.Errorf("%s: err = %v, want ErrAccountBadInput", name, err)
		}
	}

	if got, err := NormalizeHeaderRules(&schema.HeaderRules{Remove: []string{""}}); err != nil || got != nil {
		t.Fatalf("empty rules = %v, %v; want nil so the column is cleared", got, err)
	}
}

func TestApplyHeaderRules(t *testing.T) {
	h := http.Header{}
	h.Set("X-Api-Key", "sk-default")
	h.Add("Anthropic-Beta", "claude-code-20250219, advisor-tool-2026-03-01")
	h.Add("Anthropic-Beta", "interleaved-thinking-2025-05-14,advisor-tool-2027-01-01")
	h.Set("Openai-Beta", "advisor-tool-2026-03-01")

	ApplyHeaderRules(h, schema.HeaderRules{
		Set:    map[string]string{"Api-Key": "{api_key}"},
		Remove: []string{"X-Api-Key"},
		DropValues: map[string][]string{
			"Anthropic-Beta": {"advisor-tool-*"},
			"Openai-Beta":    {"advisor-tool-2026-03-01"},
		},
	}, "sk-account")

	if got := h.Get("Api-Key"); got != "sk-account" {
		t.Fatalf("api-key = %q, want the account credential", got)
	}
	if got := h.Get("X-Api-Key"); got != "" {
		t.Fatalf("x-api-key = %q, want removed", got)
	}
	if got := h.Values("Anthropic-Beta"); len(got) != 1 || got[0] != "claude-code-20250219,interleaved-thinking-2025-05-14" {
		t.Fatalf("anthropic-beta = %q, want advisor flags dropped across every line", got)
	}
	if _, ok := h["Openai-Beta"]; ok {
		t.Fatalf("a header left with no value must be removed, got %q", h.Values("Openai-Beta"))
	}
}

// The shipped Azure definition must survive the same validation an operator's
// edit goes through (SeedBuiltins runs it), including the ModelsPathNone marker
// and its header rules, and must ship a model list for sync to bind.
func TestBuiltinAzureFoundryDefinitionIsUsable(t *testing.T) {
	def, ok := BuiltinDefinitionByID("azure-foundry-claude")
	if !ok {
		t.Fatal("azure-foundry-claude builtin definition missing")
	}
	view := viewFromBuiltin(def)
	if view.PayloadError != "" || len(view.Models) == 0 || view.ModelsPath != ModelsPathNone {
		t.Fatalf("view = %+v", view)
	}
	validated, err := (&ProviderDefinitionSvc{}).validate(context.Background(), UpsertProviderDefinitionInput{
		DefinitionID:    def.DefinitionID,
		BaseURL:         def.BaseURL,
		NativeProtocols: def.NativeProtocols,
		DefaultProtocol: def.DefaultProtocol,
		HeaderRules:     &def.HeaderRules,
		ModelsJSON:      def.ModelsJSON,
		ModelsPath:      def.ModelsPath,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := validated.HeaderRules.DropValues["Anthropic-Beta"]; len(got) != 1 || got[0] != "advisor-tool-*" {
		t.Fatalf("header rules = %+v", validated.HeaderRules)
	}
}
