package biz

import (
	"sort"
	"strings"

	"github.com/google/uuid"

	"github.com/jucode/jucode/internal/data/ent"
	entapikey "github.com/jucode/jucode/internal/data/ent/apikey"
	entschema "github.com/jucode/jucode/internal/data/ent/schema"
)

// GroupRouteSignal is one group's recent record for a single model.
// A nil field means there is not enough traffic to rank on that axis, so
// the group stays at the median of the groups that do have a sample.
type GroupRouteSignal struct {
	Hit         *float64
	Success     *float64
	TTFTMs      *float64
	TPS         *float64
	OutputRatio *float64
}

// speedRefTokens is the reply length 更快 judges TTFT and TPS against.
// The score is estimated milliseconds to first token plus this many output
// tokens at the group's tokens per second, so a quicker first token and a
// faster decode both move the order.
const speedRefTokens = 128

// routePref is the per-request input to auto-route ordering. Prices are the
// model's own unit prices; the group only contributes its multiplier and its
// observed cache-hit rate.
type routePref struct {
	ModelName   string
	Input       float64
	CacheRead   float64
	Output      float64
	HasInput    bool
	Signals     map[uuid.UUID]GroupRouteSignal
	Sticky      uuid.UUID // session group, shared by models that group can serve
	ModelSticky uuid.UUID // this model left the session group
}

func selectGroup(candidates []*ent.ModelGroup, apiKey *ent.ApiKey, pref routePref) (*ent.ModelGroup, error) {
	groups, err := selectGroups(candidates, apiKey, pref)
	if err != nil {
		return nil, err
	}
	return groups[0], nil
}

func selectGroups(candidates []*ent.ModelGroup, apiKey *ent.ApiKey, pref routePref) ([]*ent.ModelGroup, error) {
	if len(candidates) == 0 {
		return nil, ErrModelNotInGroup
	}
	sort.SliceStable(candidates, func(i, j int) bool { return candidates[i].Name < candidates[j].Name })

	if apiKey == nil {
		return candidates[:1], nil
	}
	allowed := map[uuid.UUID]bool{}
	for _, id := range apiKey.AllowedGroupIds {
		allowed[id] = true
	}
	matches := candidates
	if len(allowed) > 0 {
		matches = make([]*ent.ModelGroup, 0, len(candidates))
		for _, g := range candidates {
			if allowed[g.ID] {
				matches = append(matches, g)
			}
		}
		if len(matches) == 0 {
			return nil, ErrModelNotInGroup
		}
	}
	if !apiKey.AutoRoute {
		return matches[:1], nil
	}
	ordered := orderAutoGroups(matches, apiKey, pref)
	if len(ordered) == 0 {
		return nil, ErrModelNotInGroup
	}
	return ordered, nil
}

// orderAutoGroups sorts groups the key opted into. Price mode applies the
// rate ceiling. A live session does not re-rank: it stays on the group it
// was given while that group still serves the model. A model pin, or a
// model the session group does not serve, keeps its own group.
func orderAutoGroups(matches []*ent.ModelGroup, apiKey *ent.ApiKey, pref routePref) []*ent.ModelGroup {
	if len(matches) == 0 {
		return nil
	}
	mode := entapikey.AutoRouteModePrice
	maxRate := 0.0
	var pin uuid.UUID
	if apiKey != nil {
		if apiKey.AutoRouteMode != "" {
			mode = apiKey.AutoRouteMode
		}
		maxRate = apiKey.AutoRouteMaxRate
		pin = pinnedGroup(apiKey.ModelPins, pref.ModelName)
	}
	held := heldRouteGroup(matches, pref.ModelSticky, pin, pref.Sticky)
	pool := matches
	if held != nil {
		pool = make([]*ent.ModelGroup, 0, len(matches))
		for _, group := range matches {
			if group == nil || group.ID == held.ID {
				continue
			}
			pool = append(pool, group)
		}
	}
	eligible := pool
	if mode != entapikey.AutoRouteModeStable && mode != entapikey.AutoRouteModeFast && maxRate > 0 {
		filtered := make([]*ent.ModelGroup, 0, len(pool))
		for _, group := range pool {
			if group.RateMultiplier <= maxRate || group.ID == pin {
				filtered = append(filtered, group)
			}
		}
		eligible = filtered
	}
	if len(eligible) == 0 {
		if held != nil {
			return []*ent.ModelGroup{held}
		}
		return nil
	}
	ordered := append([]*ent.ModelGroup(nil), eligible...)
	switch mode {
	case entapikey.AutoRouteModeStable:
		sortBySignal(ordered, true, func(group *ent.ModelGroup) (float64, bool) {
			signal, ok := pref.Signals[group.ID]
			if !ok || signal.Success == nil {
				return 0, false
			}
			return *signal.Success, true
		})
	case entapikey.AutoRouteModeFast:
		orderBySpeed(ordered, pref.Signals)
	default:
		sort.SliceStable(ordered, func(i, j int) bool {
			left, right := priceScore(ordered[i], pref), priceScore(ordered[j], pref)
			if left != right {
				return left < right
			}
			return ordered[i].Name < ordered[j].Name
		})
	}
	ordered = preferGroup(ordered, pin)
	if held != nil {
		return append([]*ent.ModelGroup{held}, ordered...)
	}
	return ordered
}

