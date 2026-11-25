// Package biz contains the public status page aggregation.
package biz

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"math"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/jucode/jucode/internal/data"
	"github.com/jucode/jucode/internal/data/ent"
	entgroupaccount "github.com/jucode/jucode/internal/data/ent/groupaccount"
	entmodel "github.com/jucode/jucode/internal/data/ent/model"
	entgroup "github.com/jucode/jucode/internal/data/ent/modelgroup"
	entgroupmodel "github.com/jucode/jucode/internal/data/ent/modelgroupmodel"
	entaccount "github.com/jucode/jucode/internal/data/ent/upstreamaccount"
	entaccountmodel "github.com/jucode/jucode/internal/data/ent/upstreamaccountmodel"
)

type StatusSvc struct {
	d        *data.Data
	settings *SettingsSvc
	alerts   *OpsAlertSvc
}

func NewStatusSvc(d *data.Data, settings *SettingsSvc) *StatusSvc {
	return &StatusSvc{d: d, settings: settings}
}

func (s *StatusSvc) SetOpsAlerts(alerts *OpsAlertSvc) { s.alerts = alerts }

// StartAlertLoop guarantees incident transitions are evaluated even when no
// one is polling the public status page. It remains cheap while alerts are
// disabled: only the cached settings flag is read on each tick.
func (s *StatusSvc) StartAlertLoop(ctx context.Context, interval time.Duration) {
	if s.alerts == nil {
		return
	}
	if interval < 30*time.Second {
		interval = 30 * time.Second
	}
	check := func() {
		enabled, err := s.alerts.Enabled(ctx)
		if err != nil {
			slog.Warn("ops_alert.status_check_failed", "err", err)
			return
		}
		if !enabled {
			return
		}
		if _, err := s.Snapshot(ctx); err != nil {
			slog.Warn("ops_alert.status_snapshot_failed", "err", err)
		}
	}
	check()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			check()
		}
	}
}

type StatusState string

const (
	StatusHealthy  StatusState = "healthy"
	StatusDegraded StatusState = "degraded"
	// StatusSerious sits between degraded and down: badly impaired but still
	// serving most requests. It exists because "down" previously covered
	// everything from 89% to 0%, which told a reader nothing about how bad a
	// thing was and made a mild dip look identical to a total outage.
	StatusSerious StatusState = "serious"
	StatusDown    StatusState = "down"
	StatusUnknown StatusState = "unknown"
)

const (
	// 48 buckets, not 96. The count fixes the bar's time resolution, and that
	// resolution decides whether a cell's verdict means anything: on the 6h
	// window this page shipped with, 96 buckets put a mid-traffic group at ~10
	// eligible requests per cell, where one failure reads as 90% and two read as
	// red. The bar became a stipple of noise that trained people to ignore it.
	// Halving the count doubles the traffic behind every cell (and on a 24h
	// window puts that same group near 85), so a red cell is evidence rather
	// than variance. Cells stay divisible by the 24-dot model strip.
	statusBucketCount = 48
	// Widened from 15m so a probe cadence at or under 15m always lands at least
	// one sample in the window, including after a missed tick. The trade is a
	// slower reaction to both failure and recovery.
	statusStateWindow = 30 * time.Minute
	// statusStateMinFailures keeps a single failed sample from grading anything
	// below healthy; see publicState.
	statusStateMinFailures = 2
	// Four bands: >=98 healthy, >=90 degraded, >=80 serious, below that down.
	// The healthy line sits at 98 rather than 99 because this gateway fronts
	// third-party upstreams whose own error rate it cannot control, and the
	// bottom line sits at 80 so "down" means mostly-not-working rather than
	// anything at all below 90.
	statusHealthyThreshold  = 98.0
	statusDegradedThreshold = 90.0
	statusSeriousThreshold  = 80.0
	// statusBucketMinSamples is the per-bucket floor for taking a verdict on a
	// single cell. Mirrors AVAILABILITY_MIN_SAMPLES on the client, which colours
	// the same buckets — the two must agree or the bar and the incident duration
	// will disagree about when an outage began.
	statusBucketMinSamples = 5
)

// StatusBucket is one cell of the 24h bar. It ships a verdict, never the
// evidence behind it: raw counts would let anyone reading the public endpoint
// reconstruct our traffic volume cell by cell, so they stay server-side
// (json:"-") and the client colours from State alone.
type StatusBucket struct {
	Time  time.Time   `json:"time"`
	State StatusState `json:"state"`

	Good            int      `json:"-"`
	Bad             int      `json:"-"`
	Excluded        int      `json:"-"`
	AvailabilityPct *float64 `json:"-"`
}

// GroupStatus is one card. Request counts are withheld from the payload for the
// same reason as StatusBucket — publishing them turns the status page into a
// live traffic meter for anyone who polls it. They remain populated in-process
// because the incident and state logic is built on them.
type GroupStatus struct {
	ID                 string         `json:"id"`
	Name               string         `json:"name"`
	Description        string         `json:"description"`
	State              StatusState    `json:"state"`
	StateWindowMinutes int            `json:"state_window_minutes"`
	AvailabilityPct    *float64       `json:"availability_pct"`
	LastObservedAt     *time.Time     `json:"last_observed_at"`
	Fresh              bool           `json:"fresh"`
	AvgLatencyMs       int            `json:"avg_latency_ms"`
	Buckets            []StatusBucket `json:"buckets"`

	StateSampleCount int `json:"-"`
	GoodRequests     int `json:"-"`
	BadRequests      int `json:"-"`
	ExcludedRequests int `json:"-"`
	SampleCount      int `json:"-"`
	// Models carries the per-model breakdown scoped to this group only. The
	// same model served through two groups sits behind different providers,
	// so its numbers are computed per group and never pooled across them.
	Models []ModelStatus `json:"models"`
}

