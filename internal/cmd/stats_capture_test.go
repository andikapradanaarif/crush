package cmd

import (
	"bytes"
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

func TestGatherCaptureHealthAllVerdictsAndPending(t *testing.T) {
	t.Parallel()
	conn, err := db.Connect(t.Context(), t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { conn.Close() })

	outside := `{"x.go":{"state":"out_of_workspace"}}`
	content := `{"x.go":{"state":"ok","edits":[{"new":"kept"}],"spans":[{"start":0,"end":1}]}}`
	after := `{"x.go":{"state":"ok","windows":[{"start":0,"text":"kept"}]}}`
	paths := `["x.go"]`
	insertTurnOutcome(t, conn, "mine", "s", "failed", "failed", weekAms, true, paths, outside, outside)
	insertTurnOutcome(t, conn, "mine", "s", "unknown", "unknown", weekAms, false, paths, outside, `{}`)
	insertTurnOutcome(t, conn, "mine", "s", "pending", "", weekAms, false, paths, content, `{}`)
	// Usable capture is visible even if its acceptance verdict is failed.
	insertTurnOutcome(t, conn, "mine", "s", "comparable", "failed", weekAms, true, paths, content, after)
	insertTurnOutcome(t, conn, "mine", "s", "deletion", "", weekAms, false, paths,
		`{"x.go":{"state":"ok","edits":[{"old":"removed"}],"spans":[{"start":1,"end":1}]}}`, `{}`)
	insertTurnOutcome(t, conn, "mine", "s", "unpositioned", "", weekAms, false, paths,
		`{"x.go":{"state":"ok","edits":[{"new":"kept"}]}}`, `{}`)
	insertTurnOutcome(t, conn, "mine", "s", "sensitive", "accepted", weekAms, true,
		`[".env"]`, `{".env":{"state":"sensitive"}}`, `{".env":{"state":"sensitive"}}`)
	// Read-only turns, older turns, and foreign partitions stay out.
	insertTurnOutcome(t, conn, "mine", "s", "read", "accepted", weekAms, true, `null`, `{}`, `{}`)
	insertTurnOutcome(t, conn, "mine", "s", "old", "accepted", weekAms-1, true, paths, outside, outside)
	insertTurnOutcome(t, conn, "other", "s", "foreign", "accepted", weekAms, true, paths, outside, outside)

	r, err := gatherCaptureHealth(t.Context(), conn, trendProject{path: "/repo", dataDir: "/repo/.crush", key: "mine"}, weekAms)
	require.NoError(t, err)
	require.Empty(t, r.Missing)
	require.Equal(t, 7, r.EditedTurns)
	require.Equal(t, 4, r.CapturedTurns)
	require.Equal(t, 3, r.PositionedTurns)
	require.Equal(t, 4, r.PendingTurns)
	require.Equal(t, 3, r.SettledTurns)
	require.Equal(t, 1, r.ComparableTurns)
	// These count repeated x.go instances across distinct turns.
	require.Equal(t, 7, r.EditedPathInstances)
	require.Equal(t, 4, r.CapturedPathInstances)
	require.Equal(t, 3, r.PositionedPathInstances)
	require.Equal(t, map[string]int{"out_of_workspace": 2, "sensitive": 1}, r.Excluded)
	require.Equal(t, map[string]int{"added_out_of_workspace": 1, "sensitive": 1}, r.PairExcluded)
	var pending int
	require.NoError(t, conn.QueryRowContext(t.Context(),
		`SELECT COUNT(*) FROM turn_outcomes WHERE settled_at IS NULL`).Scan(&pending))
	require.Equal(t, 4, pending, "reporting must not settle pending turns")
}

func TestGatherCaptureHealthLegacy(t *testing.T) {
	t.Parallel()
	for _, noTurns := range []bool{false, true} {
		t.Run(map[bool]string{false: "missing hunk columns", true: "missing turn table"}[noTurns], func(t *testing.T) {
			t.Parallel()
			conn, err := db.Connect(t.Context(), t.TempDir())
			require.NoError(t, err)
			t.Cleanup(func() { conn.Close() })
			insertTurnOutcome(t, conn, "mine", "s", "old", "accepted", weekAms, true, `["x.go"]`, `{}`, `{}`)
			if noTurns {
				trendExec(t, conn, "DROP TABLE turn_outcomes")
			} else {
				trendExec(t, conn, "ALTER TABLE turn_outcomes DROP COLUMN hunk_added")
				trendExec(t, conn, "ALTER TABLE turn_outcomes DROP COLUMN hunk_after")
			}
			r, err := gatherCaptureHealth(t.Context(), conn, trendProject{key: "mine"}, 0)
			require.NoError(t, err)
			require.NotEmpty(t, r.Missing)
			require.Zero(t, r.CapturedTurns)
			if !noTurns {
				require.Equal(t, 1, r.EditedTurns)
				require.Equal(t, map[string]int{"pre_capture": 1}, r.Excluded)
			}
			var output bytes.Buffer
			printCaptureHealth(&output, r)
			require.Contains(t, output.String(), "capture health unavailable")
			require.NotContains(t, output.String(), "0/0 edited turns")
			if !noTurns {
				require.Contains(t, output.String(), "recorded edit turns: 1 (0 pending, 1 settled); path instances: 1")
				require.Contains(t, output.String(), "pre_capture:1")
			} else {
				require.NotContains(t, output.String(), "recorded edit turns:")
			}
		})
	}
}

func TestPrintCaptureHealth(t *testing.T) {
	t.Parallel()
	var output bytes.Buffer
	printCaptureHealth(&output, captureHealth{
		Project: "/repo", EditedTurns: 2, SettledTurns: 1, PendingTurns: 1,
		Excluded: map[string]int{"out_of_workspace": 2},
	})
	require.Contains(t, output.String(), "0/2 edited turns have content")
	require.Contains(t, output.String(), "1 pending, 1 settled")
	require.Contains(t, output.String(), "out_of_workspace:2")
	require.Contains(t, output.String(), "Warning: no edit content was captured")
	require.Contains(t, output.String(), "Selected project directory: /repo")
	require.Contains(t, output.String(), "checkout being edited")
	output.Reset()
	// Cross-repo abstentions point at the other repo's partition, not a
	// relaunch hint (#466).
	printCaptureHealth(&output, captureHealth{
		EditedTurns: 1, Excluded: map[string]int{"cross_repo": 1},
	})
	require.Contains(t, output.String(), "other repositories")
	require.Contains(t, output.String(), "partitions")
	require.NotContains(t, output.String(), "checkout being edited")
	output.Reset()
	printCaptureHealth(&output, captureHealth{EditedTurns: 1, CapturedTurns: 1, PendingTurns: 1})
	require.Contains(t, output.String(), "captured edits have no positions")
	output.Reset()
	printCaptureHealth(&output, captureHealth{})
	require.NotContains(t, output.String(), "Warning:")
}

func TestStatsCaptureCommand(t *testing.T) {
	// Project selection avoids changing cwd or evaluating user config.
	global := t.TempDir()
	t.Setenv("CRUSH_GLOBAL_DATA", global)
	dataDir := t.TempDir()
	conn, err := db.Connect(t.Context(), dataDir)
	require.NoError(t, err)
	t.Cleanup(func() { conn.Close() })
	projectDir := t.TempDir()
	outputGit, err := exec.CommandContext(t.Context(), "git", "-C", projectDir, "init", "--quiet").CombinedOutput()
	require.NoError(t, err, "%s", outputGit)
	alias := filepath.Join(projectDir, "subdir")
	require.NoError(t, os.Mkdir(alias, 0o755))
	key := cmdlog.ProjectKeyForDir(projectDir)
	insertTurnOutcome(t, conn, key, "s", "pending", "", weekAms, false,
		`["../sibling/x.go"]`, `{"../sibling/x.go":{"state":"out_of_workspace"}}`, `{}`)
	require.NoError(t, projects.Save(&projects.ProjectList{Projects: []projects.Project{
		{Path: projectDir, DataDir: dataDir},
		{Path: alias, DataDir: dataDir},
	}}))

	cmd := newStatsCaptureCmd()
	cmd.SetContext(t.Context())
	cmd.SetArgs([]string{"--project", projectDir, "--since", "all", "--json"})
	var output bytes.Buffer
	cmd.SetOut(&output)
	require.NoError(t, cmd.Execute())
	var reports []captureHealth
	require.NoError(t, json.Unmarshal(output.Bytes(), &reports))
	require.Len(t, reports, 1)
	require.Equal(t, projectDir, reports[0].Project)
	require.Equal(t, dataDir, reports[0].DataDir)
	require.Equal(t, 1, reports[0].EditedTurns)
	require.Equal(t, 1, reports[0].PendingTurns)
	require.Contains(t, output.String(), `"edited_path_instances": 1`)
	require.Equal(t, map[string]int{"out_of_workspace": 1}, reports[0].Excluded)
	_, err = os.Stat(filepath.Join(dataDir, trendLedgerName))
	require.True(t, os.IsNotExist(err), "capture diagnostics must not append a trend ledger")

	cmd = newStatsCaptureCmd()
	cmd.SetContext(t.Context())
	cmd.SetArgs([]string{"--project", projectDir, "--since", "all"})
	output.Reset()
	cmd.SetOut(&output)
	require.NoError(t, cmd.Execute())
	require.Contains(t, output.String(), "Warning: no edit content was captured")

	cmd = newStatsCaptureCmd()
	cmd.SetContext(t.Context())
	cmd.SetArgs([]string{"--all", "--since", "all", "--json"})
	output.Reset()
	cmd.SetOut(&output)
	require.NoError(t, cmd.Execute())
	require.NoError(t, json.Unmarshal(output.Bytes(), &reports))
	require.Len(t, reports, 1, "aliases of the same database partition must not duplicate counts")
}

func TestGatherCaptureHealthCorruptRecordTime(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, paths, added, wantError string
	}{
		{"paths", `{`, `{}`, "decode edited paths"},
		{"edits", `["x.go"]`, `{`, "decode captured edits"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			conn, err := db.Connect(t.Context(), t.TempDir())
			require.NoError(t, err)
			t.Cleanup(func() { conn.Close() })
			insertTurnOutcome(t, conn, "mine", "s", "corrupt", "", weekAms, false, tc.paths, tc.added, `{}`)
			_, err = gatherCaptureHealth(t.Context(), conn, trendProject{key: "mine"}, 0)
			require.ErrorContains(t, err, tc.wantError)
		})
	}
}

