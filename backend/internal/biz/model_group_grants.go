package biz

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"github.com/jucode/jucode/internal/data/ent"
	entgroup "github.com/jucode/jucode/internal/data/ent/modelgroup"
	entgrpmodel "github.com/jucode/jucode/internal/data/ent/modelgroupmodel"
	entgrpprov "github.com/jucode/jucode/internal/data/ent/modelgroupprovider"
	entusergrant "github.com/jucode/jucode/internal/data/ent/modelgroupusergrant"
	entuggrant "github.com/jucode/jucode/internal/data/ent/modelgroupusergroupgrant"
)

type GroupGrants struct {
	UserIDs      []uuid.UUID
	UserGroupIDs []uuid.UUID
}

func (s *ModelSvc) GroupGrants(ctx context.Context, groupID uuid.UUID) (GroupGrants, error) {
	users, err := s.d.Ent.ModelGroupUserGrant.Query().
		Where(entusergrant.GroupIDEQ(groupID)).
		All(ctx)
	if err != nil {
		return GroupGrants{}, err
	}
	ugs, err := s.d.Ent.ModelGroupUserGroupGrant.Query().
		Where(entuggrant.GroupIDEQ(groupID)).
		All(ctx)
	if err != nil {
		return GroupGrants{}, err
	}
	out := GroupGrants{
		UserIDs:      make([]uuid.UUID, 0, len(users)),
		UserGroupIDs: make([]uuid.UUID, 0, len(ugs)),
	}
	for _, row := range users {
		out.UserIDs = append(out.UserIDs, row.UserID)
	}
	for _, row := range ugs {
		out.UserGroupIDs = append(out.UserGroupIDs, row.UserGroupID)
	}
	return out, nil
}

func (s *ModelSvc) ReplaceGroupGrants(ctx context.Context, groupID uuid.UUID, in GroupGrants) error {
	if _, err := s.d.Ent.ModelGroup.Get(ctx, groupID); err != nil {
		if ent.IsNotFound(err) {
			return ErrGroupNotFound
		}
		return err
	}
	tx, err := s.d.Ent.Tx(ctx)
	if err != nil {
		return err
	}
	rollback := func() { _ = tx.Rollback() }
	if _, err := tx.ModelGroupUserGrant.Delete().Where(entusergrant.GroupIDEQ(groupID)).Exec(ctx); err != nil {
		rollback()
		return err
	}
	if _, err := tx.ModelGroupUserGroupGrant.Delete().Where(entuggrant.GroupIDEQ(groupID)).Exec(ctx); err != nil {
		rollback()
		return err
	}
	for _, uid := range uniqueUUIDs(in.UserIDs) {
		if err := tx.ModelGroupUserGrant.Create().
			SetGroupID(groupID).SetUserID(uid).
			OnConflictColumns("group_id", "user_id").DoNothing().
			Exec(ctx); err != nil {
			rollback()
			return err
		}
	}
	for _, ugid := range uniqueUUIDs(in.UserGroupIDs) {
		if err := tx.ModelGroupUserGroupGrant.Create().
			SetGroupID(groupID).SetUserGroupID(ugid).
			OnConflictColumns("group_id", "user_group_id").DoNothing().
			Exec(ctx); err != nil {
			rollback()
			return err
		}
	}
	return tx.Commit()
}

func (s *ModelSvc) UserModelGroupGrants(ctx context.Context, userID uuid.UUID) ([]uuid.UUID, error) {
	rows, err := s.d.Ent.ModelGroupUserGrant.Query().
		Where(entusergrant.UserIDEQ(userID)).
		All(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]uuid.UUID, 0, len(rows))
	for _, row := range rows {
		out = append(out, row.GroupID)
	}
	return out, nil
}