type ModelStatus struct {
	ID                 string         `json:"id"`
	GroupID            string         `json:"group_id"`
	Name               string         `json:"name"`
	State              StatusState    `json:"state"`
	StateWindowMinutes int            `json:"state_window_minutes"`
	AvailabilityPct    *float64       `json:"availability_pct"`
	LastObservedAt     *time.Time     `json:"last_observed_at"`
	Fresh              bool           `json:"fresh"`
	AvgLatencyMs       int            `json:"avg_latency_ms"`
	Buckets            []StatusBucket `json:"buckets"`

	StateSampleCount int `json:"-"`
	GoodRequests     int `json:"-"`
	BadRequests      int `json:"-"`
	ExcludedRequests int `json:"-"`
	SampleCount      int `json:"-"`
}

// StatusIncident is one currently-misbehaving group, derived from the same
// 15-minute state window the group card uses. It is not a stored record: there
// is no lifecycle and no human-authored copy, so it appears and disappears with
// the underlying traffic.
type StatusIncident struct {
	GroupID    string      `json:"group_id"`
	GroupName  string      `json:"group_name"`
	State      StatusState `json:"state"`
	SuccessPct *float64    `json:"success_pct"`

	SampleCount int `json:"-"`
	// StartedAt is inferred from the bucket grid, so it is only as precise as
	// one bucket (window/96). Null when the outage predates the whole window.
	StartedAt *time.Time `json:"started_at"`
}

type PublicStatus struct {
	UpdatedAt time.Time     `json:"updated_at"`
	WindowH   int           `json:"window_hours"`
	Groups    []GroupStatus `json:"groups"`
	// Incidents lists only degraded/down groups, in the same order as Groups.
	// Empty means every configured group is healthy or simply too quiet to
	// judge — the page renders a green all-clear rather than a warning.
	Incidents []StatusIncident `json:"incidents"`
	// Upstream mirrors what OpenAI/Anthropic publish about themselves. It is
	// a separate field, not folded into Incidents, because the two answer
	// different questions: Incidents is measured from our own traffic,
	// Upstream is the vendor's self-report, and the vendor reading green
	// while our traffic degrades is routine. Null when the operator has the
	// panel switched off.
	Upstream *UpstreamSnapshot `json:"upstream,omitempty"`
}

// groupModelKey scopes a model's observations to the group that served them.
type groupModelKey struct {
	GroupID uuid.UUID
	Model   string
}

type statusAggregate struct {
	GroupID      uuid.UUID
	Good         int
	Bad          int
	Excluded     int
	LatencySum   int64
	LatencyCount int
	LastAny      *time.Time
	LastEligible *time.Time
}

func (a *statusAggregate) add(good, bad, excluded int, latencySum int64, latencyCount int, lastAny, lastEligible *time.Time) {
	a.Good += good
	a.Bad += bad
	a.Excluded += excluded
	a.LatencySum += latencySum
	a.LatencyCount += latencyCount
	if lastAny != nil && (a.LastAny == nil || lastAny.After(*a.LastAny)) {
		v := lastAny.UTC()
		a.LastAny = &v
	}
	if lastEligible != nil && (a.LastEligible == nil || lastEligible.After(*a.LastEligible)) {
		v := lastEligible.UTC()
		a.LastEligible = &v
	}
}

func (a statusAggregate) sampleCount() int { return a.Good + a.Bad }

// availabilityPercentage rounds to two decimals, which is not cosmetic. At full
// float64 precision the value is an exact rational: 99.2248062015504 is 256/258
// and nothing else, so publishing it hands out the denominator — the very
// request count the payload withholds. Two decimals collapse each value onto a
// whole family of ratios (256/258, 128/129, 384/387 …), leaving the ratio
// visible and the scale hidden.
func availabilityPercentage(good, bad int) *float64 {
	eligible := good + bad
	if eligible == 0 {
		return nil
	}
	pct := math.Round(100*float64(good)/float64(eligible)*100) / 100
	return &pct
}

// publicState grades the state window. There is no minimum sample count: with
// synthetic probes filling the window, a group that shows nothing is genuinely
// unreachable rather than merely quiet, and holding out for a business-traffic
// quorum is what used to leave low-traffic groups permanently unknown.
//
// Dropping that floor means the window can hold as few as one or two samples,
// where a single failure computes to 50% or 0% and would paint the public page
// red off one transient. So every band below healthy requires a second failure
// to have actually happened. It used to gate only the bottom two bands, letting
// one stray failure grade a quiet model degraded — and since one degraded model
// degrades its group, a single failed request in a 2-request window was enough
// to take a whole card off "运行正常". Above that floor the rule is inert: a
// busy model failing badly enough to leave healthy has many more than two
// failures.
func publicState(good, bad int) StatusState {
	n := good + bad
	if n == 0 {
		return StatusUnknown
	}
	pct := 100 * float64(good) / float64(n)
	switch {
	case pct >= statusHealthyThreshold:
		return StatusHealthy
	case bad < statusStateMinFailures:
		return StatusHealthy
	case pct >= statusDegradedThreshold:
		return StatusDegraded
	case pct >= statusSeriousThreshold:
		return StatusSerious
	default:
		return StatusDown
	}
}

