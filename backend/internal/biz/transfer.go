package biz

import (
	"context"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

// Balance 读用户当前余额。
func (a *Account) Balance(ctx context.Context, userID uuid.UUID) (decimal.Decimal, error) {
	u, err := a.d.Ent.User.Get(ctx, userID)
	if err != nil {
		return decimal.Zero, err
	}
	return u.Balance, nil
}
