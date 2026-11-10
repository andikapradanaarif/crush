package cmd

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

	"github.com/charmbracelet/crush/internal/db"
	"github.com/charmbracelet/crush/internal/filepathext"
	"github.com/spf13/cobra"
)

// captureHealth counts edit evidence independently of acceptance labels.
// Pending turns expose record-time capture before settlement, while
// comparable turns require the existing settled-pair checks.
type captureHealth struct {
	Project                 string         `json:"project"`
	DataDir                 string         `json:"data_dir"`
	SinceMS                 int64          `json:"since_ms"`
	EditedTurns             int            `json:"edited_turns"`
	CapturedTurns           int            `json:"captured_turns"`
	PositionedTurns         int            `json:"positioned_turns"`
	PendingTurns            int            `json:"pending_turns"`
	SettledTurns            int            `json:"settled_turns"`
	ComparableTurns         int            `json:"comparable_turns"`
	EditedPathInstances     int            `json:"edited_path_instances"`
	CapturedPathInstances   int            `json:"captured_path_instances"`
	PositionedPathInstances int            `json:"positioned_path_instances"`
	Excluded                map[string]int `json:"capture_excluded,omitempty"`
	PairExcluded            map[string]int `json:"pair_excluded,omitempty"`
	Missing                 []string       `json:"missing,omitempty"`
}

func init() {
	statsCmd.AddCommand(newStatsCaptureCmd())
}

func newStatsCaptureCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "capture",
		Args:  cobra.NoArgs,
		Short: "Check edit capture health",
		Long: "Check recorded edit content and positions across all turn verdicts, " +
			"including pending turns. Reads project databases without migrating or settling them.",
		RunE:         runStatsCapture,
		SilenceUsage: true,
	}
	cmd.Flags().String("project", "", "Limit to one registered project (path or basename)")
	cmd.Flags().Bool("all", false, "Report on all registered projects")
	cmd.Flags().String("since", "7d", "Lower bound: 7d, 4w, 720h, YYYY-MM-DD, or all")
	cmd.Flags().Bool("json", false, "Print capture health as JSON")
	return cmd
}

func runStatsCapture(cmd *cobra.Command, _ []string) error {
	sinceArg, _ := cmd.Flags().GetString("since")
	since, err := parseTrendSince(sinceArg)
	if err != nil {
		return err
	}
	targets, err := resolveTrendProjects(cmd)
	if err != nil {
		return err
	}
	reports := make([]captureHealth, 0, len(targets))
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
		var report captureHealth
		if err == nil {
			report, err = gatherCaptureHealth(cmd.Context(), conn, tp, since)
			conn.Close()
		}
		if err != nil {
			failure := fmt.Errorf("capture health for %s: %w", tp.path, err)
			fmt.Fprintf(cmd.ErrOrStderr(), "warn: %v\n", failure)
			failures = append(failures, failure)
			continue
		}
		reports = append(reports, report)
	}
	jsonOutput, _ := cmd.Flags().GetBool("json")
	if jsonOutput {
		enc := json.NewEncoder(cmd.OutOrStdout())
		enc.SetIndent("", "  ")
		if err := enc.Encode(reports); err != nil {
			return err
		}
	} else {
		for _, report := range reports {
			fmt.Fprintf(cmd.OutOrStdout(), "Capture — %s\n", report.Project)
			fmt.Fprintf(cmd.OutOrStdout(), "  data directory: %s\n", report.DataDir)
			printCaptureHealth(cmd.OutOrStdout(), report)
			fmt.Fprintln(cmd.OutOrStdout())
		}
	}
	return errors.Join(failures...)
}