// groupStateFromModels grades a group from its per-model verdicts, not from
// pooled request counts. A single hot failure (one model, lots of probes)
// must not paint the whole card down: "unavailable" is reserved for when
// every known model is down, or a majority of them are. One impaired model
// in a larger group is a partial outage — degraded, or serious only if a
// majority of known models are already serious-or-worse.
func groupStateFromModels(states []StatusState) StatusState {
	var known, down, serious, degraded int
	for _, state := range states {
		switch state {
		case StatusUnknown, "":
			continue
		case StatusDown:
			down++
			known++
		case StatusSerious:
			serious++
			known++
		case StatusDegraded:
			degraded++
			known++
		default:
			known++
		}
	}
	if known == 0 {
		return StatusUnknown
	}
	if down == known || down*2 > known {
		return StatusDown
	}
	if (down+serious)*2 > known {
		return StatusSerious
	}
	if down+serious+degraded > 0 {
		return StatusDegraded
	}
	return StatusHealthy
}

// groupState grades a card from its per-model vote, raised to serious when the
// group's pooled requests are themselves serious or worse. The vote alone let
// one hot failing model sit behind a "degraded" pill while most of the group's
// traffic failed (41% success read as 轻度下降). The raise stops at serious so
// that same hot model still cannot call the whole group down, which is what
// the vote exists to prevent.
func groupState(modelStates []StatusState, good, bad int) StatusState {
	vote := groupStateFromModels(modelStates)
	if severityRank(publicState(good, bad)) >= severityRank(StatusSerious) && severityRank(vote) < severityRank(StatusSerious) {
		return StatusSerious
	}
	return vote
}

// incidentStartedAt walks the bucket grid backwards and returns the start of the
// trailing run of unhealthy buckets. Empty buckets neither break the run nor
// start one: a quiet stretch inside an outage is not evidence of recovery, and
// a quiet stretch before it should not be blamed for the outage. Returns nil
// when the run reaches the start of the window, since the real onset is older
// than anything we can see.
//
// The old per-bucket sample floor is gone along with its state-window twin: a
// probed bucket only holds a couple of samples, so any floor would skip every
// cell and no incident would ever get a start time. The single-failure damping
// inside publicState now does that job — one stray failure grades as degraded,
// not down, so it no longer drags the run backwards on its own.
func incidentStartedAt(buckets []StatusBucket) *time.Time {
	var start *time.Time
	for i := len(buckets) - 1; i >= 0; i-- {
		b := buckets[i]
		if b.State == StatusUnknown {
			continue
		}
		if b.State == StatusHealthy {
			return start
		}
		// Too thin to blame: one failed request in an otherwise quiet cell would
		// drag the run backwards and inflate the "已持续约 N 小时" users see.
		// Either arm alone is enough to judge — a busy cell carries its own
		// weight, and a probe-only cell holds just two samples per half hour but
		// two failures in a row still mean something.
		if b.Good+b.Bad < statusBucketMinSamples && b.Bad < statusStateMinFailures {
			continue
		}
		t := b.Time
		start = &t
	}
	// Fell off the front of the window without seeing a healthy bucket.
	return nil
}

func emptyStatusBuckets(start time.Time, bucketDuration time.Duration) []StatusBucket {
	buckets := make([]StatusBucket, statusBucketCount)
	for i := range buckets {
		buckets[i].Time = start.Add(time.Duration(i) * bucketDuration)
		// Explicit rather than the zero value: State is what the client renders
		// now, and "" would serialize as an unhandled state rather than a gap.
		buckets[i].State = StatusUnknown
	}
	return buckets
}

