package biz

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"

	"github.com/jucode/jucode/internal/data/ent"
	entplan "github.com/jucode/jucode/internal/data/ent/plan"
	"github.com/jucode/jucode/internal/data/ent/planbankedreset"
	"github.com/jucode/jucode/internal/data/ent/predicate"
	entuser "github.com/jucode/jucode/internal/data/ent/user"
	entuserplan "github.com/jucode/jucode/internal/data/ent/userplan"
)

// Batch plan reset: an admin picks active subscription plans, by id or by
// filter, and the server clears a quota window, brings its next reset forward,
// or grants a banked reset, in the background. Job progress lives in Redis so
// any instance can report it.

var (
	ErrNoPlansToReset         = errors.New("no active subscription plans selected")
	ErrBankedResetUnavailable = errors.New("banked reset not found, already used, or its plan is no longer active")
)

const (
	PlanResetClear   = "clear"   // clear the window's used quota; its schedule stays
	PlanResetRestart = "restart" // reset the window now and count its schedule from now
	PlanResetGrant   = "grant"   // grant a banked reset of the window for the user to spend

	planResetJobTTL   = 7 * 24 * time.Hour
	planResetJobsKey  = "planreset:jobs"
	planResetJobsKept = 20
	planResetErrors   = 50
)

// UserPlanFilter narrows the active subscription plans an admin sees and
// resets. Zero fields do not filter.
type UserPlanFilter struct {
	Query        string     `json:"q,omitempty"`
	PlanID       *uuid.UUID `json:"plan_id,omitempty"`
	ExpireAfter  *time.Time `json:"expire_after,omitempty"`
	ExpireBefore *time.Time `json:"expire_before,omitempty"`
}

// resettablePlans selects the plans a reset applies to: active, unexpired
// quota subscriptions, the same rule as the single-plan reset.
func resettablePlans(client *ent.Client, f UserPlanFilter) *ent.UserPlanQuery {
	q := client.UserPlan.Query().Where(
		entuserplan.StatusEQ(entuserplan.StatusActive),
		entuserplan.ExpireAtGT(time.Now()),
		entuserplan.HasPlanWith(entplan.TypeEQ(entplan.TypeQuotaSubscription)),
	)
	if s := strings.TrimSpace(f.Query); s != "" {
		match := []predicate.User{entuser.EmailContainsFold(s), entuser.UsernameContainsFold(s), entuser.NicknameContainsFold(s)}
		if id, err := uuid.Parse(s); err == nil {
			match = append(match, entuser.IDEQ(id))
		}
		q = q.Where(entuserplan.HasUserWith(entuser.Or(match...)))
	}
	if f.PlanID != nil {
		q = q.Where(entuserplan.PlanIDEQ(*f.PlanID))
	}
	if f.ExpireAfter != nil {
		q = q.Where(entuserplan.ExpireAtGTE(*f.ExpireAfter))
	}
	if f.ExpireBefore != nil {
		q = q.Where(entuserplan.ExpireAtLT(*f.ExpireBefore))
	}
	return q
}

// ListResettableUserPlans pages the plans matching f, with user and plan loaded.
func (s *PlanSvc) ListResettableUserPlans(ctx context.Context, f UserPlanFilter, limit, offset int) ([]*ent.UserPlan, int, error) {
	q := resettablePlans(s.d.Ent, f)
	total, err := q.Clone().Count(ctx)
	if err != nil {
		return nil, 0, err
	}
	ups, err := q.WithUser().WithPlan().
		Order(ent.Desc(entuserplan.FieldStartAt), ent.Asc(entuserplan.FieldID)).
		Limit(limit).Offset(offset).
		All(ctx)
	return ups, total, err
}

// RestartQuotaCycle brings the next reset of up's window ("all" for every
// window) forward to now: the window starts over and later resets count from
// now. The new cycle has its own counters (see QuotaWindowsAt), so it starts
// empty and the old ones expire on their own. start_at and expire_at stay, so
// plan selection and refunds do not move.
func (s *PlanSvc) RestartQuotaCycle(ctx context.Context, up *ent.UserPlan, window string) error {
	now := time.Now()
	q := s.d.Ent.UserPlan.UpdateOne(up)
	switch window {
	case "all":
		q.SetQuotaAnchor5hAt(now).SetQuotaAnchorWeeklyAt(now).SetQuotaAnchorMonthlyAt(now)
	case "5h":
		q.SetQuotaAnchor5hAt(now)
	case "weekly":
		q.SetQuotaAnchorWeeklyAt(now)
	case "monthly":
		q.SetQuotaAnchorMonthlyAt(now)
	default:
		return ErrInvalidUserPlan
	}
	return q.Exec(ctx)
}

