package api

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/alicebob/miniredis/v2"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/redis/go-redis/v9"

	"github.com/jucode/jucode/internal/biz"
	"github.com/jucode/jucode/internal/cache"
	"github.com/jucode/jucode/internal/data"
	"github.com/jucode/jucode/internal/data/ent"
	entsetting "github.com/jucode/jucode/internal/data/ent/sitesetting"
	jwtpkg "github.com/jucode/jucode/internal/pkg/jwt"
)

func TestAdminSettingsPublishesMarketingBanner(t *testing.T) {
	dsn := os.Getenv("JUCODE_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("JUCODE_TEST_PG_DSN not set; skipping Postgres-backed settings route test")
	}

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open postgres: %v", err)
	}
	client := ent.NewClient(ent.Driver(entsql.OpenDB(dialect.Postgres, db)))
	if err := client.Schema.Create(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(func() {
		_ = client.Close()
		_ = db.Close()
	})

	redisServer := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: redisServer.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	d := data.NewWith(client, rdb, db)
	settings := biz.NewSettingsSvc(d)
	cacheSvc := cache.New(rdb)
	signer := jwtpkg.NewSigner("settings-route-test-secret-32-bytes", time.Hour, 24*time.Hour)
	account := biz.NewAccount(d, signer, settings)
	audit := biz.NewAuditSvc(d)

	adminUser, err := client.User.Create().
		SetEmail("marketing-banner-admin@test.invalid").
		SetPasswordHash("unused").
		SetRole("admin").
		Save(context.Background())
	if err != nil {
		t.Fatalf("create admin: %v", err)
	}
	t.Cleanup(func() { _ = client.User.DeleteOneID(adminUser.ID).Exec(context.Background()) })

	current, err := settings.GetSite(context.Background())
	if err != nil {
		t.Fatalf("load settings: %v", err)
	}
	current.MarketingBanner = biz.MarketingBannerSettings{}
	if err := settings.UpdateSite(context.Background(), *current); err != nil {
		t.Fatalf("seed settings: %v", err)
	}
	stale, err := json.Marshal(current)
	if err != nil {
		t.Fatalf("marshal stale settings: %v", err)
	}
	if err := rdb.Set(context.Background(), cache.KeySiteInfo, stale, time.Minute).Err(); err != nil {
		t.Fatalf("seed site-info cache: %v", err)
	}

	current.MarketingBanner = biz.MarketingBannerSettings{
		Enabled: true,
		Text:    "Ship with JuCode",
		LinkURL: "https://jucode.example/launch",
	}
	body, err := json.Marshal(current)
	if err != nil {
		t.Fatalf("marshal settings update: %v", err)
	}
	token, _, _, err := signer.IssueAccess(adminUser.ID, "admin")
	if err != nil {
		t.Fatalf("issue admin token: %v", err)
	}

	router := NewRouter(Deps{
		Data:     d,
		Signer:   signer,
		Account:  account,
		Settings: settings,
		Audit:    audit,
		Cache:    cacheSvc,
	})
	updateReq := httptest.NewRequest(http.MethodPut, "/v1/admin/settings", bytes.NewReader(body))
	updateReq.Header.Set("Authorization", "Bearer "+token)
	updateReq.Header.Set("Content-Type", "application/json")
	updateRes := httptest.NewRecorder()
	router.ServeHTTP(updateRes, updateReq)
	if updateRes.Code != http.StatusOK {
		t.Fatalf("update settings: status=%d body=%s", updateRes.Code, updateRes.Body.String())
	}
	if rdb.Exists(context.Background(), cache.KeySiteInfo).Val() != 0 {
		t.Fatal("site-info cache was not invalidated")
	}
	stored := map[string]string{
		"marketing_banner_enabled":  "true",
		"marketing_banner_text":     current.MarketingBanner.Text,
		"marketing_banner_link_url": current.MarketingBanner.LinkURL,
	}
	for key, want := range stored {
		row, err := client.SiteSetting.Query().Where(entsetting.KeyEQ(key)).Only(context.Background())
		if err != nil {
			t.Fatalf("read stored %s: %v", key, err)
		}
		if row.Value != want {
			t.Fatalf("stored %s = %q, want %q", key, row.Value, want)
		}
	}

	publicReq := httptest.NewRequest(http.MethodGet, "/v1/public/site-info", nil)
	publicRes := httptest.NewRecorder()
	router.ServeHTTP(publicRes, publicReq)
	if publicRes.Code != http.StatusOK {
		t.Fatalf("get public site info: status=%d body=%s", publicRes.Code, publicRes.Body.String())
	}
	if got := publicRes.Header().Get("X-Cache"); got != "MISS" {
		t.Fatalf("X-Cache = %q, want MISS", got)
	}
	var payload struct {
		MarketingBanner biz.MarketingBannerSettings `json:"marketing_banner"`
	}
	if err := json.Unmarshal(publicRes.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode public site info: %v", err)
	}
	if payload.MarketingBanner != current.MarketingBanner {
		t.Fatalf("marketing_banner = %+v, want %+v", payload.MarketingBanner, current.MarketingBanner)
	}
}
