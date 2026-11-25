package api

import (
	"context"
	"crypto/ed25519"
	"database/sql"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/alicebob/miniredis/v2"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/redis/go-redis/v9"

	"github.com/jucode/jucode/internal/biz"
	"github.com/jucode/jucode/internal/cache"
	"github.com/jucode/jucode/internal/data"
	"github.com/jucode/jucode/internal/data/ent"
	entuser "github.com/jucode/jucode/internal/data/ent/user"
	jwtpkg "github.com/jucode/jucode/internal/pkg/jwt"
	"github.com/jucode/jucode/internal/relay"
)

// TestRelayThroughRouter wires the relay as production does: real OAuth
// device tokens, the computers table, Redis presence, the account API and
// /internal/drain/start.
func TestRelayThroughRouter(t *testing.T) {
	dsn := os.Getenv("JUCODE_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("JUCODE_TEST_PG_DSN not set; skipping Postgres-backed relay route test")
	}
	ctx := context.Background()
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	client := ent.NewClient(ent.Driver(entsql.OpenDB(dialect.Postgres, db)))
	if err := client.Schema.Create(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(func() { _ = client.Close(); _ = db.Close() })
	rdb := redis.NewClient(&redis.Options{Addr: miniredis.RunT(t).Addr()})
	d := data.NewWith(client, rdb, db)
	settings := biz.NewSettingsSvc(d)
	signer := jwtpkg.NewSigner("relay-route-test-secret-32-bytes!!", time.Hour, 24*time.Hour)
	oauth := biz.NewOAuthSvc(d, signer)
	computers := biz.NewComputerSvc(d)
	rs := relay.New(relay.DefaultConfig(), computers, slog.New(slog.NewTextHandler(io.Discard, nil)))
	computers.SetKick(rs.Kick)
	t.Setenv("INTERNAL_API_TOKEN", "internal-test")
	ts := httptest.NewServer(NewRouter(Deps{
		Data: d, Signer: signer, Account: biz.NewAccount(d, signer, settings), Settings: settings,
		Audit: biz.NewAuditSvc(d), Cache: cache.New(rdb), OAuth: oauth, Drain: biz.NewDrainSvc(),
		Computers: computers, Relay: rs,
	}))
	t.Cleanup(func() { rs.Close(); ts.Close() })
	base := "ws" + strings.TrimPrefix(ts.URL, "http") + "/v1/relay"

	device := func(email, clientID, name string) (*ent.OAuthAuthorization, string) {
		u, err := client.User.Create().SetEmail(email).SetPasswordHash("x").SetStatus(entuser.StatusActive).Save(ctx)
		if err != nil {
			t.Fatal(err)
		}
		authz, tokens, err := oauth.CreateDevice(ctx, u.ID, clientID, name, "", "")
		if err != nil {
			t.Fatal(err)
		}
		return authz, tokens.AccessToken
	}
	daemon, daemonToken := device(uuid.NewString()+"@test.invalid", "jucode-cli", "Mac")
	_, phoneTokens, err := oauth.CreateDevice(ctx, daemon.UserID, "jucode-desktop", "Laptop", "", "")
	if err != nil {
		t.Fatal(err)
	}
	phoneToken := phoneTokens.AccessToken
	_, strangerToken := device(uuid.NewString()+"@test.invalid", "jucode-web", "网页")

	// The daemon connects with its token and becomes the account's computer.
	_, key, _ := ed25519.GenerateKey(nil)
	hostID := relay.HostID(key.Public().(ed25519.PublicKey))
	host, _, err := websocket.DefaultDialer.Dial(base+"/host?host="+hostID, http.Header{"Authorization": {"Bearer " + daemonToken}})
	if err != nil {
		t.Fatalf("host dial: %v", err)
	}
	defer host.Close()
	host.SetReadDeadline(time.Now().Add(5 * time.Second))
	var ch struct{ Nonce string }
	if err := host.ReadJSON(&ch); err != nil {
		t.Fatal(err)
	}
	nonce, _ := base64.RawURLEncoding.DecodeString(ch.Nonce)
	static := base64.RawURLEncoding.EncodeToString(make([]byte, 32))
	_ = host.WriteJSON(map[string]string{
		"t": "auth", "pub": base64.RawURLEncoding.EncodeToString(key.Public().(ed25519.PublicKey)),
		"sig": base64.RawURLEncoding.EncodeToString(ed25519.Sign(key, append([]byte("jucode-relay-v1:"), nonce...))),
		"v":   "0.5.0", "static": static, "name": "studio", "platform": "macos",
	})
	var ready struct{ T, Host, User string }
	if err := host.ReadJSON(&ready); err != nil || ready.Host != hostID || ready.User != daemon.UserID.String() {
		t.Fatalf("ready = %+v %v", ready, err)
	}

	list := func(token string) []biz.ComputerView {
		req, _ := http.NewRequest(http.MethodGet, ts.URL+"/v1/oauth/computers", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil || resp.StatusCode != http.StatusOK {
			t.Fatalf("list: %v %v", resp, err)
		}
		defer resp.Body.Close()
		var out struct{ Computers []biz.ComputerView }
		_ = json.NewDecoder(resp.Body).Decode(&out)
		return out.Computers
	}
	if got := list(phoneToken); len(got) != 1 || got[0].HostID != hostID || !got[0].Online || got[0].StaticPub != static || got[0].Name != "studio" {
		t.Fatalf("phone lists %+v", got)
	}
	if got := list(strangerToken); len(got) != 0 {
		t.Fatalf("stranger lists %+v", got)
	}

	// Another account is refused; the phone, with the browser-style
	// subprotocol token, gets a stream whose open names it.
	stranger, _, err := websocket.DefaultDialer.Dial(base+"/connect?host="+hostID, http.Header{"Authorization": {"Bearer " + strangerToken}})
	if err != nil {
		t.Fatal(err)
	}
	expectCloseCode(t, stranger, relay.CloseForbidden)
	d2 := websocket.Dialer{Subprotocols: []string{"jucode-relay", "jucode-bearer." + phoneToken}}
	phone, _, err := d2.Dial(base+"/connect?host="+hostID, nil)
	if err != nil {
		t.Fatalf("phone dial: %v", err)
	}
	defer phone.Close()
	_, frame, err := host.ReadMessage()
	if err != nil || frame[0] != 1 || binary.BigEndian.Uint32(frame[1:5]) == 0 {
		t.Fatalf("open frame %v %v", frame, err)
	}
	var open map[string]string
	if err := json.Unmarshal(frame[5:], &open); err != nil || open["user"] != daemon.UserID.String() || open["client"] != "jucode-desktop" || open["name"] != "Laptop" {
		t.Fatalf("open payload %s %v", frame[5:], err)
	}
	// A session JWT is not a device token.
	session, _, _, _ := signer.IssueAccess(daemon.UserID, "user")
	if _, resp, err := websocket.DefaultDialer.Dial(base+"/connect?host="+hostID, http.Header{"Authorization": {"Bearer " + session}}); err == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("session token: %v %v, want 401", resp, err)
	}

	// Draining the instance closes both sockets with 4503 and clears the
	// online mark.
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/internal/drain/start", nil)
	req.Header.Set("Authorization", "Bearer internal-test")
	if resp, err := http.DefaultClient.Do(req); err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("drain: %v %v", resp, err)
	}
	expectCloseCode(t, host, relay.CloseRestarting)
	expectCloseCode(t, phone, relay.CloseRestarting)
	for end := time.Now().Add(3 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		if exists, _ := rdb.Exists(ctx, "relay:online:"+hostID).Result(); exists == 0 {
			break
		}
		if time.Now().After(end) {
			t.Fatal("online mark kept after drain")
		}
	}
}

func expectCloseCode(t *testing.T, c *websocket.Conn, code int) {
	t.Helper()
	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	for {
		_, _, err := c.ReadMessage()
		if err == nil {
			continue
		}
		var ce *websocket.CloseError
		if !errors.As(err, &ce) || ce.Code != code {
			t.Fatalf("want close %d, got %v", code, err)
		}
		return
	}
}
