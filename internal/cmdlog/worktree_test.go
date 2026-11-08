package cmdlog

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/charmbracelet/crush/internal/db"
	"github.com/charmbracelet/crush/internal/params"
	"github.com/stretchr/testify/require"
)

// setupWorktreeRepo builds a real repository at env.workingDir plus
// one linked sibling worktree — the R8 reproduction: the owner's
// workflow mandates editing in `../<slug>` worktrees while the
// capture gate scoped everything to the workspace root.
// The returned git helper runs inside the MAIN checkout.
func setupWorktreeRepo(t *testing.T) (*labelEnv, string, func(args ...string)) {
	t.Helper()
	env := setupLabelsTest(t)
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	git := func(args ...string) {
		full := append([]string{"-C", env.workingDir}, args...)
		require.NoError(t, exec.CommandContext(t.Context(), "git", full...).Run())
	}
	git("init", "-b", "main")
	git("config", "user.email", "t@t")
	git("config", "user.name", "t")
	require.NoError(t, os.WriteFile(
		filepath.Join(env.workingDir, "base.go"), []byte("package base\n"), 0o644))
	git("add", ".")
	git("commit", "-m", "base")

	// The service is constructed BEFORE the worktree is added —
	// the worktree set must be discovered at capture time, not at
	// service construction (agents run `git worktree add`
	// mid-session).
	env.svc = NewService(db.New(env.conn), env.workingDir, params.DefaultMemory())

	wt := filepath.Join(t.TempDir(), "wt")
	git("worktree", "add", "-b", "wt1", wt)
	return env, wt, git
}

// R8: an edit inside a linked worktree of the same repo is in-scope
// capture — the cell records content and the repo-relative path the
// merged-worktree fallback later needs.
func TestWorktreeCapture_AddedCell(t *testing.T) {
	env, wt, _ := setupWorktreeRepo(t)
	target := filepath.Join(wt, "feat.go")
	require.NoError(t, os.WriteFile(target, []byte("package feat\n// new line\n"), 0o644))

	require.NoError(t, env.svc.RecordTurnOutcome(t.Context(), TurnOutcome{
		SessionID:   "s1",
		TurnID:      "t0",
		EditedPaths: []string{target},
		EditHunks:   map[string][]string{target: {"// new line"}},
		HunkAdded:   map[string][]HunkEdit{target: {{New: "// new line"}}},
		HunkSpans:   map[string][]HunkSpan{target: {{Start: 0, End: 1, After: "// new line"}}},
	}))

	var m map[string]hunkAddedEntry
	require.NoError(t, json.Unmarshal([]byte(readTurn(t, env, "s1", "t0").hunkAdded), &m))
	require.Equal(t, hunkOK, m[target].State,
		"a linked worktree is an in-scope capture root")
	require.Equal(t, "feat.go", m[target].RepoPath,
		"the cell carries the repo-relative path — the merged-"+
			"worktree fallback's lookup key")
	require.Len(t, m[target].Edits, 1)
	require.Len(t, m[target].Spans, 1)
}

// R8 settle: while the worktree still exists, reads route to the
// worktree's own checkout — not the main one.
func TestWorktreeCapture_SettleLiveWorktree(t *testing.T) {
	env, wt, _ := setupWorktreeRepo(t)
	target := filepath.Join(wt, "feat.go")
	require.NoError(t, os.WriteFile(target,
		[]byte("head\n// new line\ntail\n"), 0o644))

	require.NoError(t, env.svc.RecordTurnOutcome(t.Context(), TurnOutcome{
		SessionID:   "s1",
		TurnID:      "t0",
		EditedPaths: []string{target},
		EditHunks:   map[string][]string{target: {"// new line"}},
		HunkAdded:   map[string][]HunkEdit{target: {{New: "// new line"}}},
		HunkSpans:   map[string][]HunkSpan{target: {{Start: 0, End: 3, Before: "head", After: "tail"}}},
	}))
	dueViaNewerSession(t, env, "s2")
	c := settleTurnRow(t, env, "s1", "t0")

	require.True(t, c.settledAt.Valid)
	require.Equal(t, TurnAccepted, c.verdict.String,
		"the fingerprint survives in the worktree's own checkout")
	var after map[string]hunkAfterEntry
	require.NoError(t, json.Unmarshal([]byte(c.hunkAfter), &after))
	require.Equal(t, hunkOK, after[target].State,
		"post-window content comes from the worktree, not the main checkout")
	require.NotEmpty(t, after[target].Windows)
}

