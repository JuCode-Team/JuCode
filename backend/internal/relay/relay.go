// Package relay forwards opaque bytes between clients (phone apps, the web
// app, other desktops) and a desktop daemon ("host") that keeps one outbound
// WebSocket to the relay. It is mounted in the main server under /v1/relay
// behind OAuth device tokens: a host registers as one of its account's
// computers (biz.ComputerSvc), and only clients of the same account reach it.
// Payloads are end-to-end encrypted (Noise) and are never inspected.
// Protocol: cli/docs/relay-protocol.md, section 3.
package relay

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"

	"github.com/jucode/jucode/internal/biz"
	"github.com/jucode/jucode/internal/data/ent"
	"github.com/jucode/jucode/internal/middleware"
)

// Frame kinds on the host connection.
const (
	kindOpen  = 1
	kindData  = 2
	kindClose = 3
)

// WebSocket close codes used by the relay.
const (
	CloseProtocol     = 4400
	CloseUnauthorized = 4401
	CloseForbidden    = 4403
	CloseHostOffline  = 4404
	CloseIdle         = 4408
	CloseReplaced     = 4409
	CloseHostClosed   = 4410
	CloseTooMany      = 4429
	CloseRestarting   = 4503
)

const (
	authPrefix = "jucode-relay-v1:"
	// Subprotocol answers browsers, which offer it together with
	// "jucode-bearer.<token>" (middleware.OAuthAccess reads the token).
	Subprotocol = "jucode-relay"
)

// Registry keeps the account's computers (biz.ComputerSvc).
type Registry interface {
	// Register records an authenticated host connection.
	Register(ctx context.Context, c biz.ComputerConn) error
	// Owner returns the account a host belongs to; ok false when unknown.
	Owner(ctx context.Context, hostID string) (owner uuid.UUID, ok bool, err error)
	// Alive marks the host online on this instance; seen also stores
	// last_seen_at.
	Alive(ctx context.Context, hostID string, seen bool) error
	// Gone clears the online mark this instance set.
	Gone(ctx context.Context, hostID string) error
}

// Config holds the relay limits. DefaultConfig matches the protocol spec.
type Config struct {
	MaxMessage           int64         // max client message / data payload
	MaxStreams           int           // concurrent client streams per host
	ClientConnectsPerMin int           // new client connections per IP per minute
	HostAuthsPerMin      int           // host auth attempts per IP per minute
	AuthTimeout          time.Duration // challenge must be answered within this
	IdleTimeout          time.Duration // client stream with no traffic either way
	PingInterval         time.Duration // WebSocket ping to hosts, online mark refresh
	SeenInterval         time.Duration // last_seen_at refresh while connected
	PongWait             time.Duration // extra time after a ping for any read
	WriteTimeout         time.Duration // per write on any connection
	HostQueue            int           // frames queued towards one host
	ClientQueue          int           // messages queued towards one client
	ClientQueueBytes     int64         // bytes queued towards one client
}

func DefaultConfig() Config {
	return Config{
		MaxMessage:           1 << 20,
		MaxStreams:           32,
		ClientConnectsPerMin: 20,
		HostAuthsPerMin:      10,
		AuthTimeout:          10 * time.Second,
		IdleTimeout:          10 * time.Minute,
		PingInterval:         30 * time.Second,
		SeenInterval:         5 * time.Minute,
		PongWait:             30 * time.Second,
		WriteTimeout:         30 * time.Second,
		HostQueue:            64,
		ClientQueue:          64,
		ClientQueueBytes:     4 << 20,
	}
}

// HostID derives the host id: base64url (no padding) of the first 16 bytes
// of SHA-256 of the Ed25519 public key.
func HostID(pub ed25519.PublicKey) string {
	sum := sha256.Sum256(pub)
	return base64.RawURLEncoding.EncodeToString(sum[:16])
}

type Server struct {
	cfg      Config
	reg      Registry
	log      *slog.Logger
	up       websocket.Upgrader
	clientRL *limiter
	authRL   *limiter

	mu     sync.Mutex
	hosts  map[string]*host
	closed bool
}

