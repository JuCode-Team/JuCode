package admin

import (
	"math"
	"testing"

	entschema "github.com/jucode/jucode/internal/data/ent/schema"
)

func TestNormalizeGroupProfile(t *testing.T) {
	t.Run("blank names dropped, values clamped", func(t *testing.T) {
		got, err := normalizeGroupProfile([]entschema.GroupProfileDim{
			{Name: " 速度 ", Value: 95},
			{Name: "", Value: 50},     // unset row: must not appear
			{Name: "   ", Value: 50},  // whitespace-only: same
			{Name: "稳定性", Value: 120}, // clamps to 100
			{Name: "价格", Value: -3},   // clamps to 0
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(got) != 3 {
			t.Fatalf("want 3 dims, got %+v", got)
		}
		if got[0].Name != "速度" || got[0].Value != 95 {
			t.Fatalf("dim 0 wrong: %+v", got[0])
		}
		if got[1].Value != 100 || got[2].Value != 0 {
			t.Fatalf("clamping wrong: %+v", got)
		}
	})

	t.Run("empty input stays empty", func(t *testing.T) {
		got, err := normalizeGroupProfile(nil)
		if err != nil || len(got) != 0 {
			t.Fatalf("want empty, got %+v err=%v", got, err)
		}
	})

	t.Run("more than six rejected", func(t *testing.T) {
		in := make([]entschema.GroupProfileDim, 7)
		for i := range in {
			in[i] = entschema.GroupProfileDim{Name: "d", Value: 1}
		}
		if _, err := normalizeGroupProfile(in); err == nil {
			t.Fatal("expected error for 7 dims")
		}
	})

	t.Run("non-finite rejected", func(t *testing.T) {
		if _, err := normalizeGroupProfile([]entschema.GroupProfileDim{
			{Name: "x", Value: math.NaN()},
		}); err == nil {
			t.Fatal("expected error for NaN")
		}
	})
}