// R8 settle after merge: the worktree is gone, so the read falls to
// the merged commit on the default branch.
func TestWorktreeCapture_SettleMergedWorktree(t *testing.T) {
	env, wt, git := setupWorktreeRepo(t)
	target := filepath.Join(wt, "feat.go")
	require.NoError(t, os.WriteFile(target,
		[]byte("package feat\n// new line\n"), 0o644))

	require.NoError(t, env.svc.RecordTurnOutcome(t.Context(), TurnOutcome{
		SessionID:   "s1",
		TurnID:      "t0",
		EditedPaths: []string{target},
		EditHunks:   map[string][]string{target: {"// new line"}},
		HunkAdded:   map[string][]HunkEdit{target: {{New: "// new line"}}},
		HunkSpans:   map[string][]HunkSpan{target: {{Start: 0, End: 2, Before: "package feat"}}},
	}))

	// The work lands on the default branch, then the worktree goes.
	require.NoError(t, os.WriteFile(
		filepath.Join(env.workingDir, "feat.go"),
		[]byte("package feat\n// new line\n"), 0o644))
	git("add", "feat.go")
	git("commit", "-m", "merge feat work")
	git("worktree", "remove", "--force", wt)

	dueViaNewerSession(t, env, "s2")
	c := settleTurnRow(t, env, "s1", "t0")

	require.Equal(t, TurnAccepted, c.verdict.String,
		"the merged commit's content carries the fingerprint")
	var after map[string]hunkAfterEntry
	require.NoError(t, json.Unmarshal([]byte(c.hunkAfter), &after))
	require.Equal(t, hunkOK, after[target].State)
	require.True(t, after[target].Merged,
		"content sourced from the default branch is marked")
}

// R8 settle after loss: the worktree is removed unmerged — the work
// survives nowhere, so the row settles diverged, not missing-blind.
func TestWorktreeCapture_SettleDroppedWorktree(t *testing.T) {
	env, wt, git := setupWorktreeRepo(t)
	target := filepath.Join(wt, "feat.go")
	require.NoError(t, os.WriteFile(target, []byte("// new line\n"), 0o644))

	require.NoError(t, env.svc.RecordTurnOutcome(t.Context(), TurnOutcome{
		SessionID:   "s1",
		TurnID:      "t0",
		EditedPaths: []string{target},
		EditHunks:   map[string][]string{target: {"// new line"}},
		HunkAdded:   map[string][]HunkEdit{target: {{New: "// new line"}}},
	}))
	git("worktree", "remove", "--force", wt)

	dueViaNewerSession(t, env, "s2")
	c := settleTurnRow(t, env, "s1", "t0")
	require.Equal(t, TurnRevised, c.verdict.String,
		"unmerged work nowhere durable reads as divergence")
}

