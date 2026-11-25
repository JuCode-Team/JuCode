package biz

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jucode/jucode/internal/data/ent"
	entsiterefund "github.com/jucode/jucode/internal/data/ent/siterefund"
	entticket "github.com/jucode/jucode/internal/data/ent/ticket"
	entticketmsg "github.com/jucode/jucode/internal/data/ent/ticketmessage"
)

func (f *refundFixture) autoApprove(enabled bool, max string) {
	f.t.Helper()
	st, err := f.finance.settings.GetSite(context.Background())
	if err != nil {
		f.t.Fatal(err)
	}
	st.RefundAutoApproveEnabled, st.RefundAutoApproveMax = enabled, max
	f.finance.settings.storeSite(st)
}

func (f *refundFixture) refundTicket(o *ent.SiteOrder) *ent.Ticket {
	f.t.Helper()
	t, err := f.client.Ticket.Create().
		SetUserID(f.user.ID).SetOrderID(o.ID).SetTitle("申请退款").
		SetCategory(entticket.CategoryRefund).SetLastReplyAt(time.Now()).
		Save(context.Background())
	if err != nil {
		f.t.Fatal(err)
	}
	return t
}

func (f *refundFixture) systemMessages(t *ent.Ticket) []string {
	f.t.Helper()
	msgs, err := f.client.TicketMessage.Query().
		Where(entticketmsg.TicketIDEQ(t.ID), entticketmsg.SenderRoleAtSendEQ(entticketmsg.SenderRoleAtSendSystem)).
		All(context.Background())
	if err != nil {
		f.t.Fatal(err)
	}
	out := make([]string, len(msgs))
	for i, m := range msgs {
		out[i] = m.Body
	}
	return out
}

func TestAutoApproveTicketRefundGoesBackThroughTheChannel(t *testing.T) {
	f := newRefundFixture(t)
	ctx := context.Background()
	o := f.order("alipay", "30", 3)
	gw := &fakeRefunds{outcome: RefundOutcome{Refunded: true}}
	f.finance.SetRefundGateway("alipay", gw)
	f.autoApprove(true, "")
	tk := f.refundTicket(o)

	f.finance.AutoApproveTicketRefund(ctx, tk, "不用了")
	if len(gw.calls) != 1 || gw.calls[0].AmountCents != 3000 {
		t.Fatalf("calls = %+v", gw.calls)
	}
	eq(t, "balance", f.reload(), "0")
	got, _ := f.client.Ticket.Get(ctx, tk.ID)
	if got.Status != entticket.StatusResolved {
		t.Errorf("ticket status = %s, want resolved", got.Status)
	}
	if msgs := f.systemMessages(tk); len(msgs) != 1 || !strings.Contains(msgs[0], "已原路退回") {
		t.Errorf("system messages = %q", msgs)
	}
}

