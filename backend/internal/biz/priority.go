// Package biz — user priority recovery.
//
// Abuse-flavored incidents (policy_keyword / cyber_security) decrement a
// user's priority by 10 (see IncidentSvc.Record / decrementUserPriority).
// Without a counterpart, priority only ever ratchets down. StartRecoveryLoop
// gradually restores it: every interval, users who have NOT triggered such
// an incident in the last 24h get their priority stepped back up toward the
// default ceiling. Run under the leader so a single instance applies it.
package biz

import (
	"context"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/jucode/jucode/internal/data/ent"
	entuser "github.com/jucode/jucode/internal/data/ent/user"
	entincident "github.com/jucode/jucode/internal/data/ent/userincident"
)

const (
	// priorityCeiling is the default/max user priority (see user schema:
	// priority is 0-100, default 100). Recovery never pushes past it.
	priorityCeiling = 100
	// priorityRecoveryQuietWindow is the window over which policy hits are
	// counted when deciding whether to pause a user's recovery.
	priorityRecoveryQuietWindow = 24 * time.Hour
	// priorityRecoveryEventThreshold pauses recovery only for users with at
	// least this many policy_keyword hits in the window. A single false
	// positive (~1/day) no longer pins a user at low priority — it takes a
	// sustained pattern. cyber_security is excluded entirely (upstream-driven).
	priorityRecoveryEventThreshold = 3
)

// StartRecoveryLoop blocks, stepping eligible users' priority up by step
// every interval until ctx is cancelled. interval<=0 or step<=0 disables.
func (s *IncidentSvc) StartRecoveryLoop(ctx context.Context, interval time.Duration, step int) {
	if interval <= 0 || step <= 0 {
		return
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := s.recoverPriorities(ctx, step); err != nil {
				slog.Warn("priority.recovery_failed", "err", err.Error())
			}
		}
	}
}

// recoverPriorities bumps priority (clamped to priorityCeiling) for every
// user below the ceiling EXCEPT those with a sustained pattern of
// policy_keyword hits (>= threshold) in the window. Done as two ranged
// updates so we can clamp at the ceiling without per-row reads.
func (s *IncidentSvc) recoverPriorities(ctx context.Context, step int) error {
	since := time.Now().Add(-priorityRecoveryQuietWindow)
	var rows []struct {
		UserID uuid.UUID `json:"user_id"`
		Count  int       `json:"count"`
	}
	if err := s.d.Ent.UserIncident.Query().
		Where(
			entincident.CreatedAtGTE(since),
			entincident.UserIDNotNil(),
			entincident.KindEQ(entincident.KindPolicyKeyword),
		).
		GroupBy(entincident.FieldUserID).
		Aggregate(ent.Count()).
		Scan(ctx, &rows); err != nil {
		return err
	}
	var penalized []uuid.UUID
	for _, r := range rows {
		if r.Count >= priorityRecoveryEventThreshold {
			penalized = append(penalized, r.UserID)
		}
	}

	// Near the ceiling (priority >= ceiling-step): snap to the ceiling.
	snap := s.d.Ent.User.Update().
		Where(entuser.PriorityGTE(priorityCeiling-step), entuser.PriorityLT(priorityCeiling)).
		SetPriority(priorityCeiling)
	if len(penalized) > 0 {
		snap = snap.Where(entuser.IDNotIn(penalized...))
	}
	if _, err := snap.Save(ctx); err != nil {
		return err
	}

	// Further below: increment by step.
	bump := s.d.Ent.User.Update().
		Where(entuser.PriorityLT(priorityCeiling - step)).
		AddPriority(step)
	if len(penalized) > 0 {
		bump = bump.Where(entuser.IDNotIn(penalized...))
	}
	if _, err := bump.Save(ctx); err != nil {
		return err
	}
	return nil
}
