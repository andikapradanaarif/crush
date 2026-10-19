package cmd

// `crush stats trend` — the weekly personal trend report (#403,
// roadmap T0.4 / P5 instrument). One row per ISO week per project,
// computed read-only from the project's crush.db and appended to a
// local JSONL ledger. Trends are reported as trends — no causal
// claim, no upload.
//
// Metric definitions (all bucketed by the row's own timestamp, week
// = Monday-start UTC):
//
//   - settled turn mix / revision rate: turn_outcomes rows with
//     settled_at IS NOT NULL, bucketed by created_at. revision_rate
//     = revised / (accepted + revised); null when empty.
//   - edit cost: for verdicts accepted|revised, per edited path a
//     comparable pair = hunk_added state ok + untruncated AND
//     hunk_after state ok + untruncated + not later_turn_touched,
//     stale, relocated or drifted. cost = line-level Levenshtein
//     between the turn's recorded `new` text and the post-window
//     region content (context padding stripped via the recorded
//     spans). edit_cost_per_turn divides by edit_turns — turns that
//     produced >= 1 comparable pair — so coverage can't masquerade
//     as cheap edits (coverage reads as edit_turns/(accepted+revised));
//     null when nothing measured. Excluded paths are counted per
//     reason; a row whose hunk maps are both empty predates the
//     capture columns and excludes as pre_capture, not no_cell.
//     Tables or columns a project's schema predates are reported
//     under `missing`, not read as zeros.
//   - tokens per user turn: sessions created in the window
//     (parent_session_id IS NULL) — sum(prompt+completion tokens) /
//     user-role messages in those sessions. In a shared data dir
//     (#360) sessions are scoped to ones carrying this project's
//     ledger key; a single-tenant store counts all.
//   - repeat-failure rate: session_command_runs with last_exit != 0
//     where the same (cmd_norm, cwd) had an earlier failing session
//     AND a passing run in between — failure_memory's resolve ->
//     re-open semantics. Rows are per-session last exits, so a
//     fail-then-pass inside one session reads as its last exit.
//   - harness_bytes: rendered lessons/preferences/skills size — 0
//     until P5 lands (#229); reserved so the ledger shape is stable.

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/charmbracelet/crush/internal/cmdlog"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/db"
	"github.com/charmbracelet/crush/internal/filepathext"
	"github.com/charmbracelet/crush/internal/projects"
	"github.com/spf13/cobra"
)

const (
	// trendLedgerName is the per-project ledger file next to crush.db.
	trendLedgerName = "trend.jsonl"
	// maxTrendDiffLines bounds the edit-cost DP — the writer-side row
	// caps make larger pairs rare enough to exclude instead.
	maxTrendDiffLines = 2048
)

var trendCmd = &cobra.Command{
	Use:   "trend",
	Args:  cobra.NoArgs,
	Short: "Weekly personal trend report",
	Long: "Print and record one trend row per week per project: turn verdict mix, " +
		"revision rate, edit cost over captured hunks, tokens per user turn, " +
		"repeat-failure rate, harness growth (#403).",
	RunE: runStatsTrend,
}

func init() {
	trendCmd.Flags().String("project", "", "Limit to one registered project (path or basename)")
	trendCmd.Flags().Bool("all", false, "Report on all registered projects (from projects.json)")
	trendCmd.Flags().String("since", "all", "Lower bound: 7d, 4w, 720h, or YYYY-MM-DD")
	trendCmd.Flags().String("out", "", "Ledger path override (default <data-dir>/trend.jsonl)")
	trendCmd.Flags().Bool("no-append", false, "Print only; do not write the ledger")
	statsCmd.AddCommand(trendCmd)
}

