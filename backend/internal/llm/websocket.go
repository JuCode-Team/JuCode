package llm

import (
	"bytes"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"

	"github.com/jucode/jucode/internal/middleware"
)

const (
	// wsReadLimit caps a single inbound frame. Realtime clients stream base64
	// audio so the ceiling has to be generous, but an unbounded read lets one
	// connection allocate the process to death.
	wsReadLimit = 8 << 20
	// wsIdleTimeout closes a connection that has gone quiet. Refreshed on
	// every frame and every ping/pong, so it bounds idleness — not session
	// length, which is what a plain SetReadDeadline would have done.
	wsIdleTimeout = 15 * time.Minute
	// wsWriteTimeout bounds a single frame write to a stalled peer.
	wsWriteTimeout = 30 * time.Second
)

var wsUpgrader = websocket.Upgrader{
	ReadBufferSize:  32 << 10,
	WriteBufferSize: 32 << 10,
	// Codex enables permessage-deflate; negotiate it when the client offers.
	EnableCompression: true,
	// API clients (CLI, SDKs, browsers) connect from arbitrary origins;
	// authentication is enforced separately.
	CheckOrigin: func(*http.Request) bool { return true },
	Subprotocols: []string{
		"realtime",
		"openai-beta.realtime-v1",
		"openai-insecure-api-key",
	},
}

func (g *Gateway) websocketEnabled(c *gin.Context) bool {
	if g.settings == nil {
		return true
	}
	st, err := g.settings.GetSite(c.Request.Context())
	if err != nil || st == nil {
		return true
	}
	return st.WebSocketEnabled
}

func (g *Gateway) formatConversionEnabled(c *gin.Context) bool {
	if g.settings == nil {
		return false
	}
	st, err := g.settings.GetSite(c.Request.Context())
	if err != nil || st == nil {
		return false
	}
	return st.FormatConversionEnabled
}

// HandleRealtime implements GET /v1/realtime (OpenAI Realtime JSON events).
func (g *Gateway) HandleRealtime(c *gin.Context) {
	g.handleWebSocket(c, "realtime")
}

// HandleResponsesWS implements GET /v1/responses WebSocket (OpenAI / Codex
// Responses WS: quiet handshake, response.create frames, SSE events as JSON).
func (g *Gateway) HandleResponsesWS(c *gin.Context) {
	g.handleWebSocket(c, "responses")
}

// handleWebSocket serves one client socket, preferring a real upstream socket
// and falling back to the local HTTP bridge.
//
// The bridge is what makes the fallback honest: a client that discovers
// WebSocket support by dialing (Codex CLI, the OpenAI SDKs) reads 101 as a
// commitment it cannot walk back, so the handshake may only be completed if
// the session that follows actually works. It does, because the bridge speaks
// the same event protocol over plain HTTP generation.
func (g *Gateway) handleWebSocket(c *gin.Context, kind string) {
	requestID := gatewayRequestID(c)
	if !g.websocketEnabled(c) {
		c.JSON(http.StatusNotFound, openAIError("invalid_request_error", "websocket_disabled", "websocket endpoints are disabled", requestID))
		return
	}
	if _, ok := middleware.UserFrom(c); !ok {
		c.JSON(http.StatusUnauthorized, openAIError("authentication_error", "missing_user", "no user", requestID))
		return
	}
	if !websocket.IsWebSocketUpgrade(c.Request) {
		c.JSON(http.StatusUpgradeRequired, openAIError("invalid_request_error", "upgrade_required", "expected websocket upgrade", requestID))
		return
	}

	rawConn, err := wsUpgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		return
	}
	defer rawConn.Close()
	conn := newWSConn(rawConn)

	model := strings.TrimSpace(c.Query("model"))
	if g.proxyUpstreamWS(c, conn, kind, model) {
		return
	}
	g.serveBridgedWS(c, conn, kind, model)
}

