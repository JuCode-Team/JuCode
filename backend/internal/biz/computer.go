package biz

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
	"unicode/utf8"

	entsql "entgo.io/ent/dialect/sql"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"

	"github.com/jucode/jucode/internal/data"
	"github.com/jucode/jucode/internal/data/ent"
	entcomputer "github.com/jucode/jucode/internal/data/ent/computer"
)

// ComputerSvc keeps the account's computers: desktop daemons that connect to
// the relay (internal/relay) with the account's OAuth token. A computer
// belongs to the account of its latest host connection. Online state lives
// in Redis (relay:online:<host_id> = the instance holding the socket), kept
// alive by the relay while the host is connected.

const (
	ComputerNameMaxRune = 64
	computerOnlineTTL   = 90 * time.Second
)

var (
	ErrComputerNotFound = errors.New("computer not found")
	ErrComputerInvalid  = errors.New("invalid computer")
)

// computerGoneLua deletes the online mark only while this instance holds it,
// so a host that already reconnected to another instance stays online.
var computerGoneLua = redis.NewScript(`
if redis.call("GET", KEYS[1]) == ARGV[1] then return redis.call("DEL", KEYS[1]) end
return 0`)

// ComputerConn is what a host connection tells about the computer.
type ComputerConn struct {
	HostID          string
	UserID          uuid.UUID
	AuthorizationID uuid.UUID
	IdentityPub     string
	StaticPub       string
	Name            string
	Platform        string
	Version         string
}

// ComputerView is a computer as the account API returns it.
type ComputerView struct {
	HostID     string    `json:"host_id"`
	Name       string    `json:"name"`
	Platform   string    `json:"platform"`
	Version    string    `json:"version"`
	StaticPub  string    `json:"static_pub"`
	Online     bool      `json:"online"`
	LastSeenAt time.Time `json:"last_seen_at"`
	CreatedAt  time.Time `json:"created_at"`
}

type ComputerSvc struct {
	d        *data.Data
	instance string
	kick     func(hostID string)
}

func NewComputerSvc(d *data.Data) *ComputerSvc {
	host, _ := os.Hostname()
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return &ComputerSvc{d: d, instance: fmt.Sprintf("%s-%d-%s", host, os.Getpid(), hex.EncodeToString(b))}
}

// SetKick sets how Delete closes a computer's relay socket on this instance.
func (s *ComputerSvc) SetKick(kick func(hostID string)) { s.kick = kick }

func onlineKey(hostID string) string { return "relay:online:" + hostID }

func clipRunes(v string, n int) string {
	v = strings.ToValidUTF8(strings.TrimSpace(v), "")
	if utf8.RuneCountInString(v) <= n {
		return v
	}
	return string([]rune(v)[:n])
}

// Register records a host connection: the computer now belongs to c.UserID.
// The daemon's name replaces the stored one unless the same account renamed
// it; a computer moving to another account loses the custom name.
func (s *ComputerSvc) Register(ctx context.Context, c ComputerConn) error {
	sameOwnerCustom := `"computers"."name_custom" AND "computers"."user_id" = "excluded"."user_id"`
	return s.d.Ent.Computer.Create().
		SetHostID(c.HostID).
		SetUserID(c.UserID).
		SetAuthorizationID(c.AuthorizationID).
		SetIdentityPub(c.IdentityPub).
		SetStaticPub(c.StaticPub).
		SetName(clipRunes(c.Name, 128)).
		SetPlatform(clipRunes(c.Platform, 16)).
		SetVersion(clipRunes(c.Version, 64)).
		SetLastSeenAt(time.Now()).
		OnConflict(
			entsql.ConflictColumns(entcomputer.FieldHostID),
			entsql.ResolveWith(func(u *entsql.UpdateSet) {
				for _, f := range []string{
					entcomputer.FieldUserID, entcomputer.FieldAuthorizationID, entcomputer.FieldIdentityPub,
					entcomputer.FieldStaticPub, entcomputer.FieldPlatform, entcomputer.FieldVersion,
					entcomputer.FieldLastSeenAt,
				} {
					u.SetExcluded(f)
				}
				u.Set(entcomputer.FieldName, entsql.Expr(`CASE WHEN `+sameOwnerCustom+` THEN "computers"."name" ELSE "excluded"."name" END`))
				u.Set(entcomputer.FieldNameCustom, entsql.Expr(sameOwnerCustom))
				u.Set(entcomputer.FieldUpdatedAt, time.Now())
			}),
		).
		Exec(ctx)
}