// trendRow is one project-week line in the ledger. Rate fields are
// pointers so an empty denominator marshals as null, never a
// false zero.
type trendRow struct {
	V                 int            `json:"v"`
	Project           string         `json:"project"`
	ProjectKey        string         `json:"project_key"`
	Week              string         `json:"week"`
	SettledTurns      int            `json:"settled_turns"`
	Accepted          int            `json:"accepted"`
	Revised           int            `json:"revised"`
	Failed            int            `json:"failed"`
	Unknown           int            `json:"unknown"`
	RevisionRate      *float64       `json:"revision_rate"`
	EditPairs         int            `json:"edit_pairs"`
	EditTurns         int            `json:"edit_turns"`
	EditCostLines     int            `json:"edit_cost_lines"`
	EditCostPerTurn   *float64       `json:"edit_cost_per_turn"`
	EditCostExcluded  map[string]int `json:"edit_cost_excluded,omitempty"`
	Sessions          int            `json:"sessions"`
	UserTurns         int            `json:"user_turns"`
	TokensTotal       int64          `json:"tokens_total"`
	TokensPerTurn     *float64       `json:"tokens_per_turn"`
	CmdFailures       int            `json:"cmd_failures"`
	RepeatFailures    int            `json:"repeat_failures"`
	RepeatFailureRate *float64       `json:"repeat_failure_rate"`
	HarnessBytes      int            `json:"harness_bytes"`
	// Missing names the metric sources absent from this DB — a
	// project whose schema predates a table reports the rest and
	// says so, rather than failing or reading zeros as data.
	Missing []string `json:"missing,omitempty"`
}

// trendProject is one resolved report target.
type trendProject struct {
	path    string
	dataDir string
	key     string
}

// Mirror of cmdlog's hunk cells (its types are unexported; the JSON
// shape is the contract — marshalNoEscape wrote it).
type trendHunkEdit struct {
	New string `json:"new"`
}

type trendHunkSpan struct {
	Start int `json:"start"`
	End   int `json:"end"`
}

type trendAddedCell struct {
	State     string          `json:"state"`
	Truncated bool            `json:"truncated"`
	Edits     []trendHunkEdit `json:"edits"`
	Spans     []trendHunkSpan `json:"spans"`
}

type trendHunkWindow struct {
	Start int    `json:"start"`
	Text  string `json:"text"`
}

type trendAfterCell struct {
	State            string            `json:"state"`
	Windows          []trendHunkWindow `json:"windows"`
	Truncated        bool              `json:"truncated"`
	LaterTurnTouched bool              `json:"later_turn_touched"`
	Relocated        bool              `json:"relocated"`
	Drifted          bool              `json:"drifted"`
	Stale            bool              `json:"stale"`
}

func runStatsTrend(cmd *cobra.Command, _ []string) error {
	ctx := cmd.Context()
	sinceArg, _ := cmd.Flags().GetString("since")
	since, err := parseTrendSince(sinceArg)
	if err != nil {
		return err
	}
	targets, err := resolveTrendProjects(cmd)
	if err != nil {
		return err
	}
	noAppend, _ := cmd.Flags().GetBool("no-append")
	outOverride, _ := cmd.Flags().GetString("out")

	for _, tp := range targets {
		dbPath := filepath.Join(tp.dataDir, "crush.db")
		conn, err := db.ConnectReadOnly(ctx, dbPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "warn: skipping %s: %v\n", dbPath, err)
			continue
		}
		rows, missing, err := gatherTrend(ctx, conn, tp, since)
		conn.Close()
		if err != nil {
			fmt.Fprintf(os.Stderr, "warn: trend failed for %s: %v\n", tp.path, err)
			continue
		}
		printTrendTable(os.Stdout, tp.path, rows, missing)
		if !noAppend && len(rows) > 0 {
			ledger := outOverride
			if ledger == "" {
				ledger = filepath.Join(tp.dataDir, trendLedgerName)
			}
			if err := appendTrendLedger(ledger, tp.key, rows); err != nil {
				return fmt.Errorf("failed to write trend ledger: %w", err)
			}
			fmt.Printf("Ledger updated: %s\n", ledger)
		}
	}
	return nil
}

