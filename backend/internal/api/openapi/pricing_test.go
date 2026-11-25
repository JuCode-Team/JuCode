package openapi

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/shopspring/decimal"

	"github.com/jucode/jucode/internal/biz"
	"github.com/jucode/jucode/internal/data/ent"
	entmodel "github.com/jucode/jucode/internal/data/ent/model"
)

func dec(t *testing.T, s string) *decimal.Decimal {
	t.Helper()
	d, err := decimal.NewFromString(s)
	if err != nil {
		t.Fatalf("bad decimal %q: %v", s, err)
	}
	return &d
}

// The ratio conversion is the load-bearing claim of the whole compat layer:
// a client renders model_ratio × 2 and expects a per-1M-token price. Pin the
// arithmetic so a refactor cannot quietly change what every price aggregator
// displays.
func TestPricingRatiosMatchAbsolutePrices(t *testing.T) {
	m := &ent.Model{
		Name:                 "gpt-5.4",
		BillingMode:          entmodel.BillingModePerToken,
		InputUnitPrice:       dec(t, "0.004"), // 元 / 1K tokens → 4 元 / 1M
		OutputUnitPrice:      dec(t, "0.032"),
		CacheReadInputPrice:  dec(t, "0.0004"),
		CacheWriteInputPrice: dec(t, "0.005"),
	}
	e := buildPricingEntry(m, nil, nil, biz.ModelCapabilities{})

	// 0.004 元/1K = 4 元/1M; new-api renders ratio×2, so ratio must be 2.
	if got, want := e.ModelRatio, 2.0; got != want {
		t.Errorf("model_ratio = %v, want %v (0.004 元/1K → 4 元/1M → ratio 2)", got, want)
	}
	if got, want := e.CompletionRatio, 8.0; got != want {
		t.Errorf("completion_ratio = %v, want %v (0.032/0.004)", got, want)
	}
	if e.CacheRatio == nil || *e.CacheRatio != 0.1 {
		t.Errorf("cache_ratio = %v, want 0.1 (0.0004/0.004)", e.CacheRatio)
	}
	if e.CreateCacheRatio == nil || *e.CreateCacheRatio != 1.25 {
		t.Errorf("create_cache_ratio = %v, want 1.25 (0.005/0.004)", e.CreateCacheRatio)
	}
	// The absolute prices must survive alongside the lossy ratios.
	if got := e.JuCodePricing["input_unit_price"]; got != "0.004" {
		t.Errorf("jucode_pricing.input_unit_price = %q, want %q", got, "0.004")
	}
	if got := e.JuCodePricing["output_unit_price"]; got != "0.032" {
		t.Errorf("jucode_pricing.output_unit_price = %q, want %q", got, "0.032")
	}
}

// A model priced per request has no input price to express ratios against.
// Upstream's convention is quota_type 1 + model_price, with the ratio fields
// left at zero — not omitted, since clients read them unconditionally.
func TestPricingPerRequestModel(t *testing.T) {
	m := &ent.Model{
		Name:            "sora-video",
		BillingMode:     entmodel.BillingModePerRequest,
		PricePerRequest: dec(t, "1.5"),
	}
	e := buildPricingEntry(m, nil, nil, biz.ModelCapabilities{})

	if e.QuotaType != 1 {
		t.Errorf("quota_type = %d, want 1 for per_request billing", e.QuotaType)
	}
	if e.ModelPrice != 1.5 {
		t.Errorf("model_price = %v, want 1.5", e.ModelPrice)
	}
	if e.ModelRatio != 0 || e.CompletionRatio != 0 {
		t.Errorf("ratios should stay zero without an input price, got %v/%v", e.ModelRatio, e.CompletionRatio)
	}
	// Division by a zero base must not produce a ratio at all.
	if e.CacheRatio != nil {
		t.Errorf("cache_ratio = %v, want nil when there is no input price to divide by", *e.CacheRatio)
	}

	raw, err := json.Marshal(e)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	// Fields upstream always emits must be present even at zero, or a client
	// doing data[i].completion_ratio gets undefined instead of 0.
	for _, k := range []string{"model_name", "quota_type", "model_ratio", "model_price", "owner_by", "completion_ratio", "enable_groups", "supported_endpoint_types"} {
		if _, ok := got[k]; !ok {
			t.Errorf("field %q missing from response; upstream emits it unconditionally", k)
		}
	}
	// ...and the ones that are omitempty upstream must stay absent.
	if _, ok := got["cache_ratio"]; ok {
		t.Error("cache_ratio should be omitted when unset, not null")
	}
}

// enable_groups must list only enabled groups: a retired group is hidden from
// every other user-facing surface, and leaking it here would advertise a
// route that no longer exists.
func TestPricingEnableGroupsSkipsDisabled(t *testing.T) {
	m := &ent.Model{Name: "m", BillingMode: entmodel.BillingModePerToken}
	e := buildPricingEntry(m, nil, []*ent.ModelGroup{
		{Name: "zeta", Enabled: true},
		{Name: "retired", Enabled: false},
		{Name: "alpha", Enabled: true},
	}, biz.ModelCapabilities{})
	if len(e.EnableGroup) != 2 || e.EnableGroup[0] != "alpha" || e.EnableGroup[1] != "zeta" {
		t.Errorf("enable_groups = %v, want sorted [alpha zeta]", e.EnableGroup)
	}
}

