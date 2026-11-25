package biz

import (
	"testing"

	"github.com/google/uuid"

	"github.com/jucode/jucode/internal/data/ent"
)

func TestUserMayEnterGroup(t *testing.T) {
	ug := uuid.New()
	other := uuid.New()
	gid := uuid.New()

	public := &ent.ModelGroup{ID: uuid.New(), Name: "public"}
	exclusive := &ent.ModelGroup{ID: gid, Name: "vip", Exclusive: true}
	legacy := &ent.ModelGroup{ID: uuid.New(), Name: "legacy", AllowedUserGroupIds: []uuid.UUID{ug}}

	member := &ent.User{UserGroupID: &ug}
	outsider := &ent.User{UserGroupID: &other}
	unassigned := &ent.User{}

	granted := map[uuid.UUID]struct{}{gid: {}}

	if !userMayEnterGroup(outsider, public, nil) {
		t.Fatal("public group must stay open")
	}
	if userMayEnterGroup(outsider, exclusive, nil) {
		t.Fatal("exclusive with no grants must be locked")
	}
	if userMayEnterGroup(member, exclusive, nil) {
		t.Fatal("user group membership alone does not open an exclusive group")
	}
	if !userMayEnterGroup(outsider, exclusive, granted) {
		t.Fatal("personal/user-group grant must open the exclusive group")
	}
	if !userMayEnterGroup(unassigned, exclusive, granted) {
		t.Fatal("a personal grant works even when the user has no user group")
	}
	if userMayEnterGroup(unassigned, legacy, nil) {
		t.Fatal("unassigned users do not inherit a default user group for ACL")
	}
	if !userMayEnterGroup(member, legacy, nil) {
		t.Fatal("legacy JSON allowlist still matches")
	}
	if userMayEnterGroup(outsider, legacy, nil) {
		t.Fatal("legacy JSON allowlist must reject other user groups")
	}
}

func TestCheckAccessDeniesExclusiveWithoutGrant(t *testing.T) {
	svc := &ModelSvc{}
	exclusive := &ent.ModelGroup{ID: uuid.New(), Name: "vip", Exclusive: true}
	public := &ent.ModelGroup{ID: uuid.New(), Name: "public"}
	outsider := &ent.User{ID: uuid.New()}

	if err := svc.CheckAccess(t.Context(), outsider, nil, exclusive); err != ErrGroupDeniedForUser {
		t.Fatalf("outsider must not enter exclusive group, err=%v", err)
	}
	if err := svc.CheckAccess(t.Context(), nil, nil, exclusive); err != ErrGroupDeniedForUser {
		t.Fatalf("missing user must not enter exclusive group, err=%v", err)
	}
	if err := svc.CheckAccess(t.Context(), outsider, nil, public); err != nil {
		t.Fatalf("public group must stay open, err=%v", err)
	}
}

func TestPublicCatalogGroupsDropsExclusive(t *testing.T) {
	got := PublicCatalogGroups([]*ent.ModelGroup{
		{Name: "public", Enabled: true},
		{Name: "Grok (代理专属)", Enabled: true, Exclusive: true},
		{Name: "retired", Enabled: false},
	})
	if len(got) != 1 || got[0].Name != "public" {
		t.Fatalf("public catalog must drop exclusive (and keep caller to skip disabled), got %#v", got)
	}
}

func TestFilterAccessibleModelsExclusiveSibling(t *testing.T) {
	ug := uuid.New()
	public := &ent.ModelGroup{ID: uuid.New(), Name: "public"}
	exclusive := &ent.ModelGroup{ID: uuid.New(), Name: "cheap", Exclusive: true, RateMultiplier: 0.7}

	shared := &ent.Model{ID: uuid.New(), Name: "shared-model"}
	models := []*ent.Model{shared}
	byModel := map[uuid.UUID][]*ent.ModelGroup{
		shared.ID: {exclusive, public},
	}

	svc := &ModelSvc{}
	outsider := &ent.User{UserGroupID: &ug}
	got := svc.filterAccessibleModels(t.Context(), models, byModel, outsider, nil)
	if len(got) != 1 || got[0].Name != "shared-model" {
		t.Fatalf("outsider should still see the model via the public sibling, got %#v", got)
	}

	admin := &ent.User{Role: "admin"}
	got = svc.filterAccessibleModels(t.Context(), models, byModel, admin, nil)
	if len(got) != 1 {
		t.Fatal("admin must not bypass exclusive; they only see the public sibling")
	}
}
