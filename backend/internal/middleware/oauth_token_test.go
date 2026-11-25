package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/jucode/jucode/internal/pkg/jwt"
)

// Browsers reach the relay with the token as a WebSocket subprotocol; only
// a WebSocket upgrade on a relay route may authenticate that way.
func TestOAuthTokenFromRelaySubprotocol(t *testing.T) {
	gin.SetMode(gin.TestMode)
	var got string
	r := gin.New()
	capture := func(c *gin.Context) { got = OAuthToken(c) }
	r.GET("/v1/relay/connect", capture)
	r.GET("/v1/oauth/computers", capture)

	upgrade := http.Header{
		"Connection":             {"Upgrade"},
		"Upgrade":                {"websocket"},
		"Sec-Websocket-Protocol": {"jucode-relay, jucode-bearer.tok123"},
	}
	for _, tc := range []struct {
		path   string
		header http.Header
		want   string
	}{
		{"/v1/relay/connect", upgrade, "tok123"},
		{"/v1/relay/connect", http.Header{"Authorization": {"Bearer hdr"}, "Sec-Websocket-Protocol": {"jucode-bearer.tok123"}}, "hdr"},
		{"/v1/relay/connect", http.Header{"Sec-Websocket-Protocol": {"jucode-bearer.tok123"}}, ""}, // not an upgrade
		{"/v1/oauth/computers", upgrade, ""}, // not a relay route
	} {
		got = "unset"
		req := httptest.NewRequest(http.MethodGet, tc.path, nil)
		req.Header = tc.header
		r.ServeHTTP(httptest.NewRecorder(), req)
		if got != tc.want {
			t.Errorf("%s %v: token %q, want %q", tc.path, tc.header, got, tc.want)
		}
	}
}

// Only OAuth device access tokens pass; a console session JWT is refused
// before any device lookup (personal access tokens are not JWTs at all).
func TestOAuthAccessRefusesSessionTokens(t *testing.T) {
	gin.SetMode(gin.TestMode)
	signer := jwt.NewSigner("test-secret-test-secret-test-secret", time.Hour, time.Hour)
	session, _, _, err := signer.IssueAccess(uuid.New(), "user")
	if err != nil {
		t.Fatal(err)
	}
	r := gin.New()
	r.GET("/v1/relay/host", OAuthAccess(nil, signer), func(c *gin.Context) { c.Status(http.StatusOK) })
	for _, token := range []string{"", session, "jcp-abc"} {
		req := httptest.NewRequest(http.MethodGet, "/v1/relay/host", nil)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != http.StatusUnauthorized {
			t.Errorf("token %.12q: status %d, want 401", token, w.Code)
		}
	}
}
