// generation_task.go — persistence for async generation jobs (video, and
// later async image/audio). Backs the OpenAI-Sora-style /v1/videos surface.
package biz

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/jucode/jucode/internal/data"
	"github.com/jucode/jucode/internal/data/ent"
	entgentask "github.com/jucode/jucode/internal/data/ent/generationtask"
	entmodel "github.com/jucode/jucode/internal/data/ent/model"
)

// TaskSvc manages GenerationTask rows. It is a thin CRUD layer; the gateway
// owns the submit/poll/billing orchestration.
type TaskSvc struct{ d *data.Data }

func NewTaskSvc(d *data.Data) *TaskSvc { return &TaskSvc{d: d} }

// CreateTaskInput is the routing snapshot captured at submit time.
type CreateTaskInput struct {
	ID             uuid.UUID
	UserID         uuid.UUID
	ApiKeyID       *uuid.UUID
	OrganizationID *uuid.UUID
	RequestID      string
	Endpoint       string
	Model          string
	GroupID        *uuid.UUID
	ProviderID     *uuid.UUID
	UpstreamModel  string
	ProviderFormat string
	UpstreamTaskID string
}

// Create inserts a new task in the queued state.
func (s *TaskSvc) Create(ctx context.Context, in CreateTaskInput) (*ent.GenerationTask, error) {
	c := s.d.Ent.GenerationTask.Create().
		SetUserID(in.UserID).
		SetRequestID(in.RequestID).
		SetEndpoint(orDefault(in.Endpoint, "video_generation")).
		SetModel(in.Model).
		SetUpstreamModel(in.UpstreamModel).
		SetProviderFormat(in.ProviderFormat).
		SetUpstreamTaskID(in.UpstreamTaskID).
		SetStatus(entgentask.StatusQueued)
	if in.ID != uuid.Nil {
		c.SetID(in.ID)
	}
	if in.ApiKeyID != nil {
		c.SetAPIKeyID(*in.ApiKeyID)
	}
	if in.OrganizationID != nil {
		c.SetOrganizationID(*in.OrganizationID)
	}
	if in.GroupID != nil {
		c.SetGroupID(*in.GroupID)
	}
	if in.ProviderID != nil {
		c.SetProviderID(*in.ProviderID)
	}
	return c.Save(ctx)
}

// Get loads a task by id. Callers must check ownership (user_id) themselves.
func (s *TaskSvc) Get(ctx context.Context, id uuid.UUID) (*ent.GenerationTask, error) {
	return s.d.Ent.GenerationTask.Get(ctx, id)
}

// SetStatus advances a task's lifecycle status (queued -> running).
func (s *TaskSvc) SetStatus(ctx context.Context, id uuid.UUID, status string) error {
	return s.d.Ent.GenerationTask.UpdateOneID(id).
		SetStatus(entgentask.Status(status)).
		Exec(ctx)
}

// CompleteInput records a terminal transition.
type CompleteInput struct {
	Status        string // "succeeded" | "failed"
	BilledSeconds int
	ErrorMessage  string
}

// Complete writes the terminal state. Idempotent-friendly: the
// gateway guards double-billing with the `billed` flag via MarkBilled.
func (s *TaskSvc) Complete(ctx context.Context, id uuid.UUID, in CompleteInput) error {
	now := time.Now()
	return s.d.Ent.GenerationTask.UpdateOneID(id).
		SetStatus(entgentask.Status(in.Status)).
		SetBilledSeconds(in.BilledSeconds).
		SetErrorMessage(SanitizeTaskError(in.ErrorMessage)).
		SetCompletedAt(now).
		Exec(ctx)
}

// MarkBilled flips the billed flag and records the settled cost. Returns
// (true) only for the caller that won the transition from billed=false, so
// billing runs exactly once even under concurrent polls.
func (s *TaskSvc) MarkBilled(ctx context.Context, id uuid.UUID, cost decimal.Decimal) (bool, error) {
	n, err := s.d.Ent.GenerationTask.Update().
		Where(entgentask.IDEQ(id), entgentask.BilledEQ(false)).
		SetBilled(true).
		SetCostFinal(cost).
		Save(ctx)
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// TaskBillingContext is everything needed to settle a completed task's cost.
type TaskBillingContext struct {
	User   *ent.User
	ApiKey *ent.ApiKey // nil for OAuth-device callers
	Model  *ent.Model
	Group  *ent.ModelGroup
}

// LoadBillingContext resolves the user / api key / model / group referenced by
// a task, so billing can be settled at completion time (a different request
// than submit, possibly the background reconciler). Missing model/group is an
// error; a missing/absent api key is tolerated (nil).
func (s *TaskSvc) LoadBillingContext(ctx context.Context, t *ent.GenerationTask) (*TaskBillingContext, error) {
	user, err := s.d.Ent.User.Get(ctx, t.UserID)
	if err != nil {
		return nil, err
	}
	bc := &TaskBillingContext{User: user}
	if t.APIKeyID != nil {
		if ak, err := s.d.Ent.ApiKey.Get(ctx, *t.APIKeyID); err == nil {
			bc.ApiKey = ak
		}
	}
	m, err := s.d.Ent.Model.Query().Where(entmodel.NameEQ(t.Model)).Only(ctx)
	if err != nil {
		return nil, err
	}
	bc.Model = m
	if t.GroupID != nil {
		g, err := s.d.Ent.ModelGroup.Get(ctx, *t.GroupID)
		if err != nil {
			return nil, err
		}
		bc.Group = g
	}
	return bc, nil
}

// Inflight lists non-terminal tasks whose last update is older than `olderThan`
// — the reconciler's work queue. Bounded by `limit`.
func (s *TaskSvc) Inflight(ctx context.Context, olderThan time.Time, limit int) ([]*ent.GenerationTask, error) {
	return s.d.Ent.GenerationTask.Query().
		Where(
			entgentask.StatusIn(entgentask.StatusQueued, entgentask.StatusRunning),
			entgentask.UpdatedAtLT(olderThan),
		).
		Order(ent.Asc(entgentask.FieldUpdatedAt)).
		Limit(limit).
		All(ctx)
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}
