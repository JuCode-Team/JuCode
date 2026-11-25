package biz

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"

	"github.com/jucode/jucode/internal/data"
	entcomputer "github.com/jucode/jucode/internal/data/ent/computer"
)

// DB-backed, see agentUsageTestData:
//
//	JUCODE_TEST_PG_DSN=... go test ./internal/biz -run Computer
func TestComputerSvcPG(t *testing.T) {
	pg, u := agentUsageTestData(t)
	_, other := agentUsageTestData(t)
	ctx := context.Background()
	mr := miniredis.RunT(t)
	d := data.NewWith(pg.Ent, redis.NewClient(&redis.Options{Addr: mr.Addr()}), pg.DB())
	svc := NewComputerSvc(d)
	var kicked []string
	svc.SetKick(func(id string) { kicked = append(kicked, id) })

	host := uuid.NewString()[:22]
	conn := ComputerConn{
		HostID: host, UserID: u.ID, AuthorizationID: uuid.New(),
		IdentityPub: "id", StaticPub: "static1", Name: "studio", Platform: "macos", Version: "0.5.0",
	}
	if err := svc.Register(ctx, conn); err != nil {
		t.Fatal(err)
	}
	if owner, ok, err := svc.Owner(ctx, host); err != nil || !ok || owner != u.ID {
		t.Fatalf("owner = %v %v %v", owner, ok, err)
	}
	if _, ok, _ := svc.Owner(ctx, "unknown"); ok {
		t.Fatal("unknown host has an owner")
	}

	// Offline until the relay marks it; the list shows it either way.
	list, err := svc.List(ctx, u.ID)
	if err != nil || len(list) != 1 || list[0].Online || list[0].Name != "studio" || list[0].StaticPub != "static1" {
		t.Fatalf("list = %+v %v", list, err)
	}
	if err := svc.Alive(ctx, host, true); err != nil {
		t.Fatal(err)
	}
	if list, _ = svc.List(ctx, u.ID); !list[0].Online {
		t.Fatal("not online after Alive")
	}
	if ttl := mr.TTL(onlineKey(host)); ttl != computerOnlineTTL {
		t.Fatalf("online ttl = %v", ttl)
	}

	// A rename sticks across reconnects; the daemon's new keys and version
	// still land.
	if v, err := svc.Rename(ctx, u.ID, host, "  工作室  "); err != nil || v.Name != "工作室" || !v.Online {
		t.Fatalf("rename = %+v %v", v, err)
	}
	conn.Name, conn.StaticPub, conn.Version = "studio.local", "static2", "0.5.1"
	if err := svc.Register(ctx, conn); err != nil {
		t.Fatal(err)
	}
	list, _ = svc.List(ctx, u.ID)
	if list[0].Name != "工作室" || list[0].StaticPub != "static2" || list[0].Version != "0.5.1" {
		t.Fatalf("after reconnect = %+v", list[0])
	}
	// An empty name hands naming back to the daemon.
	if _, err := svc.Rename(ctx, u.ID, host, ""); err != nil {
		t.Fatal(err)
	}
	if err := svc.Register(ctx, conn); err != nil {
		t.Fatal(err)
	}
	if list, _ = svc.List(ctx, u.ID); list[0].Name != "studio.local" {
		t.Fatalf("name after reset = %q", list[0].Name)
	}
	if _, err := svc.Rename(ctx, other.ID, host, "x"); !errors.Is(err, ErrComputerNotFound) {
		t.Fatalf("renaming another account's computer: %v", err)
	}

	// Signing the daemon in to another account moves the computer, without
	// the old account's custom name.
	if _, err := svc.Rename(ctx, u.ID, host, "工作室"); err != nil {
		t.Fatal(err)
	}
	conn.UserID = other.ID
	if err := svc.Register(ctx, conn); err != nil {
		t.Fatal(err)
	}
	if list, _ = svc.List(ctx, u.ID); len(list) != 0 {
		t.Fatalf("old account still lists %+v", list)
	}
	if list, _ = svc.List(ctx, other.ID); len(list) != 1 || list[0].Name != "studio.local" {
		t.Fatalf("new account lists %+v", list)
	}

	// Gone only clears the mark this instance set.
	mr.Set(onlineKey(host), "another-instance")
	if err := svc.Gone(ctx, host); err != nil || !mr.Exists(onlineKey(host)) {
		t.Fatalf("cleared another instance's mark: %v", err)
	}
	if err := svc.Alive(ctx, host, false); err != nil {
		t.Fatal(err)
	}
	if err := svc.Gone(ctx, host); err != nil || mr.Exists(onlineKey(host)) {
		t.Fatalf("own mark kept: %v", err)
	}

	if err := svc.Delete(ctx, u.ID, host); !errors.Is(err, ErrComputerNotFound) {
		t.Fatalf("deleting another account's computer: %v", err)
	}
	if err := svc.Delete(ctx, other.ID, host); err != nil || len(kicked) != 1 || kicked[0] != host {
		t.Fatalf("delete: %v, kicked %v", err, kicked)
	}
	if _, ok, _ := svc.Owner(ctx, host); ok {
		t.Fatal("deleted computer still has an owner")
	}
}

func TestComputerListNewestFirstPG(t *testing.T) {
	pg, u := agentUsageTestData(t)
	ctx := context.Background()
	d := data.NewWith(pg.Ent, redis.NewClient(&redis.Options{Addr: miniredis.RunT(t).Addr()}), pg.DB())
	svc := NewComputerSvc(d)
	older, newer := uuid.NewString()[:22], uuid.NewString()[:22]
	for _, h := range []string{older, newer} {
		if err := svc.Register(ctx, ComputerConn{HostID: h, UserID: u.ID, AuthorizationID: uuid.New(), IdentityPub: "i", StaticPub: "s"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := pg.Ent.Computer.Update().Where(entcomputer.UserIDEQ(u.ID)).SetLastSeenAt(time.Now().Add(-time.Hour)).Exec(ctx); err != nil {
		t.Fatal(err)
	}
	if err := svc.Alive(ctx, newer, true); err != nil {
		t.Fatal(err)
	}
	list, err := svc.List(ctx, u.ID)
	if err != nil || len(list) != 2 || list[0].HostID != newer || !list[0].Online || list[1].Online {
		t.Fatalf("list = %+v %v", list, err)
	}
}
