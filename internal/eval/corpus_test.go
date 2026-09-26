package eval

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCorpus_QuarantineClean(t *testing.T) {
	evalDir, err := filepath.Abs(filepath.Join("..", "..", "eval"))
	require.NoError(t, err)
	corpus, err := LoadCorpus(evalDir)
	require.NoError(t, err)
	require.NotEmpty(t, corpus)

	for id, tr := range corpus {
		tr := tr
		t.Run(id, func(t *testing.T) {
			t.Parallel()
			// Network-dependent trajectories (realrepo-* clone and
			// build a pinned repo per check) can outlast the package
			// timeout under load; -short keeps the suite hermetic.
			if testing.Short() && tr.Requires.Network != nil && *tr.Requires.Network {
				t.Skip("requires network (-short)")
			}
			// Quarantine doesn't consult requires — a host missing a
			// declared tool would report every state as broken rather
			// than judging the check, so skip instead of failing.
			if missing := CheckRequires(tr); len(missing) > 0 {
				t.Skipf("missing requirements: %v", missing)
			}
			r := &Runner{EvalDir: evalDir, QuarantineRepeats: 2, WorkParent: t.TempDir()}
			t.Cleanup(r.Close)
			dir := filepath.Join(evalDir, "corpus", id)
			reason, err := r.Quarantine(context.Background(), tr, dir)
			require.NoError(t, err)
			// A rotted seed must fail CI, not just log.
			require.Empty(t, reason, "trajectory %s quarantined: %s", id, reason)
		})
	}
}

// trajWithTurns builds a corpus entry whose turn count is n.
func trajWithTurns(id string, n int) *Trajectory {
	return &Trajectory{ID: id, Task: Task{Turns: make([]string, n)}}
}

// Selector refinements (@min_turns>=N, @max_turns<=N) filter a base
// glob's matches by turn count — the broad-selector path that keeps
// short trajectories from starving prior-turns predicates (#113).
func TestSelectCorpus_TurnCountRefinements(t *testing.T) {
	t.Parallel()

	corpus := map[string]*Trajectory{
		"one-turn":   trajWithTurns("one-turn", 1),
		"three-turn": trajWithTurns("three-turn", 3),
		"four-turn":  trajWithTurns("four-turn", 4),
		"long-task":  trajWithTurns("long-task", 16),
	}
	bands := &Bands{}
	ids := func(trajs []*Trajectory, err error) []string {
		require.NoError(t, err)
		var out []string
		for _, tr := range trajs {
			out = append(out, tr.ID)
		}
		return out
	}

	require.Equal(t, []string{"four-turn", "long-task"},
		ids(SelectCorpus(corpus, bands, []string{"*@min_turns>=4"})))
	require.Equal(t, []string{"one-turn", "three-turn"},
		ids(SelectCorpus(corpus, bands, []string{"*@max_turns<=3"})))
	require.Equal(t, []string{"four-turn"},
		ids(SelectCorpus(corpus, bands, []string{"*-turn@min_turns>=4@max_turns<=10"})))
	// A refinement that empties the match set is a valid empty
	// selection — only the base glob typo-errors.
	require.Empty(t, ids(SelectCorpus(corpus, bands, []string{"*@min_turns>=99"})))
}

func TestSelectCorpus_RefinementErrors(t *testing.T) {
	t.Parallel()
	corpus := map[string]*Trajectory{"t": trajWithTurns("t", 2)}
	for _, sel := range []string{
		"*@min_turns<=4",   // wrong direction
		"*@max_turns>=4",   // wrong direction
		"*@turns>=4",       // unknown key
		"*@min_turns>=x",   // non-numeric
		"*@min_turns>=",    // missing value
		"*@min_turns>=4@x", // malformed second clause
	} {
		_, err := SelectCorpus(corpus, &Bands{}, []string{sel})
		require.Error(t, err, sel)
	}
	// A refined quarantine band selects nothing rather than erroring —
	// the bar lives in load-time validation.
	got, err := SelectCorpus(corpus, &Bands{}, []string{"band:quarantined@min_turns>=1"})
	require.NoError(t, err)
	require.Empty(t, got)
	temp := 0.0
	err = ValidateExperiment(&Experiment{
		Name:              "e",
		Model:             "p/m",
		Temperature:       &temp,
		Corpus:            []string{"band:quarantined@min_turns>=1"},
		RunsPerTrajectory: map[Band]int{BandStable: 1},
		Arms:              map[string]Arm{ArmControl: {}, ArmTreatment: {}},
	})
	require.ErrorContains(t, err, "quarantined")
}
