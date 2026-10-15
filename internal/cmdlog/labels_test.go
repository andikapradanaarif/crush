package cmdlog

import (
	"database/sql"
	"encoding/json"
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
// session's evidence when a sessions row owns it. created_at is 1:
// ancient, so it never triggers the newer-session settlement edge.
func seedLabelSession(t *testing.T, env *labelEnv, id, parentID string) {
	t.Helper()
	seedLabelSessionAt(t, env, id, parentID, 1)
}

// seedLabelSessionAt seeds a session with an explicit created_at in
// seconds — the newer-session settlement trigger needs a row whose
// timestamp provably postdates the episode's.
func seedLabelSessionAt(t *testing.T, env *labelEnv, id, parentID string, createdUnix int64) {
	t.Helper()
	var parent sql.NullString
	if parentID != "" {
		parent = sql.NullString{String: parentID, Valid: true}
	}
	_, err := env.conn.ExecContext(t.Context(),
		`INSERT INTO sessions (id, parent_session_id, title, message_count, updated_at, created_at)
		 VALUES (?, ?, 't', 1, 1, ?)`, id, parent, createdUnix)
	require.NoError(t, err)
}

// dueViaNewerSession makes the episode's settlement due: a session
// starting after the episode's created_at closes the window early.
func dueViaNewerSession(t *testing.T, env *labelEnv, id string) {
	t.Helper()
	seedLabelSessionAt(t, env, id, "", time.Now().Unix()+60)
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
	addedLines  string
	settleAt    int64
	settledAt   sql.NullInt64
}

func readLabels(t *testing.T, env *labelEnv, sessionID, target string) labelCols {
	t.Helper()
	var c labelCols
	require.NoError(t, env.conn.QueryRowContext(t.Context(),
		`SELECT label_target_hash, label_hash_changed, label_committed,
		        label_tests_green, label_wrong_target, label_steps,
		        label_tokens, labeled_at, label_added_lines, settle_at,
		        settled_at
		 FROM referent_episodes WHERE session_id = ? AND target = ?`,
		sessionID, target).
		Scan(&c.targetHash, &c.hashChanged, &c.committed, &c.testsGreen,
			&c.wrongTarget, &c.steps, &c.tokens, &c.labeledAt,
			&c.addedLines, &c.settleAt, &c.settledAt))
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
		ReferentEpisodeLabelInputs{Steps: 7, Tokens: 4200, AddedLines: []string{"x = 1"}})

	c := readLabels(t, env, "s1", target)
	require.NotEmpty(t, c.targetHash, "post-edit baseline must be captured")
	require.False(t, c.hashChanged.Valid, "settlement has not run — unchanged is unobserved, not 0")
	require.False(t, c.committed.Valid, "no repo — committed stays NULL, not fabricated 0")
	require.False(t, c.testsGreen.Valid, "no test runs — NULL is unobserved, not green")
	require.EqualValues(t, 0, c.wrongTarget)
	require.EqualValues(t, 7, c.steps)
	require.EqualValues(t, 4200, c.tokens)
	require.Positive(t, c.labeledAt)
	var added []string
	require.NoError(t, json.Unmarshal([]byte(c.addedLines), &added))
	require.Equal(t, []string{"x = 1"}, added, "the survival fingerprint stamps at record time")
	require.Equal(t, c.labeledAt+settleWindowMillis, c.settleAt,
		"the window's outer edge is created + 24h")
	require.False(t, c.settledAt.Valid, "a fresh row opens unsettled")
}

// Without the window closing, settlement must not touch the row:
// fixed-window means the label waits for its observation close.
func TestSettleReferentLabels_NotDue(t *testing.T) {
	env := setupLabelsTest(t)
	target := "x.go"
	require.NoError(t, os.WriteFile(filepath.Join(env.workingDir, target), []byte("v1"), 0o644))

	recordLabeledEpisode(t, env, "s1", "m1", target,
		ReferentEpisodeLabelInputs{AddedLines: []string{"v1"}})
	require.NoError(t, env.svc.SettleReferentLabels(t.Context(), 10))
	c := readLabels(t, env, "s1", target)
	require.False(t, c.settledAt.Valid, "window still open — the row must not settle early")
	require.False(t, c.hashChanged.Valid)
}

