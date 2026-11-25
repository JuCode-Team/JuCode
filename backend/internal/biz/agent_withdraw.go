package biz

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/jucode/jucode/internal/data/ent"
	entagentprofile "github.com/jucode/jucode/internal/data/ent/agentprofile"
	entagententry "github.com/jucode/jucode/internal/data/ent/agentsettlemententry"
	entbalancetx "github.com/jucode/jucode/internal/data/ent/balancetx"
	entuser "github.com/jucode/jucode/internal/data/ent/user"
)

var (
	ErrWithdrawAmountInvalid = errors.New("withdraw amount must be positive with at most 2 decimals")
	ErrWithdrawExceeds       = errors.New("withdraw amount exceeds withdrawable commission")
)

// Withdrawable is the commission an agent may move into their balance: the
// pending net the platform owes them (-TotalPayable), truncated to cents.
// Card-code sales the agent still owes are netted first, so it is zero
// whenever the agent is a net debtor.
func (p *PendingSummary) Withdrawable() decimal.Decimal {
	if p.TotalPayable.Sign() >= 0 {
		return decimal.Zero
	}
	return p.TotalPayable.Neg().Truncate(2)
}

// WithdrawToBalance credits amount of owed commission to the agent's own
// platform balance and records a balance_withdraw entry (+amount) so the
// pending net, and any later admin settlement, already reflect it.
func (s *AgentSvc) WithdrawToBalance(ctx context.Context, agentID uuid.UUID, amount decimal.Decimal) (*ent.BalanceTx, error) {
	if amount.Sign() <= 0 || !amount.Equal(amount.Truncate(2)) {
		return nil, ErrWithdrawAmountInvalid
	}
	tx, err := s.d.Ent.Tx(ctx)
	if err != nil {
		return nil, err
	}
	rollback := func(e error) (*ent.BalanceTx, error) { _ = tx.Rollback(); return nil, e }

	// The profile row lock serialises withdrawals (and Settle) per agent,
	// so two concurrent requests cannot both spend the same pending net.
	prof, err := tx.AgentProfile.Query().
		Where(entagentprofile.UserIDEQ(agentID)).
		ForUpdate().
		Only(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			return rollback(ErrAgentNotEligible)
		}
		return rollback(err)
	}
	if prof.Status != entagentprofile.StatusActive {
		return rollback(ErrAgentSuspended)
	}
	rows, err := tx.AgentSettlementEntry.Query().
		Where(entagententry.AgentIDEQ(agentID), entagententry.SettlementIDIsNil()).
		All(ctx)
	if err != nil {
		return rollback(err)
	}
	sum := &PendingSummary{}
	for _, r := range rows {
		sum.TotalPayable = sum.TotalPayable.Add(r.PlatformPayable)
	}
	if amount.GreaterThan(sum.Withdrawable()) {
		return rollback(fmt.Errorf("%w (available %s)", ErrWithdrawExceeds, sum.Withdrawable().StringFixed(2)))
	}

	u, err := tx.User.Query().Where(entuser.IDEQ(agentID)).ForUpdate().Only(ctx)
	if err != nil {
		return rollback(err)
	}
	newBal := u.Balance.Add(amount)
	if _, err := tx.User.UpdateOneID(u.ID).SetBalance(newBal).SetCurrency(CurrencyCNY).Save(ctx); err != nil {
		return rollback(err)
	}
	btx, err := tx.BalanceTx.Create().
		SetUserID(u.ID).
		SetKind(entbalancetx.KindAgentCommission).
		SetAmount(amount).
		SetBalanceAfter(newBal).
		SetCurrency(CurrencyCNY).
		SetNote("代理佣金提现").
		Save(ctx)
	if err != nil {
		return rollback(err)
	}
	if _, err := tx.AgentSettlementEntry.Create().
		SetAgentID(agentID).
		SetKind(entagententry.KindBalanceWithdraw).
		SetSaleAmount(decimal.Zero).
		SetPlatformPayable(amount).
		SetSharePctAtEvent(decimal.Zero).
		SetCurrency(CurrencyCNY).
		SetSourceType(entagententry.SourceTypeBalanceTx).
		SetSourceID(btx.ID).
		SetUserID(agentID).
		SetNote("提现到平台余额").
		Save(ctx); err != nil {
		return rollback(err)
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return btx, nil
}