// Owner returns the account a computer belongs to; ok is false for a host
// never registered (or removed).
func (s *ComputerSvc) Owner(ctx context.Context, hostID string) (uuid.UUID, bool, error) {
	c, err := s.d.Ent.Computer.Query().Where(entcomputer.HostIDEQ(hostID)).Only(ctx)
	if ent.IsNotFound(err) {
		return uuid.Nil, false, nil
	}
	if err != nil {
		return uuid.Nil, false, err
	}
	return c.UserID, true, nil
}

// Alive marks the host online on this instance; seen also stores
// last_seen_at.
func (s *ComputerSvc) Alive(ctx context.Context, hostID string, seen bool) error {
	if err := s.d.Redis.Set(ctx, onlineKey(hostID), s.instance, computerOnlineTTL).Err(); err != nil {
		return err
	}
	if !seen {
		return nil
	}
	return s.d.Ent.Computer.Update().Where(entcomputer.HostIDEQ(hostID)).SetLastSeenAt(time.Now()).Exec(ctx)
}

// Gone clears the online mark when this instance holds it.
func (s *ComputerSvc) Gone(ctx context.Context, hostID string) error {
	return computerGoneLua.Run(ctx, s.d.Redis, []string{onlineKey(hostID)}, s.instance).Err()
}

// List returns userID's computers, most recently seen first.
func (s *ComputerSvc) List(ctx context.Context, userID uuid.UUID) ([]ComputerView, error) {
	rows, err := s.d.Ent.Computer.Query().
		Where(entcomputer.UserIDEQ(userID)).
		Order(ent.Desc(entcomputer.FieldLastSeenAt)).
		All(ctx)
	if err != nil || len(rows) == 0 {
		return []ComputerView{}, err
	}
	keys := make([]string, len(rows))
	for i, c := range rows {
		keys[i] = onlineKey(c.HostID)
	}
	online, err := s.d.Redis.MGet(ctx, keys...).Result()
	if err != nil {
		return nil, err
	}
	out := make([]ComputerView, len(rows))
	for i, c := range rows {
		out[i] = computerView(c, online[i] != nil)
	}
	return out, nil
}

// Rename sets the name the account shows; an empty name hands naming back
// to the daemon, whose hostname replaces it on its next connection.
func (s *ComputerSvc) Rename(ctx context.Context, userID uuid.UUID, hostID, name string) (*ComputerView, error) {
	name = strings.TrimSpace(name)
	if !utf8.ValidString(name) || utf8.RuneCountInString(name) > ComputerNameMaxRune {
		return nil, fmt.Errorf("%w: name exceeds %d characters", ErrComputerInvalid, ComputerNameMaxRune)
	}
	c, err := s.d.Ent.Computer.Query().Where(entcomputer.UserIDEQ(userID), entcomputer.HostIDEQ(hostID)).Only(ctx)
	if ent.IsNotFound(err) {
		return nil, ErrComputerNotFound
	}
	if err != nil {
		return nil, err
	}
	upd := c.Update().SetNameCustom(name != "")
	if name != "" {
		upd.SetName(name)
	}
	if c, err = upd.Save(ctx); err != nil {
		return nil, err
	}
	online, err := s.d.Redis.Exists(ctx, onlineKey(hostID)).Result()
	if err != nil {
		return nil, err
	}
	v := computerView(c, online > 0)
	return &v, nil
}

// Delete removes userID's computer and closes its relay socket here; a
// daemon still signed in registers it again when it reconnects.
func (s *ComputerSvc) Delete(ctx context.Context, userID uuid.UUID, hostID string) error {
	n, err := s.d.Ent.Computer.Delete().Where(entcomputer.UserIDEQ(userID), entcomputer.HostIDEQ(hostID)).Exec(ctx)
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrComputerNotFound
	}
	if s.kick != nil {
		s.kick(hostID)
	}
	return nil
}

func computerView(c *ent.Computer, online bool) ComputerView {
	return ComputerView{
		HostID: c.HostID, Name: c.Name, Platform: c.Platform, Version: c.Version,
		StaticPub: c.StaticPub, Online: online, LastSeenAt: c.LastSeenAt, CreatedAt: c.CreatedAt,
	}
}