func New(cfg Config, reg Registry, log *slog.Logger) *Server {
	return &Server{
		cfg: cfg,
		reg: reg,
		log: log,
		up: websocket.Upgrader{
			HandshakeTimeout: 10 * time.Second,
			Subprotocols:     []string{Subprotocol},
			// Any Origin is accepted on purpose: the web app lives on its
			// own domain and other clients have no Origin at all.
			// Access is the account's token plus the end-to-end Noise layer.
			CheckOrigin: func(*http.Request) bool { return true },
		},
		clientRL: newLimiter(cfg.ClientConnectsPerMin, time.Minute),
		authRL:   newLimiter(cfg.HostAuthsPerMin, time.Minute),
		hosts:    map[string]*host{},
	}
}

// Mount serves GET <rg>/host and GET <rg>/connect; rg must authenticate
// with middleware.OAuthAccess.
func (s *Server) Mount(rg *gin.RouterGroup) {
	rg.GET("/host", s.handleHost)
	rg.GET("/connect", s.handleConnect)
}

// Drain disconnects every host, and with them all client streams, with
// 4503 so they reconnect, through HAProxy, to the instance that stays.
func (s *Server) Drain() {
	s.mu.Lock()
	hosts := make([]*host, 0, len(s.hosts))
	for _, h := range s.hosts {
		hosts = append(hosts, h)
	}
	s.mu.Unlock()
	for _, h := range hosts {
		h.close(CloseRestarting, "server restarting")
	}
}

// Close drains and makes the relay refuse new hosts. Used on shutdown;
// hijacked WebSocket connections are not tracked by http.Server.Shutdown.
func (s *Server) Close() {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	s.Drain()
}

// Kick disconnects hostID if it is connected here (its computer was
// removed from the account).
func (s *Server) Kick(hostID string) {
	s.mu.Lock()
	h := s.hosts[hostID]
	s.mu.Unlock()
	if h != nil {
		h.close(websocket.CloseNormalClosure, "computer removed")
	}
}

// KickAuthz disconnects the host and client streams of a revoked OAuth
// authorization held here: a token is only checked when a socket opens.
func (s *Server) KickAuthz(authz uuid.UUID) {
	s.mu.Lock()
	hosts := make([]*host, 0, len(s.hosts))
	for _, h := range s.hosts {
		hosts = append(hosts, h)
	}
	s.mu.Unlock()
	for _, h := range hosts {
		if h.authz == authz {
			h.close(CloseUnauthorized, "device revoked")
			continue
		}
		h.mu.Lock()
		var revoked []*stream
		for _, st := range h.streams {
			if st.authz == authz {
				revoked = append(revoked, st)
			}
		}
		h.mu.Unlock()
		for _, st := range revoked {
			st.finish(CloseUnauthorized, "device revoked", true)
		}
	}
}

// identity is the OAuth device middleware.OAuthAccess resolved.
func identity(c *gin.Context) (uuid.UUID, *ent.OAuthAuthorization, bool) {
	uid, ok := middleware.UserIDFrom(c)
	authz, authzOK := middleware.AuthzFrom(c)
	return uid, authz, ok && authzOK
}

func registryCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 5*time.Second)
}

// closeConn sends a close frame, waits briefly for the peer's reply (so the
// close code is not lost to a TCP reset) and closes the connection. Only for
// connections with no other reader.
func closeConn(c *websocket.Conn, code int, reason string) {
	_ = c.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(code, reason), time.Now().Add(time.Second))
	_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
	for {
		if _, _, err := c.ReadMessage(); err != nil {
			break
		}
	}
	_ = c.Close()
}

// closeLater sends a close frame and closes the socket after a grace period;
// the connection's own reader exits on the peer's close reply or the close.
func closeLater(c *websocket.Conn, code int, reason string) {
	_ = c.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(code, reason), time.Now().Add(time.Second))
	time.AfterFunc(2*time.Second, func() { _ = c.Close() })
}

func frame(kind byte, sid uint32, payload []byte) []byte {
	f := make([]byte, 5+len(payload))
	f[0] = kind
	binary.BigEndian.PutUint32(f[1:5], sid)
	copy(f[5:], payload)
	return f
}

