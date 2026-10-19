package cmd

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/crush/internal/db"
	"github.com/stretchr/testify/require"
)

// trendFixture stamps rows into a migrated scratch DB. All timestamps
// are fixed so week buckets are deterministic:
// weekA = the ISO week of Monday 2026-10-05, weekB = 2026-10-12.
var (
	weekAms = time.Date(2026, 10, 6, 10, 0, 0, 0, time.UTC).UnixMilli()
	weekBms = time.Date(2026, 10, 13, 10, 0, 0, 0, time.UTC).UnixMilli()
)

func trendExec(t *testing.T, conn *sql.DB, q string, args ...any) {
	t.Helper()
	_, err := conn.ExecContext(t.Context(), q, args...)
	require.NoError(t, err)
}

func insertTurnOutcome(t *testing.T, conn *sql.DB, key, sid, tid, verdict string,
	createdMs int64, settled bool, paths, added, after string,
) {
	t.Helper()
	var settledAt any
	if settled {
		settledAt = createdMs + 3600_000
	}
	trendExec(t, conn, `INSERT INTO turn_outcomes
		(session_id, turn_id, project_key, edited_paths, edit_hunks,
		 commands, hunk_added, hunk_after, next_verdict, verdict,
		 settle_at, settled_at, param_version, created_at)
		VALUES (?, ?, ?, ?, '[]', '[]', ?, ?, '', ?, ?, ?, 'pv0', ?)`,
		sid, tid, key, paths, added, after, verdict, createdMs+86_400_000,
		settledAt, createdMs)
}

func insertSession(t *testing.T, conn *sql.DB, id string, createdSec, tokens int64, userMsgs int) {
	t.Helper()
	trendExec(t, conn, `INSERT INTO sessions
		(id, title, prompt_tokens, completion_tokens, cost, created_at, updated_at)
		VALUES (?, 's', ?, 0, 0, ?, ?)`, id, tokens, createdSec, createdSec)
	for i := range userMsgs {
		trendExec(t, conn, `INSERT INTO messages
			(id, session_id, role, parts, created_at, updated_at)
			VALUES (?, ?, 'user', '[]', ?, ?)`,
			fmt.Sprintf("%s-u%d", id, i), id, createdSec, createdSec)
	}
	// An assistant message keeps the shape realistic without
	// affecting the user-turn count.
	trendExec(t, conn, `INSERT INTO messages
		(id, session_id, role, parts, created_at, updated_at)
		VALUES (?, ?, 'assistant', '[]', ?, ?)`,
		id+"-a", id, createdSec, createdSec)
}

func insertCmdRun(t *testing.T, conn *sql.DB, key, sid, cmdNorm, cwd string, exit, atMs int64) {
	t.Helper()
	trendExec(t, conn, `INSERT INTO session_command_runs
		(session_id, cmd_norm, cwd, kind, last_exit, last_at, project_key, param_version)
		VALUES (?, ?, ?, 'test', ?, ?, ?, 'pv0')`,
		sid, cmdNorm, cwd, exit, atMs, key)
}

// window builds a hunk_after window whose lines at [start,start+n)
// carry content and the rest is context filler.
func window(start int, total int, at int, content []string) trendHunkWindow {
	lines := make([]string, total)
	for i := range lines {
		lines[i] = fmt.Sprintf("ctx-%d", i)
	}
	copy(lines[at:], content)
	return trendHunkWindow{Start: start, Text: strings.Join(lines, "\n")}
}

