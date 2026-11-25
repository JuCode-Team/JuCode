package alipay

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jucode/jucode/internal/biz"
)

// refundServer answers alipay.trade.refund / alipay.trade.fastpay.refund.query
// with the given bodies and keeps the last biz_content it saw.
func refundServer(t *testing.T, refund, query string, seen *map[string]any) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		_ = json.Unmarshal([]byte(r.PostForm.Get("biz_content")), seen)
		switch r.PostForm.Get("method") {
		case "alipay.trade.refund":
			_, _ = w.Write([]byte(refund))
		case "alipay.trade.fastpay.refund.query":
			_, _ = w.Write([]byte(query))
		default:
			t.Errorf("unexpected method %q", r.PostForm.Get("method"))
		}
	}))
}

func TestRefundOutcomes(t *testing.T) {
	cases := []struct {
		name, refund, query string
		refunded, rejected  bool
		unknown             bool
	}{
		{name: "money moved",
			refund:   `{"alipay_trade_refund_response":{"code":"10000","msg":"Success","fund_change":"Y","refund_fee":"12.34"}}`,
			refunded: true},
		{name: "repeat request confirmed by query",
			refund:   `{"alipay_trade_refund_response":{"code":"10000","msg":"Success","fund_change":"N"}}`,
			query:    `{"alipay_trade_fastpay_refund_query_response":{"code":"10000","msg":"Success","refund_status":"REFUND_SUCCESS"}}`,
			refunded: true},
		{name: "N and nothing confirmed stays unknown",
			refund: `{"alipay_trade_refund_response":{"code":"10000","msg":"Success","fund_change":"N"}}`,
			query:  `{"alipay_trade_fastpay_refund_query_response":{"code":"10000","msg":"Success"}}`},
		{name: "business failure is a refusal",
			refund:   `{"alipay_trade_refund_response":{"code":"40004","msg":"Business Failed","sub_code":"ACQ.TRADE_STATUS_ERROR","sub_msg":"交易状态不合法"}}`,
			rejected: true},
		{name: "system error is unknown",
			refund:  `{"alipay_trade_refund_response":{"code":"40004","msg":"Business Failed","sub_code":"ACQ.SYSTEM_ERROR","sub_msg":"系统错误"}}`,
			unknown: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var seen map[string]any
			srv := refundServer(t, tc.refund, tc.query, &seen)
			defer srv.Close()
			c := newTestClient(t)
			c.cfg.GatewayURL = srv.URL
			out, err := NewFinanceAdapter(c).Refund(context.Background(), refundReq())
			if tc.unknown {
				if err == nil {
					t.Fatalf("want an error for an unknown outcome, got %+v", out)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if out.Refunded != tc.refunded || out.Rejected != tc.rejected {
				t.Errorf("outcome = %+v", out)
			}
		})
	}
}

func TestRefundRequestFields(t *testing.T) {
	var seen map[string]any
	srv := refundServer(t, `{"alipay_trade_refund_response":{"code":"10000","fund_change":"Y"}}`, "", &seen)
	defer srv.Close()
	c := newTestClient(t)
	c.cfg.GatewayURL = srv.URL
	if _, err := NewFinanceAdapter(c).Refund(context.Background(), refundReq()); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"out_trade_no": "SO1", "out_request_no": "RF1", "refund_amount": "12.34", "refund_reason": "余额退款"}
	for k, v := range want {
		if seen[k] != v {
			t.Errorf("%s = %v, want %v", k, seen[k], v)
		}
	}
}

func refundReq() biz.RefundRequest {
	return biz.RefundRequest{OrderNo: "SO1", RequestNo: "RF1", AmountCents: 1234, Reason: "余额退款"}
}
