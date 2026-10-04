package agent

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/charmbracelet/crush/internal/cmdlog"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/csync"
	"github.com/stretchr/testify/require"
)

func readTelemetry(t *testing.T, dir string) []map[string]any {
	t.Helper()
	f, err := os.Open(filepath.Join(dir, "memory-telemetry.jsonl"))
	require.NoError(t, err)
	defer f.Close()
	var recs []map[string]any
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var rec map[string]any
		require.NoError(t, json.Unmarshal(sc.Bytes(), &rec))
		recs = append(recs, rec)
	}
	return recs
}

// Disabled or missing-data-dir configurations yield no logger — the
// option is off unless explicitly enabled.
func TestNewMemoryTelemetry_Disabled(t *testing.T) {
	t.Parallel()
	require.Nil(t, newMemoryTelemetry(false, t.TempDir(), "/w"))
	require.Nil(t, newMemoryTelemetry(true, "", "/w"))
	require.NotNil(t, newMemoryTelemetry(true, t.TempDir(), "/w"))
}

// First call per session writes session_start (snapshot + holdout
// arm); every call writes a turn record carrying the prompt and the
// selection evidence.
func TestMemoryTelemetry_RecordTurn(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	mt := newMemoryTelemetry(true, dir, t.TempDir())
	mt.roll = func(string) float64 { return 0.99 } // never hold out

	old := cmdlog.Failure{Cmd: "go test ./decoy", LastSeen: time.Now().Add(-48 * time.Hour)}
	decisions := []FailureDecision{
		{Signature: "s1", Admit: true},
		{Signature: "s2", Admit: false, Reason: "stale_suspect"},
		{Signature: "s3", Admit: false, Reason: "stale_suspect"},
	}
	mt.recordTurn("sess-1", "fix the test", []string{"<open_failures>x</open_failures>"}, []cmdlog.Failure{old}, decisions, "coder", true, false, nil)
	mt.recordTurn("sess-1", "again", nil, nil, nil, "coder", true, false, nil)

	recs := readTelemetry(t, dir)
	require.Len(t, recs, 3, "one session_start + two turn records")

	require.Equal(t, "session_start", recs[0]["type"])
	require.Equal(t, "sess-1", recs[0]["session_id"])
	require.Equal(t, "coder", recs[0]["agent"])
	require.Equal(t, "fix the test", recs[0]["prompt"])
	require.Equal(t, true, recs[0]["memory_armed"])
	require.Equal(t, false, recs[0]["holdout"])
	require.Contains(t, recs[0], "workdir")
	require.Contains(t, recs[0], "head_sha") // empty outside a repo — present either way

	turn := recs[1]
	require.Equal(t, "turn", turn["type"])
	require.Equal(t, "fix the test", turn["prompt"])
	require.Equal(t, true, turn["memory_armed"])
	require.Equal(t, []any{"open_failures"}, turn["sections"])
	require.Equal(t, float64(1), turn["candidates"])
	require.Equal(t, float64(1), turn["admitted"])
	require.Equal(t, map[string]any{"stale_suspect": float64(2)}, turn["rejections"])
	ages, ok := turn["candidate_age_days"].([]any)
	require.True(t, ok)
	require.InDelta(t, 2.0, ages[0].(float64), 0.02)

	quiet := recs[2]
	require.Equal(t, float64(0), quiet["candidates"])
	require.Equal(t, float64(0), quiet["admitted"])
	require.NotContains(t, quiet, "rejections")
}

// The holdout coin flips once per session and the assignment is
// sticky — later calls must see the same arm.
func TestMemoryTelemetry_HoldoutSticky(t *testing.T) {
	t.Parallel()
	mt := newMemoryTelemetry(true, t.TempDir(), "/w")
	mt.roll = func(string) float64 { return 0.05 } // under the 0.1 rate
	require.True(t, mt.holdoutOff("a"))
	require.True(t, mt.holdoutOff("a"), "assignment must be stable")
	mt.roll = func(string) float64 { return 0.5 }
	require.False(t, mt.holdoutOff("b"))
	require.False(t, mt.holdoutOff("b"))
}

// Logging failures never propagate — a read-only data dir must not
// disturb the run, and session_start retries on the next turn rather
// than being skipped forever.
func TestMemoryTelemetry_AppendFailureSwallowed(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "nope")
	mt := newMemoryTelemetry(true, dir, "/w")
	blocked := filepath.Join(dir, "memory-telemetry.jsonl")
	// A directory where the file should be makes OpenFile fail.
	require.NoError(t, os.MkdirAll(blocked, 0o755))
	mt.recordTurn("s", "p", nil, nil, nil, "coder", false, false, nil) // must not panic
	require.False(t, mt.seen["s"], "session_start stays unwritten after a failed append")
}

// One logger shared across agents (the coordinator's shape): the
// session's holdout arm and session_start are written once no
// matter which agent asks — a session switching coder→plan keeps
// the same control arm instead of flipping a second coin.
func TestMemoryTelemetry_SharedAcrossAgents(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	mt := newMemoryTelemetry(true, dir, t.TempDir())
	mt.roll = func(string) float64 { return 0.05 } // held out

	require.True(t, mt.holdoutOff("s"), "coder's coin")
	require.True(t, mt.holdoutOff("s"), "plan sees the same arm — one session, one coin")

	mt.recordTurn("s", "p", nil, nil, nil, "coder", true, true, nil)
	mt.recordTurn("s", "p", nil, nil, nil, "plan", true, true, nil)
	recs := readTelemetry(t, dir)
	require.Len(t, recs, 3, "one session_start + two turn records — not two session_starts")
	require.Equal(t, "session_start", recs[0]["type"])
	require.Equal(t, "coder", recs[1]["agent"])
	require.Equal(t, "plan", recs[2]["agent"])
}