func TestGatherTrend(t *testing.T) {
	t.Parallel()

	conn, err := db.Connect(t.Context(), t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { conn.Close() })

	key := "proj"

	// Week A: accepted with a comparable pair (region differs by one
	// line → cost 1), revised excluded (later_turn_touched), one
	// failed, one unknown, one still-unsettled (not counted).
	addedOK := `{"a.go":{"v":1,"state":"ok","edits":[{"old":"x","new":"alpha\nbeta"}],"spans":[{"start":10,"end":12}]}}`
	afterChanged, _ := json.Marshal(map[string]trendAfterCell{
		"a.go": {State: "ok", Windows: []trendHunkWindow{
			window(0, 40, 10, []string{"alpha", "beta_CHANGED"}),
		}},
	})
	insertTurnOutcome(t, conn, key, "s1", "t1", "accepted", weekAms, true,
		`["a.go"]`, addedOK, string(afterChanged))
	insertTurnOutcome(t, conn, key, "s1", "t2", "revised", weekAms+1000, true,
		`["b.go"]`, `{"b.go":{"v":1,"state":"ok","edits":[{"new":"n"}],"spans":[{"start":0,"end":1}]}}`,
		`{"b.go":{"v":1,"state":"ok","later_turn_touched":true,"windows":[{"start":0,"text":"n"}]}}`)
	insertTurnOutcome(t, conn, key, "s1", "t3", "failed", weekAms+2000, true, `[]`, `{}`, `{}`)
	insertTurnOutcome(t, conn, key, "s1", "t4", "unknown", weekAms+3000, true, `[]`, `{}`, `{}`)
	insertTurnOutcome(t, conn, key, "s1", "t5", "", weekAms+4000, false, `["c.go"]`, `{}`, `{}`)

	// Week B: accepted whose after-side is region_unknown (excluded),
	// and one on a sensitive path (excluded).
	insertTurnOutcome(t, conn, key, "s2", "t6", "accepted", weekBms, true,
		`["gone.go"]`, `{"gone.go":{"v":1,"state":"ok","edits":[{"new":"z"}],"spans":[{"start":0,"end":1}]}}`,
		`{"gone.go":{"v":1,"state":"region_unknown"}}`)
	insertTurnOutcome(t, conn, key, "s2", "t7", "accepted", weekBms+1000, true,
		`[".env"]`, `{".env":{"v":1,"state":"sensitive"}}`, `{".env":{"v":1,"state":"sensitive"}}`)

	// Sessions: two in week A (10 user turns, 1000 tokens), one in
	// week B (4 user turns, 400 tokens).
	insertSession(t, conn, "s1", weekAms/1000, 700, 6)
	insertSession(t, conn, "s1b", weekAms/1000+100, 300, 4)
	insertSession(t, conn, "s2", weekBms/1000, 400, 4)

	// Command runs: go test fails s1, passes s1b, fails again s2 —
	// the week-B failure is a re-open (repeat). `make lint` fails
	// twice with no pass between — week-A failure is first-seen, not
	// a repeat.
	insertCmdRun(t, conn, key, "s1", "go test", "", 1, weekAms)
	insertCmdRun(t, conn, key, "s1b", "go test", "", 0, weekAms+500)
	insertCmdRun(t, conn, key, "s2", "go test", "", 1, weekBms)
	insertCmdRun(t, conn, key, "s1", "make lint", "", 1, weekAms+100)
	insertCmdRun(t, conn, key, "s1b", "make lint", "", 1, weekAms+600)

	rows, err := gatherTrend(t.Context(), conn, trendProject{path: "/p", key: key}, 0)
	require.NoError(t, err)
	require.Len(t, rows, 2)

	a := rows[0]
	require.Equal(t, "2026-10-05", a.Week)
	require.Equal(t, 4, a.SettledTurns)
	require.Equal(t, 1, a.Accepted)
	require.Equal(t, 1, a.Revised)
	require.Equal(t, 1, a.Failed)
	require.Equal(t, 1, a.Unknown)
	require.InDelta(t, 0.5, *a.RevisionRate, 1e-9)
	require.Equal(t, 1, a.EditPairs)
	require.Equal(t, 1, a.EditTurns)
	require.Equal(t, 1, a.EditCostLines)
	require.Equal(t, map[string]int{"later_turn_touched": 1}, a.EditCostExcluded)
	require.InDelta(t, 0.5, *a.EditCostPerTurn, 1e-9) // 1 line / 2 acc+rev turns
	require.Equal(t, 2, a.Sessions)
	require.Equal(t, 10, a.UserTurns)
	require.EqualValues(t, 1000, a.TokensTotal)
	require.InDelta(t, 100.0, *a.TokensPerTurn, 1e-9)
	require.Equal(t, 3, a.CmdFailures)
	require.Equal(t, 0, a.RepeatFailures)
	require.InDelta(t, 0.0, *a.RepeatFailureRate, 1e-9)

	b := rows[1]
	require.Equal(t, "2026-10-12", b.Week)
	require.Equal(t, 2, b.Accepted)
	require.Equal(t, 0, b.EditPairs)
	require.Equal(t, map[string]int{"after_region_unknown": 1, "sensitive": 1}, b.EditCostExcluded)
	require.Equal(t, 1, b.CmdFailures)
	require.Equal(t, 1, b.RepeatFailures)
	require.InDelta(t, 1.0, *b.RepeatFailureRate, 1e-9)
	require.InDelta(t, 100.0, *b.TokensPerTurn, 1e-9)
}

func TestGatherTrendSharedStore(t *testing.T) {
	t.Parallel()

	conn, err := db.Connect(t.Context(), t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { conn.Close() })

	// Two partitions: sessions carrying another project's ledger
	// rows must not count toward this project's tokens.
	insertTurnOutcome(t, conn, "mine", "sa", "ta", "accepted", weekAms, true, `[]`, `{}`, `{}`)
	insertTurnOutcome(t, conn, "other", "sb", "tb", "accepted", weekAms, true, `[]`, `{}`, `{}`)
	insertSession(t, conn, "sa", weekAms/1000, 100, 2)
	insertSession(t, conn, "sb", weekAms/1000, 900, 2)

	rows, err := gatherTrend(t.Context(), conn, trendProject{path: "/p", key: "mine"}, 0)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, 1, rows[0].Sessions)
	require.EqualValues(t, 100, rows[0].TokensTotal)
}

