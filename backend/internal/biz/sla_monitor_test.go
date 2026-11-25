package biz

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
)

type fakeSLAIncident struct {
	slaStoredIncident
	openedAt            time.Time
	openAvailabilityPct *float64
	resolvedAt          *time.Time
	resolution          string
}

type fakeSLAStore struct {
	rows             map[uuid.UUID]*fakeSLAIncident
	openByGroupModel map[string]uuid.UUID
	createAttempts   int
}

func newFakeSLAStore() *fakeSLAStore {
	return &fakeSLAStore{
		rows:             make(map[uuid.UUID]*fakeSLAIncident),
		openByGroupModel: make(map[string]uuid.UUID),
	}
}

func (f *fakeSLAStore) listOpenForSLA(context.Context) ([]slaStoredIncident, error) {
	out := make([]slaStoredIncident, 0)
	for _, row := range f.rows {
		if row.resolvedAt == nil {
			out = append(out, row.slaStoredIncident)
		}
	}
	return out, nil
}

func (f *fakeSLAStore) createOpenForSLA(_ context.Context, in slaOpenIncident) error {
	f.createAttempts++
	key := in.GroupID.String() + "\x00" + in.Model
	if _, exists := f.openByGroupModel[key]; exists {
		return nil
	}
	id := uuid.New()
	f.rows[id] = &fakeSLAIncident{
		slaStoredIncident: slaStoredIncident{
			ID:                       id,
			GroupID:                  in.GroupID,
			Model:                    in.Model,
			Severity:                 in.Severity,
			WorstSeverity:            in.Severity,
			WorstAvailabilityPct:     cloneFloat(in.AvailabilityPct),
			LastAvailabilityPct:      cloneFloat(in.AvailabilityPct),
			MaximumWindowSampleCount: in.SampleCount,
		},
		openedAt:            in.DetectedAt,
		openAvailabilityPct: cloneFloat(in.AvailabilityPct),
	}
	f.openByGroupModel[key] = id
	return nil
}

func (f *fakeSLAStore) updateOpenForSLA(_ context.Context, in slaIncidentUpdate) error {
	row := f.rows[in.ID]
	if row == nil || row.resolvedAt != nil {
		return nil
	}
	if in.CurrentSeverity != "" {
		row.Severity = in.CurrentSeverity
	}
	row.WorstSeverity = in.WorstSeverity
	row.WorstAvailabilityPct = cloneFloat(in.WorstAvailabilityPct)
	row.LastAvailabilityPct = cloneFloat(in.LastAvailabilityPct)
	row.MaximumWindowSampleCount = in.MaximumWindowSampleCount
	return nil
}

func (f *fakeSLAStore) resolveForSLA(_ context.Context, id uuid.UUID, at time.Time, resolution string, worst StatusState) error {
	row := f.rows[id]
	if row == nil || row.resolvedAt != nil {
		return nil
	}
	resolvedAt := at
	row.resolvedAt = &resolvedAt
	row.resolution = resolution
	row.Severity = worst
	delete(f.openByGroupModel, row.GroupID.String()+"\x00"+row.Model)
	return nil
}

func (f *fakeSLAStore) only(t *testing.T) *fakeSLAIncident {
	t.Helper()
	if len(f.rows) != 1 {
		t.Fatalf("incident count = %d, want 1", len(f.rows))
	}
	for _, row := range f.rows {
		return row
	}
	return nil
}

func newTestSLAMonitor(store slaIncidentStore) *SLAMonitorSvc {
	return &SLAMonitorSvc{
		store:   store,
		now:     time.Now,
		streaks: make(map[uuid.UUID]*slaMonitorStreak),
	}
}

func reconcileSLA(t *testing.T, monitor *SLAMonitorSvc, groups []slaMonitorGroup, aggregates map[uuid.UUID]slaMonitorAggregate, tick int) {
	t.Helper()
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC).Add(time.Duration(tick) * time.Minute)
	if err := monitor.reconcile(context.Background(), groups, aggregates, now); err != nil {
		t.Fatalf("reconcile tick %d: %v", tick, err)
	}
}

