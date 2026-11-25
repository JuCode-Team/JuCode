package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"

	"github.com/jucode/jucode/internal/biz"
	"github.com/jucode/jucode/internal/data/ent"
	entusagelog "github.com/jucode/jucode/internal/data/ent/usagelog"
	"github.com/jucode/jucode/internal/middleware"
)

// wsSessionStateBudget caps the client-controlled state one bridged socket may
// pin in memory, per collection. Both the conversation items and the warmup
// cache below grow purely from client frames, and nothing else bounds them:
// wsReadLimit caps how big one frame can be, not how many arrive, and the idle
// timeout refreshes on every frame — so an authenticated socket can otherwise
// walk a node out of memory. Oldest entries are dropped first, which is what a
// context window does to a long conversation anyway.
const wsSessionStateBudget = 8 << 20

type realtimeSession struct {
	mu           sync.Mutex
	id           string
	model        string
	instructions string
	tools        []any
	items        []map[string]any
	itemSizes    []int
	itemBytes    int
	voice        string
	// synthetic holds generate:false warmup bodies keyed by the response
	// id we minted. Codex chains the next turn with previous_response_id
	// pointing at that id; HTTP upstreams do not know it, so we merge
	// and drop the id before the HTTP bridge.
	synthetic map[string]map[string]any
	// Insertion order, so the budget can evict the oldest chain first. Codex
	// chains from the response it just made, so the recent end is the end that
	// matters.
	syntheticOrder []string
	syntheticBytes int
}

func newRealtimeSession(id, model string) *realtimeSession {
	return &realtimeSession{id: id, model: model, synthetic: map[string]map[string]any{}}
}

// approxJSONSize is the retained cost of a decoded frame, close enough for a
// budget: the in-memory maps carry more overhead than their serialized form,
// so this under-counts in the safe direction only by a constant factor.
func approxJSONSize(v any) int {
	b, err := json.Marshal(v)
	if err != nil {
		return 0
	}
	return len(b)
}

func (s *realtimeSession) putSynthetic(id string, req map[string]any) {
	if s == nil || id == "" {
		return
	}
	clone := cloneMap(req)
	size := approxJSONSize(clone)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.synthetic == nil {
		s.synthetic = map[string]map[string]any{}
	}
	if old, exists := s.synthetic[id]; exists {
		s.syntheticBytes -= approxJSONSize(old)
	} else {
		s.syntheticOrder = append(s.syntheticOrder, id)
	}
	s.synthetic[id] = clone
	s.syntheticBytes += size
	for s.syntheticBytes > wsSessionStateBudget && len(s.syntheticOrder) > 1 {
		oldest := s.syntheticOrder[0]
		s.syntheticOrder = s.syntheticOrder[1:]
		s.syntheticBytes -= approxJSONSize(s.synthetic[oldest])
		delete(s.synthetic, oldest)
	}
}

func (s *realtimeSession) lookupSynthetic(id string) (map[string]any, bool) {
	if s == nil || id == "" {
		return nil, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	req, ok := s.synthetic[id]
	if !ok {
		return nil, false
	}
	return cloneMap(req), true
}

func (s *realtimeSession) snapshot() (model, instructions string, tools []any, items []map[string]any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	tools = append([]any(nil), s.tools...)
	items = append([]map[string]any(nil), s.items...)
	return s.model, s.instructions, tools, items
}

func (s *realtimeSession) applyUpdate(sess map[string]any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if m := strVal(sess["model"]); m != "" {
		s.model = m
	}
	if v := sess["instructions"]; v != nil {
		s.instructions = flattenChatContent(v)
	}
	if t := asSlice(sess["tools"]); t != nil {
		s.tools = t
	}
	if v := strVal(sess["voice"]); v != "" {
		s.voice = v
	}
}

func (s *realtimeSession) addItem(item map[string]any) {
	size := approxJSONSize(item)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.items = append(s.items, item)
	s.itemSizes = append(s.itemSizes, size)
	s.itemBytes += size
	// Always keep the newest item even if it alone blows the budget — dropping
	// the turn the client just sent would answer a different question than the
	// one it asked.
	for s.itemBytes > wsSessionStateBudget && len(s.items) > 1 {
		s.itemBytes -= s.itemSizes[0]
		s.items, s.itemSizes = s.items[1:], s.itemSizes[1:]
	}
}

func (s *realtimeSession) view() map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	return map[string]any{
		"id":           s.id,
		"object":       "realtime.session",
		"model":        s.model,
		"instructions": s.instructions,
		"tools":        s.tools,
		"voice":        s.voice,
	}
}

