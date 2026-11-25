package biz

import (
	"testing"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/jucode/jucode/internal/data/ent"
)

func TestModelSupportsImage(t *testing.T) {
	price := decimal.RequireFromString("0.2")
	cases := []struct {
		name  string
		model *ent.Model
		want  bool
	}{
		{"nil", nil, false},
		{"text", &ent.Model{Name: "claude-sonnet-4-6"}, false},
		{"flat image price", &ent.Model{Name: "flux", OutputImagePrice: &price}, true},
		{"gpt-image priced like text", &ent.Model{Name: "gpt-image-2"}, true},
		{"dall-e", &ent.Model{Name: "dall-e-3"}, true},
		{"chat image tokens are not an image group", &ent.Model{Name: "gpt-5", ImageOutputUnitPrice: &price}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := modelSupportsImage(tc.model); got != tc.want {
				t.Fatalf("modelSupportsImage = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestModelNameServed(t *testing.T) {
	group := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	other := uuid.MustParse("22222222-2222-2222-2222-222222222222")
	served := map[uuid.UUID]map[string]struct{}{
		group: {"gpt-image-2": {}},
	}
	if !modelNameServed(served, group, "gpt-image-2") {
		t.Fatal("served model must stay in the picker catalog")
	}
	if modelNameServed(served, group, "claude-fable-5") {
		t.Fatal("attached model with no live account must be omitted")
	}
	if modelNameServed(served, other, "gpt-image-2") {
		t.Fatal("a model served by another group must not leak")
	}
}
