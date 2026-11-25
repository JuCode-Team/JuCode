package user

import (
	"slices"
	"testing"

	"github.com/google/uuid"

	"github.com/jucode/jucode/internal/data/ent"
)

func TestToApiKeyViewHidesRetiredGroups(t *testing.T) {
	enabled, retired := uuid.New(), uuid.New()
	live := map[uuid.UUID]struct{}{enabled: {}}

	v := toApiKeyView(&ent.ApiKey{AllowedGroupIds: []uuid.UUID{retired, enabled}}, live)
	if !slices.Equal(v.AllowedGroupIDs, []string{enabled.String()}) || v.AllowedGroupsUnrestricted {
		t.Fatalf("view = %v unrestricted=%v, want only the enabled group", v.AllowedGroupIDs, v.AllowedGroupsUnrestricted)
	}

	v = toApiKeyView(&ent.ApiKey{AllowedGroupIds: []uuid.UUID{retired}}, live)
	if v.AllowedGroupIDs == nil || len(v.AllowedGroupIDs) != 0 || v.AllowedGroupsUnrestricted {
		t.Fatalf("all-retired view = %#v unrestricted=%v, want [] and restricted", v.AllowedGroupIDs, v.AllowedGroupsUnrestricted)
	}

	v = toApiKeyView(&ent.ApiKey{AllowedGroupIds: []uuid.UUID{}}, live)
	if len(v.AllowedGroupIDs) != 0 || !v.AllowedGroupsUnrestricted {
		t.Fatalf("empty stored list must read as unrestricted, got %v", v.AllowedGroupsUnrestricted)
	}
}