func TestPricingEnableGroupsSkipsExclusive(t *testing.T) {
	m := &ent.Model{Name: "m", BillingMode: entmodel.BillingModePerToken}
	e := buildPricingEntry(m, nil, []*ent.ModelGroup{
		{Name: "public", Enabled: true},
		{Name: "Grok (代理专属)", Enabled: true, Exclusive: true},
	}, biz.ModelCapabilities{})
	if len(e.EnableGroup) != 1 || e.EnableGroup[0] != "public" {
		t.Errorf("enable_groups = %v, want [public] without exclusive siblings", e.EnableGroup)
	}
}

// The endpoint-type heuristic reads prices because there is no endpoint
// column. Pin each branch so adding a price field later cannot silently
// reclassify a text model as a video one.
func TestSupportedEndpointTypes(t *testing.T) {
	tests := []struct {
		name  string
		model *ent.Model
		want  string
	}{
		{"video", &ent.Model{OutputSecondPrice: dec(t, "0.1")}, "openai-video"},
		{"image", &ent.Model{OutputImagePrice: dec(t, "0.2")}, "image-generation"},
		{"tts", &ent.Model{InputCharacterPrice: dec(t, "0.001")}, "audio-speech"},
		{"text", &ent.Model{InputUnitPrice: dec(t, "0.004")}, "openai"},
		// A multimodal chat model prices the image tokens its built-in
		// image_gen tool emits. That must not make it an image endpoint —
		// this is the regression that mislabelled the whole gpt-5.x family.
		{"multimodal chat", &ent.Model{
			InputUnitPrice:       dec(t, "0.005"),
			OutputUnitPrice:      dec(t, "0.03"),
			ImageOutputUnitPrice: dec(t, "0.03"),
		}, "openai"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := supportedEndpointTypes(tc.model)
			if len(got) == 0 || got[0] != tc.want {
				t.Errorf("supportedEndpointTypes = %v, want first entry %q", got, tc.want)
			}
		})
	}
}

// Model metadata must reach /api/pricing, since that is the whole reason the
// endpoint carries fields new-api does not have — and unconfigured values
// must be omitted, not defaulted.
func TestPricingCarriesModelMetadata(t *testing.T) {
	m := &ent.Model{Name: "claude-opus-4-5", BillingMode: entmodel.BillingModePerToken}
	e := buildPricingEntry(m, nil, nil, biz.ModelCapabilities{
		ContextWindow: 200_000, MaxContextWindow: 1_000_000, MaxOutputTokens: 64_000,
		ReasoningEfforts: []string{"low", "high"},
	})
	if e.ContextWindow != 200_000 || e.MaxContextWindow != 1_000_000 || e.MaxOutputTokens != 64_000 || len(e.ReasoningEfforts) != 2 {
		t.Fatalf("entry = %+v", e)
	}
	raw, err := json.Marshal(buildPricingEntry(m, nil, nil, biz.ModelCapabilities{}))
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"context_window", "max_context_window", "max_output_tokens", "reasoning_efforts"} {
		if strings.Contains(string(raw), `"`+field+`"`) {
			t.Errorf("unconfigured %s must be omitted: %s", field, raw)
		}
	}
}

// Mount* must not panic on gin's route tree. The groups overlap (/api twice,
// /dashboard at root and under /v1), which is exactly the shape that trips
// gin's wildcard conflict detection.
func TestMountRoutesDoNotConflict(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	h := New(nil, nil, nil, nil, nil, nil, nil)

	h.MountPricing(r.Group("/api"))
	h.MountTokenUsage(r.Group("/api"))
	h.MountBilling(r.Group(""))
	h.MountBilling(r.Group("/v1"))
	h.MountOpen(r.Group("/v1/open"))

	want := map[string]bool{
		"GET /api/pricing":                       false,
		"GET /api/usage/token/":                  false,
		"GET /dashboard/billing/subscription":    false,
		"GET /dashboard/billing/usage":           false,
		"GET /v1/dashboard/billing/subscription": false,
		"GET /v1/dashboard/billing/usage":        false,
		"GET /v1/open/balance":                   false,
		"GET /v1/open/key":                       false,
		"GET /v1/open/groups":                    false,
		"GET /v1/open/usage":                     false,
		"GET /v1/open/usage/models":              false,
		"GET /v1/open/logs":                      false,
	}
	for _, ri := range r.Routes() {
		key := ri.Method + " " + ri.Path
		if _, ok := want[key]; ok {
			want[key] = true
		}
	}
	for route, found := range want {
		if !found {
			t.Errorf("route not registered: %s", route)
		}
	}
}
