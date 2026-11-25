// model_groups.go: ModelGroup CRUD plus the link tables that connect
// a group to models and providers. Kept beside model.go (same
// ModelSvc receiver) so the two share one set of fields, but pulled
// out for readability — Group lifecycle and Model lifecycle have
// different change cadences in practice.
package biz

import (
	"context"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/jucode/jucode/internal/data/ent"
	entmodel "github.com/jucode/jucode/internal/data/ent/model"
	entgroup "github.com/jucode/jucode/internal/data/ent/modelgroup"
	entgrpmodel "github.com/jucode/jucode/internal/data/ent/modelgroupmodel"
	entgrpprov "github.com/jucode/jucode/internal/data/ent/modelgroupprovider"
	entusergrant "github.com/jucode/jucode/internal/data/ent/modelgroupusergrant"
	entuggrant "github.com/jucode/jucode/internal/data/ent/modelgroupusergroupgrant"
	entschema "github.com/jucode/jucode/internal/data/ent/schema"
)

type CreateGroupInput struct {
	Name           string
	BillingSource  string // plan_only | balance_only
	RateMultiplier float64
	Description    string
	Profile        []entschema.GroupProfileDim
	Enabled        *bool
}

func (s *ModelSvc) CreateGroup(ctx context.Context, in CreateGroupInput) (*ent.ModelGroup, error) {
	mult := in.RateMultiplier
	if mult <= 0 {
		mult = 1.0
	}
	q := s.d.Ent.ModelGroup.Create().
		SetName(in.Name).
		SetBillingSource(entgroup.BillingSource(in.BillingSource)).
		SetRateMultiplier(mult).
		SetDescription(in.Description)
	if len(in.Profile) > 0 {
		q = q.SetProfile(in.Profile)
	}
	if in.Enabled != nil {
		q = q.SetEnabled(*in.Enabled)
	}
	return q.Save(ctx)
}

// ListGroups returns groups in admin-defined order. Name is the tiebreaker so
// the listing stays stable and deterministic while every row still has the
// default sort_order of 0.
func (s *ModelSvc) ListGroups(ctx context.Context) ([]*ent.ModelGroup, error) {
	return s.d.Ent.ModelGroup.Query().
		Order(ent.Asc(entgroup.FieldSortOrder), ent.Asc(entgroup.FieldName)).
		All(ctx)
}

// ReorderGroups applies a new relative order for the given ids. The group list
// is not paginated today, but it goes through the same splice-and-renumber path
// as ReorderModels so a partial list can never collapse the rest to zero.
func (s *ModelSvc) ReorderGroups(ctx context.Context, ids []uuid.UUID) error {
	if len(ids) == 0 {
		return nil
	}
	all, err := s.ListGroups(ctx)
	if err != nil {
		return err
	}
	current := make([]uuid.UUID, len(all))
	sortOrders := make(map[uuid.UUID]int, len(all))
	for i, g := range all {
		current[i] = g.ID
		sortOrders[g.ID] = g.SortOrder
	}
	next, err := spliceOrder(current, ids)
	if err != nil {
		return ErrGroupNotFound
	}

	tx, err := s.d.Ent.Tx(ctx)
	if err != nil {
		return err
	}
	rollback := func() { _ = tx.Rollback() }
	for i, id := range next {
		if sortOrders[id] == i {
			continue
		}
		if err := tx.ModelGroup.UpdateOneID(id).SetSortOrder(i).Exec(ctx); err != nil {
			rollback()
			if ent.IsNotFound(err) {
				return ErrGroupNotFound
			}
			return err
		}
	}
	return tx.Commit()
}

