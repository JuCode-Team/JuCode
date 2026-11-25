// Package biz — automated account sanction engine.
//
// SanctionSvc turns confirmed abuse signals into graduated, mostly-reversible
// account actions: cooldown (priority drop) -> temporary freeze -> permanent
// ban. It is deliberately driven ONLY by LLM-confirmed violations (see
// ModerationSvc), never by raw keyword hits — so a dictionary false positive
// can at worst cost a single request a 422, never a frozen or banned account.
//
// Counters live in Redis as fixed-window counters (cheap, no per-request DB
// aggregation). Freezes are time-boxed via user.frozen_until and lift on their
// own at expiry; bans flip user.status and require a human to reverse.
package biz

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"

	"github.com/jucode/jucode/internal/data"
	entuser "github.com/jucode/jucode/internal/data/ent/user"
	entincident "github.com/jucode/jucode/internal/data/ent/userincident"
)

// Sanction thresholds. Conservative defaults; tune in code (kept here rather
// than config to avoid a sprawling config surface for an internal policy).
const (
	sanctionCooldownWindow = time.Hour          // counting window for cooldown
	sanctionCooldownCount  = 3                  // confirmed violations -> cooldown
	sanctionHighWindow     = 24 * time.Hour     // counting window for freeze
	sanctionHighCount      = 5                  // high+ confirmed -> freeze
	sanctionFreezeShort    = 2 * time.Hour      // first freeze duration
	sanctionFreezeLong     = 24 * time.Hour     // repeat / critical freeze duration
	sanctionFreezeMemory   = 7 * 24 * time.Hour // how long a freeze is "remembered"
	sanctionCooldownDrop   = 20                 // priority decrement on cooldown
)

type SanctionSvc struct {
	d     *data.Data
	rdb   *redis.Client
	audit *AuditSvc
	log   *slog.Logger
}

func NewSanctionSvc(d *data.Data, rdb *redis.Client, audit *AuditSvc, log *slog.Logger) *SanctionSvc {
	if log == nil {
		log = slog.Default()
	}
	return &SanctionSvc{d: d, rdb: rdb, audit: audit, log: log}
}

// RegisterAbuse records one LLM-confirmed violation for uid and applies the
// escalation ladder. severity is the confirmed severity ("low".."critical").
// Best-effort: all failures are logged, never returned — this runs off the
// hot path inside the moderation worker.
func (s *SanctionSvc) RegisterAbuse(ctx context.Context, uid uuid.UUID, severity, reason string) {
	if s == nil || uid == uuid.Nil {
		return
	}
	blocks := s.incr(ctx, s.key("viol", uid), sanctionCooldownWindow)
	highs := int64(0)
	if severityRankBiz(severity) >= severityRankBiz("high") {
		highs = s.incr(ctx, s.key("high", uid), sanctionHighWindow)
	}
	frozenBefore := s.count(ctx, s.key("frz", uid))

	switch {
	case severityRankBiz(severity) >= severityRankBiz("critical"):
		// Critical + a freeze already on record in the memory window =>
		// repeat offender, escalate to a permanent ban. Otherwise a long
		// freeze first.
		if frozenBefore >= 1 {
			s.ban(ctx, uid, reason)
		} else {
			s.freeze(ctx, uid, sanctionFreezeLong, reason)
		}
	case highs >= sanctionHighCount:
		dur := sanctionFreezeShort
		if frozenBefore >= 1 {
			dur = sanctionFreezeLong
		}
		s.freeze(ctx, uid, dur, reason)
	case blocks >= sanctionCooldownCount:
		s.cooldown(ctx, uid, reason)
	}
}