// forget drops a deleted session's bookkeeping so seen/arms don't
// grow unboundedly in a long-lived process.
func TestMemoryTelemetry_Forget(t *testing.T) {
	t.Parallel()
	mt := newMemoryTelemetry(true, t.TempDir(), "/w")
	mt.roll = func(string) float64 { return 0.99 }
	require.False(t, mt.holdoutOff("s"))
	mt.seen["s"] = true
	mt.forget("s")
	require.NotContains(t, mt.arms, "s")
	require.NotContains(t, mt.seen, "s")
}

// An eval child must never coin a holdout: a trajectory arm enabling
// telemetry would silently drop ~10% of runs to suppression with no
// marker in the RunRecord. The flag-manifest env var marks a
// harness-driven process.
func TestMemoryTelemetry_EvalEnvSkipsHoldout(t *testing.T) {
	// Not parallel — Setenv is process-global.
	t.Setenv(EvalFlagsEnvVar, "failure_memory")
	mt := newMemoryTelemetry(true, t.TempDir(), "/w")
	mt.roll = func(string) float64 { return 0 } // would always hold out
	require.False(t, mt.holdoutOff("s"))
	require.False(t, mt.holdoutOff(""), "an empty key never arms")
}

// The default coin is deterministic per session ID: a process restart
// cannot re-flip a held-out session into the treatment arm.
func TestMemoryTelemetry_HoldoutStableAcrossRestarts(t *testing.T) {
	t.Parallel()
	mt := newMemoryTelemetry(true, t.TempDir(), "/w")
	mt2 := newMemoryTelemetry(true, t.TempDir(), "/w")
	for _, id := range []string{"sess-a", "sess-b", "sess-c"} {
		require.Equal(t, mt.holdoutOff(id), mt2.holdoutOff(id))
	}
}

// The seam the whole experiment hangs on: a held-out session must
// suppress the injection while the fetch and the selector still run —
// the audit and the log capture what the suppressed arm would have
// rendered.
func TestMemoryTelemetry_HoldoutSuppressesInjection(t *testing.T) {
	t.Parallel()
	a, env, sessionID := newTurnCtxAgent(t, &config.Config{})
	a.tailAudit = csync.NewMap[string, TailAudit]()
	a.failureMemory = true
	dir := t.TempDir()
	mt := newMemoryTelemetry(true, dir, env.workingDir)
	mt.roll = func(string) float64 { return 0 } // always hold out
	a.memoryTelemetry = mt
	env.cmdlog.RecordRun(t.Context(), cmdlog.Run{
		SessionID: "prior", Command: "make test",
		CWD: env.workingDir, Stdout: "FAIL", ExitCode: 1, Ran: true,
	})

	// The selector binds this prompt — under holdout nothing renders.
	tail := a.turnTailMessages(t.Context(), SessionAgentCall{
		SessionID: sessionID, Prompt: "the test fails — fix it",
	}, nil)
	require.Empty(t, tail)

	// The audit carries the counterfactual: the candidate was
	// evaluated and admitted — then suppressed.
	audit, ok := a.tailAudit.Get(sessionID)
	require.True(t, ok)
	require.Empty(t, audit.Sections)
	require.NotEmpty(t, audit.Decisions)
	require.True(t, audit.Decisions[0].Admit)

	// The log records the arm and the would-be render.
	recs := readTelemetry(t, dir)
	require.Len(t, recs, 2)
	require.Equal(t, true, recs[0]["holdout"])
	require.Equal(t, true, recs[1]["holdout"])
	require.Equal(t, float64(1), recs[1]["admitted"])
	require.Empty(t, recs[1]["sections"])
}

// The treatment arm — telemetry on, coin favors injection — renders
// normally.
func TestMemoryTelemetry_TreatmentArmStillRenders(t *testing.T) {
	t.Parallel()
	a, env, sessionID := newTurnCtxAgent(t, &config.Config{})
	a.tailAudit = csync.NewMap[string, TailAudit]()
	a.failureMemory = true
	dir := t.TempDir()
	mt := newMemoryTelemetry(true, dir, env.workingDir)
	mt.roll = func(string) float64 { return 0.99 } // never hold out
	a.memoryTelemetry = mt
	env.cmdlog.RecordRun(t.Context(), cmdlog.Run{
		SessionID: "prior", Command: "make test",
		CWD: env.workingDir, Stdout: "FAIL", ExitCode: 1, Ran: true,
	})

	tail := a.turnTailMessages(t.Context(), SessionAgentCall{
		SessionID: sessionID, Prompt: "the test fails — fix it",
	}, nil)
	require.Len(t, tail, 1)

	recs := readTelemetry(t, dir)
	require.Len(t, recs, 2)
	require.Equal(t, false, recs[0]["holdout"])
	require.Equal(t, false, recs[1]["holdout"])
	require.Equal(t, []any{"open_failures"}, recs[1]["sections"])
}
