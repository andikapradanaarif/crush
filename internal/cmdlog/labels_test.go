package cmdlog

import (
	"database/sql"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/charmbracelet/crush/internal/db"
	"github.com/charmbracelet/crush/internal/params"
	"github.com/stretchr/testify/require"
)

// labelEnv wires a service whose workingDir holds real files — the
// survival/commit checks are artifact checks, so they need a real
// filesystem (and, for commits, a real repository). conn is kept for
// seeding sessions and asserting label columns directly.
type labelEnv struct {
	svc        Service
	conn       *sql.DB
	workingDir string
}

func setupLabelsTest(t *testing.T) *labelEnv {
	t.Helper()
	conn, err := db.Connect(t.Context(), t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { conn.Close() })
	workingDir := t.TempDir()
	return &labelEnv{
		svc:        NewService(db.New(conn), workingDir, params.DefaultMemory()),
		conn:       conn,
		workingDir: workingDir,
	}
}

// seedLabelSession inserts the session row the test-verdict subquery
// joins on — command_memory's last_session_id only counts toward a
// session's evidence when a sessions row owns it.
func seedLabelSession(t *testing.T, env *labelEnv, id, parentID string) {
	t.Helper()
	var parent sql.NullString
	if parentID != "" {
		parent = sql.NullString{String: parentID, Valid: true}
	}
	_, err := env.conn.ExecContext(t.Context(),
		`INSERT INTO sessions (id, parent_session_id, title, message_count, updated_at, created_at)
		 VALUES (?, ?, 't', 1, 1, 1)`, id, parent)
	require.NoError(t, err)
}

// labelCols reads back one episode's label columns for assertions.
type labelCols struct {
	targetHash  string
	hashChanged sql.NullInt64
	committed   sql.NullInt64
	testsGreen  sql.NullInt64
	wrongTarget int64
	steps       int64
	tokens      int64
	labeledAt   int64
}

func readLabels(t *testing.T, env *labelEnv, sessionID, target string) labelCols {
	t.Helper()
	var c labelCols
	require.NoError(t, env.conn.QueryRowContext(t.Context(),
		`SELECT label_target_hash, label_hash_changed, label_committed,
		        label_tests_green, label_wrong_target, label_steps,
		        label_tokens, labeled_at
		 FROM referent_episodes WHERE session_id = ? AND target = ?`,
		sessionID, target).
		Scan(&c.targetHash, &c.hashChanged, &c.committed, &c.testsGreen,
			&c.wrongTarget, &c.steps, &c.tokens, &c.labeledAt))
	return c
}

func recordLabeledEpisode(t *testing.T, env *labelEnv, session, msg, target string,
	in ReferentEpisodeLabelInputs,
) {
	t.Helper()
	ep := ReferentEpisode{
		Phrase:          "config",
		Target:          target,
		SessionID:       session,
		SourceMessageID: msg,
		ToolCallID:      "call-" + msg,
		Verdict:         ReferentAccepted,
	}
	require.NoError(t, env.svc.RecordReferentEpisode(t.Context(), ep, 5))
	require.NoError(t, env.svc.LabelReferentEpisode(t.Context(), ep, in))
}

