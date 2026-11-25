package biz

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/jucode/jucode/internal/data/ent"
)

// The model-listing endpoints must advertise exactly what a generation
// request would accept for the same credential, so the filter behind them is
// checked against the same CheckAccess rules Resolve applies.
func TestFilterAccessibleModels(t *testing.T) {
	vipUserGroup := uuid.New()
	otherUserGroup := uuid.New()

	publicGroup := &ent.ModelGroup{ID: uuid.New(), Name: "public"}
	vipGroup := &ent.ModelGroup{ID: uuid.New(), Name: "vip", AllowedUserGroupIds: []uuid.UUID{vipUserGroup}}

	free := &ent.Model{ID: uuid.New(), Name: "free-model"}
	vipOnly := &ent.Model{ID: uuid.New(), Name: "vip-model"}
	both := &ent.Model{ID: uuid.New(), Name: "both-model"}
	orphan := &ent.Model{ID: uuid.New(), Name: "orphan-model"}

	models := []*ent.Model{free, vipOnly, both, orphan}
	byModel := map[uuid.UUID][]*ent.ModelGroup{
		free.ID:    {publicGroup},
		vipOnly.ID: {vipGroup},
		both.ID:    {publicGroup, vipGroup},
		// orphan is deliberately absent: no enabled group links it.
	}

	tests := []struct {
		name   string
		user   *ent.User
		apiKey *ent.ApiKey
		want   []string
	}{
		{
			name: "user outside the vip group only sees public models",
			user: &ent.User{UserGroupID: &otherUserGroup},
			want: []string{"free-model", "both-model"},
		},
		{
			name: "user with no group only sees public models",
			user: &ent.User{},
			want: []string{"free-model", "both-model"},
		},
		{
			name: "vip user sees the restricted model too",
			user: &ent.User{UserGroupID: &vipUserGroup},
			want: []string{"free-model", "vip-model", "both-model"},
		},
		{
			name:   "key scoped to the vip group hides public-only models",
			user:   &ent.User{UserGroupID: &vipUserGroup},
			apiKey: &ent.ApiKey{AllowedGroupIds: []uuid.UUID{vipGroup.ID}},
			want:   []string{"vip-model", "both-model"},
		},
		{
			name:   "key scoped to a group the user may not access sees nothing",
			user:   &ent.User{UserGroupID: &otherUserGroup},
			apiKey: &ent.ApiKey{AllowedGroupIds: []uuid.UUID{vipGroup.ID}},
			want:   []string{},
		},
		{
			name:   "unscoped key falls back to the user's own access",
			user:   &ent.User{UserGroupID: &vipUserGroup},
			apiKey: &ent.ApiKey{AllowedGroupIds: []uuid.UUID{}},
			want:   []string{"free-model", "vip-model", "both-model"},
		},
	}

	svc := &ModelSvc{}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := svc.filterAccessibleModels(context.Background(), models, byModel, tt.user, tt.apiKey)
			names := make([]string, 0, len(got))
			for _, m := range got {
				names = append(names, m.Name)
			}
			if len(names) != len(tt.want) {
				t.Fatalf("got %v, want %v", names, tt.want)
			}
			for i := range names {
				if names[i] != tt.want[i] {
					t.Fatalf("got %v, want %v", names, tt.want)
				}
			}
		})
	}
}

// A model attached to no enabled group would fail Resolve with
// ErrModelNotInGroup, so it must never be advertised.
func TestFilterAccessibleModelsDropsUngroupedModel(t *testing.T) {
	orphan := &ent.Model{ID: uuid.New(), Name: "orphan-model"}
	got := (&ModelSvc{}).filterAccessibleModels(context.Background(),
		[]*ent.Model{orphan}, map[uuid.UUID][]*ent.ModelGroup{}, &ent.User{}, nil)
	if len(got) != 0 {
		t.Fatalf("ungrouped model was listed: %#v", got)
	}
}