func (s *ModelSvc) LoadGroupsForModels(ctx context.Context, models []*ent.Model) (map[uuid.UUID][]*ent.ModelGroup, error) {
	modelIDs := make([]uuid.UUID, 0, len(models))
	for _, m := range models {
		modelIDs = append(modelIDs, m.ID)
	}
	if len(modelIDs) == 0 {
		return map[uuid.UUID][]*ent.ModelGroup{}, nil
	}
	links, err := s.d.Ent.ModelGroupModel.Query().
		Where(entgrpmodel.ModelIDIn(modelIDs...)).
		All(ctx)
	if err != nil {
		return nil, err
	}
	groupIDSet := make(map[uuid.UUID]struct{}, len(links))
	for _, link := range links {
		groupIDSet[link.GroupID] = struct{}{}
	}
	groupIDs := make([]uuid.UUID, 0, len(groupIDSet))
	for id := range groupIDSet {
		groupIDs = append(groupIDs, id)
	}
	if len(groupIDs) == 0 {
		return map[uuid.UUID][]*ent.ModelGroup{}, nil
	}
	groups, err := s.d.Ent.ModelGroup.Query().
		Where(entgroup.IDIn(groupIDs...), entgroup.EnabledEQ(true)).
		All(ctx)
	if err != nil {
		return nil, err
	}
	byID := make(map[uuid.UUID]*ent.ModelGroup, len(groups))
	for _, group := range groups {
		byID[group.ID] = group
	}
	byModel := make(map[uuid.UUID][]*ent.ModelGroup, len(models))
	for _, link := range links {
		if group, ok := byID[link.GroupID]; ok {
			byModel[link.ModelID] = append(byModel[link.ModelID], group)
		}
	}
	// Same ordering rule as ListGroups: admin-defined first, name as the
	// tiebreaker. Keeps a model's group badges in the same order the admin
	// arranged the group list in.
	for id := range byModel {
		sort.Slice(byModel[id], func(i, j int) bool {
			a, b := byModel[id][i], byModel[id][j]
			if a.SortOrder != b.SortOrder {
				return a.SortOrder < b.SortOrder
			}
			return a.Name < b.Name
		})
	}
	return byModel, nil
}

func (s *ModelSvc) DeleteGroup(ctx context.Context, id uuid.UUID) error {
	tx, err := s.d.Ent.Tx(ctx)
	if err != nil {
		return err
	}
	rollback := func() { _ = tx.Rollback() }
	if _, err := tx.ModelGroupModel.Delete().
		Where(entgrpmodel.GroupIDEQ(id)).
		Exec(ctx); err != nil {
		rollback()
		return err
	}
	if _, err := tx.ModelGroupProvider.Delete().
		Where(entgrpprov.GroupIDEQ(id)).
		Exec(ctx); err != nil {
		rollback()
		return err
	}
	if _, err := tx.ModelGroupUserGrant.Delete().
		Where(entusergrant.GroupIDEQ(id)).
		Exec(ctx); err != nil {
		rollback()
		return err
	}
	if _, err := tx.ModelGroupUserGroupGrant.Delete().
		Where(entuggrant.GroupIDEQ(id)).
		Exec(ctx); err != nil {
		rollback()
		return err
	}
	err = tx.ModelGroup.DeleteOneID(id).Exec(ctx)
	if err != nil {
		rollback()
	}
	if ent.IsNotFound(err) {
		return ErrGroupNotFound
	}
	if err != nil {
		return err
	}
	return tx.Commit()
}

type UpdateGroupInput struct {
	Name                *string
	BillingSource       *string
	RateMultiplier      *float64
	Description         *string
	Profile             *[]entschema.GroupProfileDim
	AllowedUserGroupIDs *[]uuid.UUID
	Enabled             *bool
	Exclusive           *bool
}

func (s *ModelSvc) UpdateGroup(ctx context.Context, id uuid.UUID, in UpdateGroupInput) (*ent.ModelGroup, error) {
	q := s.d.Ent.ModelGroup.UpdateOneID(id)
	if in.Name != nil {
		q = q.SetName(*in.Name)
	}
	if in.BillingSource != nil {
		q = q.SetBillingSource(entgroup.BillingSource(*in.BillingSource))
	}
	if in.RateMultiplier != nil {
		q = q.SetRateMultiplier(*in.RateMultiplier)
	}
	if in.Description != nil {
		q = q.SetDescription(*in.Description)
	}
	if in.Profile != nil {
		q = q.SetProfile(*in.Profile)
	}
	if in.Enabled != nil {
		q = q.SetEnabled(*in.Enabled)
	}
	if in.Exclusive != nil {
		q = q.SetExclusive(*in.Exclusive)
	} else if in.AllowedUserGroupIDs != nil && len(*in.AllowedUserGroupIDs) > 0 {
		q = q.SetExclusive(true)
	}
	if in.AllowedUserGroupIDs != nil {
		q = q.SetAllowedUserGroupIds([]uuid.UUID{})
	}
	g, err := q.Save(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			return nil, ErrGroupNotFound
		}
		return nil, err
	}
	if in.AllowedUserGroupIDs != nil {
		existing, gerr := s.GroupGrants(ctx, id)
		if gerr != nil {
			return nil, gerr
		}
		if err := s.ReplaceGroupGrants(ctx, id, GroupGrants{
			UserIDs:      existing.UserIDs,
			UserGroupIDs: *in.AllowedUserGroupIDs,
		}); err != nil {
			return nil, err
		}
		if grants, gerr := s.GroupGrants(ctx, id); gerr == nil {
			g.AllowedUserGroupIds = grants.UserGroupIDs
		}
	}
	return g, nil
}

