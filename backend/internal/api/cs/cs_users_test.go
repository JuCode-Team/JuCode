package cs

import (
	"testing"

	"github.com/shopspring/decimal"
)

func TestSignedBalanceAmount(t *testing.T) {
	cases := []struct {
		name string
		kind string
		in   string
		want string
	}{
		{name: "topup positive", kind: "topup", in: "10", want: "10"},
		{name: "topup negative", kind: "topup", in: "-10", want: "10"},
		{name: "refund positive", kind: "refund", in: "10", want: "10"},
		{name: "deduct positive", kind: "deduct", in: "10", want: "-10"},
		{name: "deduct negative", kind: "deduct", in: "-10", want: "-10"},
		{name: "adjust positive", kind: "adjust", in: "10", want: "10"},
		{name: "adjust negative", kind: "adjust", in: "-10", want: "-10"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := decimal.RequireFromString(tc.in)
			want := decimal.RequireFromString(tc.want)
			if got := signedBalanceAmount(tc.kind, in); !got.Equal(want) {
				t.Fatalf("signedBalanceAmount(%q, %s) = %s, want %s", tc.kind, in, got, want)
			}
		})
	}
}
