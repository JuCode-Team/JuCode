package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// The no-header and malformed-header paths never touch the organization
// service, so they are testable with a nil svc. The member-check path is
// covered by the end-to-end verification flow (needs Postgres).

func runOrgContext(t *testing.T, header string) (*gin.Context, *httptest.ResponseRecorder, bool) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/v1/me", nil)
	if header != "" {
		c.Request.Header.Set(HeaderOrganizationID, header)
	}
	c.Set(CtxUserID, uuid.New())
	nextCalled := false
	handler := OrgContext(nil)
	// Wrap Next detection: gin's test context runs middleware directly;
	// aborted contexts skip nothing here, so track via IsAborted instead.
	handler(c)
	nextCalled = !c.IsAborted()
	return c, w, nextCalled
}

func TestOrgContextNoHeaderIsPersonal(t *testing.T) {
	c, w, next := runOrgContext(t, "")
	if !next {
		t.Fatalf("request aborted without header, status=%d", w.Code)
	}
	if _, _, ok := OrgContextFrom(c); ok {
		t.Fatal("no header must leave the context in personal scope")
	}
}

func TestOrgContextRejectsMalformedID(t *testing.T) {
	_, w, next := runOrgContext(t, "not-a-uuid")
	if next {
		t.Fatal("malformed org id must abort")
	}
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

func TestOrgContextFromRoundTrip(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	if _, _, ok := OrgContextFrom(c); ok {
		t.Fatal("empty context must not report an org")
	}
	id := uuid.New()
	c.Set(CtxOrgID, id)
	c.Set(CtxOrgRole, "admin")
	orgID, role, ok := OrgContextFrom(c)
	if !ok || orgID != id || role != "admin" {
		t.Fatalf("round trip = (%v, %q, %v)", orgID, role, ok)
	}
}

func TestIsOrgManager(t *testing.T) {
	for role, want := range map[string]bool{"owner": true, "admin": true, "member": false, "": false} {
		if got := IsOrgManager(role); got != want {
			t.Fatalf("IsOrgManager(%q) = %v, want %v", role, got, want)
		}
	}
}