// serveBridgedWS answers the socket locally, turning generation events into
// internal HTTP requests and carrying the protocol envelope around them.
func (g *Gateway) serveBridgedWS(c *gin.Context, conn *wsConn, kind, model string) {
	sessionID := "sess_" + randomMessageID()
	sess := newRealtimeSession(sessionID, model)
	// OpenAI Realtime greets with session.created. Responses WS does not:
	// Codex/OpenAI send response.create on a quiet socket, and a greeting
	// response.created would be consumed as the first turn (or poison the
	// next one).
	if kind != "responses" {
		_ = conn.writeJSON(map[string]any{
			"type":    "session.created",
			"session": sess.view(),
		})
	}

	for {
		// Armed per iteration rather than once per completed read: a turn runs
		// synchronously inside handleWSClientEvent below, and a long one (an
		// agentic Codex turn can stream for many minutes) would otherwise
		// return to a deadline set before it started and drop a healthy socket
		// the instant it finished answering.
		conn.armReadDeadline()
		_, raw, err := conn.ReadMessage()
		if err != nil {
			return
		}
		raw = bytes.TrimSpace(raw)
		if len(raw) == 0 {
			continue
		}
		var ev map[string]any
		if err := json.Unmarshal(raw, &ev); err != nil {
			_ = conn.writeJSON(map[string]any{
				"type":  "error",
				"error": map[string]any{"type": "invalid_request_error", "message": "event must be JSON"},
			})
			continue
		}
		if err := g.handleWSClientEvent(conn, kind, ev, sess, c); err != nil {
			return
		}
	}
}

// wsConn owns the write side of one WebSocket. gorilla allows a single
// concurrent writer per connection and the bridge writes from both the client
// read loop and the SSE pump, so the lock is per connection — a process-wide
// one would let a single stalled peer block every other session's writes for
// the whole write timeout.
type wsConn struct {
	*websocket.Conn
	mu sync.Mutex
}

func newWSConn(c *websocket.Conn) *wsConn {
	w := &wsConn{Conn: c}
	c.SetReadLimit(wsReadLimit)
	// A peer that only answers pings is alive, so a pong counts as activity.
	c.SetPongHandler(func(string) error {
		return c.SetReadDeadline(time.Now().Add(wsIdleTimeout))
	})
	// So does one that sends them, and that case needs saying explicitly:
	// ReadMessage never returns for a control frame, so the read loop's own
	// refresh never fires for a peer whose only traffic is pings — a client
	// idling through a silent stretch would be dropped as stalled. Replaces
	// gorilla's default handler, so it still owes the pong.
	c.SetPingHandler(func(appData string) error {
		_ = c.SetReadDeadline(time.Now().Add(wsIdleTimeout))
		err := c.WriteControl(websocket.PongMessage, []byte(appData), time.Now().Add(wsWriteTimeout))
		if errors.Is(err, websocket.ErrCloseSent) {
			return nil
		}
		var ne net.Error
		if errors.As(err, &ne) && ne.Timeout() {
			return nil
		}
		return err
	})
	return w
}

func (w *wsConn) armReadDeadline() {
	_ = w.SetReadDeadline(time.Now().Add(wsIdleTimeout))
}

func (w *wsConn) writeJSON(v any) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	_ = w.SetWriteDeadline(time.Now().Add(wsWriteTimeout))
	return w.WriteJSON(v)
}

// writeJSONEvent tags an event with the stream it belongs to. Codex
// multiplexes turns over one socket and matches replies by stream_id, so an
// event that omits it is attributed to the wrong turn.
func (w *wsConn) writeJSONEvent(ev map[string]any, streamID string) error {
	if streamID != "" {
		if _, ok := ev["stream_id"]; !ok {
			ev["stream_id"] = streamID
		}
	}
	return w.writeJSON(ev)
}

func (w *wsConn) writeMessage(typ int, msg []byte) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	_ = w.SetWriteDeadline(time.Now().Add(wsWriteTimeout))
	return w.WriteMessage(typ, msg)
}
