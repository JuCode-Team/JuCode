package biz

import (
	"testing"

	"github.com/google/uuid"
)

func TestEvaluateScores(t *testing.T) {
	cases := []struct {
		name      string
		scores    map[string]float64
		threshold float64
		wantViol  bool
		wantSev   string
	}{
		{"clean below threshold", map[string]float64{"violence": 0.2, "hate": 0.1}, 0.5, false, "low"},
		{"medium band", map[string]float64{"violence": 0.74}, 0.5, true, "medium"},
		{"high band", map[string]float64{"illicit": 0.9}, 0.5, true, "high"},
		{"critical band", map[string]float64{"illicit/violent": 0.97}, 0.5, true, "critical"},
		{"minors forced critical", map[string]float64{"sexual/minors": 0.6}, 0.5, true, "critical"},
		{"strict threshold flags low-confidence", map[string]float64{"harassment": 0.35}, 0.3, true, "low"},
	}
	for _, c := range cases {
		v := evaluateScores(c.scores, c.threshold)
		if v.Violation != c.wantViol {
			t.Errorf("%s: violation=%v want %v", c.name, v.Violation, c.wantViol)
		}
		if v.Violation && v.Severity != c.wantSev {
			t.Errorf("%s: severity=%q want %q (max %.2f)", c.name, v.Severity, c.wantSev, v.MaxScore)
		}
	}
}

func TestSampleHitBounds(t *testing.T) {
	uid := uuid.New()
	if sampleHit(uid, 0) {
		t.Errorf("rate 0 must never sample")
	}
	if !sampleHit(uid, 1) {
		t.Errorf("rate 1 must always sample")
	}
}

func TestModerationNilReceiverSafe(t *testing.T) {
	var m *ModerationSvc // nil
	m.MaybeReview(ModerationRequest{Text: "anything"})
}