type wsDialer interface {
	DialWS(ctx context.Context, path string, header http.Header) (*websocket.Conn, *http.Response, error)
}

// wsProxyDenied reports gate failures that must not fall through to the HTTP
// bridge. The client already has a 101; bridging would accept the socket and
// only fail on the first response.create. Closing with an error is the
// honest counterpart of the 403/429 HTTP would have returned.
func wsProxyDenied(err error) bool {
	return errors.Is(err, biz.ErrGroupDeniedForUser) ||
		errors.Is(err, biz.ErrGroupDeniedForApiKey) ||
		errors.Is(err, biz.ErrRPMLimit) ||
		errors.Is(err, biz.ErrTPMLimit) ||
		errors.Is(err, biz.ErrConcurrencyLimit)
}

// denyOrFallback writes a client-visible error and reports the session
// handled when err is a gate failure. Any other error (missing route,
// no DialWS, handshake refused) falls through so the HTTP bridge can try.
func denyOrFallback(conn *wsConn, err error) bool {
	if !wsProxyDenied(err) {
		return false
	}
	writeWSGateError(conn, err)
	return true
}

func writeWSGateError(conn *wsConn, err error) {
	if conn == nil || err == nil {
		return
	}
	typ := "invalid_request_error"
	switch {
	case errors.Is(err, biz.ErrGroupDeniedForUser), errors.Is(err, biz.ErrGroupDeniedForApiKey):
		typ = "permission_error"
	case errors.Is(err, biz.ErrRPMLimit), errors.Is(err, biz.ErrTPMLimit), errors.Is(err, biz.ErrConcurrencyLimit):
		typ = "rate_limit_error"
	case errors.Is(err, biz.ErrInsufficientBalance), errors.Is(err, biz.ErrOrganizationLimit),
		errors.Is(err, biz.ErrMemberLimit), errors.Is(err, biz.ErrPlanQuotaExhausted),
		errors.Is(err, biz.ErrMonthlySpendLimitExceeded):
		typ = "billing_error"
	}
	_ = conn.writeJSON(map[string]any{
		"type":  "error",
		"error": map[string]any{"type": typ, "message": err.Error()},
	})
}

// proxyUpstreamWS relays the client socket to a real upstream socket, reporting
// whether it took the session. true also covers a gate denial (access / plan /
// RPM / concurrency): the client already has a 101, so we write an error
// frame instead of falling through to the bridge. false is only "try the
// HTTP bridge" — no route, no DialWS, handshake refused.
//
// A pass-through session skips that pipeline entirely, so the gates it would
// have applied are applied here instead: access, plan, concurrency, rate
// limit, and per-response billing off the relayed frames.
func (g *Gateway) proxyUpstreamWS(c *gin.Context, clientConn *wsConn, kind, model string) bool {
	if model == "" || g.models == nil {
		return false
	}
	user, ok := middleware.UserFrom(c)
	if !ok {
		return false
	}
	apiKey, _ := middleware.ApiKeyFrom(c)
	ctx := c.Request.Context()

	route, err := g.models.Resolve(ctx, model, user, apiKey, biz.RequestProtocolOpenAI)
	if err != nil || route == nil || len(route.Providers) == 0 {
		return false
	}
	// A WebSocket session has no client session anchor; the account sticky
	// still pins by user+group.
	if len(route.Providers) == 0 {
		return false
	}
	rp := route.Providers[0]
	prov, err := g.buildProviderForAttempt(ctx, rp, biz.RequestProtocolOpenAI)
	if err != nil {
		return false
	}
	dialer, ok := prov.(wsDialer)
	if !ok {
		return false
	}
	upstreamModel := rp.UpstreamModel
	if upstreamModel == "" {
		upstreamModel = model
	}

	if err := g.models.CheckAccess(ctx, user, apiKey, route.Group); err != nil {
		return denyOrFallback(clientConn, err)
	}
	planState, err := g.billing.ResolvePlanState(ctx, user, apiKey, route.Group)
	if err != nil {
		return denyOrFallback(clientConn, err)
	}
	release := func() {}
	if g.concur != nil {
		rel, err := g.concur.AcquireWithPlanState(ctx, user, route.Group, planState)
		if err != nil {
			return denyOrFallback(clientConn, err)
		}
		// Held for the whole session: an open socket occupies an upstream slot
		// exactly like an in-flight HTTP stream does.
		release = rel
	}
	lc := logContext{
		IP:              c.ClientIP(),
		UserAgent:       c.Request.UserAgent(),
		ApiKeyName:      keyName(apiKey),
		AuthorizationID: middleware.AuthzIDFrom(c),
	}
	// Peek reserves nothing. Commit happens per settled turn, not at dial:
	// opening the socket is not a generation, and a Codex handshake that
	// never sends response.create must not consume an RPM slot.
	ticket, rlErr := g.rateLimit.Peek(ctx, user, route.Group, planState)
	lc.RateTicket = ticket
	if rlErr != nil {
		release()
		return denyOrFallback(clientConn, rlErr)
	}

	// Realtime carries the model in the query string; the Responses socket
	// takes it per response.create, like its HTTP counterpart.
	path := "/v1/responses"
	if kind == "realtime" {
		path = "/v1/realtime?model=" + url.QueryEscape(upstreamModel)
	}
	up, _, err := dialer.DialWS(ctx, path, c.Request.Header)
	if err != nil {
		// Every provider built on the OpenAI shape structurally offers DialWS,
		// but most OpenAI-compatible relays have no socket behind it, so this
		// is the ordinary case rather than an exceptional one.
		release()
		return false
	}
	defer up.Close()
	defer release()

	upConn := newWSConn(up)
	meter := &realtimeMeter{
		g: g, ctx: ctx, user: user, apiKey: apiKey,
		requestID: gatewayRequestID(c), model: route.Model.Name, route: route,
		routeAccount: rp, upstreamModel: upstreamModel,
		lc: lc, planState: planState,
		clientConn: clientConn, upstreamConn: upConn,
		holdKeyPrefix: newBillingHoldKey("ws"),
	}
	pumpWebSockets(ctx, clientConn, upConn, meter.gateTurn, meter.observe)
	meter.wait()
	return true
}

