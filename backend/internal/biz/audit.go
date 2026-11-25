package biz

import (
	"context"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/jucode/jucode/internal/data"
	"github.com/jucode/jucode/internal/data/ent"
	entaudit "github.com/jucode/jucode/internal/data/ent/adminauditlog"
)

// AuditSvc captures admin mutations. Calls are best-effort: a failed
// audit insert must never block the underlying admin action.
type AuditSvc struct{ d *data.Data }

func NewAuditSvc(d *data.Data) *AuditSvc { return &AuditSvc{d: d} }

type AuditEntry struct {
	ActorUserID uuid.UUID
	Action      string         // e.g. "provider.create", "user.topup"
	Target      string         // resource id or descriptor
	Payload     map[string]any // arbitrary, jsonb-stored
	IP          string
	UA          string
}

func (s *AuditSvc) Record(ctx context.Context, e AuditEntry) (*ent.AdminAuditLog, error) {
	q := s.d.Ent.AdminAuditLog.Create().
		SetActorUserID(e.ActorUserID).
		SetAction(e.Action).
		SetTarget(e.Target).
		SetIP(e.IP).
		SetUa(e.UA)
	if e.Payload != nil {
		q = q.SetPayload(e.Payload)
	}
	return q.Save(ctx)
}

// RecordAsync detaches from the request lifecycle. The audit
// middleware uses this so a slow audit insert never delays the admin
// response the user is waiting on.
func (s *AuditSvc) RecordAsync(e AuditEntry) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if _, err := s.Record(ctx, e); err != nil {
			slog.Warn("audit.record_failed", "action", e.Action, "err", err.Error())
		}
	}()
}

// AuditListInput drives the admin /v1/admin/audit-logs query.
type AuditListInput struct {
	ActorUserID *uuid.UUID
	Action      string
	Target      string
	Since       time.Time
	Until       time.Time
	Limit       int
	Offset      int
}

func (s *AuditSvc) List(ctx context.Context, in AuditListInput) ([]*ent.AdminAuditLog, int, error) {
	q := s.d.Ent.AdminAuditLog.Query()
	if in.ActorUserID != nil {
		q = q.Where(entaudit.ActorUserIDEQ(*in.ActorUserID))
	}
	if in.Action != "" {
		q = q.Where(entaudit.ActionContainsFold(in.Action))
	}
	if in.Target != "" {
		q = q.Where(entaudit.TargetContainsFold(in.Target))
	}
	if !in.Since.IsZero() {
		q = q.Where(entaudit.CreatedAtGTE(in.Since))
	}
	if !in.Until.IsZero() {
		q = q.Where(entaudit.CreatedAtLT(in.Until))
	}
	total, err := q.Clone().Count(ctx)
	if err != nil {
		return nil, 0, err
	}
	limit := in.Limit
	if limit <= 0 || limit > 500 {
		limit = 50
	}
	rows, err := q.
		Order(ent.Desc(entaudit.FieldCreatedAt)).
		Limit(limit).Offset(in.Offset).
		All(ctx)
	return rows, total, err
}
