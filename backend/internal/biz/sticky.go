package biz

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"

	"github.com/jucode/jucode/internal/data"
)

// StickySvc keeps a conversation's later turns on the model group that served
// it, so a fresh ranking cannot cold-start the upstream's prompt cache. The
// gateway reads the assignment before routing (LookupConversation) and writes
// it after a successful response (RememberConversation).
type StickySvc struct {
	d        *data.Data
	settings *SettingsSvc
}

func NewStickySvc(d *data.Data, settings *SettingsSvc) *StickySvc {
	return &StickySvc{d: d, settings: settings}
}

// errStickyDisabled is returned (and silently swallowed by callers) when the
// admin sets sticky_ttl_seconds to 0. The gateway treats it as "no sticky
// available; pick by priority/weight" without logging an error.
var errStickyDisabled = errors.New("sticky disabled")

// IsStickyDisabled reports whether err is the silent disable signal.
func IsStickyDisabled(err error) bool { return errors.Is(err, errStickyDisabled) }

func (s *StickySvc) ttl(ctx context.Context) (int, error) {
	cfg, err := s.settings.GetSite(ctx)
	if err != nil {
		return 0, err
	}
	return cfg.StickyTTLSeconds, nil
}

func stickySessionKey(userID uuid.UUID, conversationID string) string {
	return "sticky:sess:" + userID.String() + ":" + strings.TrimSpace(conversationID)
}

func stickyGroupKey(userID uuid.UUID, model, conversationID string) string {
	return "sticky:grp:" + userID.String() + ":" + strings.TrimSpace(model) + ":" + strings.TrimSpace(conversationID)
}

// conversationAssignment is where a conversation's later turns should land.
// Session is the group models share. Model is set when this model does not
// use that group.
type conversationAssignment struct {
	Session uuid.UUID
	Model   uuid.UUID
}

// nextConversationAssignment keeps a session on its group. moved means the
// group that was tried could not serve the request, so the assignment that
// pointed at it follows the group that did.
func nextConversationAssignment(cur conversationAssignment, chosen uuid.UUID, moved bool) conversationAssignment {
	if chosen == uuid.Nil {
		return cur
	}
	if cur.Session == uuid.Nil {
		return conversationAssignment{Session: chosen}
	}
	if moved {
		if cur.Model != uuid.Nil {
			return conversationAssignment{Session: cur.Session, Model: chosen}
		}
		return conversationAssignment{Session: chosen}
	}
	if chosen == cur.Session {
		return conversationAssignment{Session: cur.Session}
	}
	return conversationAssignment{Session: cur.Session, Model: chosen}
}

// LookupConversation returns the session group and, when this model does not
// use it, the model's own group.
func (s *StickySvc) LookupConversation(ctx context.Context, userID uuid.UUID, model, conversationID string) (uuid.UUID, uuid.UUID, error) {
	if s == nil || s.d == nil || s.d.Redis == nil || strings.TrimSpace(conversationID) == "" || strings.TrimSpace(model) == "" {
		return uuid.Nil, uuid.Nil, nil
	}
	ttl, err := s.ttl(ctx)
	if err != nil {
		return uuid.Nil, uuid.Nil, err
	}
	if ttl <= 0 {
		return uuid.Nil, uuid.Nil, errStickyDisabled
	}
	modelGroup, err := s.readStickyID(ctx, stickyGroupKey(userID, model, conversationID))
	if err != nil {
		return uuid.Nil, uuid.Nil, err
	}
	session, err := s.readStickyID(ctx, stickySessionKey(userID, conversationID))
	if err != nil {
		return uuid.Nil, uuid.Nil, err
	}
	return session, modelGroup, nil
}

func (s *StickySvc) readStickyID(ctx context.Context, key string) (uuid.UUID, error) {
	v, err := s.d.Redis.Get(ctx, key).Result()
	if err != nil {
		if errors.Is(err, redis.Nil) {
			return uuid.Nil, nil
		}
		return uuid.Nil, err
	}
	id, err := uuid.Parse(strings.TrimSpace(v))
	if err != nil {
		return uuid.Nil, nil
	}
	return id, nil
}

// RememberConversation records the group that just served this turn.
// moved is true when an earlier group in this request failed outright.
func (s *StickySvc) RememberConversation(ctx context.Context, userID, groupID uuid.UUID, model, conversationID string, moved bool) error {
	if s == nil || s.d == nil || s.d.Redis == nil || groupID == uuid.Nil || strings.TrimSpace(conversationID) == "" || strings.TrimSpace(model) == "" {
		return nil
	}
	ttl, err := s.ttl(ctx)
	if err != nil || ttl <= 0 {
		return err
	}
	session, modelGroup, err := s.LookupConversation(ctx, userID, model, conversationID)
	if err != nil {
		return err
	}
	next := nextConversationAssignment(conversationAssignment{Session: session, Model: modelGroup}, groupID, moved)
	dur := time.Duration(ttl) * time.Second
	pipe := s.d.Redis.Pipeline()
	pipe.Set(ctx, stickySessionKey(userID, conversationID), next.Session.String(), dur)
	modelKey := stickyGroupKey(userID, model, conversationID)
	if next.Model == uuid.Nil {
		pipe.Del(ctx, modelKey)
	} else {
		pipe.Set(ctx, modelKey, next.Model.String(), dur)
	}
	_, err = pipe.Exec(ctx)
	return err
}
