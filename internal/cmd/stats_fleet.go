package cmd

// `crush stats fleet` — the fleet capture-health report (#436,
// roadmap R0.1 / H0). Per registered project: real-user settled
// turns and their verdict mix (the audited filter — stored harness
// repair prompts never count as user turns), the share of edited
// path instances whose hunk_added cells are `ok`, and a pre/post
// split on the workdir stamp so "no post-#433 rows yet" reads
// differently from "capture is broken". H0 reads the post-#433
// share: pre-fix rows are frozen history, honest absence.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/charmbracelet/crush/internal/cmdlog"
	"github.com/charmbracelet/crush/internal/db"
	"github.com/charmbracelet/crush/internal/filepathext"
	"github.com/spf13/cobra"
)

// h0OKShare is the roadmap §4 H0 threshold: the share of real-user
// edited paths whose hunk cells are `ok` on post-#433 rows.
const h0OKShare = 0.8

// fleetHealth is one project partition's capture-health row.
// Share fields are pointers so an empty denominator marshals as
// null rather than a false zero — the trend convention.
type fleetHealth struct {
	Project        string         `json:"project"`
	DataDir        string         `json:"data_dir"`
	SinceMS        int64          `json:"since_ms"`
	SettledTurns   int            `json:"settled_turns"`
	PendingTurns   int            `json:"pending_turns"`
	Accepted       int            `json:"accepted"`
	Revised        int            `json:"revised"`
	Failed         int            `json:"failed"`
	Unknown        int            `json:"unknown"`
	RepairExcluded int            `json:"repair_turns_excluded"`
	EditedPaths    int            `json:"edited_path_instances"`
	OKPaths        int            `json:"ok_path_instances"`
	OKShare        *float64       `json:"ok_share"`
	PostFixTurns   int            `json:"post_fix_turns"`
	PostFixPaths   int            `json:"post_fix_path_instances"`
	PostFixOKPaths int            `json:"post_fix_ok_paths"`
	PostFixOKShare *float64       `json:"post_fix_ok_share"`
	Excluded       map[string]int `json:"excluded,omitempty"`
	Missing        []string       `json:"missing,omitempty"`
	// H0 is pass | fail | pending | pre-schema. Pending covers
	// "no post-#433 rows" and "post-#433 turns but no edits yet" —
	// the states the H0 gate must not misread as failure.
	H0 string `json:"h0"`
}

// fleetSummary counts projects per H0 status. Failed counts
// partitions whose DB could not be opened or read — reported as
// warnings, but the fleet total should not silently absorb them.
type fleetSummary struct {
	Projects  int `json:"projects"`
	Pass      int `json:"pass"`
	Fail      int `json:"fail"`
	Pending   int `json:"pending"`
	PreSchema int `json:"pre_schema"`
	Failed    int `json:"failed_targets,omitempty"`
}

type fleetReport struct {
	GeneratedAt string        `json:"generated_at"`
	SinceMS     int64         `json:"since_ms"`
	Projects    []fleetHealth `json:"projects"`
	Summary     fleetSummary  `json:"summary"`
}

func init() {
	statsCmd.AddCommand(newStatsFleetCmd())
}

func newStatsFleetCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "fleet",
		Args:  cobra.NoArgs,
		Short: "Fleet capture-health report (H0)",
		Long: "Report per project: real-user settled turns and verdict mix " +
			"(harness repair prompts excluded), the ok share of edited path " +
			"hunk cells, and the post-#433 split the H0 gate reads.",
		RunE:         runStatsFleet,
		SilenceUsage: true,
	}
	cmd.Flags().String("project", "", "Limit to one registered project (path or basename)")
	cmd.Flags().Bool("all", false, "Report on all registered projects")
	cmd.Flags().String("since", "all", "Lower bound: 7d, 4w, 720h, YYYY-MM-DD, or all")
	cmd.Flags().Bool("json", false, "Print the fleet report as JSON")
	return cmd
}

