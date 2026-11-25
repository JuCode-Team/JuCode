package admin

import (
	"testing"
	"time"
)

func TestCooldownSeconds(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		body    int
		query   string
		want    time.Duration
		wantErr bool
	}{
		{name: "body wins", body: 300, query: "60", want: 5 * time.Minute},
		{name: "query fallback", body: 0, query: "600", want: 10 * time.Minute},
		{name: "query wins when body absent", body: -1, query: "60", want: time.Minute},
		{name: "missing everywhere", body: 0, query: "", wantErr: true},
		{name: "non-numeric query", body: 0, query: "abc", wantErr: true},
		{name: "at the cap", body: int(maxManualCooldown.Seconds()), want: maxManualCooldown},
		{name: "over the cap", body: int(maxManualCooldown.Seconds()) + 1, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := cooldownSeconds(tc.body, tc.query)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("cooldownSeconds(%d, %q) = %v, want error", tc.body, tc.query, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("cooldownSeconds(%d, %q): %v", tc.body, tc.query, err)
			}
			if got != tc.want {
				t.Fatalf("cooldownSeconds(%d, %q) = %v, want %v", tc.body, tc.query, got, tc.want)
			}
		})
	}
}
