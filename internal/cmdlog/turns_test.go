package cmdlog

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/crush/internal/db"
	"github.com/charmbracelet/crush/internal/params"
	"github.com/stretchr/testify/require"
)

// turnCols reads back one turn row for assertions.
type turnCols struct {
	verdict     sql.NullString
	settledAt   sql.NullInt64
	settleAt    int64
	nextVerdict string
	editedPaths string
	hunkAdded   string
	hunkAfter   string
}

func readTurn(t *testing.T, env *labelEnv, sessionID, turnID string) turnCols {
	t.Helper()
	var c turnCols
	require.NoError(t, env.conn.QueryRowContext(t.Context(),
		`SELECT verdict, settled_at, settle_at, next_verdict, edited_paths,
		        hunk_added, hunk_after
		 FROM turn_outcomes
		 WHERE session_id = ? AND turn_id = ?`, sessionID, turnID).
		Scan(&c.verdict, &c.settledAt, &c.settleAt, &c.nextVerdict,
			&c.editedPaths, &c.hunkAdded, &c.hunkAfter))
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
		"a judged row is never rewritten — the conflict arm's WHERE refuses")

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
	require.Equal(t, ReferentAccepted, c.nextVerdict)
}

// The backfill can mint a placeholder for a concurrent session's
// in-flight turn (final turn, no follow-up observed). When that
// session's real next prompt lands, the live record must repair the
// placeholder — an empty verdict is unjudged, so evidence and
// verdict refresh. Judged ('accepted'/'revised') or frozen rows are
// never rewritten.
func TestRecordTurnOutcome_PlaceholderRepair(t *testing.T) {
	env := setupLabelsTest(t)

	// The placeholder: no verdict observed, no evidence span.
	require.NoError(t, env.svc.RecordTurnOutcome(t.Context(), TurnOutcome{
		SessionID: "s1", TurnID: "t0",
	}))
	// The live pass lands with the real evidence and judgment.
	require.NoError(t, env.svc.RecordTurnOutcome(t.Context(), TurnOutcome{
		SessionID:   "s1",
		TurnID:      "t0",
		EditedPaths: []string{"x.go"},
		Commands:    []string{"go test ./..."},
		NextVerdict: ReferentRevised,
	}))
	c := readTurn(t, env, "s1", "t0")
	require.Equal(t, ReferentRevised, c.nextVerdict,
		"the placeholder repairs once the real prompt lands")
	require.JSONEq(t, `["x.go"]`, c.editedPaths)

	// A judged row is immutable — a stale backfill re-scan can
	// never overwrite a real verdict.
	require.NoError(t, env.svc.RecordTurnOutcome(t.Context(), TurnOutcome{
		SessionID: "s1", TurnID: "t0", NextVerdict: ReferentUnknown,
	}))
	c = readTurn(t, env, "s1", "t0")
	require.Equal(t, ReferentRevised, c.nextVerdict)
	require.JSONEq(t, `["x.go"]`, c.editedPaths)
}

// 'unknown' is a judged cue verdict, not a placeholder: a row that
// resolved to unknown with full evidence is never clobbered by a
// sparser re-record.
func TestRecordTurnOutcome_JudgedUnknownKeepsEvidence(t *testing.T) {
	env := setupLabelsTest(t)

	require.NoError(t, env.svc.RecordTurnOutcome(t.Context(), TurnOutcome{
		SessionID:   "s1",
		TurnID:      "t0",
		EditedPaths: []string{"x.go"},
		Commands:    []string{"go test ./..."},
		NextVerdict: ReferentUnknown,
	}))
	require.NoError(t, env.svc.RecordTurnOutcome(t.Context(), TurnOutcome{
		SessionID: "s1", TurnID: "t0",
	}))
	c := readTurn(t, env, "s1", "t0")
	require.Equal(t, ReferentUnknown, c.nextVerdict)
	require.JSONEq(t, `["x.go"]`, c.editedPaths,
		"a judged-unknown row keeps its evidence")
}