// ---- host side ----

type host struct {
	srv   *Server
	id    string
	ip    string
	authz uuid.UUID // the daemon's OAuth authorization
	conn  *websocket.Conn
	out   chan []byte
	done  chan struct{}
	once  sync.Once

	mu      sync.Mutex
	streams map[uint32]*stream // nil once the host is closed
	nextSID uint32
	opened  int
}

func (s *Server) handleHost(c *gin.Context) {
	uid, authz, ok := identity(c)
	if !ok {
		c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "missing identity"})
		return
	}
	want, ip := c.Query("host"), c.ClientIP()
	conn, err := s.up.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		return // the upgrader already answered with an HTTP error
	}
	if !s.authRL.allow(ip) {
		s.log.Warn("relay host auth rate limited", "ip", ip)
		closeConn(conn, CloseTooMany, "too many auth attempts")
		return
	}
	conn.SetReadLimit(s.cfg.MaxMessage + 5)
	m, err := s.authenticate(conn)
	if err != nil {
		s.log.Info("relay host auth failed", "ip", ip, "err", err)
		closeConn(conn, CloseUnauthorized, "unauthorized")
		return
	}
	id := m.id
	if id != want {
		closeConn(conn, CloseProtocol, "host id mismatch")
		return
	}
	ctx, cancel := registryCtx()
	err = s.reg.Register(ctx, biz.ComputerConn{
		HostID: id, UserID: uid, AuthorizationID: authz.ID,
		IdentityPub: m.Pub, StaticPub: m.Static, Name: m.Name, Platform: m.Platform, Version: m.V,
	})
	cancel()
	if err != nil {
		s.log.Error("relay host register failed", "host", id, "err", err)
		closeConn(conn, websocket.CloseInternalServerErr, "cannot register computer")
		return
	}
	_ = conn.SetWriteDeadline(time.Now().Add(s.cfg.WriteTimeout))
	if err := conn.WriteJSON(map[string]string{"t": "ready", "host": id, "user": uid.String()}); err != nil {
		_ = conn.Close()
		return
	}
	_ = conn.SetWriteDeadline(time.Time{})

	h := &host{
		srv: s, id: id, ip: ip, conn: conn, authz: authz.ID,
		out:     make(chan []byte, s.cfg.HostQueue),
		done:    make(chan struct{}),
		streams: map[uint32]*stream{},
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		closeConn(conn, CloseRestarting, "server restarting")
		return
	}
	old := s.hosts[id]
	s.hosts[id] = h
	s.mu.Unlock()
	if old != nil {
		old.close(CloseReplaced, "replaced by newer connection")
	}
	ctx, cancel = registryCtx()
	if err := s.reg.Alive(ctx, id, false); err != nil {
		s.log.Warn("relay presence mark failed", "host", id, "err", err)
	}
	cancel()
	s.log.Info("relay host connected", "host", id, "user", uid, "ip", ip, "version", m.V, "replaced", old != nil)

	go h.writeLoop()
	go h.presenceLoop()
	h.readLoop()
	_ = conn.Close()
}

// authMsg is the host's answer to the challenge.
type authMsg struct {
	T        string `json:"t"`
	Pub      string `json:"pub"`
	Sig      string `json:"sig"`
	V        string `json:"v"`
	Static   string `json:"static"`
	Name     string `json:"name"`
	Platform string `json:"platform"`
	id       string
}