// resolveTrendProjects applies the same convention as `crush stats`:
// default = the current directory's project, --project = one
// registered project, --all = every registered project that has a
// crush.db.
func resolveTrendProjects(cmd *cobra.Command) ([]trendProject, error) {
	projectArg, _ := cmd.Flags().GetString("project")
	useAll, _ := cmd.Flags().GetBool("all")
	dataDirFlag, _ := cmd.Flags().GetString("data-dir")

	if projectArg == "" && !useAll {
		cwd, err := ResolveCwd(cmd)
		if err != nil {
			return nil, err
		}
		dataDir := dataDirFlag
		if dataDir == "" {
			cfg, err := config.Init(cmd.Context(), cwd, "", false)
			if err != nil {
				return nil, fmt.Errorf("failed to initialize config: %w", err)
			}
			dataDir = cfg.Config().Options.DataDirectory
		}
		return []trendProject{{
			path:    cwd,
			dataDir: dataDir,
			key:     cmdlog.ProjectKeyForDir(cwd),
		}}, nil
	}

	list, err := projects.List()
	if err != nil {
		return nil, fmt.Errorf("failed to load projects: %w", err)
	}
	var out []trendProject
	for _, p := range list {
		if _, err := os.Stat(filepath.Join(p.DataDir, "crush.db")); err != nil {
			continue
		}
		if projectArg != "" &&
			filepathext.Canonical(p.Path) != filepathext.Canonical(projectArg) &&
			filepath.Base(p.Path) != projectArg {
			continue
		}
		out = append(out, trendProject{
			path:    p.Path,
			dataDir: p.DataDir,
			key:     cmdlog.ProjectKeyForDir(p.Path),
		})
	}
	if len(out) == 0 {
		if projectArg != "" {
			return nil, fmt.Errorf("no registered project matches %q", projectArg)
		}
		return nil, fmt.Errorf("no registered projects with a crush.db")
	}
	return out, nil
}

// parseTrendSince resolves --since into a millisecond lower bound.
// "all" is zero; Nd/Nw/Nh count back from now; YYYY-MM-DD is a date.
func parseTrendSince(s string) (int64, error) {
	s = strings.TrimSpace(strings.ToLower(s))
	if s == "" || s == "all" {
		return 0, nil
	}
	if d, err := time.Parse("2006-01-02", s); err == nil {
		return d.UnixMilli(), nil
	}
	if len(s) < 2 {
		return 0, fmt.Errorf("invalid --since %q (want 7d, 4w, 720h, YYYY-MM-DD, all)", s)
	}
	n, err := strconv.ParseInt(s[:len(s)-1], 10, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("invalid --since %q (want 7d, 4w, 720h, YYYY-MM-DD, all)", s)
	}
	var span time.Duration
	switch s[len(s)-1] {
	case 'd':
		span = time.Duration(n) * 24 * time.Hour
	case 'w':
		span = time.Duration(n) * 7 * 24 * time.Hour
	case 'h':
		span = time.Duration(n) * time.Hour
	default:
		return 0, fmt.Errorf("invalid --since %q (want 7d, 4w, 720h, YYYY-MM-DD, all)", s)
	}
	return time.Now().Add(-span).UnixMilli(), nil
}

// trendWeekStart floors a millisecond timestamp to its ISO week's
// Monday (UTC) and labels it YYYY-MM-DD.
func trendWeekStart(ms int64) string {
	d := time.UnixMilli(ms).UTC()
	monday := d.AddDate(0, 0, -(int(d.Weekday())+6)%7)
	return monday.Format("2006-01-02")
}

// trendSchema is what this DB actually has — a project that never
// ran a newer binary is missing the younger tables/columns, and the
// report degrades metric-by-metric instead of failing.
type trendSchema struct {
	turns     bool
	hunkCells bool
	cmdRuns   bool
	// memberTables are the keyed tables usable for session
	// membership scoping in a shared store.
	memberTables []string
}

