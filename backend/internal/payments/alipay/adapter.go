package alipay

import (
	"context"
	"errors"
	"net/http"

	"github.com/jucode/jucode/internal/biz"
)

type FinanceAdapter struct {
	client *Client
}

func NewFinanceAdapter(client *Client) *FinanceAdapter {
	if client == nil {
		return nil
	}
	return &FinanceAdapter{client: client}
}

func (a *FinanceAdapter) NativePrepay(ctx context.Context, in biz.NativePrepayInput) (*biz.NativePrepayResult, error) {
	if a == nil || a.client == nil {
		return nil, ErrDisabled
	}
	args := NativePrepayInput{
		OrderNo:     in.OrderNo,
		Description: in.Description,
		AmountCents: in.AmountCents,
		Attach:      in.Attach,
		ReturnURL:   in.ReturnURL,
		QuitURL:     in.QuitURL,
	}
	prepay := a.client.NativePrepay
	if in.Wap {
		prepay = a.client.WapPrepay
	}
	res, err := prepay(ctx, args)
	if err != nil {
		return nil, err
	}
	return &biz.NativePrepayResult{
		CodeURL:     res.CodeURL,
		AmountCents: res.AmountCents,
	}, nil
}

func (a *FinanceAdapter) Query(ctx context.Context, in biz.PaymentQueryInput) (*biz.PaymentQueryResult, error) {
	if a == nil || a.client == nil {
		return nil, ErrDisabled
	}
	res, err := a.client.QueryByOutTradeNo(ctx, in.OrderNo)
	if err != nil {
		return nil, err
	}
	return &biz.PaymentQueryResult{
		OrderNo:       in.OrderNo,
		TradeState:    res.TradeState,
		TransactionID: res.TransactionID,
		PayerOpenID:   res.PayerOpenID,
		AmountCents:   res.AmountCents,
	}, nil
}

func (a *FinanceAdapter) ParseCallback(headers http.Header, body []byte) (*biz.PaymentCallbackResult, error) {
	if a == nil || a.client == nil {
		return nil, ErrDisabled
	}
	res, err := a.client.ParseCallback(headers, body)
	if err != nil {
		return nil, err
	}
	return &biz.PaymentCallbackResult{
		OrderNo:       res.OrderNo,
		TradeState:    res.TradeState,
		TransactionID: res.TransactionID,
		PayerOpenID:   res.PayerOpenID,
		AmountCents:   res.AmountCents,
	}, nil
}

// Refund and QueryRefund make the adapter a biz.RefundGateway.
func (a *FinanceAdapter) Refund(ctx context.Context, in biz.RefundRequest) (biz.RefundOutcome, error) {
	if a == nil || a.client == nil {
		return biz.RefundOutcome{}, ErrDisabled
	}
	res, err := a.client.Refund(ctx, RefundInput{
		OrderNo:     in.OrderNo,
		RequestNo:   in.RequestNo,
		AmountCents: in.AmountCents,
		Reason:      in.Reason,
	})
	if err != nil {
		return refundErrOutcome(err)
	}
	if res.FundChanged {
		return biz.RefundOutcome{Refunded: true}, nil
	}
	// "N" also answers a RequestNo that was already refunded; ask.
	return a.QueryRefund(ctx, in.OrderNo, in.RequestNo)
}

func (a *FinanceAdapter) QueryRefund(ctx context.Context, orderNo, requestNo string) (biz.RefundOutcome, error) {
	if a == nil || a.client == nil {
		return biz.RefundOutcome{}, ErrDisabled
	}
	ok, err := a.client.QueryRefund(ctx, orderNo, requestNo)
	if err != nil {
		return refundErrOutcome(err)
	}
	// Without REFUND_SUCCESS the refund may not be processed yet; leave the
	// outcome unknown rather than guess.
	return biz.RefundOutcome{Refunded: ok}, nil
}

// refundErrOutcome sorts Alipay errors into a definite refusal (no money
// moved) and an unknown outcome. Business failures (40004) and request
// errors (20001, 40002, 40006) mean the refund was not made; service
// unavailable (20000), ACQ.SYSTEM_ERROR and transport errors do not tell.
func refundErrOutcome(err error) (biz.RefundOutcome, error) {
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		return biz.RefundOutcome{}, err
	}
	switch {
	case apiErr.SubCode == "ACQ.SYSTEM_ERROR", apiErr.Code == "20000":
		return biz.RefundOutcome{}, err
	case apiErr.Code == "40004", apiErr.Code == "20001", apiErr.Code == "40002", apiErr.Code == "40006":
		detail := apiErr.SubMsg
		if detail == "" {
			detail = apiErr.Msg
		}
		return biz.RefundOutcome{Rejected: true, Detail: detail + "（" + apiErr.SubCode + "）"}, nil
	default:
		return biz.RefundOutcome{}, err
	}
}
