package eval

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/charmbracelet/crush/internal/db"
)

// Scripted seeds write real command/failure rows under a controlled
// clock: two sessions at different ages leave correctly partitioned,
// backdated rows — including a resolved failure produced by a fixed
// command succeeding in the later session.
func TestRunScriptedSeeds(t *testing.T) {
	now := time.Now()
	r := &Runner{EvalDir: t.TempDir(), Home: t.TempDir(), Now: func() time.Time { return now }}
	workdir := t.TempDir()

	seeds := []ScriptedSeed{
		{AgoSeconds: 72 * 3600, Commands: []string{"cat missing-marker"}},
		{AgoSeconds: 3600, Commands: []string{"touch missing-marker", "cat missing-marker", "true"}},
	}
	ids, err := r.runScriptedSeeds(context.Background(), workdir, seeds, "eval-test")
	require.NoError(t, err)
	require.Len(t, ids, 2)
	require.NotEqual(t, ids[0], ids[1])

	dataDir := DataDirFor(workdir)
	conn, err := db.Connect(context.Background(), dataDir)
	require.NoError(t, err)
	defer func() { require.NoError(t, db.Release(dataDir)) }()

	var openCount, resolvedCount int
	require.NoError(t, conn.QueryRowContext(context.Background(),
		`SELECT count(*) FROM failure_memory WHERE resolved_in = ''`).Scan(&openCount))
	require.NoError(t, conn.QueryRowContext(context.Background(),
		`SELECT count(*) FROM failure_memory WHERE resolved_in = ?`, ids[1]).Scan(&resolvedCount))
	require.Zero(t, openCount)
	require.Equal(t, 1, resolvedCount)

	// Sessions land backdated to their ago_seconds clock — the age
	// hints and session spacing the selector's ordering reads.
	var sessionCreated int64
	require.NoError(t, conn.QueryRowContext(context.Background(),
		`SELECT created_at FROM sessions WHERE id = ?`, ids[0]).Scan(&sessionCreated))
	require.InDelta(t, now.Add(-72*time.Hour).Unix(), sessionCreated, 60)

	// Command rows carry the scripted session's provenance and the
	// same backdated clock — last_at ~= now - ago, not wall time.
	var cmdCount int
	var lastAt, lastAtResolved int64
	var lastSession, projKey string
	require.NoError(t, conn.QueryRowContext(context.Background(),
		`SELECT count(*) FROM command_memory`).Scan(&cmdCount))
	require.Equal(t, 3, cmdCount)
	// The seed's partition is the pinned key — a measured run
	// resolving the same project_key sees every seeded row.
	require.NoError(t, conn.QueryRowContext(context.Background(),
		`SELECT DISTINCT project_key FROM command_memory`).Scan(&projKey))
	require.Equal(t, "eval-test", projKey)
	require.NoError(t, conn.QueryRowContext(context.Background(),
		`SELECT last_at, last_session_id FROM command_memory WHERE cmd_norm = 'cat missing-marker'`).
		Scan(&lastAt, &lastSession))
	require.Equal(t, ids[1], lastSession)
	require.InDelta(t, now.Add(-time.Hour).UnixMilli(), lastAt, 60_000)

	// The 72h-old touch-free failure was superseded: the row's
	// first_seen stays on the old clock — staleness is authored.
	var firstSeen int64
	require.NoError(t, conn.QueryRowContext(context.Background(),
		`SELECT first_seen FROM failure_memory WHERE resolved_in != ''`).Scan(&firstSeen))
	require.InDelta(t, now.Add(-72*time.Hour).UnixMilli(), firstSeen, 60_000)

	require.NoError(t, conn.QueryRowContext(context.Background(),
		`SELECT last_at FROM command_memory WHERE cmd_norm = 'true'`).Scan(&lastAtResolved))
	require.InDelta(t, now.Add(-time.Hour).UnixMilli(), lastAtResolved, 60_000)
}

