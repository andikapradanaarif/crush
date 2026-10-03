package agent

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/charmbracelet/crush/internal/cmdlog"
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
	mt.roll = func() float64 { return 0.99 } // never hold out

	old := cmdlog.Failure{Cmd: "go test ./decoy", LastSeen: time.Now().Add(-48 * time.Hour)}
	decisions := []FailureDecision{
		{Signature: "s1", Admit: true},
		{Signature: "s2", Admit: false, Reason: "stale_suspect"},
		{Signature: "s3", Admit: false, Reason: "stale_suspect"},
	}
	mt.recordTurn("sess-1", "fix the test", []string{"<open_failures>x</open_failures>"}, []cmdlog.Failure{old}, decisions, true, false, nil)
	mt.recordTurn("sess-1", "again", nil, nil, nil, true, false, nil)

	recs := readTelemetry(t, dir)
	require.Len(t, recs, 3, "one session_start + two turn records")

	require.Equal(t, "session_start", recs[0]["type"])
	require.Equal(t, "sess-1", recs[0]["session_id"])
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
	mt.roll = func() float64 { return 0.05 } // under the 0.1 rate
	require.True(t, mt.holdoutOff("a"))
	require.True(t, mt.holdoutOff("a"), "assignment must be stable")
	mt.roll = func() float64 { return 0.5 }
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
	mt.recordTurn("s", "p", nil, nil, nil, false, false, nil) // must not panic
	require.False(t, mt.seen["s"], "session_start stays unwritten after a failed append")
}