func hasTable(ctx context.Context, conn *sql.DB, name string) bool {
	var n int
	if err := conn.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`,
		name).Scan(&n); err != nil {
		return false
	}
	return n > 0
}

func hasColumn(ctx context.Context, conn *sql.DB, table, col string) bool {
	rows, err := conn.QueryContext(ctx, `PRAGMA table_info(`+table+`)`)
	if err != nil {
		return false
	}
	defer rows.Close()
	found := false
	for rows.Next() {
		var cid int
		var name, typ string
		var notnull, pk int
		var dflt any
		if rows.Scan(&cid, &name, &typ, &notnull, &dflt, &pk) == nil && name == col {
			found = true
		}
	}
	return found && rows.Err() == nil
}

func probeTrendSchema(ctx context.Context, conn *sql.DB) trendSchema {
	var s trendSchema
	if s.turns = hasTable(ctx, conn, "turn_outcomes"); s.turns {
		s.hunkCells = hasColumn(ctx, conn, "turn_outcomes", "hunk_added") &&
			hasColumn(ctx, conn, "turn_outcomes", "hunk_after")
	}
	s.cmdRuns = hasTable(ctx, conn, "session_command_runs")
	// The membership probe's keyed tables — the same set the settle
	// pass's session-membership EXISTS uses, plus turn outcomes.
	// command_memory keys sessions under last_session_id; the rest
	// use session_id.
	for t, col := range map[string]string{
		"turn_outcomes":        "session_id",
		"session_command_runs": "session_id",
		"command_memory":       "last_session_id",
		"failure_memory":       "session_id",
		"referent_episodes":    "session_id",
	} {
		if hasTable(ctx, conn, t) && hasColumn(ctx, conn, t, "project_key") {
			s.memberTables = append(s.memberTables, t+"."+col)
		}
	}
	slices.Sort(s.memberTables)
	return s
}

// sharedStore reports whether the DB mixes project partitions — more
// than one non-empty project_key across the keyed tables (#360).
// Single-tenant stores count every session; shared ones scope
// sessions to rows carrying this project's key. keyed entries are
// "table.session_col".
func sharedStore(ctx context.Context, conn *sql.DB, keyed []string) (bool, error) {
	if len(keyed) == 0 {
		return false, nil
	}
	var unions []string
	for _, tc := range keyed {
		t, _, _ := strings.Cut(tc, ".")
		unions = append(unions,
			fmt.Sprintf(`SELECT DISTINCT project_key FROM %s WHERE project_key != ''`, t))
	}
	var n int
	err := conn.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM (`+strings.Join(unions, " UNION ")+`)`).Scan(&n)
	return n > 1, err
}

