package relay

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"

	"github.com/jucode/jucode/internal/biz"
	"github.com/jucode/jucode/internal/data/ent"
	"github.com/jucode/jucode/internal/middleware"
)

func testConfig() Config {
	cfg := DefaultConfig()
	cfg.ClientConnectsPerMin = 1000
	cfg.HostAuthsPerMin = 1000
	return cfg
}

// Two accounts' OAuth devices, by bearer token.
var (
	alice = &ent.OAuthAuthorization{ID: uuid.New(), UserID: uuid.New(), ClientID: "jucode-desktop", Name: "Laptop"}
	bob   = &ent.OAuthAuthorization{ID: uuid.New(), UserID: uuid.New(), ClientID: "jucode-web", Name: "网页"}
	// The daemon's own device in alice's account.
	aliceDaemon = &ent.OAuthAuthorization{ID: uuid.New(), UserID: alice.UserID, ClientID: "jucode-cli", Name: "Mac"}
	tokens      = map[string]*ent.OAuthAuthorization{"alice": alice, "bob": bob, "daemon": aliceDaemon}
)

// fakeAuth stands in for middleware.OAuthAccess: same token extraction,
// devices from the tokens table.
func fakeAuth(c *gin.Context) {
	authz := tokens[middleware.OAuthToken(c)]
	if authz == nil {
		c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "invalid oauth access token"})
		return
	}
	c.Set(middleware.CtxUserID, authz.UserID)
	c.Set(middleware.CtxAuthz, authz)
}

// fakeRegistry is biz.ComputerSvc in memory.
type fakeRegistry struct {
	mu     sync.Mutex
	owners map[string]uuid.UUID
	conns  []biz.ComputerConn
	online map[string]bool
	seen   int
}

func (r *fakeRegistry) Register(_ context.Context, c biz.ComputerConn) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.owners[c.HostID] = c.UserID
	r.conns = append(r.conns, c)
	return nil
}

func (r *fakeRegistry) Owner(_ context.Context, id string) (uuid.UUID, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	u, ok := r.owners[id]
	return u, ok, nil
}

func (r *fakeRegistry) Alive(_ context.Context, id string, seen bool) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.online[id] = true
	if seen {
		r.seen++
	}
	return nil
}

func (r *fakeRegistry) Gone(_ context.Context, id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.online, id)
	return nil
}

func (r *fakeRegistry) isOnline(id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.online[id]
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for end := time.Now().Add(3 * time.Second); time.Now().Before(end); time.Sleep(10 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}

type testRelay struct {
	base string
	srv  *Server
	reg  *fakeRegistry
}

func newTestRelay(t *testing.T, cfg Config) *testRelay {
	t.Helper()
	gin.SetMode(gin.TestMode)
	reg := &fakeRegistry{owners: map[string]uuid.UUID{}, online: map[string]bool{}}
	rs := New(cfg, reg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	r := gin.New()
	rs.Mount(r.Group("/v1/relay", fakeAuth))
	ts := httptest.NewServer(r)
	t.Cleanup(func() { rs.Close(); ts.Close() })
	return &testRelay{base: "ws" + strings.TrimPrefix(ts.URL, "http") + "/v1/relay", srv: rs, reg: reg}
}

func newRelay(t *testing.T, cfg Config) string { return newTestRelay(t, cfg).base }

func dialAs(t *testing.T, url, token string) *websocket.Conn {
	t.Helper()
	c, _, err := websocket.DefaultDialer.Dial(url, http.Header{"Authorization": {"Bearer " + token}})
	if err != nil {
		t.Fatalf("dial %s: %v", url, err)
	}
	t.Cleanup(func() { c.Close() })
	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	return c
}

// dial connects as alice's phone.
func dial(t *testing.T, url string) *websocket.Conn { return dialAs(t, url, "alice") }

func newKey(t *testing.T) ed25519.PrivateKey {
	_, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	return priv
}

func hostID(priv ed25519.PrivateKey) string { return HostID(priv.Public().(ed25519.PublicKey)) }

var testStatic = base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{9}, 32))

