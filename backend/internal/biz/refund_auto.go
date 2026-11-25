package biz

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jucode/jucode/internal/data/ent"
	entticketmsg "github.com/jucode/jucode/internal/data/ent/ticketmessage"
)

// AutoApproveTicketRefund handles a refund ticket a user just opened for
// one of their orders, when RefundAutoApproveEnabled is on: it raises a
// refund request for the order's default refundable amount and, when the
// order's channel can refund automatically and the amount is within the
// cap, approves it at once. The outcome is posted on the ticket; anything it
// does not approve stays pending for staff as before. Callers skip it for
// personal access tokens: like self-service refunds, moving money needs a
// signed-in session.
func (s *FinanceSvc) AutoApproveTicketRefund(ctx context.Context, t *ent.Ticket, reason string) {
	if t.OrderID == nil || s.settings == nil {
		return
	}
	st, err := s.settings.GetSite(ctx)
	if err != nil || !st.RefundAutoApproveEnabled {
		return
	}
	limit, ok := st.RefundAutoApproveCap()
	if !ok {
		return
	}
	o, err := s.d.Ent.SiteOrder.Get(ctx, *t.OrderID)
	if err != nil || o.UserID != t.UserID {
		return
	}
	// The gateway call may take a while and the ticket is already saved:
	// the user hanging up must not leave a refund half booked.
	ctx = context.WithoutCancel(ctx)

	r, err := s.CreateRefund(ctx, CreateRefundInput{OrderID: o.ID, Reason: reason, Note: "自动审批"})
	switch {
	case errors.Is(err, ErrNothingRefundable):
		s.postTicketSystemMessage(ctx, t, "该订单当前没有可退金额，已消费的部分不能退。如有疑问，请在工单里说明。")
		return
	case err != nil:
		if !errors.Is(err, ErrRefundPending) {
			slog.Warn("refund auto-approve: create refund", "ticket", t.ID, "order", o.ID, "err", err)
		}
		return
	}
	amount := r.Amount.StringFixed(2)
	gw := s.refundGateways[o.PaymentChannel]
	switch {
	case gw == nil:
		s.postTicketSystemMessage(ctx, t, fmt.Sprintf("已提交退款申请，金额 ¥%s。该订单的支付方式不支持自动退款，客服审核后处理。", amount))
		return
	case limit.Sign() > 0 && r.Amount.GreaterThan(limit):
		s.postTicketSystemMessage(ctx, t, fmt.Sprintf("已提交退款申请，金额 ¥%s，超过自动审批上限，客服审核后处理。", amount))
		return
	}
	if _, err := s.approveGatewayRefund(ctx, gw, r.ID, t.UserID, "自动审批"); err != nil {
		slog.Warn("refund auto-approve: approve", "ticket", t.ID, "refund", r.ID, "err", err)
		s.postTicketSystemMessage(ctx, t, fmt.Sprintf("已提交退款申请，金额 ¥%s。自动退款未完成，客服会继续处理。", amount))
		return
	}
	s.postTicketSystemMessage(ctx, t, fmt.Sprintf("退款 ¥%s 已原路退回，到账时间以支付渠道为准。", amount))
}

// postTicketSystemMessage adds a message from the system to the ticket. It
// counts as a staff reply for unread marks but leaves the status alone.
func (s *FinanceSvc) postTicketSystemMessage(ctx context.Context, t *ent.Ticket, body string) {
	now := time.Now()
	if _, err := s.d.Ent.TicketMessage.Create().
		SetTicketID(t.ID).
		SetSenderUserID(t.UserID).
		SetSenderRoleAtSend(entticketmsg.SenderRoleAtSendSystem).
		SetBody(body).
		Save(ctx); err != nil {
		slog.Warn("refund auto-approve: post ticket message", "ticket", t.ID, "err", err)
		return
	}
	_ = s.d.Ent.Ticket.UpdateOneID(t.ID).SetLastReplyAt(now).SetLastStaffReplyAt(now).Exec(ctx)
}
