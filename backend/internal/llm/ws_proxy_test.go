package llm

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func upstreamWSServer(t *testing.T, handle func(*websocket.Conn)) *httptest.Server {
	t.Helper()
	up := websocket.Upgrader{
		CheckOrigin:  func(*http.Request) bool { return true },
		Subprotocols: []string{"realtime"},
	}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close()
		handle(c)
	}))
}

// DialWS is what decides whether a WebSocket request can be served at all, so
// it has to reach the upstream with our credential and without the caller's.
func TestDialWSReachesUpstream(t *testing.T) {
	var got struct {
		uri, auth, proto, beta string
	}
	srv := upstreamWSServer(t, func(c *websocket.Conn) {
		_ = c.WriteJSON(map[string]any{"type": "session.created"})
	})
	defer srv.Close()
	// Captured by wrapping the handler the server already has.
	inner := srv.Config.Handler
	srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.uri = r.URL.RequestURI()
		got.auth = r.Header.Get("Authorization")
		got.proto = r.Header.Get("Sec-WebSocket-Protocol")
		got.beta = r.Header.Get("OpenAI-Beta")
		inner.ServeHTTP(w, r)
	})

	p := &openaiProvider{name: "up", baseURL: srv.URL, apiKey: "sk-upstream"}
	clientHdr := http.Header{}
	// The browser convention smuggles the caller's JuCode key through the
	// subprotocol list; it must not reach the upstream operator.
	clientHdr.Set("Sec-WebSocket-Protocol", "realtime, openai-insecure-api-key.sk-juc-caller-secret")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, _, err := p.DialWS(ctx, "/v1/realtime?model=gpt-realtime", clientHdr)
	if err != nil {
		t.Fatalf("DialWS: %v", err)
	}
	defer conn.Close()

	if got.uri != "/v1/realtime?model=gpt-realtime" {
		t.Errorf("upstream URI = %q", got.uri)
	}
	if got.auth != "Bearer sk-upstream" {
		t.Errorf("upstream auth = %q, want the provider key", got.auth)
	}
	if strings.Contains(got.proto, "sk-juc-caller-secret") {
		t.Errorf("caller credential leaked upstream: %q", got.proto)
	}
	if !strings.Contains(got.proto, "realtime") {
		t.Errorf("real negotiation value dropped: %q", got.proto)
	}
	if got.beta != "realtime=v1" {
		t.Errorf("OpenAI-Beta = %q", got.beta)
	}

	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, msg, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read from upstream: %v", err)
	}
	if !strings.Contains(string(msg), "session.created") {
		t.Errorf("upstream frame = %s", msg)
	}
}

// Responses WS is a different product from Realtime. Defaulting the Realtime
// beta on a /v1/responses dial is what makes Codex's proxy handshake fail
// (or look like it failed) and silently fall back to the HTTP bridge.
func TestDialWSResponsesPathOmitsRealtimeBeta(t *testing.T) {
	var gotBeta string
	srv := upstreamWSServer(t, func(*websocket.Conn) {})
	defer srv.Close()
	inner := srv.Config.Handler
	srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBeta = r.Header.Get("OpenAI-Beta")
		inner.ServeHTTP(w, r)
	})

	p := &openaiProvider{name: "up", baseURL: srv.URL, apiKey: "sk-upstream"}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, _, err := p.DialWS(ctx, "/v1/responses", http.Header{})
	if err != nil {
		t.Fatalf("DialWS: %v", err)
	}
	defer conn.Close()
	if gotBeta == "realtime=v1" {
		t.Fatalf("OpenAI-Beta = %q on /v1/responses; Codex rejects that default", gotBeta)
	}

	client := http.Header{}
	client.Set("OpenAI-Beta", "responses-websocket=v1")
	conn2, _, err := p.DialWS(ctx, "/v1/responses", client)
	if err != nil {
		t.Fatalf("DialWS with client beta: %v", err)
	}
	defer conn2.Close()
	if gotBeta != "responses-websocket=v1" {
		t.Fatalf("client OpenAI-Beta was dropped: got %q", gotBeta)
	}
}

// The strip is the last gate before a customer's key would leave for a
// third-party operator, so it must not hinge on the client spelling the marker
// the way we expect.
func TestSanitizeUpstreamSubprotocolsIgnoresCasing(t *testing.T) {
	for _, raw := range []string{
		"realtime, openai-insecure-api-key.sk-secret",
		"realtime, OpenAI-Insecure-API-Key.sk-secret",
		"realtime, OPENAI-INSECURE-API-KEY.sk-secret",
	} {
		got := sanitizeUpstreamSubprotocols(raw)
		if strings.Contains(got, "sk-secret") {
			t.Errorf("credential survived %q → %q", raw, got)
		}
		if !strings.Contains(got, "realtime") {
			t.Errorf("negotiation value dropped from %q → %q", raw, got)
		}
	}
}