// authHost runs the challenge/auth exchange as alice's daemon, signing with
// signKey but presenting priv's public key.
func authHost(t *testing.T, base string, priv, signKey ed25519.PrivateKey) *websocket.Conn {
	t.Helper()
	return authHostAt(t, base+"/host?host="+hostID(priv), "daemon", priv, signKey)
}

func authHostAt(t *testing.T, url, token string, priv, signKey ed25519.PrivateKey) *websocket.Conn {
	t.Helper()
	c := dialAs(t, url, token)
	var ch struct{ T, Nonce string }
	if err := c.ReadJSON(&ch); err != nil || ch.T != "challenge" {
		t.Fatalf("challenge: %+v %v", ch, err)
	}
	nonce, err := base64.RawURLEncoding.DecodeString(ch.Nonce)
	if err != nil || len(nonce) != 32 {
		t.Fatalf("nonce: %q %v", ch.Nonce, err)
	}
	sig := ed25519.Sign(signKey, append([]byte("jucode-relay-v1:"), nonce...))
	err = c.WriteJSON(map[string]string{
		"t":        "auth",
		"pub":      base64.RawURLEncoding.EncodeToString(priv.Public().(ed25519.PublicKey)),
		"sig":      base64.RawURLEncoding.EncodeToString(sig),
		"v":        "test",
		"static":   testStatic,
		"name":     "studio",
		"platform": "macos",
	})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func connectHost(t *testing.T, base string, priv ed25519.PrivateKey) (*websocket.Conn, string) {
	t.Helper()
	c := authHost(t, base, priv, priv)
	var ready struct{ T, Host, User string }
	if err := c.ReadJSON(&ready); err != nil || ready.T != "ready" {
		t.Fatalf("ready: %+v %v", ready, err)
	}
	if want := hostID(priv); ready.Host != want {
		t.Fatalf("host id %q, want %q", ready.Host, want)
	}
	if ready.User != aliceDaemon.UserID.String() {
		t.Fatalf("ready user %q, want %s", ready.User, aliceDaemon.UserID)
	}
	return c, ready.Host
}

func readFrame(t *testing.T, c *websocket.Conn) (byte, uint32, []byte) {
	t.Helper()
	mt, data, err := c.ReadMessage()
	if err != nil {
		t.Fatalf("host read: %v", err)
	}
	if mt != websocket.BinaryMessage || len(data) < 5 {
		t.Fatalf("bad frame: type %d len %d", mt, len(data))
	}
	return data[0], binary.BigEndian.Uint32(data[1:5]), data[5:]
}

func expectFrame(t *testing.T, c *websocket.Conn, kind byte, sid uint32, payload []byte) {
	t.Helper()
	k, s, p := readFrame(t, c)
	if k != kind || s != sid || !bytes.Equal(p, payload) {
		t.Fatalf("frame = (%d, %d, %d bytes), want (%d, %d, %d bytes)", k, s, len(p), kind, sid, len(payload))
	}
}

func expectClose(t *testing.T, c *websocket.Conn, code int) {
	t.Helper()
	for {
		_, _, err := c.ReadMessage()
		if err == nil {
			continue
		}
		var ce *websocket.CloseError
		if !errors.As(err, &ce) {
			t.Fatalf("want close %d, got %v", code, err)
		}
		if ce.Code != code {
			t.Fatalf("close code %d (%q), want %d", ce.Code, ce.Text, code)
		}
		return
	}
}

// openStream connects alice's phone and returns it with the sid the host saw.
func openStream(t *testing.T, base, id string, h *websocket.Conn) (*websocket.Conn, uint32) {
	t.Helper()
	c := dial(t, base+"/connect?host="+id)
	return c, expectOpen(t, h, alice)
}

// expectOpen reads an open frame whose payload names the caller.
func expectOpen(t *testing.T, h *websocket.Conn, caller *ent.OAuthAuthorization) uint32 {
	t.Helper()
	k, sid, p := readFrame(t, h)
	if k != kindOpen || sid == 0 {
		t.Fatalf("open frame = (%d, %d, %d bytes)", k, sid, len(p))
	}
	var got map[string]string
	if err := json.Unmarshal(p, &got); err != nil {
		t.Fatalf("open payload %q: %v", p, err)
	}
	want := map[string]string{"user": caller.UserID.String(), "authz": caller.ID.String(), "client": caller.ClientID, "name": caller.Name}
	if len(got) != len(want) {
		t.Fatalf("open payload = %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("open payload = %v, want %v", got, want)
		}
	}
	return sid
}

func TestHostID(t *testing.T) {
	pub := ed25519.PublicKey(bytes.Repeat([]byte{7}, 32))
	sum := sha256.Sum256(pub)
	want := base64.RawURLEncoding.EncodeToString(sum[:16])
	if got := HostID(pub); got != want || len(got) != 22 {
		t.Fatalf("HostID = %q, want %q (22 chars)", got, want)
	}
}

func TestHostAuthRegistersComputerForTokenAccount(t *testing.T) {
	tr := newTestRelay(t, testConfig())
	key := newKey(t)
	_, id := connectHost(t, tr.base, key)
	waitFor(t, "online mark", func() bool { return tr.reg.isOnline(id) })
	tr.reg.mu.Lock()
	got := tr.reg.conns[0]
	tr.reg.mu.Unlock()
	want := biz.ComputerConn{
		HostID: id, UserID: aliceDaemon.UserID, AuthorizationID: aliceDaemon.ID,
		IdentityPub: base64.RawURLEncoding.EncodeToString(key.Public().(ed25519.PublicKey)),
		StaticPub:   testStatic, Name: "studio", Platform: "macos", Version: "test",
	}
	if got != want {
		t.Fatalf("registered %+v, want %+v", got, want)
	}
}

// Without a valid token the upgrade is refused with HTTP 401, on both
// routes.
func TestRelayNeedsToken(t *testing.T) {
	base := newRelay(t, testConfig())
	for _, path := range []string{"/host?host=x", "/connect?host=x"} {
		for _, h := range []http.Header{nil, {"Authorization": {"Bearer nobody"}}} {
			_, resp, err := websocket.DefaultDialer.Dial(base+path, h)
			if err == nil || resp == nil || resp.StatusCode != http.StatusUnauthorized {
				t.Fatalf("%s %v: %v %v, want 401", path, h, resp, err)
			}
		}
	}
}

// Browsers offer the token as a subprotocol and get "jucode-relay" back.
func TestSubprotocolToken(t *testing.T) {
	base := newRelay(t, testConfig())
	h, id := connectHost(t, base, newKey(t))
	d := websocket.Dialer{Subprotocols: []string{"jucode-relay", "jucode-bearer.alice"}}
	c, resp, err := d.Dial(base+"/connect?host="+id, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()
	if c.Subprotocol() != "jucode-relay" || resp.Header.Get("Sec-WebSocket-Protocol") != "jucode-relay" {
		t.Fatalf("subprotocol %q", c.Subprotocol())
	}
	expectOpen(t, h, alice)

	d.Subprotocols = []string{"jucode-relay", "jucode-bearer.nobody"}
	if _, resp, err := d.Dial(base+"/connect?host="+id, nil); err == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("bad subprotocol token: %v %v, want 401", resp, err)
	}
}

// The host query (HAProxy hashes on it) must name the authenticating key.
func TestHostQueryMustMatchKey(t *testing.T) {
	base := newRelay(t, testConfig())
	key := newKey(t)
	c := authHostAt(t, base+"/host?host="+hostID(newKey(t)), "daemon", key, key)
	expectClose(t, c, CloseProtocol)
}

func TestHostAuthBadStaticKey(t *testing.T) {
	base := newRelay(t, testConfig())
	key := newKey(t)
	c := dialAs(t, base+"/host?host="+hostID(key), "daemon")
	var ch struct{ T, Nonce string }
	if err := c.ReadJSON(&ch); err != nil {
		t.Fatal(err)
	}
	nonce, _ := base64.RawURLEncoding.DecodeString(ch.Nonce)
	_ = c.WriteJSON(map[string]string{
		"t": "auth", "pub": base64.RawURLEncoding.EncodeToString(key.Public().(ed25519.PublicKey)),
		"sig": base64.RawURLEncoding.EncodeToString(ed25519.Sign(key, append([]byte("jucode-relay-v1:"), nonce...))),
		"v":   "test", "static": "short",
	})
	expectClose(t, c, CloseUnauthorized)
}

// Clients reach only their own account's computers: unknown hosts and
// hosts of another account close 4403 after the upgrade.
func TestConnectOnlyOwnComputers(t *testing.T) {
	tr := newTestRelay(t, testConfig())
	h, id := connectHost(t, tr.base, newKey(t))
	expectClose(t, dialAs(t, tr.base+"/connect?host="+id, "bob"), CloseForbidden)
	expectClose(t, dial(t, tr.base+"/connect?host=nobody"), CloseForbidden)

	// The owner is the registry's, read on every connect: once the computer
	// moves to bob's account (its daemon signed in there), alice is out.
	tr.reg.mu.Lock()
	tr.reg.owners[id] = bob.UserID
	tr.reg.mu.Unlock()
	expectClose(t, dial(t, tr.base+"/connect?host="+id), CloseForbidden)
	dialAs(t, tr.base+"/connect?host="+id, "bob")
	expectOpen(t, h, bob)
}

func TestDrainClosesSockets(t *testing.T) {
	tr := newTestRelay(t, testConfig())
	h, id := connectHost(t, tr.base, newKey(t))
	c, _ := openStream(t, tr.base, id, h)
	waitFor(t, "online mark", func() bool { return tr.reg.isOnline(id) })
	tr.srv.Drain()
	expectClose(t, h, CloseRestarting)
	expectClose(t, c, CloseRestarting)
	waitFor(t, "online mark cleared", func() bool { return !tr.reg.isOnline(id) })
	// Drain is not shutdown: the host may come back (stop drain / rollback).
	connectHost(t, tr.base, newKey(t))
}

func TestKickClosesHost(t *testing.T) {
	tr := newTestRelay(t, testConfig())
	h, id := connectHost(t, tr.base, newKey(t))
	tr.srv.Kick(id)
	expectClose(t, h, websocket.CloseNormalClosure)
}

// A revoked phone loses its open streams; a revoked daemon its host socket.
func TestKickAuthzClosesRevokedSockets(t *testing.T) {
	tr := newTestRelay(t, testConfig())
	h, id := connectHost(t, tr.base, newKey(t))
	c, _ := openStream(t, tr.base, id, h)
	tr.srv.KickAuthz(alice.ID)
	expectClose(t, c, CloseUnauthorized)
	tr.srv.KickAuthz(aliceDaemon.ID)
	expectClose(t, h, CloseUnauthorized)
}

func TestPresenceRefreshStoresLastSeen(t *testing.T) {
	cfg := testConfig()
	cfg.PingInterval = 20 * time.Millisecond
	cfg.SeenInterval = 50 * time.Millisecond
	tr := newTestRelay(t, cfg)
	connectHost(t, tr.base, newKey(t))
	waitFor(t, "last_seen refresh", func() bool {
		tr.reg.mu.Lock()
		defer tr.reg.mu.Unlock()
		return tr.reg.seen >= 2
	})
}

func TestHostAuthBadSignature(t *testing.T) {
	base := newRelay(t, testConfig())
	c := authHost(t, base, newKey(t), newKey(t))
	expectClose(t, c, CloseUnauthorized)
}

func TestHostAuthTimeout(t *testing.T) {
	cfg := testConfig()
	cfg.AuthTimeout = 200 * time.Millisecond
	base := newRelay(t, cfg)
	c := dial(t, base+"/host")
	if _, _, err := c.ReadMessage(); err != nil { // challenge
		t.Fatal(err)
	}
	expectClose(t, c, CloseUnauthorized)
}

func TestHostOffline(t *testing.T) {
	tr := newTestRelay(t, testConfig())
	tr.reg.owners["registered"] = alice.UserID
	c := dial(t, tr.base+"/connect?host=registered")
	expectClose(t, c, CloseHostOffline)
}

func TestForwarding(t *testing.T) {
	base := newRelay(t, testConfig())
	h, id := connectHost(t, base, newKey(t))
	c1, sid1 := openStream(t, base, id, h)
	c2, sid2 := openStream(t, base, id, h)
	if sid1 == sid2 {
		t.Fatalf("duplicate sid %d", sid1)
	}

	if err := c1.WriteMessage(websocket.BinaryMessage, []byte("hello")); err != nil {
		t.Fatal(err)
	}
	expectFrame(t, h, kindData, sid1, []byte("hello"))
	if err := c2.WriteMessage(websocket.BinaryMessage, []byte{}); err != nil {
		t.Fatal(err)
	}
	expectFrame(t, h, kindData, sid2, []byte{})

	if err := h.WriteMessage(websocket.BinaryMessage, frame(kindData, sid2, []byte("world"))); err != nil {
		t.Fatal(err)
	}
	mt, p, err := c2.ReadMessage()
	if err != nil || mt != websocket.BinaryMessage || string(p) != "world" {
		t.Fatalf("client read: %d %q %v", mt, p, err)
	}
	// Data for an unknown sid is dropped, not fatal.
	if err := h.WriteMessage(websocket.BinaryMessage, frame(kindData, 9999, []byte("x"))); err != nil {
		t.Fatal(err)
	}
	if err := h.WriteMessage(websocket.BinaryMessage, frame(kindData, sid1, []byte("still"))); err != nil {
		t.Fatal(err)
	}
	if _, p, err := c1.ReadMessage(); err != nil || string(p) != "still" {
		t.Fatalf("client read: %q %v", p, err)
	}
}

// TestTerminalBurstWithinWindow: one daemon terminal flow-control window
// (512 KiB raw, about 16 Noise messages of base64 JSON) sent back to back
// fits the per-client queue, so no relay change is needed for terminals.
func TestTerminalBurstWithinWindow(t *testing.T) {
	base := newRelay(t, testConfig())
	h, id := connectHost(t, base, newKey(t))
	c, sid := openStream(t, base, id, h)

	for i := range 16 {
		if err := h.WriteMessage(websocket.BinaryMessage, frame(kindData, sid, bytes.Repeat([]byte{byte(i)}, 44<<10))); err != nil {
			t.Fatal(err)
		}
	}
	for i := range 16 {
		_, p, err := c.ReadMessage()
		if err != nil {
			t.Fatalf("message %d: %v", i, err)
		}
		if !bytes.Equal(p, bytes.Repeat([]byte{byte(i)}, 44<<10)) {
			t.Fatalf("message %d out of order or truncated (%d bytes)", i, len(p))
		}
	}
	// The stream is still open both ways.
	if err := c.WriteMessage(websocket.BinaryMessage, []byte("ack")); err != nil {
		t.Fatal(err)
	}
	expectFrame(t, h, kindData, sid, []byte("ack"))
}

func TestHostReplacement(t *testing.T) {
	tr := newTestRelay(t, testConfig())
	base := tr.base
	key := newKey(t)
	h1, id := connectHost(t, base, key)
	c, _ := openStream(t, base, id, h1)

	h2, _ := connectHost(t, base, key)
	expectClose(t, h1, CloseReplaced)
	expectClose(t, c, CloseHostOffline)
	time.Sleep(50 * time.Millisecond)
	if !tr.reg.isOnline(id) {
		t.Fatal("the replaced connection cleared the online mark")
	}

	c2, sid := openStream(t, base, id, h2)
	if err := c2.WriteMessage(websocket.BinaryMessage, []byte("hi")); err != nil {
		t.Fatal(err)
	}
	expectFrame(t, h2, kindData, sid, []byte("hi"))
}

func TestHostClosesStream(t *testing.T) {
	base := newRelay(t, testConfig())
	h, id := connectHost(t, base, newKey(t))
	c, sid := openStream(t, base, id, h)
	if err := h.WriteMessage(websocket.BinaryMessage, frame(kindClose, sid, nil)); err != nil {
		t.Fatal(err)
	}
	expectClose(t, c, CloseHostClosed)
}

// A daemon refuses a device with a last message, then closes the stream:
// the client must get the message before the close.
func TestHostCloseDeliversQueuedData(t *testing.T) {
	base := newRelay(t, testConfig())
	h, id := connectHost(t, base, newKey(t))
	for i := 0; i < 20; i++ {
		c, sid := openStream(t, base, id, h)
		if err := h.WriteMessage(websocket.BinaryMessage, frame(kindData, sid, []byte("refused"))); err != nil {
			t.Fatal(err)
		}
		if err := h.WriteMessage(websocket.BinaryMessage, frame(kindClose, sid, nil)); err != nil {
			t.Fatal(err)
		}
		if _, p, err := c.ReadMessage(); err != nil || string(p) != "refused" {
			t.Fatalf("attempt %d: client read %q %v", i, p, err)
		}
		expectClose(t, c, CloseHostClosed)
		c.Close()
	}
}

func TestClientClosesStream(t *testing.T) {
	base := newRelay(t, testConfig())
	h, id := connectHost(t, base, newKey(t))
	c, sid := openStream(t, base, id, h)
	c.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
	expectFrame(t, h, kindClose, sid, nil)
}

func TestHostDisconnectClosesStreams(t *testing.T) {
	base := newRelay(t, testConfig())
	h, id := connectHost(t, base, newKey(t))
	c, _ := openStream(t, base, id, h)
	h.Close()
	expectClose(t, c, CloseHostOffline)
	expectClose(t, dial(t, base+"/connect?host="+id), CloseHostOffline)
}

func TestStreamLimit(t *testing.T) {
	cfg := testConfig()
	cfg.MaxStreams = 2
	base := newRelay(t, cfg)
	h, id := connectHost(t, base, newKey(t))
	c1, _ := openStream(t, base, id, h)
	openStream(t, base, id, h)
	expectClose(t, dial(t, base+"/connect?host="+id), CloseTooMany)

	// Closing one frees a slot.
	c1.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
	if k, _, _ := readFrame(t, h); k != kindClose {
		t.Fatalf("want close frame, got kind %d", k)
	}
	openStream(t, base, id, h)
}

func TestMessageSizeLimit(t *testing.T) {
	base := newRelay(t, testConfig())
	h, id := connectHost(t, base, newKey(t))
	c, sid := openStream(t, base, id, h)

	max := make([]byte, 1<<20)
	if err := c.WriteMessage(websocket.BinaryMessage, max); err != nil {
		t.Fatal(err)
	}
	expectFrame(t, h, kindData, sid, max)
	if err := h.WriteMessage(websocket.BinaryMessage, frame(kindData, sid, max)); err != nil {
		t.Fatal(err)
	}
	if _, p, err := c.ReadMessage(); err != nil || len(p) != len(max) {
		t.Fatalf("client read %d bytes: %v", len(p), err)
	}

	if err := c.WriteMessage(websocket.BinaryMessage, make([]byte, 1<<20+1)); err != nil {
		t.Fatal(err)
	}
	expectClose(t, c, websocket.CloseMessageTooBig)
	expectFrame(t, h, kindClose, sid, nil)

	// A host frame over the limit drops the host.
	if err := h.WriteMessage(websocket.BinaryMessage, frame(kindData, 1, make([]byte, 1<<20+1))); err != nil {
		t.Fatal(err)
	}
	expectClose(t, h, websocket.CloseMessageTooBig)
}

func TestTextFrameRejected(t *testing.T) {
	base := newRelay(t, testConfig())
	h, id := connectHost(t, base, newKey(t))
	c, sid := openStream(t, base, id, h)
	if err := c.WriteMessage(websocket.TextMessage, []byte("hi")); err != nil {
		t.Fatal(err)
	}
	expectClose(t, c, CloseProtocol)
	expectFrame(t, h, kindClose, sid, nil)
}

func TestIdleStream(t *testing.T) {
	cfg := testConfig()
	cfg.IdleTimeout = 200 * time.Millisecond
	base := newRelay(t, cfg)
	h, id := connectHost(t, base, newKey(t))
	c, sid := openStream(t, base, id, h)
	expectClose(t, c, CloseIdle)
	expectFrame(t, h, kindClose, sid, nil)
}

func TestClientRateLimit(t *testing.T) {
	cfg := testConfig()
	cfg.ClientConnectsPerMin = 2
	base := newRelay(t, cfg)
	expectClose(t, dial(t, base+"/connect?host=x"), CloseForbidden)
	expectClose(t, dial(t, base+"/connect?host=x"), CloseForbidden)
	expectClose(t, dial(t, base+"/connect?host=x"), CloseTooMany)
}