// A command that never reaches an exit status — unparseable — aborts
// the seed rather than write a partial state the manifest didn't
// author.
func TestRunScriptedSeedsNoVerdict(t *testing.T) {
	r := &Runner{EvalDir: t.TempDir(), Home: t.TempDir()}
	workdir := t.TempDir()

	seeds := []ScriptedSeed{
		{AgoSeconds: 60, Commands: []string{"if ;;; then"}},
	}
	ids, err := r.runScriptedSeeds(context.Background(), workdir, seeds, "eval-test")
	require.Error(t, err)
	require.Contains(t, err.Error(), "no verdict")
	require.Empty(t, ids)
}

// End-to-end through ExecuteRun: scripted seeds write the ledger, the
// seed-check gate reads it, and the measured run proceeds only after
// the designed state verified. WarmStart records the seed session ids
// for provenance.
func TestExecuteRun_ScriptedSeedsGatePasses(t *testing.T) {
	t.Parallel()
	root := newEvalDir(t)
	trajDir := writeTrajectory(t, filepath.Join(root, "corpus"), "t1", map[string]any{
		"seed_commands": []any{
			map[string]any{"ago_seconds": 7200, "commands": []any{"cat missing-marker"}},
			map[string]any{"ago_seconds": 60, "commands": []any{"true"}},
		},
		"check": map[string]any{
			"script":             "check.sh",
			"expect_start_state": "fail",
			"seed_script":        "seed_check.sh",
		},
	})
	// The gate asserts the authored dose: one open failure row and
	// two seed sessions — exactly what the two-element spec writes.
	require.NoError(t, os.WriteFile(filepath.Join(trajDir, "seed_check.sh"), []byte(`#!/bin/bash
db="$(dirname "$EVAL_WORKDIR")/$(basename "$EVAL_WORKDIR").crush-data/crush.db"
open_rows=$(sqlite3 "$db" "SELECT COUNT(*) FROM failure_memory WHERE resolved_in = '';")
sessions=$(sqlite3 "$db" "SELECT COUNT(*) FROM sessions WHERE title LIKE 'seed %';")
echo "EVAL_JSON {\"open_rows\":${open_rows:-0},\"seed_sessions\":${sessions:-0}}"
[ "$open_rows" -eq 1 ] && [ "$sessions" -eq 2 ]
`), 0o755))

	traj, err := LoadTrajectory(trajDir)
	require.NoError(t, err)
	exp := &Experiment{
		Name: "e1", Model: "mock/m", Temperature: ptr(0.0),
		Arms: map[string]Arm{ArmControl: {}},
	}
	r := &Runner{EvalDir: root, Driver: noDBDriver{}, WorkParent: t.TempDir()}

	rec, err := r.ExecuteRun(context.Background(), exp, traj, trajDir,
		ArmControl, Arm{}, &FlagsManifest{Defaults: map[string]any{}}, 1, "inv1")
	require.NoError(t, err)
	require.Equal(t, OutcomePass, rec.Outcome)
	require.NotNil(t, rec.WarmStart)
	require.Equal(t, 2, rec.WarmStart.Sessions)
	require.Len(t, rec.WarmStart.SessionIDs, 2)
}

