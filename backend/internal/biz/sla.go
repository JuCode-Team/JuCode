// sla.go: the admin-facing SLA report behind GET /v1/admin/observability/sla.
//
// It is the public status page's privileged sibling: same source table
// (availability_observations), same group scope and display order
// (SiteSettings.StatusPageGroupIDs), same state bands (publicState) and the
// same two-decimal percentage (availabilityPercentage) — but business traffic
// only, everywhere. The public page pads its bar and state pill with probe
// traffic so a quiet model still reports reachability; the admin report
// publishes raw good/bad/excluded counts per cell, and probe-padded counts
// would misstate what customers actually experienced. See
// trafficClassPredicate("sla") in status.go.
//
// Implemented as a package-level function rather than a StatusSvc method
// because the admin handler holds an *ent.Client, a raw *sql.DB and a
// SettingsSvc but no StatusSvc — and admin.New must not grow another
// constructor argument.
package biz

import (
	"context"
	"database/sql"
	"time"

	"github.com/google/uuid"

	"github.com/jucode/jucode/internal/data/ent"
	entgroup "github.com/jucode/jucode/internal/data/ent/modelgroup"
)

// SLAThresholds echoes the publicState bands so the frontend colours the
// admin report with the exact numbers the verdicts were graded against.
type SLAThresholds struct {
	Healthy  float64 `json:"healthy"`
	Degraded float64 `json:"degraded"`
	Serious  float64 `json:"serious"`
}

// AdminSLABucket is one cell of the admin grid. Unlike the public
// StatusBucket the evidence ships alongside the verdict — counts are an
// admin privilege.
type AdminSLABucket struct {
	Time            time.Time   `json:"time"`
	State           StatusState `json:"state"`
	AvailabilityPct *float64    `json:"availability_pct"`
	Good            int         `json:"good"`
	Bad             int         `json:"bad"`
	Excluded        int         `json:"excluded"`
}

// AdminSLAModel is the per-(group, model) breakdown. OpenIncidentID is
// reserved for the incidents subsystem and stays null until that ships;
// the field is emitted now so the frontend contract doesn't change later.
type AdminSLAModel struct {
	Model           string           `json:"model"`
	State           StatusState      `json:"state"`
	AvailabilityPct *float64         `json:"availability_pct"`
	Good            int              `json:"good"`
	Bad             int              `json:"bad"`
	Excluded        int              `json:"excluded"`
	AvgLatencyMs    *int             `json:"avg_latency_ms"`
	OpenIncidentID  *string          `json:"open_incident_id"`
	Buckets         []AdminSLABucket `json:"buckets"`
}

type AdminSLAGroup struct {
	GroupID         string           `json:"group_id"`
	Name            string           `json:"name"`
	State           StatusState      `json:"state"`
	AvailabilityPct *float64         `json:"availability_pct"`
	Good            int              `json:"good"`
	Bad             int              `json:"bad"`
	Excluded        int              `json:"excluded"`
	AvgLatencyMs    *int             `json:"avg_latency_ms"`
	OpenIncidentID  *string          `json:"open_incident_id"`
	Buckets         []AdminSLABucket `json:"buckets"`
	Models          []AdminSLAModel  `json:"models"`
}

type AdminSLA struct {
	WindowHours int             `json:"window_hours"`
	UpdatedAt   time.Time       `json:"updated_at"`
	Thresholds  SLAThresholds   `json:"thresholds"`
	Groups      []AdminSLAGroup `json:"groups"`
}

// slaAccumulator folds bucket-scan rows into whole-window totals, the way
// queryHistory folds them for the public page. Business-only everywhere
// means the totals really are the sum of the buckets, so one scan per
// resolution answers both the grid and the headline.
type slaAccumulator struct {
	Good, Bad, Excluded int
	LatencySum          int64
	LatencyCount        int64
	Buckets             []AdminSLABucket
}

func newSLAAccumulator(start time.Time, bucketDuration time.Duration) *slaAccumulator {
	buckets := make([]AdminSLABucket, statusBucketCount)
	for i := range buckets {
		buckets[i].Time = start.Add(time.Duration(i) * bucketDuration)
		buckets[i].State = StatusUnknown
	}
	return &slaAccumulator{Buckets: buckets}
}

func (a *slaAccumulator) add(idx, good, bad, excluded int, latencySum, latencyCount int64) {
	a.Good += good
	a.Bad += bad
	a.Excluded += excluded
	a.LatencySum += latencySum
	a.LatencyCount += latencyCount
	if idx >= 0 && idx < statusBucketCount {
		b := &a.Buckets[idx]
		b.Good += good
		b.Bad += bad
		b.Excluded += excluded
		b.AvailabilityPct = availabilityPercentage(b.Good, b.Bad)
		b.State = publicState(b.Good, b.Bad)
	}
}

func (a *slaAccumulator) avgLatencyMs() *int {
	if a.LatencyCount == 0 {
		return nil
	}
	avg := int(a.LatencySum / a.LatencyCount)
	return &avg
}

