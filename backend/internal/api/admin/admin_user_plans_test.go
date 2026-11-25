package admin

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jucode/jucode/internal/biz"
	"github.com/jucode/jucode/internal/data"
	"github.com/jucode/jucode/internal/data/ent"
	"github.com/jucode/jucode/internal/middleware"
	"github.com/redis/go-redis/v9"
	"github.com/shopspring/decimal"
)

func TestAdminUserPlansLifecycle(t *testing.T) {
	dsn, addr := os.Getenv("JUCODE_TEST_PG_DSN"), os.Getenv("JUCODE_TEST_REDIS_ADDR")
	if dsn == "" || addr == "" {
		t.Skip("requires test Postgres and Redis")
	}
	ctx := context.Background()
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	client := ent.NewClient(ent.Driver(entsql.OpenDB(dialect.Postgres, db)))
	t.Cleanup(func() { client.Close() })
	if err := client.Schema.Create(ctx); err != nil {
		t.Fatal(err)
	}
	rdb := redis.NewClient(&redis.Options{Addr: addr})
	t.Cleanup(func() { rdb.Close() })
	svc := biz.NewPlanSvc(data.NewWith(client, rdb, db), nil)
	u, err := client.User.Create().SetEmail(uuid.NewString() + "@test.invalid").SetPasswordHash("x").Save(ctx)
	if err != nil {
		t.Fatal(err)
	}
	p, err := client.Plan.Create().SetPrice(decimal.Zero).SetPeriodDays(30).SetName("test").SetQuota5h(decimal.NewFromInt(100)).Save(ctx)
	if err != nil {
		t.Fatal(err)
	}
	p2, err := client.Plan.Create().SetPrice(decimal.Zero).SetPeriodDays(30).SetName("replacement").Save(ctx)
	if err != nil {
		t.Fatal(err)
	}
	payg, err := client.Plan.Create().SetPrice(decimal.Zero).SetPeriodDays(30).SetName("payg").SetType("pay_as_you_go").SetInitialBalance(decimal.NewFromInt(50)).Save(ctx)
	if err != nil {
		t.Fatal(err)
	}
	h := &Handler{users: client, rawDB: db, plans: svc}
	gin.SetMode(gin.TestMode)
	router := gin.New()
	role := "admin"
	group := router.Group("/admin", func(c *gin.Context) { c.Set(middleware.CtxRole, role) }, middleware.AdminOnly())
	h.Mount(group)
	call := func(method, path, body string, want int) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(method, "/admin/users/"+path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != want {
			t.Fatalf("%s %s: got %d want %d: %s", method, path, rec.Code, want, rec.Body.String())
		}
		return rec
	}
	uid := u.ID.String()
	role = "user"
	call("GET", uid+"/plans", "", 403)
	call("POST", uid+"/plans", `{}`, 403)
	role = "admin"
	h.SetStepUpGuard(func(c *gin.Context) { c.AbortWithStatus(403) })
	// Mount captured the original guard; verify the protected routes in a fresh router.
	guarded := gin.New()
	h.Mount(guarded.Group("/admin"))
	for _, method := range []string{"POST", "PATCH", "DELETE"} {
		path := "/admin/users/" + uid + "/plans"
		if method != "POST" {
			path += "/" + uuid.NewString()
		}
		rec := httptest.NewRecorder()
		guarded.ServeHTTP(rec, httptest.NewRequest(method, path, strings.NewReader(`{}`)))
		if rec.Code != 403 {
			t.Fatalf("step-up bypass for %s: %d", method, rec.Code)
		}
	}
	call("POST", uid+"/plans", `{}`, 400)
	call("POST", uid+"/plans", `{"request_id":"`+uuid.NewString()+`","plan_id":"`+p.ID.String()+`","expire_at":"2000-01-01T00:00:00Z"}`, 400)
	rec := call("POST", uid+"/plans", `{"request_id":"`+uuid.NewString()+`","plan_id":"`+p.ID.String()+`"}`, 201)
	var created struct {
		ID uuid.UUID `json:"user_plan_id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	path := uid + "/plans/" + created.ID.String()
	up, err := client.UserPlan.Get(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := svc.ConsumeQuotaOnce(ctx, up, p, decimal.NewFromInt(25), "test-settlement"); err != nil {
		t.Fatal(err)
	}
	_, err = client.UsageLog.Create().SetUserID(u.ID).SetUserPlanID(up.ID).SetModel("test").SetEndpoint("chat_completions").SetStatus("ok").SetBillingSource("plan").SetCostFinal(decimal.NewFromInt(25)).Save(ctx)
	if err != nil {
		t.Fatal(err)
	}
	read := func() map[string]any {
		t.Helper()
		r := call("GET", uid+"/plans", "", 200)
		var result struct {
			Plans []map[string]any `json:"plans"`
		}
		if err := json.Unmarshal(r.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		for _, item := range result.Plans {
			if item["user_plan_id"] == up.ID.String() {
				return item
			}
		}
		t.Fatal("missing entitlement")
		return nil
	}
	row := read()
	if row["used_5h"] != "25" || row["total_cost"] != "25" || row["requests"] != float64(1) {
		t.Fatalf("usage: %+v", row)
	}
	other := uuid.NewString() + "/plans/" + up.ID.String()
	call("PATCH", other, `{"plan_id":"`+p2.ID.String()+`"}`, 404)
	call("DELETE", other, "", 404)
	call("POST", other+"/reset-quota", `{"window":"all"}`, 404)
	call("PATCH", path, `{}`, 400)
	call("PATCH", path, `{"plan_id":"`+payg.ID.String()+`"}`, 400)
	call("PATCH", path, `{"plan_id":"`+p2.ID.String()+`","expire_at":"`+time.Now().Add(48*time.Hour).UTC().Format(time.RFC3339)+`"}`, 200)
	row = read()
	if row["plan_id"] != p2.ID.String() || row["used_5h"] != "25" {
		t.Fatalf("update lost usage: %+v", row)
	}
	call("POST", path+"/reset-quota", `{"window":"bogus"}`, 400)
	call("POST", path+"/reset-quota", `{"window":"5h"}`, 204)
	row = read()
	if row["used_5h"] != "0" || row["used_weekly"] != "25" || row["total_cost"] != "25" {
		t.Fatalf("partial reset: %+v", row)
	}
	call("POST", path+"/reset-quota", `{"window":"all"}`, 204)
	if _, consumed, err := svc.ConsumeQuotaOnce(ctx, up, p, decimal.NewFromInt(25), "test-settlement"); err != nil || consumed {
		t.Fatalf("reset removed idempotency: %v, %v", consumed, err)
	}
	row = read()
	if row["used_monthly"] != "0" || row["total_cost"] != "25" {
		t.Fatalf("full reset: %+v", row)
	}
	call("DELETE", path, "", 204)
	call("DELETE", path, "", 204)
	call("PATCH", path, `{"request_id":"`+uuid.NewString()+`","plan_id":"`+p.ID.String()+`"}`, 400)
	call("POST", path+"/reset-quota", `{"window":"all"}`, 400)
	if row := read(); row["status"] != "canceled" || row["current"] != false || row["total_cost"] != "25" {
		t.Fatalf("delete: %+v", row)
	}
	grant := `{"request_id":"` + uuid.NewString() + `","plan_id":"` + payg.ID.String() + `"}`
	call("POST", uid+"/plans", grant, 201)
	call("POST", uid+"/plans", grant, 201)
	u, err = client.User.Get(ctx, u.ID)
	if err != nil || !u.Balance.Equal(decimal.NewFromInt(50)) {
		t.Fatalf("initial gift not credited: %v %v", u, err)
	}
}