// pumpWebSockets relays frames between the client and the upstream until
// either side goes away. observe, when non-nil, sees every upstream→client
// text frame — that is the direction that carries usage. gate, when non-nil,
// runs on every client→upstream text frame BEFORE it is forwarded; a non-nil
// error drops the frame and ends the session — this is how a turn that fails
// its billing reserve is stopped before the upstream produces any output.
func pumpWebSockets(ctx context.Context, client, upstream *wsConn, gate func([]byte) error, observe func([]byte)) {
	done := make(chan struct{}, 2)
	var relays sync.WaitGroup
	copyDir := func(src, dst *wsConn, gate func([]byte) error, tap func([]byte)) {
		defer func() {
			done <- struct{}{}
			relays.Done()
		}()
		for {
			src.armReadDeadline()
			typ, msg, err := src.ReadMessage()
			if err != nil {
				return
			}
			if typ == websocket.TextMessage {
				if gate != nil {
					if err := gate(msg); err != nil {
						return
					}
				}
				if tap != nil {
					tap(msg)
				}
			}
			if err := dst.writeMessage(typ, msg); err != nil {
				return
			}
		}
	}
	relays.Add(2)
	go copyDir(client, upstream, gate, nil)
	go copyDir(upstream, client, nil, observe)
	select {
	case <-ctx.Done():
	case <-done:
	}
	// One side going away ends the session, but the other relay is still
	// parked in ReadMessage. Closing both sockets unblocks it, and waiting for
	// both to exit is what guarantees the tap cannot fire after this returns:
	// the caller settles the meter next, and a tap landing in that window adds
	// to a WaitGroup already being waited on — documented misuse that panics
	// on a goroutine with no recover, taking the whole node down with it.
	_ = client.Close()
	_ = upstream.Close()
	relays.Wait()
}