func TestSLAMonitorOpensOnThirdConsecutiveTick(t *testing.T) {
	store := newFakeSLAStore()
	monitor := newTestSLAMonitor(store)
	group := slaMonitorGroup{ID: uuid.New(), Name: "primary"}
	bad := map[uuid.UUID]slaMonitorAggregate{group.ID: {Good: 90, Bad: 10}}

	reconcileSLA(t, monitor, []slaMonitorGroup{group}, bad, 1)
	reconcileSLA(t, monitor, []slaMonitorGroup{group}, bad, 2)
	if len(store.rows) != 0 {
		t.Fatal("incident opened before the third consecutive tick")
	}
	reconcileSLA(t, monitor, []slaMonitorGroup{group}, bad, 3)
	row := store.only(t)
	if row.Severity != StatusDegraded {
		t.Fatalf("severity = %q, want degraded", row.Severity)
	}
}

func TestSLAMonitorSampleFloorKeepsThinFailureClosed(t *testing.T) {
	store := newFakeSLAStore()
	monitor := newTestSLAMonitor(store)
	group := slaMonitorGroup{ID: uuid.New(), Name: "quiet"}
	thin := map[uuid.UUID]slaMonitorAggregate{group.ID: {Good: 1, Bad: 1}}

	for tick := 1; tick <= 6; tick++ {
		reconcileSLA(t, monitor, []slaMonitorGroup{group}, thin, tick)
	}
	if len(store.rows) != 0 {
		t.Fatal("thin traffic opened an incident")
	}
}

func TestSLAMonitorRecoveryNeedsFiveQualifiedHealthyTicks(t *testing.T) {
	store := newFakeSLAStore()
	monitor := newTestSLAMonitor(store)
	group := slaMonitorGroup{ID: uuid.New(), Name: "recovery"}
	failing := map[uuid.UUID]slaMonitorAggregate{group.ID: {Good: 80, Bad: 20}}
	healthy := map[uuid.UUID]slaMonitorAggregate{group.ID: {Good: 100}}
	quietHealthy := map[uuid.UUID]slaMonitorAggregate{group.ID: {Good: 1}}

	for tick := 1; tick <= slaOpenConsecutive; tick++ {
		reconcileSLA(t, monitor, []slaMonitorGroup{group}, failing, tick)
	}
	row := store.only(t)
	for tick := 4; tick <= 7; tick++ {
		reconcileSLA(t, monitor, []slaMonitorGroup{group}, healthy, tick)
	}
	if row.resolvedAt != nil {
		t.Fatal("incident resolved before five healthy ticks")
	}
	reconcileSLA(t, monitor, []slaMonitorGroup{group}, quietHealthy, 8)
	if row.resolvedAt != nil {
		t.Fatal("quiet tick counted as recovery")
	}
	reconcileSLA(t, monitor, []slaMonitorGroup{group}, healthy, 9)
	if row.resolvedAt == nil || row.resolution != "auto_recovered" {
		t.Fatalf("resolution = %q at %v, want auto_recovered", row.resolution, row.resolvedAt)
	}
}

func TestSLAMonitorFlapKeepsOneIncident(t *testing.T) {
	store := newFakeSLAStore()
	monitor := newTestSLAMonitor(store)
	group := slaMonitorGroup{ID: uuid.New(), Name: "flappy"}
	failing := map[uuid.UUID]slaMonitorAggregate{group.ID: {Good: 90, Bad: 10}}
	healthy := map[uuid.UUID]slaMonitorAggregate{group.ID: {Good: 100}}

	for tick := 1; tick <= 3; tick++ {
		reconcileSLA(t, monitor, []slaMonitorGroup{group}, failing, tick)
	}
	for tick := 4; tick <= 7; tick++ {
		reconcileSLA(t, monitor, []slaMonitorGroup{group}, healthy, tick)
	}
	reconcileSLA(t, monitor, []slaMonitorGroup{group}, failing, 8)
	for tick := 9; tick <= 13; tick++ {
		reconcileSLA(t, monitor, []slaMonitorGroup{group}, healthy, tick)
	}
	row := store.only(t)
	if row.resolution != "auto_recovered" {
		t.Fatalf("resolution = %q, want auto_recovered", row.resolution)
	}
}

