package biz

import (
	"testing"

	"github.com/jucode/jucode/internal/data/ent"
)

func TestAccountContextWindow(t *testing.T) {
	ptr := func(v int) *int { return &v }
	model := &ent.Model{ContextWindow: ptr(1_050_000)}
	cases := []struct {
		name  string
		m     *ent.Model
		group *int
		bound *ent.UpstreamAccountModel
		want  int
	}{
		{"override wins", model, ptr(400_000), &ent.UpstreamAccountModel{ContextWindow: ptr(272_000)}, 272_000},
		{"inherits model default", model, nil, &ent.UpstreamAccountModel{}, 1_050_000},
		{"no binding", model, nil, nil, 1_050_000},
		{"nothing configured", &ent.Model{}, nil, &ent.UpstreamAccountModel{}, 0},
		{"zero override inherits", model, nil, &ent.UpstreamAccountModel{ContextWindow: ptr(0)}, 1_050_000},
		{"group window over the model default", model, ptr(400_000), &ent.UpstreamAccountModel{}, 400_000},
		{"zero group window inherits", model, ptr(0), nil, 1_050_000},
	}
	for _, tc := range cases {
		if got := AccountContextWindow(tc.m, tc.group, tc.bound); got != tc.want {
			t.Errorf("%s: got %d, want %d", tc.name, got, tc.want)
		}
	}
}

func TestNormalizeEfforts(t *testing.T) {
	got := normalizeEfforts([]string{" low", "high", "", "low"})
	if len(got) != 2 || got[0] != "low" || got[1] != "high" {
		t.Fatalf("got %#v", got)
	}
	if normalizeEfforts([]string{" "}) != nil {
		t.Fatal("blank ladder must normalize to nil (not configured)")
	}
}