// realtimeMeter bills a proxied session. The upstream reports usage once per
// response, so each response becomes its own charge and usage_log row: an
// hour-long session is billed as it goes instead of in one lump at disconnect,
// which is the part that gets lost when a node dies.
//
// Per-turn billing gate: every client response.create passes through gateTurn
// BEFORE it reaches the upstream, which reserves the turn (estimate above the
// 0.25 water-line, or last-request remaining). Exact settle may take the
// balance negative; CapAtHold does not. observe settles that hold with the
// usage the terminal frame reports; a session that dies mid-turn releases it
// in wait(). Turns the upstream starts on its own (server VAD) have no create
// frame to gate, so they charge directly and re-run the precheck afterwards —
// an exhausted account ends the session instead of riding free.
type realtimeMeter struct {
	g             *Gateway
	ctx           context.Context
	user          *ent.User
	apiKey        *ent.ApiKey
	requestID     string
	model         string
	route         *biz.Route
	routeAccount  biz.RouteAccount
	upstreamModel string
	lc            logContext
	planState     *biz.PlanChargeState
	clientConn    *wsConn
	upstreamConn  *wsConn

	// startedAt is the last response.created seen, turns the number settled;
	// both are only touched by the single pump goroutine that calls observe.
	startedAt time.Time
	turns     int
	wg        sync.WaitGroup

	// pending is the open hold for the turn currently generating. gateTurn
	// (client pump goroutine) writes it, observe (upstream pump goroutine)
	// consumes it, wait() releases a leftover — hence the mutex.
	pendingMu sync.Mutex
	pending   *biz.Reservation
	turnSeq   int
	// holdKeyPrefix is random and server-owned. requestID remains the trace
	// identity and may come from the client's X-Request-Id header.
	holdKeyPrefix string
}

// gateTurn is the client→upstream tap: each response.create re-runs the
// billing gate BEFORE the upstream may generate. A failed reserve answers
// the client with an error frame and returns non-nil, which makes the pump
// drop the frame and end the session — an open socket must not keep
// generating against an exhausted balance.
func (m *realtimeMeter) gateTurn(frame []byte) error {
	if !bytes.Contains(frame, []byte(`"response.create"`)) {
		return nil
	}
	var ev struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(frame, &ev); err != nil || ev.Type != "response.create" {
		return nil
	}
	m.pendingMu.Lock()
	m.turnSeq++
	seq := m.turnSeq
	inFlight := m.pending != nil
	m.pendingMu.Unlock()
	if inFlight {
		// One active response at a time is the Realtime contract; the open
		// hold already covers the turn in progress and the upstream will
		// reject the concurrent create itself.
		return nil
	}
	est := m.g.billing.EstimateMaxCost(m.ctx, m.route.Model, m.route.Group, biz.CostEstimateInput{
		BodyBytes: len(frame),
	})
	resv, err := m.g.billing.Reserve(m.ctx, biz.ReserveInput{
		User: m.user, ApiKey: m.apiKey, Group: m.route.Group, PlanState: m.planState,
		Key:    fmt.Sprintf("%s:turn%d", m.holdKeyPrefix, seq),
		Amount: est, TTL: biz.ReserveTTLWSTurn,
		Note: "realtime ws turn reserve " + m.requestID,
	})
	if err != nil {
		writeWSGateError(m.clientConn, err)
		return err
	}
	m.pendingMu.Lock()
	m.pending = resv
	m.pendingMu.Unlock()
	return nil
}

// wsResponseTerminal reports terminal response frames independently of usage.
// A complete terminal without usage is still terminal and must stop the
// session; otherwise the next response.create would reuse the previous hold.
func wsResponseTerminal(frame []byte) (string, bool) {
	if !bytes.Contains(frame, []byte(`"response.done"`)) &&
		!bytes.Contains(frame, []byte(`"response.completed"`)) &&
		!bytes.Contains(frame, []byte(`"response.failed"`)) &&
		!bytes.Contains(frame, []byte(`"response.incomplete"`)) {
		return "", false
	}
	var ev struct {
		Type     string `json:"type"`
		Response struct {
			ID string `json:"id"`
		} `json:"response"`
	}
	if err := json.Unmarshal(frame, &ev); err != nil {
		return "", false
	}
	switch ev.Type {
	case "response.done", "response.completed", "response.failed", "response.incomplete":
		return ev.Response.ID, true
	default:
		return "", false
	}
}

// wsResponseSettled reports whether an upstream frame ends a successful turn
// and carries the usage to bill it by, returning that usage and response id.
//
// Both dialects ride the same pump: Realtime closes a turn with response.done,
// the Responses socket with response.completed. Recognising only one of them
// means every session on the other bills nothing at all.
func wsResponseSettled(frame []byte) (*Accumulator, string, bool) {
	responseID, terminal := wsResponseTerminal(frame)
	if !terminal {
		return nil, "", false
	}
	var ev struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(frame, &ev); err != nil {
		return nil, "", false
	}
	if ev.Type != "response.done" && ev.Type != "response.completed" {
		return nil, "", false
	}
	// Both dialects put usage at .response.usage and report input_tokens
	// inclusive of the cached and audio portions — the shape the Accumulator
	// already parses, including the audio split that keeps voice tokens off
	// the text price. Reusing it means one place decides how a token total
	// decomposes.
	acc := &Accumulator{}
	acc.Inspect(frame)
	if !acc.HasUsage {
		return nil, "", false
	}
	return acc, responseID, true
}

