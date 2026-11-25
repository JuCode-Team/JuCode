package biz

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// Invalid contact input is rejected before any storage access, so a
// zero-value service is enough here.
func TestUpdateSiteRejectsInvalidContactFields(t *testing.T) {
	s := &SettingsSvc{}
	cases := map[string]func(*SiteSettings){
		"group too long":     func(in *SiteSettings) { in.ContactGroup = strings.Repeat("1", 65) },
		"group two lines":    func(in *SiteSettings) { in.ContactGroup = "123\n456" },
		"label too long":     func(in *SiteSettings) { in.ContactGroupLabel = strings.Repeat("群", 33) },
		"qr javascript url":  func(in *SiteSettings) { in.ContactGroupQRURL = "javascript:alert(1)" },
		"qr relative no dot": func(in *SiteSettings) { in.ContactGroupQRURL = "qr.png" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			in := siteDefaults
			mutate(&in)
			err := s.UpdateSite(context.Background(), in)
			if !errors.Is(err, ErrInvalidSiteSettings) || !strings.Contains(err.Error(), "contact_group") {
				t.Fatalf("err = %v, want ErrInvalidSiteSettings for a contact field", err)
			}
		})
	}
}

func TestContactSettingsDefaults(t *testing.T) {
	if siteDefaults.ContactDockEnabled || siteDefaults.ContactGroup != "" || siteDefaults.ContactGroupQRURL != "" || siteDefaults.ContactGroupLabel != "QQ 群" {
		t.Fatalf("contact defaults = %q %q %q %v, want empty, QQ 群 and disabled",
			siteDefaults.ContactGroup, siteDefaults.ContactGroupLabel, siteDefaults.ContactGroupQRURL, siteDefaults.ContactDockEnabled)
	}
}

func TestAgentRefundSettingsDefaultsAndValidation(t *testing.T) {
	if siteDefaults.AgentPlanRefundDayDivisor != 30 || siteDefaults.AgentPlanRefundRatePct != 70 || siteDefaults.AgentBalanceRefundRatePct != 90 {
		t.Fatalf("agent refund defaults = %d %v %v", siteDefaults.AgentPlanRefundDayDivisor,
			siteDefaults.AgentPlanRefundRatePct, siteDefaults.AgentBalanceRefundRatePct)
	}
	s := &SettingsSvc{}
	for field, mutate := range map[string]func(*SiteSettings){
		"agent_plan_refund_day_divisor": func(in *SiteSettings) { in.AgentPlanRefundDayDivisor = 0 },
		"agent_plan_refund_rate_pct":    func(in *SiteSettings) { in.AgentPlanRefundRatePct = 101 },
		"agent_balance_refund_rate_pct": func(in *SiteSettings) { in.AgentBalanceRefundRatePct = -1 },
	} {
		in := siteDefaults
		mutate(&in)
		err := s.UpdateSite(context.Background(), in)
		if !errors.Is(err, ErrInvalidSiteSettings) || !strings.Contains(err.Error(), field) {
			t.Errorf("%s: err = %v, want ErrInvalidSiteSettings naming it", field, err)
		}
	}
}

func TestTopupPresets(t *testing.T) {
	if got := siteDefaults.TopupPresets; len(got) != 5 || got[0] != 50 || got[4] != 1000 {
		t.Fatalf("default topup presets = %v", got)
	}
	if err := validTopupPresets([]int{10, 0}); !errors.Is(err, ErrInvalidSiteSettings) {
		t.Errorf("zero amount: err = %v", err)
	}
	if err := validTopupPresets(make([]int, maxTopupPresets+1)); !errors.Is(err, ErrInvalidSiteSettings) {
		t.Errorf("too many: err = %v", err)
	}
	if err := validTopupPresets([]int{}); err != nil {
		t.Errorf("empty list: err = %v", err)
	}
}