func TestTrendPairCostRegionSlicing(t *testing.T) {
	t.Parallel()

	// The after-window's context padding must not count: identical
	// region → zero cost even with 32 filler lines on each side.
	content := []string{"keep-1", "keep-2"}
	win := window(5, 70, 37, content) // span [42,44) inside window
	added := trendAddedCell{
		State: "ok",
		Edits: []trendHunkEdit{{New: "keep-1\nkeep-2"}},
		Spans: []trendHunkSpan{{Start: 42, End: 44}},
	}
	after := trendAfterCell{State: "ok", Windows: []trendHunkWindow{win}}

	// trendPairCost works on the raw JSON shape.
	addedJSON, _ := json.Marshal(map[string]trendAddedCell{"f.go": added})
	afterJSON, _ := json.Marshal(map[string]trendAfterCell{"f.go": after})
	r := &trendRow{}
	ok := trendEditCost(r, `["f.go"]`, string(addedJSON), string(afterJSON))
	require.True(t, ok)
	require.Equal(t, 1, r.EditPairs)
	require.Equal(t, 0, r.EditCostLines)

	// A one-line user revision costs 1.
	win2 := window(5, 70, 37, []string{"keep-1", "user-rewrote-this"})
	after2 := trendAfterCell{State: "ok", Windows: []trendHunkWindow{win2}}
	after2JSON, _ := json.Marshal(map[string]trendAfterCell{"f.go": after2})
	r2 := &trendRow{}
	require.True(t, trendEditCost(r2, `["f.go"]`, string(addedJSON), string(after2JSON)))
	require.Equal(t, 1, r2.EditCostLines)
}

func TestLevenshteinLines(t *testing.T) {
	t.Parallel()

	d, ok := levenshteinLines([]string{"a", "b"}, []string{"a", "b"})
	require.True(t, ok)
	require.Equal(t, 0, d)
	d, ok = levenshteinLines([]string{"a", "b"}, []string{"a", "x", "b"})
	require.True(t, ok)
	require.Equal(t, 1, d)
	d, ok = levenshteinLines(nil, []string{"a"})
	require.True(t, ok)
	require.Equal(t, 1, d)
	_, ok = levenshteinLines(make([]string, maxTrendDiffLines+1), nil)
	require.False(t, ok)
}

func TestParseTrendSince(t *testing.T) {
	t.Parallel()

	z, err := parseTrendSince("all")
	require.NoError(t, err)
	require.Zero(t, z)
	d, err := parseTrendSince("2026-10-05")
	require.NoError(t, err)
	require.Equal(t, time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC).UnixMilli(), d)
	w, err := parseTrendSince("2w")
	require.NoError(t, err)
	require.InDelta(t, time.Now().Add(-14*24*time.Hour).UnixMilli(), w, 5000)
	_, err = parseTrendSince("bogus")
	require.Error(t, err)
	_, err = parseTrendSince("7x")
	require.Error(t, err)
}

func TestTrendWeekStart(t *testing.T) {
	t.Parallel()

	// 2026-10-19 is a Monday; Wednesday must floor to it.
	require.Equal(t, "2026-10-19",
		trendWeekStart(time.Date(2026, 10, 19, 0, 0, 0, 0, time.UTC).UnixMilli()))
	require.Equal(t, "2026-10-19",
		trendWeekStart(time.Date(2026, 10, 21, 15, 0, 0, 0, time.UTC).UnixMilli()))
	require.Equal(t, "2026-10-19",
		trendWeekStart(time.Date(2026, 10, 25, 23, 59, 0, 0, time.UTC).UnixMilli()))
}

func TestAppendTrendLedgerDedupe(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	ledger := filepath.Join(dir, "trend.jsonl")

	row := func(week string, accepted int) trendRow {
		return trendRow{V: 1, Project: "/p", ProjectKey: "k", Week: week, Accepted: accepted}
	}
	require.NoError(t, appendTrendLedger(ledger, "k", []trendRow{row("2026-10-05", 1)}))
	require.NoError(t, appendTrendLedger(ledger, "k", []trendRow{row("2026-10-12", 2)}))
	// Re-running the same week replaces, not duplicates.
	require.NoError(t, appendTrendLedger(ledger, "k", []trendRow{row("2026-10-05", 9)}))

	data, err := os.ReadFile(ledger)
	require.NoError(t, err)
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	require.Len(t, lines, 2)
	got := map[string]trendRow{}
	for _, l := range lines {
		var r trendRow
		require.NoError(t, json.Unmarshal([]byte(l), &r))
		got[r.Week] = r
	}
	// The regenerated week replaced its prior row in place-count;
	// order is append — consumers sort by week.
	require.Equal(t, 9, got["2026-10-05"].Accepted)
	require.Equal(t, 2, got["2026-10-12"].Accepted)
}