func gatherTrend(ctx context.Context, conn *sql.DB, tp trendProject, sinceMs int64) ([]trendRow, []string, error) {
	schema := probeTrendSchema(ctx, conn)
	shared, err := sharedStore(ctx, conn, schema.memberTables)
	if err != nil {
		return nil, nil, fmt.Errorf("detect partitions: %w", err)
	}
	var missing []string
	if !schema.turns {
		missing = append(missing, "turn_outcomes")
	} else if !schema.hunkCells {
		missing = append(missing, "turn_outcomes.hunk_cells")
	}
	if !schema.cmdRuns {
		missing = append(missing, "session_command_runs")
	}

	weeks := map[string]*trendRow{}
	bucket := func(ms int64) *trendRow {
		w := trendWeekStart(ms)
		r, ok := weeks[w]
		if !ok {
			r = &trendRow{
				V:          1,
				Project:    tp.path,
				ProjectKey: tp.key,
				Week:       w,
				Missing:    missing,
			}
			weeks[w] = r
		}
		return r
	}

	if schema.turns {
		if err := trendTurns(ctx, conn, tp.key, sinceMs, schema.hunkCells, bucket); err != nil {
			return nil, nil, err
		}
	}
	if err := trendTokens(ctx, conn, tp.key, shared, schema.memberTables, sinceMs, bucket); err != nil {
		return nil, nil, err
	}
	if schema.cmdRuns {
		if err := trendFailures(ctx, conn, tp.key, sinceMs, bucket); err != nil {
			return nil, nil, err
		}
	}

	keys := make([]string, 0, len(weeks))
	for k := range weeks {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	rows := make([]trendRow, 0, len(keys))
	for _, k := range keys {
		finalizeTrendRow(weeks[k])
		rows = append(rows, *weeks[k])
	}
	return rows, missing, nil
}

// trendTurns buckets settled turn_outcomes and runs the edit-cost
// pass over their hunk pairs. A DB whose turn_outcomes predates the
// hunk columns still yields the verdict mix — every eligible path is
// excluded as pre_capture.
func trendTurns(ctx context.Context, conn *sql.DB, key string, sinceMs int64, hunkCells bool, bucket func(int64) *trendRow) error {
	hunkCols := ", hunk_added, hunk_after"
	if !hunkCells {
		hunkCols = ""
	}
	rows, err := conn.QueryContext(ctx, `SELECT created_at, verdict, edited_paths`+hunkCols+`
		FROM turn_outcomes
		WHERE project_key = ? AND settled_at IS NOT NULL AND created_at >= ?
		ORDER BY created_at ASC`, key, sinceMs)
	if err != nil {
		return fmt.Errorf("turn outcomes: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var created int64
		var verdict, pathsJSON, addedJSON, afterJSON string
		var scanErr error
		if hunkCells {
			scanErr = rows.Scan(&created, &verdict, &pathsJSON, &addedJSON, &afterJSON)
		} else {
			scanErr = rows.Scan(&created, &verdict, &pathsJSON)
		}
		if scanErr != nil {
			return fmt.Errorf("turn outcomes scan: %w", scanErr)
		}
		r := bucket(created)
		r.SettledTurns++
		switch verdict {
		case cmdlog.TurnAccepted:
			r.Accepted++
		case cmdlog.TurnRevised:
			r.Revised++
		case cmdlog.TurnFailed:
			r.Failed++
		default:
			r.Unknown++
		}
		if verdict != cmdlog.TurnAccepted && verdict != cmdlog.TurnRevised {
			continue
		}
		if !hunkCells {
			if r.EditCostExcluded == nil {
				r.EditCostExcluded = map[string]int{}
			}
			var paths []string
			_ = json.Unmarshal([]byte(pathsJSON), &paths)
			r.EditCostExcluded["pre_capture"] += len(paths)
			continue
		}
		if trendEditCost(r, pathsJSON, addedJSON, afterJSON) {
			r.EditTurns++
		}
	}
	return rows.Err()
}

// trendEditCost accumulates the per-path line-Levenshtein of one
// turn's (agent output, settled region) pairs into the week row —
// true when at least one path compared. Every non-comparable path
// lands in EditCostExcluded under its reason.
func trendEditCost(r *trendRow, pathsJSON, addedJSON, afterJSON string) bool {
	var paths []string
	var added map[string]trendAddedCell
	var after map[string]trendAfterCell
	if json.Unmarshal([]byte(pathsJSON), &paths) != nil {
		paths = nil
	}
	// Cells written by cmdlog are JSON-or-empty; a corrupt cell reads
	// as no evidence, same convention as the settle pass.
	_ = json.Unmarshal([]byte(addedJSON), &added)
	_ = json.Unmarshal([]byte(afterJSON), &after)

	excl := func(reason string) {
		if r.EditCostExcluded == nil {
			r.EditCostExcluded = map[string]int{}
		}
		r.EditCostExcluded[reason]++
	}

	if len(added) == 0 && len(after) == 0 {
		// Settlement always writes an after-cell per edited path, so
		// both maps empty means the row predates the capture columns
		// — report that era, not a key miss.
		for range paths {
			excl("pre_capture")
		}
		return false
	}

	any := false
	for _, p := range paths {
		a, aOK := added[p]
		f, fOK := after[p]
		switch {
		case !aOK && !fOK:
			excl("no_cell")
			continue
		case a.State == "sensitive" || f.State == "sensitive":
			excl("sensitive")
			continue
		case !aOK:
			excl("no_added_cell")
			continue
		case a.State != "ok":
			excl("added_" + a.State)
			continue
		case a.Truncated:
			excl("truncated")
			continue
		case !fOK:
			excl("no_after_cell")
			continue
		case f.State != "ok":
			excl("after_" + f.State)
			continue
		case f.Truncated:
			excl("truncated")
			continue
		case f.LaterTurnTouched:
			excl("later_turn_touched")
			continue
		case f.Stale:
			excl("stale")
			continue
		case f.Relocated || f.Drifted:
			excl("unverified_position")
			continue
		}
		dist, ok := trendPairCost(a, f)
		if !ok {
			excl("diff_too_large")
			continue
		}
		r.EditPairs++
		r.EditCostLines += dist
		any = true
	}
	return any
}

// trendPairCost diffs one path's pair: the turn's `new` text against
// the settled region — the recorded spans' content with the capture's
// context padding removed. Both sides are diffed at line level.
func trendPairCost(a trendAddedCell, f trendAfterCell) (int, bool) {
	var added []string
	for _, e := range a.Edits {
		added = append(added, splitTrendLines(e.New)...)
	}

	var region []string
	for _, w := range f.Windows {
		lines := splitTrendLines(w.Text)
		// The window was cut from expanded, merged spans; a recorded
		// span intersecting the window belongs to it (a disjoint
		// span's expanded range would have merged it in otherwise).
		lo, hi := len(lines), 0
		for _, s := range a.Spans {
			if s.End <= w.Start || s.Start >= w.Start+len(lines) {
				continue
			}
			lo = min(lo, s.Start-w.Start)
			hi = max(hi, s.End-w.Start)
		}
		if hi <= lo {
			// No recorded span maps inside — defensive; windows are
			// built from spans so this shouldn't occur on real rows.
			continue
		}
		region = append(region, lines[max(lo, 0):min(hi, len(lines))]...)
	}
	return levenshteinLines(added, region)
}

// splitTrendLines splits stored text into lines, dropping the single
// trailing empty artifact a final newline leaves — so a pure
// deletion's empty `new` diffs as 0, not 1.
func splitTrendLines(s string) []string {
	if s == "" {
		return nil
	}
	lines := strings.Split(s, "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

// levenshteinLines is a two-row line-level edit distance — the
// CIPHER-style cost of turning the agent's output into what the
// region became. Pairs beyond maxTrendDiffLines report !ok rather
// than pay the O(n·m).
func levenshteinLines(a, b []string) (int, bool) {
	if len(a) > maxTrendDiffLines || len(b) > maxTrendDiffLines {
		return 0, false
	}
	if len(a) == 0 {
		return len(b), true
	}
	if len(b) == 0 {
		return len(a), true
	}
	prev := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i, x := range a {
		cur := make([]int, len(b)+1)
		cur[0] = i + 1
		for j, y := range b {
			cost := 1
			if x == y {
				cost = 0
			}
			cur[j+1] = min(min(cur[j]+1, prev[j+1]+1), prev[j]+cost)
		}
		prev = cur
	}
	return prev[len(b)], true
}

// trendTokens buckets session counters and user-turn counts. In a
// shared store only sessions carrying this project's ledger key
// count; in a single-tenant store every top-level session does.
func trendTokens(ctx context.Context, conn *sql.DB, key string, shared bool, memberTables []string, sinceMs int64, bucket func(int64) *trendRow) error {
	query := `SELECT s.created_at, s.prompt_tokens + s.completion_tokens,
		(SELECT COUNT(*) FROM messages m WHERE m.session_id = s.id AND m.role = 'user')
		FROM sessions s
		WHERE s.parent_session_id IS NULL AND s.created_at >= ?`
	args := []any{sinceMs / 1000}
	if shared && len(memberTables) > 0 {
		var ors []string
		for _, tc := range memberTables {
			t, col, _ := strings.Cut(tc, ".")
			ors = append(ors, fmt.Sprintf(
				`EXISTS (SELECT 1 FROM %s WHERE %s = s.id AND project_key = ?)`, t, col))
			args = append(args, key)
		}
		query += ` AND (` + strings.Join(ors, " OR ") + `)`
	}

	rows, err := conn.QueryContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("sessions: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var createdSec, tokens, userTurns int64
		if err := rows.Scan(&createdSec, &tokens, &userTurns); err != nil {
			return fmt.Errorf("sessions scan: %w", err)
		}
		r := bucket(createdSec * 1000)
		r.Sessions++
		r.UserTurns += int(userTurns)
		r.TokensTotal += tokens
	}
	return rows.Err()
}

// trendFailures walks session_command_runs per (cmd_norm, cwd)
// timeline: a failing row is a repeat when the command had failed in
// an earlier session and passed at least once since — the same
// open/resolved/re-opened shape failure_memory keeps collapsed.
func trendFailures(ctx context.Context, conn *sql.DB, key string, sinceMs int64, bucket func(int64) *trendRow) error {
	rows, err := conn.QueryContext(ctx, `SELECT cmd_norm, cwd, last_exit, last_at
		FROM session_command_runs WHERE project_key = ?
		ORDER BY cmd_norm, cwd, last_at`, key)
	if err != nil {
		return fmt.Errorf("command runs: %w", err)
	}
	defer rows.Close()

	type state struct {
		everFailed bool
		open       bool
	}
	states := map[string]*state{}
	for rows.Next() {
		var cmdNorm, cwd string
		var exit, lastAt int64
		if err := rows.Scan(&cmdNorm, &cwd, &exit, &lastAt); err != nil {
			return fmt.Errorf("command runs scan: %w", err)
		}
		k := cmdNorm + "\x00" + cwd
		st, ok := states[k]
		if !ok {
			st = &state{}
			states[k] = st
		}
		if exit != 0 {
			if lastAt >= sinceMs {
				r := bucket(lastAt)
				r.CmdFailures++
				if st.everFailed && !st.open {
					r.RepeatFailures++
				}
			}
			st.everFailed, st.open = true, true
		} else {
			st.open = false
		}
	}
	return rows.Err()
}

// finalizeTrendRow derives the rate fields once the week's raw
// counts are in. Null rates on empty denominators stay null.
func finalizeTrendRow(r *trendRow) {
	if n := r.Accepted + r.Revised; n > 0 {
		rev := float64(r.Revised) / float64(n)
		r.RevisionRate = &rev
	}
	// cost/turn is per measured turn — dividing by all eligible turns
	// would fold coverage into the cost signal; coverage reads as
	// edit_turns/(accepted+revised). Null when nothing measured.
	if r.EditTurns > 0 {
		cost := float64(r.EditCostLines) / float64(r.EditTurns)
		r.EditCostPerTurn = &cost
	}
	if r.UserTurns > 0 {
		v := float64(r.TokensTotal) / float64(r.UserTurns)
		r.TokensPerTurn = &v
	}
	if r.CmdFailures > 0 {
		v := float64(r.RepeatFailures) / float64(r.CmdFailures)
		r.RepeatFailureRate = &v
	}
}

// appendTrendLedger writes the week's rows to the JSONL ledger,
// replacing prior rows for the same (project_key, week) — a re-run
// regenerates a week rather than duplicating it.
func appendTrendLedger(path, key string, rows []trendRow) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	regen := map[string]bool{}
	for _, r := range rows {
		regen[r.Week] = true
	}
	var kept []string
	if f, err := os.Open(path); err == nil {
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for sc.Scan() {
			line := sc.Text()
			var prior struct {
				ProjectKey string `json:"project_key"`
				Week       string `json:"week"`
			}
			if json.Unmarshal([]byte(line), &prior) == nil &&
				prior.ProjectKey == key && regen[prior.Week] {
				continue
			}
			kept = append(kept, line)
		}
		f.Close()
		if err := sc.Err(); err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
		return err
	}

	var b strings.Builder
	for _, l := range kept {
		b.WriteString(l)
		b.WriteByte('\n')
	}
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	for _, r := range rows {
		if err := enc.Encode(r); err != nil {
			return err
		}
	}
	// tmp+rename: the ledger is the durable artifact, so a mid-write
	// crash must not leave it torn.
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(b.String()), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func printTrendTable(w io.Writer, project string, rows []trendRow, missing []string) {
	if len(rows) == 0 {
		fmt.Fprintf(w, "Trend — %s: no rows in window\n", project)
		if len(missing) > 0 {
			fmt.Fprintf(w, "  missing sources: %s\n\n", strings.Join(missing, ", "))
		}
		return
	}
	fmt.Fprintf(w, "Trend — %s\n", project)
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "week\tsettled\tacc\trev\tfail\tunk\trev%\tcost/turn\tpairs\ttok/turn\tfails\trep%")
	for _, r := range rows {
		fmt.Fprintf(tw, "%s\t%d\t%d\t%d\t%d\t%d\t%s\t%s\t%d\t%s\t%d\t%s\n",
			r.Week, r.SettledTurns, r.Accepted, r.Revised, r.Failed, r.Unknown,
			pctStr(r.RevisionRate), numStr(r.EditCostPerTurn), r.EditPairs,
			numStr(r.TokensPerTurn), r.CmdFailures, pctStr(r.RepeatFailureRate))
	}
	tw.Flush()

	// Exclusion totals make the edit-cost coverage legible per the
	// issue's "document which rows are excluded" requirement.
	excl := map[string]int{}
	for _, r := range rows {
		for reason, n := range r.EditCostExcluded {
			excl[reason] += n
		}
	}
	if len(excl) > 0 {
		reasons := make([]string, 0, len(excl))
		for reason := range excl {
			reasons = append(reasons, reason)
		}
		slices.Sort(reasons)
		parts := make([]string, 0, len(reasons))
		for _, reason := range reasons {
			parts = append(parts, fmt.Sprintf("%s:%d", reason, excl[reason]))
		}
		fmt.Fprintf(w, "  edit-cost exclusions: %s\n", strings.Join(parts, "  "))
	}
	if len(missing) > 0 {
		fmt.Fprintf(w, "  missing sources: %s\n", strings.Join(missing, ", "))
	}
	fmt.Fprintln(w)
}

func pctStr(f *float64) string {
	if f == nil {
		return "-"
	}
	return fmt.Sprintf("%.0f%%", *f*100)
}

func numStr(f *float64) string {
	if f == nil {
		return "-"
	}
	return fmt.Sprintf("%.1f", *f)
}