// A newer session in the project closes the window: the row settles
// once and freezes — later state cannot rewrite the verdict.
func TestSettleReferentLabels_NewerSessionSettlesOnce(t *testing.T) {
	env := setupLabelsTest(t)
	target := "x.go"
	path := filepath.Join(env.workingDir, target)
	require.NoError(t, os.WriteFile(path, []byte("v1"), 0o644))

	recordLabeledEpisode(t, env, "s1", "m1", target,
		ReferentEpisodeLabelInputs{AddedLines: []string{"v1"}})
	dueViaNewerSession(t, env, "s2")

	require.NoError(t, env.svc.SettleReferentLabels(t.Context(), 10))
	c := readLabels(t, env, "s1", target)
	require.True(t, c.settledAt.Valid, "a newer session closes the window")
	require.True(t, c.hashChanged.Valid)
	require.EqualValues(t, 0, c.hashChanged.Int64, "added lines still present — survived")

	// Diverge the file and re-run: a frozen label does not
	// re-derive — that is the whole point of settlement.
	require.NoError(t, os.WriteFile(path, []byte("unrelated"), 0o644))
	require.NoError(t, env.svc.SettleReferentLabels(t.Context(), 10))
	c = readLabels(t, env, "s1", target)
	require.EqualValues(t, 0, c.hashChanged.Int64, "settled labels freeze — divergence after freeze cannot rewrite them")
}

// The +24h cap is the second trigger: no newer session needed once
// the window's outer edge passes.
func TestSettleReferentLabels_CapSettles(t *testing.T) {
	env := setupLabelsTest(t)
	target := "x.go"
	require.NoError(t, os.WriteFile(filepath.Join(env.workingDir, target), []byte("v1"), 0o644))

	recordLabeledEpisode(t, env, "s1", "m1", target,
		ReferentEpisodeLabelInputs{AddedLines: []string{"v1"}})

	// Advance the service clock past the cap.
	now := time.Now()
	env.svc = NewService(db.New(env.conn), env.workingDir, params.DefaultMemory(),
		WithClock(func() time.Time { return now.Add(25 * time.Hour) }))
	require.NoError(t, env.svc.SettleReferentLabels(t.Context(), 10))
	c := readLabels(t, env, "s1", target)
	require.True(t, c.settledAt.Valid, "the +24h cap settles idle rows with no newer session")
	require.EqualValues(t, 0, c.hashChanged.Int64)
}

// Rows recorded before the settle_at column carry 0: they fall back
// to created_at + 24h and still terminate.
func TestSettleReferentLabels_LegacyRowFallsBack(t *testing.T) {
	env := setupLabelsTest(t)
	target := "x.go"
	require.NoError(t, os.WriteFile(filepath.Join(env.workingDir, target), []byte("v1"), 0o644))

	recordLabeledEpisode(t, env, "s1", "m1", target,
		ReferentEpisodeLabelInputs{AddedLines: []string{"v1"}})
	// Simulate the pre-#340 shape: no settle_at, no fingerprint.
	_, err := env.conn.ExecContext(t.Context(),
		`UPDATE referent_episodes SET settle_at = 0, label_added_lines = ''
		 WHERE session_id = 's1'`)
	require.NoError(t, err)

	now := time.Now()
	env.svc = NewService(db.New(env.conn), env.workingDir, params.DefaultMemory(),
		WithClock(func() time.Time { return now.Add(25 * time.Hour) }))
	require.NoError(t, env.svc.SettleReferentLabels(t.Context(), 10))
	c := readLabels(t, env, "s1", target)
	require.True(t, c.settledAt.Valid)
	require.False(t, c.hashChanged.Valid,
		"no fingerprint ever existed — survival stays NULL, not a fabricated verdict")
}

// Hunk survival: the file diverged only when fewer than half the
// recorded added lines remain — an unrelated edit elsewhere is not a
// revision.
func TestSettleReferentLabels_HunkSurvival(t *testing.T) {
	env := setupLabelsTest(t)
	target := "x.go"
	path := filepath.Join(env.workingDir, target)

	added := []string{"func newThing() {", "return 1", "}"}
	require.NoError(t, os.WriteFile(path,
		[]byte("package x\n\nfunc newThing() {\nreturn 1\n}\n"), 0o644))
	recordLabeledEpisode(t, env, "s1", "m1", target,
		ReferentEpisodeLabelInputs{AddedLines: added})
	dueViaNewerSession(t, env, "s2")

	// Unrelated edit appended: all fingerprint lines still present.
	require.NoError(t, os.WriteFile(path,
		[]byte("package x\n\nfunc newThing() {\nreturn 1\n}\n\nfunc other() {}\n"), 0o644))
	require.NoError(t, env.svc.SettleReferentLabels(t.Context(), 10))
	c := readLabels(t, env, "s1", target)
	require.True(t, c.settledAt.Valid)
	require.EqualValues(t, 0, c.hashChanged.Int64,
		"unrelated edits elsewhere in the file are not divergence")
}