// hunk_added records the verbatim per-edit pairs — redacted,
// bounded, and span-located at record time (#394).
func TestRecordTurnOutcome_HunkAdded(t *testing.T) {
	env := setupLabelsTest(t)
	// The file reflects the agent's output when the row records.
	require.NoError(t, os.WriteFile(filepath.Join(env.workingDir, "x.go"),
		[]byte("head\nkey := \"val\"\nAPI_KEY=abc123\ntail\n"), 0o644))
	require.NoError(t, env.svc.RecordTurnOutcome(t.Context(), TurnOutcome{
		SessionID:   "s1",
		TurnID:      "t0",
		EditedPaths: []string{"x.go", ".env", "big.go"},
		HunkAdded: map[string][]HunkEdit{
			"x.go":   {{Old: "old", New: "key := \"val\"\nAPI_KEY=abc123"}},
			".env":   {{New: "DATABASE_URL=postgres://u:p@h/db"}},
			"big.go": {{New: strings.Repeat("b", maxHunkPathRunes+10)}},
		},
	}))

	var m map[string]hunkAddedEntry
	require.NoError(t, json.Unmarshal([]byte(readTurn(t, env, "s1", "t0").hunkAdded), &m))
	require.Equal(t, "key := \"val\"\n[REDACTED]",
		m["x.go"].Edits[0].New,
		"a credential assignment must not persist verbatim")
	require.Equal(t, "old", m["x.go"].Edits[0].Old,
		"the pre-edit text is preserved for region location")
	require.Equal(t, [][2]int{{1, 3}}, m["x.go"].Spans,
		"the new text's line span is located at record time")
	require.Equal(t, hunkSensitive, m[".env"].State,
		"a credential-carrying path stores a state, never content")
	require.Empty(t, m[".env"].Edits)
	require.True(t, m["big.go"].Truncated,
		"an edit past the per-path bound is cut and flagged")
}

// The placeholder-repair arm refreshes hunk_added along with the
// rest of the evidence.
func TestRecordTurnOutcome_PlaceholderRepairsHunkAdded(t *testing.T) {
	env := setupLabelsTest(t)
	require.NoError(t, os.WriteFile(filepath.Join(env.workingDir, "x.go"),
		[]byte("added text\n"), 0o644))
	require.NoError(t, env.svc.RecordTurnOutcome(t.Context(), TurnOutcome{
		SessionID: "s1", TurnID: "t0",
	}))
	require.NoError(t, env.svc.RecordTurnOutcome(t.Context(), TurnOutcome{
		SessionID:   "s1",
		TurnID:      "t0",
		EditedPaths: []string{"x.go"},
		HunkAdded:   map[string][]HunkEdit{"x.go": {{New: "added text"}}},
		NextVerdict: ReferentAccepted,
	}))
	require.JSONEq(t,
		`{"x.go":{"edits":[{"new":"added text"}],"spans":[[0,1]]}}`,
		readTurn(t, env, "s1", "t0").hunkAdded)
}

// hunk_after slices the post-window content around the record-time
// spans — the "what the region became" half of the pair. States name
// every non-content outcome.
func TestSettleTurnOutcomes_HunkAfter(t *testing.T) {
	env := setupLabelsTest(t)
	dir := env.workingDir
	write := func(p, content string) {
		require.NoError(t, os.WriteFile(filepath.Join(dir, p), []byte(content), 0o644))
	}
	write("x.go", "head\nagent wrote this\nmid\ntail\n")
	write(".env", "SECRET=x")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "bin.dat"),
		[]byte{'a', 0, 'b'}, 0o644))

	require.NoError(t, env.svc.RecordTurnOutcome(t.Context(), TurnOutcome{
		SessionID: "s1",
		TurnID:    "t0",
		EditedPaths: []string{
			"x.go", "gone.go", "bin.dat", "../esc.go", ".env",
		},
		HunkAdded: map[string][]HunkEdit{
			"x.go":    {{New: "agent wrote this"}},
			"gone.go": {{New: "was there"}},
		},
	}))

	// Post-window: the region is revised.
	write("x.go", "head\nuser rewrote it\nmid\ntail\n")

	dueViaNewerSession(t, env, "s2")
	c := settleTurnRow(t, env, "s1", "t0")

	var m map[string]hunkAfterEntry
	require.NoError(t, json.Unmarshal([]byte(c.hunkAfter), &m))
	require.Equal(t, hunkOK, m["x.go"].State)
	require.Len(t, m["x.go"].Windows, 1)
	require.Contains(t, m["x.go"].Windows[0], "user rewrote it",
		"the window holds what the region became, not the head")
	require.Contains(t, m["x.go"].Windows[0], "head",
		"context slack keeps the region's surroundings")
	require.Equal(t, hunkMissing, m["gone.go"].State,
		"a file that no longer reads records its state, not content")
	require.Equal(t, hunkBinary, m["bin.dat"].State,
		"a NUL byte marks binary content — never persisted as text")
	require.Equal(t, hunkOutOfWorkspace, m["../esc.go"].State,
		"a path escaping the workspace is out of scope, not missing")
	require.Equal(t, hunkSensitive, m[".env"].State)
}