func (s *ModelSvc) ReplaceUserModelGroupGrants(ctx context.Context, userID uuid.UUID, groupIDs []uuid.UUID) error {
	ids, err := s.exclusiveGroupIDs(ctx, groupIDs)
	if err != nil {
		return err
	}
	tx, err := s.d.Ent.Tx(ctx)
	if err != nil {
		return err
	}
	rollback := func() { _ = tx.Rollback() }
	if _, err := tx.ModelGroupUserGrant.Delete().Where(entusergrant.UserIDEQ(userID)).Exec(ctx); err != nil {
		rollback()
		return err
	}
	for _, gid := range ids {
		if err := tx.ModelGroupUserGrant.Create().
			SetGroupID(gid).SetUserID(userID).
			OnConflictColumns("group_id", "user_id").DoNothing().
			Exec(ctx); err != nil {
			rollback()
			return err
		}
	}
	return tx.Commit()
}

func (s *ModelSvc) UserGroupModelGroupGrants(ctx context.Context, userGroupID uuid.UUID) ([]uuid.UUID, error) {
	rows, err := s.d.Ent.ModelGroupUserGroupGrant.Query().
		Where(entuggrant.UserGroupIDEQ(userGroupID)).
		All(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]uuid.UUID, 0, len(rows))
	for _, row := range rows {
		out = append(out, row.GroupID)
	}
	return out, nil
}

func (s *ModelSvc) ReplaceUserGroupModelGroupGrants(ctx context.Context, userGroupID uuid.UUID, groupIDs []uuid.UUID) error {
	ids, err := s.exclusiveGroupIDs(ctx, groupIDs)
	if err != nil {
		return err
	}
	tx, err := s.d.Ent.Tx(ctx)
	if err != nil {
		return err
	}
	rollback := func() { _ = tx.Rollback() }
	if _, err := tx.ModelGroupUserGroupGrant.Delete().Where(entuggrant.UserGroupIDEQ(userGroupID)).Exec(ctx); err != nil {
		rollback()
		return err
	}
	for _, gid := range ids {
		if err := tx.ModelGroupUserGroupGrant.Create().
			SetGroupID(gid).SetUserGroupID(userGroupID).
			OnConflictColumns("group_id", "user_group_id").DoNothing().
			Exec(ctx); err != nil {
			rollback()
			return err
		}
	}
	return tx.Commit()
}

func (s *ModelSvc) exclusiveGroupIDs(ctx context.Context, groupIDs []uuid.UUID) ([]uuid.UUID, error) {
	ids := uniqueUUIDs(groupIDs)
	if len(ids) == 0 {
		return nil, nil
	}
	rows, err := s.d.Ent.ModelGroup.Query().
		Where(entgroup.IDIn(ids...), entgroup.ExclusiveEQ(true)).
		All(ctx)
	if err != nil {
		return nil, err
	}
	if len(rows) != len(ids) {
		return nil, fmt.Errorf("only exclusive model groups can be granted")
	}
	out := make([]uuid.UUID, 0, len(rows))
	for _, g := range rows {
		out = append(out, g.ID)
	}
	return out, nil
}

type CloneGroupInput struct {
	Name           *string
	Exclusive      *bool
	RateMultiplier *float64
}

