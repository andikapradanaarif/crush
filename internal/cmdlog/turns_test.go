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

// turnCols reads back one turn row for assertions.
type turnCols struct {
	verdict   sql.NullString
	settledAt sql.NullInt64
	settleAt  int64
}

func readTurn(t *testing.T, env *labelEnv, sessionID, turnID string) turnCols {
	t.Helper()
	var c turnCols
	require.NoError(t, env.conn.QueryRowContext(t.Context(),
		`SELECT verdict, settled_at, settle_at FROM turn_outcomes
		 WHERE session_id = ? AND turn_id = ?`, sessionID, turnID).
		Scan(&c.verdict, &c.settledAt, &c.settleAt))
	return c
}

func settleTurnRow(t *testing.T, env *labelEnv, sessionID, turnID string) turnCols {
	t.Helper()
	require.NoError(t, env.svc.SettleTurnOutcomes(t.Context(), 10))
	return readTurn(t, env, sessionID, turnID)
}

func TestRecordTurnOutcome_Idempotent(t *testing.T) {
	env := setupLabelsTest(t)
	o := TurnOutcome{
		SessionID: "s1", TurnID: "t0",
		EditedPaths: []string{"x.go"},
		EditHunks:   map[string][]string{"x.go": {"v1"}},
		Commands:    []string{"go test ./..."},
		NextVerdict: ReferentAccepted,
	}
	require.NoError(t, env.svc.RecordTurnOutcome(t.Context(), o))
	require.NoError(t, env.svc.RecordTurnOutcome(t.Context(), o),
		"INSERT OR IGNORE — a re-derivation re-records nothing")

	has, err := env.svc.HasTurnOutcome(t.Context(), "s1", "t0")
	require.NoError(t, err)
	require.True(t, has)
	has, err = env.svc.HasTurnOutcome(t.Context(), "s1", "t1")
	require.NoError(t, err)
	require.False(t, has)

	var n int64
	require.NoError(t, env.conn.QueryRowContext(t.Context(),
		`SELECT COUNT(*) FROM turn_outcomes`).Scan(&n))
	require.EqualValues(t, 1, n)

	c := readTurn(t, env, "s1", "t0")
	require.False(t, c.settledAt.Valid, "a fresh row opens unsettled")
	require.False(t, c.verdict.Valid, "the verdict is the settlement's to write")
}

func TestSettleTurnOutcomes_NotDue(t *testing.T) {
	env := setupLabelsTest(t)
	require.NoError(t, env.svc.RecordTurnOutcome(t.Context(), TurnOutcome{
		SessionID: "s1", TurnID: "t0", Commands: []string{"ls"},
	}))
	require.NoError(t, env.svc.SettleTurnOutcomes(t.Context(), 10))
	c := readTurn(t, env, "s1", "t0")
	require.False(t, c.settledAt.Valid, "window open — no settlement")
}

// Verdict rules, one row each: read-only is unknown; a revision cue
// is revised even when nothing else fired; surviving edits accept;
// diverged edits revise; red session tests fail; a commit-evidenced
// edit accepts; frozen rows never re-derive.
func TestSettleTurnOutcomes_VerdictMatrix(t *testing.T) {
	env := setupLabelsTest(t)
	dir := env.workingDir

	write := func(p, content string) {
		require.NoError(t, os.WriteFile(filepath.Join(dir, p), []byte(content), 0o644))
	}
	write("kept.go", "v1")
	write("gone.go", "v1")
	require.NoError(t, os.Remove(filepath.Join(dir, "gone.go")))

	rows := []TurnOutcome{
		{SessionID: "s1", TurnID: "t0"}, // nothing produced — unknown
		{
			SessionID: "s1", TurnID: "t1", // revision cue beats survival
			EditedPaths: []string{"kept.go"}, EditHunks: map[string][]string{"kept.go": {"v1"}},
			NextVerdict: ReferentRevised,
		},
		{
			SessionID: "s1", TurnID: "t2", // surviving edit accepts
			EditedPaths: []string{"kept.go"}, EditHunks: map[string][]string{"kept.go": {"v1"}},
			NextVerdict: ReferentAccepted,
		},
		{
			SessionID: "s1", TurnID: "t3", // diverged edit revises
			EditedPaths: []string{"gone.go"}, EditHunks: map[string][]string{"gone.go": {"v1"}},
			NextVerdict: ReferentAccepted,
		},
		{SessionID: "s1", TurnID: "t4", // command-only in a green session
			Commands: []string{"go build ./..."}, NextVerdict: ReferentAccepted},
	}
	for _, o := range rows {
		require.NoError(t, env.svc.RecordTurnOutcome(t.Context(), o))
	}

	// s1's green test run is the session-end state every row reads.
	seedLabelSession(t, env, "s1", "")
	env.svc.RecordRun(t.Context(), Run{
		SessionID: "s1", Command: "go test ./...", CWD: dir,
		Ran: true, ExitCode: 0,
	})

	dueViaNewerSession(t, env, "s2")
	want := map[string]string{
		"t0": TurnUnknown, "t1": TurnRevised, "t2": TurnAccepted,
		"t3": TurnRevised, "t4": TurnAccepted,
	}
	for id, v := range want {
		require.Equal(t, v, settleTurnRow(t, env, "s1", id).verdict.String,
			"turn %s", id)
	}
}

