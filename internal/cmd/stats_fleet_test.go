package cmd

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/charmbracelet/crush/internal/cmdlog"
	"github.com/charmbracelet/crush/internal/db"
	"github.com/charmbracelet/crush/internal/projects"
	"github.com/stretchr/testify/require"
)

// insertFleetTurn stamps a turn row with the workdir column the
// fleet era split reads — "" reproduces pre-#433 rows.
func insertFleetTurn(t *testing.T, conn *sql.DB, key, sid, tid, verdict string,
	createdMs int64, settled bool, workdir, paths, added string,
) {
	t.Helper()
	var settledAt any
	if settled {
		settledAt = createdMs + 3600_000
	}
	trendExec(t, conn, `INSERT INTO turn_outcomes
		(session_id, turn_id, project_key, edited_paths, edit_hunks,
		 commands, hunk_added, hunk_after, next_verdict, verdict,
		 settle_at, settled_at, param_version, created_at, workdir)
		VALUES (?, ?, ?, ?, '[]', '[]', ?, '{}', '', ?, ?, ?, 'pv0', ?, ?)`,
		sid, tid, key, paths, added, verdict, createdMs+86_400_000,
		settledAt, createdMs, workdir)
}

func seedRepairMessage(t *testing.T, conn *sql.DB, sid, mid string) {
	t.Helper()
	trendExec(t, conn, `INSERT OR IGNORE INTO sessions
		(id, title, prompt_tokens, completion_tokens, cost, created_at, updated_at)
		VALUES (?, 's', 0, 0, 0, 1, 1)`, sid)
	trendExec(t, conn, `INSERT INTO messages
		(id, session_id, role, parts, created_at, updated_at)
		VALUES (?, ?, 'user', ?, 1, 1)`, mid, sid,
		`[{"type":"text","data":{"text":"Observed failures still open: rerun the check"}}]`)
}

func TestGatherFleetHealth(t *testing.T) {
	t.Parallel()
	conn, err := db.Connect(t.Context(), t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { conn.Close() })

	okCell := `{"x.go":{"state":"ok","edits":[{"new":"kept"}]}}`
	oowCell := `{"x.go":{"state":"out_of_workspace"}}`
	paths := `["x.go"]`

	// Pre-fix real-user settled rows, one per verdict.
	insertFleetTurn(t, conn, "mine", "s", "t-a", "accepted", weekAms, true, "", paths, okCell)
	insertFleetTurn(t, conn, "mine", "s", "t-r", "revised", weekAms, true, "", paths, oowCell)
	insertFleetTurn(t, conn, "mine", "s", "t-f", "failed", weekAms, true, "", paths, oowCell)
	insertFleetTurn(t, conn, "mine", "s", "t-u", "unknown", weekAms, true, "", `[]`, `{}`)
	// Post-fix rows: one pending ok, one settled with a mixed pair.
	insertFleetTurn(t, conn, "mine", "s", "t-p", "", weekAms, false, "/repo", paths, okCell)
	insertFleetTurn(t, conn, "mine", "s", "t-w", "accepted", weekAms, true, "/repo",
		`["x.go","y.go"]`,
		`{"x.go":{"state":"ok","edits":[{"new":"k"}]},"y.go":{"state":"out_of_workspace"}}`)
	// A repair-prompt minted row is excluded, not a settled user turn.
	seedRepairMessage(t, conn, "s", "s-u0")
	insertFleetTurn(t, conn, "mine", "s", "s-u0", "accepted", weekAms, true, "", paths, okCell)
	// The same mint on a post-#433 row trips the #437 audit counter.
	seedRepairMessage(t, conn, "s", "s-u1")
	insertFleetTurn(t, conn, "mine", "s", "s-u1", "accepted", weekAms, true, "/repo", paths, okCell)
	// A foreign partition stays out.
	insertFleetTurn(t, conn, "other", "s", "t-x", "accepted", weekAms, true, "/repo", paths, okCell)

	r, err := gatherFleetHealth(t.Context(), conn, trendProject{key: "mine"}, 0)
	require.NoError(t, err)
	require.Empty(t, r.Missing)
	require.Equal(t, 5, r.SettledTurns)
	require.Equal(t, 2, r.Accepted)
	require.Equal(t, 1, r.Revised)
	require.Equal(t, 1, r.Failed)
	require.Equal(t, 1, r.Unknown)
	require.Equal(t, 1, r.PendingTurns)
	require.Equal(t, 2, r.NonPromptTurnsExcluded)
	require.Equal(t, 1, r.PostFixNonPromptTurns,
		"a post-#433 row minted on a repair boundary is the #437 violation the audit counts")
	require.Equal(t, 6, r.EditedPaths)
	// Designed abstentions leave the denominator (#466): the three
	// out_of_workspace cells report as their own share instead.
	require.Equal(t, 3, r.InScopePaths)
	require.Equal(t, 3, r.AbstainedPaths)
	require.Equal(t, 3, r.OKPaths)
	require.InDelta(t, 1.0, *r.OKShare, 0.001)
	require.Equal(t, 2, r.PostFixTurns)
	require.Equal(t, 3, r.PostFixPaths)
	require.Equal(t, 2, r.PostFixInScopePaths)
	require.Equal(t, 1, r.PostFixAbstainedPaths)
	require.Equal(t, 2, r.PostFixOKPaths)
	require.InDelta(t, 1.0, *r.PostFixOKShare, 0.001)
	require.Equal(t, "pass", r.H0)
	require.Equal(t, map[string]int{"out_of_workspace": 3}, r.Excluded)

	var output bytes.Buffer
	printFleetHealth(&output, r)
	require.Contains(t, output.String(), "post-#433: 1")
	require.Contains(t, output.String(), "Warning: #437 audit")
}