// Snapshot reads business observations plus synthetic probe observations. The
// split is per-field and deliberate: the published availability percentage
// counts business traffic only, while the state pill and the 24h bar also count
// probes so a model nobody happens to be calling still reports whether it works.
// It deliberately does not fall back to retired provider-level data, which
// measured a channel rather than a (group, model) route.
func (s *StatusSvc) Snapshot(ctx context.Context) (*PublicStatus, error) {
	cfg, err := s.settings.GetSite(ctx)
	if err != nil {
		return nil, err
	}
	windowHours := cfg.StatusPageWindowHours
	if windowHours < 1 {
		windowHours = 24
	}
	if windowHours > 168 {
		windowHours = 168
	}
	now := time.Now().UTC()
	out := &PublicStatus{
		UpdatedAt: now,
		WindowH:   windowHours,
		Groups:    []GroupStatus{},
		Incidents: []StatusIncident{},
	}
	// Filled before the no-groups early return: the vendor mirror is
	// independent of our own group config and stays useful even on an
	// otherwise unconfigured page.
	if cfg.StatusPageUpstreamEnabled {
		out.Upstream = readUpstream(ctx, s.d)
	}
	if len(cfg.StatusPageGroupIDs) == 0 {
		return out, nil
	}

	groups, err := s.d.Ent.ModelGroup.Query().
		Where(entgroup.IDIn(cfg.StatusPageGroupIDs...), entgroup.EnabledEQ(true), entgroup.ExclusiveEQ(false)).
		All(ctx)
	if err != nil {
		return nil, err
	}
	if len(groups) == 0 {
		return out, nil
	}
	// StatusPageGroupIDs is an ordered list the admin arranged by hand, so the
	// configured order is the display order. Groups deleted since the setting
	// was saved simply drop out.
	groups = orderGroupsByConfig(groups, cfg.StatusPageGroupIDs)

	window := time.Duration(windowHours) * time.Hour
	bucketDuration := window / statusBucketCount
	if bucketDuration <= 0 {
		bucketDuration = time.Minute
	}
	// Align the bucket grid to UTC time boundaries. The query itself remains
	// strictly [start, now), so future or clock-skewed rows are never clamped
	// into the last bucket.
	bucketEnd := now.Truncate(bucketDuration).Add(bucketDuration)
	windowStart := bucketEnd.Add(-bucketDuration * statusBucketCount)
	ids := make([]uuid.UUID, 0, len(groups))
	for _, g := range groups {
		ids = append(ids, g.ID)
	}
	modelsByGroup, err := s.selectedModels(ctx, ids)
	if err != nil {
		return nil, err
	}
	// Three reads per resolution, deliberately not two: the bar and the state
	// pill count probes, the headline percentage does not, so the window-wide
	// business-only totals can no longer be folded out of the bucket scan.
	buckets, history, err := s.queryHistory(ctx, windowStart, now, bucketDuration, ids)
	if err != nil {
		return nil, err
	}
	stateSince := now.Add(-statusStateWindow)
	current, err := s.queryAggregate(ctx, "aggregate", stateSince, now, ids)
	if err != nil {
		return nil, err
	}
	published, err := s.queryAggregate(ctx, "window", windowStart, now, ids)
	if err != nil {
		return nil, err
	}

	pairs, modelNames := groupModelPairs(groups, modelsByGroup)
	modelBuckets, modelHistory, err := s.queryModelHistory(ctx, windowStart, now, bucketDuration, ids, pairs, modelNames)
	if err != nil {
		return nil, err
	}
	modelCurrent, err := s.queryModelAggregate(ctx, "aggregate", stateSince, now, ids, pairs, modelNames)
	if err != nil {
		return nil, err
	}
	modelPublished, err := s.queryModelAggregate(ctx, "window", windowStart, now, ids, pairs, modelNames)
	if err != nil {
		return nil, err
	}

	for _, g := range groups {
		h := history[g.ID]
		c := current[g.ID]
		// p is business-only; h and c include probes. Percentage and latency
		// come from p so synthetic traffic cannot flatter either — probe pings
		// are far faster than real work and would drag the average down.
		p := published[g.ID]
		card := GroupStatus{
			ID:                 g.ID.String(),
			Name:               g.Name,
			Description:        g.Description,
			State:              publicState(c.Good, c.Bad),
			StateWindowMinutes: int(statusStateWindow / time.Minute),
			StateSampleCount:   c.sampleCount(),
			AvailabilityPct:    availabilityPercentage(p.Good, p.Bad),
			GoodRequests:       p.Good,
			BadRequests:        p.Bad,
			ExcludedRequests:   p.Excluded,
			SampleCount:        p.sampleCount(),
			LastObservedAt:     h.LastAny,
			Fresh:              c.sampleCount() > 0 && c.LastEligible != nil,
			Buckets:            buckets[g.ID],
			Models:             []ModelStatus{},
		}
		if p.LatencyCount > 0 {
			card.AvgLatencyMs = int(p.LatencySum / int64(p.LatencyCount))
		}
		modelStates := make([]StatusState, 0, len(modelsByGroup[g.ID]))
		for _, model := range modelsByGroup[g.ID] {
			key := groupModelKey{GroupID: g.ID, Model: model.Name}
			mh := modelHistory[key]
			mc := modelCurrent[key]
			mp := modelPublished[key]
			row := ModelStatus{
				ID:                 model.ID.String(),
				GroupID:            g.ID.String(),
				Name:               model.Name,
				State:              publicState(mc.Good, mc.Bad),
				StateWindowMinutes: int(statusStateWindow / time.Minute),
				StateSampleCount:   mc.sampleCount(),
				AvailabilityPct:    availabilityPercentage(mp.Good, mp.Bad),
				GoodRequests:       mp.Good,
				BadRequests:        mp.Bad,
				ExcludedRequests:   mp.Excluded,
				SampleCount:        mp.sampleCount(),
				LastObservedAt:     mh.LastAny,
				Fresh:              mc.sampleCount() > 0 && mc.LastEligible != nil,
				Buckets:            modelBuckets[key],
			}
			if mp.LatencyCount > 0 {
				row.AvgLatencyMs = int(mp.LatencySum / int64(mp.LatencyCount))
			}
			card.Models = append(card.Models, row)
			modelStates = append(modelStates, row.State)
		}
		// With a model breakdown, the card's pill and bar vote by how many
		// models are impaired; pooled counts can only raise that to serious
		// (see groupState), so a high-volume failure cannot turn the whole
		// group red.
		if len(card.Models) > 0 {
			card.State = groupState(modelStates, c.Good, c.Bad)
			for i := range card.Buckets {
				bucketStates := make([]StatusState, 0, len(card.Models))
				for _, model := range card.Models {
					key := groupModelKey{GroupID: g.ID, Model: model.Name}
					if bs := modelBuckets[key]; i < len(bs) {
						bucketStates = append(bucketStates, bs[i].State)
					}
				}
				card.Buckets[i].State = groupState(bucketStates, card.Buckets[i].Good, card.Buckets[i].Bad)
			}
		}
		out.Groups = append(out.Groups, card)
		if card.State == StatusDown || card.State == StatusSerious || card.State == StatusDegraded {
			out.Incidents = append(out.Incidents, StatusIncident{
				GroupID:     card.ID,
				GroupName:   card.Name,
				State:       card.State,
				SuccessPct:  availabilityPercentage(c.Good, c.Bad),
				SampleCount: card.StateSampleCount,
				StartedAt:   incidentStartedAt(card.Buckets),
			})
		}
	}
	if s.alerts != nil {
		s.alerts.ReconcileStatusIncidentsAsync(out.Groups)
	}
	return out, nil
}

