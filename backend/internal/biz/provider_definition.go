package biz

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jucode/jucode/internal/data"
	"github.com/jucode/jucode/internal/data/ent"
	entdefinition "github.com/jucode/jucode/internal/data/ent/providerdefinition"
	"github.com/jucode/jucode/internal/data/ent/schema"
)

// ErrDefinitionNotFound / ErrDefinitionBadInput are the definition-layer
// sentinels the admin boundary maps onto 404 / 400.
var (
	ErrDefinitionNotFound = errors.New("provider definition not found")
	ErrDefinitionBadInput = errors.New("invalid provider definition")
)

// ProviderDefinitionModel is one entry of a definition's model list.
type ProviderDefinitionModel struct {
	Name string `json:"name"`
	// Protocol is the wire protocol the model is served over; empty means the
	// definition default.
	Protocol string `json:"protocol,omitempty"`
}

// ProviderDefinitionView is the admin-facing projection of a definition.
type ProviderDefinitionView struct {
	DefinitionID    string                    `json:"definition_id"`
	Name            string                    `json:"name"`
	Vendor          string                    `json:"vendor"`
	BaseURL         string                    `json:"base_url"`
	APIFormat       string                    `json:"api_format"`
	NativeProtocols []string                  `json:"native_protocols"`
	DefaultProtocol string                    `json:"default_protocol"`
	AllowConversion bool                      `json:"allow_conversion"`
	EndpointPaths   map[string]any            `json:"endpoint_paths,omitempty"`
	HeaderRules     schema.HeaderRules        `json:"header_rules"`
	Models          []ProviderDefinitionModel `json:"models,omitempty"`
	// ModelsPath overrides the upstream model-list endpoint for sync
	// ("" = the /v1/models convention, ModelsPathNone = the vendor has no
	// list endpoint and sync binds Models instead).
	ModelsPath string `json:"models_path,omitempty"`
	Enabled    bool   `json:"enabled"`
	// CredentialLabel / CredentialHint name the secret this vendor expects.
	CredentialLabel string `json:"credential_label,omitempty"`
	CredentialHint  string `json:"credential_hint,omitempty"`
	Builtin         bool   `json:"builtin"`
	// Source is "builtin" while the row still comes from code, "database"
	// once it has been seeded or authored.
	Source    string     `json:"source"`
	Version   int        `json:"version"`
	Note      string     `json:"note,omitempty"`
	UpdatedAt *time.Time `json:"updated_at,omitempty"`
	// PayloadError reports a stored payload that failed to parse. It is
	// surfaced rather than fatal so a broken definition cannot hide the rest.
	PayloadError string `json:"payload_error,omitempty"`
}

// UpsertProviderDefinitionInput is the write shape.
type UpsertProviderDefinitionInput struct {
	DefinitionID    string
	Name            string
	Vendor          string
	BaseURL         string
	APIFormat       string
	NativeProtocols []string
	DefaultProtocol string
	AllowConversion bool
	EndpointPaths   map[string]string
	HeaderRules     *schema.HeaderRules
	ModelsJSON      string
	ModelsPath      string
	Enabled         *bool
	Note            string
	// CredentialLabel / CredentialHint describe the secret this vendor wants.
	CredentialLabel string
	CredentialHint  string
}

// ProviderDefinitionSvc owns provider definitions: the vendor payloads that
// accounts are created from.
//
// Resolution is overlay-style: a database row with the same definition id wins
// over the shipped definition, so an operator can fork a builtin without
// touching code, and a definition removed from the table falls back to the
// shipped copy.
type ProviderDefinitionSvc struct {
	d *data.Data
}

func NewProviderDefinitionSvc(d *data.Data) *ProviderDefinitionSvc {
	return &ProviderDefinitionSvc{d: d}
}

