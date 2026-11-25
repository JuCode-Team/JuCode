package llm

import "time"

type latencyTracker struct {
	start time.Time
	m     map[string]int
}

func newLatencyTracker(start time.Time) *latencyTracker {
	return &latencyTracker{start: start, m: make(map[string]int, 16)}
}

func (t *latencyTracker) SinceStart(key string) {
	if t == nil || key == "" || t.start.IsZero() {
		return
	}
	t.m[key] = int(time.Since(t.start).Milliseconds())
}

func (t *latencyTracker) Duration(key string, d time.Duration) {
	if t == nil || key == "" {
		return
	}
	t.m[key] = int(d.Milliseconds())
}

func (t *latencyTracker) Value(key string, v int) {
	if t == nil || key == "" {
		return
	}
	t.m[key] = v
}

func (t *latencyTracker) Snapshot() map[string]int {
	if t == nil || len(t.m) == 0 {
		return nil
	}
	out := make(map[string]int, len(t.m))
	for k, v := range t.m {
		out[k] = v
	}
	return out
}
