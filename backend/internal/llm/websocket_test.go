package llm

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"

	"github.com/jucode/jucode/internal/data/ent"
	"github.com/jucode/jucode/internal/middleware"
)

func mountGeneration(t *testing.T, requireAuth bool) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	gw := &Gateway{}
	r := gin.New()
	v1 := r.Group("/v1")
	v1.Use(func(c *gin.Context) {
		if requireAuth && c.GetHeader("Authorization") == "" && c.Query("api_key") == "" {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "missing credentials"})
			return
		}
		if c.GetHeader("Authorization") != "" || c.Query("api_key") != "" || !requireAuth {
			c.Set(middleware.CtxUser, &ent.User{})
		}
		c.Next()
	})
	// Same routes MountGeneration registers on the process router.
	v1.POST("/chat/completions", gw.HandleChatCompletions)
	v1.POST("/completions", gw.HandleCompletions)
	v1.POST("/responses", gw.HandleResponses)
	v1.POST("/alpha/search", gw.HandleAlphaSearch)
	v1.GET("/realtime", gw.HandleRealtime)
	v1.GET("/responses", gw.HandleResponsesWS)
	return r
}

func TestWebSocketRejectsUnauthenticatedUpgrade(t *testing.T) {
	r := mountGeneration(t, true)
	req := httptest.NewRequest(http.MethodGet, "/v1/realtime", nil)
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Sec-WebSocket-Version", "13")
	req.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401; body=%s", w.Code, w.Body.String())
	}
}

func TestWebSocketRealtimeUpgradeAndAck(t *testing.T) {
	r := mountGeneration(t, true)
	srv := httptest.NewServer(r)
	defer srv.Close()

	u := "ws" + strings.TrimPrefix(srv.URL, "http") + "/v1/realtime"
	hdr := http.Header{}
	hdr.Set("Authorization", "Bearer test-key")
	conn, resp, err := websocket.DefaultDialer.Dial(u, hdr)
	if err != nil {
		t.Fatalf("dial: %v (resp=%v)", err, resp)
	}
	defer conn.Close()

	_, created, err := conn.ReadMessage()
	if err != nil {
		t.Fatal(err)
	}
	var ev map[string]any
	if err := json.Unmarshal(created, &ev); err != nil {
		t.Fatalf("created not JSON: %s", created)
	}
	if strVal(ev["type"]) != "session.created" {
		t.Fatalf("first event type = %q body=%s", ev["type"], created)
	}

	if err := conn.WriteJSON(map[string]any{"type": "session.update", "session": map[string]any{"model": "gpt-test"}}); err != nil {
		t.Fatal(err)
	}
	_, ack, err := conn.ReadMessage()
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(ack, &ev); err != nil {
		t.Fatalf("ack not JSON: %s", ack)
	}
	if strVal(ev["type"]) != "session.updated" {
		t.Fatalf("ack type = %q body=%s", ev["type"], ack)
	}
}

func TestWebSocketResponsesStaysQuietUntilCreate(t *testing.T) {
	r := mountGeneration(t, true)
	srv := httptest.NewServer(r)
	defer srv.Close()

	u := "ws" + strings.TrimPrefix(srv.URL, "http") + "/v1/responses"
	hdr := http.Header{}
	hdr.Set("Authorization", "Bearer test-key")
	conn, resp, err := websocket.DefaultDialer.Dial(u, hdr)
	if err != nil {
		t.Fatalf("dial: %v (resp=%v)", err, resp)
	}
	defer conn.Close()

	_ = conn.SetReadDeadline(time.Now().Add(150 * time.Millisecond))
	if _, msg, err := conn.ReadMessage(); err == nil {
		t.Fatalf("unsolicited event after upgrade: %s", msg)
	}
	_ = conn.SetReadDeadline(time.Time{})
}

func TestWebSocketResponsesHTTPBridgeEvent(t *testing.T) {
	r := mountGeneration(t, true)
	srv := httptest.NewServer(r)
	defer srv.Close()

	u := "ws" + strings.TrimPrefix(srv.URL, "http") + "/v1/responses"
	hdr := http.Header{}
	hdr.Set("Authorization", "Bearer test-key")
	conn, resp, err := websocket.DefaultDialer.Dial(u, hdr)
	if err != nil {
		t.Fatalf("dial: %v (resp=%v)", err, resp)
	}
	defer conn.Close()

	phrase := "bridge-ping"
	if err := conn.WriteJSON(map[string]any{
		"type": "response.create",
		"request": map[string]any{
			"model":    "gpt-test",
			"messages": []any{map[string]any{"role": "user", "content": phrase}},
		},
	}); err != nil {
		t.Fatal(err)
	}
	_, ack, err := conn.ReadMessage()
	if err != nil {
		t.Fatal(err)
	}
	var ev map[string]any
	if err := json.Unmarshal(ack, &ev); err != nil {
		t.Fatalf("ack not JSON: %s", ack)
	}
	if strVal(ev["type"]) != "response.created" {
		t.Fatalf("ack type = %q body=%s", ev["type"], ack)
	}
	respObj := asMap(ev["response"])
	if respObj == nil || strVal(respObj["id"]) == "" {
		t.Fatalf("response.id missing: %s", ack)
	}
}

