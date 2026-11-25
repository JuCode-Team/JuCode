package llm

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/jucode/jucode/internal/biz"
	"github.com/jucode/jucode/internal/data/ent"
	llmpkg "github.com/jucode/jucode/internal/llm"
)

func TestGrokBuildModelsEnvelope(t *testing.T) {
	configured, bare := uuid.New(), uuid.New()
	got := grokBuildModels([]*ent.Model{
		{ID: configured, Name: "gpt-5.5", CreatedAt: time.Unix(0, 0)},
		{ID: bare, Name: "gpt-6-sol", CreatedAt: time.Unix(0, 0)},
	}, map[uuid.UUID]biz.ModelCapabilities{
		configured: {ContextWindow: 272_000, MaxContextWindow: 1_050_000, ReasoningEfforts: []string{"low", "high"}},
	})
	if got.Object != "list" || len(got.Data) != 2 {
		t.Fatalf("envelope = %+v", got)
	}
	entry := got.Data[0]
	if entry.ID != "gpt-5.5" || entry.APIBackend != "responses" || !entry.SupportedInAPI {
		t.Fatalf("entry = %+v", entry)
	}
	// The conservative (smallest) window is what an unaware client gets.
	if entry.ContextWindow != 272_000 || len(entry.ReasoningEfforts) != 2 || entry.ReasoningEfforts[0].Value != "low" {
		t.Fatalf("metadata = %+v", entry)
	}
	// Unconfigured capabilities are omitted, never defaulted.
	if e := got.Data[1]; e.ContextWindow != 0 || len(e.ReasoningEfforts) != 0 {
		t.Fatalf("unconfigured model advertised metadata: %+v", e)
	}
	_ = llmpkg.GrokBuildModelsResponse{}
}

// A picker-hidden model stays in /v1/models (clients call it by name) and is
// marked "hidden"; other entries carry no "hidden" key at all.
func TestOpenAIModelsMarksPickerHidden(t *testing.T) {
	got := openAIModels([]*ent.Model{
		{ID: uuid.New(), Name: "gpt-5.5", CreatedAt: time.Unix(0, 0)},
		{ID: uuid.New(), Name: "codex-auto-review", CreatedAt: time.Unix(0, 0), PickerHidden: true},
	}, nil)
	if len(got) != 2 {
		t.Fatalf("entries = %v, want both models listed", got)
	}
	if _, ok := got[0]["hidden"]; ok {
		t.Fatalf("visible model carries hidden: %v", got[0])
	}
	if got[1]["id"] != "codex-auto-review" || got[1]["hidden"] != true {
		t.Fatalf("hidden model entry = %v", got[1])
	}
}