// A span located at record survives the revision at settle — the
// window is cut around the region, not around a matching line. This
// is the fully-rewritten case the fingerprint can never anchor.
func TestSettleTurnOutcomes_HunkAfterRewrittenRegion(t *testing.T) {
	env := setupLabelsTest(t)
	dir := env.workingDir
	body := strings.Repeat("prelude line\n", 60) +
		"AGENT FUNCTION\n" + strings.Repeat("tail line\n", 60)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "x.go"), []byte(body), 0o644))
	require.NoError(t, env.svc.RecordTurnOutcome(t.Context(), TurnOutcome{
		SessionID:   "s1",
		TurnID:      "t0",
		EditedPaths: []string{"x.go"},
		HunkAdded:   map[string][]HunkEdit{"x.go": {{New: "AGENT FUNCTION"}}},
	}))

	// The region is fully rewritten — no agent text remains to
	// match. The stored span still slices the right lines.
	body = strings.Repeat("prelude line\n", 60) +
		"USER REWRITE\n" + strings.Repeat("tail line\n", 60)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "x.go"), []byte(body), 0o644))

	dueViaNewerSession(t, env, "s2")
	var m map[string]hunkAfterEntry
	require.NoError(t, json.Unmarshal(
		[]byte(settleTurnRow(t, env, "s1", "t0").hunkAfter), &m))
	require.Equal(t, hunkOK, m["x.go"].State)
	require.Len(t, m["x.go"].Windows, 1)
	require.Contains(t, m["x.go"].Windows[0], "USER REWRITE",
		"the recorded span cuts the rewritten region even when "+
			"nothing matches the fingerprint")
	require.NotContains(t, m["x.go"].Windows[0], "prelude line\n\n",
		"the window is the region ±context, not the whole file")
}

// A symlink inside the workspace resolving outside is out of scope,
// not readable content — the read goes through os.Root.
func TestSettleTurnOutcomes_HunkAfterSymlinkEscape(t *testing.T) {
	env := setupLabelsTest(t)
	outside := filepath.Join(t.TempDir(), "outside.txt")
	require.NoError(t, os.WriteFile(outside, []byte("escaped"), 0o644))
	require.NoError(t, os.Symlink(outside, filepath.Join(env.workingDir, "link.txt")))
	require.NoError(t, env.svc.RecordTurnOutcome(t.Context(), TurnOutcome{
		SessionID: "s1", TurnID: "t0", EditedPaths: []string{"link.txt"},
	}))
	dueViaNewerSession(t, env, "s2")

	var m map[string]hunkAfterEntry
	require.NoError(t, json.Unmarshal(
		[]byte(settleTurnRow(t, env, "s1", "t0").hunkAfter), &m))
	require.Equal(t, hunkOutOfWorkspace, m["link.txt"].State,
		"a symlink escape is out of scope — the target is never read")
}

// hunk_after windows are redacted and bounded like every other
// durable string; a region line past the path bound flags
// truncation.
func TestSettleTurnOutcomes_HunkAfterBoundAndRedact(t *testing.T) {
	env := setupLabelsTest(t)
	dir := env.workingDir
	// One giant line: the window is span±context, so only an
	// oversized region line can push past the path bound.
	bigLine := "MARKER" + strings.Repeat("x", maxHunkPathRunes+10)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "big.txt"),
		[]byte(bigLine+"\nrest\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "s.txt"),
		[]byte("password=hunter2\nrest"), 0o644))
	require.NoError(t, env.svc.RecordTurnOutcome(t.Context(), TurnOutcome{
		SessionID:   "s1",
		TurnID:      "t0",
		EditedPaths: []string{"big.txt", "s.txt"},
		HunkAdded: map[string][]HunkEdit{
			"big.txt": {{New: bigLine}},
			"s.txt":   {{New: "password=hunter2"}},
		},
	}))
	dueViaNewerSession(t, env, "s2")

	var m map[string]hunkAfterEntry
	require.NoError(t, json.Unmarshal(
		[]byte(settleTurnRow(t, env, "s1", "t0").hunkAfter), &m))
	require.True(t, m["big.txt"].Truncated,
		"a window past the per-path bound flags truncation")
	total := 0
	for _, w := range m["big.txt"].Windows {
		total += len([]rune(w))
	}
	require.LessOrEqual(t, total, maxHunkPathRunes)
	require.Equal(t, "[REDACTED]\nrest", m["s.txt"].Windows[0],
		"the credential never persists; the rest of the region does")
}

