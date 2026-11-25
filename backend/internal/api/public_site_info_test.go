package api

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/jucode/jucode/internal/biz"
)

func TestPublicSiteInfoHidesRefundAutoApproval(t *testing.T) {
	st := &biz.SiteSettings{SelfRefundEnabled: true, RefundAutoApproveEnabled: true, RefundAutoApproveMax: "50"}
	b, err := json.Marshal(publicSiteInfo{SiteSettings: st})
	if err != nil {
		t.Fatal(err)
	}
	out := string(b)
	if strings.Contains(out, "refund_auto_approve") {
		t.Errorf("public site info leaks auto-approval settings: %s", out)
	}
	if !strings.Contains(out, `"self_refund_enabled":true`) {
		t.Errorf("public site info lacks self_refund_enabled: %s", out)
	}
}