func gatherCaptureHealth(ctx context.Context, conn *sql.DB, tp trendProject, sinceMS int64) (captureHealth, error) {
	report := captureHealth{Project: tp.path, DataDir: tp.dataDir, SinceMS: sinceMS}
	schema := probeTrendSchema(ctx, conn)
	if !schema.turns {
		report.Missing = []string{"turn_outcomes"}
		return report, nil
	}
	hunkCols := "hunk_added, hunk_after"
	if !schema.hunkCells {
		report.Missing = []string{"turn_outcomes.hunk_cells"}
		hunkCols = "'{}', '{}'"
	}
	rows, err := conn.QueryContext(ctx, `SELECT edited_paths, settled_at, `+hunkCols+`
		FROM turn_outcomes WHERE project_key = ? AND created_at >= ?`, tp.key, sinceMS)
	if err != nil {
		return report, err
	}
	defer rows.Close()
	for rows.Next() {
		var pathsJSON, addedJSON, afterJSON string
		var settled sql.NullInt64
		if err := rows.Scan(&pathsJSON, &settled, &addedJSON, &afterJSON); err != nil {
			return report, err
		}
		var paths []string
		// Invalid record-time evidence is a diagnostic failure, not zero
		// capture. Settled pairs retain trend's missing-evidence rules.
		if err := json.Unmarshal([]byte(pathsJSON), &paths); err != nil {
			return report, fmt.Errorf("decode edited paths: %w", err)
		}
		if len(paths) == 0 {
			continue
		}
		var added map[string]trendAddedCell
		if addedJSON != "" {
			if err := json.Unmarshal([]byte(addedJSON), &added); err != nil {
				return report, fmt.Errorf("decode captured edits: %w", err)
			}
		}
		report.EditedTurns++
		report.EditedPathInstances += len(paths)
		captured, positioned := false, false
		for _, path := range paths {
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
				if report.Excluded == nil {
					report.Excluded = map[string]int{}
				}
				report.Excluded[reason]++
				continue
			}
			captured = true
			report.CapturedPathInstances++
			// A zero-length span locates a pure deletion's boundary.
			if slices.ContainsFunc(cell.Spans, func(s trendHunkSpan) bool { return s.Start >= 0 && s.End >= s.Start }) {
				positioned = true
				report.PositionedPathInstances++
			}
		}
		if captured {
			report.CapturedTurns++
		}
		if positioned {
			report.PositionedTurns++
		}
		if !settled.Valid {
			report.PendingTurns++
			continue
		}
		report.SettledTurns++
		pair := &trendRow{}
		if trendEditCost(pair, pathsJSON, addedJSON, afterJSON) {
			report.ComparableTurns++
		}
		for reason, n := range pair.EditCostExcluded {
			if report.PairExcluded == nil {
				report.PairExcluded = map[string]int{}
			}
			report.PairExcluded[reason] += n
		}
	}
	return report, rows.Err()
}

func printCaptureHealth(w io.Writer, r captureHealth) {
	if len(r.Missing) > 0 {
		fmt.Fprintf(w, "  capture health unavailable: missing %s\n", strings.Join(r.Missing, ", "))
		if !slices.Contains(r.Missing, "turn_outcomes") {
			fmt.Fprintf(w, "  recorded edit turns: %d (%d pending, %d settled); path instances: %d\n",
				r.EditedTurns, r.PendingTurns, r.SettledTurns, r.EditedPathInstances)
			printStatExclusions(w, "record-time exclusions", r.Excluded)
		}
		return
	}
	fmt.Fprintf(w, "  capture: %d/%d edited turns have content; %d have positions (%d pending, %d settled)\n",
		r.CapturedTurns, r.EditedTurns, r.PositionedTurns, r.PendingTurns, r.SettledTurns)
	fmt.Fprintf(w, "  comparable settled turns: %d/%d (all verdicts)\n", r.ComparableTurns, r.SettledTurns)
	if r.EditedTurns == 0 {
		fmt.Fprintln(w, "  No recorded edit turns in this window. Check again after continuing the session or starting a later session.")
	}
	printStatExclusions(w, "record-time exclusions", r.Excluded)
	printStatExclusions(w, "settled-pair exclusions", r.PairExcluded)
	if r.EditedTurns > 0 && r.CapturedTurns == 0 {
		fmt.Fprintln(w, "  Warning: no edit content was captured in this window.")
		if r.Excluded["out_of_workspace"] > 0 {
			fmt.Fprintf(w, "  Selected project directory: %s\n", r.Project)
			fmt.Fprintln(w, "  Start a new Crush session in the checkout being edited, with that checkout's own .crush directory.")
		}
		if r.Excluded["cross_repo"] > 0 {
			fmt.Fprintf(w, "  %d edits belong to other repositories — their evidence lives in those repos' partitions, not this project's.\n",
				r.Excluded["cross_repo"])
		}
	} else if r.CapturedTurns > 0 && r.PositionedTurns == 0 {
		fmt.Fprintln(w, "  Warning: captured edits have no positions; settled regions cannot be compared yet.")
	}
}

func printStatExclusions(w io.Writer, label string, excluded map[string]int) {
	if len(excluded) == 0 {
		return
	}
	reasons := make([]string, 0, len(excluded))
	for reason := range excluded {
		reasons = append(reasons, reason)
	}
	slices.Sort(reasons)
	parts := make([]string, 0, len(reasons))
	for _, reason := range reasons {
		parts = append(parts, fmt.Sprintf("%s:%d", reason, excluded[reason]))
	}
	fmt.Fprintf(w, "  %s: %s\n", label, strings.Join(parts, "  "))
}