// The URL-credentials pattern scrubs connection strings even though
// no sensitive key name is present.
func TestSettleTurnOutcomes_HunkAfterURLCredentials(t *testing.T) {
	env := setupLabelsTest(t)
	require.NoError(t, os.WriteFile(filepath.Join(env.workingDir, "cfg.txt"),
		[]byte("db=postgres://admin:hunter2@db.internal:5432/prod"), 0o644))
	require.NoError(t, env.svc.RecordTurnOutcome(t.Context(), TurnOutcome{
		SessionID:   "s1",
		TurnID:      "t0",
		EditedPaths: []string{"cfg.txt"},
		HunkAdded:   map[string][]HunkEdit{"cfg.txt": {{New: "db="}}},
	}))
	dueViaNewerSession(t, env, "s2")

	var m map[string]hunkAfterEntry
	require.NoError(t, json.Unmarshal(
		[]byte(settleTurnRow(t, env, "s1", "t0").hunkAfter), &m))
	require.Len(t, m["cfg.txt"].Windows, 1)
	require.NotContains(t, m["cfg.txt"].Windows[0], "hunter2")
	require.Contains(t, m["cfg.txt"].Windows[0], "postgres://[REDACTED]@db.internal",
		"the scheme and host survive; the user:pass does not")
}

// A row recorded before this channel existed — edited_paths but no
// hunk_added — has no span to slice: region_unknown, not a file head.
func TestSettleTurnOutcomes_HunkAfterWithoutSpans(t *testing.T) {
	env := setupLabelsTest(t)
	require.NoError(t, os.WriteFile(filepath.Join(env.workingDir, "x.go"), []byte("v1"), 0o644))
	require.NoError(t, env.svc.RecordTurnOutcome(t.Context(), TurnOutcome{
		SessionID:   "s1",
		TurnID:      "t0",
		EditedPaths: []string{"x.go"},
		EditHunks:   map[string][]string{"x.go": {"v1"}},
	}))
	dueViaNewerSession(t, env, "s2")
	c := settleTurnRow(t, env, "s1", "t0")
	require.JSONEq(t, `{}`, c.hunkAdded, "no added text was recorded")
	var m map[string]hunkAfterEntry
	require.NoError(t, json.Unmarshal([]byte(c.hunkAfter), &m))
	require.Equal(t, hunkRegionUnknown, m["x.go"].State,
		"no record-time span means no region — never a misleading head")
	require.Empty(t, m["x.go"].Windows)
}

// A file past maxHunkReadBytes never gets read or scanned — the
// state names the skip, and the fingerprint abstains with it.
func TestSettleTurnOutcomes_HunkAfterTooLarge(t *testing.T) {
	env := setupLabelsTest(t)
	require.NoError(t, os.WriteFile(filepath.Join(env.workingDir, "lock.txt"),
		[]byte(strings.Repeat("x", maxHunkReadBytes+1)), 0o644))
	require.NoError(t, env.svc.RecordTurnOutcome(t.Context(), TurnOutcome{
		SessionID: "s1", TurnID: "t0",
		EditedPaths: []string{"lock.txt"},
		EditHunks:   map[string][]string{"lock.txt": {"x"}},
	}))
	dueViaNewerSession(t, env, "s2")

	c := settleTurnRow(t, env, "s1", "t0")
	var m map[string]hunkAfterEntry
	require.NoError(t, json.Unmarshal([]byte(c.hunkAfter), &m))
	require.Equal(t, hunkTooLarge, m["lock.txt"].State)
	require.Equal(t, TurnUnknown, c.verdict.String,
		"a too-large read abstains from the verdict like a binary")
}

