package biz

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/jucode/jucode/internal/data"
	entgroup "github.com/jucode/jucode/internal/data/ent/modelgroup"
)

const (
	slaOpenConsecutive  = 3
	slaCloseConsecutive = 5
	slaOpenMinSamples   = 10
	slaOpenMinFailures  = 2
	slaStateWindow      = 30 * time.Minute
)

type slaIncidentStore interface {
	listOpenForSLA(context.Context) ([]slaStoredIncident, error)
	createOpenForSLA(context.Context, slaOpenIncident) error
	updateOpenForSLA(context.Context, slaIncidentUpdate) error
	resolveForSLA(context.Context, uuid.UUID, time.Time, string, StatusState) error
}

type slaStoredIncident struct {
	ID                       uuid.UUID
	GroupID                  uuid.UUID
	Model                    string
	Severity                 StatusState
	WorstSeverity            StatusState
	WorstAvailabilityPct     *float64
	LastAvailabilityPct      *float64
	MaximumWindowSampleCount int
}

type slaOpenIncident struct {
	GroupID         uuid.UUID
	GroupName       string
	Model           string
	Severity        StatusState
	StartedAt       time.Time
	DetectedAt      time.Time
	AvailabilityPct *float64
	SampleCount     int
}

type slaIncidentUpdate struct {
	ID                       uuid.UUID
	CurrentSeverity          StatusState
	WorstSeverity            StatusState
	WorstAvailabilityPct     *float64
	LastAvailabilityPct      *float64
	MaximumWindowSampleCount int
}

type slaMonitorGroup struct {
	ID   uuid.UUID
	Name string
}

type slaMonitorAggregate struct {
	Good int
	Bad  int
}

func (a slaMonitorAggregate) sampleCount() int { return a.Good + a.Bad }

type slaMonitorStreak struct {
	unhealthy int
	healthy   int
}

// SLAMonitorSvc evaluates business traffic only and persists group-level SLA
// incident lifecycles. StartLoop must run under the process leader callback.
type SLAMonitorSvc struct {
	d        *data.Data
	settings *SettingsSvc
	store    slaIncidentStore
	logger   *slog.Logger
	now      func() time.Time
	streaks  map[uuid.UUID]*slaMonitorStreak
}

func NewSLAMonitorSvc(d *data.Data, settings *SettingsSvc, incidents *SystemIncidentSvc, logger *slog.Logger) *SLAMonitorSvc {
	if logger == nil {
		logger = slog.Default()
	}
	return &SLAMonitorSvc{
		d:        d,
		settings: settings,
		store:    incidents,
		logger:   logger,
		now:      time.Now,
		streaks:  make(map[uuid.UUID]*slaMonitorStreak),
	}
}

func (s *SLAMonitorSvc) StartLoop(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = time.Minute
	}
	s.tickAndLog(ctx)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.tickAndLog(ctx)
		}
	}
}

func (s *SLAMonitorSvc) tickAndLog(ctx context.Context) {
	if err := s.Tick(ctx); err != nil && ctx.Err() == nil {
		s.logger.Warn("sla monitor tick failed", "err", err.Error())
	}
}

func (s *SLAMonitorSvc) Tick(ctx context.Context) error {
	now := s.now().UTC()
	groups, err := s.statusPageGroups(ctx)
	if err != nil {
		return err
	}
	aggregates, err := s.queryAggregates(ctx, now.Add(-slaStateWindow), now, groups)
	if err != nil {
		return err
	}
	return s.reconcile(ctx, groups, aggregates, now)
}

func (s *SLAMonitorSvc) statusPageGroups(ctx context.Context) ([]slaMonitorGroup, error) {
	cfg, err := s.settings.GetSite(ctx)
	if err != nil {
		return nil, err
	}
	if len(cfg.StatusPageGroupIDs) == 0 {
		return nil, nil
	}
	rows, err := s.d.Ent.ModelGroup.Query().
		Where(entgroup.IDIn(cfg.StatusPageGroupIDs...), entgroup.EnabledEQ(true)).
		All(ctx)
	if err != nil {
		return nil, err
	}
	rows = orderGroupsByConfig(rows, cfg.StatusPageGroupIDs)
	out := make([]slaMonitorGroup, 0, len(rows))
	for _, row := range rows {
		out = append(out, slaMonitorGroup{ID: row.ID, Name: row.Name})
	}
	return out, nil
}