func (s *Server) authenticate(conn *websocket.Conn) (*authMsg, error) {
	nonce := make([]byte, 32)
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	deadline := time.Now().Add(s.cfg.AuthTimeout)
	_ = conn.SetWriteDeadline(deadline)
	_ = conn.SetReadDeadline(deadline)
	if err := conn.WriteJSON(map[string]string{"t": "challenge", "nonce": base64.RawURLEncoding.EncodeToString(nonce)}); err != nil {
		return nil, err
	}
	mt, data, err := conn.ReadMessage()
	if err != nil {
		return nil, err
	}
	if mt != websocket.TextMessage {
		return nil, errors.New("auth: expected text message")
	}
	var m authMsg
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("auth: %w", err)
	}
	if m.T != "auth" {
		return nil, errors.New("auth: unexpected message type")
	}
	pub, err := base64.RawURLEncoding.DecodeString(m.Pub)
	if err != nil || len(pub) != ed25519.PublicKeySize {
		return nil, errors.New("auth: bad public key")
	}
	sig, err := base64.RawURLEncoding.DecodeString(m.Sig)
	if err != nil || len(sig) != ed25519.SignatureSize {
		return nil, errors.New("auth: bad signature encoding")
	}
	if !ed25519.Verify(pub, append([]byte(authPrefix), nonce...), sig) {
		return nil, errors.New("auth: signature mismatch")
	}
	if static, err := base64.RawURLEncoding.DecodeString(m.Static); err != nil || len(static) != 32 {
		return nil, errors.New("auth: bad static key")
	}
	_ = conn.SetWriteDeadline(time.Time{})
	_ = conn.SetReadDeadline(time.Time{})
	m.id = HostID(pub)
	return &m, nil
}

// presenceLoop keeps the host's online mark (and last_seen_at) fresh while
// it is connected.
func (h *host) presenceLoop() {
	cfg := h.srv.cfg
	tick := time.NewTicker(cfg.PingInterval)
	defer tick.Stop()
	seen := time.Now()
	for {
		select {
		case <-tick.C:
			store := time.Since(seen) >= cfg.SeenInterval
			if store {
				seen = time.Now()
			}
			ctx, cancel := registryCtx()
			if err := h.srv.reg.Alive(ctx, h.id, store); err != nil {
				h.srv.log.Warn("relay presence refresh failed", "host", h.id, "err", err)
			}
			cancel()
		case <-h.done:
			return
		}
	}
}

func (h *host) readLoop() {
	c := h.conn
	wait := h.srv.cfg.PingInterval + h.srv.cfg.PongWait
	_ = c.SetReadDeadline(time.Now().Add(wait))
	c.SetPongHandler(func(string) error { return c.SetReadDeadline(time.Now().Add(wait)) })
	for {
		mt, data, err := c.ReadMessage()
		if err != nil {
			h.close(websocket.CloseNormalClosure, "")
			return
		}
		_ = c.SetReadDeadline(time.Now().Add(wait))
		if mt != websocket.BinaryMessage || len(data) < 5 {
			h.close(CloseProtocol, "protocol error")
			continue // drain until the socket closes
		}
		sid := binary.BigEndian.Uint32(data[1:5])
		h.mu.Lock()
		st := h.streams[sid]
		h.mu.Unlock()
		switch data[0] {
		case kindData:
			if st != nil { // unknown sid: stream just closed, drop
				st.send(data[5:])
			}
		case kindClose:
			if st != nil {
				st.closeAfterQueued()
			}
		default:
			h.close(CloseProtocol, "protocol error")
		}
	}
}

func (h *host) writeLoop() {
	ping := time.NewTicker(h.srv.cfg.PingInterval)
	defer ping.Stop()
	for {
		select {
		case f := <-h.out:
			_ = h.conn.SetWriteDeadline(time.Now().Add(h.srv.cfg.WriteTimeout))
			if err := h.conn.WriteMessage(websocket.BinaryMessage, f); err != nil {
				h.close(websocket.CloseInternalServerErr, "write failed")
				return
			}
		case <-ping.C:
			if err := h.conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(h.srv.cfg.WriteTimeout)); err != nil {
				h.close(websocket.CloseInternalServerErr, "ping failed")
				return
			}
		case <-h.done:
			return
		}
	}
}

// enqueue queues a frame for the host; it blocks (backpressure on the
// sender) until queued, the host closes, or cancel fires.
func (h *host) enqueue(f []byte, cancel <-chan struct{}) bool {
	select {
	case h.out <- f:
		return true
	case <-h.done:
		return false
	case <-cancel:
		return false
	}
}

