package biz

import (
	"context"
	"database/sql"
	"os"
	"testing"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/shopspring/decimal"

	"github.com/jucode/jucode/internal/data"
	"github.com/jucode/jucode/internal/data/ent"
	entplan "github.com/jucode/jucode/internal/data/ent/plan"
	entuser "github.com/jucode/jucode/internal/data/ent/user"
	entuserplan "github.com/jucode/jucode/internal/data/ent/userplan"
)

// Needs Postgres and Redis:
//
//	JUCODE_TEST_PG_DSN='postgres://postgres:test@127.0.0.1:5499/jucode_test?sslmode=disable' \
//	JUCODE_TEST_REDIS_ADDR=127.0.0.1:6399 go test ./internal/biz -run PlanBatchReset
func TestPlanBatchReset(t *testing.T) {
	dsn, addr := os.Getenv("JUCODE_TEST_PG_DSN"), os.Getenv("JUCODE_TEST_REDIS_ADDR")
	if dsn == "" || addr == "" {
		t.Skip("JUCODE_TEST_PG_DSN or JUCODE_TEST_REDIS_ADDR not set")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	client := ent.NewClient(ent.Driver(entsql.OpenDB(dialect.Postgres, db)))
	ctx := context.Background()
	if err := client.Schema.Create(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	rdb := redis.NewClient(&redis.Options{Addr: addr})
	t.Cleanup(func() { _ = client.Close(); _ = rdb.Close() })
	svc := NewPlanSvc(data.NewWith(client, rdb, db), nil)

	token := uuid.NewString()[:8]
	user := func(name string) *ent.User {
		u, err := client.User.Create().SetEmail(token + "-" + name + "@test.invalid").SetPasswordHash("x").SetStatus(entuser.StatusActive).Save(ctx)
		if err != nil {
			t.Fatalf("create user: %v", err)
		}
		return u
	}
	plan := func(typ entplan.Type) *ent.Plan {
		p, err := client.Plan.Create().SetName("batch-reset-" + uuid.NewString()).SetPrice(decimal.NewFromInt(10)).SetType(typ).
			SetQuota5h(decimal.NewFromInt(100)).SetQuotaWeekly(decimal.NewFromInt(100)).SetQuotaMonthly(decimal.NewFromInt(100)).Save(ctx)
		if err != nil {
			t.Fatalf("create plan: %v", err)
		}
		return p
	}
	now := time.Now()
	userPlan := func(u *ent.User, p *ent.Plan, expire time.Time, status entuserplan.Status) *ent.UserPlan {
		up, err := client.UserPlan.Create().SetUserID(u.ID).SetPlanID(p.ID).SetStartAt(now.Add(-240 * time.Hour)).
			SetExpireAt(expire).SetStatus(status).Save(ctx)
		if err != nil {
			t.Fatalf("create user plan: %v", err)
		}
		t.Cleanup(func() { _ = rdb.Del(ctx, quotaKeys(up, time.Now())...).Err() })
		return up
	}
	a, b := user("a"), user("b")
	sub, paygo := plan(entplan.TypeQuotaSubscription), plan(entplan.TypePayAsYouGo)
	upA := userPlan(a, sub, now.Add(480*time.Hour), entuserplan.StatusActive)
	upB := userPlan(b, sub, now.Add(480*time.Hour), entuserplan.StatusActive)
	expired := userPlan(a, sub, now.Add(-time.Hour), entuserplan.StatusActive)
	userPlan(a, paygo, now.Add(480*time.Hour), entuserplan.StatusActive)
	userPlan(a, sub, now.Add(480*time.Hour), entuserplan.StatusCanceled)

	// Only active, unexpired subscriptions are listed and resettable.
	ups, total, err := svc.ListResettableUserPlans(ctx, UserPlanFilter{Query: token}, 50, 0)
	if err != nil || total != 2 || len(ups) != 2 {
		t.Fatalf("list = %d rows, total %d, err %v; want upA and upB", len(ups), total, err)
	}

	for _, up := range []*ent.UserPlan{upA, upB} {
		if _, err := svc.ConsumeQuota(ctx, up, sub, decimal.NewFromInt(7)); err != nil {
			t.Fatalf("consume: %v", err)
		}
	}
	wait := func(job *PlanResetJob) PlanResetJob {
		t.Helper()
		for i := 0; i < 100; i++ {
			jobs, err := svc.PlanResetJobs(ctx)
			if err != nil {
				t.Fatalf("jobs: %v", err)
			}
			for _, j := range jobs {
				if j.ID == job.ID && j.Status == "done" {
					return j
				}
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatalf("job %s did not finish", job.ID)
		return PlanResetJob{}
	}

	job, err := svc.StartPlanReset(ctx, a.ID, PlanResetClear, "weekly", nil, &UserPlanFilter{Query: token})
	if err != nil {
		t.Fatalf("start clear: %v", err)
	}
	if done := wait(job); done.Total != 2 || done.Done != 2 || done.Failed != 0 {
		t.Fatalf("clear job = %+v, want 2 done, 0 failed", done)
	}
	for _, up := range []*ent.UserPlan{upA, upB} {
		v, _ := svc.PeekQuota(ctx, up)
		if !v.UsedWeekly.IsZero() || !v.Used5h.Equal(decimal.NewFromInt(7)) {
			t.Errorf("after clear weekly: %+v, want weekly 0 and 5h untouched", v)
		}
	}

	// Ineligible ids are dropped at submit time. Bringing the weekly reset
	// forward moves only that window's schedule.
	job, err = svc.StartPlanReset(ctx, a.ID, PlanResetRestart, "weekly", []uuid.UUID{upA.ID, expired.ID}, nil)
	if err != nil {
		t.Fatalf("start restart: %v", err)
	}
	if done := wait(job); done.Total != 1 || done.Failed != 0 {
		t.Fatalf("restart job = %+v, want 1 plan", done)
	}
	restarted, err := client.UserPlan.Get(ctx, upA.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if restarted.QuotaAnchorWeeklyAt == nil || time.Since(*restarted.QuotaAnchorWeeklyAt) > time.Minute || !restarted.StartAt.Equal(upA.StartAt) {
		t.Fatalf("weekly anchor %v start %v, want anchor now and start unchanged", restarted.QuotaAnchorWeeklyAt, restarted.StartAt)
	}
	if restarted.QuotaAnchor5hAt != nil || restarted.QuotaAnchorMonthlyAt != nil {
		t.Fatalf("5h anchor %v monthly anchor %v, want untouched", restarted.QuotaAnchor5hAt, restarted.QuotaAnchorMonthlyAt)
	}
	w := QuotaWindowsAt(restarted, time.Now())
	if !w[1].End.Equal(restarted.QuotaAnchorWeeklyAt.Add(7 * 24 * time.Hour)) {
		t.Errorf("weekly window ends %s, want 7 days after the restart", w[1].End)
	}
	if !w[2].End.Equal(upA.StartAt.Add(30 * 24 * time.Hour)) {
		t.Errorf("monthly window ends %s, want 30 days after start_at", w[2].End)
	}
	if v, _ := svc.PeekQuota(ctx, restarted); !v.UsedWeekly.IsZero() || !v.Used5h.Equal(decimal.NewFromInt(7)) || !v.UsedMonthly.Equal(decimal.NewFromInt(7)) {
		t.Errorf("after weekly restart: %+v, want weekly empty, 5h and monthly kept", v)
	}
	// A request priced before the restart settles into the old cycle (upA is
	// in its second week). Neither now nor once the new cycle reaches its own
	// second week may that usage show up.
	if _, err := svc.ConsumeQuota(ctx, upA, sub, decimal.NewFromInt(9)); err != nil {
		t.Fatalf("stale consume: %v", err)
	}
	later := restarted.QuotaAnchorWeeklyAt.Add(7*24*time.Hour + time.Hour)
	if QuotaWindowsAt(restarted, later)[1].Key == QuotaWindowsAt(upA, time.Now())[1].Key {
		t.Errorf("restarted week 2 reuses the old cycle's week-2 counter")
	}
	if v, _ := svc.PeekQuota(ctx, restarted); !v.UsedWeekly.IsZero() {
		t.Errorf("stale settle leaked into the restarted cycle: %+v", v)
	}

	job, err = svc.StartPlanReset(ctx, a.ID, PlanResetRestart, "all", []uuid.UUID{upB.ID}, nil)
	if err != nil {
		t.Fatalf("start restart all: %v", err)
	}
	wait(job)
	all, err := client.UserPlan.Get(ctx, upB.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if all.QuotaAnchor5hAt == nil || all.QuotaAnchorWeeklyAt == nil || all.QuotaAnchorMonthlyAt == nil ||
		!all.QuotaAnchor5hAt.Equal(*all.QuotaAnchorMonthlyAt) || !all.QuotaAnchorWeeklyAt.Equal(*all.QuotaAnchorMonthlyAt) {
		t.Fatalf("restart all anchors = %v %v %v, want one shared anchor", all.QuotaAnchor5hAt, all.QuotaAnchorWeeklyAt, all.QuotaAnchorMonthlyAt)
	}
	if v, _ := svc.PeekQuota(ctx, all); !v.Used5h.IsZero() || !v.UsedMonthly.IsZero() {
		t.Errorf("after restart all: %+v, want all windows empty", v)
	}

	// A granted reset waits until its user spends it, and clears only its window.
	job, err = svc.StartPlanReset(ctx, a.ID, PlanResetGrant, "5h", []uuid.UUID{upA.ID}, nil)
	if err != nil {
		t.Fatalf("start grant: %v", err)
	}
	wait(job)
	banked, err := svc.UnusedBankedResets(ctx, upA.ID)
	if err != nil || len(banked[upA.ID]) != 1 || banked[upA.ID][0].Window != "5h" {
		t.Fatalf("banked = %+v, err %v; want one 5h reset", banked, err)
	}
	br := banked[upA.ID][0]
	if v, _ := svc.PeekQuota(ctx, restarted); !v.Used5h.Equal(decimal.NewFromInt(16)) {
		t.Fatalf("granting cleared quota: %+v", v)
	}
	if err := svc.UseBankedReset(ctx, b.ID, br.ID); err != ErrBankedResetUnavailable {
		t.Errorf("another user spending it err = %v", err)
	}
	if err := svc.UseBankedReset(ctx, a.ID, br.ID); err != nil {
		t.Fatalf("use: %v", err)
	}
	if v, _ := svc.PeekQuota(ctx, restarted); !v.Used5h.IsZero() || !v.UsedMonthly.Equal(decimal.NewFromInt(16)) {
		t.Errorf("after use: %+v, want 5h empty and monthly kept", v)
	}
	if err := svc.UseBankedReset(ctx, a.ID, br.ID); err != ErrBankedResetUnavailable {
		t.Errorf("spending it twice err = %v", err)
	}
	if banked, _ := svc.UnusedBankedResets(ctx, upA.ID); len(banked[upA.ID]) != 0 {
		t.Errorf("spent reset still listed: %+v", banked)
	}
	// A reset on a plan that is no longer active is void.
	void, err := client.PlanBankedReset.Create().SetUserPlanID(expired.ID).SetWindow("all").SetGrantedBy(a.ID).Save(ctx)
	if err != nil {
		t.Fatalf("create void reset: %v", err)
	}
	if err := svc.UseBankedReset(ctx, a.ID, void.ID); err != ErrBankedResetUnavailable {
		t.Errorf("reset on expired plan err = %v", err)
	}

	if _, err := svc.StartPlanReset(ctx, a.ID, "wipe", "all", []uuid.UUID{upA.ID}, nil); err != ErrInvalidUserPlan {
		t.Errorf("unknown action err = %v", err)
	}
	if _, err := svc.StartPlanReset(ctx, a.ID, PlanResetRestart, "", []uuid.UUID{upA.ID}, nil); err != ErrInvalidUserPlan {
		t.Errorf("missing window err = %v", err)
	}
	if _, err := svc.StartPlanReset(ctx, a.ID, PlanResetClear, "all", []uuid.UUID{expired.ID}, nil); err != ErrNoPlansToReset {
		t.Errorf("nothing eligible err = %v", err)
	}
}
