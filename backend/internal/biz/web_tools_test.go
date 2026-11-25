package biz

import (
	"errors"
	"testing"

	"github.com/shopspring/decimal"
)

func TestNormalizeWebToolSettingsKeepsDefaultsAndRejectsBadInput(t *testing.T) {
	got, err := NormalizeWebToolSettings(WebToolSettings{
		Engines: map[string]WebEngineSetting{"search.brave": {Enabled: false, Price: decimal.RequireFromString("0.08")}},
		UserRPM: 30,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Engines["search.brave"].Enabled || !got.Engines["search.brave"].Price.Equal(decimal.RequireFromString("0.08")) {
		t.Fatalf("brave = %+v", got.Engines["search.brave"])
	}
	if !got.Engines["fetch.jina"].Enabled || !got.Engines["fetch.jina"].Price.Equal(decimal.RequireFromString("0.01")) {
		t.Fatalf("an engine left out must keep its default: %+v", got.Engines["fetch.jina"])
	}
	for name, in := range map[string]WebToolSettings{
		"unknown engine": {Engines: map[string]WebEngineSetting{"search.google": {}}, UserRPM: 10},
		"negative price": {Engines: map[string]WebEngineSetting{"fetch.jina": {Price: decimal.RequireFromString("-1")}}, UserRPM: 10},
		"zero rpm":       {UserRPM: 0},
	} {
		if _, err := NormalizeWebToolSettings(in); !errors.Is(err, ErrInvalidSiteSettings) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
}

func TestWebEngineByKindDefaultsAndVendors(t *testing.T) {
	if e, ok := WebEngineByKind(WebToolSearch, ""); !ok || e.ID != "search.auto" || len(e.Vendors) != 2 {
		t.Fatalf("default search = %+v", e)
	}
	if e, ok := WebEngineByKind(WebToolFetch, " Firecrawl "); !ok || e.ID != "fetch.firecrawl" {
		t.Fatalf("firecrawl = %+v", e)
	}
	if _, ok := WebEngineByKind(WebToolFetch, "brave"); ok {
		t.Fatal("brave does not fetch pages")
	}
}