// A red session-end test state fails a turn whose edits survived.
func TestSettleTurnOutcomes_RedTestsFail(t *testing.T) {
	env := setupLabelsTest(t)
	require.NoError(t, os.WriteFile(filepath.Join(env.workingDir, "x.go"), []byte("v1"), 0o644))
	seedLabelSession(t, env, "s1", "")
	env.svc.RecordRun(t.Context(), Run{
		SessionID: "s1", Command: "go test ./...", CWD: env.workingDir,
		Ran: true, ExitCode: 1, Stderr: "FAIL",
	})
	require.NoError(t, env.svc.RecordTurnOutcome(t.Context(), TurnOutcome{
		SessionID: "s1", TurnID: "t0",
		EditedPaths: []string{"x.go"}, EditHunks: map[string][]string{"x.go": {"v1"}},
		NextVerdict: ReferentAccepted,
	}))
	dueViaNewerSession(t, env, "s2")
	require.Equal(t, TurnFailed, settleTurnRow(t, env, "s1", "t0").verdict.String,
		"survived edits in a red session are failed, not accepted")
}

// Once settled the verdict freezes: later divergence cannot rewrite
// it — the same fixed-window contract episodes hold.
func TestSettleTurnOutcomes_Frozen(t *testing.T) {
	env := setupLabelsTest(t)
	path := filepath.Join(env.workingDir, "x.go")
	require.NoError(t, os.WriteFile(path, []byte("v1"), 0o644))
	require.NoError(t, env.svc.RecordTurnOutcome(t.Context(), TurnOutcome{
		SessionID: "s1", TurnID: "t0",
		EditedPaths: []string{"x.go"}, EditHunks: map[string][]string{"x.go": {"v1"}},
	}))
	dueViaNewerSession(t, env, "s2")
	require.Equal(t, TurnAccepted, settleTurnRow(t, env, "s1", "t0").verdict.String)

	require.NoError(t, os.Remove(path))
	require.NoError(t, env.svc.SettleTurnOutcomes(t.Context(), 10))
	require.Equal(t, TurnAccepted, readTurn(t, env, "s1", "t0").verdict.String,
		"a settled verdict is frozen — deletion after the window cannot rewrite it")
}

// A row stamped with its real (past) turn time — the backfill shape —
// reads commit evidence inside its true window, not record-time's.
func TestSettleTurnOutcomes_RealTimestampWindow(t *testing.T) {
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
	// hasRepo is detected at construction — rebuild over the repo.
	env.svc = NewService(db.New(env.conn), env.workingDir, params.DefaultMemory())

	// The turn happened two hours ago; the backfill records it now.
	turnAt := time.Now().Add(-2 * time.Hour)
	require.NoError(t, os.WriteFile(filepath.Join(env.workingDir, "x.go"), []byte("v1"), 0o644))
	git("add", "x.go")
	// An unrecorded commit 30min after the turn — inside the window
	// the next session (+60s) will close, outside any record-time
	// window (which would start at now, after this commit).
	commitAt := turnAt.Add(30 * time.Minute).UTC().Format("2006-01-02T15:04:05Z")
	cmd := exec.CommandContext(t.Context(), "git", "-C", env.workingDir,
		"commit", "-m", "external commit")
	cmd.Env = append(os.Environ(), "GIT_COMMITTER_DATE="+commitAt)
	require.NoError(t, cmd.Run())

	require.NoError(t, env.svc.RecordTurnOutcome(t.Context(), TurnOutcome{
		SessionID: "s1", TurnID: "t0",
		EditedPaths: []string{"x.go"},
		// No fingerprint — survival cannot run, so commit evidence
		// alone decides; without the real window this settles unknown.
		CreatedAt:   turnAt.UnixMilli(),
		NextVerdict: ReferentAccepted,
	}))
	dueViaNewerSession(t, env, "s2")
	require.Equal(t, TurnAccepted, settleTurnRow(t, env, "s1", "t0").verdict.String,
		"the real turn timestamp keeps historical commit evidence reachable")
}

// The +24h cap settles without any newer session.
func TestSettleTurnOutcomes_CapSettles(t *testing.T) {
	env := setupLabelsTest(t)
	require.NoError(t, env.svc.RecordTurnOutcome(t.Context(), TurnOutcome{
		SessionID: "s1", TurnID: "t0", Commands: []string{"ls"},
	}))
	now := time.Now()
	env.svc = NewService(db.New(env.conn), env.workingDir, params.DefaultMemory(),
		WithClock(func() time.Time { return now.Add(25 * time.Hour) }))
	c := settleTurnRow(t, env, "s1", "t0")
	require.True(t, c.settledAt.Valid)
	require.Equal(t, TurnUnknown, c.verdict.String,
		"a command-only turn with no test evidence is unknown, not accepted by default")
}

// A sub-agent child session is not a settlement boundary for turns
// either — same predicate, same consequence.
func TestSettleTurnOutcomes_ChildSessionDoesNotSettle(t *testing.T) {
	env := setupLabelsTest(t)
	require.NoError(t, env.svc.RecordTurnOutcome(t.Context(), TurnOutcome{
		SessionID: "s1", TurnID: "t0", Commands: []string{"ls"},
	}))
	seedLabelSessionAt(t, env, "child", "s1", time.Now().Unix()+60)
	require.NoError(t, env.svc.SettleTurnOutcomes(t.Context(), 10))
	require.False(t, readTurn(t, env, "s1", "t0").settledAt.Valid,
		"a delegated child is not a session boundary")

	seedLabelSessionAt(t, env, "s2", "", time.Now().Unix()+60)
	require.NoError(t, env.svc.SettleTurnOutcomes(t.Context(), 10))
	require.True(t, readTurn(t, env, "s1", "t0").settledAt.Valid)
}