// --- Group <-> Model links ---

func (s *ModelSvc) AttachModelToGroup(ctx context.Context, groupID, modelID uuid.UUID) error {
	return s.d.Ent.ModelGroupModel.Create().
		SetGroupID(groupID).
		SetModelID(modelID).
		OnConflictColumns("group_id", "model_id").
		DoNothing().
		Exec(ctx)
}

// SetModelProbe updates the probe settings on an existing link. Deliberately
// separate from AttachModelToGroup: attach is idempotent membership
// (OnConflict.DoNothing), so folding probe settings into it would silently reset
// them every time someone re-added a model that was already in the group.
//
// Both arguments are optional so the UI can flip the switch without having to
// know the current interval, and vice versa. contextWindow is the model's
// window through the group (see AccountContextWindow); 0 clears it.
func (s *ModelSvc) SetModelProbe(ctx context.Context, groupID, modelID uuid.UUID, enabled *bool, intervalSeconds *int, disableThinking *bool, contextWindow *int) error {
	q := s.d.Ent.ModelGroupModel.Update().
		Where(entgrpmodel.GroupIDEQ(groupID), entgrpmodel.ModelIDEQ(modelID))
	if enabled != nil {
		q = q.SetProbeEnabled(*enabled)
	}
	if intervalSeconds != nil {
		// Floor matches the model-probe scheduler's own clamp. Enforced here too so the
		// stored value is the one that actually runs — an admin who typed 5
		// should see 60 come back, not believe a 5s cadence is in effect.
		interval := *intervalSeconds
		if min := int(modelProbeMinInterval / time.Second); interval < min {
			interval = min
		}
		q = q.SetProbeIntervalSeconds(interval)
	}
	if disableThinking != nil {
		q = q.SetProbeDisableThinking(*disableThinking)
	}
	if contextWindow != nil {
		if err := validCapability("context_window", *contextWindow); err != nil {
			return err
		}
		if *contextWindow == 0 {
			q = q.ClearContextWindow()
		} else {
			q = q.SetContextWindow(*contextWindow)
		}
	}
	n, err := q.Save(ctx)
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrModelNotInGroup
	}
	return nil
}

func (s *ModelSvc) DetachModelFromGroup(ctx context.Context, groupID, modelID uuid.UUID) error {
	_, err := s.d.Ent.ModelGroupModel.Delete().
		Where(entgrpmodel.GroupIDEQ(groupID), entgrpmodel.ModelIDEQ(modelID)).
		Exec(ctx)
	return err
}

// --- Group <-> Provider links ---

func (s *ModelSvc) AttachProviderToGroup(ctx context.Context, groupID, providerID uuid.UUID, priority, weight int) error {
	if priority <= 0 {
		priority = 100
	}
	if weight <= 0 {
		weight = 100
	}
	return s.d.Ent.ModelGroupProvider.Create().
		SetGroupID(groupID).
		SetProviderID(providerID).
		SetPriority(priority).
		SetWeight(weight).
		OnConflictColumns("group_id", "provider_id").
		UpdateNewValues().
		Exec(ctx)
}

func (s *ModelSvc) DetachProviderFromGroup(ctx context.Context, groupID, providerID uuid.UUID) error {
	_, err := s.d.Ent.ModelGroupProvider.Delete().
		Where(entgrpprov.GroupIDEQ(groupID), entgrpprov.ProviderIDEQ(providerID)).
		Exec(ctx)
	return err
}

// GroupCatalog is the search surface of one group: enabled model names and
// the distinct vendor names among them. The API key picker filters on this.
// Disabled models are omitted so a retired model cannot be found.
// SupportsImage is true when any of those models is an image-generation
// model (flat per-image price, or a known image model name such as gpt-image).
type GroupCatalog struct {
	Models        []string
	Vendors       []string
	SupportsImage bool
}

