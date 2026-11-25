package biz

import (
	"context"
	"testing"

	"github.com/shopspring/decimal"

	"github.com/jucode/jucode/internal/data"
	entredeemrec "github.com/jucode/jucode/internal/data/ent/redeemrecord"
)

// A refund reverses at the share the code was sold at, even after the site
// cap is lowered and the agent's effective share rises.
func TestAgentRefundUsesShareAtSale(t *testing.T) {
	_, settings, client := testInviteEnv(t)
	ctx := context.Background()
	setMaxIncentive(t, settings, 30)
	d := data.NewWith(client, nil, nil)
	agents := NewAgentSvc(d, settings)
	redeem := NewRedeemSvc(d, nil, settings)
	redeem.SetAgentSvc(agents)
	agent := newInviteTestUser(t, client)
	buyer := newInviteTestUser(t, client)

	amount := decimal.NewFromInt(100)
	codes, err := redeem.GenerateBatch(ctx, GenerateInput{
		Kind: "balance", Amount: &amount, Count: 1, MaxUses: 1,
		CreatedBy: agent.ID, AgentID: &agent.ID,
	})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if _, err := redeem.Redeem(ctx, buyer.ID, codes[0].Code, "203.0.113.7"); err != nil {
		t.Fatalf("redeem: %v", err)
	}
	rec, err := client.RedeemRecord.Query().Where(entredeemrec.UserIDEQ(buyer.ID)).Only(ctx)
	if err != nil {
		t.Fatalf("load record: %v", err)
	}

	setMaxIncentive(t, settings, 20)
	res, err := agents.RegisterRefund(ctx, agent.ID, rec.ID, "test")
	if err != nil {
		t.Fatalf("refund: %v", err)
	}
	// 100 × 0.9 retention = 90 refunded; the sale was booked at 70%.
	if !res.PlatformDeduct.Equal(decimal.NewFromInt(-63)) {
		t.Fatalf("platform deduct = %s, want -63", res.PlatformDeduct)
	}
}

// The refund rate comes from site settings.
func TestAgentRefundUsesConfiguredRate(t *testing.T) {
	_, settings, client := testInviteEnv(t)
	ctx := context.Background()
	setMaxIncentive(t, settings, 30)
	st, err := settings.GetSite(ctx)
	if err != nil {
		t.Fatal(err)
	}
	st.AgentBalanceRefundRatePct = 50
	settings.storeSite(st)
	d := data.NewWith(client, nil, nil)
	agents := NewAgentSvc(d, settings)
	redeem := NewRedeemSvc(d, nil, settings)
	redeem.SetAgentSvc(agents)
	agent := newInviteTestUser(t, client)
	buyer := newInviteTestUser(t, client)

	amount := decimal.NewFromInt(100)
	codes, err := redeem.GenerateBatch(ctx, GenerateInput{
		Kind: "balance", Amount: &amount, Count: 1, MaxUses: 1,
		CreatedBy: agent.ID, AgentID: &agent.ID,
	})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if _, err := redeem.Redeem(ctx, buyer.ID, codes[0].Code, "203.0.113.7"); err != nil {
		t.Fatalf("redeem: %v", err)
	}
	rec, err := client.RedeemRecord.Query().Where(entredeemrec.UserIDEQ(buyer.ID)).Only(ctx)
	if err != nil {
		t.Fatalf("load record: %v", err)
	}
	res, err := agents.RegisterRefund(ctx, agent.ID, rec.ID, "test")
	if err != nil {
		t.Fatalf("refund: %v", err)
	}
	if !res.RefundAmount.Equal(decimal.NewFromInt(50)) || !res.PlatformDeduct.Equal(decimal.NewFromInt(-35)) {
		t.Fatalf("refund = %s, deduct = %s; want 50 and -35", res.RefundAmount, res.PlatformDeduct)
	}
}