// More than half the added lines gone reads diverged.
func TestSettleReferentLabels_HunkDiverged(t *testing.T) {
	env := setupLabelsTest(t)
	target := "x.go"
	require.NoError(t, os.WriteFile(filepath.Join(env.workingDir, target),
		[]byte("package x\n"), 0o644))

	recordLabeledEpisode(t, env, "s1", "m1", target,
		ReferentEpisodeLabelInputs{AddedLines: []string{"a()", "b()", "c()"}})
	dueViaNewerSession(t, env, "s2")

	require.NoError(t, env.svc.SettleReferentLabels(t.Context(), 10))
	c := readLabels(t, env, "s1", target)
	require.EqualValues(t, 1, c.hashChanged.Int64,
		"all three added lines absent — the edit did not survive")
}

func TestSettleReferentLabels_DeletedFile(t *testing.T) {
	env := setupLabelsTest(t)
	target := "x.go"
	path := filepath.Join(env.workingDir, target)
	require.NoError(t, os.WriteFile(path, []byte("v1"), 0o644))

	recordLabeledEpisode(t, env, "s1", "m1", target,
		ReferentEpisodeLabelInputs{AddedLines: []string{"v1"}})
	require.NoError(t, os.Remove(path))
	dueViaNewerSession(t, env, "s2")
	require.NoError(t, env.svc.SettleReferentLabels(t.Context(), 10))
	c := readLabels(t, env, "s1", target)
	require.EqualValues(t, 1, c.hashChanged.Int64, "deleted is diverged — the edit did not survive")
}

// Settlement reads the session's test state once — a test run
// landing between record and settle is the session-end verdict the
// label was waiting for.
func TestSettleReferentLabels_TestsGreenAtSettle(t *testing.T) {
	env := setupLabelsTest(t)
	seedLabelSession(t, env, "s1", "")
	target := "x.go"
	require.NoError(t, os.WriteFile(filepath.Join(env.workingDir, target), []byte("v1"), 0o644))

	recordLabeledEpisode(t, env, "s1", "m1", target, ReferentEpisodeLabelInputs{})
	c := readLabels(t, env, "s1", target)
	require.False(t, c.testsGreen.Valid, "no test observed at record time — NULL")

	env.svc.RecordRun(t.Context(), Run{
		SessionID: "s1", Command: "go test ./...", CWD: env.workingDir,
		Ran: true, ExitCode: 0,
	})
	dueViaNewerSession(t, env, "s2")
	require.NoError(t, env.svc.SettleReferentLabels(t.Context(), 10))
	c = readLabels(t, env, "s1", target)
	require.True(t, c.testsGreen.Valid)
	require.EqualValues(t, 1, c.testsGreen.Int64,
		"the verdict read at settle is the session-end state")
}

// A commit the harness itself ran is not acceptance evidence: the
// command_memory row for the git-commit run excludes it.
func TestSettleReferentLabels_AgentCommitExcluded(t *testing.T) {
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
	// Pin the episode clock a minute back so the commit provably
	// postdates created_at.
	env.svc = NewService(db.New(env.conn), env.workingDir, params.DefaultMemory(),
		WithClock(func() time.Time { return time.Now().Add(-time.Minute) }))

	target := "x.go"
	path := filepath.Join(env.workingDir, target)
	require.NoError(t, os.WriteFile(path, []byte("v1"), 0o644))
	recordLabeledEpisode(t, env, "s1", "m1", target,
		ReferentEpisodeLabelInputs{AddedLines: []string{"v1"}})
	seedLabelSession(t, env, "s1", "")

	// The agent commits its own work — recorded through a
	// real-clock service so the ledger stamps wall time.
	svcReal := NewService(db.New(env.conn), env.workingDir, params.DefaultMemory())
	git("add", target)
	git("commit", "-m", "agent commit")
	svcReal.RecordRun(t.Context(), Run{
		SessionID: "s1", Command: "git commit -m \"agent commit\"",
		CWD: env.workingDir, Ran: true, ExitCode: 0,
	})

	dueViaNewerSession(t, env, "s2")
	require.NoError(t, env.svc.SettleReferentLabels(t.Context(), 10))
	c := readLabels(t, env, "s1", target)
	require.True(t, c.committed.Valid)
	require.EqualValues(t, 0, c.committed.Int64,
		"a commit the harness ran is agent-made, not user acceptance")

	// A commit with no ledger run IS external evidence. Pin its
	// committer date 30s out so it lands outside the recorded run's
	// skew bound but still inside the window s4 closes at +90s.
	recordLabeledEpisode(t, env, "s3", "m3", target,
		ReferentEpisodeLabelInputs{AddedLines: []string{"v1"}})
	require.NoError(t, os.WriteFile(path, []byte("v1\n// external touch"), 0o644))
	git("add", target)
	// UTC before formatting — "Z" on local wall time dates the
	// commit by the machine's offset, outside the test window.
	future := time.Now().Add(30 * time.Second).UTC().Format("2006-01-02T15:04:05Z")
	cmd := exec.CommandContext(t.Context(), "git", "-C", env.workingDir,
		"commit", "-m", "user commit")
	cmd.Env = append(os.Environ(), "GIT_COMMITTER_DATE="+future)
	require.NoError(t, cmd.Run())
	seedLabelSessionAt(t, env, "s4", "", time.Now().Unix()+90)
	require.NoError(t, env.svc.SettleReferentLabels(t.Context(), 10))
	c = readLabels(t, env, "s3", target)
	require.EqualValues(t, 1, c.committed.Int64,
		"an unrecorded commit inside the window is external evidence")
}