func (s *ModelSvc) CloneGroup(ctx context.Context, id uuid.UUID, in CloneGroupInput) (*ent.ModelGroup, error) {
	src, err := s.d.Ent.ModelGroup.Get(ctx, id)
	if err != nil {
		if ent.IsNotFound(err) {
			return nil, ErrGroupNotFound
		}
		return nil, err
	}
	exclusive := true
	if in.Exclusive != nil {
		exclusive = *in.Exclusive
	}
	rate := src.RateMultiplier
	if in.RateMultiplier != nil && *in.RateMultiplier > 0 {
		rate = *in.RateMultiplier
	}
	baseName := strings.TrimSpace(ptrString(in.Name))
	if baseName == "" {
		baseName = src.Name + " (副本)"
	}
	name, err := s.uniqueGroupName(ctx, baseName)
	if err != nil {
		return nil, err
	}

	modelLinks, err := s.d.Ent.ModelGroupModel.Query().Where(entgrpmodel.GroupIDEQ(id)).All(ctx)
	if err != nil {
		return nil, err
	}
	provLinks, err := s.d.Ent.ModelGroupProvider.Query().Where(entgrpprov.GroupIDEQ(id)).All(ctx)
	if err != nil {
		return nil, err
	}
	all, err := s.ListGroups(ctx)
	if err != nil {
		return nil, err
	}
	sortOrder := len(all)

	tx, err := s.d.Ent.Tx(ctx)
	if err != nil {
		return nil, err
	}
	rollback := func() { _ = tx.Rollback() }
	clone, err := tx.ModelGroup.Create().
		SetName(name).
		SetBillingSource(src.BillingSource).
		SetRateMultiplier(rate).
		SetDescription(src.Description).
		SetEnabled(src.Enabled).
		SetExclusive(exclusive).
		SetAllowedUserGroupIds([]uuid.UUID{}).
		SetSortOrder(sortOrder).
		Save(ctx)
	if err != nil {
		rollback()
		return nil, err
	}
	for _, link := range modelLinks {
		if err := tx.ModelGroupModel.Create().
			SetGroupID(clone.ID).
			SetModelID(link.ModelID).
			SetProbeEnabled(false).
			SetProbeIntervalSeconds(link.ProbeIntervalSeconds).
			SetProbeDisableThinking(link.ProbeDisableThinking).
			Exec(ctx); err != nil {
			rollback()
			return nil, err
		}
	}
	for _, link := range provLinks {
		if err := tx.ModelGroupProvider.Create().
			SetGroupID(clone.ID).
			SetProviderID(link.ProviderID).
			SetPriority(link.Priority).
			SetWeight(link.Weight).
			SetEnabled(link.Enabled).
			Exec(ctx); err != nil {
			rollback()
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return clone, nil
}

func (s *ModelSvc) uniqueGroupName(ctx context.Context, base string) (string, error) {
	name := base
	for n := 2; n < 50; n++ {
		exists, err := s.d.Ent.ModelGroup.Query().Where(entgroup.NameEQ(name)).Exist(ctx)
		if err != nil {
			return "", err
		}
		if !exists {
			return name, nil
		}
		name = fmt.Sprintf("%s %d", base, n)
	}
	return "", fmt.Errorf("could not allocate a unique group name")
}

func ptrString(v *string) string {
	if v == nil {
		return ""
	}
	return *v
}

func uniqueUUIDs(in []uuid.UUID) []uuid.UUID {
	seen := map[uuid.UUID]struct{}{}
	out := make([]uuid.UUID, 0, len(in))
	for _, id := range in {
		if id == uuid.Nil {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out
}

// MigrateLegacyGroupACL copies non-empty allowed_user_group_ids into
// model_group_user_group_grants, marks those groups exclusive, and clears
// the JSON. Ghost user-group IDs are skipped.
func MigrateLegacyGroupACL(ctx context.Context, client *ent.Client) error {
	if client == nil {
		return nil
	}
	groups, err := client.ModelGroup.Query().All(ctx)
	if err != nil {
		return err
	}
	if len(groups) == 0 {
		return nil
	}
	ugs, err := client.UserGroup.Query().All(ctx)
	if err != nil {
		return err
	}
	alive := map[uuid.UUID]struct{}{}
	for _, ug := range ugs {
		alive[ug.ID] = struct{}{}
	}
	for _, g := range groups {
		if len(g.AllowedUserGroupIds) == 0 {
			continue
		}
		ids := make([]uuid.UUID, 0, len(g.AllowedUserGroupIds))
		for _, id := range g.AllowedUserGroupIds {
			if _, ok := alive[id]; ok {
				ids = append(ids, id)
			}
		}
		q := client.ModelGroup.UpdateOneID(g.ID).
			SetExclusive(true).
			SetAllowedUserGroupIds([]uuid.UUID{})
		if _, err := q.Save(ctx); err != nil {
			return err
		}
		for _, ugid := range uniqueUUIDs(ids) {
			if err := client.ModelGroupUserGroupGrant.Create().
				SetGroupID(g.ID).SetUserGroupID(ugid).
				OnConflictColumns("group_id", "user_group_id").DoNothing().
				Exec(ctx); err != nil {
				return err
			}
		}
	}
	return nil
}
