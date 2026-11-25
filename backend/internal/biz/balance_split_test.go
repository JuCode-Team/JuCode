package biz

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/jucode/jucode/internal/data/ent"
	entbalancetx "github.com/jucode/jucode/internal/data/ent/balancetx"
)

func TestBalanceSplitDrainsGiftThenManualThenPaid(t *testing.T) {
	finance, _, _, _, client := couponTestEnv(t)
	ctx := context.Background()
	u := newInviteTestUser(t, client)
	start := time.Now().Add(-time.Hour)
	balance := decimal.Zero
	want := func(step, refundable string) {
		t.Helper()
		got, err := finance.BalanceSplitFor(ctx, &ent.User{ID: u.ID, Balance: balance})
		if err != nil {
			t.Fatalf("%s: %v", step, err)
		}
		if w := decimal.RequireFromString(refundable); !got.Refundable.Equal(w) {
			t.Errorf("%s: refundable = %s, want %s", step, got.Refundable, w)
		}
	}
	n := 0
	tx := func(kind entbalancetx.Kind, amount, note string) {
		t.Helper()
		n++
		balance = balance.Add(decimal.RequireFromString(amount))
		if _, err := client.BalanceTx.Create().
			SetUserID(u.ID).SetKind(kind).SetAmount(decimal.RequireFromString(amount)).
			SetBalanceAfter(balance).SetNote(note).SetCreatedAt(start.Add(time.Duration(n) * time.Minute)).
			Save(ctx); err != nil {
			t.Fatalf("insert tx: %v", err)
		}
	}
	tx(entbalancetx.KindDeduct, "-1", "")
	tx(entbalancetx.KindTopup, "100", "site order: SO1")
	want("paid only", "99")
	tx(entbalancetx.KindAdjust, "30", "")
	tx(entbalancetx.KindInviteReward, "10", "")
	want("one of each", "99")
	tx(entbalancetx.KindDeduct, "-4", "")
	want("usage drains the gift first", "99")
	tx(entbalancetx.KindDeduct, "-20", "")
	want("then manual credits", "99")
	tx(entbalancetx.KindDeduct, "-20", "")
	want("then paid money", "95")
	tx(entbalancetx.KindInviteRebate, "5", "")
	tx(entbalancetx.KindDeduct, "-50", siteOrderRefundNote+"SO1")
	want("a cash refund only takes paid money", "45")
	balance = decimal.RequireFromString("170")
	want("balance with no ledger source is not paid money", "45")
	balance = decimal.RequireFromString("3")
	want("capped at the balance, gifts first", "0")
}

func TestAgentCouponsPaused(t *testing.T) {
	_, agents, _, settings, client := couponTestEnv(t)
	ctx := context.Background()
	_, code := newCouponAgent(t, agents, client, 5)
	buyer := newInviteTestUser(t, client)
	st, err := settings.GetSite(ctx)
	if err != nil {
		t.Fatal(err)
	}
	st.AgentCouponsEnabled = false
	settings.storeSite(st)
	if _, err := agents.QuoteCoupon(ctx, buyer.ID, code); !errors.Is(err, ErrCouponsPaused) {
		t.Fatalf("err = %v, want ErrCouponsPaused", err)
	}
}
