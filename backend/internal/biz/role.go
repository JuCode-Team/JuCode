package biz

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"

	"github.com/jucode/jucode/internal/data"
	"github.com/jucode/jucode/internal/data/ent"
	entuserrole "github.com/jucode/jucode/internal/data/ent/userrole"
)

// Role names used in the user_roles table. The primary `user.role` enum
// (user | admin) is independent — admin always wins, those two values
// never appear in user_roles.
const (
	RoleCS    = "cs"
	RoleAgent = "agent"
)

// ErrInvalidRole is returned for any role name not in the allowed set.
var ErrInvalidRole = errors.New("invalid role")

type RoleSvc struct{ d *data.Data }

func NewRoleSvc(d *data.Data) *RoleSvc { return &RoleSvc{d: d} }

// validRole reports whether r is one of the supported functional roles.
func validRole(r string) bool {
	switch r {
	case RoleCS, RoleAgent:
		return true
	}
	return false
}

// Grant assigns a functional role to a user. Idempotent — the unique
// index on (user_id, role) ensures double-grants are no-ops. grantedBy
// may be uuid.Nil for system / migration paths.
func (s *RoleSvc) Grant(ctx context.Context, userID uuid.UUID, role string, grantedBy uuid.UUID) error {
	if !validRole(role) {
		return ErrInvalidRole
	}
	exists, err := s.d.Ent.UserRole.Query().
		Where(entuserrole.UserIDEQ(userID), entuserrole.RoleEQ(entuserrole.Role(role))).
		Exist(ctx)
	if err != nil {
		return err
	}
	if exists {
		_ = s.invalidateCache(ctx, userID)
		return nil
	}
	q := s.d.Ent.UserRole.Create().
		SetUserID(userID).
		SetRole(entuserrole.Role(role))
	if grantedBy != uuid.Nil {
		q = q.SetGrantedByUserID(grantedBy)
	}
	if _, err := q.Save(ctx); err != nil {
		// In case of a concurrent insert that beats the Exist check, the
		// unique constraint will fire — surface as success.
		if ent.IsConstraintError(err) {
			_ = s.invalidateCache(ctx, userID)
			return nil
		}
		return err
	}
	return s.invalidateCache(ctx, userID)
}

// Revoke removes a functional role. Missing rows are ignored.
func (s *RoleSvc) Revoke(ctx context.Context, userID uuid.UUID, role string) error {
	if !validRole(role) {
		return ErrInvalidRole
	}
	_, err := s.d.Ent.UserRole.Delete().
		Where(entuserrole.UserIDEQ(userID), entuserrole.RoleEQ(entuserrole.Role(role))).
		Exec(ctx)
	if err != nil {
		return err
	}
	return s.invalidateCache(ctx, userID)
}

// ListForUser returns the functional roles assigned to a user. Does NOT
// include "admin" (that lives on user.role). Result is sorted by role
// name for stable display.
func (s *RoleSvc) ListForUser(ctx context.Context, userID uuid.UUID) ([]string, error) {
	rows, err := s.d.Ent.UserRole.Query().
		Where(entuserrole.UserIDEQ(userID)).
		Order(ent.Asc(entuserrole.FieldRole)).
		All(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, string(r.Role))
	}
	return out, nil
}

// HasAny reports whether the user holds any of the given roles. Admin
// (user.role) implicitly counts as every functional role — this single
// call is the source of truth used by the RequireRoleAny middleware.
//
// Cached in Redis for 60s with the same key family as the auth-status
// cache, invalidated on Grant/Revoke and via the existing admin
// user-mutation paths. The cache value is a CSV of role names; we read
// it on every authorised request so it stays hot.
func (s *RoleSvc) HasAny(ctx context.Context, userID uuid.UUID, roles ...string) (bool, error) {
	if len(roles) == 0 {
		return false, nil
	}
	have, err := s.cachedRoles(ctx, userID)
	if err != nil {
		return false, err
	}
	for _, want := range roles {
		// "admin" check is delegated to the primary-role gate in the
		// middleware; HasAny only inspects functional roles plus the
		// implicit-admin flag carried alongside.
		if want == "admin" {
			if have.IsAdmin {
				return true, nil
			}
			continue
		}
		for _, r := range have.Roles {
			if r == want {
				return true, nil
			}
		}
		// Admins implicitly carry every functional role so a route gated
		// to "cs" or "agent" is always reachable by an admin.
		if have.IsAdmin {
			if validRole(want) {
				return true, nil
			}
		}
	}
	return false, nil
}