func (m *realtimeMeter) observe(frame []byte) {
	acc, responseID, ok := wsResponseSettled(frame)
	if !ok {
		if _, terminal := wsResponseTerminal(frame); terminal {
			// No actual usage means there is no safe amount to settle. Stop
			// before another turn can ride the still-open hold; wait() returns
			// that hold after both relays have exited.
			err := errors.New("upstream terminal response missing billing usage")
			writeWSGateError(m.clientConn, err)
			_ = m.clientConn.Close()
			_ = m.upstreamConn.Close()
			return
		}
		if bytes.Contains(frame, []byte(`"response.created"`)) {
			m.startedAt = time.Now()
		}
		return
	}
	requestID := m.requestID
	if responseID != "" {
		requestID += ":" + responseID
	}
	startedAt := m.startedAt
	if startedAt.IsZero() {
		startedAt = time.Now()
	}
	// Each settled turn is one request, including the first — the same
	// accounting as the bridged path, where every generation is its own
	// HTTP call. Opening the socket is not a turn.
	m.turns++

	// Take the turn's hold (if gateTurn opened one) so the charge below
	// settles it instead of double-charging.
	m.pendingMu.Lock()
	resv := m.pending
	m.pending = nil
	m.pendingMu.Unlock()
	lc := m.lc
	lc.Reservation = resv

	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		// Off the relay path: a charge must not stall the audio stream, and
		// chargeAndLog already detaches its context and swallows failures.
		// usage_log.endpoint has no realtime value, so these land under
		// chat_completions like every other conversational generation.
		ctx := context.WithoutCancel(context.Background())
		m.lc.RateTicket.Commit(ctx)
		m.g.chargeAndLog(ctx, m.user, m.apiKey, requestID,
			entusagelog.EndpointChatCompletions, m.model, m.route, m.routeAccount, m.upstreamModel,
			acc, nil, startedAt, nil, 0, http.StatusOK, lc, m.planState)
		if resv == nil {
			// Upstream-initiated turn (server VAD) — it was never gated, so
			// re-run the precheck after the ungated VAD charge; once
			// balance <= 0, close the session so a later turn cannot add
			// another overdraft.
			if _, err := m.g.billing.ResolvePlanState(ctx, m.user, m.apiKey, m.route.Group); err != nil {
				writeWSGateError(m.clientConn, err)
				_ = m.clientConn.Close()
				_ = m.upstreamConn.Close()
			}
		}
	}()
}

func (m *realtimeMeter) wait() {
	m.wg.Wait()
	// Session ended mid-turn (client hung up / upstream died before the
	// terminal usage frame): nothing observable to settle on the realtime
	// pass-through, so the hold is returned in full.
	m.pendingMu.Lock()
	resv := m.pending
	m.pending = nil
	m.pendingMu.Unlock()
	if resv != nil {
		m.g.releaseReservation(context.WithoutCancel(context.Background()), resv)
	}
}

func (g *Gateway) handleWSClientEvent(conn *wsConn, kind string, ev map[string]any, sess *realtimeSession, parent *gin.Context) error {
	typ := strVal(ev["type"])
	if typ == "" {
		typ = "message"
	}
	eventID := strVal(ev["event_id"])

	switch typ {
	case "session.update":
		if sess != nil {
			if s := asMap(ev["session"]); s != nil {
				sess.applyUpdate(s)
			}
			return conn.writeJSON(map[string]any{"type": "session.updated", "session": sess.view()})
		}
		return conn.writeJSON(map[string]any{"type": "session.updated", "session": firstNonNil(ev["session"], map[string]any{})})
	case "conversation.item.create":
		item := asMap(ev["item"])
		if item == nil {
			item = ev
		}
		if sess != nil {
			sess.addItem(item)
		}
		return conn.writeJSON(map[string]any{"type": "conversation.item.created", "item": item})
	case "input_audio_buffer.commit":
		return conn.writeJSON(map[string]any{"type": "input_audio_buffer.committed", "event_id": eventID})
	case "ping":
		return conn.writeJSON(map[string]any{"type": "pong", "event_id": eventID})
	}

	if typ == "response.create" {
		return g.bridgeWSGeneration(parent, conn, kind, ev, sess)
	}

	return conn.writeJSON(map[string]any{
		"type":     typ + ".ack",
		"event_id": eventID,
		"ok":       true,
	})
}