// heldRouteGroup is the group a later turn must keep. A model that already
// left the session stays there; an explicit pin is that model's own group;
// otherwise the session group holds every model it can serve.
func heldRouteGroup(groups []*ent.ModelGroup, modelSticky, pin, session uuid.UUID) *ent.ModelGroup {
	if g := groupByID(groups, modelSticky); g != nil {
		return g
	}
	if g := groupByID(groups, pin); g != nil {
		return g
	}
	return groupByID(groups, session)
}

func groupByID(groups []*ent.ModelGroup, id uuid.UUID) *ent.ModelGroup {
	if id == uuid.Nil {
		return nil
	}
	for _, group := range groups {
		if group != nil && group.ID == id {
			return group
		}
	}
	return nil
}

func priceScore(group *ent.ModelGroup, pref routePref) float64 {
	if !pref.HasInput {
		return group.RateMultiplier
	}
	hit := 0.0
	ratio := 0.25
	if signal, ok := pref.Signals[group.ID]; ok {
		if signal.Hit != nil {
			hit = *signal.Hit
			if hit < 0 {
				hit = 0
			}
			if hit > 1 {
				hit = 1
			}
		}
		if signal.OutputRatio != nil && *signal.OutputRatio >= 0 {
			ratio = *signal.OutputRatio
		}
	}
	unit := (1-hit)*pref.Input + hit*pref.CacheRead + ratio*pref.Output
	return group.RateMultiplier * unit
}

func orderBySpeed(groups []*ent.ModelGroup, signals map[uuid.UUID]GroupRouteSignal) {
	var ttfts, tpss []float64
	for _, group := range groups {
		signal, ok := signals[group.ID]
		if !ok {
			continue
		}
		if signal.TTFTMs != nil {
			ttfts = append(ttfts, *signal.TTFTMs)
		}
		if signal.TPS != nil && *signal.TPS > 0 {
			tpss = append(tpss, *signal.TPS)
		}
	}
	medTTFT, hasTTFT := medianFloat(ttfts)
	medTPS, hasTPS := medianFloat(tpss)
	sortBySignal(groups, false, func(group *ent.ModelGroup) (float64, bool) {
		signal, ok := signals[group.ID]
		return speedScore(signal, ok, medTTFT, hasTTFT, medTPS, hasTPS)
	})
}

func speedScore(signal GroupRouteSignal, present bool, medTTFT float64, hasTTFT bool, medTPS float64, hasTPS bool) (float64, bool) {
	if !present {
		return 0, false
	}
	ownTTFT := signal.TTFTMs != nil
	ownTPS := signal.TPS != nil && *signal.TPS > 0
	if !ownTTFT && !ownTPS {
		return 0, false
	}
	ttft := 0.0
	if ownTTFT {
		ttft = *signal.TTFTMs
	} else if hasTTFT {
		ttft = medTTFT
	}
	tps := 0.0
	if ownTPS {
		tps = *signal.TPS
	} else if hasTPS {
		tps = medTPS
	}
	score := ttft
	if tps > 0 {
		score += speedRefTokens / tps * 1000
	}
	return score, true
}

func sortBySignal(groups []*ent.ModelGroup, higherBetter bool, score func(*ent.ModelGroup) (float64, bool)) {
	known := make([]float64, 0, len(groups))
	for _, group := range groups {
		if value, ok := score(group); ok {
			known = append(known, value)
		}
	}
	neutral, hasNeutral := medianFloat(known)
	sort.SliceStable(groups, func(i, j int) bool {
		left, okL := score(groups[i])
		right, okR := score(groups[j])
		if !okL && hasNeutral {
			left = neutral
		}
		if !okR && hasNeutral {
			right = neutral
		}
		if left != right {
			if higherBetter {
				return left > right
			}
			return left < right
		}
		return groups[i].Name < groups[j].Name
	})
}

func medianFloat(values []float64) (float64, bool) {
	if len(values) == 0 {
		return 0, false
	}
	sorted := append([]float64(nil), values...)
	sort.Float64s(sorted)
	n := len(sorted)
	if n%2 == 0 {
		return (sorted[n/2-1] + sorted[n/2]) / 2, true
	}
	return sorted[n/2], true
}

func preferGroup(groups []*ent.ModelGroup, id uuid.UUID) []*ent.ModelGroup {
	if id == uuid.Nil || len(groups) < 2 {
		return groups
	}
	idx := -1
	for i, group := range groups {
		if group.ID == id {
			idx = i
			break
		}
	}
	if idx <= 0 {
		return groups
	}
	chosen := groups[idx]
	copy(groups[1:idx+1], groups[:idx])
	groups[0] = chosen
	return groups
}

func pinnedGroup(pins []entschema.ModelPin, model string) uuid.UUID {
	model = strings.TrimSpace(model)
	if model == "" {
		return uuid.Nil
	}
	var id uuid.UUID
	for _, pin := range pins {
		if strings.EqualFold(strings.TrimSpace(pin.Model), model) {
			id = pin.GroupID
		}
	}
	return id
}