func TestSLAMonitorWorstFieldsAreMonotone(t *testing.T) {
	store := newFakeSLAStore()
	monitor := newTestSLAMonitor(store)
	group := slaMonitorGroup{ID: uuid.New(), Name: "changing"}
	degraded := map[uuid.UUID]slaMonitorAggregate{group.ID: {Good: 95, Bad: 5}}
	down := map[uuid.UUID]slaMonitorAggregate{group.ID: {Good: 10, Bad: 10}}
	serious := map[uuid.UUID]slaMonitorAggregate{group.ID: {Good: 85, Bad: 15}}

	for tick := 1; tick <= 3; tick++ {
		reconcileSLA(t, monitor, []slaMonitorGroup{group}, degraded, tick)
	}
	reconcileSLA(t, monitor, []slaMonitorGroup{group}, down, 4)
	reconcileSLA(t, monitor, []slaMonitorGroup{group}, serious, 5)
	row := store.only(t)
	if row.Severity != StatusSerious {
		t.Fatalf("current severity = %q, want serious", row.Severity)
	}
	if row.WorstSeverity != StatusDown {
		t.Fatalf("worst severity = %q, want down", row.WorstSeverity)
	}
	if row.WorstAvailabilityPct == nil || *row.WorstAvailabilityPct != 50 {
		t.Fatalf("worst availability = %v, want 50", row.WorstAvailabilityPct)
	}
	if row.LastAvailabilityPct == nil || *row.LastAvailabilityPct != 85 {
		t.Fatalf("last availability = %v, want 85", row.LastAvailabilityPct)
	}
	if row.MaximumWindowSampleCount != 100 {
		t.Fatalf("sample count = %d, want monotone maximum 100", row.MaximumWindowSampleCount)
	}
}

func TestSLAMonitorDuplicateOpenIsBlocked(t *testing.T) {
	store := newFakeSLAStore()
	group := slaMonitorGroup{ID: uuid.New(), Name: "race"}
	open := slaOpenIncident{
		GroupID:         group.ID,
		GroupName:       group.Name,
		Severity:        StatusDegraded,
		StartedAt:       time.Now(),
		DetectedAt:      time.Now(),
		AvailabilityPct: pct(90),
		SampleCount:     10,
	}
	for attempt := 0; attempt < 2; attempt++ {
		if err := store.createOpenForSLA(context.Background(), open); err != nil {
			t.Fatalf("create attempt %d: %v", attempt+1, err)
		}
	}
	store.only(t)
	if store.createAttempts != 2 {
		t.Fatalf("create attempts = %d, want both monitors to attempt once", store.createAttempts)
	}
}

func TestSLAMonitorStaleClosesRemovedGroup(t *testing.T) {
	store := newFakeSLAStore()
	monitor := newTestSLAMonitor(store)
	group := slaMonitorGroup{ID: uuid.New(), Name: "removed"}
	failing := map[uuid.UUID]slaMonitorAggregate{group.ID: {Good: 80, Bad: 20}}

	for tick := 1; tick <= 3; tick++ {
		reconcileSLA(t, monitor, []slaMonitorGroup{group}, failing, tick)
	}
	row := store.only(t)
	reconcileSLA(t, monitor, nil, nil, 4)
	if row.resolvedAt == nil || row.resolution != "stale_closed" {
		t.Fatalf("resolution = %q at %v, want stale_closed", row.resolution, row.resolvedAt)
	}
}