// bridgeWSGeneration answers a response.create by replaying it as an internal
// HTTP generation. Dialect conversion is not decided here: runInternalGeneration
// re-enters the HTTP handler, which reads the site setting itself.
func (g *Gateway) bridgeWSGeneration(parent *gin.Context, conn *wsConn, kind string, ev map[string]any, sess *realtimeSession) error {
	req := extractWSRequest(ev)
	if req == nil && sess != nil {
		req = sessionToGenerationRequest(kind, sess, ev)
	}
	if req == nil {
		return conn.writeJSON(map[string]any{
			"type":  "error",
			"error": map[string]any{"type": "invalid_request_error", "message": "response.create missing request"},
		})
	}
	streamID := strVal(req["stream_id"])
	if streamID == "" {
		streamID = strVal(ev["stream_id"])
	}
	warmup := isWSWarmup(req)
	req = applyPreviousResponse(req, sess)
	if strVal(req["model"]) == "" && sess != nil {
		model, _, _, _ := sess.snapshot()
		req["model"] = model
	}
	req["stream"] = true
	httpReq := stripWSTransportFields(req)
	if strVal(httpReq["model"]) == "" {
		return conn.writeJSONEvent(map[string]any{
			"type":  "error",
			"error": map[string]any{"type": "invalid_request_error", "message": "response.create missing model"},
		}, streamID)
	}

	respID := "resp_" + randomMessageID()
	created := map[string]any{
		"type": "response.created",
		"response": map[string]any{
			"id":     respID,
			"object": "response",
			"status": "in_progress",
			"model":  httpReq["model"],
		},
	}
	if err := conn.writeJSONEvent(created, streamID); err != nil {
		return err
	}

	if warmup {
		sess.putSynthetic(respID, httpReq)
		return conn.writeJSONEvent(map[string]any{
			"type": "response.completed",
			"response": map[string]any{
				"id":     respID,
				"object": "response",
				"status": "completed",
				"output": []any{},
			},
		}, streamID)
	}

	body, err := json.Marshal(httpReq)
	if err != nil {
		return err
	}

	path := "/v1/chat/completions"
	if kind == "responses" || detectRequestDialect(httpReq) == DialectOpenAI {
		path = "/v1/responses"
	}

	bridge := &wsSSEBridge{conn: conn, kind: kind, respID: respID, streamID: streamID, header: make(http.Header)}
	g.runInternalGeneration(parent, path, body, bridge)
	bridge.finish()
	if bridge.sawTerminal {
		return nil
	}
	if bridge.status >= 400 {
		return conn.writeJSONEvent(map[string]any{
			"type":   "error",
			"status": bridge.status,
			"error":  map[string]any{"type": "api_error", "message": "upstream request failed"},
		}, streamID)
	}
	if kind == "realtime" {
		return conn.writeJSON(map[string]any{
			"type":     "response.done",
			"response": map[string]any{"id": respID, "status": "completed"},
		})
	}
	return conn.writeJSONEvent(map[string]any{
		"type": "response.completed",
		"response": map[string]any{
			"id":     respID,
			"object": "response",
			"status": "completed",
			"output": []any{},
		},
	}, streamID)
}

func sessionToGenerationRequest(kind string, sess *realtimeSession, ev map[string]any) map[string]any {
	model, instructions, tools, items := sess.snapshot()
	if extra := asMap(ev["response"]); extra != nil {
		if m := strVal(extra["model"]); m != "" {
			model = m
		}
		if t := asSlice(extra["tools"]); len(t) > 0 {
			tools = t
		}
		if v := extra["instructions"]; v != nil {
			instructions = flattenChatContent(v)
		}
	}
	if model == "" {
		return nil
	}
	if kind == "responses" {
		out := map[string]any{"model": model, "stream": true}
		if instructions != "" {
			out["instructions"] = instructions
		}
		if len(tools) > 0 {
			out["tools"] = tools
		}
		if len(items) > 0 {
			out["input"] = items
		} else {
			out["input"] = ""
		}
		return out
	}
	msgs := make([]any, 0, len(items)+1)
	if instructions != "" {
		msgs = append(msgs, map[string]any{"role": "system", "content": instructions})
	}
	for _, it := range items {
		role := strVal(it["role"])
		if role == "" {
			role = "user"
		}
		content := it["content"]
		if content == nil {
			content = flattenChatContent(it["text"])
		}
		if s := flattenChatContent(content); s != "" || role != "" {
			msgs = append(msgs, map[string]any{"role": role, "content": firstNonEmpty(s, flattenChatContent(content))})
		}
	}
	out := map[string]any{"model": model, "stream": true, "messages": msgs}
	if len(tools) > 0 {
		out["tools"] = tools
	}
	return out
}