func TestGatherFleetHealthPostFixPass(t *testing.T) {
	t.Parallel()
	conn, err := db.Connect(t.Context(), t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { conn.Close() })

	okCell := `{"x.go":{"state":"ok","edits":[{"new":"kept"}]}}`
	insertFleetTurn(t, conn, "mine", "s", "t1", "accepted", weekAms, true, "/repo",
		`["x.go"]`, okCell)
	insertFleetTurn(t, conn, "mine", "s", "t2", "revised", weekAms, true, "/repo",
		`["x.go","y.go"]`,
		`{"x.go":{"state":"ok","edits":[{"new":"k"}]},"y.go":{"state":"ok","edits":[{"new":"j"}]}}`)
	insertFleetTurn(t, conn, "mine", "s", "t3", "accepted", weekAms, true, "",
		`["x.go"]`, `{"x.go":{"state":"out_of_workspace"}}`)

	r, err := gatherFleetHealth(t.Context(), conn, trendProject{key: "mine"}, 0)
	require.NoError(t, err)
	require.Equal(t, 2, r.PostFixTurns)
	require.Equal(t, 3, r.PostFixPaths)
	require.Equal(t, 3, r.PostFixOKPaths)
	require.InDelta(t, 1.0, *r.PostFixOKShare, 0.001)
	require.Equal(t, "pass", r.H0)
	// The all-time share measures in-scope paths only — the pre-fix
	// out_of_workspace cell abstains (visible in Excluded), so the
	// share no longer prices designed abstentions as failures.
	require.Equal(t, 4, r.EditedPaths)
	require.Equal(t, 3, r.InScopePaths)
	require.Equal(t, 1, r.AbstainedPaths)
	require.Equal(t, 3, r.OKPaths)
	require.InDelta(t, 1.0, *r.OKShare, 0.001)
}