func TestLabelReferentEpisode_SnapshotSignals(t *testing.T) {
	env := setupLabelsTest(t)
	target := "internal/config/config.go"
	require.NoError(t, os.MkdirAll(filepath.Join(env.workingDir, "internal/config"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(env.workingDir, target), []byte("package config"), 0o644))

	recordLabeledEpisode(t, env, "s1", "m1", target,
		ReferentEpisodeLabelInputs{Steps: 7, Tokens: 4200})

	c := readLabels(t, env, "s1", target)
	require.NotEmpty(t, c.targetHash, "post-edit baseline must be captured")
	require.False(t, c.hashChanged.Valid, "maturity pass has not run — unchanged is unobserved, not 0")
	require.False(t, c.committed.Valid, "no repo — committed stays NULL, not fabricated 0")
	require.False(t, c.testsGreen.Valid, "no test runs — NULL is unobserved, not green")
	require.EqualValues(t, 0, c.wrongTarget)
	require.EqualValues(t, 7, c.steps)
	require.EqualValues(t, 4200, c.tokens)
	require.Positive(t, c.labeledAt)
}

// The missing file at record time writes no baseline — and the
// maturity pass must then leave the episode alone: no baseline, no
// survival check, and the row must not churn in the pending scan.
func TestLabelReferentEpisode_MissingFileNoBaseline(t *testing.T) {
	env := setupLabelsTest(t)
	recordLabeledEpisode(t, env, "s1", "m1", "gone.go", ReferentEpisodeLabelInputs{})

	c := readLabels(t, env, "s1", "gone.go")
	require.Empty(t, c.targetHash)

	require.NoError(t, env.svc.MatureReferentLabels(t.Context(), 10))
	c = readLabels(t, env, "s1", "gone.go")
	require.False(t, c.hashChanged.Valid,
		"no baseline means the survival check can never run — NULL stays NULL")
}

func TestSessionTestsGreen(t *testing.T) {
	env := setupLabelsTest(t)
	seedLabelSession(t, env, "s1", "")
	seedLabelSession(t, env, "s1-child", "s1")

	// No test runs yet — unobserved.
	recordLabeledEpisode(t, env, "s1", "m1", "x.go", ReferentEpisodeLabelInputs{})
	c := readLabels(t, env, "s1", "x.go")
	require.False(t, c.testsGreen.Valid)

	// A clean test run by the session — observed green.
	env.svc.RecordRun(t.Context(), Run{
		SessionID: "s1", Command: "go test ./...", CWD: env.workingDir,
		Ran: true, ExitCode: 0,
	})
	recordLabeledEpisode(t, env, "s1", "m2", "y.go", ReferentEpisodeLabelInputs{})
	c = readLabels(t, env, "s1", "y.go")
	require.True(t, c.testsGreen.Valid)
	require.EqualValues(t, 1, c.testsGreen.Int64)

	// A failing test by the task child folds into the parent's
	// verdict — session evidence errs inclusive, same as the
	// reconcile edge's session-with-children shape.
	env.svc.RecordRun(t.Context(), Run{
		SessionID: "s1-child", Command: "go test ./pkg", CWD: env.workingDir,
		Ran: true, ExitCode: 1, Stderr: "FAIL",
	})
	recordLabeledEpisode(t, env, "s1", "m3", "z.go", ReferentEpisodeLabelInputs{})
	c = readLabels(t, env, "s1", "z.go")
	require.True(t, c.testsGreen.Valid)
	require.EqualValues(t, 0, c.testsGreen.Int64)
}

// Survival: a file still matching its baseline reads hash_changed=0;
// modified reads 1; deleted reads 1 — the edit did not survive.
func TestMatureReferentLabels_HashCheck(t *testing.T) {
	env := setupLabelsTest(t)
	target := "x.go"
	path := filepath.Join(env.workingDir, target)
	require.NoError(t, os.WriteFile(path, []byte("v1"), 0o644))

	recordLabeledEpisode(t, env, "s1", "m1", target, ReferentEpisodeLabelInputs{})
	require.NoError(t, env.svc.MatureReferentLabels(t.Context(), 10))
	c := readLabels(t, env, "s1", target)
	require.True(t, c.hashChanged.Valid)
	require.EqualValues(t, 0, c.hashChanged.Int64, "file matches baseline — still surviving")

	require.NoError(t, os.WriteFile(path, []byte("v2"), 0o644))
	require.NoError(t, env.svc.MatureReferentLabels(t.Context(), 10))
	c = readLabels(t, env, "s1", target)
	require.EqualValues(t, 1, c.hashChanged.Int64, "diverged from baseline — edit no longer intact")

	// Restoring the baseline content reads surviving again — the
	// signal is latest-state, not latched.
	require.NoError(t, os.WriteFile(path, []byte("v1"), 0o644))
	require.NoError(t, env.svc.MatureReferentLabels(t.Context(), 10))
	c = readLabels(t, env, "s1", target)
	require.EqualValues(t, 0, c.hashChanged.Int64)
}

func TestMatureReferentLabels_DeletedFile(t *testing.T) {
	env := setupLabelsTest(t)
	target := "x.go"
	path := filepath.Join(env.workingDir, target)
	require.NoError(t, os.WriteFile(path, []byte("v1"), 0o644))

	recordLabeledEpisode(t, env, "s1", "m1", target, ReferentEpisodeLabelInputs{})
	require.NoError(t, os.Remove(path))
	require.NoError(t, env.svc.MatureReferentLabels(t.Context(), 10))
	c := readLabels(t, env, "s1", target)
	require.EqualValues(t, 1, c.hashChanged.Int64, "deleted is diverged — the edit did not survive")
}

// Committed is the strongest survival signal — monotone once
// observed, and NULL outside a repository.
func TestMatureReferentLabels_Committed(t *testing.T) {
	env := setupLabelsTest(t)
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	git := func(args ...string) {
		full := append([]string{"-C", env.workingDir}, args...)
		require.NoError(t, exec.CommandContext(t.Context(), "git", full...).Run())
	}
	git("init")
	git("config", "user.email", "t@t")
	git("config", "user.name", "t")
	// Rebuild the service so hasRepo sees the initialized repo, and
	// pin the clock a minute back — the episode's created_at then
	// provably predates the commit that lands in the test, keeping
	// the after-episode comparison off the same-second edge.
	env.svc = NewService(db.New(env.conn), env.workingDir, params.DefaultMemory(),
		WithClock(func() time.Time { return time.Now().Add(-time.Minute) }))

	target := "x.go"
	path := filepath.Join(env.workingDir, target)
	require.NoError(t, os.WriteFile(path, []byte("v1"), 0o644))
	recordLabeledEpisode(t, env, "s1", "m1", target, ReferentEpisodeLabelInputs{})

	require.NoError(t, env.svc.MatureReferentLabels(t.Context(), 10))
	c := readLabels(t, env, "s1", target)
	require.True(t, c.committed.Valid)
	require.EqualValues(t, 0, c.committed.Int64, "inside a repo the check runs — 0, not NULL")

	git("add", target)
	git("commit", "-m", "land the edit")
	require.NoError(t, env.svc.MatureReferentLabels(t.Context(), 10))
	c = readLabels(t, env, "s1", target)
	require.EqualValues(t, 1, c.committed.Int64, "a commit newer than the episode touched the target")

	// Diverging the file later cannot un-observe the landed commit.
	require.NoError(t, os.WriteFile(path, []byte("v2"), 0o644))
	require.NoError(t, env.svc.MatureReferentLabels(t.Context(), 10))
	c = readLabels(t, env, "s1", target)
	require.EqualValues(t, 1, c.committed.Int64, "committed is monotone — a commit cannot un-happen")
	require.EqualValues(t, 1, c.hashChanged.Int64)
}

// A commit older than the episode is history, not the episode's
// landing — the window is created_at, not any-touch-ever.
func TestMatureReferentLabels_OldCommitDoesNotCount(t *testing.T) {
	env := setupLabelsTest(t)
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	git := func(args ...string) {
		full := append([]string{"-C", env.workingDir}, args...)
		require.NoError(t, exec.CommandContext(t.Context(), "git", full...).Run())
	}
	git("init")
	git("config", "user.email", "t@t")
	git("config", "user.name", "t")

	target := "x.go"
	path := filepath.Join(env.workingDir, target)
	require.NoError(t, os.WriteFile(path, []byte("v0"), 0o644))
	git("add", target)
	git("commit", "-m", "pre-episode history")
	// Pin the clock an hour forward — the episode's created_at
	// provably postdates the commit, off the same-second edge.
	env.svc = NewService(db.New(env.conn), env.workingDir, params.DefaultMemory(),
		WithClock(func() time.Time { return time.Now().Add(time.Hour) }))

	// The episode's "edit" lands after the commit.
	require.NoError(t, os.WriteFile(path, []byte("v0-edit"), 0o644))
	recordLabeledEpisode(t, env, "s1", "m1", target, ReferentEpisodeLabelInputs{})
	require.NoError(t, env.svc.MatureReferentLabels(t.Context(), 10))
	c := readLabels(t, env, "s1", target)
	require.EqualValues(t, 0, c.committed.Int64,
		"a commit older than created_at is pre-episode history, not the landing")
}

func TestFileHash_TraversalGuard(t *testing.T) {
	env := setupLabelsTest(t)
	svc := env.svc.(*service)
	require.Empty(t, svc.fileHash("../escape"))
	require.Empty(t, svc.fileHash("/abs/path"))
	require.Empty(t, svc.fileHash(""))
	require.NoError(t, os.WriteFile(filepath.Join(env.workingDir, "x.go"), []byte("v"), 0o644))
	require.NotEmpty(t, svc.fileHash("x.go"))
}

// The maturity pass is project-scoped — another partition's pending
// episodes are not this project's backlog.
func TestMatureReferentLabels_ProjectPartition(t *testing.T) {
	conn, err := db.Connect(t.Context(), t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { conn.Close() })
	q := db.New(conn)
	dir := t.TempDir()
	svcA := NewService(q, dir, params.DefaultMemory(), WithProjectKey("proj-a"))
	svcB := NewService(q, dir, params.DefaultMemory(), WithProjectKey("proj-b"))

	target := "x.go"
	require.NoError(t, os.WriteFile(filepath.Join(dir, target), []byte("v1"), 0o644))
	ep := ReferentEpisode{
		Phrase: "config", Target: target, SessionID: "s1",
		SourceMessageID: "m1", ToolCallID: "c1", Verdict: ReferentAccepted,
	}
	require.NoError(t, svcA.RecordReferentEpisode(t.Context(), ep, 5))
	require.NoError(t, svcA.LabelReferentEpisode(t.Context(), ep, ReferentEpisodeLabelInputs{}))
	ep.SessionID = "s2"
	ep.SourceMessageID = "m2"
	require.NoError(t, svcB.RecordReferentEpisode(t.Context(), ep, 5))
	require.NoError(t, svcB.LabelReferentEpisode(t.Context(), ep, ReferentEpisodeLabelInputs{}))

	// Diverge the file, then run only proj-a's pass — proj-b's row
	// must still read pending (hash_changed NULL, not 1).
	require.NoError(t, os.WriteFile(filepath.Join(dir, target), []byte("v2"), 0o644))
	require.NoError(t, svcA.MatureReferentLabels(t.Context(), 10))

	var changedA, changedB sql.NullInt64
	require.NoError(t, conn.QueryRowContext(t.Context(),
		`SELECT label_hash_changed FROM referent_episodes WHERE session_id = 's1'`).Scan(&changedA))
	require.NoError(t, conn.QueryRowContext(t.Context(),
		`SELECT label_hash_changed FROM referent_episodes WHERE session_id = 's2'`).Scan(&changedB))
	require.EqualValues(t, 1, changedA.Int64)
	require.False(t, changedB.Valid, "proj-b's pending row is not proj-a's backlog")
}