// A commit older than the episode is history — the window's lower
// edge is created_at.
func TestSettleReferentLabels_OldCommitDoesNotCount(t *testing.T) {
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
	env.svc = NewService(db.New(env.conn), env.workingDir, params.DefaultMemory(),
		WithClock(func() time.Time { return time.Now().Add(time.Hour) }))

	require.NoError(t, os.WriteFile(path, []byte("v0-edit"), 0o644))
	recordLabeledEpisode(t, env, "s1", "m1", target,
		ReferentEpisodeLabelInputs{AddedLines: []string{"v0-edit"}})
	// A session even newer than the pinned clock closes the window.
	seedLabelSessionAt(t, env, "s2", "", time.Now().Add(2*time.Hour).Unix())
	require.NoError(t, env.svc.SettleReferentLabels(t.Context(), 10))
	c := readLabels(t, env, "s1", target)
	require.EqualValues(t, 0, c.committed.Int64,
		"a commit older than created_at is pre-episode history, not the landing")
}

// Divergence beats commit (#340): a settled row whose edit diverged
// reads as failure even when a commit landed — revised-then-
// committed is a revision.
func TestReferentEpisodeOutcome_DivergenceBeatsCommit(t *testing.T) {
	t.Parallel()
	require.False(t, ReferentEpisodeOutcome(
		ReferentAccepted,
		sql.NullInt64{Int64: 1, Valid: true},
		sql.NullInt64{Int64: 1, Valid: true}),
		"diverged + committed is a revision, not an acceptance")
	require.True(t, ReferentEpisodeOutcome(
		ReferentAccepted,
		sql.NullInt64{Int64: 1, Valid: true},
		sql.NullInt64{Int64: 0, Valid: true}),
		"survived + committed is acceptance")
	require.True(t, ReferentEpisodeOutcome(
		ReferentRevised,
		sql.NullInt64{Int64: 1, Valid: true},
		sql.NullInt64{}),
		"committed with no survival read falls to the cue-independent commit signal")
	require.False(t, ReferentEpisodeOutcome(
		ReferentRevised, sql.NullInt64{}, sql.NullInt64{Int64: 1, Valid: true}),
		"diverged alone is failure regardless of cue")
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

// The settle pass is project-scoped — another partition's due rows
// are not this project's backlog.
func TestSettleReferentLabels_ProjectPartition(t *testing.T) {
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
	in := ReferentEpisodeLabelInputs{AddedLines: []string{"v1"}}
	require.NoError(t, svcA.RecordReferentEpisode(t.Context(), ep, 5))
	require.NoError(t, svcA.LabelReferentEpisode(t.Context(), ep, in))
	ep.SessionID = "s2"
	ep.SourceMessageID = "m2"
	require.NoError(t, svcB.RecordReferentEpisode(t.Context(), ep, 5))
	require.NoError(t, svcB.LabelReferentEpisode(t.Context(), ep, in))

	// Both due via a shared newer session; only proj-a settles.
	seedLabelSessionAt(t, &labelEnv{conn: conn}, "s3", "", time.Now().Unix()+60)
	require.NoError(t, svcA.SettleReferentLabels(t.Context(), 10))

	var settledA, settledB sql.NullInt64
	require.NoError(t, conn.QueryRowContext(t.Context(),
		`SELECT settled_at FROM referent_episodes WHERE session_id = 's1'`).Scan(&settledA))
	require.NoError(t, conn.QueryRowContext(t.Context(),
		`SELECT settled_at FROM referent_episodes WHERE session_id = 's2'`).Scan(&settledB))
	require.True(t, settledA.Valid)
	require.False(t, settledB.Valid, "proj-b's due row is not proj-a's backlog")
}