// GroupLiveStates is the status-page pill for an arbitrary set of groups,
// including exclusive ones the public page hides. It uses the same 30-minute
// per-model vote as Snapshot (pooled over the selected models here), without
// the 24h bar.
func (s *StatusSvc) GroupLiveStates(ctx context.Context, groupIDs []uuid.UUID) (map[uuid.UUID]StatusState, error) {
	out := make(map[uuid.UUID]StatusState, len(groupIDs))
	for _, id := range groupIDs {
		out[id] = StatusUnknown
	}
	if len(groupIDs) == 0 {
		return out, nil
	}
	now := time.Now().UTC()
	modelsByGroup, err := s.selectedModels(ctx, groupIDs)
	if err != nil {
		return nil, err
	}
	groups := make([]*ent.ModelGroup, len(groupIDs))
	for i, id := range groupIDs {
		groups[i] = &ent.ModelGroup{ID: id}
	}
	pairs, names := groupModelPairs(groups, modelsByGroup)
	current, err := s.queryModelAggregate(ctx, "aggregate", now.Add(-statusStateWindow), now, groupIDs, pairs, names)
	if err != nil {
		return nil, err
	}
	for _, id := range groupIDs {
		models := modelsByGroup[id]
		if len(models) == 0 {
			continue
		}
		states := make([]StatusState, 0, len(models))
		var good, bad int
		for _, model := range models {
			sample := current[groupModelKey{GroupID: id, Model: model.Name}]
			states = append(states, publicState(sample.Good, sample.Bad))
			good += sample.Good
			bad += sample.Bad
		}
		out[id] = groupState(states, good, bad)
	}
	return out, nil
}

// orderGroupsByConfig reorders the queried groups to match the admin-configured
// ID order. Any group present in the query but missing from the config list
// (impossible today, since the query filters on that same list) is appended so
// it can never be silently dropped.
func orderGroupsByConfig(groups []*ent.ModelGroup, order []uuid.UUID) []*ent.ModelGroup {
	byID := make(map[uuid.UUID]*ent.ModelGroup, len(groups))
	for _, g := range groups {
		byID[g.ID] = g
	}
	out := make([]*ent.ModelGroup, 0, len(groups))
	for _, id := range order {
		if g, ok := byID[id]; ok {
			out = append(out, g)
			delete(byID, id)
		}
	}
	for _, g := range groups {
		if _, ok := byID[g.ID]; ok {
			out = append(out, g)
		}
	}
	return out
}

// groupModelPairs flattens the group→models map into the (group, model) keys
// the per-group aggregation is seeded with, plus the distinct model names used
// to bound the SQL scan.
func groupModelPairs(groups []*ent.ModelGroup, modelsByGroup map[uuid.UUID][]*ent.Model) ([]groupModelKey, []string) {
	pairs := make([]groupModelKey, 0)
	names := make([]string, 0)
	seenName := make(map[string]struct{})
	for _, g := range groups {
		for _, model := range modelsByGroup[g.ID] {
			pairs = append(pairs, groupModelKey{GroupID: g.ID, Model: model.Name})
			if _, ok := seenName[model.Name]; ok {
				continue
			}
			seenName[model.Name] = struct{}{}
			names = append(names, model.Name)
		}
	}
	return pairs, names
}

// selectedModels resolves which enabled models each group can actually serve.
// Membership on the group is not enough: a customer can only reach a model
// through a provider that lists it, which is the same intersection the admin
// group editor shows as "N 个模型已添加".
func (s *StatusSvc) selectedModels(ctx context.Context, groupIDs []uuid.UUID) (map[uuid.UUID][]*ent.Model, error) {
	return selectedGroupModels(ctx, s.d.Ent, groupIDs)
}

// selectedGroupModels is the client-parameterized body of selectedModels,
// shared with the admin SLA snapshot (sla.go) which runs outside StatusSvc.
func selectedGroupModels(ctx context.Context, client *ent.Client, groupIDs []uuid.UUID) (map[uuid.UUID][]*ent.Model, error) {
	out := make(map[uuid.UUID][]*ent.Model, len(groupIDs))
	links, err := client.ModelGroupModel.Query().
		Where(entgroupmodel.GroupIDIn(groupIDs...)).
		All(ctx)
	if err != nil {
		return nil, err
	}
	modelIDs := make([]uuid.UUID, 0, len(links))
	seen := make(map[uuid.UUID]struct{}, len(links))
	for _, link := range links {
		if _, ok := seen[link.ModelID]; ok {
			continue
		}
		seen[link.ModelID] = struct{}{}
		modelIDs = append(modelIDs, link.ModelID)
	}
	if len(modelIDs) == 0 {
		return out, nil
	}
	models, err := client.Model.Query().
		Where(entmodel.IDIn(modelIDs...), entmodel.EnabledEQ(true)).
		Order(ent.Asc(entmodel.FieldName)).
		All(ctx)
	if err != nil {
		return nil, err
	}
	byID := make(map[uuid.UUID]*ent.Model, len(models))
	for _, model := range models {
		byID[model.ID] = model
	}
	// Walk the name-ordered model list per group so each group's breakdown
	// keeps a stable alphabetical order regardless of link insertion order.
	linksByGroup := make(map[uuid.UUID]map[uuid.UUID]struct{}, len(groupIDs))
	for _, link := range links {
		if _, ok := byID[link.ModelID]; !ok {
			continue
		}
		if linksByGroup[link.GroupID] == nil {
			linksByGroup[link.GroupID] = make(map[uuid.UUID]struct{})
		}
		linksByGroup[link.GroupID][link.ModelID] = struct{}{}
	}
	for groupID, ids := range linksByGroup {
		for _, model := range models {
			if _, ok := ids[model.ID]; ok {
				out[groupID] = append(out[groupID], model)
			}
		}
	}
	served, err := groupServedModelNames(ctx, client, groupIDs)
	if err != nil {
		return nil, err
	}
	return filterRoutableModels(out, served), nil
}