func TestStatsCaptureCommandContinuesAfterFailure(t *testing.T) {
	for _, openFailure := range []bool{true, false} {
		t.Run(map[bool]string{true: "database open failure", false: "corrupt record"}[openFailure], func(t *testing.T) {
			t.Setenv("CRUSH_GLOBAL_DATA", t.TempDir())
			badProject, goodProject := t.TempDir(), t.TempDir()
			badData, goodData := t.TempDir(), t.TempDir()
			if openFailure {
				// A directory exists for selection but cannot open as SQLite.
				require.NoError(t, os.Mkdir(filepath.Join(badData, "crush.db"), 0o755))
			} else {
				conn, err := db.Connect(t.Context(), badData)
				require.NoError(t, err)
				t.Cleanup(func() { conn.Close() })
				insertTurnOutcome(t, conn, cmdlog.ProjectKeyForDir(badProject), "s", "bad", "", weekAms, false, `{`, `{}`, `{}`)
			}
			conn, err := db.Connect(t.Context(), goodData)
			require.NoError(t, err)
			t.Cleanup(func() { conn.Close() })
			insertTurnOutcome(t, conn, cmdlog.ProjectKeyForDir(goodProject), "s", "good", "", weekAms, false,
				`["x.go"]`, `{"x.go":{"state":"ok","edits":[{"new":"kept"}],"spans":[{"start":0,"end":1}]}}`, `{}`)
			require.NoError(t, projects.Save(&projects.ProjectList{Projects: []projects.Project{
				{Path: badProject, DataDir: badData},
				{Path: goodProject, DataDir: goodData},
			}}))
			cmd := newStatsCaptureCmd()
			cmd.SetContext(t.Context())
			cmd.SetArgs([]string{"--all", "--since", "all", "--json"})
			var output, warnings bytes.Buffer
			cmd.SetOut(&output)
			cmd.SetErr(&warnings)
			require.ErrorContains(t, cmd.Execute(), badProject)
			require.Contains(t, warnings.String(), "warn: capture health for "+badProject)
			var reports []captureHealth
			require.NoError(t, json.Unmarshal(output.Bytes(), &reports), "partial output must remain valid JSON")
			require.Len(t, reports, 1)
			require.Equal(t, goodProject, reports[0].Project)
			require.Equal(t, 1, reports[0].CapturedTurns)
		})
	}
}
