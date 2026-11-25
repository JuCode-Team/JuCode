package biz

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	entsiteorder "github.com/jucode/jucode/internal/data/ent/siteorder"
)

// A verified notice for an order that is already credited changes nothing:
// a late closed-trade notice must not overwrite the payment it was credited
// with, and a repeated success is not an error.
func TestPaymentNoticeAfterCreditLeavesTheOrderAlone(t *testing.T) {
	finance, _, _, _, client := couponTestEnv(t)
	ctx := context.Background()
	u := newInviteTestUser(t, client)
	o, err := client.SiteOrder.Create().
		SetOrderNo("SO" + uuid.NewString()[:8]).SetUserID(u.ID).
		SetKind(entsiteorder.KindBalance).
		SetAmount(decimal.NewFromInt(10)).SetCreditAmount(decimal.NewFromInt(10)).
		SetPaymentChannel("alipay").
		Save(ctx)
	if err != nil {
		t.Fatal(err)
	}
	paid := PaymentCallbackResult{OrderNo: o.OrderNo, TradeState: "TRADE_SUCCESS", TransactionID: "T1", PayerOpenID: "buyer", AmountCents: 1000}
	got, err := finance.ApplyAlipayCallback(ctx, paid)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != entsiteorder.StatusPaid {
		t.Fatalf("status = %s, want paid", got.Status)
	}
	if _, err := finance.ApplyAlipayCallback(ctx, paid); err != nil {
		t.Fatalf("repeated success: %v", err)
	}
	late := PaymentCallbackResult{OrderNo: o.OrderNo, TradeState: "TRADE_CLOSED", TransactionID: "T2", PayerOpenID: "someone"}
	if _, err := finance.ApplyAlipayCallback(ctx, late); err != nil {
		t.Fatalf("late notice: %v", err)
	}
	got, _ = client.SiteOrder.Get(ctx, o.ID)
	if got.Status != entsiteorder.StatusPaid || got.PaymentTradeState != "TRADE_SUCCESS" ||
		got.PaymentTransactionID != "T1" || got.PaymentPayerOpenid != "buyer" {
		t.Errorf("order after late notice = %s %s %s %s", got.Status, got.PaymentTradeState, got.PaymentTransactionID, got.PaymentPayerOpenid)
	}
	bal, _ := client.User.Get(ctx, u.ID)
	if !bal.Balance.Equal(decimal.NewFromInt(10)) {
		t.Errorf("balance = %s, want 10 (credited once)", bal.Balance)
	}
}

// Paying an order after it was cancelled credits nothing, but the payment
// stays on record so staff can find and refund it.
func TestPaymentForCancelledOrderIsRecorded(t *testing.T) {
	finance, _, _, _, client := couponTestEnv(t)
	ctx := context.Background()
	u := newInviteTestUser(t, client)
	o, err := client.SiteOrder.Create().
		SetOrderNo("SO" + uuid.NewString()[:8]).SetUserID(u.ID).
		SetKind(entsiteorder.KindBalance).SetStatus(entsiteorder.StatusCanceled).
		SetAmount(decimal.NewFromInt(10)).SetCreditAmount(decimal.NewFromInt(10)).
		SetPaymentChannel("alipay").
		Save(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_, err = finance.ApplyAlipayCallback(ctx, PaymentCallbackResult{OrderNo: o.OrderNo, TradeState: "TRADE_SUCCESS", TransactionID: "T9", AmountCents: 1000})
	if !errors.Is(err, ErrOrderInvalidState) {
		t.Fatalf("err = %v, want ErrOrderInvalidState", err)
	}
	got, _ := client.SiteOrder.Get(ctx, o.ID)
	if got.Status != entsiteorder.StatusCanceled || got.PaymentTransactionID != "T9" {
		t.Errorf("order = %s %q, want canceled with the transaction recorded", got.Status, got.PaymentTransactionID)
	}
}
