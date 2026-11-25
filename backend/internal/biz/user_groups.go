// user_groups.go: UserGroup CRUD + the "set default" transaction.
// User groups gate which ModelGroups a user can reach; their lifecycle
// is independent of model/model-group changes but is conventionally
// owned by ModelSvc to keep the routing decisions in one place.
package biz

import (
	"context"

	"github.com/google/uuid"

	"github.com/jucode/jucode/internal/data/ent"
	entuggrant "github.com/jucode/jucode/internal/data/ent/modelgroupusergroupgrant"
	entusergroup "github.com/jucode/jucode/internal/data/ent/usergroup"
)

type CreateUserGroupInput struct {
	Name             string
	Description      string
	SortOrder        int
	ConcurrencyLimit int
	RpmLimit         int
	TpmLimit         int
}

func (s *ModelSvc) CreateUserGroup(ctx context.Context, in CreateUserGroupInput) (*ent.UserGroup, error) {
	return s.d.Ent.UserGroup.Create().
		SetName(in.Name).
		SetDescription(in.Description).
		SetSortOrder(in.SortOrder).
		SetConcurrencyLimit(in.ConcurrencyLimit).
		SetRpmLimit(in.RpmLimit).
		SetTpmLimit(in.TpmLimit).
		Save(ctx)
}

func (s *ModelSvc) ListUserGroups(ctx context.Context) ([]*ent.UserGroup, error) {
	return s.d.Ent.UserGroup.Query().
		Order(ent.Asc(entusergroup.FieldSortOrder), ent.Asc(entusergroup.FieldName)).
		All(ctx)
}

type UpdateUserGroupInput struct {
	Name             *string
	Description      *string
	SortOrder        *int
	ConcurrencyLimit *int
	RpmLimit         *int
	TpmLimit         *int
}

func (s *ModelSvc) UpdateUserGroup(ctx context.Context, id uuid.UUID, in UpdateUserGroupInput) (*ent.UserGroup, error) {
	q := s.d.Ent.UserGroup.UpdateOneID(id)
	if in.Name != nil {
		q = q.SetName(*in.Name)
	}
	if in.Description != nil {
		q = q.SetDescription(*in.Description)
	}
	if in.SortOrder != nil {
		q = q.SetSortOrder(*in.SortOrder)
	}
	if in.ConcurrencyLimit != nil {
		q = q.SetConcurrencyLimit(*in.ConcurrencyLimit)
	}
	if in.RpmLimit != nil {
		q = q.SetRpmLimit(*in.RpmLimit)
	}
	if in.TpmLimit != nil {
		q = q.SetTpmLimit(*in.TpmLimit)
	}
	v, err := q.Save(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			return nil, ErrUserGroupNotFound
		}
		return nil, err
	}
	return v, nil
}

// SetDefaultUserGroup marks the given group as the default (used for users
// with NULL user_group_id) and clears the flag from every other row. Runs
// in a transaction so there's never two defaults visible at once.
func (s *ModelSvc) SetDefaultUserGroup(ctx context.Context, id uuid.UUID) error {
	tx, err := s.d.Ent.Tx(ctx)
	if err != nil {
		return err
	}
	if _, err := tx.UserGroup.Update().
		Where(entusergroup.IsDefaultEQ(true)).
		SetIsDefault(false).
		Save(ctx); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := tx.UserGroup.UpdateOneID(id).SetIsDefault(true).Exec(ctx); err != nil {
		_ = tx.Rollback()
		if ent.IsNotFound(err) {
			return ErrUserGroupNotFound
		}
		return err
	}
	return tx.Commit()
}

func (s *ModelSvc) DeleteUserGroup(ctx context.Context, id uuid.UUID) error {
	tx, err := s.d.Ent.Tx(ctx)
	if err != nil {
		return err
	}
	if _, err := tx.ModelGroupUserGroupGrant.Delete().Where(entuggrant.UserGroupIDEQ(id)).Exec(ctx); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := tx.UserGroup.DeleteOneID(id).Exec(ctx); err != nil {
		_ = tx.Rollback()
		if ent.IsNotFound(err) {
			return ErrUserGroupNotFound
		}
		return err
	}
	return tx.Commit()
}