func TestAutoApproveTicketRefundLeavesTheRestToStaff(t *testing.T) {
	ctx := context.Background()

	t.Run("disabled", func(t *testing.T) {
		f := newRefundFixture(t)
		o := f.order("alipay", "30", 3)
		gw := &fakeRefunds{outcome: RefundOutcome{Refunded: true}}
		f.finance.SetRefundGateway("alipay", gw)
		f.finance.AutoApproveTicketRefund(ctx, f.refundTicket(o), "")
		if n, _ := f.client.SiteRefund.Query().Where(entsiterefund.OrderIDEQ(o.ID)).Count(ctx); n != 0 || len(gw.calls) != 0 {
			t.Errorf("refunds = %d, calls = %d; want none", n, len(gw.calls))
		}
	})

	t.Run("channel without a gateway", func(t *testing.T) {
		f := newRefundFixture(t)
		o := f.order("manual", "30", 3)
		f.finance.SetRefundGateway("alipay", &fakeRefunds{outcome: RefundOutcome{Refunded: true}})
		f.autoApprove(true, "")
		tk := f.refundTicket(o)
		f.finance.AutoApproveTicketRefund(ctx, tk, "")
		r, err := f.client.SiteRefund.Query().Where(entsiterefund.OrderIDEQ(o.ID)).Only(ctx)
		if err != nil || r.Status != entsiterefund.StatusPending {
			t.Fatalf("refund = %+v, %v; want one pending for staff", r, err)
		}
		if msgs := f.systemMessages(tk); len(msgs) != 1 || !strings.Contains(msgs[0], "不支持自动退款") {
			t.Errorf("system messages = %q", msgs)
		}
	})

	t.Run("over the cap", func(t *testing.T) {
		f := newRefundFixture(t)
		o := f.order("alipay", "30", 3)
		gw := &fakeRefunds{outcome: RefundOutcome{Refunded: true}}
		f.finance.SetRefundGateway("alipay", gw)
		f.autoApprove(true, "20")
		tk := f.refundTicket(o)
		f.finance.AutoApproveTicketRefund(ctx, tk, "")
		r, err := f.client.SiteRefund.Query().Where(entsiterefund.OrderIDEQ(o.ID)).Only(ctx)
		if err != nil || r.Status != entsiterefund.StatusPending || len(gw.calls) != 0 {
			t.Fatalf("refund = %+v, %v; calls = %d", r, err, len(gw.calls))
		}
		eq(t, "balance", f.reload(), "30")
		if msgs := f.systemMessages(tk); len(msgs) != 1 || !strings.Contains(msgs[0], "超过自动审批上限") {
			t.Errorf("system messages = %q", msgs)
		}
	})

	t.Run("someone else's order", func(t *testing.T) {
		f := newRefundFixture(t)
		o := f.order("alipay", "30", 3)
		f.finance.SetRefundGateway("alipay", &fakeRefunds{outcome: RefundOutcome{Refunded: true}})
		f.autoApprove(true, "")
		tk := f.refundTicket(o)
		tk.UserID = newInviteTestUser(t, f.client).ID
		f.finance.AutoApproveTicketRefund(ctx, tk, "")
		if n, _ := f.client.SiteRefund.Query().Where(entsiterefund.OrderIDEQ(o.ID)).Count(ctx); n != 0 {
			t.Errorf("refunds = %d, want 0", n)
		}
	})
}

func TestRefundAutoApproveCap(t *testing.T) {
	for _, c := range []struct {
		in   string
		want string
		ok   bool
	}{{"", "0", true}, {" 0 ", "0", true}, {"12.5", "12.5", true}, {"-1", "0", false}, {"abc", "0", false}} {
		st := &SiteSettings{RefundAutoApproveMax: c.in}
		got, ok := st.RefundAutoApproveCap()
		if ok != c.ok || got.String() != c.want {
			t.Errorf("%q: got %s, %v; want %s, %v", c.in, got, ok, c.want, c.ok)
		}
	}
}

// A subscription refund the channel has been asked to pay, with no answer
// yet, is in flight: a self-service refund must neither cancel it nor send
// another refund for the same plan, and staff cannot reject it by hand.
func TestAutoApprovedSubscriptionRefundInFlightIsNotRefundedAgain(t *testing.T) {
	f := newRefundFixture(t)
	ctx := context.Background()
	o, _ := f.subscription("alipay", "30")
	gw := &fakeRefunds{} // neither refunded nor rejected: outcome unknown
	f.finance.SetRefundGateway("alipay", gw)
	f.autoApprove(true, "")
	tk := f.refundTicket(o)

	f.finance.AutoApproveTicketRefund(ctx, tk, "")
	if len(gw.calls) != 1 {
		t.Fatalf("calls = %d, want 1", len(gw.calls))
	}
	r, err := f.client.SiteRefund.Query().Where(entsiterefund.OrderIDEQ(o.ID)).Only(ctx)
	if err != nil || r.Status != entsiterefund.StatusPending || !sentToGateway(r) {
		t.Fatalf("refund = %+v, %v; want pending and sent", r, err)
	}
	if msgs := f.systemMessages(tk); len(msgs) != 1 || strings.Contains(msgs[0], "预扣") {
		t.Errorf("system messages = %q", msgs)
	}

	if _, err := f.finance.ExecuteSelfRefund(ctx, f.user.ID); err != nil {
		t.Fatal(err)
	}
	if len(gw.calls) != 1 {
		t.Errorf("self-service refund sent %d more refunds", len(gw.calls)-1)
	}
	again, _ := f.client.SiteRefund.Get(ctx, r.ID)
	if again.Status != entsiterefund.StatusPending {
		t.Errorf("in-flight refund status = %s, want pending", again.Status)
	}
	if _, err := f.finance.RejectRefund(ctx, r.ID, f.user.ID, ""); !errors.Is(err, ErrRefundManaged) {
		t.Errorf("reject err = %v, want ErrRefundManaged", err)
	}
}