// List returns every definition: database rows plus shipped definitions that
// have no row yet.
func (s *ProviderDefinitionSvc) List(ctx context.Context) ([]ProviderDefinitionView, error) {
	rows, err := s.d.Ent.ProviderDefinition.Query().
		Order(ent.Asc(entdefinition.FieldVendor), ent.Asc(entdefinition.FieldDefinitionID)).
		All(ctx)
	if err != nil {
		return nil, err
	}
	seen := make(map[string]bool, len(rows))
	out := make([]ProviderDefinitionView, 0, len(rows)+2)
	for _, row := range rows {
		view := s.viewFromRow(row)
		seen[view.DefinitionID] = true
		out = append(out, view)
	}
	for _, builtin := range builtinDefinitions() {
		if seen[builtin.DefinitionID] {
			continue
		}
		out = append(out, viewFromBuiltin(builtin))
	}
	return out, nil
}

// Resolve returns one definition by its stable id, preferring the database row.
func (s *ProviderDefinitionSvc) Resolve(ctx context.Context, definitionID string) (*ProviderDefinitionView, error) {
	id := strings.TrimSpace(definitionID)
	if id == "" {
		return nil, fmt.Errorf("%w: definition_id is required", ErrDefinitionBadInput)
	}
	row, err := s.d.Ent.ProviderDefinition.Query().
		Where(entdefinition.DefinitionIDEQ(id)).
		First(ctx)
	if err == nil {
		view := s.viewFromRow(row)
		return &view, nil
	}
	if !ent.IsNotFound(err) {
		return nil, err
	}
	if builtin, ok := BuiltinDefinitionByID(id); ok {
		view := viewFromBuiltin(builtin)
		return &view, nil
	}
	return nil, ErrDefinitionNotFound
}

// SeedBuiltins materializes shipped definitions that have no row yet, so an
// operator can edit them. Existing rows are never overwritten.
func (s *ProviderDefinitionSvc) SeedBuiltins(ctx context.Context) (inserted int, skipped int, err error) {
	for _, builtin := range builtinDefinitions() {
		exists, err := s.d.Ent.ProviderDefinition.Query().
			Where(entdefinition.DefinitionIDEQ(builtin.DefinitionID)).
			Exist(ctx)
		if err != nil {
			return inserted, skipped, err
		}
		if exists {
			skipped++
			continue
		}
		in := UpsertProviderDefinitionInput{
			DefinitionID:    builtin.DefinitionID,
			Name:            builtin.Name,
			Vendor:          builtin.Vendor,
			BaseURL:         builtin.BaseURL,
			APIFormat:       builtin.APIFormat,
			NativeProtocols: builtin.NativeProtocols,
			DefaultProtocol: builtin.DefaultProtocol,
			EndpointPaths:   stringifyEndpointPaths(builtin.EndpointPaths),
			HeaderRules:     &builtin.HeaderRules,
			ModelsJSON:      builtin.ModelsJSON,
			ModelsPath:      builtin.ModelsPath,
			Note:            builtin.Note,
		}
		enabled := true
		in.Enabled = &enabled
		if _, err := s.upsert(ctx, in, true); err != nil {
			return inserted, skipped, err
		}
		inserted++
	}
	return inserted, skipped, nil
}

// Upsert creates or replaces a definition.
func (s *ProviderDefinitionSvc) Upsert(ctx context.Context, in UpsertProviderDefinitionInput) (*ProviderDefinitionView, error) {
	return s.upsert(ctx, in, false)
}