// #360: under a shared data_directory a row recorded by worktree B's
// service settles in worktree A's pass — reads route by the row's
// stamped workdir, not the settling service's checkout.
func TestWorktreeCapture_SharedStoreRouting(t *testing.T) {
	env, wt, git := setupWorktreeRepo(t)
	git("worktree", "list") // sanity: the wt is registered

	// Service B is the worktree's own — it records a row whose
	// targets are B-relative.
	svcB := NewService(db.New(env.conn), wt, params.DefaultMemory())
	target := "feat.go"
	require.NoError(t, os.WriteFile(filepath.Join(wt, target),
		[]byte("// from b\n"), 0o644))
	require.NoError(t, svcB.RecordTurnOutcome(t.Context(), TurnOutcome{
		SessionID:   "sb",
		TurnID:      "t0",
		EditedPaths: []string{target},
		EditHunks:   map[string][]string{target: {"// from b"}},
		HunkAdded:   map[string][]HunkEdit{target: {{New: "// from b"}}},
		HunkSpans:   map[string][]HunkSpan{target: {{Start: 0, End: 1}}},
	}))

	// B's row settles under A's pass (env.svc is A's service).
	dueViaNewerSession(t, env, "s2")
	c := settleTurnRow(t, env, "sb", "t0")
	require.Equal(t, TurnAccepted, c.verdict.String,
		"A's pass reads B's checkout via the stamped workdir — "+
			"A's own checkout never had feat.go")

	// And A reading B's file must not leak into A's own rows: a
	// same-named file in A is evidence for A's row only.
	require.NoError(t, env.svc.RecordTurnOutcome(t.Context(), TurnOutcome{
		SessionID:   "sa",
		TurnID:      "t0",
		EditedPaths: []string{"gone.go"},
		EditHunks:   map[string][]string{"gone.go": {"missing"}},
	}))
	dueViaNewerSession(t, env, "s3")
	c = settleTurnRow(t, env, "sa", "t0")
	require.Equal(t, TurnRevised, c.verdict.String,
		"A's own missing file still reads diverged — the stamp "+
			"routes reads, not the project")
}

// Scope is not widened past the repo: an absolute path outside every
// worktree stays out of scope, and sensitive basenames inside a
// worktree still store state only.
func TestWorktreeCapture_GatesHold(t *testing.T) {
	env, wt, _ := setupWorktreeRepo(t)
	outside := filepath.Join(t.TempDir(), "outside.go")
	require.NoError(t, os.WriteFile(outside, []byte("x"), 0o644))
	sensitive := filepath.Join(wt, ".env")

	require.NoError(t, env.svc.RecordTurnOutcome(t.Context(), TurnOutcome{
		SessionID:   "s1",
		TurnID:      "t0",
		EditedPaths: []string{outside, sensitive},
		HunkAdded: map[string][]HunkEdit{
			outside:   {{New: "x"}},
			sensitive: {{New: "KEY=v"}},
		},
	}))
	var m map[string]hunkAddedEntry
	require.NoError(t, json.Unmarshal([]byte(readTurn(t, env, "s1", "t0").hunkAdded), &m))
	require.Equal(t, hunkOutOfWorkspace, m[outside].State,
		"a path under no worktree of this repo remains out of scope")
	require.Equal(t, hunkSensitive, m[sensitive].State,
		"the sensitive gate applies inside worktrees too")
	require.Empty(t, m[sensitive].Edits)
}

// The episode channel shares the gates: a worktree target's label
// hash records and its survival check reads the worktree checkout.
func TestWorktreeCapture_EpisodeLabels(t *testing.T) {
	env, wt, _ := setupWorktreeRepo(t)
	target := filepath.Join(wt, "feat.go")
	require.NoError(t, os.WriteFile(target, []byte("v1\n"), 0o644))

	recordLabeledEpisode(t, env, "s1", "m1", target,
		ReferentEpisodeLabelInputs{AddedLines: []string{"v1"}})
	c := readLabels(t, env, "s1", target)
	require.NotEmpty(t, c.targetHash,
		"the record-time hash reads the worktree file")

	dueViaNewerSession(t, env, "s2")
	require.NoError(t, env.svc.SettleReferentLabels(t.Context(), 10))
	c = readLabels(t, env, "s1", target)
	require.True(t, c.settledAt.Valid)
	require.True(t, c.hashChanged.Valid)
	require.EqualValues(t, 0, c.hashChanged.Int64,
		"the worktree file still carries the added line")
}
