package params

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestEdgesOrDefault(t *testing.T) {
	t.Parallel()
	require.Equal(t, DefaultEdges(), Edges{}.OrDefault())
	e := DefaultEdges()
	e.MaxRepairAttempts = 4
	require.Equal(t, e, e.OrDefault(), "a resolved set returns itself")
}

func TestEdgesOrDefaultAllFlagsOff(t *testing.T) {
	t.Parallel()
	// An authored all-false flag set is legitimate — "every edge
	// off" — and must not collapse back to defaults the way a
	// zero-value compare would.
	e := DefaultEdges()
	e.VerificationEnabled = false
	e.TodosEnabled = false
	e.StallEnabled = false
	e.VerifyLoopEnabled = false
	e.BurnWatchEnabled = false
	e.ReconcileEnabled = false
	e.FreshReviewEnabled = false
	require.Equal(t, e, e.OrDefault(), "all-false flags are a resolved set, not a missing one")
}

func TestEdgeEnabled(t *testing.T) {
	t.Parallel()
	e := DefaultEdges()
	for _, name := range []string{
		"verification", "todos", "stall", "verify-loop",
		"burn-watch", "reconcile", "fresh-review",
	} {
		require.True(t, e.EdgeEnabled(name), "%s ships enabled", name)
	}
	e.StallEnabled = false
	require.False(t, e.EdgeEnabled("stall"))
	require.True(t, e.EdgeEnabled("burn-watch"), "other edges stay enabled")
	require.True(t, e.EdgeEnabled("some-future-edge"), "an unknown edge reports enabled")
}

func TestResolveEdges(t *testing.T) {
	t.Parallel()

	t.Run("empty overlay resolves defaults", func(t *testing.T) {
		t.Parallel()
		for _, overlay := range []map[string]any{nil, {}} {
			e, err := ResolveEdges(overlay)
			require.NoError(t, err)
			require.Equal(t, DefaultEdges(), e)
		}
	})

	t.Run("overlay applies", func(t *testing.T) {
		t.Parallel()
		e, err := ResolveEdges(map[string]any{
			"max_repair_attempts":        4,
			"burn_watch_steps_threshold": 50,
			"stall_enabled":              false,
		})
		require.NoError(t, err)
		require.Equal(t, 4, e.MaxRepairAttempts)
		require.Equal(t, 50, e.BurnWatchStepsThreshold)
		require.False(t, e.StallEnabled)
		require.Equal(t, DefaultEdges().VerifyLoopMinSteps, e.VerifyLoopMinSteps,
			"unmentioned keys stay at default")
	})

	t.Run("unknown key is a hard error", func(t *testing.T) {
		t.Parallel()
		_, err := ResolveEdges(map[string]any{"no_such_knob": 3})
		require.Error(t, err)
	})

	t.Run("wrong type is a hard error", func(t *testing.T) {
		t.Parallel()
		_, err := ResolveEdges(map[string]any{"max_repair_attempts": "two"})
		require.Error(t, err)
		_, err = ResolveEdges(map[string]any{"stall_enabled": "yes"})
		require.Error(t, err)
	})

	t.Run("out of bounds is a hard error", func(t *testing.T) {
		t.Parallel()
		for _, overlay := range []map[string]any{
			{"max_repair_attempts": -1},
			{"max_repair_attempts": 8},
			{"burn_watch_steps_threshold": 0},
			{"burn_watch_min_steps": 0},
			{"burn_watch_min_steps": 31}, // past the default step arm
			{"burn_watch_input_tokens": 999},
			{"verify_loop_min_steps": 1},
			{"loop_detection_window_size": 2},
			{"loop_detection_max_repeats": 11}, // past the default window
			{"scope_gate_min_exploration": 0},
			{"scope_gate_plan_bounce_budget": 0},
			{"scope_gate_evidence_bounce_budget": 21},
		} {
			_, err := ResolveEdges(overlay)
			require.Error(t, err, "overlay %v must fail", overlay)
		}
	})

	t.Run("zero repair budget is legal", func(t *testing.T) {
		t.Parallel()
		e, err := ResolveEdges(map[string]any{"max_repair_attempts": 0})
		require.NoError(t, err)
		require.Equal(t, 0, e.MaxRepairAttempts)
	})
}

func TestVersion(t *testing.T) {
	t.Parallel()

	t.Run("stable and distinct from pv0", func(t *testing.T) {
		t.Parallel()
		v := Version(DefaultMemory(), DefaultEdges())
		require.True(t, strings.HasPrefix(v, "pv1-"), v)
		require.NotEqual(t, "pv0", v)
		require.Equal(t, v, Version(DefaultMemory(), DefaultEdges()),
			"version must be deterministic")
	})

	t.Run("edge change rotates the snapshot", func(t *testing.T) {
		t.Parallel()
		base := Version(DefaultMemory(), DefaultEdges())
		e := DefaultEdges()
		e.MaxRepairAttempts++
		require.NotEqual(t, base, Version(DefaultMemory(), e))
		e = DefaultEdges()
		e.BurnWatchEnabled = false
		require.NotEqual(t, base, Version(DefaultMemory(), e),
			"an enable flag is snapshot surface too")
	})

	t.Run("memory change rotates the snapshot", func(t *testing.T) {
		t.Parallel()
		base := Version(DefaultMemory(), DefaultEdges())
		m := DefaultMemory()
		m.FetchLimit++
		require.NotEqual(t, base, Version(m, DefaultEdges()))
	})
}
