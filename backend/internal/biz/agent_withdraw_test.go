package biz

import (
	"context"
	"errors"
	"testing"

	"github.com/shopspring/decimal"

	entagententry "github.com/jucode/jucode/internal/data/ent/agentsettlemententry"
	entbalancetx "github.com/jucode/jucode/internal/data/ent/balancetx"
	entsiteorder "github.com/jucode/jucode/internal/data/ent/siteorder"
)

func TestWithdrawCommissionToBalance(t *testing.T) {
	finance, agents, _, _, client := couponTestEnv(t)
	ctx := context.Background()
	agent, code := newCouponAgent(t, agents, client, 10)
	buyer := newInviteTestUser(t, client)

	order, err := finance.CreateOrder(ctx, CreateOrderInput{
		UserID: buyer.ID, Kind: string(entsiteorder.KindBalance),
		Amount: decimal.NewFromInt(100), Currency: CurrencyCNY, CouponCode: code,
	})
	if err != nil {
		t.Fatalf("create order: %v", err)
	}
	if _, err := finance.MarkOrderPaid(ctx, order.ID); err != nil {
		t.Fatalf("pay: %v", err)
	}
	// Rebate 18 owed; a card sale of 10 at share 70 owes the platform 7.
	if _, err := client.AgentSettlementEntry.Create().
		SetAgentID(agent.ID).SetKind(entagententry.KindSale).
		SetSaleAmount(decimal.NewFromInt(10)).SetPlatformPayable(decimal.NewFromInt(7)).
		SetSharePctAtEvent(decimal.NewFromInt(70)).SetSourceID(buyer.ID).
		Save(ctx); err != nil {
		t.Fatalf("sale entry: %v", err)
	}
	sum, err := agents.PendingSummary(ctx, agent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !sum.Withdrawable().Equal(decimal.NewFromInt(11)) {
		t.Fatalf("withdrawable = %s, want 11", sum.Withdrawable())
	}

	for _, bad := range []string{"0", "-1", "0.001"} {
		if _, err := agents.WithdrawToBalance(ctx, agent.ID, decimal.RequireFromString(bad)); !errors.Is(err, ErrWithdrawAmountInvalid) {
			t.Errorf("withdraw %s error = %v, want ErrWithdrawAmountInvalid", bad, err)
		}
	}
	if _, err := agents.WithdrawToBalance(ctx, agent.ID, decimal.RequireFromString("11.01")); !errors.Is(err, ErrWithdrawExceeds) {
		t.Fatalf("over-withdraw error = %v, want ErrWithdrawExceeds", err)
	}

	btx, err := agents.WithdrawToBalance(ctx, agent.ID, decimal.NewFromInt(8))
	if err != nil {
		t.Fatalf("withdraw: %v", err)
	}
	if btx.Kind != entbalancetx.KindAgentCommission || !btx.BalanceAfter.Equal(agent.Balance.Add(decimal.NewFromInt(8))) {
		t.Fatalf("balance tx kind/after = %s/%s", btx.Kind, btx.BalanceAfter)
	}
	sum, err = agents.PendingSummary(ctx, agent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !sum.Withdrawable().Equal(decimal.NewFromInt(3)) || !sum.Withdrawn.Equal(decimal.NewFromInt(8)) {
		t.Fatalf("after withdraw: withdrawable/withdrawn = %s/%s, want 3/8", sum.Withdrawable(), sum.Withdrawn)
	}
	if _, err := agents.WithdrawToBalance(ctx, agent.ID, decimal.NewFromInt(4)); !errors.Is(err, ErrWithdrawExceeds) {
		t.Fatalf("second over-withdraw error = %v, want ErrWithdrawExceeds", err)
	}

	// Settling nets the withdrawal: the platform owes only the remaining 3.
	st, _, err := agents.Settle(ctx, agent.ID, agent.ID, "")
	if err != nil {
		t.Fatalf("settle: %v", err)
	}
	if !st.TotalPayable.Equal(decimal.NewFromInt(-3)) {
		t.Fatalf("settlement total = %s, want -3", st.TotalPayable)
	}
	if _, err := agents.WithdrawToBalance(ctx, agent.ID, decimal.NewFromInt(1)); !errors.Is(err, ErrWithdrawExceeds) {
		t.Fatalf("withdraw after settle error = %v, want ErrWithdrawExceeds", err)
	}
}
