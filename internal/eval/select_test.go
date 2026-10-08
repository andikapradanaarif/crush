package eval

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestRunSelectProbe(t *testing.T) {
	corpus := t.TempDir()
	spec := GenSpec{
		Quirks: 4, Distractors: 2, Plausibility: "high",
		Prompt: "vague", Depth: 2, Seed: 42,
	}
	dir, err := Generate(corpus, spec)
	require.NoError(t, err)
	traj, err := LoadTrajectory(dir)
	require.NoError(t, err)

	r := &Runner{EvalDir: t.TempDir(), Home: t.TempDir()}
	ctx := context.Background()

	// The seeded instance leaves all three pools populated — the
	// probe must see candidates from each.
	rep, err := r.RunSelectProbe(ctx, traj, dir, "", nil)
	require.NoError(t, err)
	require.Equal(t, traj.ID, rep.Trajectory)
	require.NotEmpty(t, rep.Prompt)
	require.Positive(t, rep.Counts["open_seen"])
	require.Positive(t, rep.Counts["resolved_seen"])
	require.Positive(t, rep.Counts["command_seen"])
	require.Equal(t, len(rep.Candidates), rep.Counts["candidates"])
	for _, c := range rep.Candidates {
		require.Contains(t, []string{"open", "resolved", "command"}, c.Pool)
		require.NotEmpty(t, c.Reason)
	}

	// A second probe reuses the seeded workdir — seeds run once.
	rep2, err := r.RunSelectProbe(ctx, traj, dir, "", nil)
	require.NoError(t, err)
	require.Equal(t, rep.Workdir, rep2.Workdir)

	// Zero render limit suppresses the pool at fetch — no open
	// candidates, no open admits, other pools untouched.
	repOff, err := r.RunSelectProbe(ctx, traj, dir, "",
		map[string]any{"open_render_limit": 0})
	require.NoError(t, err)
	require.Zero(t, repOff.Counts["open_seen"])
	require.Zero(t, repOff.Counts["open_admit"])
	for _, c := range repOff.Candidates {
		require.NotEqual(t, "open", c.Pool)
	}
	require.Positive(t, repOff.Counts["resolved_seen"])
}

// The seeded.ok marker is bounded by the authored rows' remaining
// TTL — they age on the wall clock, so a genwork dir old enough
// that its oldest row crossed open_failure_ttl serves under-dosed
// state silently. Reuse must stop at ttl-maxAgo, not at the
// marker's existence.
func TestEnsureSeeded_StaleMarkerReseeds(t *testing.T) {
	t.Parallel()
	corpus := t.TempDir()
	spec := GenSpec{
		Quirks: 1, Distractors: 0, Plausibility: "low",
		Prompt: "explicit", Seed: 9,
	}
	dir, err := Generate(corpus, spec)
	require.NoError(t, err)
	traj, err := LoadTrajectory(dir)
	require.NoError(t, err)

	now := time.Now()
	r := &Runner{EvalDir: t.TempDir(), Home: t.TempDir(), Now: func() time.Time { return now }}
	ctx := context.Background()
	const ttl = 720 * time.Hour

	w1, _, err := r.EnsureSeeded(ctx, traj, dir, ttl)
	require.NoError(t, err)
	w2, _, err := r.EnsureSeeded(ctx, traj, dir, ttl)
	require.NoError(t, err)
	require.Equal(t, w1, w2, "fresh marker must reuse the seeded dir")

	// Jump to the boundary: the oldest authored row is exactly at
	// the TTL now — the dir must re-seed rather than reuse.
	var maxAgo float64
	for _, s := range traj.SeedCommands {
		maxAgo = max(maxAgo, s.AgoSeconds)
	}
	now = now.Add(ttl - time.Duration(maxAgo)*time.Second)
	w3, _, err := r.EnsureSeeded(ctx, traj, dir, ttl)
	require.NoError(t, err)
	require.NotEqual(t, w1, w3, "stale marker must re-materialize, not reuse")
}

func TestRunSelectSweep(t *testing.T) {
	corpus := t.TempDir()
	r := &Runner{EvalDir: t.TempDir(), Home: t.TempDir()}
	var rows []SelectSweepRow
	err := r.RunSelectSweep(context.Background(), corpus, SelectSweepSpec{
		Quirks: []int{1}, Distractors: []int{0, 1},
		Plausibility: []string{"low"}, Prompt: []string{"vague"},
		Depth: []int{0}, Replicates: 1, Seed: 7,
	}, nil, func(row SelectSweepRow) { rows = append(rows, row) })
	require.NoError(t, err)
	require.Len(t, rows, 2)
	for i, row := range rows {
		require.Empty(t, row.Error)
		require.NotNil(t, row.Report)
		require.Equal(t, i, row.Cell.Distractors)
		require.Positive(t, row.Report.Counts["command_seen"])
	}
	// K=1 stored one more distractor row than K=0 — the sweep sees
	// the dose it generated.
	require.Greater(t, rows[1].Report.Counts["candidates"],
		rows[0].Report.Counts["candidates"])
}