// GrantBankedReset gives up one banked reset of window for its user to spend.
func (s *PlanSvc) GrantBankedReset(ctx context.Context, up *ent.UserPlan, window string, actor uuid.UUID) error {
	return s.d.Ent.PlanBankedReset.Create().
		SetUserPlanID(up.ID).
		SetWindow(planbankedreset.Window(window)).
		SetGrantedBy(actor).
		Exec(ctx)
}

// UnusedBankedResets returns the unspent banked resets of the given plans,
// oldest first, keyed by user plan.
func (s *PlanSvc) UnusedBankedResets(ctx context.Context, userPlanIDs ...uuid.UUID) (map[uuid.UUID][]*ent.PlanBankedReset, error) {
	rows, err := s.d.Ent.PlanBankedReset.Query().
		Where(planbankedreset.UserPlanIDIn(userPlanIDs...), planbankedreset.UsedAtIsNil()).
		Order(ent.Asc(planbankedreset.FieldCreatedAt)).
		All(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[uuid.UUID][]*ent.PlanBankedReset, len(userPlanIDs))
	for _, r := range rows {
		out[r.UserPlanID] = append(out[r.UserPlanID], r)
	}
	return out, nil
}

// UseBankedReset spends one of userID's banked resets. The row stays locked
// until the counters are cleared, so a failed clear leaves it unspent. It runs
// detached from the request: a client that disconnects after the clear would
// otherwise roll back used_at and keep the reset spendable.
func (s *PlanSvc) UseBankedReset(ctx context.Context, userID, id uuid.UUID) error {
	ctx = context.WithoutCancel(ctx)
	tx, err := s.d.Ent.Tx(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	br, err := tx.PlanBankedReset.Query().
		Where(planbankedreset.IDEQ(id), planbankedreset.UsedAtIsNil()).
		ForUpdate().
		Only(ctx)
	if ent.IsNotFound(err) {
		return ErrBankedResetUnavailable
	}
	if err != nil {
		return err
	}
	up, err := resettablePlans(tx.Client(), UserPlanFilter{}).
		Where(entuserplan.IDEQ(br.UserPlanID), entuserplan.UserIDEQ(userID)).
		Only(ctx)
	if ent.IsNotFound(err) {
		return ErrBankedResetUnavailable
	}
	if err != nil {
		return err
	}
	if err := tx.PlanBankedReset.UpdateOne(br).SetUsedAt(time.Now()).Exec(ctx); err != nil {
		return err
	}
	if err := s.ResetQuota(ctx, up, string(br.Window)); err != nil {
		return err
	}
	return tx.Commit()
}

type PlanResetJob struct {
	ID        string    `json:"id"`
	Action    string    `json:"action"`
	Window    string    `json:"window,omitempty"`
	Status    string    `json:"status"` // running | done
	Total     int       `json:"total"`
	Done      int       `json:"done"`
	Failed    int       `json:"failed"`
	Errors    []string  `json:"errors"`
	CreatedBy uuid.UUID `json:"created_by"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// StartPlanReset resolves the selection now, so plans that match the filter
// later are not swept in, and resets them in the background. ids is used
// when filter is nil; either way only resettable plans are kept.
func (s *PlanSvc) StartPlanReset(ctx context.Context, actor uuid.UUID, action, window string, ids []uuid.UUID, filter *UserPlanFilter) (*PlanResetJob, error) {
	switch {
	case action != PlanResetClear && action != PlanResetRestart && action != PlanResetGrant:
		return nil, ErrInvalidUserPlan
	case window != "all" && window != "5h" && window != "weekly" && window != "monthly":
		return nil, ErrInvalidUserPlan
	}
	var q *ent.UserPlanQuery
	if filter != nil {
		q = resettablePlans(s.d.Ent, *filter)
	} else {
		q = resettablePlans(s.d.Ent, UserPlanFilter{}).Where(entuserplan.IDIn(ids...))
	}
	targets, err := q.IDs(ctx)
	if err != nil {
		return nil, err
	}
	if len(targets) == 0 {
		return nil, ErrNoPlansToReset
	}
	now := time.Now()
	job := &PlanResetJob{
		ID: uuid.NewString(), Action: action, Window: window, Status: "running",
		Total: len(targets), Errors: []string{}, CreatedBy: actor, CreatedAt: now, UpdatedAt: now,
	}
	if err := s.savePlanResetJob(ctx, job); err != nil {
		return nil, err
	}
	pipe := s.d.Redis.TxPipeline()
	pipe.LPush(ctx, planResetJobsKey, job.ID)
	pipe.LTrim(ctx, planResetJobsKey, 0, planResetJobsKept-1)
	pipe.Expire(ctx, planResetJobsKey, planResetJobTTL)
	if _, err := pipe.Exec(ctx); err != nil {
		return nil, err
	}
	go s.runPlanReset(*job, targets)
	return job, nil
}

// runPlanReset outlives the request, so it runs on its own context. Progress
// is saved often enough that a job whose instance died shows as a "running"
// job with a stale updated_at; the page reports it as interrupted.
func (s *PlanSvc) runPlanReset(job PlanResetJob, ids []uuid.UUID) {
	ctx := context.Background()
	for _, id := range ids {
		if err := s.resetOne(ctx, job.CreatedBy, job.Action, job.Window, id); err != nil {
			job.Failed++
			if len(job.Errors) < planResetErrors {
				job.Errors = append(job.Errors, fmt.Sprintf("%s: %v", id, err))
			}
		}
		job.Done++
		if job.Done%20 == 0 {
			s.savePlanResetJobLogged(ctx, &job)
		}
	}
	job.Status = "done"
	s.savePlanResetJobLogged(ctx, &job)
}

func (s *PlanSvc) resetOne(ctx context.Context, actor uuid.UUID, action, window string, id uuid.UUID) error {
	up, err := resettablePlans(s.d.Ent, UserPlanFilter{}).Where(entuserplan.IDEQ(id)).Only(ctx)
	if ent.IsNotFound(err) {
		return errors.New("套餐已不再有效")
	}
	if err != nil {
		return err
	}
	switch action {
	case PlanResetRestart:
		return s.RestartQuotaCycle(ctx, up, window)
	case PlanResetGrant:
		return s.GrantBankedReset(ctx, up, window, actor)
	}
	return s.ResetQuota(ctx, up, window)
}

func planResetJobKey(id string) string { return "planreset:job:" + id }

func (s *PlanSvc) savePlanResetJob(ctx context.Context, job *PlanResetJob) error {
	job.UpdatedAt = time.Now()
	b, err := json.Marshal(job)
	if err != nil {
		return err
	}
	return s.d.Redis.Set(ctx, planResetJobKey(job.ID), b, planResetJobTTL).Err()
}

func (s *PlanSvc) savePlanResetJobLogged(ctx context.Context, job *PlanResetJob) {
	if err := s.savePlanResetJob(ctx, job); err != nil {
		slog.Warn("plan reset: save job progress", "job_id", job.ID, "err", err)
	}
}

// PlanResetJobs returns the most recent jobs, newest first.
func (s *PlanSvc) PlanResetJobs(ctx context.Context) ([]PlanResetJob, error) {
	ids, err := s.d.Redis.LRange(ctx, planResetJobsKey, 0, -1).Result()
	if err != nil || len(ids) == 0 {
		return []PlanResetJob{}, err
	}
	keys := make([]string, len(ids))
	for i, id := range ids {
		keys[i] = planResetJobKey(id)
	}
	vals, err := s.d.Redis.MGet(ctx, keys...).Result()
	if err != nil && !errors.Is(err, redis.Nil) {
		return nil, err
	}
	jobs := make([]PlanResetJob, 0, len(vals))
	for _, v := range vals {
		raw, ok := v.(string)
		if !ok {
			continue // expired
		}
		var job PlanResetJob
		if err := json.Unmarshal([]byte(raw), &job); err != nil {
			return nil, err
		}
		jobs = append(jobs, job)
	}
	return jobs, nil
}