func (g *Gateway) runInternalGeneration(parent *gin.Context, path string, body []byte, w http.ResponseWriter) {
	if g.models == nil {
		return
	}
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if parent != nil && parent.Request != nil {
		req = req.WithContext(parent.Request.Context())
		req.RemoteAddr = parent.Request.RemoteAddr
		ApplyOpenAIClientSessionHeaders(req.Header, parent.Request.Header)
		if v := parent.Request.Header.Get("X-Forwarded-For"); v != "" {
			req.Header.Set("X-Forwarded-For", v)
		}
	}
	c, _ := gin.CreateTestContext(w)
	c.Request = req
	if parent != nil {
		for k, v := range parent.Keys {
			c.Set(k, v)
		}
	}
	if strings.Contains(path, "/responses") {
		g.HandleResponses(c)
		return
	}
	g.HandleChatCompletions(c)
}

type wsSSEBridge struct {
	conn        *wsConn
	kind        string
	respID      string
	streamID    string
	header      http.Header
	status      int
	mu          sync.Mutex
	buf         bytes.Buffer
	sawTerminal bool
}

func (w *wsSSEBridge) Header() http.Header { return w.header }
func (w *wsSSEBridge) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
}

func (w *wsSSEBridge) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buf.Write(p)
	data := w.buf.Bytes()
	var rest []byte
	for {
		i := bytes.Index(data, []byte("\n\n"))
		if i < 0 {
			i = bytes.Index(data, []byte("\r\n\r\n"))
			if i < 0 {
				rest = append([]byte(nil), data...)
				break
			}
			frame := data[:i]
			data = data[i+4:]
			w.emitSSEFrame(frame)
			continue
		}
		frame := data[:i]
		data = data[i+2:]
		w.emitSSEFrame(frame)
	}
	w.buf.Reset()
	w.buf.Write(rest)
	return len(p), nil
}
func (w *wsSSEBridge) Flush() {}

func (w *wsSSEBridge) emitSSEFrame(frame []byte) {
	var payload []byte
	for _, line := range bytes.Split(frame, []byte("\n")) {
		line = bytes.TrimRight(line, "\r")
		if bytes.HasPrefix(line, []byte("data:")) {
			payload = bytes.TrimSpace(line[len("data:"):])
		}
	}
	if len(payload) == 0 || bytes.Equal(payload, []byte("[DONE]")) {
		return
	}
	if w.kind == "responses" {
		var obj map[string]any
		if json.Unmarshal(payload, &obj) == nil {
			w.markTerminal(obj)
			_ = w.conn.writeJSONEvent(obj, w.streamID)
			return
		}
	}
	var obj map[string]any
	if json.Unmarshal(payload, &obj) != nil {
		return
	}
	text := ""
	if choices := asSlice(obj["choices"]); len(choices) > 0 {
		if ch := asMap(choices[0]); ch != nil {
			if d := asMap(ch["delta"]); d != nil {
				text = flattenChatContent(d["content"])
			}
		}
	}
	if t := strVal(obj["delta"]); t != "" && text == "" {
		text = t
	}
	if text != "" {
		_ = w.conn.writeJSONEvent(map[string]any{
			"type":        "response.output_text.delta",
			"response_id": w.respID,
			"delta":       text,
		}, w.streamID)
	}
}

func (w *wsSSEBridge) markTerminal(obj map[string]any) {
	switch strVal(obj["type"]) {
	case "response.completed", "response.incomplete", "response.failed", "error":
		w.sawTerminal = true
	}
}