func (s *SLAMonitorSvc) queryAggregates(ctx context.Context, start, end time.Time, groups []slaMonitorGroup) (map[uuid.UUID]slaMonitorAggregate, error) {
	out := make(map[uuid.UUID]slaMonitorAggregate, len(groups))
	if len(groups) == 0 {
		return out, nil
	}
	placeholders := make([]string, 0, len(groups))
	args := make([]any, 0, len(groups)+2)
	args = append(args, start, end)
	for i, group := range groups {
		placeholders = append(placeholders, fmt.Sprintf("$%d", i+3))
		args = append(args, group.ID)
		out[group.ID] = slaMonitorAggregate{}
	}
	query := fmt.Sprintf(`SELECT group_id,
  count(*) FILTER (WHERE outcome = 'good')::int,
  count(*) FILTER (WHERE outcome = 'bad')::int
FROM availability_observations
WHERE traffic_class = 'business'
  AND started_at >= $1
  AND started_at < $2
  AND group_id IN (%s)
GROUP BY group_id`, strings.Join(placeholders, ","))
	rows, err := s.d.DB().QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id uuid.UUID
		var aggregate slaMonitorAggregate
		if err := rows.Scan(&id, &aggregate.Good, &aggregate.Bad); err != nil {
			return nil, err
		}
		out[id] = aggregate
	}
	return out, rows.Err()
}

func (s *SLAMonitorSvc) reconcile(ctx context.Context, groups []slaMonitorGroup, aggregates map[uuid.UUID]slaMonitorAggregate, now time.Time) error {
	openRows, err := s.store.listOpenForSLA(ctx)
	if err != nil {
		return err
	}
	active := make(map[uuid.UUID]slaMonitorGroup, len(groups))
	for _, group := range groups {
		active[group.ID] = group
	}
	open := make(map[uuid.UUID]slaStoredIncident, len(openRows))
	for _, incident := range openRows {
		if _, ok := active[incident.GroupID]; !ok {
			if err := s.store.resolveForSLA(ctx, incident.ID, now, "stale_closed", incident.WorstSeverity); err != nil {
				return err
			}
			delete(s.streaks, incident.GroupID)
			continue
		}
		open[incident.GroupID] = incident
	}

	for _, group := range groups {
		aggregate := aggregates[group.ID]
		samples := aggregate.sampleCount()
		state := publicState(aggregate.Good, aggregate.Bad)
		pct := availabilityPercentage(aggregate.Good, aggregate.Bad)
		streak := s.streaks[group.ID]
		if streak == nil {
			streak = &slaMonitorStreak{}
			s.streaks[group.ID] = streak
		}

		if incident, ok := open[group.ID]; ok {
			update := updateForObservation(incident, state, pct, samples)
			if samples > 0 {
				if err := s.store.updateOpenForSLA(ctx, update); err != nil {
					return err
				}
			}
			switch {
			case state == StatusHealthy && samples >= slaOpenMinSamples:
				streak.healthy++
			case state != StatusUnknown && samples >= slaOpenMinSamples:
				streak.healthy = 0
			}
			if streak.healthy >= slaCloseConsecutive {
				if err := s.store.resolveForSLA(ctx, incident.ID, now, "auto_recovered", update.WorstSeverity); err != nil {
					return err
				}
				streak.healthy = 0
			}
			streak.unhealthy = 0
			continue
		}

		confidentFailure := state != StatusHealthy &&
			state != StatusUnknown &&
			samples >= slaOpenMinSamples &&
			aggregate.Bad >= slaOpenMinFailures
		if !confidentFailure {
			streak.unhealthy = 0
			streak.healthy = 0
			continue
		}
		streak.unhealthy++
		if streak.unhealthy < slaOpenConsecutive {
			continue
		}
		if err := s.store.createOpenForSLA(ctx, slaOpenIncident{
			GroupID:         group.ID,
			GroupName:       truncateRunes(group.Name, 256),
			Model:           "",
			Severity:        state,
			StartedAt:       now,
			DetectedAt:      now,
			AvailabilityPct: pct,
			SampleCount:     samples,
		}); err != nil {
			return err
		}
		streak.unhealthy = 0
	}
	return nil
}

func updateForObservation(incident slaStoredIncident, state StatusState, pct *float64, samples int) slaIncidentUpdate {
	out := slaIncidentUpdate{
		ID:                       incident.ID,
		WorstSeverity:            incident.WorstSeverity,
		WorstAvailabilityPct:     cloneFloat(incident.WorstAvailabilityPct),
		LastAvailabilityPct:      cloneFloat(incident.LastAvailabilityPct),
		MaximumWindowSampleCount: incident.MaximumWindowSampleCount,
	}
	if samples > out.MaximumWindowSampleCount {
		out.MaximumWindowSampleCount = samples
	}
	if pct != nil {
		out.LastAvailabilityPct = cloneFloat(pct)
		if out.WorstAvailabilityPct == nil || *pct < *out.WorstAvailabilityPct {
			out.WorstAvailabilityPct = cloneFloat(pct)
		}
	}
	if isIncidentState(state) {
		out.CurrentSeverity = state
		if severityRank(state) > severityRank(out.WorstSeverity) {
			out.WorstSeverity = state
		}
	}
	return out
}

func isIncidentState(state StatusState) bool {
	return state == StatusDegraded || state == StatusSerious || state == StatusDown
}

func severityRank(state StatusState) int {
	switch state {
	case StatusDown:
		return 3
	case StatusSerious:
		return 2
	case StatusDegraded:
		return 1
	default:
		return 0
	}
}

func truncateRunes(v string, max int) string {
	runes := []rune(v)
	if len(runes) <= max {
		return v
	}
	return string(runes[:max])
}
