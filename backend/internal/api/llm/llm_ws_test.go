package llm

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"

	"github.com/jucode/jucode/internal/biz"
	"github.com/jucode/jucode/internal/data/ent"
	llmpkg "github.com/jucode/jucode/internal/llm"
	"github.com/jucode/jucode/internal/middleware"
)

func TestMountGenerationWebSocketAndConversion(t *testing.T) {
	gin.SetMode(gin.TestMode)
	gw := &llmpkg.Gateway{}
	h := New(gw, nil)
	r := gin.New()
	v1 := r.Group("/v1")
	v1.Use(func(c *gin.Context) {
		if c.GetHeader("Authorization") == "" {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "missing credentials"})
			return
		}
		c.Set(middleware.CtxUser, &ent.User{})
		c.Next()
	})
	h.MountGeneration(v1)

	unauth := httptest.NewRequest(http.MethodGet, "/v1/realtime", nil)
	unauth.Header.Set("Connection", "Upgrade")
	unauth.Header.Set("Upgrade", "websocket")
	unauth.Header.Set("Sec-WebSocket-Version", "13")
	unauth.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, unauth)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("unauth status = %d body=%s", w.Code, w.Body.String())
	}

	srv := httptest.NewServer(r)
	defer srv.Close()
	u := "ws" + strings.TrimPrefix(srv.URL, "http") + "/v1/realtime"
	hdr := http.Header{}
	hdr.Set("Authorization", "Bearer test")
	conn, resp, err := websocket.DefaultDialer.Dial(u, hdr)
	if err != nil {
		t.Fatalf("dial: %v resp=%v", err, resp)
	}
	defer conn.Close()
	_, msg, err := conn.ReadMessage()
	if err != nil {
		t.Fatal(err)
	}
	var ev map[string]any
	if err := json.Unmarshal(msg, &ev); err != nil {
		t.Fatalf("not JSON: %s", msg)
	}
	if ev["type"] != "session.created" {
		t.Fatalf("type = %v body=%s", ev["type"], msg)
	}
	if err := conn.WriteJSON(map[string]any{"type": "ping", "event_id": "e1"}); err != nil {
		t.Fatal(err)
	}
	_, ack, err := conn.ReadMessage()
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(ack, &ev); err != nil {
		t.Fatal(err)
	}
	if ev["type"] != "pong" {
		t.Fatalf("ack type = %v body=%s", ev["type"], ack)
	}

	// Converted HTTP path: same RewriteForAccount handle() uses. The account
	// speaks Messages natively, so a Chat Completions body has to be converted
	// for it (the site switch plus the account's opt-in allowing it).
	reqBody := `{"model":"m","messages":[{"role":"system","content":"sys-y"},{"role":"user","content":"mount-phrase"}],"web_search_options":{"search_context_size":"low"}}`
	acct := &ent.UpstreamAccount{
		NativeProtocols: []string{"messages"},
		AllowConversion: true,
		Status:          "active",
		Schedulable:     true,
	}
	out, path, target, err := llmpkg.RewriteForAccount(true, llmpkg.DialectCompatible, biz.RequestProtocolCompatible, acct, "", []byte(reqBody))
	if err != nil {
		t.Fatal(err)
	}
	if path != "/v1/messages" || target != llmpkg.DialectAnthropic {
		t.Fatalf("path=%s target=%s", path, target)
	}
	if !strings.Contains(string(out), "sys-y") || !strings.Contains(string(out), "mount-phrase") || !strings.Contains(string(out), "web_search_20250305") {
		t.Fatalf("converted body missing fields: %s", out)
	}
}