// A seed gate that reads zero rows — the scripted seed wrote nothing
// the check expects — rejects inconclusive before the measured run.
func TestExecuteRun_ScriptedSeedsGateFails(t *testing.T) {
	t.Parallel()
	root := newEvalDir(t)
	trajDir := writeTrajectory(t, filepath.Join(root, "corpus"), "t1", map[string]any{
		"seed_commands": []any{
			map[string]any{"ago_seconds": 60, "commands": []any{"true"}},
		},
		"check": map[string]any{
			"script":             "check.sh",
			"expect_start_state": "fail",
			"seed_script":        "seed_check.sh",
		},
	})
	require.NoError(t, os.WriteFile(filepath.Join(trajDir, "seed_check.sh"), []byte(`#!/bin/bash
db="$(dirname "$EVAL_WORKDIR")/$(basename "$EVAL_WORKDIR").crush-data/crush.db"
open_rows=$(sqlite3 "$db" "SELECT COUNT(*) FROM failure_memory WHERE resolved_in = '';")
echo "EVAL_JSON {\"open_rows\":${open_rows:-0}}"
[ "$open_rows" -ge 1 ]
`), 0o755))

	traj, err := LoadTrajectory(trajDir)
	require.NoError(t, err)
	exp := &Experiment{
		Name: "e1", Model: "mock/m", Temperature: ptr(0.0),
		Arms: map[string]Arm{ArmControl: {}},
	}
	r := &Runner{EvalDir: root, Driver: noDBDriver{}, WorkParent: t.TempDir()}

	rec, err := r.ExecuteRun(context.Background(), exp, traj, trajDir,
		ArmControl, Arm{}, &FlagsManifest{Defaults: map[string]any{}}, 1, "inv1")
	require.NoError(t, err)
	require.Equal(t, OutcomeInconclusive, rec.Outcome)
	require.Contains(t, fmt.Sprint(rec.CheckDetail), "seed state assertion failed")
}

// Snapshot replay: attempt 2 restores the seeded workdir + crush.db
// byte-for-byte rather than re-seeding — the session rows stay at
// the two the first attempt wrote, provenance carries, and spend
// fields read zero (the seed was paid once).
func TestExecuteRun_SeedSnapshotReplay(t *testing.T) {
	t.Parallel()
	root := newEvalDir(t)
	trajDir := writeTrajectory(t, filepath.Join(root, "corpus"), "t1", map[string]any{
		"seed_commands": []any{
			map[string]any{"ago_seconds": 7200, "commands": []any{"cat missing-marker"}},
			map[string]any{"ago_seconds": 60, "commands": []any{"touch from-seed", "rm hello.txt"}},
		},
		// hello.txt comes from the fixture — the seed deleted it.
		// If restore overlaid onto the fresh materialize instead of
		// replacing it, the file would resurrect and this check
		// would fail on attempt 2.
		"check_script_body": "#!/bin/bash\ntest -f fixed.marker && test ! -f hello.txt\n",
	})
	traj, err := LoadTrajectory(trajDir)
	require.NoError(t, err)
	exp := &Experiment{
		Name: "e1", Model: "mock/m", Temperature: ptr(0.0),
		Arms: map[string]Arm{ArmControl: {}},
	}
	// WorkParent must persist across attempts — snapshots live
	// beside the materialized workdirs.
	workParent := t.TempDir()
	r := &Runner{EvalDir: root, Driver: noDBDriver{}, WorkParent: workParent}
	manifest := &FlagsManifest{Defaults: map[string]any{}}

	rec1, err := r.ExecuteRun(context.Background(), exp, traj, trajDir,
		ArmControl, Arm{}, manifest, 1, "inv1")
	require.NoError(t, err)
	require.Equal(t, OutcomePass, rec1.Outcome)
	require.Equal(t, 2, rec1.WarmStart.Sessions)
	require.Len(t, rec1.WarmStart.SessionIDs, 2)
	require.DirExists(t, r.snapshotDir(traj.ID+"-"+rec1.Env.ContentHash))

	rec2, err := r.ExecuteRun(context.Background(), exp, traj, trajDir,
		ArmControl, Arm{}, manifest, 2, "inv1")
	require.NoError(t, err)
	require.Equal(t, OutcomePass, rec2.Outcome)
	// Provenance carries the original seed sessions; spend reads
	// zero — the seed was paid by attempt 1.
	require.Equal(t, rec1.WarmStart.SessionIDs, rec2.WarmStart.SessionIDs)
	require.Equal(t, 2, rec2.WarmStart.Sessions)
	require.Zero(t, rec2.WarmStart.Steps)
	require.Zero(t, rec2.WarmStart.Tokens)

	// The restored db carries the seeded rows under the pinned key.
	conn, err := db.ConnectReadOnly(context.Background(),
		filepath.Join(root, rec2.SessionDB))
	require.NoError(t, err)
	defer conn.Close()
	var sessions, openRows int
	require.NoError(t, conn.QueryRowContext(context.Background(),
		`SELECT count(*) FROM sessions WHERE title LIKE 'seed %'`).Scan(&sessions))
	require.Equal(t, 2, sessions, "restore re-seeded — sessions doubled")
	require.NoError(t, conn.QueryRowContext(context.Background(),
		`SELECT count(*) FROM failure_memory WHERE resolved_in = ''`).Scan(&openRows))
	require.Equal(t, 1, openRows)
}