func (s *ProviderDefinitionSvc) upsert(ctx context.Context, in UpsertProviderDefinitionInput, builtin bool) (*ProviderDefinitionView, error) {
	id := strings.TrimSpace(in.DefinitionID)
	if id == "" {
		return nil, fmt.Errorf("%w: definition_id is required", ErrDefinitionBadInput)
	}
	if strings.ContainsAny(id, " \t/") {
		return nil, fmt.Errorf("%w: definition_id must not contain spaces or slashes", ErrDefinitionBadInput)
	}
	viewport, err := s.validate(ctx, in)
	if err != nil {
		return nil, err
	}

	existing, err := s.d.Ent.ProviderDefinition.Query().Where(entdefinition.DefinitionIDEQ(id)).First(ctx)
	switch {
	case err == nil:
		q := s.d.Ent.ProviderDefinition.UpdateOneID(existing.ID).
			SetName(in.Name).
			SetVendor(in.Vendor).
			SetBaseURL(strings.TrimSpace(in.BaseURL)).
			SetAPIFormat(strings.TrimSpace(in.APIFormat)).
			SetNativeProtocols(viewport.NativeProtocols).
			SetDefaultProtocol(viewport.DefaultProtocol).
			SetAllowConversion(in.AllowConversion).
			SetEndpointPaths(viewport.EndpointPaths).
			SetHeaderRules(viewport.HeaderRules).
			SetModelsJSON(strings.TrimSpace(in.ModelsJSON)).
			SetModelsPath(strings.TrimSpace(in.ModelsPath)).
			SetCredentialLabel(viewport.CredentialLabel).
			SetCredentialHint(viewport.CredentialHint).
			SetNote(in.Note).
			SetVersion(existing.Version + 1)
		if in.Enabled != nil {
			q = q.SetEnabled(*in.Enabled)
		}
		row, err := q.Save(ctx)
		if err != nil {
			return nil, err
		}
		view := s.viewFromRow(row)
		return &view, nil
	case !ent.IsNotFound(err):
		return nil, err
	}

	q := s.d.Ent.ProviderDefinition.Create().
		SetDefinitionID(id).
		SetName(in.Name).
		SetVendor(in.Vendor).
		SetBaseURL(strings.TrimSpace(in.BaseURL)).
		SetAPIFormat(strings.TrimSpace(in.APIFormat)).
		SetNativeProtocols(viewport.NativeProtocols).
		SetDefaultProtocol(viewport.DefaultProtocol).
		SetAllowConversion(in.AllowConversion).
		SetEndpointPaths(viewport.EndpointPaths).
		SetHeaderRules(viewport.HeaderRules).
		SetModelsJSON(strings.TrimSpace(in.ModelsJSON)).
		SetModelsPath(strings.TrimSpace(in.ModelsPath)).
		SetCredentialLabel(viewport.CredentialLabel).
		SetCredentialHint(viewport.CredentialHint).
		SetBuiltin(builtin).
		SetNote(in.Note)
	if in.Enabled != nil {
		q = q.SetEnabled(*in.Enabled)
	}
	row, err := q.Save(ctx)
	if err != nil {
		return nil, err
	}
	view := s.viewFromRow(row)
	return &view, nil
}

// Delete removes a database row. A shipped definition reappears from code, so
// deleting one is "revert to shipped", never "lose the vendor".
func (s *ProviderDefinitionSvc) Delete(ctx context.Context, definitionID string) error {
	n, err := s.d.Ent.ProviderDefinition.Delete().
		Where(entdefinition.DefinitionIDEQ(strings.TrimSpace(definitionID))).
		Exec(ctx)
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrDefinitionNotFound
	}
	return nil
}

// validate normalizes and checks a write payload, returning the view whose
// normalized fields the caller stores.
func (s *ProviderDefinitionSvc) validate(ctx context.Context, in UpsertProviderDefinitionInput) (*ProviderDefinitionView, error) {
	view := &ProviderDefinitionView{
		DefinitionID:    strings.TrimSpace(in.DefinitionID),
		Name:            in.Name,
		Vendor:          in.Vendor,
		BaseURL:         strings.TrimSpace(in.BaseURL),
		APIFormat:       strings.TrimSpace(in.APIFormat),
		AllowConversion: in.AllowConversion,
		Note:            in.Note,
		ModelsPath:      strings.TrimSpace(in.ModelsPath),
		Enabled:         true,
	}
	view.CredentialLabel = strings.TrimSpace(in.CredentialLabel)
	view.CredentialHint = strings.TrimSpace(in.CredentialHint)
	if view.ModelsPath != "" && view.ModelsPath != ModelsPathNone {
		if err := validateEndpointPath(view.ModelsPath); err != nil {
			return nil, err
		}
	}

	protocols, err := NormalizeNativeProtocols(in.NativeProtocols)
	if err != nil {
		return nil, err
	}
	view.NativeProtocols = protocols
	if raw := strings.TrimSpace(in.DefaultProtocol); raw != "" {
		normalized, ok := ParseNativeProtocol(raw)
		if !ok {
			return nil, fmt.Errorf("%w: unknown default_protocol %q", ErrDefinitionBadInput, in.DefaultProtocol)
		}
		view.DefaultProtocol = normalized
	} else if len(protocols) > 0 {
		view.DefaultProtocol = protocols[0]
	}

	paths, err := NormalizeEndpointPaths(in.EndpointPaths)
	if err != nil {
		return nil, err
	}
	rules, err := NormalizeHeaderRules(in.HeaderRules)
	if err != nil {
		return nil, err
	}
	if rules != nil {
		view.HeaderRules = *rules
	}
	if len(paths) > 0 {
		view.EndpointPaths = map[string]any{}
		for key, value := range paths {
			view.EndpointPaths[key] = value
		}
	}

	if raw := strings.TrimSpace(in.ModelsJSON); raw != "" {
		var models []ProviderDefinitionModel
		if err := json.Unmarshal([]byte(raw), &models); err != nil {
			return nil, fmt.Errorf("%w: models must be a JSON array: %v", ErrDefinitionBadInput, err)
		}
		for i := range models {
			if strings.TrimSpace(models[i].Name) == "" {
				return nil, fmt.Errorf("%w: model %d has no name", ErrDefinitionBadInput, i)
			}
			if models[i].Protocol != "" {
				normalized, ok := ParseNativeProtocol(models[i].Protocol)
				if !ok {
					return nil, fmt.Errorf("%w: model %s has unknown protocol %q", ErrDefinitionBadInput, models[i].Name, models[i].Protocol)
				}
				models[i].Protocol = normalized
			}
		}
		view.Models = models
	}
	return view, nil
}