// The row cap is a hard bound on both columns: enough full-size
// paths push the last one to an explicit over_cap state rather than
// silently dropping it or exceeding the limit.
func TestSettleTurnOutcomes_HunkRowCap(t *testing.T) {
	env := setupLabelsTest(t)
	dir := env.workingDir
	var paths []string
	hunks := map[string][]HunkEdit{}
	finger := map[string][]string{}
	// Five paths, one oversized line each: every window truncates
	// at the per-path bound, so four paths exhaust the row cap.
	for i := range 5 {
		p := fmt.Sprintf("c%d.txt", i)
		line := "M" + strings.Repeat("x", maxHunkPathRunes+10)
		require.NoError(t, os.WriteFile(filepath.Join(dir, p), []byte(line), 0o644))
		paths = append(paths, p)
		hunks[p] = []HunkEdit{{New: line}}
		finger[p] = []string{"M"}
	}
	require.NoError(t, env.svc.RecordTurnOutcome(t.Context(), TurnOutcome{
		SessionID: "s1", TurnID: "t0",
		EditedPaths: paths, EditHunks: finger, HunkAdded: hunks,
	}))
	addedRow := readTurn(t, env, "s1", "t0").hunkAdded
	var added map[string]hunkAddedEntry
	require.NoError(t, json.Unmarshal([]byte(addedRow), &added))
	require.Equal(t, hunkOverCap, added["c4.txt"].State,
		"the path past the row cap records over_cap, not silence")
	addedTotal := 0
	for _, e := range added {
		for _, ed := range e.Edits {
			addedTotal += len([]rune(ed.Old)) + len([]rune(ed.New))
		}
	}
	require.LessOrEqual(t, addedTotal, maxHunkRowRunes,
		"the added column never exceeds the row bound")

	dueViaNewerSession(t, env, "s2")
	c := settleTurnRow(t, env, "s1", "t0")
	var after map[string]hunkAfterEntry
	require.NoError(t, json.Unmarshal([]byte(c.hunkAfter), &after))
	require.Equal(t, hunkOverCap, after["c4.txt"].State)
	afterTotal := 0
	for _, e := range after {
		for _, w := range e.Windows {
			afterTotal += len([]rune(w))
		}
	}
	require.LessOrEqual(t, afterTotal, maxHunkRowRunes,
		"the after column never exceeds the row bound")
}

// Source text stores literally — no < > & escaping — so the cell
// stays readable in sqlite3.
func TestSettleTurnOutcomes_HunkAfterNoHTMLEscape(t *testing.T) {
	env := setupLabelsTest(t)
	require.NoError(t, os.WriteFile(filepath.Join(env.workingDir, "x.go"),
		[]byte("if a < b && c {\n}\n"), 0o644))
	require.NoError(t, env.svc.RecordTurnOutcome(t.Context(), TurnOutcome{
		SessionID: "s1", TurnID: "t0",
		EditedPaths: []string{"x.go"},
		HunkAdded:   map[string][]HunkEdit{"x.go": {{New: "if a < b && c {"}}},
	}))
	dueViaNewerSession(t, env, "s2")

	c := settleTurnRow(t, env, "s1", "t0")
	require.Contains(t, c.hunkAfter, "if a < b && c {")
	require.NotContains(t, c.hunkAfter, "\\u003c",
		"json.Marshal would escape the angle bracket")
}