func (s *SanctionSvc) freeze(ctx context.Context, uid uuid.UUID, dur time.Duration, reason string) {
	until := time.Now().Add(dur)
	u, err := s.d.Ent.User.Get(ctx, uid)
	if err != nil {
		s.log.Warn("sanction.freeze_lookup", "user_id", uid, "err", err.Error())
		return
	}
	// Idempotent: don't shorten an existing, further-out freeze.
	if u.FrozenUntil != nil && u.FrozenUntil.After(until) {
		return
	}
	if err := s.d.Ent.User.UpdateOneID(uid).SetFrozenUntil(until).Exec(ctx); err != nil {
		s.log.Warn("sanction.freeze_apply", "user_id", uid, "err", err.Error())
		return
	}
	s.incr(ctx, s.key("frz", uid), sanctionFreezeMemory)
	s.record(ctx, uid, "freeze", "high", fmt.Sprintf("auto-frozen until %s: %s", until.Format(time.RFC3339), reason))
	s.log.Info("sanction.freeze", "user_id", uid, "until", until, "reason", reason)
}

func (s *SanctionSvc) ban(ctx context.Context, uid uuid.UUID, reason string) {
	if err := s.d.Ent.User.UpdateOneID(uid).SetStatus(entuser.StatusBanned).Exec(ctx); err != nil {
		s.log.Warn("sanction.ban_apply", "user_id", uid, "err", err.Error())
		return
	}
	s.record(ctx, uid, "ban", "critical", "auto-banned (repeat critical violation): "+reason)
	s.log.Warn("sanction.ban", "user_id", uid, "reason", reason)
}

func (s *SanctionSvc) cooldown(ctx context.Context, uid uuid.UUID, reason string) {
	// Drop priority (clamped at 0) so routing deprioritizes the user. Two
	// ranged updates avoid a racy read-modify-write against the recovery loop.
	_, _ = s.d.Ent.User.Update().
		Where(entuser.IDEQ(uid), entuser.PriorityGTE(sanctionCooldownDrop)).
		AddPriority(-sanctionCooldownDrop).Save(ctx)
	_, _ = s.d.Ent.User.Update().
		Where(entuser.IDEQ(uid), entuser.PriorityGT(0), entuser.PriorityLT(sanctionCooldownDrop)).
		SetPriority(0).Save(ctx)
	s.record(ctx, uid, "cooldown", "medium", "auto-cooldown (priority lowered): "+reason)
	s.log.Info("sanction.cooldown", "user_id", uid, "reason", reason)
}

// record writes both an audit-log row (actor = nil = system) and a user
// incident so the action shows up in both the admin audit trail and the
// user's incident history.
func (s *SanctionSvc) record(ctx context.Context, uid uuid.UUID, action, severity, msg string) {
	if s.audit != nil {
		s.audit.RecordAsync(AuditEntry{
			ActorUserID: uuid.Nil,
			Action:      "sanction.auto." + action,
			Target:      uid.String(),
			Payload:     map[string]any{"reason": msg},
		})
	}
	if _, err := s.d.Ent.UserIncident.Create().
		SetUserID(uid).
		SetKind(entincident.KindSanction).
		SetSeverity(entincident.Severity(severity)).
		SetAction(action).
		SetMessage(msg).
		Save(ctx); err != nil {
		s.log.Warn("sanction.incident", "user_id", uid, "err", err.Error())
	}
}

func (s *SanctionSvc) key(kind string, uid uuid.UUID) string {
	return fmt.Sprintf("sanction:%s:%s", kind, uid.String())
}

// incr bumps a fixed-window counter and (re)applies its TTL, returning the
// new value. Redis failures degrade to 0 (no escalation) rather than block.
func (s *SanctionSvc) incr(ctx context.Context, key string, ttl time.Duration) int64 {
	if s.rdb == nil {
		return 0
	}
	n, err := s.rdb.Incr(ctx, key).Result()
	if err != nil {
		s.log.Warn("sanction.incr", "key", key, "err", err.Error())
		return 0
	}
	if n == 1 {
		_ = s.rdb.Expire(ctx, key, ttl).Err()
	}
	return n
}

func (s *SanctionSvc) count(ctx context.Context, key string) int64 {
	if s.rdb == nil {
		return 0
	}
	n, err := s.rdb.Get(ctx, key).Int64()
	if err != nil {
		return 0
	}
	return n
}
