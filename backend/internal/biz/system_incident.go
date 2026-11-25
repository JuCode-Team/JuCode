package biz

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/jucode/jucode/internal/data"
	"github.com/jucode/jucode/internal/data/ent"
	entsystemincident "github.com/jucode/jucode/internal/data/ent/systemincident"
)

// SystemIncidentSvc owns persisted system SLA incidents. It deliberately does
// not use Redis: this is a small operational history table.
type SystemIncidentSvc struct {
	d *data.Data
}

func NewSystemIncidentSvc(d *data.Data) *SystemIncidentSvc {
	return &SystemIncidentSvc{d: d}
}

type SystemIncidentListInput struct {
	Status  string
	GroupID *uuid.UUID
	Start   *time.Time
	End     *time.Time
	Limit   int
	Offset  int
}

func (s *SystemIncidentSvc) List(ctx context.Context, in SystemIncidentListInput) ([]*ent.SystemIncident, int, error) {
	q := s.d.Ent.SystemIncident.Query()
	switch in.Status {
	case "open":
		q.Where(entsystemincident.ResolvedAtIsNil())
	case "resolved":
		q.Where(entsystemincident.ResolvedAtNotNil())
	}
	if in.GroupID != nil {
		q.Where(entsystemincident.GroupIDEQ(*in.GroupID))
	}
	if in.Start != nil {
		q.Where(entsystemincident.StartedAtGTE(*in.Start))
	}
	if in.End != nil {
		q.Where(entsystemincident.StartedAtLTE(*in.End))
	}
	total, err := q.Clone().Count(ctx)
	if err != nil {
		return nil, 0, err
	}
	rows, err := q.
		Order(ent.Desc(entsystemincident.FieldStartedAt), ent.Desc(entsystemincident.FieldID)).
		Limit(in.Limit).
		Offset(in.Offset).
		All(ctx)
	return rows, total, err
}

func (s *SystemIncidentSvc) Get(ctx context.Context, id uuid.UUID) (*ent.SystemIncident, error) {
	return s.d.Ent.SystemIncident.Get(ctx, id)
}

func (s *SystemIncidentSvc) UpdateNote(ctx context.Context, id uuid.UUID, note string) (*ent.SystemIncident, error) {
	return s.d.Ent.SystemIncident.UpdateOneID(id).SetNote(note).Save(ctx)
}

func (s *SystemIncidentSvc) listOpenForSLA(ctx context.Context) ([]slaStoredIncident, error) {
	rows, err := s.d.Ent.SystemIncident.Query().
		Where(
			entsystemincident.ResolvedAtIsNil(),
			entsystemincident.ModelEQ(""),
		).
		All(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]slaStoredIncident, 0, len(rows))
	for _, row := range rows {
		out = append(out, slaStoredIncident{
			ID:                       row.ID,
			GroupID:                  row.GroupID,
			Model:                    row.Model,
			Severity:                 StatusState(row.Severity),
			WorstSeverity:            StatusState(row.WorstSeverity),
			WorstAvailabilityPct:     cloneFloat(row.WorstAvailabilityPct),
			LastAvailabilityPct:      cloneFloat(row.LastAvailabilityPct),
			MaximumWindowSampleCount: row.SampleCount,
		})
	}
	return out, nil
}

func (s *SystemIncidentSvc) createOpenForSLA(ctx context.Context, in slaOpenIncident) error {
	return s.d.Ent.SystemIncident.Create().
		SetGroupID(in.GroupID).
		SetGroupName(in.GroupName).
		SetModel(in.Model).
		SetSeverity(entsystemincident.Severity(in.Severity)).
		SetWorstSeverity(entsystemincident.WorstSeverity(in.Severity)).
		SetStartedAt(in.StartedAt).
		SetDetectedAt(in.DetectedAt).
		SetNillableOpenAvailabilityPct(in.AvailabilityPct).
		SetNillableWorstAvailabilityPct(in.AvailabilityPct).
		SetNillableLastAvailabilityPct(in.AvailabilityPct).
		SetSampleCount(in.SampleCount).
		OnConflict().
		DoNothing().
		Exec(ctx)
}

func (s *SystemIncidentSvc) updateOpenForSLA(ctx context.Context, in slaIncidentUpdate) error {
	q := s.d.Ent.SystemIncident.Update().
		Where(
			entsystemincident.IDEQ(in.ID),
			entsystemincident.ResolvedAtIsNil(),
		).
		SetWorstSeverity(entsystemincident.WorstSeverity(in.WorstSeverity)).
		SetSampleCount(in.MaximumWindowSampleCount).
		SetNillableWorstAvailabilityPct(in.WorstAvailabilityPct).
		SetNillableLastAvailabilityPct(in.LastAvailabilityPct)
	if in.CurrentSeverity != "" {
		q.SetSeverity(entsystemincident.Severity(in.CurrentSeverity))
	}
	_, err := q.Save(ctx)
	return err
}

func (s *SystemIncidentSvc) resolveForSLA(ctx context.Context, id uuid.UUID, at time.Time, resolution string, worst StatusState) error {
	_, err := s.d.Ent.SystemIncident.Update().
		Where(
			entsystemincident.IDEQ(id),
			entsystemincident.ResolvedAtIsNil(),
		).
		SetResolvedAt(at).
		SetResolution(resolution).
		SetSeverity(entsystemincident.Severity(worst)).
		Save(ctx)
	return err
}

func cloneFloat(v *float64) *float64 {
	if v == nil {
		return nil
	}
	out := *v
	return &out
}