// The corpus host for the LOO ladder: its scripted seeds must
// materialize all three memory pools — resolved, open, command —
// and the seed_check gate must see exactly that state.
func TestSeededThreePoolFixture(t *testing.T) {
	if testing.Short() {
		t.Skip("runs the real fixture's go toolchain")
	}
	trajDir := filepath.Join("..", "..", "eval", "corpus", "seeded-three-pool")
	traj, err := LoadTrajectory(trajDir)
	require.NoError(t, err)

	r := &Runner{EvalDir: t.TempDir(), Home: t.TempDir()}
	workdir, err := Materialize(context.Background(), traj, trajDir, t.TempDir(), r.checkEnv())
	require.NoError(t, err)
	defer func() {
		os.RemoveAll(workdir)
		os.RemoveAll(DataDirFor(workdir))
	}()

	ids, err := r.runScriptedSeeds(context.Background(), workdir, traj.SeedCommands, "eval-test")
	require.NoError(t, err)
	require.Len(t, ids, 2)

	schk := runCheckScript(context.Background(), traj.Check.SeedScript, trajDir, workdir, r.checkEnv(), checkTimeout(traj))
	require.NoError(t, schk.Err)
	require.Equal(t, 0, schk.Exit, "seed gate rejected the fixture's own seed state: %s", schk.Stderr)
}

// The validation rules seed_commands must satisfy at load time.
func TestValidateTrajectory_SeedCommands(t *testing.T) {
	t.Parallel()
	root := newEvalDir(t)

	// seed_script accepts seed_commands as a seeding source.
	dir := writeTrajectory(t, filepath.Join(root, "corpus"), "t-ok", map[string]any{
		"seed_commands": []any{
			map[string]any{"ago_seconds": 60, "commands": []any{"true"}},
		},
		"check": map[string]any{
			"script":             "check.sh",
			"expect_start_state": "fail",
			"seed_script":        "seed_check.sh",
		},
	})
	require.NoError(t, os.WriteFile(filepath.Join(dir, "seed_check.sh"), []byte("#!/bin/bash\nexit 0\n"), 0o755))
	_, err := LoadTrajectory(dir)
	require.NoError(t, err)

	// Negative ago_seconds is rejected — seeds model the past.
	dir = writeTrajectory(t, filepath.Join(root, "corpus"), "t-neg", map[string]any{
		"seed_commands": []any{
			map[string]any{"ago_seconds": -5, "commands": []any{"true"}},
		},
	})
	_, err = LoadTrajectory(dir)
	require.Error(t, err)
	require.Contains(t, err.Error(), "ago_seconds must be >= 0")

	// An empty command list is rejected.
	dir = writeTrajectory(t, filepath.Join(root, "corpus"), "t-empty", map[string]any{
		"seed_commands": []any{
			map[string]any{"ago_seconds": 60, "commands": []any{}},
		},
	})
	_, err = LoadTrajectory(dir)
	require.Error(t, err)
	require.Contains(t, err.Error(), "must contain at least one command")
}