func runStatsFleet(cmd *cobra.Command, _ []string) error {
	sinceArg, _ := cmd.Flags().GetString("since")
	since, err := parseTrendSince(sinceArg)
	if err != nil {
		return err
	}
	targets, err := resolveTrendProjects(cmd)
	if err != nil {
		return err
	}
	reports := make([]fleetHealth, 0, len(targets))
	var failures []error
	type partition struct{ dataDir, key string }
	seen := map[partition]bool{}
	for _, tp := range targets {
		id := partition{filepathext.Canonical(tp.dataDir), tp.key}
		if seen[id] {
			continue
		}
		seen[id] = true
		conn, err := db.ConnectReadOnly(cmd.Context(), filepath.Join(tp.dataDir, "crush.db"))
		var report fleetHealth
		if err == nil {
			report, err = gatherFleetHealth(cmd.Context(), conn, tp, since)
			conn.Close()
		}
		if err != nil {
			failure := fmt.Errorf("fleet health for %s: %w", tp.path, err)
			fmt.Fprintf(cmd.ErrOrStderr(), "warn: %v\n", failure)
			failures = append(failures, failure)
			continue
		}
		reports = append(reports, report)
	}
	jsonOutput, _ := cmd.Flags().GetBool("json")
	if jsonOutput {
		out := fleetReport{
			GeneratedAt: time.Now().UTC().Format(time.RFC3339),
			SinceMS:     since,
			Projects:    reports,
			Summary:     summarizeFleet(reports, len(failures)),
		}
		enc := json.NewEncoder(cmd.OutOrStdout())
		enc.SetIndent("", "  ")
		if err := enc.Encode(out); err != nil {
			return err
		}
	} else {
		for _, report := range reports {
			fmt.Fprintf(cmd.OutOrStdout(), "Fleet — %s\n", report.Project)
			fmt.Fprintf(cmd.OutOrStdout(), "  data directory: %s\n", report.DataDir)
			printFleetHealth(cmd.OutOrStdout(), report)
			fmt.Fprintln(cmd.OutOrStdout())
		}
		printFleetSummary(cmd.OutOrStdout(), summarizeFleet(reports, len(failures)))
	}
	return errors.Join(failures...)
}

// gatherFleetHealth reads one partition's turn rows once: verdict
// mix over real-user settled rows, ok share over real-user edited
// paths, and the workdir-stamp era split. The real-user filter is
// the audited one — a stored repair prompt never mints a user turn.
func gatherFleetHealth(ctx context.Context, conn *sql.DB, tp trendProject, sinceMS int64) (fleetHealth, error) {
	r := fleetHealth{Project: tp.path, DataDir: tp.dataDir, SinceMS: sinceMS}
	schema := probeTrendSchema(ctx, conn)
	if !schema.turns {
		r.Missing = []string{"turn_outcomes"}
		r.H0 = "pre-schema"
		return r, nil
	}
	workdirCol := hasColumn(ctx, conn, "turn_outcomes", "workdir")
	if !schema.hunkCells {
		r.Missing = append(r.Missing, "turn_outcomes.hunk_cells")
	}
	if !workdirCol {
		r.Missing = append(r.Missing, "turn_outcomes.workdir")
	}
	hunkSel := "t.hunk_added"
	if !schema.hunkCells {
		hunkSel = "'{}'"
	}
	workdirSel := "t.workdir"
	if !workdirCol {
		workdirSel = "''"
	}
	rows, err := conn.QueryContext(ctx, `SELECT COALESCE(t.verdict, ''), t.settled_at,
		COALESCE(t.edited_paths, '[]'), COALESCE(`+hunkSel+`, '{}'),
		COALESCE(`+workdirSel+`, ''), m.parts
		FROM turn_outcomes t
		LEFT JOIN messages m ON m.id = t.turn_id AND m.session_id = t.session_id
		WHERE t.project_key = ? AND t.created_at >= ?`, tp.key, sinceMS)
	if err != nil {
		return r, err
	}
	defer rows.Close()
	for rows.Next() {
		var verdict, pathsJSON, addedJSON, workdir string
		var settled sql.NullInt64
		var parts sql.NullString
		if err := rows.Scan(&verdict, &settled, &pathsJSON, &addedJSON, &workdir, &parts); err != nil {
			return r, err
		}
		if parts.Valid && trendNonUserTurn(parts.String) {
			r.RepairExcluded++
			continue
		}
		postFix := workdir != ""
		if postFix {
			r.PostFixTurns++
		}
		if settled.Valid {
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
		} else {
			r.PendingTurns++
		}
		var paths []string
		if err := json.Unmarshal([]byte(pathsJSON), &paths); err != nil {
			return r, fmt.Errorf("decode edited paths: %w", err)
		}
		if len(paths) == 0 {
			continue
		}
		var added map[string]trendAddedCell
		if addedJSON != "" {
			if err := json.Unmarshal([]byte(addedJSON), &added); err != nil {
				return r, fmt.Errorf("decode captured edits: %w", err)
			}
		}
		for _, path := range paths {
			r.EditedPaths++
			if postFix {
				r.PostFixPaths++
			}
			cell, exists := added[path]
			reason := ""
			switch {
			case !schema.hunkCells:
				reason = "pre_capture"
			case !exists:
				reason = "no_added_cell"
			case cell.State == "":
				reason = "unknown_state"
			case cell.State != "ok":
				reason = cell.State
			case !slices.ContainsFunc(cell.Edits, func(e trendHunkEdit) bool { return e.Old != "" || e.New != "" }):
				reason = "no_content"
			}
			if reason != "" {
				if r.Excluded == nil {
					r.Excluded = map[string]int{}
				}
				r.Excluded[reason]++
				continue
			}
			r.OKPaths++
			if postFix {
				r.PostFixOKPaths++
			}
		}
	}
	if err := rows.Err(); err != nil {
		return r, err
	}
	if r.EditedPaths > 0 {
		share := float64(r.OKPaths) / float64(r.EditedPaths)
		r.OKShare = &share
	}
	if r.PostFixPaths > 0 {
		share := float64(r.PostFixOKPaths) / float64(r.PostFixPaths)
		r.PostFixOKShare = &share
	}
	switch {
	case r.PostFixPaths == 0:
		r.H0 = "pending"
	case *r.PostFixOKShare >= h0OKShare:
		r.H0 = "pass"
	default:
		r.H0 = "fail"
	}
	return r, nil
}