// groupServedModelNames is the set of model names each group can actually
// route right now: enabled group→account links × live accounts × enabled
// upstream_account_models. Same rule as groupRouteAccounts.
func groupServedModelNames(ctx context.Context, client *ent.Client, groupIDs []uuid.UUID) (map[uuid.UUID]map[string]struct{}, error) {
	return groupModelNames(ctx, client, groupIDs, true)
}

// groupConfiguredModelNames is the set of model names an active account on
// the group is set up to serve, including accounts that are temporarily not
// schedulable. The key picker uses this so a cooldown does not hide the
// models a key is being granted. Disabled, expired, and deleted accounts
// still contribute nothing.
func groupConfiguredModelNames(ctx context.Context, client *ent.Client, groupIDs []uuid.UUID) (map[uuid.UUID]map[string]struct{}, error) {
	return groupModelNames(ctx, client, groupIDs, false)
}

func groupModelNames(ctx context.Context, client *ent.Client, groupIDs []uuid.UUID, liveOnly bool) (map[uuid.UUID]map[string]struct{}, error) {
	out := make(map[uuid.UUID]map[string]struct{}, len(groupIDs))
	if len(groupIDs) == 0 {
		return out, nil
	}
	links, err := client.GroupAccount.Query().
		Where(entgroupaccount.GroupIDIn(groupIDs...), entgroupaccount.EnabledEQ(true)).
		All(ctx)
	if err != nil {
		return nil, err
	}
	accountIDs := make([]uuid.UUID, 0, len(links))
	seen := make(map[uuid.UUID]struct{})
	for _, link := range links {
		if _, ok := seen[link.AccountID]; ok {
			continue
		}
		seen[link.AccountID] = struct{}{}
		accountIDs = append(accountIDs, link.AccountID)
	}
	if len(accountIDs) == 0 {
		return out, nil
	}
	accounts, err := client.UpstreamAccount.Query().
		Where(entaccount.IDIn(accountIDs...), entaccount.DeletedAtIsNil()).
		All(ctx)
	if err != nil {
		return nil, err
	}
	// Status must be active either way. liveOnly also drops accounts that
	// are not schedulable, so a parked account cannot keep a status card's
	// model strip alive. The picker keeps those accounts: the key is
	// granted for longer than a cooldown.
	enabled := make(map[uuid.UUID]struct{}, len(accounts))
	ids := make([]uuid.UUID, 0, len(accounts))
	for _, acct := range accounts {
		if acct.Status != entaccount.StatusActive || (liveOnly && !acct.Schedulable) {
			continue
		}
		enabled[acct.ID] = struct{}{}
		ids = append(ids, acct.ID)
	}
	if len(ids) == 0 {
		return out, nil
	}
	bound, err := client.UpstreamAccountModel.Query().
		Where(entaccountmodel.AccountIDIn(ids...), entaccountmodel.EnabledEQ(true)).
		All(ctx)
	if err != nil {
		return nil, err
	}
	namesByAccount := make(map[uuid.UUID][]string, len(ids))
	for _, model := range bound {
		namesByAccount[model.AccountID] = append(namesByAccount[model.AccountID], model.ModelName)
	}
	for _, link := range links {
		if _, ok := enabled[link.AccountID]; !ok {
			continue
		}
		if out[link.GroupID] == nil {
			out[link.GroupID] = make(map[string]struct{})
		}
		for _, name := range namesByAccount[link.AccountID] {
			out[link.GroupID][name] = struct{}{}
		}
	}
	return out, nil
}

// filterRoutableModels keeps only models a group's providers actually serve.
// A leftover group membership (a model no provider in the group serves) must
// not appear on /status just because the catalog row is still attached.
func filterRoutableModels(modelsByGroup map[uuid.UUID][]*ent.Model, served map[uuid.UUID]map[string]struct{}) map[uuid.UUID][]*ent.Model {
	out := make(map[uuid.UUID][]*ent.Model, len(modelsByGroup))
	for groupID, models := range modelsByGroup {
		names := served[groupID]
		if len(names) == 0 {
			continue
		}
		for _, model := range models {
			if _, ok := names[model.Name]; ok {
				out[groupID] = append(out[groupID], model)
			}
		}
	}
	return out
}

func (s *StatusSvc) queryHistory(ctx context.Context, start, end time.Time, bucketDuration time.Duration, ids []uuid.UUID) (map[uuid.UUID][]StatusBucket, map[uuid.UUID]statusAggregate, error) {
	result := make(map[uuid.UUID][]StatusBucket, len(ids))
	history := make(map[uuid.UUID]statusAggregate, len(ids))
	for _, id := range ids {
		result[id] = emptyStatusBuckets(start, bucketDuration)
		history[id] = statusAggregate{GroupID: id}
	}
	query, args := availabilityGroupQuery("bucket", start, end, bucketDuration, ids)
	rows, err := s.d.DB().QueryContext(ctx, query, args...)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id uuid.UUID
		var idx, good, bad, excluded int
		var latencySum, latencyCount int64
		var lastAny, lastEligible sql.NullTime
		if err := rows.Scan(&id, &idx, &good, &bad, &excluded, &latencySum, &latencyCount, &lastAny, &lastEligible); err != nil {
			return nil, nil, err
		}
		h := history[id]
		h.add(good, bad, excluded, latencySum, int(latencyCount), nullTimePtr(lastAny), nullTimePtr(lastEligible))
		history[id] = h
		if idx >= 0 && idx < statusBucketCount {
			b := result[id][idx]
			b.Good += good
			b.Bad += bad
			b.Excluded += excluded
			b.AvailabilityPct = availabilityPercentage(b.Good, b.Bad)
			b.State = publicState(b.Good, b.Bad)
			result[id][idx] = b
		}
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	return result, history, nil
}

