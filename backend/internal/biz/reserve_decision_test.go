package biz

import (
	"errors"
	"testing"

	"github.com/shopspring/decimal"
)

func TestDecideMonetaryReserve(t *testing.T) {
	t.Parallel()
	d := decimal.RequireFromString
	tests := []struct {
		name             string
		available        string
		estimate         string
		otherInteractive int
		wantHold         string
		wantErr          error
	}{
		{name: "zero balance", available: "0", estimate: "2", otherInteractive: 0, wantErr: ErrInsufficientBalance},
		{name: "negative balance", available: "-0.1", estimate: "2", otherInteractive: 0, wantErr: ErrInsufficientBalance},
		{name: "last request estimate exceeds", available: "0.20", estimate: "2.00", otherInteractive: 0, wantHold: "0.20"},
		{name: "low water small estimate", available: "0.20", estimate: "0.05", otherInteractive: 0, wantHold: "0.20"},
		{name: "last request blocked by in-flight", available: "0.20", estimate: "2.00", otherInteractive: 1, wantErr: ErrInsufficientBalance},
		{name: "ordinary full hold", available: "1.00", estimate: "0.30", otherInteractive: 0, wantHold: "0.30"},
		{name: "ordinary at threshold", available: "0.25", estimate: "0.25", otherInteractive: 0, wantHold: "0.25"},
		{name: "became last while other hold live", available: "0.40", estimate: "0.50", otherInteractive: 1, wantErr: ErrInsufficientBalance},
		{name: "became last no other hold", available: "0.40", estimate: "0.50", otherInteractive: 0, wantHold: "0.40"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := decideMonetaryReserve(d(tc.available), d(tc.estimate), tc.otherInteractive)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("err = %v, want %v", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if !got.Equal(d(tc.wantHold)) {
				t.Fatalf("hold = %s, want %s", got, tc.wantHold)
			}
		})
	}
}
