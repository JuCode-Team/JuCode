package admin

import (
	"testing"
	"time"
)

func TestBoundedAnomalyStart(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 28, 20, 0, 0, 0, time.UTC)
	oldStart := now.Add(-7 * 24 * time.Hour)
	recentStart := now.Add(-2 * time.Hour)
	historicalEnd := now.Add(-30 * 24 * time.Hour)
	historicalStart := historicalEnd.Add(-48 * time.Hour)

	cases := []struct {
		name    string
		anomaly string
		start   *time.Time
		end     *time.Time
		want    *time.Time
	}{
		{name: "ordinary query stays unbounded"},
		{name: "ordinary query keeps requested start", start: &oldStart, want: &oldStart},
		{name: "missing start is capped", anomaly: "any", want: timePointer(now.Add(-24 * time.Hour))},
		{name: "old explicit start is capped", anomaly: "error", start: &oldStart, want: timePointer(now.Add(-24 * time.Hour))},
		{name: "recent explicit start wins", anomaly: "time", start: &recentStart, want: &recentStart},
		{
			name:    "historical search is capped relative to explicit end",
			anomaly: "none",
			start:   &historicalStart,
			end:     &historicalEnd,
			want:    timePointer(historicalEnd.Add(-24 * time.Hour)),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := boundedAnomalyStart(tc.anomaly, now, tc.start, tc.end)
			if tc.want == nil {
				if got != nil {
					t.Fatalf("start = %v, want nil", got)
				}
				return
			}
			if got == nil || !got.Equal(*tc.want) {
				t.Fatalf("start = %v, want %v", got, *tc.want)
			}
		})
	}
}

func timePointer(v time.Time) *time.Time { return &v }