// queryAggregate collapses a range to one row per group. kind selects which
// traffic counts — "aggregate" for the probe-inclusive state window, "window"
// for the business-only headline. See trafficClassPredicate.
func (s *StatusSvc) queryAggregate(ctx context.Context, kind string, start, end time.Time, ids []uuid.UUID) (map[uuid.UUID]statusAggregate, error) {
	result := make(map[uuid.UUID]statusAggregate, len(ids))
	for _, id := range ids {
		result[id] = statusAggregate{GroupID: id}
	}
	query, args := availabilityGroupQuery(kind, start, end, 0, ids)
	rows, err := s.d.DB().QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id uuid.UUID
		var good, bad, excluded int
		var latencySum, latencyCount int64
		var lastAny, lastEligible sql.NullTime
		if err := rows.Scan(&id, &good, &bad, &excluded, &latencySum, &latencyCount, &lastAny, &lastEligible); err != nil {
			return nil, err
		}
		a := result[id]
		a.add(good, bad, excluded, latencySum, int(latencyCount), nullTimePtr(lastAny), nullTimePtr(lastEligible))
		result[id] = a
	}
	return result, rows.Err()
}

func (s *StatusSvc) queryModelHistory(ctx context.Context, start, end time.Time, bucketDuration time.Duration, groupIDs []uuid.UUID, pairs []groupModelKey, names []string) (map[groupModelKey][]StatusBucket, map[groupModelKey]statusAggregate, error) {
	result := make(map[groupModelKey][]StatusBucket, len(pairs))
	history := make(map[groupModelKey]statusAggregate, len(pairs))
	for _, key := range pairs {
		result[key] = emptyStatusBuckets(start, bucketDuration)
		history[key] = statusAggregate{GroupID: key.GroupID}
	}
	if len(names) == 0 {
		return result, history, nil
	}
	query, args := availabilityModelQuery("bucket", start, end, bucketDuration, groupIDs, names)
	rows, err := s.d.DB().QueryContext(ctx, query, args...)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var groupID uuid.UUID
		var name string
		var idx, good, bad, excluded int
		var latencySum, latencyCount int64
		var lastAny, lastEligible sql.NullTime
		if err := rows.Scan(&groupID, &name, &idx, &good, &bad, &excluded, &latencySum, &latencyCount, &lastAny, &lastEligible); err != nil {
			return nil, nil, err
		}
		// The SQL scan bounds group and model independently, so it can return
		// pairs that are not actually linked. Only seeded pairs are kept.
		key := groupModelKey{GroupID: groupID, Model: name}
		if _, ok := history[key]; !ok {
			continue
		}
		h := history[key]
		h.add(good, bad, excluded, latencySum, int(latencyCount), nullTimePtr(lastAny), nullTimePtr(lastEligible))
		history[key] = h
		if idx >= 0 && idx < statusBucketCount {
			b := result[key][idx]
			b.Good += good
			b.Bad += bad
			b.Excluded += excluded
			b.AvailabilityPct = availabilityPercentage(b.Good, b.Bad)
			b.State = publicState(b.Good, b.Bad)
			result[key][idx] = b
		}
	}
	return result, history, rows.Err()
}

// queryModelAggregate is queryAggregate at (group, model) resolution; kind
// carries the same meaning.
func (s *StatusSvc) queryModelAggregate(ctx context.Context, kind string, start, end time.Time, groupIDs []uuid.UUID, pairs []groupModelKey, names []string) (map[groupModelKey]statusAggregate, error) {
	result := make(map[groupModelKey]statusAggregate, len(pairs))
	for _, key := range pairs {
		result[key] = statusAggregate{GroupID: key.GroupID}
	}
	if len(names) == 0 {
		return result, nil
	}
	query, args := availabilityModelQuery(kind, start, end, 0, groupIDs, names)
	rows, err := s.d.DB().QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var groupID uuid.UUID
		var name string
		var good, bad, excluded int
		var latencySum, latencyCount int64
		var lastAny, lastEligible sql.NullTime
		if err := rows.Scan(&groupID, &name, &good, &bad, &excluded, &latencySum, &latencyCount, &lastAny, &lastEligible); err != nil {
			return nil, err
		}
		key := groupModelKey{GroupID: groupID, Model: name}
		if _, ok := result[key]; !ok {
			continue
		}
		a := result[key]
		a.add(good, bad, excluded, latencySum, int(latencyCount), nullTimePtr(lastAny), nullTimePtr(lastEligible))
		result[key] = a
	}
	return result, rows.Err()
}

// trafficClassPredicate splits the readers of this table by what they claim.
//
//   - "window" backs the headline availability percentage, which is a promise
//     about the service customers actually received. Synthetic traffic must
//     never dilute it, so it counts business rows only.
//   - "bucket" (the 24h bar) and "aggregate" (the state pill) answer a
//     different question — "was this reachable" — and probes exist precisely to
//     answer it for models nobody happened to call. Both count probes.
//   - "sla" backs the admin SLA report (sla.go). Like "window" it is a claim
//     about served traffic, so it is business-only — including its buckets,
//     which is where it deliberately differs from the public bar: the admin
//     page publishes raw good/bad/excluded counts and a percentage per cell,
//     and a probe-padded percentage would be a lie the public page never
//     tells (the public bar ships a state verdict, never a number).
//
// The bar and the percentage therefore disagree by design on a quiet model: the
// bar shows coloured cells while the percentage may be null. That is the whole
// point of running a prober without moving the published number.
func trafficClassPredicate(kind string) string {
	if kind == "window" || kind == "sla" {
		return "traffic_class = 'business'"
	}
	return "traffic_class IN ('business','probe')"
}