// A post-fix turn whose edits are all designed abstentions yields no
// in-scope denominator — pending, not a 0% fail (#466).
func TestGatherFleetHealthAllAbstained(t *testing.T) {
	t.Parallel()
	conn, err := db.Connect(t.Context(), t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { conn.Close() })

	insertFleetTurn(t, conn, "mine", "s", "t1", "accepted", weekAms, true, "/repo",
		`["x.go","y.go"]`,
		`{"x.go":{"state":"cross_repo"},"y.go":{"state":"out_of_workspace"}}`)

	r, err := gatherFleetHealth(t.Context(), conn, trendProject{key: "mine"}, 0)
	require.NoError(t, err)
	require.Equal(t, 1, r.PostFixTurns)
	require.Equal(t, 2, r.PostFixPaths)
	require.Zero(t, r.PostFixInScopePaths)
	require.Equal(t, 2, r.PostFixAbstainedPaths)
	require.Nil(t, r.PostFixOKShare)
	require.Equal(t, "pending", r.H0)

	var output bytes.Buffer
	printFleetHealth(&output, r)
	require.Contains(t, output.String(), "all outside scope")
	require.Contains(t, output.String(), "cross_repo:1")
}

func TestGatherFleetHealthPendingEra(t *testing.T) {
	t.Parallel()
	conn, err := db.Connect(t.Context(), t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { conn.Close() })

	// Post-fix turns exist but none edited — capture cannot be read.
	insertFleetTurn(t, conn, "mine", "s", "t1", "accepted", weekAms, true, "/repo", `[]`, `{}`)

	r, err := gatherFleetHealth(t.Context(), conn, trendProject{key: "mine"}, 0)
	require.NoError(t, err)
	require.Equal(t, 1, r.PostFixTurns)
	require.Zero(t, r.PostFixPaths)
	require.Nil(t, r.PostFixOKShare)
	require.Equal(t, "pending", r.H0)
	require.Nil(t, r.OKShare, "zero edited paths is no share, not zero")

	var output bytes.Buffer
	printFleetHealth(&output, r)
	require.Contains(t, output.String(), "post-#433 turns recorded, no edited paths yet")
}

func TestGatherFleetHealthLegacy(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		alter      func(t *testing.T, conn *sql.DB)
		wantH0     string
		wantMiss   string
		wantPrint  string
		wantExcl   map[string]int
		wantEdited int
	}{
		{
			name:      "missing turn_outcomes",
			alter:     func(t *testing.T, conn *sql.DB) { trendExec(t, conn, "DROP TABLE turn_outcomes") },
			wantH0:    "pre-schema",
			wantMiss:  "turn_outcomes",
			wantPrint: "pre-schema",
		},
		{
			name: "missing hunk columns",
			alter: func(t *testing.T, conn *sql.DB) {
				trendExec(t, conn, "ALTER TABLE turn_outcomes DROP COLUMN hunk_added")
				trendExec(t, conn, "ALTER TABLE turn_outcomes DROP COLUMN hunk_after")
			},
			// A post-fix stamped row whose paths cannot read cells is
			// an H0 failure — pending is only for "cannot read yet".
			wantH0:     "fail",
			wantMiss:   "turn_outcomes.hunk_cells",
			wantPrint:  "H0: fail",
			wantExcl:   map[string]int{"pre_capture": 1},
			wantEdited: 1,
		},
		{
			name:       "missing workdir column",
			alter:      func(t *testing.T, conn *sql.DB) { trendExec(t, conn, "ALTER TABLE turn_outcomes DROP COLUMN workdir") },
			wantH0:     "pending",
			wantMiss:   "turn_outcomes.workdir",
			wantPrint:  "every row is pre-fix",
			wantEdited: 1,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			conn, err := db.Connect(t.Context(), t.TempDir())
			require.NoError(t, err)
			t.Cleanup(func() { conn.Close() })
			insertFleetTurn(t, conn, "mine", "s", "t1", "accepted", weekAms, true, "/repo",
				`["x.go"]`, `{"x.go":{"state":"ok","edits":[{"new":"k"}]}}`)
			tc.alter(t, conn)

			r, err := gatherFleetHealth(t.Context(), conn, trendProject{key: "mine"}, 0)
			require.NoError(t, err)
			require.Contains(t, r.Missing, tc.wantMiss)
			require.Equal(t, tc.wantH0, r.H0)
			require.Equal(t, tc.wantEdited, r.EditedPaths)
			if tc.wantExcl != nil {
				require.Equal(t, tc.wantExcl, r.Excluded)
			}
			var output bytes.Buffer
			printFleetHealth(&output, r)
			require.Contains(t, output.String(), tc.wantPrint)
		})
	}
}

