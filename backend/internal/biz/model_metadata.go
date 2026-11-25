package biz

import (
	"context"
	"slices"

	"github.com/google/uuid"

	"github.com/jucode/jucode/internal/data/ent"
	entgroupaccount "github.com/jucode/jucode/internal/data/ent/groupaccount"
	entgrpmodel "github.com/jucode/jucode/internal/data/ent/modelgroupmodel"
	entaccount "github.com/jucode/jucode/internal/data/ent/upstreamaccount"
	entaccountmodel "github.com/jucode/jucode/internal/data/ent/upstreamaccountmodel"
)

// ModelCapabilities is the capability metadata /v1/models and /api/pricing
// advertise for a model. Every value is operator-configured (models table,
// per-account override for the window); a zero / nil value means "not
// configured" and the field is omitted rather than guessed — a client
// budgeting against a made-up window truncates or overflows prompts.
type ModelCapabilities struct {
	// ContextWindow is the smallest window among the accounts that can serve
	// the model, so any request that fits it fits whichever account the
	// router picks. MaxContextWindow is the largest; a client may opt into
	// it, and the gateway then fails a too-long request over to an account
	// with a larger window (see llm.contextRetry).
	ContextWindow    int
	MaxContextWindow int
	MaxOutputTokens  int
	ReasoningEfforts []string
	// Groups is the same window range through each group that serves the
	// model (a group's channel may give it another window); groups whose
	// accounts configure none are left out.
	Groups map[uuid.UUID]WindowRange
}

// WindowRange is the smallest and largest context window over a set of accounts.
type WindowRange struct {
	ContextWindow    int
	MaxContextWindow int
}

func (r *WindowRange) add(w int) {
	if r.ContextWindow == 0 || w < r.ContextWindow {
		r.ContextWindow = w
	}
	if w > r.MaxContextWindow {
		r.MaxContextWindow = w
	}
}

// AccountContextWindow is the window one account binding gives a model in a
// group: the binding's own override, else the group's window for the model
// (groupWindow, from model_group_models), else the model default; 0 when none
// is configured.
func AccountContextWindow(m *ent.Model, groupWindow *int, bound *ent.UpstreamAccountModel) int {
	if bound != nil && bound.ContextWindow != nil && *bound.ContextWindow > 0 {
		return *bound.ContextWindow
	}
	if groupWindow != nil && *groupWindow > 0 {
		return *groupWindow
	}
	if m != nil && m.ContextWindow != nil && *m.ContextWindow > 0 {
		return *m.ContextWindow
	}
	return 0
}

// ModelCapabilities computes the advertised capabilities of each model over
// the accounts reachable through groupsByModel (the caller's accessible
// groups for /v1/models, the public catalog groups for /api/pricing).
// Accounts are filtered the way groupRouteAccounts filters them, minus the
// per-request protocol check; windows nobody configured are skipped.
func (s *ModelSvc) ModelCapabilities(ctx context.Context, models []*ent.Model, groupsByModel map[uuid.UUID][]*ent.ModelGroup) (map[uuid.UUID]ModelCapabilities, error) {
	out := make(map[uuid.UUID]ModelCapabilities, len(models))
	groupIDs := map[uuid.UUID]bool{}
	names := make([]string, 0, len(models))
	for _, m := range models {
		names = append(names, m.Name)
		for _, g := range groupsByModel[m.ID] {
			groupIDs[g.ID] = true
		}
	}
	linksByGroup := map[uuid.UUID][]*ent.GroupAccount{}
	bound := map[uuid.UUID]map[string]*ent.UpstreamAccountModel{}
	usable := map[uuid.UUID]bool{}
	// group → model → the group's window for it.
	groupWindows := map[uuid.UUID]map[uuid.UUID]*int{}
	if len(groupIDs) > 0 {
		ids := make([]uuid.UUID, 0, len(groupIDs))
		for id := range groupIDs {
			ids = append(ids, id)
		}
		modelIDs := make([]uuid.UUID, 0, len(models))
		for _, m := range models {
			modelIDs = append(modelIDs, m.ID)
		}
		windows, err := s.d.Ent.ModelGroupModel.Query().
			Where(entgrpmodel.GroupIDIn(ids...), entgrpmodel.ModelIDIn(modelIDs...), entgrpmodel.ContextWindowNotNil()).
			All(ctx)
		if err != nil {
			return nil, err
		}
		for _, w := range windows {
			if groupWindows[w.GroupID] == nil {
				groupWindows[w.GroupID] = map[uuid.UUID]*int{}
			}
			groupWindows[w.GroupID][w.ModelID] = w.ContextWindow
		}
		links, err := s.d.Ent.GroupAccount.Query().
			Where(entgroupaccount.GroupIDIn(ids...), entgroupaccount.EnabledEQ(true)).
			All(ctx)
		if err != nil {
			return nil, err
		}
		accountIDs := make([]uuid.UUID, 0, len(links))
		for _, l := range links {
			linksByGroup[l.GroupID] = append(linksByGroup[l.GroupID], l)
			accountIDs = append(accountIDs, l.AccountID)
		}
		if len(accountIDs) > 0 {
			accounts, err := s.d.Ent.UpstreamAccount.Query().
				Where(entaccount.IDIn(accountIDs...), entaccount.DeletedAtIsNil(),
					entaccount.StatusEQ(entaccount.StatusActive), entaccount.SchedulableEQ(true)).
				All(ctx)
			if err != nil {
				return nil, err
			}
			for _, a := range accounts {
				usable[a.ID] = true
			}
			rows, err := s.d.Ent.UpstreamAccountModel.Query().
				Where(entaccountmodel.AccountIDIn(accountIDs...), entaccountmodel.ModelNameIn(names...),
					entaccountmodel.EnabledEQ(true)).
				All(ctx)
			if err != nil {
				return nil, err
			}
			for _, r := range rows {
				if bound[r.AccountID] == nil {
					bound[r.AccountID] = map[string]*ent.UpstreamAccountModel{}
				}
				bound[r.AccountID][r.ModelName] = r
			}
		}
	}

	for _, m := range models {
		c := ModelCapabilities{ReasoningEfforts: m.ReasoningEfforts}
		if m.MaxOutputTokens != nil && *m.MaxOutputTokens > 0 {
			c.MaxOutputTokens = *m.MaxOutputTokens
		}
		var all WindowRange
		for _, g := range groupsByModel[m.ID] {
			groupWindow := groupWindows[g.ID][m.ID]
			var through WindowRange
			for _, l := range linksByGroup[g.ID] {
				if !usable[l.AccountID] {
					continue
				}
				row := bound[l.AccountID][m.Name]
				if row == nil || (len(l.ModelNames) > 0 && !slices.Contains(l.ModelNames, m.Name)) {
					continue
				}
				if w := AccountContextWindow(m, groupWindow, row); w > 0 {
					through.add(w)
				}
			}
			if through.ContextWindow == 0 {
				continue
			}
			if c.Groups == nil {
				c.Groups = map[uuid.UUID]WindowRange{}
			}
			c.Groups[g.ID] = through
			all.add(through.ContextWindow)
			all.add(through.MaxContextWindow)
		}
		// No group gave a window: the model default is still the operator's
		// statement about the model.
		if all.ContextWindow == 0 {
			if w := AccountContextWindow(m, nil, nil); w > 0 {
				all.add(w)
			}
		}
		c.ContextWindow, c.MaxContextWindow = all.ContextWindow, all.MaxContextWindow
		out[m.ID] = c
	}
	return out, nil
}