// bucketedAvailabilityKind reports whether a kind selects the per-bucket query
// shape ("bucket" for the public bar, "sla" for the admin SLA grid) rather
// than the single-row aggregate shape.
func bucketedAvailabilityKind(kind string) bool {
	return kind == "bucket" || kind == "sla"
}

func availabilityGroupQuery(kind string, start, end time.Time, bucketDuration time.Duration, ids []uuid.UUID) (string, []any) {
	pl := make([]string, 0, len(ids))
	args := []any{start, end}
	placeholderOffset := 3
	if bucketedAvailabilityKind(kind) {
		placeholderOffset = 4
	}
	for i, id := range ids {
		pl = append(pl, fmt.Sprintf("$%d", i+placeholderOffset))
		args = append(args, id)
	}
	where := fmt.Sprintf("%s AND started_at >= $1 AND started_at < $2 AND group_id IN (%s)", trafficClassPredicate(kind), strings.Join(pl, ","))
	if bucketedAvailabilityKind(kind) {
		args = append([]any{start, end, bucketDuration.Seconds()}, args[2:]...)
		return fmt.Sprintf(`SELECT group_id,
  floor(extract(epoch FROM (started_at - $1)) / $3)::int AS bucket_index,
  count(*) FILTER (WHERE outcome = 'good')::int,
  count(*) FILTER (WHERE outcome = 'bad')::int,
  count(*) FILTER (WHERE outcome = 'excluded')::int,
  coalesce(sum(latency_ms) FILTER (WHERE outcome IN ('good','bad')), 0)::bigint,
  count(latency_ms) FILTER (WHERE outcome IN ('good','bad') AND latency_ms > 0)::bigint,
  max(started_at),
  max(started_at) FILTER (WHERE outcome IN ('good','bad'))
FROM availability_observations WHERE %s
GROUP BY group_id, bucket_index ORDER BY group_id, bucket_index`, where), args
	}
	return fmt.Sprintf(`SELECT group_id,
  count(*) FILTER (WHERE outcome = 'good')::int,
  count(*) FILTER (WHERE outcome = 'bad')::int,
  count(*) FILTER (WHERE outcome = 'excluded')::int,
  coalesce(sum(latency_ms) FILTER (WHERE outcome IN ('good','bad')), 0)::bigint,
  count(latency_ms) FILTER (WHERE outcome IN ('good','bad') AND latency_ms > 0)::bigint,
  max(started_at),
  max(started_at) FILTER (WHERE outcome IN ('good','bad'))
FROM availability_observations WHERE %s GROUP BY group_id`, where), args
}

func availabilityModelQuery(kind string, start, end time.Time, bucketDuration time.Duration, groupIDs []uuid.UUID, names []string) (string, []any) {
	placeholderOffset := 3
	args := []any{start, end}
	if bucketedAvailabilityKind(kind) {
		placeholderOffset = 4
		args = append(args, bucketDuration.Seconds())
	}
	groupPlaceholders := make([]string, 0, len(groupIDs))
	for i, id := range groupIDs {
		placeholder := i + placeholderOffset
		groupPlaceholders = append(groupPlaceholders, fmt.Sprintf("$%d", placeholder))
		args = append(args, id)
	}
	modelOffset := placeholderOffset + len(groupIDs)
	modelPlaceholders := make([]string, 0, len(names))
	for i, name := range names {
		placeholder := i + modelOffset
		modelPlaceholders = append(modelPlaceholders, fmt.Sprintf("$%d", placeholder))
		args = append(args, name)
	}
	where := fmt.Sprintf("%s AND started_at >= $1 AND started_at < $2 AND group_id IN (%s) AND model IN (%s)", trafficClassPredicate(kind), strings.Join(groupPlaceholders, ","), strings.Join(modelPlaceholders, ","))
	// Grouping by (group_id, model) keeps each group's providers separate. The
	// same model name behind two groups is two different upstream fleets, so
	// pooling them would hide a group that is actually down.
	if bucketedAvailabilityKind(kind) {
		return fmt.Sprintf(`SELECT group_id, model,
  floor(extract(epoch FROM (started_at - $1)) / $3)::int AS bucket_index,
  count(*) FILTER (WHERE outcome = 'good')::int,
  count(*) FILTER (WHERE outcome = 'bad')::int,
  count(*) FILTER (WHERE outcome = 'excluded')::int,
  coalesce(sum(latency_ms) FILTER (WHERE outcome IN ('good','bad')), 0)::bigint,
  count(latency_ms) FILTER (WHERE outcome IN ('good','bad') AND latency_ms > 0)::bigint,
  max(started_at),
  max(started_at) FILTER (WHERE outcome IN ('good','bad'))
FROM availability_observations WHERE %s
GROUP BY group_id, model, bucket_index ORDER BY group_id, model, bucket_index`, where), args
	}
	return fmt.Sprintf(`SELECT group_id, model,
  count(*) FILTER (WHERE outcome = 'good')::int,
  count(*) FILTER (WHERE outcome = 'bad')::int,
  count(*) FILTER (WHERE outcome = 'excluded')::int,
  coalesce(sum(latency_ms) FILTER (WHERE outcome IN ('good','bad')), 0)::bigint,
  count(latency_ms) FILTER (WHERE outcome IN ('good','bad') AND latency_ms > 0)::bigint,
  max(started_at),
  max(started_at) FILTER (WHERE outcome IN ('good','bad'))
FROM availability_observations WHERE %s GROUP BY group_id, model`, where), args
}

func nullTimePtr(v sql.NullTime) *time.Time {
	if !v.Valid {
		return nil
	}
	t := v.Time.UTC()
	return &t
}