func TestStatsFleetCommand(t *testing.T) {
	t.Setenv("CRUSH_GLOBAL_DATA", t.TempDir())
	dataDir := t.TempDir()
	conn, err := db.Connect(t.Context(), dataDir)
	require.NoError(t, err)
	t.Cleanup(func() { conn.Close() })
	projectDir := t.TempDir()
	out, err := exec.CommandContext(t.Context(), "git", "-C", projectDir, "init", "--quiet").CombinedOutput()
	require.NoError(t, err, "%s", out)
	alias := filepath.Join(projectDir, "subdir")
	require.NoError(t, os.Mkdir(alias, 0o755))
	key := cmdlog.ProjectKeyForDir(projectDir)
	insertFleetTurn(t, conn, key, "s", "t1", "accepted", weekAms, true, "/repo",
		`["x.go"]`, `{"x.go":{"state":"ok","edits":[{"new":"k"}]}}`)
	require.NoError(t, projects.Save(&projects.ProjectList{Projects: []projects.Project{
		{Path: projectDir, DataDir: dataDir},
		{Path: alias, DataDir: dataDir},
	}}))

	cmd := newStatsFleetCmd()
	cmd.SetContext(t.Context())
	cmd.SetArgs([]string{"--all", "--json"})
	var output bytes.Buffer
	cmd.SetOut(&output)
	require.NoError(t, cmd.Execute())
	var report fleetReport
	require.NoError(t, json.Unmarshal(output.Bytes(), &report))
	require.Len(t, report.Projects, 1, "aliases of one partition must not duplicate")
	r := report.Projects[0]
	require.Equal(t, projectDir, r.Project)
	require.Equal(t, 1, r.SettledTurns)
	require.Equal(t, 1, r.PostFixTurns)
	require.Equal(t, "pass", r.H0)
	require.Equal(t, fleetSummary{Projects: 1, Pass: 1}, report.Summary)

	// Text mode prints the per-project block plus the summary.
	cmd = newStatsFleetCmd()
	cmd.SetContext(t.Context())
	cmd.SetArgs([]string{"--all"})
	output.Reset()
	cmd.SetOut(&output)
	require.NoError(t, cmd.Execute())
	require.Contains(t, output.String(), "Fleet — "+projectDir)
	require.Contains(t, output.String(), "real-user settled: 1")
	require.Contains(t, output.String(), "H0: pass")
	require.Contains(t, output.String(), "Fleet summary")
	require.Contains(t, output.String(), "pass 1")
}

func TestStatsFleetCommandContinuesAfterFailure(t *testing.T) {
	t.Setenv("CRUSH_GLOBAL_DATA", t.TempDir())
	badProject, goodProject := t.TempDir(), t.TempDir()
	badData, goodData := t.TempDir(), t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(badData, "crush.db"), 0o755))
	conn, err := db.Connect(t.Context(), goodData)
	require.NoError(t, err)
	t.Cleanup(func() { conn.Close() })
	insertFleetTurn(t, conn, cmdlog.ProjectKeyForDir(goodProject), "s", "t1", "accepted",
		weekAms, true, "/repo", `["x.go"]`, `{"x.go":{"state":"ok","edits":[{"new":"k"}]}}`)
	require.NoError(t, projects.Save(&projects.ProjectList{Projects: []projects.Project{
		{Path: badProject, DataDir: badData},
		{Path: goodProject, DataDir: goodData},
	}}))

	cmd := newStatsFleetCmd()
	cmd.SetContext(t.Context())
	cmd.SetArgs([]string{"--all", "--json"})
	var output, warnings bytes.Buffer
	cmd.SetOut(&output)
	cmd.SetErr(&warnings)
	require.ErrorContains(t, cmd.Execute(), badProject)
	require.Contains(t, warnings.String(), "warn: fleet health for "+badProject)
	var report fleetReport
	require.NoError(t, json.Unmarshal(output.Bytes(), &report), "partial output must stay valid JSON")
	require.Len(t, report.Projects, 1)
	require.Equal(t, goodProject, report.Projects[0].Project)
}
