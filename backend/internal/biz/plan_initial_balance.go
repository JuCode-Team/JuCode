package biz

import (
	"context"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/jucode/jucode/internal/data/ent"
	entbalancetx "github.com/jucode/jucode/internal/data/ent/balancetx"
)

// grantPlanInitialBalance credits a pay_as_you_go plan's initial balance
// to the user. plan.InitialBalance is denominated in points (1 point = 1
// CNY) — credited as-is, no exchange rate. The grant is a gift, not a cash
// topup, and is tied to the entitlement that issued it.
func grantPlanInitialBalance(ctx context.Context, tx *ent.Tx, u *ent.User, plan *ent.Plan, userPlanID uuid.UUID, note string) error {
	if plan.Type != "pay_as_you_go" || plan.InitialBalance.Sign() <= 0 {
		return nil
	}
	amount := plan.InitialBalance
	newBal := u.Balance.Add(amount)
	if _, err := tx.User.UpdateOneID(u.ID).SetBalance(newBal).SetCurrency(CurrencyCNY).Save(ctx); err != nil {
		return err
	}
	if _, err := tx.BalanceTx.Create().
		SetUserID(u.ID).
		SetKind(entbalancetx.KindGift).
		SetAmount(amount).
		SetBalanceAfter(newBal).
		SetCurrency(CurrencyCNY).
		SetRelatedUserPlanID(userPlanID).
		SetNote(note).
		Save(ctx); err != nil {
		return err
	}
	u.Balance = newBal
	return nil
}

type planGiftLot struct {
	userPlanID uuid.UUID
	remaining  decimal.Decimal
}

// remainingPlanGift replays the immutable personal balance ledger using
// gift-first, FIFO consumption. Spending/transfer rows consume outstanding
// gifts; cash-refund rows do not. Gift revocation rows consume only the
// entitlement they reference.
func remainingPlanGift(ctx context.Context, tx *ent.Tx, userID, userPlanID uuid.UUID) (decimal.Decimal, error) {
	rows, err := tx.BalanceTx.Query().
		Where(entbalancetx.UserIDEQ(userID)).
		Order(ent.Asc(entbalancetx.FieldCreatedAt), ent.Asc(entbalancetx.FieldID)).
		All(ctx)
	if err != nil {
		return decimal.Zero, err
	}

	lots := make([]planGiftLot, 0)
	for _, row := range rows {
		if row.Kind == entbalancetx.KindGift {
			if row.RelatedUserPlanID == nil {
				continue
			}
			if row.Amount.Sign() > 0 {
				lots = append(lots, planGiftLot{
					userPlanID: *row.RelatedUserPlanID,
					remaining:  row.Amount,
				})
			} else if row.Amount.Sign() < 0 {
				consumePlanGiftLots(lots, *row.RelatedUserPlanID, row.Amount.Abs())
			}
			continue
		}
		if row.Amount.Sign() < 0 && row.Kind != entbalancetx.KindRefund {
			consumeGiftLotsFIFO(lots, row.Amount.Abs())
		}
	}

	remaining := decimal.Zero
	for _, lot := range lots {
		if lot.userPlanID == userPlanID {
			remaining = remaining.Add(lot.remaining)
		}
	}
	return remaining, nil
}

func consumePlanGiftLots(lots []planGiftLot, userPlanID uuid.UUID, amount decimal.Decimal) {
	for i := range lots {
		if amount.Sign() <= 0 {
			return
		}
		if lots[i].userPlanID != userPlanID || lots[i].remaining.Sign() <= 0 {
			continue
		}
		used := decimal.Min(lots[i].remaining, amount)
		lots[i].remaining = lots[i].remaining.Sub(used)
		amount = amount.Sub(used)
	}
}

func consumeGiftLotsFIFO(lots []planGiftLot, amount decimal.Decimal) {
	for i := range lots {
		if amount.Sign() <= 0 {
			return
		}
		if lots[i].remaining.Sign() <= 0 {
			continue
		}
		used := decimal.Min(lots[i].remaining, amount)
		lots[i].remaining = lots[i].remaining.Sub(used)
		amount = amount.Sub(used)
	}
}

// revokeRemainingPlanGift removes only the promotional balance that remains
// for one entitlement. The caller must hold the user row lock so the ledger
// replay and balance update are atomic with concurrent charges and topups.
func revokeRemainingPlanGift(ctx context.Context, tx *ent.Tx, u *ent.User, userPlanID uuid.UUID, note string) (decimal.Decimal, error) {
	remaining, err := remainingPlanGift(ctx, tx, u.ID, userPlanID)
	if err != nil {
		return decimal.Zero, err
	}
	available := decimal.Max(u.Balance, decimal.Zero)
	remaining = decimal.Min(remaining, available)
	if remaining.Sign() <= 0 {
		return decimal.Zero, nil
	}

	newBal := u.Balance.Sub(remaining)
	if _, err := tx.User.UpdateOneID(u.ID).
		SetBalance(newBal).
		SetCurrency(CurrencyCNY).
		Save(ctx); err != nil {
		return decimal.Zero, err
	}
	if _, err := tx.BalanceTx.Create().
		SetUserID(u.ID).
		SetKind(entbalancetx.KindGift).
		SetAmount(remaining.Neg()).
		SetBalanceAfter(newBal).
		SetCurrency(CurrencyCNY).
		SetRelatedUserPlanID(userPlanID).
		SetNote(note).
		Save(ctx); err != nil {
		return decimal.Zero, err
	}
	u.Balance = newBal
	return remaining, nil
}
