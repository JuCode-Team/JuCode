package biz

import "testing"

func TestMarketingBannerDefaultsDisabledAndEmpty(t *testing.T) {
	got := siteDefaults.MarketingBanner
	if got.Enabled {
		t.Fatal("marketing banner is enabled by default")
	}
	if got.Text != "" || got.LinkURL != "" {
		t.Fatalf("marketing banner defaults are not empty: %+v", got)
	}
}
