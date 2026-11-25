package biz

import (
	"context"

	"github.com/google/uuid"

	"github.com/jucode/jucode/internal/data/ent"
	entusergrant "github.com/jucode/jucode/internal/data/ent/modelgroupusergrant"
	entuggrant "github.com/jucode/jucode/internal/data/ent/modelgroupusergroupgrant"
)

type ctxGrantedGroupsKey struct{}

// WithGrantedGroups stashes the exclusive-group IDs this user may enter so
// CheckAccess / Resolve / listing share one lookup per request.
func WithGrantedGroups(ctx context.Context, ids map[uuid.UUID]struct{}) context.Context {
	if ids == nil {
		ids = map[uuid.UUID]struct{}{}
	}
	return context.WithValue(ctx, ctxGrantedGroupsKey{}, ids)
}

func grantedGroupsFrom(ctx context.Context) (map[uuid.UUID]struct{}, bool) {
	ids, ok := ctx.Value(ctxGrantedGroupsKey{}).(map[uuid.UUID]struct{})
	return ids, ok
}

// PublicCatalogGroups drops exclusive groups (and disabled ones) so
// unauthenticated catalogs never advertise a locked route. Exclusive
// must be stripped before Redis cache: ent's Exclusive bool uses
// omitempty, so a stale payload without the field unmarshals as
// public and would leak if filtering only happens after a cache hit.
func PublicCatalogGroups(groups []*ent.ModelGroup) []*ent.ModelGroup {
	if len(groups) == 0 {
		return groups
	}
	out := make([]*ent.ModelGroup, 0, len(groups))
	for _, g := range groups {
		if g == nil || !g.Enabled || g.Exclusive {
			continue
		}
		out = append(out, g)
	}
	return out
}

func groupIsRestricted(group *ent.ModelGroup) bool {
	if group == nil {
		return false
	}
	return group.Exclusive || len(group.AllowedUserGroupIds) > 0
}

// userMayEnterGroup is the in-memory half of CheckAccess. granted is the
// exclusive-group set from the grant tables (may be empty). Legacy JSON
// allowlists still match so rows that have not been migrated keep working.
func userMayEnterGroup(user *ent.User, group *ent.ModelGroup, granted map[uuid.UUID]struct{}) bool {
	if group == nil {
		return true
	}
	if !groupIsRestricted(group) {
		return true
	}
	if granted != nil {
		if _, ok := granted[group.ID]; ok {
			return true
		}
	}
	if user != nil && user.UserGroupID != nil {
		for _, id := range group.AllowedUserGroupIds {
			if id == *user.UserGroupID {
				return true
			}
		}
	}
	return false
}

// LoadGrantedGroupIDs returns exclusive ModelGroup IDs this user may enter
// via a personal grant or their assigned UserGroup. A nil user_group_id
// does not inherit the default UserGroup's grants.
func (s *ModelSvc) LoadGrantedGroupIDs(ctx context.Context, user *ent.User) (map[uuid.UUID]struct{}, error) {
	out := map[uuid.UUID]struct{}{}
	if s == nil || s.d == nil || user == nil {
		return out, nil
	}
	userRows, err := s.d.Ent.ModelGroupUserGrant.Query().
		Where(entusergrant.UserIDEQ(user.ID)).
		All(ctx)
	if err != nil {
		return nil, err
	}
	for _, row := range userRows {
		out[row.GroupID] = struct{}{}
	}
	if user.UserGroupID == nil {
		return out, nil
	}
	ugRows, err := s.d.Ent.ModelGroupUserGroupGrant.Query().
		Where(entuggrant.UserGroupIDEQ(*user.UserGroupID)).
		All(ctx)
	if err != nil {
		return nil, err
	}
	for _, row := range ugRows {
		out[row.GroupID] = struct{}{}
	}
	return out, nil
}

func (s *ModelSvc) grantedGroups(ctx context.Context, user *ent.User) (map[uuid.UUID]struct{}, error) {
	if ids, ok := grantedGroupsFrom(ctx); ok {
		return ids, nil
	}
	return s.LoadGrantedGroupIDs(ctx, user)
}

func (s *ModelSvc) filterGroupsForUser(ctx context.Context, user *ent.User, groups []*ent.ModelGroup) ([]*ent.ModelGroup, error) {
	if len(groups) == 0 {
		return groups, nil
	}
	granted, err := s.grantedGroups(ctx, user)
	if err != nil {
		return nil, err
	}
	out := make([]*ent.ModelGroup, 0, len(groups))
	for _, g := range groups {
		if userMayEnterGroup(user, g, granted) {
			out = append(out, g)
		}
	}
	return out, nil
}