// AdminSLASnapshot computes the report for the given window (whole hours;
// the handler maps 24h/7d/30d onto 24/168/720). groupID, when non-nil,
// narrows the configured scope to that one group — an ID outside the scope
// yields an empty group list rather than an error, mirroring how the
// series endpoint answers unknown-but-valid IDs with zeros.
func AdminSLASnapshot(ctx context.Context, client *ent.Client, db *sql.DB, settings *SettingsSvc, windowHours int, groupID *uuid.UUID) (*AdminSLA, error) {
	cfg, err := settings.GetSite(ctx)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	out := &AdminSLA{
		WindowHours: windowHours,
		UpdatedAt:   now,
		Thresholds: SLAThresholds{
			Healthy:  statusHealthyThreshold,
			Degraded: statusDegradedThreshold,
			Serious:  statusSeriousThreshold,
		},
		Groups: []AdminSLAGroup{},
	}
	// Scope and order come from the same admin-arranged list the public page
	// renders, so the two views always show the same groups in the same order.
	scope := cfg.StatusPageGroupIDs
	if groupID != nil {
		narrowed := make([]uuid.UUID, 0, 1)
		for _, id := range scope {
			if id == *groupID {
				narrowed = append(narrowed, id)
			}
		}
		scope = narrowed
	}
	if len(scope) == 0 {
		return out, nil
	}
	groups, err := client.ModelGroup.Query().
		Where(entgroup.IDIn(scope...), entgroup.EnabledEQ(true)).
		All(ctx)
	if err != nil {
		return nil, err
	}
	if len(groups) == 0 {
		return out, nil
	}
	groups = orderGroupsByConfig(groups, scope)

	// Same grid construction as the public Snapshot: 48 buckets aligned to
	// UTC boundaries, query range strictly [start, now) so clock-skewed rows
	// never land in the trailing (in-progress) bucket.
	window := time.Duration(windowHours) * time.Hour
	bucketDuration := window / statusBucketCount
	if bucketDuration <= 0 {
		bucketDuration = time.Minute
	}
	bucketEnd := now.Truncate(bucketDuration).Add(bucketDuration)
	windowStart := bucketEnd.Add(-bucketDuration * statusBucketCount)

	ids := make([]uuid.UUID, 0, len(groups))
	for _, g := range groups {
		ids = append(ids, g.ID)
	}
	modelsByGroup, err := selectedGroupModels(ctx, client, ids)
	if err != nil {
		return nil, err
	}

	groupAgg, err := querySLAGroups(ctx, db, windowStart, now, bucketDuration, ids)
	if err != nil {
		return nil, err
	}
	pairs, modelNames := groupModelPairs(groups, modelsByGroup)
	modelAgg, err := querySLAModels(ctx, db, windowStart, now, bucketDuration, ids, pairs, modelNames)
	if err != nil {
		return nil, err
	}

	for _, g := range groups {
		agg := groupAgg[g.ID]
		if agg == nil {
			agg = newSLAAccumulator(windowStart, bucketDuration)
		}
		card := AdminSLAGroup{
			GroupID:         g.ID.String(),
			Name:            g.Name,
			State:           publicState(agg.Good, agg.Bad),
			AvailabilityPct: availabilityPercentage(agg.Good, agg.Bad),
			Good:            agg.Good,
			Bad:             agg.Bad,
			Excluded:        agg.Excluded,
			AvgLatencyMs:    agg.avgLatencyMs(),
			Buckets:         agg.Buckets,
			Models:          []AdminSLAModel{},
		}
		for _, model := range modelsByGroup[g.ID] {
			mAgg := modelAgg[groupModelKey{GroupID: g.ID, Model: model.Name}]
			if mAgg == nil {
				mAgg = newSLAAccumulator(windowStart, bucketDuration)
			}
			card.Models = append(card.Models, AdminSLAModel{
				Model:           model.Name,
				State:           publicState(mAgg.Good, mAgg.Bad),
				AvailabilityPct: availabilityPercentage(mAgg.Good, mAgg.Bad),
				Good:            mAgg.Good,
				Bad:             mAgg.Bad,
				Excluded:        mAgg.Excluded,
				AvgLatencyMs:    mAgg.avgLatencyMs(),
				Buckets:         mAgg.Buckets,
			})
		}
		out.Groups = append(out.Groups, card)
	}
	return out, nil
}

func querySLAGroups(ctx context.Context, db *sql.DB, start, end time.Time, bucketDuration time.Duration, ids []uuid.UUID) (map[uuid.UUID]*slaAccumulator, error) {
	result := make(map[uuid.UUID]*slaAccumulator, len(ids))
	for _, id := range ids {
		result[id] = newSLAAccumulator(start, bucketDuration)
	}
	query, args := availabilityGroupQuery("sla", start, end, bucketDuration, ids)
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id uuid.UUID
		var idx, good, bad, excluded int
		var latencySum, latencyCount int64
		var lastAny, lastEligible sql.NullTime
		if err := rows.Scan(&id, &idx, &good, &bad, &excluded, &latencySum, &latencyCount, &lastAny, &lastEligible); err != nil {
			return nil, err
		}
		if a, ok := result[id]; ok {
			a.add(idx, good, bad, excluded, latencySum, latencyCount)
		}
	}
	return result, rows.Err()
}

func querySLAModels(ctx context.Context, db *sql.DB, start, end time.Time, bucketDuration time.Duration, groupIDs []uuid.UUID, pairs []groupModelKey, names []string) (map[groupModelKey]*slaAccumulator, error) {
	result := make(map[groupModelKey]*slaAccumulator, len(pairs))
	for _, key := range pairs {
		result[key] = newSLAAccumulator(start, bucketDuration)
	}
	if len(names) == 0 {
		return result, nil
	}
	query, args := availabilityModelQuery("sla", start, end, bucketDuration, groupIDs, names)
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var groupID uuid.UUID
		var name string
		var idx, good, bad, excluded int
		var latencySum, latencyCount int64
		var lastAny, lastEligible sql.NullTime
		if err := rows.Scan(&groupID, &name, &idx, &good, &bad, &excluded, &latencySum, &latencyCount, &lastAny, &lastEligible); err != nil {
			return nil, err
		}
		// Bounded independently on group and model, so unlinked pairs can come
		// back — only seeded pairs are kept, same as the public page.
		if a, ok := result[groupModelKey{GroupID: groupID, Model: name}]; ok {
			a.add(idx, good, bad, excluded, latencySum, latencyCount)
		}
	}
	return result, rows.Err()
}
