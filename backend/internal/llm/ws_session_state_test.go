package llm

import (
	"fmt"
	"strings"
	"testing"
)

// Both collections on a bridged session grow purely from client frames, and
// nothing else bounds them: the frame limit caps one frame's size, not how many
// arrive, and the idle timeout refreshes on every frame. Without a budget one
// authenticated socket can walk the node out of memory.
func TestRealtimeSessionStateStaysBounded(t *testing.T) {
	// ~64 KiB per entry, so the budget is crossed well inside the loop.
	blob := strings.Repeat("x", 64<<10)

	t.Run("warmup cache", func(t *testing.T) {
		sess := newRealtimeSession("sess", "m")
		var lastID string
		for i := 0; i < 400; i++ {
			lastID = fmt.Sprintf("resp_%d", i)
			sess.putSynthetic(lastID, map[string]any{"model": "m", "input": blob})
		}
		if sess.syntheticBytes > wsSessionStateBudget {
			t.Errorf("retained %d bytes, over the %d budget", sess.syntheticBytes, wsSessionStateBudget)
		}
		if len(sess.synthetic) != len(sess.syntheticOrder) {
			t.Errorf("map has %d entries but order has %d — eviction leaked", len(sess.synthetic), len(sess.syntheticOrder))
		}
		// The chain Codex would follow next is the most recent one.
		if _, ok := sess.lookupSynthetic(lastID); !ok {
			t.Error("newest warmup was evicted; the next turn cannot chain from it")
		}
	})

	t.Run("conversation items", func(t *testing.T) {
		sess := newRealtimeSession("sess", "m")
		for i := 0; i < 400; i++ {
			sess.addItem(map[string]any{"type": "message", "content": blob, "seq": i})
		}
		if sess.itemBytes > wsSessionStateBudget {
			t.Errorf("retained %d bytes, over the %d budget", sess.itemBytes, wsSessionStateBudget)
		}
		if len(sess.items) != len(sess.itemSizes) {
			t.Errorf("items=%d sizes=%d — eviction left the two out of step", len(sess.items), len(sess.itemSizes))
		}
		// Oldest goes first, so the newest turn is still there to answer.
		_, _, _, items := sess.snapshot()
		if len(items) == 0 || intVal(items[len(items)-1]["seq"]) != 399 {
			t.Error("newest item was evicted; the session would answer a stale turn")
		}
	})

	// A single entry larger than the whole budget is still kept: dropping the
	// turn the client just sent would answer a different question.
	t.Run("overwrite does not double-count", func(t *testing.T) {
		sess := newRealtimeSession("sess", "m")
		sess.putSynthetic("resp_1", map[string]any{"input": blob})
		first := sess.syntheticBytes
		sess.putSynthetic("resp_1", map[string]any{"input": blob})
		if sess.syntheticBytes != first {
			t.Errorf("overwrite grew the budget from %d to %d", first, sess.syntheticBytes)
		}
		if len(sess.syntheticOrder) != 1 {
			t.Errorf("order has %d entries after overwrite, want 1", len(sess.syntheticOrder))
		}
	})

	t.Run("oversized single entry", func(t *testing.T) {
		sess := newRealtimeSession("sess", "m")
		sess.addItem(map[string]any{"content": strings.Repeat("y", wsSessionStateBudget+1024)})
		if len(sess.items) != 1 {
			t.Errorf("items=%d, want the one oversized item kept", len(sess.items))
		}
		sess.putSynthetic("resp_big", map[string]any{"input": strings.Repeat("y", wsSessionStateBudget+1024)})
		if _, ok := sess.lookupSynthetic("resp_big"); !ok {
			t.Error("oversized warmup dropped itself")
		}
	})
}
