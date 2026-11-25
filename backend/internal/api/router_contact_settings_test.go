package api

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"image"
	"image/png"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/alicebob/miniredis/v2"
	"github.com/gin-gonic/gin"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/redis/go-redis/v9"

	"github.com/jucode/jucode/internal/biz"
	"github.com/jucode/jucode/internal/cache"
	"github.com/jucode/jucode/internal/data"
	"github.com/jucode/jucode/internal/data/ent"
	entsetting "github.com/jucode/jucode/internal/data/ent/sitesetting"
	jwtpkg "github.com/jucode/jucode/internal/pkg/jwt"
)

// TestAdminContactSettingsAndSiteAssets covers the admin write -> public read
// path for the contact dock settings and the QR image upload -> public fetch.
func TestAdminContactSettingsAndSiteAssets(t *testing.T) {
	dsn := os.Getenv("JUCODE_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("JUCODE_TEST_PG_DSN not set; skipping Postgres-backed settings route test")
	}
	ctx := context.Background()
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open postgres: %v", err)
	}
	client := ent.NewClient(ent.Driver(entsql.OpenDB(dialect.Postgres, db)))
	if err := client.Schema.Create(ctx); err != nil {
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
	signer := jwtpkg.NewSigner("contact-route-test-secret-32-bytes!", time.Hour, 24*time.Hour)
	adminUser, err := client.User.Create().
		SetEmail("contact-settings-admin@test.invalid").
		SetPasswordHash("unused").
		SetRole("admin").
		Save(ctx)
	if err != nil {
		t.Fatalf("create admin: %v", err)
	}
	t.Cleanup(func() { _ = client.User.DeleteOneID(adminUser.ID).Exec(ctx) })
	token, _, _, err := signer.IssueAccess(adminUser.ID, "admin")
	if err != nil {
		t.Fatalf("issue admin token: %v", err)
	}
	assetDir := t.TempDir()
	router := NewRouter(Deps{
		Data:         d,
		Signer:       signer,
		Account:      biz.NewAccount(d, signer, settings),
		Settings:     settings,
		Audit:        biz.NewAuditSvc(d),
		Cache:        cache.New(rdb),
		SiteAssetDir: assetDir,
	})
	do := func(req *http.Request) *httptest.ResponseRecorder {
		req.Header.Set("Authorization", "Bearer "+token)
		res := httptest.NewRecorder()
		router.ServeHTTP(res, req)
		return res
	}

	// Upload the QR code.
	var img bytes.Buffer
	if err := png.Encode(&img, image.NewRGBA(image.Rect(0, 0, 4, 4))); err != nil {
		t.Fatalf("encode png: %v", err)
	}
	var form bytes.Buffer
	mw := multipart.NewWriter(&form)
	fw, _ := mw.CreateFormFile("file", "qr.png")
	_, _ = fw.Write(img.Bytes())
	_ = mw.Close()
	upReq := httptest.NewRequest(http.MethodPost, "/v1/admin/site-assets", &form)
	upReq.Header.Set("Content-Type", mw.FormDataContentType())
	upRes := do(upReq)
	if upRes.Code != http.StatusOK {
		t.Fatalf("upload: status=%d body=%s", upRes.Code, upRes.Body.String())
	}
	var uploaded struct {
		URL string `json:"url"`
	}
	_ = json.Unmarshal(upRes.Body.Bytes(), &uploaded)
	getRes := httptest.NewRecorder()
	router.ServeHTTP(getRes, httptest.NewRequest(http.MethodGet, uploaded.URL, nil))
	if getRes.Code != http.StatusOK || !bytes.Equal(getRes.Body.Bytes(), img.Bytes()) {
		t.Fatalf("fetch %s: status=%d len=%d", uploaded.URL, getRes.Code, getRes.Body.Len())
	}

	// Save the contact settings.
	current, err := settings.GetSite(ctx)
	if err != nil {
		t.Fatalf("load settings: %v", err)
	}
	current.ContactGroup = "123456789"
	current.ContactGroupQRURL = uploaded.URL
	current.ContactDockEnabled = true
	body, _ := json.Marshal(current)
	putReq := httptest.NewRequest(http.MethodPut, "/v1/admin/settings", bytes.NewReader(body))
	putReq.Header.Set("Content-Type", "application/json")
	if res := do(putReq); res.Code != http.StatusOK {
		t.Fatalf("update settings: status=%d body=%s", res.Code, res.Body.String())
	}
	t.Cleanup(func() {
		_, _ = client.SiteSetting.Delete().Where(entsetting.KeyIn("contact_group", "contact_group_label", "contact_group_qr_url", "contact_dock_enabled")).Exec(ctx)
	})
	for key, want := range map[string]string{
		"contact_group":        "123456789",
		"contact_group_label":  "QQ 群",
		"contact_group_qr_url": uploaded.URL,
		"contact_dock_enabled": "true",
	} {
		row, err := client.SiteSetting.Query().Where(entsetting.KeyEQ(key)).Only(ctx)
		if err != nil || row.Value != want {
			t.Fatalf("stored %s = %v (err %v), want %q", key, row, err, want)
		}
	}

	pubRes := httptest.NewRecorder()
	router.ServeHTTP(pubRes, httptest.NewRequest(http.MethodGet, "/v1/public/site-info", nil))
	var pub struct {
		Group   string `json:"contact_group"`
		QR      string `json:"contact_group_qr_url"`
		Enabled bool   `json:"contact_dock_enabled"`
	}
	if err := json.Unmarshal(pubRes.Body.Bytes(), &pub); err != nil {
		t.Fatalf("decode site-info: %v", err)
	}
	if pub.Group != "123456789" || pub.QR != uploaded.URL || !pub.Enabled {
		t.Fatalf("public site-info contact = %+v", pub)
	}

	// Invalid group is a 400, not a storage error.
	current.ContactGroup = "12\nab"
	body, _ = json.Marshal(current)
	badReq := httptest.NewRequest(http.MethodPut, "/v1/admin/settings", bytes.NewReader(body))
	badReq.Header.Set("Content-Type", "application/json")
	if res := do(badReq); res.Code != http.StatusBadRequest {
		t.Fatalf("invalid group: status=%d body=%s", res.Code, res.Body.String())
	}
}

func TestPublicSiteAssetsOnlyServesUploadedNames(t *testing.T) {
	gin.SetMode(gin.TestMode)
	dir := t.TempDir()
	const name = "0123456789abcdef.png"
	if err := os.WriteFile(dir+"/"+name, []byte("png"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dir+"/secret.txt", []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := gin.New()
	mountSiteAssets(r.Group("/v1"), dir)

	res := httptest.NewRecorder()
	r.ServeHTTP(res, httptest.NewRequest(http.MethodGet, "/v1/public/site-assets/"+name, nil))
	if res.Code != http.StatusOK {
		t.Fatalf("asset status = %d", res.Code)
	}
	if got := res.Header().Get("Cache-Control"); got != "public, max-age=86400" {
		t.Fatalf("Cache-Control = %q", got)
	}
	for _, path := range []string{
		"/v1/public/site-assets/secret.txt",
		"/v1/public/site-assets/..%2Fsecret.txt",
		"/v1/public/site-assets/%2E%2E%2Fetc%2Fpasswd",
		"/v1/public/site-assets/fedcba9876543210.png",
	} {
		res := httptest.NewRecorder()
		r.ServeHTTP(res, httptest.NewRequest(http.MethodGet, path, nil))
		if res.Code != http.StatusNotFound {
			t.Fatalf("%s status = %d, want 404", path, res.Code)
		}
	}
}