// A later same-session turn that touched the same path flags the
// after cell — the region's content isn't necessarily a user edit.
func TestSettleTurnOutcomes_LaterTurnTouched(t *testing.T) {
	env := setupLabelsTest(t)
	require.NoError(t, os.WriteFile(filepath.Join(env.workingDir, "x.go"),
		[]byte("v1\n"), 0o644))
	require.NoError(t, env.svc.RecordTurnOutcome(t.Context(), TurnOutcome{
		SessionID: "s1", TurnID: "t0",
		EditedPaths: []string{"x.go"},
		HunkAdded:   map[string][]HunkEdit{"x.go": {{New: "v1"}}},
	}))
	require.NoError(t, env.svc.RecordTurnOutcome(t.Context(), TurnOutcome{
		SessionID: "s1", TurnID: "t1",
		EditedPaths: []string{"x.go"},
		CreatedAt:   time.Now().UnixMilli() + 1,
	}))
	dueViaNewerSession(t, env, "s2")

	var m map[string]hunkAfterEntry
	require.NoError(t, json.Unmarshal(
		[]byte(settleTurnRow(t, env, "s1", "t0").hunkAfter), &m))
	require.True(t, m["x.go"].LaterTurnTouched,
		"a later turn on the same path marks the cell")
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// A frozen row refuses even the placeholder-repair arm.
func TestRecordTurnOutcome_FrozenImmutable(t *testing.T) {
	env := setupLabelsTest(t)
	require.NoError(t, env.svc.RecordTurnOutcome(t.Context(), TurnOutcome{
		SessionID:   "s1",
		TurnID:      "t0",
		EditedPaths: []string{"x.go"},
		NextVerdict: ReferentAccepted,
		// Far enough back that the +24h cap has already elapsed.
		CreatedAt: time.Now().Add(-48 * time.Hour).UnixMilli(),
	}))
	require.NoError(t, env.svc.SettleTurnOutcomes(t.Context(), 10))

	require.NoError(t, env.svc.RecordTurnOutcome(t.Context(), TurnOutcome{
		SessionID: "s1", TurnID: "t0", NextVerdict: ReferentRevised,
	}))
	c := readTurn(t, env, "s1", "t0")
	require.Equal(t, ReferentAccepted, c.nextVerdict,
		"a settled row's record is closed — nothing rewrites it")
	require.True(t, c.settledAt.Valid)
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

	dueViaNewerSession(t, env, "s2")
	require.NoError(t, env.svc.SettleTurnOutcomes(t.Context(), 10))
	require.True(t, readTurn(t, env, "s1", "t0").settledAt.Valid)
}

// The newer-session trigger is project-scoped: a session whose only
// membership proof lives under another key must not close this
// project's window — the EXISTS requires a same-project ledger row.
func TestSettleTurnOutcomes_ForeignSessionNotBoundary(t *testing.T) {
	env := setupLabelsTest(t)
	require.NoError(t, env.svc.RecordTurnOutcome(t.Context(), TurnOutcome{
		SessionID: "s1", TurnID: "t0", Commands: []string{"ls"},
	}))

	foreign := NewService(db.New(env.conn), env.workingDir, params.DefaultMemory(),
		WithProjectKey("pk-foreign"))
	seedLabelSessionAt(t, env, "sf", "", time.Now().Unix()+60)
	foreign.RecordRun(t.Context(), Run{
		SessionID: "sf", Command: "ls", CWD: env.workingDir,
		Ran: true, ExitCode: 0,
	})

	require.NoError(t, env.svc.SettleTurnOutcomes(t.Context(), 10))
	require.False(t, readTurn(t, env, "s1", "t0").settledAt.Valid,
		"a foreign-project session must not close this window")
}

// The settle pass itself is project-scoped: another partition's due
// turn rows are not this project's backlog — the same contract the
// episode pass's partition test pins.
func TestSettleTurnOutcomes_ProjectPartition(t *testing.T) {
	conn, err := db.Connect(t.Context(), t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { conn.Close() })
	q := db.New(conn)
	dir := t.TempDir()
	svcA := NewService(q, dir, params.DefaultMemory(), WithProjectKey("proj-a"))
	svcB := NewService(q, dir, params.DefaultMemory(), WithProjectKey("proj-b"))
	e := &labelEnv{conn: conn}

	// Both due via the +24h cap — no newer-session trigger needed.
	old := time.Now().Add(-48 * time.Hour).UnixMilli()
	require.NoError(t, svcA.RecordTurnOutcome(t.Context(), TurnOutcome{
		SessionID: "s1", TurnID: "t0", Commands: []string{"ls"}, CreatedAt: old,
	}))
	require.NoError(t, svcB.RecordTurnOutcome(t.Context(), TurnOutcome{
		SessionID: "s2", TurnID: "t0", Commands: []string{"ls"}, CreatedAt: old,
	}))
	require.NoError(t, svcA.SettleTurnOutcomes(t.Context(), 10))
	require.True(t, readTurn(t, e, "s1", "t0").settledAt.Valid)
	require.False(t, readTurn(t, e, "s2", "t0").settledAt.Valid,
		"proj-b's due row is not proj-a's backlog")
}

// An 'accepted' cue is a weak signal — with no readable artifact
// evidence (the fingerprint never landed, no repo to show a commit,
// no test verdict) there is nothing to accept ON. The row settles
// unknown: missing evidence is never a passing grade.
func TestSettleTurnOutcomes_AcceptedCueNoEvidenceUnknown(t *testing.T) {
	env := setupLabelsTest(t)
	require.NoError(t, env.svc.RecordTurnOutcome(t.Context(), TurnOutcome{
		SessionID:   "s1",
		TurnID:      "t0",
		EditedPaths: []string{"x.go"},
		// No EditHunks entry for the path: survival cannot read,
		// and a TempDir workspace carries no repo or test runs.
		NextVerdict: ReferentAccepted,
	}))
	dueViaNewerSession(t, env, "s2")
	require.Equal(t, TurnUnknown, settleTurnRow(t, env, "s1", "t0").verdict.String,
		"an accepted cue with zero decidable evidence settles unknown")
}

// Turn hunks carry the same trimmed fingerprint — indented code must
// survive there too.
func TestSettleTurnOutcomes_IndentedLinesSurvive(t *testing.T) {
	env := setupLabelsTest(t)
	require.NoError(t, os.WriteFile(filepath.Join(env.workingDir, "x.go"),
		[]byte("package x\n\nfunc f() {\n\treturn 1\n}\n"), 0o644))
	require.NoError(t, env.svc.RecordTurnOutcome(t.Context(), TurnOutcome{
		SessionID:   "s1",
		TurnID:      "t0",
		EditedPaths: []string{"x.go"},
		EditHunks:   map[string][]string{"x.go": {"return 1"}},
		NextVerdict: ReferentAccepted,
	}))
	dueViaNewerSession(t, env, "s2")
	require.Equal(t, TurnAccepted, settleTurnRow(t, env, "s1", "t0").verdict.String,
		"indented lines survive — raw-content probing read them as diverged")
}

// The backfill corpus is the project's own: a session with ledger
// rows only in another partition, or with no rows at all, never
// enters it — otherwise its turns would be written under this
// project's key and settle diverged on missing paths.
func TestListRecentSessionIDs_ProjectPartition(t *testing.T) {
	conn, err := db.Connect(t.Context(), t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { conn.Close() })
	q := db.New(conn)
	dir := t.TempDir()
	svcA := NewService(q, dir, params.DefaultMemory(), WithProjectKey("proj-a"))
	svcB := NewService(q, dir, params.DefaultMemory(), WithProjectKey("proj-b"))
	e := &labelEnv{conn: conn}

	seedLabelSession(t, e, "live", "")
	seedLabelSessionAt(t, e, "own", "", 50)
	seedLabelSessionAt(t, e, "foreign", "", 60)
	seedLabelSessionAt(t, e, "empty", "", 70)
	seedLabelSessionAt(t, e, "scanned", "", 80)
	// command_memory upserts by (project_key, cmd_norm) and keeps
	// only the latest last_session_id — distinct commands so each
	// session keeps its own membership proof.
	svcA.RecordRun(t.Context(), Run{SessionID: "own", Command: "ls", CWD: dir, Ran: true, ExitCode: 0})
	svcB.RecordRun(t.Context(), Run{SessionID: "foreign", Command: "pwd", CWD: dir, Ran: true, ExitCode: 0})
	svcA.RecordRun(t.Context(), Run{SessionID: "scanned", Command: "git status", CWD: dir, Ran: true, ExitCode: 0})
	// The corpus admits only sessions with an unrecorded real-user
	// turn — "own" and "foreign" carry one message each; "scanned"
	// has its turn already recorded, so it exits.
	userMsg := func(sessionID, id string) {
		_, err := conn.ExecContext(t.Context(),
			`INSERT INTO messages (id, session_id, role, parts, created_at, updated_at)
			 VALUES (?, ?, 'user', '[]', 1, 1)`, id, sessionID)
		require.NoError(t, err)
	}
	userMsg("own", "um1")
	userMsg("foreign", "um2")
	userMsg("scanned", "um3")
	require.NoError(t, svcA.RecordTurnOutcome(t.Context(), TurnOutcome{
		SessionID: "scanned", TurnID: "um3",
	}))

	rows, err := svcA.ListRecentSessionIDs(t.Context(), "live", 10)
	require.NoError(t, err)
	got := make([]string, 0, len(rows))
	for _, r := range rows {
		got = append(got, r.ID)
	}
	require.Equal(t, []string{"own"}, got,
		"foreign-keyed, row-less, message-less, and fully-scanned sessions are not this project's corpus")
}