func (w *wsSSEBridge) finish() {
	w.mu.Lock()
	leftover := bytes.TrimSpace(w.buf.Bytes())
	w.buf.Reset()
	status := w.status
	saw := w.sawTerminal
	w.mu.Unlock()
	if saw || len(leftover) == 0 {
		return
	}
	var obj map[string]any
	if json.Unmarshal(leftover, &obj) != nil {
		return
	}
	errBody := obj["error"]
	if errBody == nil {
		return
	}
	ev := map[string]any{"type": "error", "error": errBody}
	if status != 0 {
		ev["status"] = status
	}
	_ = w.conn.writeJSONEvent(ev, w.streamID)
	w.mu.Lock()
	w.sawTerminal = true
	w.mu.Unlock()
}

func extractWSRequest(ev map[string]any) map[string]any {
	var req map[string]any
	for _, key := range []string{"request", "body"} {
		if m := asMap(ev[key]); m != nil {
			if m["model"] != nil || m["messages"] != nil || m["input"] != nil {
				req = cloneMap(m)
				break
			}
		}
	}
	if req == nil {
		if resp := asMap(ev["response"]); resp != nil {
			if resp["model"] != nil || resp["input"] != nil || resp["instructions"] != nil {
				req = cloneMap(resp)
			}
		}
	}
	if req == nil && (ev["model"] != nil || ev["messages"] != nil || ev["input"] != nil) {
		req = cloneMap(ev)
	}
	if req == nil {
		return nil
	}
	// Transport fields live on the envelope in the official Codex payload.
	for _, k := range []string{"generate", "stream_id", "previous_response_id"} {
		if _, ok := req[k]; !ok {
			if v, exists := ev[k]; exists {
				req[k] = v
			}
		}
	}
	return req
}

func isWSWarmup(req map[string]any) bool {
	g, ok := req["generate"].(bool)
	return ok && !g
}

// stripWSTransportFields drops the keys that exist only on the wire protocol.
// They are named rather than filtered against an allowlist of generation
// params: a leaked envelope key costs the whole request ("Unsupported
// parameter: type"), but a generation param missing from an allowlist would be
// dropped silently, and clients are free to send params we have not heard of.
func stripWSTransportFields(req map[string]any) map[string]any {
	out := cloneMap(req)
	delete(out, "type")
	delete(out, "stream_id")
	delete(out, "generate")
	delete(out, "event_id")
	return out
}

func applyPreviousResponse(req map[string]any, sess *realtimeSession) map[string]any {
	prevID := strVal(req["previous_response_id"])
	if prevID == "" || sess == nil {
		return req
	}
	stored, ok := sess.lookupSynthetic(prevID)
	if !ok {
		return req
	}
	return mergeWSFollowup(stored, req)
}

func mergeWSFollowup(prev, next map[string]any) map[string]any {
	out := cloneMap(prev)
	if out == nil {
		out = map[string]any{}
	}
	for k, v := range next {
		switch k {
		case "previous_response_id", "type", "stream_id", "generate", "event_id":
			continue
		case "input":
			out["input"] = concatWSInput(prev["input"], v)
		default:
			if v != nil {
				out[k] = v
			}
		}
	}
	delete(out, "previous_response_id")
	return out
}

func concatWSInput(prev, next any) any {
	if next == nil {
		return prev
	}
	if prev == nil {
		return next
	}
	if ps, ok := prev.(string); ok {
		if ns, ok := next.(string); ok {
			switch {
			case ps == "":
				return ns
			case ns == "":
				return ps
			default:
				return ps + ns
			}
		}
	}
	ps := asSlice(prev)
	ns := asSlice(next)
	if ps == nil && ns == nil {
		return []any{prev, next}
	}
	if ps == nil {
		ps = []any{prev}
	}
	if ns == nil {
		ns = []any{next}
	}
	out := make([]any, 0, len(ps)+len(ns))
	return append(append(out, ps...), ns...)
}

func cloneMap(m map[string]any) map[string]any {
	if m == nil {
		return nil
	}
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func detectRequestDialect(req map[string]any) Dialect {
	if req["messages"] != nil {
		return DialectCompatible
	}
	if req["input"] != nil || req["instructions"] != nil {
		return DialectOpenAI
	}
	if req["max_tokens"] != nil && asSlice(req["messages"]) == nil {
		return DialectAnthropic
	}
	return DialectCompatible
}

var (
	_ http.ResponseWriter = (*wsSSEBridge)(nil)
	_ io.Writer           = (*wsSSEBridge)(nil)
)