// GroupCatalogs loads that surface for the given groups in one query.
// Every requested id is present; a group with no enabled models gets
// empty slices rather than a missing entry.
func (s *ModelSvc) GroupCatalogs(ctx context.Context, groupIDs []uuid.UUID) (map[uuid.UUID]GroupCatalog, error) {
	out := make(map[uuid.UUID]GroupCatalog, len(groupIDs))
	for _, id := range groupIDs {
		out[id] = GroupCatalog{Models: []string{}, Vendors: []string{}}
	}
	if len(groupIDs) == 0 {
		return out, nil
	}
	links, err := s.d.Ent.ModelGroupModel.Query().
		Where(entgrpmodel.GroupIDIn(groupIDs...)).
		WithModel(func(q *ent.ModelQuery) {
			q.Where(entmodel.EnabledEQ(true)).WithVendor()
		}).
		All(ctx)
	if err != nil {
		return nil, err
	}
	// A catalog row still attached to the group does not count unless an
	// active account on that group is configured to serve it. A cooldown
	// still counts: this list is what the key may call, and the status
	// pill already says whether routing is up.
	served, err := groupConfiguredModelNames(ctx, s.d.Ent, groupIDs)
	if err != nil {
		return nil, err
	}
	models := make(map[uuid.UUID]map[string]struct{}, len(groupIDs))
	vendors := make(map[uuid.UUID]map[string]struct{}, len(groupIDs))
	images := make(map[uuid.UUID]bool, len(groupIDs))
	for _, link := range links {
		m := link.Edges.Model
		if m == nil || !m.Enabled || m.Name == "" || !modelNameServed(served, link.GroupID, m.Name) {
			continue
		}
		addCatalogName(models, link.GroupID, m.Name)
		if v := m.Edges.Vendor; v != nil && v.Name != "" {
			addCatalogName(vendors, link.GroupID, v.Name)
		}
		if modelSupportsImage(m) {
			images[link.GroupID] = true
		}
	}
	for id := range out {
		out[id] = GroupCatalog{
			Models:        sortedCatalogNames(models[id]),
			Vendors:       sortedCatalogNames(vendors[id]),
			SupportsImage: images[id],
		}
	}
	return out, nil
}

// modelSupportsImage reports whether this model can generate images.
// OutputImagePrice is the flat per-image price and is nil when the model
// cannot emit images. gpt-image and dall-e are priced like text models, so
// the name check covers that gap. Image token prices on a chat model do not
// count: those price a built-in tool, not an image-generation group.
func modelSupportsImage(m *ent.Model) bool {
	if m == nil {
		return false
	}
	if m.OutputImagePrice != nil {
		return true
	}
	return imageGenerationModelName(m.Name)
}

func modelNameServed(served map[uuid.UUID]map[string]struct{}, groupID uuid.UUID, name string) bool {
	names := served[groupID]
	if len(names) == 0 {
		return false
	}
	_, ok := names[name]
	return ok
}

func imageGenerationModelName(name string) bool {
	n := strings.ToLower(strings.TrimSpace(name))
	n = strings.ReplaceAll(n, "_", "-")
	return strings.HasPrefix(n, "gpt-image-") ||
		strings.HasPrefix(n, "dall-e-") ||
		n == "image2" ||
		n == "image-2"
}

func addCatalogName(dst map[uuid.UUID]map[string]struct{}, groupID uuid.UUID, name string) {
	set := dst[groupID]
	if set == nil {
		set = map[string]struct{}{}
		dst[groupID] = set
	}
	set[name] = struct{}{}
}

func sortedCatalogNames(set map[string]struct{}) []string {
	if len(set) == 0 {
		return []string{}
	}
	out := make([]string, 0, len(set))
	for name := range set {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// GroupModelNames returns, per group, the names of the enabled models it
// serves, so a client can offer only the groups that can run a model.
func (s *ModelSvc) GroupModelNames(ctx context.Context, groupIDs []uuid.UUID) (map[uuid.UUID][]string, error) {
	out := make(map[uuid.UUID][]string, len(groupIDs))
	if len(groupIDs) == 0 {
		return out, nil
	}
	links, err := s.d.Ent.ModelGroupModel.Query().
		Where(entgrpmodel.GroupIDIn(groupIDs...)).
		WithModel().
		All(ctx)
	if err != nil {
		return nil, err
	}
	for _, link := range links {
		if m := link.Edges.Model; m != nil && m.Enabled {
			out[link.GroupID] = append(out[link.GroupID], m.Name)
		}
	}
	return out, nil
}