// A relayed session goes quiet whenever the human does, and a client that
// keeps it alive with pings must still get its pong — we replace gorilla's
// default ping handler to refresh the idle deadline, and owe the reply.
func TestWSConnAnswersClientPing(t *testing.T) {
	pinged := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, err := wsUpgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer raw.Close()
		conn := newWSConn(raw)
		conn.armReadDeadline()
		// Control frames are processed inside ReadMessage, so the read has to
		// be in flight for the ping handler to run at all.
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}))
	defer srv.Close()

	cli, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer cli.Close()
	cli.SetPongHandler(func(string) error {
		close(pinged)
		return nil
	})
	if err := cli.WriteControl(websocket.PingMessage, []byte("keepalive"), time.Now().Add(5*time.Second)); err != nil {
		t.Fatalf("ping: %v", err)
	}
	// The pong is only surfaced while a read is in flight.
	go func() {
		_ = cli.SetReadDeadline(time.Now().Add(5 * time.Second))
		_, _, _ = cli.ReadMessage()
	}()
	select {
	case <-pinged:
	case <-time.After(5 * time.Second):
		t.Fatal("no pong: a pinging client would be dropped as idle")
	}
}

// The proxy is only worth anything if frames actually move in both directions
// and the meter's tap sees the upstream side, which is where usage rides.
func TestPumpWebSocketsRelaysBothDirections(t *testing.T) {
	upstream := upstreamWSServer(t, func(c *websocket.Conn) {
		for {
			typ, msg, err := c.ReadMessage()
			if err != nil {
				return
			}
			if err := c.WriteMessage(typ, append([]byte("up:"), msg...)); err != nil {
				return
			}
		}
	})
	defer upstream.Close()

	var mu sync.Mutex
	var tapped [][]byte
	done := make(chan struct{})

	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(done)
		client, err := wsUpgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer client.Close()
		up, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(upstream.URL, "http"), nil)
		if err != nil {
			return
		}
		defer up.Close()
		pumpWebSockets(r.Context(), newWSConn(client), newWSConn(up), nil, func(f []byte) {
			mu.Lock()
			tapped = append(tapped, append([]byte(nil), f...))
			mu.Unlock()
		})
	}))
	defer gateway.Close()

	cli, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(gateway.URL, "http"), nil)
	if err != nil {
		t.Fatalf("dial gateway: %v", err)
	}

	for _, want := range []string{"hello", "second"} {
		if err := cli.WriteMessage(websocket.TextMessage, []byte(want)); err != nil {
			t.Fatalf("write: %v", err)
		}
		_ = cli.SetReadDeadline(time.Now().Add(5 * time.Second))
		_, got, err := cli.ReadMessage()
		if err != nil {
			t.Fatalf("read back: %v", err)
		}
		if string(got) != "up:"+want {
			t.Fatalf("round trip = %q, want %q", got, "up:"+want)
		}
	}

	cli.Close()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("pump did not finish after the client left")
	}

	mu.Lock()
	defer mu.Unlock()
	if len(tapped) != 2 {
		t.Fatalf("tap saw %d upstream frames, want 2", len(tapped))
	}
	for _, f := range tapped {
		if !strings.HasPrefix(string(f), "up:") {
			t.Errorf("tap saw a non-upstream frame: %q", f)
		}
	}
}

// The meter settles right after the pump returns, so a tap that can still fire
// at that moment adds to a WaitGroup already being waited on. That is
// documented misuse: the runtime panics, and it panics on a relay goroutine
// with no recover above it, which takes the whole node down. The pump therefore
// owes the caller a guarantee that both relays are finished before it returns.
func TestPumpWebSocketsStopsTappingBeforeItReturns(t *testing.T) {
	// The upstream keeps talking after the client is gone — a Realtime turn
	// still streaming when the human closes the tab.
	upstream := upstreamWSServer(t, func(c *websocket.Conn) {
		for i := 0; i < 2000; i++ {
			if err := c.WriteMessage(websocket.TextMessage, []byte("frame")); err != nil {
				return
			}
		}
	})
	defer upstream.Close()

	var wg sync.WaitGroup
	returned := make(chan struct{})
	// A tap that mirrors the meter: it adds to the same WaitGroup the caller
	// waits on, so an Add after the pump returns reproduces the crash.
	tap := func([]byte) {
		select {
		case <-returned:
			t.Error("tap fired after the pump returned: meter.wait() would race this Add")
			return
		default:
		}
		wg.Add(1)
		wg.Done()
	}

	served := make(chan struct{})
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(served)
		client, err := wsUpgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer client.Close()
		up, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(upstream.URL, "http"), nil)
		if err != nil {
			return
		}
		defer up.Close()
		pumpWebSockets(r.Context(), newWSConn(client), newWSConn(up), nil, tap)
		close(returned)
		wg.Wait()
	}))
	defer gateway.Close()

	cli, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(gateway.URL, "http"), nil)
	if err != nil {
		t.Fatalf("dial gateway: %v", err)
	}
	// Leave mid-stream, which is the case that matters: the upstream relay is
	// parked in ReadMessage with plenty left to send.
	cli.Close()

	select {
	case <-served:
	case <-time.After(10 * time.Second):
		t.Fatal("pump never returned after the client left")
	}
}
