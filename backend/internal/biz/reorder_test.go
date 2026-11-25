package biz

import (
	"testing"

	"github.com/google/uuid"
)

// ids gives stable, comparable uuids so a failure message points at a position
// rather than a random hex string.
func reorderIDs(n int) []uuid.UUID {
	out := make([]uuid.UUID, n)
	for i := range out {
		out[i] = uuid.MustParse("00000000-0000-0000-0000-0000000000" + string("0123456789abcdef"[i/16]) + string("0123456789abcdef"[i%16]))
	}
	return out
}

func indexOfAll(t *testing.T, all, subset []uuid.UUID) []int {
	t.Helper()
	pos := make([]int, len(subset))
	for i, want := range subset {
		pos[i] = -1
		for j, have := range all {
			if have == want {
				pos[i] = j
				break
			}
		}
	}
	return pos
}

func assertOrder(t *testing.T, got, want []uuid.UUID) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("length %d, want %d", len(got), len(want))
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("position %d: got %v, want %v\nfull: %v", i, got[i], want[i], got)
		}
	}
}

func TestSpliceOrder(t *testing.T) {
	id := reorderIDs(6)

	t.Run("full list is a straight rewrite", func(t *testing.T) {
		got, err := spliceOrder(id[:4], []uuid.UUID{id[3], id[2], id[1], id[0]})
		if err != nil {
			t.Fatal(err)
		}
		assertOrder(t, got, []uuid.UUID{id[3], id[2], id[1], id[0]})
	})

	// This is the case that makes paginated reordering safe: rows outside the
	// submitted page must not move, and the submitted rows may only occupy the
	// slots they already held.
	t.Run("partial list only rearranges its own slots", func(t *testing.T) {
		current := []uuid.UUID{id[0], id[1], id[2], id[3], id[4]}
		got, err := spliceOrder(current, []uuid.UUID{id[3], id[1]})
		if err != nil {
			t.Fatal(err)
		}
		// Slots 1 and 3 were held by the subset; everything else is untouched.
		assertOrder(t, got, []uuid.UUID{id[0], id[3], id[2], id[1], id[4]})
	})

	t.Run("non-contiguous subset keeps outsiders in place", func(t *testing.T) {
		current := []uuid.UUID{id[0], id[1], id[2], id[3], id[4], id[5]}
		got, err := spliceOrder(current, []uuid.UUID{id[4], id[2], id[0]})
		if err != nil {
			t.Fatal(err)
		}
		assertOrder(t, got, []uuid.UUID{id[4], id[1], id[2], id[3], id[0], id[5]})
		// id[1], id[3], id[5] never moved.
		if pos := indexOfAll(t, got, []uuid.UUID{id[1], id[3], id[5]}); pos[0] != 1 || pos[1] != 3 || pos[2] != 5 {
			t.Fatalf("outsiders moved: %v", pos)
		}
	})

	t.Run("single element is a no-op", func(t *testing.T) {
		current := []uuid.UUID{id[0], id[1], id[2]}
		got, err := spliceOrder(current, []uuid.UUID{id[1]})
		if err != nil {
			t.Fatal(err)
		}
		assertOrder(t, got, current)
	})

	t.Run("unknown id is rejected whole", func(t *testing.T) {
		if _, err := spliceOrder([]uuid.UUID{id[0], id[1]}, []uuid.UUID{id[0], id[5]}); err == nil {
			t.Fatal("expected an error for an id not present in the current order")
		}
	})

	// A duplicate would otherwise silently overwrite one slot and leave another
	// holding a stale id, quietly losing a row from the ordering.
	t.Run("duplicate id is rejected", func(t *testing.T) {
		if _, err := spliceOrder([]uuid.UUID{id[0], id[1], id[2]}, []uuid.UUID{id[0], id[0]}); err == nil {
			t.Fatal("expected an error for a duplicated id")
		}
	})

	t.Run("does not mutate the input slice", func(t *testing.T) {
		current := []uuid.UUID{id[0], id[1], id[2]}
		before := append([]uuid.UUID(nil), current...)
		if _, err := spliceOrder(current, []uuid.UUID{id[2], id[0]}); err != nil {
			t.Fatal(err)
		}
		assertOrder(t, current, before)
	})

	t.Run("result is always a permutation", func(t *testing.T) {
		current := []uuid.UUID{id[0], id[1], id[2], id[3], id[4]}
		got, err := spliceOrder(current, []uuid.UUID{id[4], id[0], id[2]})
		if err != nil {
			t.Fatal(err)
		}
		seen := make(map[uuid.UUID]int, len(got))
		for _, v := range got {
			seen[v]++
		}
		if len(seen) != len(current) {
			t.Fatalf("got %d distinct ids, want %d: %v", len(seen), len(current), got)
		}
		for v, n := range seen {
			if n != 1 {
				t.Fatalf("id %v appears %d times", v, n)
			}
		}
	})
}