func TestWebSocketResponsesCodexWarmupAndFollowup(t *testing.T) {
	r := mountGeneration(t, true)
	srv := httptest.NewServer(r)
	defer srv.Close()

	u := "ws" + strings.TrimPrefix(srv.URL, "http") + "/v1/responses"
	hdr := http.Header{}
	hdr.Set("Authorization", "Bearer test-key")
	conn, resp, err := websocket.DefaultDialer.Dial(u, hdr)
	if err != nil {
		t.Fatalf("dial: %v (resp=%v)", err, resp)
	}
	defer conn.Close()

	if err := conn.WriteJSON(map[string]any{
		"type":         "response.create",
		"stream_id":    "main",
		"model":        "gpt-test",
		"store":        false,
		"generate":     false,
		"instructions": "Use tools.",
		"input":        []any{},
		"tools":        []any{map[string]any{"type": "function", "name": "lookup"}},
	}); err != nil {
		t.Fatal(err)
	}

	created := readWSEvent(t, conn)
	if strVal(created["type"]) != "response.created" {
		t.Fatalf("warmup first event = %v", created)
	}
	if strVal(created["stream_id"]) != "main" {
		t.Fatalf("warmup missing stream_id: %v", created)
	}
	warmupID := strVal(asMap(created["response"])["id"])
	if warmupID == "" {
		t.Fatal("warmup response id missing")
	}
	completed := readWSEvent(t, conn)
	if strVal(completed["type"]) != "response.completed" {
		t.Fatalf("warmup second event = %v", completed)
	}
	if strVal(asMap(completed["response"])["id"]) != warmupID {
		t.Fatalf("warmup completed id = %v want %s", completed, warmupID)
	}

	if err := conn.WriteJSON(map[string]any{
		"type":                 "response.create",
		"stream_id":            "main",
		"model":                "gpt-test",
		"store":                false,
		"previous_response_id": warmupID,
		"input": []any{map[string]any{
			"type":    "message",
			"role":    "user",
			"content": []any{map[string]any{"type": "input_text", "text": "Find fizz_buzz()"}},
		}},
	}); err != nil {
		t.Fatal(err)
	}
	follow := readWSEvent(t, conn)
	if strVal(follow["type"]) != "response.created" {
		t.Fatalf("follow-up first event = %v (leftover completed would poison Codex)", follow)
	}
	if strVal(follow["stream_id"]) != "main" {
		t.Fatalf("follow-up missing stream_id: %v", follow)
	}
}

func TestExtractAndMergeCodexWSRequest(t *testing.T) {
	ev := map[string]any{
		"type":      "response.create",
		"stream_id": "planner",
		"generate":  false,
		"model":     "gpt-test",
		"input":     []any{map[string]any{"role": "user", "content": "hi"}},
		"tools":     []any{map[string]any{"type": "function", "name": "lookup"}},
	}
	req := extractWSRequest(ev)
	if strVal(req["model"]) != "gpt-test" || !isWSWarmup(req) {
		t.Fatalf("extract official payload: %#v", req)
	}
	stripped := stripWSTransportFields(req)
	if _, ok := stripped["type"]; ok {
		t.Fatalf("type leaked to HTTP body: %#v", stripped)
	}
	if _, ok := stripped["generate"]; ok {
		t.Fatalf("generate leaked to HTTP body: %#v", stripped)
	}
	if _, ok := stripped["stream_id"]; ok {
		t.Fatalf("stream_id leaked to HTTP body: %#v", stripped)
	}

	sess := newRealtimeSession("sess", "gpt-test")
	sess.putSynthetic("resp_warm", stripped)
	follow := extractWSRequest(map[string]any{
		"type":                 "response.create",
		"model":                "gpt-test",
		"previous_response_id": "resp_warm",
		"input": []any{map[string]any{
			"type": "message", "role": "user",
			"content": []any{map[string]any{"type": "input_text", "text": "next"}},
		}},
	})
	merged := applyPreviousResponse(follow, sess)
	if _, ok := merged["previous_response_id"]; ok {
		t.Fatalf("synthetic previous_response_id must be dropped: %#v", merged)
	}
	in := asSlice(merged["input"])
	if len(in) != 2 {
		t.Fatalf("expected concatenated input, got %#v", merged["input"])
	}
	if asSlice(merged["tools"]) == nil {
		t.Fatalf("warmup tools dropped: %#v", merged)
	}
}

func readWSEvent(t *testing.T, conn *websocket.Conn) map[string]any {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, raw, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var ev map[string]any
	if err := json.Unmarshal(raw, &ev); err != nil {
		t.Fatalf("not JSON: %s", raw)
	}
	return ev
}

func TestMountedGenerationRegistersWSAndChat(t *testing.T) {
	r := mountGeneration(t, true)
	var hasChat, hasRT, hasRespWS bool
	for _, rt := range r.Routes() {
		switch {
		case rt.Method == http.MethodPost && strings.HasSuffix(rt.Path, "/chat/completions"):
			hasChat = true
		case rt.Method == http.MethodGet && strings.HasSuffix(rt.Path, "/realtime"):
			hasRT = true
		case rt.Method == http.MethodGet && strings.HasSuffix(rt.Path, "/responses"):
			hasRespWS = true
		}
	}
	if !hasChat || !hasRT || !hasRespWS {
		t.Fatalf("routes missing chat=%v realtime=%v responsesWS=%v", hasChat, hasRT, hasRespWS)
	}
}