// RoleSnapshot is what HasAny stashes in Redis: the primary admin flag
// and the explicit functional roles. Kept tiny because it's read on
// every gated request.
type RoleSnapshot struct {
	IsAdmin bool     `json:"is_admin"`
	Roles   []string `json:"roles"`
}

const roleCacheTTL = 60 * time.Second

func roleCacheKey(uid uuid.UUID) string { return "auth:roles:" + uid.String() }

func (s *RoleSvc) cachedRoles(ctx context.Context, userID uuid.UUID) (*RoleSnapshot, error) {
	key := roleCacheKey(userID)
	if v, err := s.d.Redis.Get(ctx, key).Result(); err == nil {
		return decodeRoleSnapshot(v), nil
	}
	// Transient redis errors fall through to DB rather than failing the
	// request — auth gates already pay the DB cost once on miss.
	u, err := s.d.Ent.User.Get(ctx, userID)
	if err != nil {
		return nil, err
	}
	roles, err := s.ListForUser(ctx, userID)
	if err != nil {
		return nil, err
	}
	snap := &RoleSnapshot{IsAdmin: string(u.Role) == "admin", Roles: roles}
	_ = s.d.Redis.Set(ctx, key, encodeRoleSnapshot(snap), roleCacheTTL).Err()
	return snap, nil
}

// invalidateCache wipes the role snapshot so the next gated request
// observes the fresh state.
func (s *RoleSvc) invalidateCache(ctx context.Context, userID uuid.UUID) error {
	return s.d.Redis.Del(ctx, roleCacheKey(userID)).Err()
}

// InvalidateUserRoleCache is exposed for callers that mutate the primary
// user.role enum (admin demote/promote) — they can't reach the private
// invalidateCache so they call this instead.
func (s *RoleSvc) InvalidateUserRoleCache(ctx context.Context, userID uuid.UUID) {
	_ = s.invalidateCache(ctx, userID)
}

// encodeRoleSnapshot serialises the snapshot as a tiny CSV-ish string:
// "is_admin=1;roles=cs,agent". Avoids the JSON-tag boilerplate and keeps
// the redis value pleasant to inspect with `GET`.
func encodeRoleSnapshot(s *RoleSnapshot) string {
	out := "0"
	if s.IsAdmin {
		out = "1"
	}
	for i, r := range s.Roles {
		if i == 0 {
			out += "|"
		} else {
			out += ","
		}
		out += r
	}
	return out
}

func decodeRoleSnapshot(v string) *RoleSnapshot {
	snap := &RoleSnapshot{}
	if v == "" {
		return snap
	}
	// Split on '|' between admin flag and roles list.
	var adminPart, rolesPart string
	for i := 0; i < len(v); i++ {
		if v[i] == '|' {
			adminPart = v[:i]
			rolesPart = v[i+1:]
			break
		}
	}
	if adminPart == "" {
		adminPart = v
	}
	snap.IsAdmin = adminPart == "1"
	if rolesPart == "" {
		return snap
	}
	start := 0
	for i := 0; i <= len(rolesPart); i++ {
		if i == len(rolesPart) || rolesPart[i] == ',' {
			if i > start {
				r := rolesPart[start:i]
				if validRole(r) {
					snap.Roles = append(snap.Roles, r)
				}
			}
			start = i + 1
		}
	}
	return snap
}
