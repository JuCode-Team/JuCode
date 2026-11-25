// model_vendors.go: ModelVendor CRUD + the LoadVendors helper used by
// list-style handlers to decorate model views without N+1 queries.
// slugify lives here because it's the auto-derivation rule for new
// vendor slugs and has no other callers.
package biz

import (
	"context"
	"regexp"
	"strings"

	"github.com/google/uuid"

	"github.com/jucode/jucode/internal/data/ent"
	entvendor "github.com/jucode/jucode/internal/data/ent/modelvendor"
)

type CreateVendorInput struct {
	Name        string
	Slug        string // empty → derived from name
	Description string
	SortOrder   int
}

func (s *ModelSvc) CreateVendor(ctx context.Context, in CreateVendorInput) (*ent.ModelVendor, error) {
	slug := in.Slug
	if slug == "" {
		slug = slugify(in.Name)
	}
	return s.d.Ent.ModelVendor.Create().
		SetName(in.Name).
		SetSlug(slug).
		SetDescription(in.Description).
		SetSortOrder(in.SortOrder).
		Save(ctx)
}

func (s *ModelSvc) ListVendors(ctx context.Context) ([]*ent.ModelVendor, error) {
	return s.d.Ent.ModelVendor.Query().
		Order(ent.Asc(entvendor.FieldSortOrder), ent.Asc(entvendor.FieldName)).
		All(ctx)
}

type UpdateVendorInput struct {
	Name        *string
	Slug        *string
	Description *string
	SortOrder   *int
}

func (s *ModelSvc) UpdateVendor(ctx context.Context, id uuid.UUID, in UpdateVendorInput) (*ent.ModelVendor, error) {
	q := s.d.Ent.ModelVendor.UpdateOneID(id)
	if in.Name != nil {
		q = q.SetName(*in.Name)
	}
	if in.Slug != nil {
		q = q.SetSlug(*in.Slug)
	}
	if in.Description != nil {
		q = q.SetDescription(*in.Description)
	}
	if in.SortOrder != nil {
		q = q.SetSortOrder(*in.SortOrder)
	}
	v, err := q.Save(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			return nil, ErrVendorNotFound
		}
		return nil, err
	}
	return v, nil
}

func (s *ModelSvc) DeleteVendor(ctx context.Context, id uuid.UUID) error {
	// Models that reference this vendor have their vendor_id automatically
	// cleared by ent's optional-edge cascade (vendor_id is Optional+Nillable).
	err := s.d.Ent.ModelVendor.DeleteOneID(id).Exec(ctx)
	if err != nil && ent.IsNotFound(err) {
		return ErrVendorNotFound
	}
	return err
}

// LoadVendors fetches every model's vendor (when set) in a single query, then
// returns a map keyed by model_id so handlers can decorate model views without
// an N+1.
func (s *ModelSvc) LoadVendors(ctx context.Context, models []*ent.Model) (map[uuid.UUID]*ent.ModelVendor, error) {
	ids := make(map[uuid.UUID]struct{})
	for _, m := range models {
		if m.VendorID != nil {
			ids[*m.VendorID] = struct{}{}
		}
	}
	if len(ids) == 0 {
		return map[uuid.UUID]*ent.ModelVendor{}, nil
	}
	uuidList := make([]uuid.UUID, 0, len(ids))
	for id := range ids {
		uuidList = append(uuidList, id)
	}
	vs, err := s.d.Ent.ModelVendor.Query().Where(entvendor.IDIn(uuidList...)).All(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[uuid.UUID]*ent.ModelVendor, len(vs))
	for _, v := range vs {
		out[v.ID] = v
	}
	return out, nil
}

// slugify makes a URL-safe lowercase identifier (a-z, 0-9, dashes). Used to
// auto-generate vendor slugs when the admin didn't provide one.
var slugifyRe = regexp.MustCompile(`[^a-z0-9]+`)

func slugify(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = slugifyRe.ReplaceAllString(s, "-")
	return strings.Trim(s, "-")
}
