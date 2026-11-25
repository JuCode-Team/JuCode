package biz

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/jucode/jucode/internal/data/ent"
)

// buckets builds a grid one minute apart starting at base. A nil entry means
// "no traffic in this bucket"; otherwise the value is the availability percent.
//
// State and percentage are both derived from the counts, the way Snapshot
// derives them — the onset walk reads State now, and a fixture that set only the
// counts would leave every cell at the empty state and vacuously pass. The total
// is large enough that percentages near the healthy threshold survive the
// integer split: at 20 samples per cell, 98% and 97.9% both truncate to 19 good
// and the boundary cases below would silently test the same thing.
func buckets(base time.Time, pcts []*float64) []StatusBucket {
	const total = 1000
	out := make([]StatusBucket, len(pcts))
	for i, p := range pcts {
		out[i] = StatusBucket{Time: base.Add(time.Duration(i) * time.Minute), State: StatusUnknown}
		if p == nil {
			continue
		}
		good := int(*p / 100 * total)
		out[i].Good, out[i].Bad = good, total-good
		out[i].AvailabilityPct = availabilityPercentage(out[i].Good, out[i].Bad)
		out[i].State = publicState(out[i].Good, out[i].Bad)
	}
	return out
}

// loneFailureBucket carries exactly one failure — real, but uncorroborated. It
// must neither start an incident nor extend one backwards.
func loneFailureBucket(base time.Time, index int) StatusBucket {
	b := StatusBucket{
		Time: base.Add(time.Duration(index) * time.Minute),
		Good: 0,
		Bad:  1,
	}
	b.AvailabilityPct = availabilityPercentage(b.Good, b.Bad)
	b.State = publicState(b.Good, b.Bad)
	return b
}

func TestIncidentStartedAtIgnoresLoneFailures(t *testing.T) {
	base := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)

	// One failed request in a quiet bucket, with healthy traffic either side:
	// not an incident, so there is no onset to report.
	grid := buckets(base, []*float64{pct(100), pct(100), pct(100)})
	grid[1] = loneFailureBucket(base, 1)
	if got := incidentStartedAt(grid); got != nil {
		t.Errorf("a lone failure started an incident at %v", got)
	}

	// A lone failure sitting before a real outage must not be absorbed into the
	// run and reported as its onset — that inflates the duration shown to users.
	grid = buckets(base, []*float64{pct(100), pct(100), pct(20), pct(20)})
	grid[1] = loneFailureBucket(base, 1)
	got := incidentStartedAt(grid)
	if got == nil {
		t.Fatal("real outage produced no onset")
	}
	if want := base.Add(2 * time.Minute); !got.Equal(want) {
		t.Errorf("onset = %v, want %v (the first corroborated bucket)", got, want)
	}
}

func pct(v float64) *float64 { return &v }

func TestIncidentStartedAt(t *testing.T) {
	base := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)

	cases := []struct {
		name string
		in   []*float64
		want *time.Time // offset applied below via index
		idx  int        // expected bucket index, -1 for nil
	}{
		{name: "all healthy", in: []*float64{pct(100), pct(100), pct(100)}, idx: -1},
		{name: "trailing outage starts at first bad bucket", in: []*float64{pct(100), pct(50), pct(40)}, idx: 1},
		{name: "recovery ends the run", in: []*float64{pct(10), pct(100), pct(50)}, idx: 2},
		// An empty bucket mid-outage is silence, not recovery: the run continues
		// through it and the onset stays at the earlier bad bucket.
		{name: "empty bucket does not break the run", in: []*float64{pct(100), pct(50), nil, pct(40)}, idx: 1},
		// A trailing empty bucket must not become the onset.
		{name: "empty bucket does not start a run", in: []*float64{pct(100), nil, pct(40)}, idx: 2},
		// No healthy bucket anywhere: the real onset predates the window.
		{name: "outage older than window", in: []*float64{pct(10), pct(20), pct(30)}, idx: -1},
		{name: "no traffic at all", in: []*float64{nil, nil, nil}, idx: -1},
		{name: "empty grid", in: nil, idx: -1},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := incidentStartedAt(buckets(base, tc.in))
			if tc.idx < 0 {
				if got != nil {
					t.Fatalf("got %v, want nil", got)
				}
				return
			}
			want := base.Add(time.Duration(tc.idx) * time.Minute)
			if got == nil || !got.Equal(want) {
				t.Fatalf("got %v, want %v", got, want)
			}
		})
	}
}

// The healthy threshold is inclusive: exactly 99% counts as healthy and ends an
// outage run, a hair under it does not. Both grids open with a healthy bucket
// so the run cannot reach the front of the window, which would return nil for
// a different reason.
func TestIncidentStartedAtUsesHealthyThreshold(t *testing.T) {
	base := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)

	atThreshold := incidentStartedAt(buckets(base, []*float64{pct(100), pct(statusHealthyThreshold), pct(50)}))
	if want := base.Add(2 * time.Minute); atThreshold == nil || !atThreshold.Equal(want) {
		t.Fatalf("onset %v, want %v — a bucket at exactly the healthy threshold must end the run", atThreshold, want)
	}

	justUnder := incidentStartedAt(buckets(base, []*float64{pct(100), pct(statusHealthyThreshold - 0.1), pct(50)}))
	if want := base.Add(time.Minute); justUnder == nil || !justUnder.Equal(want) {
		t.Fatalf("onset %v, want %v — a bucket just under the threshold must continue the run", justUnder, want)
	}
}

func TestOrderGroupsByConfig(t *testing.T) {
	a := &ent.ModelGroup{ID: uuid.New(), Name: "alpha"}
	b := &ent.ModelGroup{ID: uuid.New(), Name: "bravo"}
	c := &ent.ModelGroup{ID: uuid.New(), Name: "charlie"}

	names := func(gs []*ent.ModelGroup) []string {
		out := make([]string, len(gs))
		for i, g := range gs {
			out[i] = g.Name
		}
		return out
	}
	eq := func(t *testing.T, got, want []string) {
		t.Helper()
		if len(got) != len(want) {
			t.Fatalf("got %v, want %v", got, want)
		}
		for i := range got {
			if got[i] != want[i] {
				t.Fatalf("got %v, want %v", got, want)
			}
		}
	}

	t.Run("configured order wins over name order", func(t *testing.T) {
		// Query results arrive name-ordered; the config says otherwise.
		got := orderGroupsByConfig([]*ent.ModelGroup{a, b, c}, []uuid.UUID{c.ID, a.ID, b.ID})
		eq(t, names(got), []string{"charlie", "alpha", "bravo"})
	})

	t.Run("group deleted since save is skipped", func(t *testing.T) {
		got := orderGroupsByConfig([]*ent.ModelGroup{a, c}, []uuid.UUID{c.ID, uuid.New(), a.ID})
		eq(t, names(got), []string{"charlie", "alpha"})
	})

	t.Run("group missing from config is appended, never dropped", func(t *testing.T) {
		got := orderGroupsByConfig([]*ent.ModelGroup{a, b, c}, []uuid.UUID{c.ID})
		eq(t, names(got), []string{"charlie", "alpha", "bravo"})
	})

	t.Run("empty config keeps query order", func(t *testing.T) {
		got := orderGroupsByConfig([]*ent.ModelGroup{a, b}, nil)
		eq(t, names(got), []string{"alpha", "bravo"})
	})
}