func summarizeFleet(reports []fleetHealth, failed int) fleetSummary {
	s := fleetSummary{Projects: len(reports), Failed: failed}
	for _, r := range reports {
		switch r.H0 {
		case "pass":
			s.Pass++
		case "fail":
			s.Fail++
		case "pre-schema":
			s.PreSchema++
		default:
			s.Pending++
		}
	}
	return s
}

func printFleetHealth(w io.Writer, r fleetHealth) {
	if slices.Contains(r.Missing, "turn_outcomes") {
		fmt.Fprintln(w, "  fleet health unavailable: missing turn_outcomes")
		fmt.Fprintln(w, "  H0: pre-schema — this database predates turn outcomes")
		return
	}
	if len(r.Missing) > 0 {
		fmt.Fprintf(w, "  missing: %s\n", strings.Join(r.Missing, ", "))
	}
	fmt.Fprintf(w, "  real-user settled: %d   verdict mix: A %d · R %d · F %d · U %d\n",
		r.SettledTurns, r.Accepted, r.Revised, r.Failed, r.Unknown)
	fmt.Fprintf(w, "  pending real-user turns: %d   repair-prompt rows excluded: %d\n",
		r.PendingTurns, r.RepairExcluded)
	if r.OKShare == nil {
		fmt.Fprintf(w, "  edited paths: %d instances — no edited paths recorded\n", r.EditedPaths)
	} else {
		fmt.Fprintf(w, "  edited paths: %d instances, ok %d (%.1f%%)\n",
			r.EditedPaths, r.OKPaths, *r.OKShare*100)
	}
	switch {
	case r.PostFixTurns == 0 && slices.Contains(r.Missing, "turn_outcomes.workdir"):
		fmt.Fprintln(w, "  post-#433: schema predates the workdir stamp — every row is pre-fix")
	case r.PostFixTurns == 0:
		fmt.Fprintln(w, "  post-#433: no rows yet")
	default:
		share := "—"
		if r.PostFixOKShare != nil {
			share = fmt.Sprintf("%.1f%%", *r.PostFixOKShare*100)
		}
		fmt.Fprintf(w, "  post-#433: %d turns · %d edited paths · ok %d (%s)\n",
			r.PostFixTurns, r.PostFixPaths, r.PostFixOKPaths, share)
	}
	printStatExclusions(w, "non-ok reasons", r.Excluded)
	switch r.H0 {
	case "pass":
		fmt.Fprintf(w, "  H0: pass — ok share %.1f%% ≥ %.0f%% on post-#433 rows\n",
			*r.PostFixOKShare*100, h0OKShare*100)
	case "fail":
		fmt.Fprintf(w, "  H0: fail — ok share %.1f%% < %.0f%% on post-#433 rows\n",
			*r.PostFixOKShare*100, h0OKShare*100)
	default:
		switch {
		case r.PostFixTurns == 0:
			fmt.Fprintln(w, "  H0: pending — needs post-#433 rows with edits")
		case r.PostFixPaths == 0:
			fmt.Fprintln(w, "  H0: pending — post-#433 turns recorded, no edited paths yet")
		}
	}
}

func printFleetSummary(w io.Writer, s fleetSummary) {
	fmt.Fprintf(w, "Fleet summary — H0 ≥ %.0f%% ok share on post-#433 rows:\n", h0OKShare*100)
	fmt.Fprintf(w, "  %d projects: pass %d · fail %d · pending %d · pre-schema %d",
		s.Projects, s.Pass, s.Fail, s.Pending, s.PreSchema)
	if s.Failed > 0 {
		fmt.Fprintf(w, " · errors %d", s.Failed)
	}
	fmt.Fprintln(w)
}
