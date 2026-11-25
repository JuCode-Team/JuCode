package biz

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jucode/jucode/internal/data/ent"
	entplan "github.com/jucode/jucode/internal/data/ent/plan"
	entuser "github.com/jucode/jucode/internal/data/ent/user"
	entuserplan "github.com/jucode/jucode/internal/data/ent/userplan"
)

var ErrInvalidUserPlan = errors.New("invalid user plan change")

// GrantUserPlan is a manual entitlement grant, including the plan's initial gift.
// It does not create a paid order or debit the user's balance.
func (s *PlanSvc) GrantUserPlan(ctx context.Context, uid, planID, requestID uuid.UUID, expires *time.Time) (*ent.UserPlan, error) {
	tx, err := s.d.Ent.Tx(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	u, err := tx.User.Query().Where(entuser.IDEQ(uid)).ForUpdate().Only(ctx)
	if err != nil {
		return nil, err
	}
	if requestID == uuid.Nil {
		return nil, ErrInvalidUserPlan
	}
	if existing, err := tx.UserPlan.Get(ctx, requestID); err == nil {
		if existing.UserID != uid || existing.PlanID != planID || (expires != nil && !existing.ExpireAt.Equal(*expires)) {
			return nil, ErrInvalidUserPlan
		}
		if err := tx.Commit(); err != nil {
			return nil, err
		}
		return existing.Unwrap(), nil
	} else if !ent.IsNotFound(err) {
		return nil, err
	}
	p, err := tx.Plan.Get(ctx, planID)
	if err != nil {
		return nil, err
	}
	if p.Status != entplan.StatusActive {
		return nil, ErrInvalidUserPlan
	}
	now := time.Now()
	days := p.PeriodDays
	if days <= 0 {
		days = 90
	}
	end := now.AddDate(0, 0, days)
	if expires != nil {
		end = *expires
	}
	if !end.After(now) || end.Year() > 9999 {
		return nil, ErrInvalidUserPlan
	}
	up, err := tx.UserPlan.Create().SetID(requestID).SetUserID(uid).SetPlanID(planID).SetStartAt(now).SetExpireAt(end).Save(ctx)
	if err != nil {
		return nil, err
	}
	if err := grantPlanInitialBalance(ctx, tx, u, p, up.ID, "admin plan grant"); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return up.Unwrap(), nil
}

// Updating the entitlement keeps its ID and consumed quota. Cross-type changes
// require a new entitlement so historical spending cannot change billing modes.
func (s *PlanSvc) UpdateUserPlan(ctx context.Context, uid, id uuid.UUID, planID *uuid.UUID, expires *time.Time, cancel bool) (*ent.UserPlan, error) {
	tx, err := s.d.Ent.Tx(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	up, err := tx.UserPlan.Query().Where(entuserplan.IDEQ(id), entuserplan.UserIDEQ(uid)).ForUpdate().Only(ctx)
	if err != nil {
		return nil, err
	}
	q := tx.UserPlan.UpdateOne(up)
	if cancel {
		q.SetStatus(entuserplan.StatusCanceled)
	} else {
		if planID == nil && expires == nil || up.Status == entuserplan.StatusCanceled {
			return nil, ErrInvalidUserPlan
		}
		if planID != nil && *planID != up.PlanID {
			old, err := tx.Plan.Get(ctx, up.PlanID)
			if err != nil {
				return nil, err
			}
			p, err := tx.Plan.Get(ctx, *planID)
			if err != nil {
				return nil, err
			}
			if p.Status != entplan.StatusActive || p.Type != old.Type {
				return nil, ErrInvalidUserPlan
			}
			q.SetPlanID(*planID)
		}
		if expires != nil {
			if !expires.After(up.StartAt) || !expires.After(time.Now()) || expires.Year() > 9999 {
				return nil, ErrInvalidUserPlan
			}
			q.SetExpireAt(*expires).SetStatus(entuserplan.StatusActive)
		}
	}
	up, err = q.Save(ctx)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return up.Unwrap(), nil
}

// ResetQuota deletes only current counters in a single atomic Redis command.
// Settlement idempotency keys stay intact, so retries cannot bill twice.
func (s *PlanSvc) ResetQuota(ctx context.Context, up *ent.UserPlan, window string) error {
	keys := quotaKeys(up, time.Now())
	switch window {
	case "all":
	case "5h":
		keys = keys[:1]
	case "weekly":
		keys = keys[1:2]
	case "monthly":
		keys = keys[2:]
	default:
		return ErrInvalidUserPlan
	}
	return s.d.Redis.Del(ctx, keys...).Err()
}