func (h *host) close(code int, reason string) {
	h.once.Do(func() {
		s := h.srv
		s.mu.Lock()
		current := s.hosts[h.id] == h
		if current {
			delete(s.hosts, h.id)
		}
		s.mu.Unlock()
		close(h.done)
		closeLater(h.conn, code, reason)
		if current { // a replacement keeps the online mark
			go func() {
				ctx, cancel := registryCtx()
				defer cancel()
				if err := s.reg.Gone(ctx, h.id); err != nil {
					s.log.Warn("relay presence clear failed", "host", h.id, "err", err)
				}
			}()
		}

		h.mu.Lock()
		streams := h.streams
		h.streams = nil
		opened := h.opened
		h.mu.Unlock()
		streamCode, streamReason := CloseHostOffline, "host offline"
		if code == CloseRestarting {
			streamCode, streamReason = code, reason
		}
		for _, st := range streams {
			st.finish(streamCode, streamReason, false)
		}
		s.log.Info("relay host disconnected", "host", h.id, "ip", h.ip, "code", code,
			"streams_opened", opened, "streams_dropped", len(streams))
	})
}

// addStream registers a client stream, or returns a close code.
func (h *host) addStream(conn *websocket.Conn, authz uuid.UUID) (*stream, int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.streams == nil {
		return nil, CloseHostOffline
	}
	if len(h.streams) >= h.srv.cfg.MaxStreams {
		return nil, CloseTooMany
	}
	for {
		h.nextSID++
		if _, used := h.streams[h.nextSID]; h.nextSID != 0 && !used {
			break
		}
	}
	st := &stream{
		h: h, sid: h.nextSID, conn: conn, authz: authz,
		out:  make(chan []byte, h.srv.cfg.ClientQueue),
		done: make(chan struct{}),
	}
	st.touch()
	h.streams[st.sid] = st
	h.opened++
	return st, 0
}

// ---- client side ----

type stream struct {
	h      *host
	sid    uint32
	authz  uuid.UUID // the caller's OAuth authorization
	conn   *websocket.Conn
	out    chan []byte
	queued atomic.Int64
	last   atomic.Int64 // unix nanos of last traffic either way
	done   chan struct{}
	once   sync.Once
}

func (st *stream) touch() { st.last.Store(time.Now().UnixNano()) }

// openPayload is the open frame's payload: who is calling, from the token.
type openPayload struct {
	User   string `json:"user"`
	Authz  string `json:"authz"`
	Client string `json:"client"`
	Name   string `json:"name"`
}

func (s *Server) handleConnect(c *gin.Context) {
	uid, authz, ok := identity(c)
	if !ok {
		c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "missing identity"})
		return
	}
	id, ip := c.Query("host"), c.ClientIP()
	conn, err := s.up.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		return
	}
	if !s.clientRL.allow(ip) {
		s.log.Warn("relay client connect rate limited", "ip", ip)
		closeConn(conn, CloseTooMany, "too many connections")
		return
	}
	// The upgrade comes first for every refusal so browsers see the code.
	ctx, cancel := registryCtx()
	owner, known, err := s.reg.Owner(ctx, id)
	cancel()
	if err != nil {
		s.log.Error("relay owner lookup failed", "host", id, "err", err)
		closeConn(conn, websocket.CloseInternalServerErr, "try again")
		return
	}
	if !known || owner != uid {
		closeConn(conn, CloseForbidden, "not your computer")
		return
	}
	payload, err := json.Marshal(openPayload{User: uid.String(), Authz: authz.ID.String(), Client: authz.ClientID, Name: authz.Name})
	if err != nil {
		_ = conn.Close()
		return
	}
	s.mu.Lock()
	h := s.hosts[id]
	s.mu.Unlock()
	if h == nil {
		closeConn(conn, CloseHostOffline, "host offline")
		return
	}
	st, code := h.addStream(conn, authz.ID)
	if st == nil {
		reason := "host offline"
		if code == CloseTooMany {
			reason = "too many streams"
		}
		closeConn(conn, code, reason)
		return
	}
	defer conn.Close()
	conn.SetReadLimit(s.cfg.MaxMessage)
	s.log.Debug("relay stream opened", "host", h.id, "sid", st.sid, "ip", ip)
	if !h.enqueue(frame(kindOpen, st.sid, payload), st.done) {
		st.finish(CloseHostOffline, "host offline", false)
	}
	go st.writeLoop()
	st.readLoop()
}