func (s *ProviderDefinitionSvc) viewFromRow(row *ent.ProviderDefinition) ProviderDefinitionView {
	view := ProviderDefinitionView{
		DefinitionID:    row.DefinitionID,
		Name:            row.Name,
		Vendor:          row.Vendor,
		BaseURL:         row.BaseURL,
		APIFormat:       row.APIFormat,
		NativeProtocols: row.NativeProtocols,
		DefaultProtocol: row.DefaultProtocol,
		AllowConversion: row.AllowConversion,
		EndpointPaths:   row.EndpointPaths,
		HeaderRules:     row.HeaderRules,
		ModelsPath:      row.ModelsPath,
		CredentialLabel: row.CredentialLabel,
		CredentialHint:  row.CredentialHint,
		Enabled:         row.Enabled,
		Builtin:         row.Builtin,
		Source:          "database",
		Version:         row.Version,
		Note:            row.Note,
	}
	updated := row.UpdatedAt
	view.UpdatedAt = &updated
	view.Models = parseModelsLenient(row.ModelsJSON, &view)
	return view
}

func viewFromBuiltin(builtin BuiltinDefinition) ProviderDefinitionView {
	view := ProviderDefinitionView{
		DefinitionID:    builtin.DefinitionID,
		Name:            builtin.Name,
		Vendor:          builtin.Vendor,
		BaseURL:         builtin.BaseURL,
		APIFormat:       builtin.APIFormat,
		NativeProtocols: builtin.NativeProtocols,
		DefaultProtocol: builtin.DefaultProtocol,
		ModelsPath:      builtin.ModelsPath,
		EndpointPaths:   builtin.EndpointPaths,
		HeaderRules:     builtin.HeaderRules,
		CredentialLabel: builtin.CredentialLabel,
		CredentialHint:  builtin.CredentialHint,
		Enabled:         true,
		Builtin:         true,
		Source:          "builtin",
		Version:         0,
		Note:            builtin.Note,
	}
	view.Models = parseModelsLenient(builtin.ModelsJSON, &view)
	return view
}

func parseModelsLenient(raw string, view *ProviderDefinitionView) []ProviderDefinitionModel {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return nil
	}
	var models []ProviderDefinitionModel
	if err := json.Unmarshal([]byte(trimmed), &models); err != nil {
		view.PayloadError = "models: " + err.Error()
		return nil
	}
	return models
}

func stringifyEndpointPaths(paths map[string]any) map[string]string {
	if len(paths) == 0 {
		return nil
	}
	out := make(map[string]string, len(paths))
	for key, value := range paths {
		if text, ok := value.(string); ok {
			out[key] = text
		}
	}
	return out
}
