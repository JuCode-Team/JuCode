package user

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/jucode/jucode/internal/middleware"
)

func scopeContext(t *testing.T, uid uuid.UUID, url string) (*gin.Context, *httptest.ResponseRecorder) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, url, nil)
	if uid != uuid.Nil {
		c.Set(middleware.CtxUserID, uid)
	}
	return c, w
}

// Personal scope: pinned to the caller AND excludes org-billed rows —
// this is what keeps org spending out of the personal dashboard. The
// OAuth device read routes hit the same branch (they never carry an org
// context), so this doubles as the "no context defaults to personal"
// regression test.
func TestStatsScopePersonal(t *testing.T) {
	uid := uuid.New()
	c, _ := scopeContext(t, uid, "/v1/me/stats/usage")
	h := &Handler{}
	sc, ok := h.statsScopeFrom(c)
	if !ok {
		t.Fatal("personal scope must resolve")
	}
	if sc.UserID == nil || *sc.UserID != uid || !sc.PersonalOnly || sc.OrganizationID != nil || sc.Manager {
		t.Fatalf("unexpected scope %+v", sc)
	}
	if sc.CacheKey != uid.String() {
		t.Fatalf("cache key %q", sc.CacheKey)
	}
}

func TestStatsScopeOrgMemberIsSelfOnly(t *testing.T) {
	uid, orgID := uuid.New(), uuid.New()
	c, _ := scopeContext(t, uid, "/v1/me/stats/usage")
	c.Set(middleware.CtxOrgID, orgID)
	c.Set(middleware.CtxOrgRole, "member")
	h := &Handler{}
	sc, ok := h.statsScopeFrom(c)
	if !ok {
		t.Fatal("member scope must resolve")
	}
	if sc.OrganizationID == nil || *sc.OrganizationID != orgID {
		t.Fatalf("member scope missing org: %+v", sc)
	}
	if sc.UserID == nil || *sc.UserID != uid {
		t.Fatal("member scope must stay pinned to the caller's own rows")
	}
	if sc.Manager || sc.PersonalOnly {
		t.Fatalf("unexpected flags %+v", sc)
	}
	want := "org:" + orgID.String() + ":u:" + uid.String()
	if sc.CacheKey != want {
		t.Fatalf("cache key %q, want %q", sc.CacheKey, want)
	}
}

func TestStatsScopeOrgManagerSeesWholeOrg(t *testing.T) {
	uid, orgID := uuid.New(), uuid.New()
	for _, role := range []string{"owner", "admin"} {
		c, _ := scopeContext(t, uid, "/v1/me/stats/usage")
		c.Set(middleware.CtxOrgID, orgID)
		c.Set(middleware.CtxOrgRole, role)
		h := &Handler{}
		sc, ok := h.statsScopeFrom(c)
		if !ok {
			t.Fatalf("%s scope must resolve", role)
		}
		if sc.UserID != nil {
			t.Fatalf("%s must not be user-pinned: %+v", role, sc)
		}
		if !sc.Manager || sc.OrganizationID == nil || *sc.OrganizationID != orgID {
			t.Fatalf("%s scope wrong: %+v", role, sc)
		}
		if sc.CacheKey != "org:"+orgID.String() {
			t.Fatalf("cache key %q", sc.CacheKey)
		}
	}
}

// member_user_id narrows the manager's org-wide view to one member and
// MUST be reflected in the cache key — the lifetime endpoint caches on
// the scope key alone, so a missing segment would leak the filtered
// numbers to the unfiltered view (and vice versa).
func TestStatsScopeManagerMemberFilter(t *testing.T) {
	uid, orgID, target := uuid.New(), uuid.New(), uuid.New()
	c, _ := scopeContext(t, uid, "/v1/me/stats/usage?member_user_id="+target.String())
	c.Set(middleware.CtxOrgID, orgID)
	c.Set(middleware.CtxOrgRole, "owner")
	h := &Handler{}
	sc, ok := h.statsScopeFrom(c)
	if !ok {
		t.Fatal("manager member-filter scope must resolve")
	}
	if sc.UserID == nil || *sc.UserID != target {
		t.Fatalf("member filter not applied: %+v", sc)
	}
	want := "org:" + orgID.String() + ":m:" + target.String()
	if sc.CacheKey != want {
		t.Fatalf("cache key %q, want %q", sc.CacheKey, want)
	}
}

// A plain member must not be able to smuggle the manager-only
// member_user_id filter — the scope stays pinned to the caller.
func TestStatsScopeMemberIgnoresMemberFilter(t *testing.T) {
	uid, orgID, target := uuid.New(), uuid.New(), uuid.New()
	c, _ := scopeContext(t, uid, "/v1/me/stats/usage?member_user_id="+target.String())
	c.Set(middleware.CtxOrgID, orgID)
	c.Set(middleware.CtxOrgRole, "member")
	h := &Handler{}
	sc, ok := h.statsScopeFrom(c)
	if !ok {
		t.Fatal("member scope must resolve")
	}
	if sc.UserID == nil || *sc.UserID != uid {
		t.Fatalf("member filter must be ignored for non-managers: %+v", sc)
	}
}

func TestStatsScopeManagerRejectsBadMemberFilter(t *testing.T) {
	uid, orgID := uuid.New(), uuid.New()
	c, w := scopeContext(t, uid, "/v1/me/stats/usage?member_user_id=nope")
	c.Set(middleware.CtxOrgID, orgID)
	c.Set(middleware.CtxOrgRole, "admin")
	h := &Handler{}
	if _, ok := h.statsScopeFrom(c); ok {
		t.Fatal("malformed member_user_id must fail")
	}
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

// Regression: /v1/me/* authenticates with middleware.JWT, which sets
// CtxUserID and NOT CtxUser. handleMyRate originally read CtxUser via
// middleware.UserFrom, so it returned 401 for every browser session — and
// because the widget polls every 5s, the frontend's global 401 interceptor
// signed users out moments after they opened the usage page.
//
// The identity lookup must therefore accept a context carrying only
// CtxUserID. This asserts the accessor contract rather than the full handler,
// which needs a database.
func TestMyRateIdentityComesFromCtxUserID(t *testing.T) {
	uid := uuid.New()
	c, _ := scopeContext(t, uid, "/v1/me/stats/rate")

	got, ok := middleware.UserIDFrom(c)
	if !ok || got != uid {
		t.Fatalf("UserIDFrom = (%v, %v); handler would reject an authenticated session", got, ok)
	}
	if _, ok := middleware.UserFrom(c); ok {
		t.Fatal("CtxUser is set on a JWT-only context; the premise of this regression no longer holds")
	}
}
