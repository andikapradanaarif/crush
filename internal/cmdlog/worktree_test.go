package cmdlog

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charmbracelet/crush/internal/db"
	"github.com/charmbracelet/crush/internal/filepathext"
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
	require.Equal(t, TurnUnknown, c.verdict.String,
		"unmerged work nowhere durable reads as divergence — "+
			"unknown without a revised cue")
}

// #466: a target under a different repository records cross_repo —
// a designed abstention the fleet share must not price as failure —
// while a path under no repository at all keeps out_of_workspace.
func TestCrossRepoState(t *testing.T) {
	env, _, _ := setupWorktreeRepo(t)

	// A second, unrelated repository with a real file inside.
	other := filepath.Join(t.TempDir(), "other-repo")
	gitInOther := func(args ...string) {
		full := append([]string{"-C", other}, args...)
		require.NoError(t, exec.CommandContext(t.Context(), "git", full...).Run())
	}
	require.NoError(t, os.MkdirAll(other, 0o755))
	gitInOther("init", "-b", "main")
	gitInOther("config", "user.email", "t@t")
	gitInOther("config", "user.name", "t")
	foreign := filepath.Join(other, "f.go")
	require.NoError(t, os.WriteFile(foreign, []byte("package f\n"), 0o644))
	gitInOther("add", ".")
	gitInOther("commit", "-m", "base")

	// A path under no repository — a plain scratch dir.
	unscoped := filepath.Join(t.TempDir(), "scratch.go")
	require.NoError(t, os.WriteFile(unscoped, []byte("x"), 0o644))

	require.NoError(t, env.svc.RecordTurnOutcome(t.Context(), TurnOutcome{
		SessionID:   "s1",
		TurnID:      "t0",
		EditedPaths: []string{foreign, unscoped},
		HunkAdded: map[string][]HunkEdit{
			foreign:  {{New: "package f"}},
			unscoped: {{New: "x"}},
		},
	}))
	var m map[string]hunkAddedEntry
	require.NoError(t, json.Unmarshal([]byte(readTurn(t, env, "s1", "t0").hunkAdded), &m))
	require.Equal(t, hunkCrossRepo, m[foreign].State,
		"a path inside another repository is a designed abstention — "+
			"that repo's partition owns its evidence")
	require.Equal(t, hunkOutOfWorkspace, m[unscoped].State,
		"a path under no repository keeps the plain abstention")
	require.Empty(t, m[foreign].Edits, "abstentions store no content either way")
}