func (st *stream) readLoop() {
	for {
		mt, data, err := st.conn.ReadMessage()
		if err != nil {
			// Client closed or went away; for an oversized message gorilla
			// has already sent close 1009.
			st.finish(websocket.CloseNormalClosure, "", true)
			return
		}
		select {
		case <-st.done:
			continue // closing: discard until the socket closes
		default:
		}
		if mt != websocket.BinaryMessage {
			st.finish(CloseProtocol, "binary frames only", true)
			continue
		}
		st.touch()
		st.h.enqueue(frame(kindData, st.sid, data), st.done)
	}
}

func (st *stream) writeLoop() {
	cfg := st.h.srv.cfg
	idle := time.NewTicker(cfg.IdleTimeout / 10)
	defer idle.Stop()
	for {
		select {
		case p := <-st.out:
			if p == nil { // the host closed after what was queued before it
				st.finish(CloseHostClosed, "host closed", false)
				return
			}
			st.queued.Add(-int64(len(p)))
			_ = st.conn.SetWriteDeadline(time.Now().Add(cfg.WriteTimeout))
			if err := st.conn.WriteMessage(websocket.BinaryMessage, p); err != nil {
				st.finish(websocket.CloseInternalServerErr, "write failed", true)
				return
			}
		case <-idle.C:
			if time.Since(time.Unix(0, st.last.Load())) >= cfg.IdleTimeout {
				st.finish(CloseIdle, "idle timeout", true)
				return
			}
		case <-st.done:
			return
		}
	}
}

// send queues a host payload for the client without blocking the host
// connection; a client that cannot keep up loses its stream.
func (st *stream) send(p []byte) {
	n := int64(len(p))
	if st.queued.Load()+n > st.h.srv.cfg.ClientQueueBytes {
		st.finish(websocket.CloseTryAgainLater, "slow consumer", true)
		return
	}
	st.queued.Add(n)
	select {
	case st.out <- p:
		st.touch()
	default:
		st.queued.Add(-n)
		st.finish(websocket.CloseTryAgainLater, "slow consumer", true)
	}
}

// closeAfterQueued ends the stream once the client has the host's queued
// payloads: a daemon refuses a device with a last message and then closes.
func (st *stream) closeAfterQueued() {
	select {
	case st.out <- nil:
	default:
		st.finish(CloseHostClosed, "host closed", false)
	}
}

// finish ends the stream once: unregisters it, optionally tells the host,
// and closes the client with code.
func (st *stream) finish(code int, reason string, notifyHost bool) {
	st.once.Do(func() {
		h := st.h
		h.mu.Lock()
		delete(h.streams, st.sid)
		active := len(h.streams)
		h.mu.Unlock()
		close(st.done)
		if notifyHost {
			h.enqueue(frame(kindClose, st.sid, nil), nil)
		}
		closeLater(st.conn, code, reason)
		h.srv.log.Debug("relay stream closed", "host", h.id, "sid", st.sid, "code", code, "active", active)
	})
}

// ---- rate limiting ----

// limiter is a fixed-window counter per key.
type limiter struct {
	n   int
	win time.Duration

	mu sync.Mutex
	m  map[string]*window
}

type window struct {
	start time.Time
	count int
}

func newLimiter(n int, win time.Duration) *limiter {
	return &limiter{n: n, win: win, m: map[string]*window{}}
}

func (l *limiter) allow(key string) bool {
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.m) > 4096 {
		for k, w := range l.m {
			if now.Sub(w.start) >= l.win {
				delete(l.m, k)
			}
		}
	}
	w := l.m[key]
	if w == nil || now.Sub(w.start) >= l.win {
		w = &window{start: now}
		l.m[key] = w
	}
	if w.count >= l.n {
		return false
	}
	w.count++
	return true
}