// #466: the settle read names the same abstention for a foreign-repo
// path — escaped, not missing — while an unresolvable relative
// target (no stamped workdir) abstains as out_of_workspace rather
// than probing the process CWD.
func TestCrossRepoSettleRead(t *testing.T) {
	env, _, _ := setupWorktreeRepo(t)

	other := filepath.Join(t.TempDir(), "other-repo")
	gitInOther := func(args ...string) {
		full := append([]string{"-C", other}, args...)
		require.NoError(t, exec.CommandContext(t.Context(), "git", full...).Run())
	}
	require.NoError(t, os.MkdirAll(other, 0o755))
	gitInOther("init", "-b", "main")
	foreign := filepath.Join(other, "f.go")
	require.NoError(t, os.WriteFile(foreign, []byte("package f\n"), 0o644))

	svc := env.svc.(*service)
	v := svc.captureView(t.Context())
	fr := svc.readHunkSource(t.Context(), foreign, env.workingDir, v, "")
	require.Equal(t, hunkCrossRepo, fr.state,
		"a foreign-repo path abstains at settle like it did at record")
	require.True(t, fr.escaped,
		"escaped skips the missing-means-diverged arm")

	require.Equal(t, hunkOutOfWorkspace,
		svc.outOfScopeState(t.Context(), v, ""),
		"an empty resolved path never probes the process CWD")
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
	require.Equal(t, TurnUnknown, c.verdict.String,
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

// #360 review: the placeholder-repair upsert must re-stamp workdir.
// Under a shared store, A's backfill can mint a placeholder for B's
// in-flight turn — carrying A's workdir. B's live record refreshes
// the evidence columns, and the stamp must move with them or settle
// resolves B-relative spellings against A's checkout — the
// wrong-checkout read this fix exists to remove.
func TestWorktreeCapture_PlaceholderRepairRestampsWorkdir(t *testing.T) {
	env, wt, _ := setupWorktreeRepo(t)

	// A's backfill mints the placeholder for B's in-flight turn.
	require.NoError(t, env.svc.RecordTurnOutcome(t.Context(), TurnOutcome{
		SessionID:   "sb",
		TurnID:      "t0",
		EditedPaths: []string{"feat.go"},
		EditHunks:   map[string][]string{"feat.go": {"// from b"}},
	}))

	// B's own record lands under the same (session, turn) — the
	// upsert repairs evidence and re-stamps the recorder's
	// checkout.
	svcB := NewService(db.New(env.conn), wt, params.DefaultMemory())
	require.NoError(t, os.WriteFile(filepath.Join(wt, "feat.go"),
		[]byte("// from b\n"), 0o644))
	require.NoError(t, svcB.RecordTurnOutcome(t.Context(), TurnOutcome{
		SessionID:   "sb",
		TurnID:      "t0",
		EditedPaths: []string{"feat.go"},
		EditHunks:   map[string][]string{"feat.go": {"// from b"}},
		HunkAdded:   map[string][]HunkEdit{"feat.go": {{New: "// from b"}}},
		HunkSpans:   map[string][]HunkSpan{"feat.go": {{Start: 0, End: 1}}},
	}))

	var workdir string
	require.NoError(t, env.conn.QueryRowContext(t.Context(),
		`SELECT workdir FROM turn_outcomes
		 WHERE session_id = 'sb' AND turn_id = 't0'`).Scan(&workdir))
	require.Equal(t, filepathext.Canonical(wt), filepathext.Canonical(workdir),
		"the repair re-stamps the recording checkout — the excluded "+
			"row's spellings resolve against it")

	// A's settle pass reads B's checkout via the re-stamped
	// workdir; feat.go exists only there.
	dueViaNewerSession(t, env, "s2")
	c := settleTurnRow(t, env, "sb", "t0")
	require.Equal(t, TurnAccepted, c.verdict.String,
		"the re-stamped workdir routes the read to B")
}

// The merged fallback returns exact bytes — hashing and window
// coordinates run on them, so a blob with leading blank lines must
// not come back whitespace-trimmed (review: gitOut's TrimSpace
// corrupted both consumers).
func TestWorktreeCapture_MergedContentUntrimmed(t *testing.T) {
	env, _, git := setupWorktreeRepo(t)
	raw := "\n\n\npackage lead\n\n// code\n"
	require.NoError(t, os.WriteFile(
		filepath.Join(env.workingDir, "lead.go"), []byte(raw), 0o644))
	git("add", "lead.go")
	git("commit", "-m", "lead")

	require.NoError(t, env.svc.RecordReferentEpisode(t.Context(), ReferentEpisode{
		Phrase: "it", Target: "lead.go", SessionID: "s1",
		SourceMessageID: "m1", ToolCallID: "c1", Verdict: ReferentAccepted,
	}, 0))
	// The file leaves the checkout uncommitted — the label pass's
	// hash read falls to the merged blob.
	require.NoError(t, os.Remove(filepath.Join(env.workingDir, "lead.go")))
	require.NoError(t, env.svc.LabelReferentEpisode(t.Context(), ReferentEpisode{
		Phrase: "it", Target: "lead.go", SessionID: "s1",
		SourceMessageID: "m1", ToolCallID: "c1", Verdict: ReferentAccepted,
	}, ReferentEpisodeLabelInputs{}))

	sum := sha256.Sum256([]byte(raw))
	require.Equal(t, hex.EncodeToString(sum[:]),
		readLabels(t, env, "s1", "lead.go").targetHash,
		"the merged fallback hashes the blob's exact bytes")
}

// A worktree nested INSIDE the main checkout (repo/wt) resolves
// under the outer root once removed — the target wt/f.go would read
// main:wt/f.go (miss → diverged) where the stamped f.go hits. The
// recorded repo path must win the merged arm, matching settleTurn's
// hint-first rule (review).
func TestWorktreeCapture_NestedWorktreeEpisode(t *testing.T) {
	env, _, git := setupWorktreeRepo(t)
	nested := filepath.Join(env.workingDir, "wt")
	git("worktree", "add", "-b", "nested", "wt")
	require.NoError(t, os.WriteFile(filepath.Join(nested, "f.go"),
		[]byte("v1\n"), 0o644))

	recordLabeledEpisode(t, env, "s1", "m1", "wt/f.go",
		ReferentEpisodeLabelInputs{AddedLines: []string{"v1"}})

	// The work merges to the default branch, then the nested
	// worktree is removed — wt/f.go still resolves, but under the
	// main root now; the stamped f.go is the durable address.
	require.NoError(t, os.WriteFile(filepath.Join(env.workingDir, "f.go"),
		[]byte("v1\n"), 0o644))
	git("add", "f.go")
	git("commit", "-m", "merge nested work")
	git("worktree", "remove", "--force", "wt")

	dueViaNewerSession(t, env, "s2")
	require.NoError(t, env.svc.SettleReferentLabels(t.Context(), 10))
	c := readLabels(t, env, "s1", "wt/f.go")
	require.True(t, c.hashChanged.Valid)
	require.EqualValues(t, 0, c.hashChanged.Int64,
		"the stamped repo path f.go reads the merged blob — "+
			"computing wt/f.go under the outer root would miss")
}

// The merged fallback obeys the same MaxHunkReadBytes stat gate as
// the fs arm — a giant blob is too_large, not an unbounded read
// feeding the capture scan (review).
func TestWorktreeCapture_MergedTooLarge(t *testing.T) {
	env, wt, git := setupWorktreeRepo(t)
	target := filepath.Join(wt, "big.go")
	big := "// " + strings.Repeat("x", MaxHunkReadBytes) + "\n"
	require.NoError(t, os.WriteFile(target, []byte(big), 0o644))

	require.NoError(t, env.svc.RecordTurnOutcome(t.Context(), TurnOutcome{
		SessionID:   "s1",
		TurnID:      "t0",
		EditedPaths: []string{target},
		HunkAdded:   map[string][]HunkEdit{target: {{New: "// big"}}},
		HunkSpans:   map[string][]HunkSpan{target: {{Start: 0, End: 1}}},
	}))
	require.NoError(t, os.WriteFile(
		filepath.Join(env.workingDir, "big.go"), []byte(big), 0o644))
	git("add", "big.go")
	git("commit", "-m", "big")
	git("worktree", "remove", "--force", wt)

	dueViaNewerSession(t, env, "s2")
	c := settleTurnRow(t, env, "s1", "t0")
	var after map[string]hunkAfterEntry
	require.NoError(t, json.Unmarshal([]byte(c.hunkAfter), &after))
	require.Equal(t, hunkTooLarge, after[target].State,
		"an over-cap merged blob reports too_large like the fs arm")
	require.Empty(t, after[target].Windows)
}
