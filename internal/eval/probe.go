package eval

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/charmbracelet/crush/internal/agent/tools"
	"github.com/charmbracelet/crush/internal/db"
)

// Probes are offline mechanism checks over preserved session DBs —
// the tier between "read the code" and "run the experiment" (issue
// #109). Each probe consumes the analyzer's labeled call sequence
// plus a light per-call payload pass, so ordering, turn segmentation,
// path normalization, and placeholder labeling stay single-sourced in
// AnalyzeSessionDB.
//
// The read-classification probes instrument the two confirmed failure
// mechanisms: the #98 post-edit region's served class (views a region
// injection could have satisfied) and the collapse signature
// (turn-start re-reads of previously edited files).

// ProbeReport is one probe's tabular output over one session DB.
type ProbeReport struct {
	Name    string     `json:"name"`
	Session string     `json:"session"`
	Columns []string   `json:"columns"`
	Rows    [][]string `json:"rows"`
	// Counts carries the probe's summary tallies.
	Counts map[string]int `json:"counts"`
	// Notes are caveat lines printed under the table.
	Notes []string `json:"notes,omitempty"`
}

// String renders the report as an aligned table plus counts and notes.
func (r *ProbeReport) String() string {
	var b bytes.Buffer
	fmt.Fprintf(&b, "== %s (session %s)\n", r.Name, r.Session)
	w := tabwriter.NewWriter(&b, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, strings.Join(r.Columns, "\t"))
	for _, row := range r.Rows {
		fmt.Fprintln(w, strings.Join(row, "\t"))
	}
	w.Flush()
	for _, k := range sortedKeys(r.Counts) {
		fmt.Fprintf(&b, "%s=%d  ", k, r.Counts[k])
	}
	if len(r.Counts) > 0 {
		b.WriteByte('\n')
	}
	for _, n := range r.Notes {
		fmt.Fprintf(&b, "note: %s\n", n)
	}
	return b.String()
}

// ProbeNames lists the registered probe names.
func ProbeNames() []string {
	names := make([]string, 0, len(probes))
	for n := range probes {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// RunProbe runs a named probe over a session DB. The opts are the
// analyzer's — SessionID picks the session (default latest parent),
// Workdir anchors relative call paths, Turns supply authoritative
// process-turn boundaries, GOOS normalizes foreign-OS path spellings.
func RunProbe(ctx context.Context, dbPath, name string, opts AnalyzeOptions) (*ProbeReport, error) {
	p, ok := probes[name]
	if !ok {
		return nil, fmt.Errorf("unknown probe %q (known: %s)", name, strings.Join(ProbeNames(), ", "))
	}
	cm, err := AnalyzeSessionDB(ctx, dbPath, opts)
	if err != nil {
		return nil, err
	}
	payloads, err := loadCallPayloads(ctx, dbPath, cm.SessionID)
	if err != nil {
		return nil, err
	}
	rep := p(cm, payloads)
	rep.Session = cm.SessionID
	return rep, nil
}

// probes is the registry of named probes. Probes are additive —
// register new mechanism questions here rather than widening
// CallMetrics for one-off checks.
var probes = map[string]func(*CallMetrics, map[string]callPayload) *ProbeReport{
	"post-edit-window":    probePostEditWindow,
	"view-edit-same-file": probeViewEditSameFile,
	"turn-start-reread":   probeTurnStartReread,
}

// callPayload is the per-call raw material the labeled CallRecord
// doesn't carry: the call's input JSON and its result's metadata JSON
// (edit results persist old_content/new_content there — the probe's
// edit-span source).
type callPayload struct {
	Input     string
	Meta      string
	IsError   bool
	HasResult bool
}

// loadCallPayloads scans the session's parts a second time for inputs
// and result metadata, keyed by tool-call ID. Ordering and labels come
// from AnalyzeSessionDB — this pass is a dumb id→payload map and
// deliberately re-derives nothing about the sequence.
func loadCallPayloads(ctx context.Context, dbPath, sessionID string) (map[string]callPayload, error) {
	conn, err := db.ConnectReadOnly(ctx, dbPath)
	if err != nil {
		return nil, fmt.Errorf("open session db: %w", err)
	}
	defer conn.Close()

	rows, err := conn.QueryContext(ctx,
		`SELECT parts FROM messages WHERE session_id = ? ORDER BY created_at, rowid`, sessionID)
	if err != nil {
		return nil, fmt.Errorf("list messages: %w", err)
	}
	defer rows.Close()

	out := map[string]callPayload{}
	for rows.Next() {
		var partsJS string
		if err := rows.Scan(&partsJS); err != nil {
			return nil, fmt.Errorf("scan message: %w", err)
		}
		parts, err := decodeParts(partsJS)
		if err != nil {
			return nil, fmt.Errorf("decode parts: %w", err)
		}
		for _, p := range parts {
			switch p.Type {
			case "tool_call":
				var tc rawToolCall
				if json.Unmarshal(p.Data, &tc) != nil {
					continue
				}
				pl := out[tc.ID]
				pl.Input = tc.Input
				out[tc.ID] = pl
			case "tool_result":
				var tr rawToolResult
				if json.Unmarshal(p.Data, &tr) != nil {
					continue
				}
				pl := out[tr.ToolCallID]
				pl.Meta = tr.Metadata
				pl.IsError = tr.IsError
				pl.HasResult = true
				out[tr.ToolCallID] = pl
			}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// lineRange is an inclusive 1-based line span.
type lineRange struct {
	Lo, Hi int
}

func (r lineRange) contains(lo, hi int) bool { return lo >= r.Lo && hi <= r.Hi }
func (r lineRange) overlaps(lo, hi int) bool { return lo <= r.Hi && hi >= r.Lo }
func (r lineRange) String() string           { return fmt.Sprintf("%d-%d", r.Lo, r.Hi) }

// postEditPad is the ±line padding around an edit span — the issue's
// ±10-line served-region band.
const postEditPad = 10

// editSite is the span footprint of the latest successful mutating
// call on a path — several ranges for multiedit/replace_all.
type editSite struct {
	Name    string
	Seq     int
	Windows []lineRange
	// Known is false when the mutation's span isn't derivable —
	// lsp_rename/lsp_replace_symbol workspace edits, missing result
	// metadata on older artifacts, or a mutation input that doesn't
	// parse. Views against it classify span_unknown rather than
	// guessing.
	Known bool
}

// realCall reports whether a labeled call is a genuine dispatched
// tool call — placeholders (canceled/interrupted/truncated) are
// sequence labels, not calls.
func realCall(c *CallRecord) bool {
	return !c.Canceled && !c.Interrupted && !c.Truncated
}

// mutatingCall reports whether a call mutates file content —
// WriteToolNames plus download, which writes as a side effect.
func mutatingCall(name string) bool {
	return tools.WriteToolNames[name] || name == tools.DownloadToolName
}

// viewRange resolves a read-class call's effective line range in
// 1-based coordinates: offset is the tool's 0-based start line, limit
// resolves non-positive/absent to the tool default — the same
// defaults viewWindow canonicalizes.
func viewRange(input string) (lo, hi int) {
	var f struct {
		Offset *int `json:"offset"`
		Limit  *int `json:"limit"`
	}
	off, lim := 0, tools.DefaultReadLimit
	if json.Unmarshal([]byte(input), &f) == nil {
		if f.Offset != nil {
			off = *f.Offset
		}
		if f.Limit != nil {
			lim = *f.Limit
		}
	}
	if off < 0 {
		off = 0
	}
	if lim <= 0 {
		lim = tools.DefaultReadLimit
	}
	return off + 1, off + lim
}

// editSpans derives the post-edit line windows of a successful
// mutating call, in NEW-file coordinates: the viewed file is the
// post-edit file, so each new_string occurrence's span in new_content
// is the region the view could land in. Empty new_string (a deletion)
// resolves via the old_string's position in old_content — the join
// point is the region. ok=false means the span isn't derivable.
func editSpans(name, input, meta string) ([]lineRange, bool) {
	switch name {
	case "edit", "multiedit":
		var in struct {
			OldString string `json:"old_string"`
			NewString string `json:"new_string"`
			Edits     []struct {
				OldString string `json:"old_string"`
				NewString string `json:"new_string"`
			} `json:"edits"`
		}
		var m struct {
			OldContent string `json:"old_content"`
			NewContent string `json:"new_content"`
		}
		if json.Unmarshal([]byte(input), &in) != nil ||
			json.Unmarshal([]byte(meta), &m) != nil {
			return nil, false
		}
		ops := in.Edits
		if in.OldString != "" || in.NewString != "" {
			ops = append(ops, struct {
				OldString string `json:"old_string"`
				NewString string `json:"new_string"`
			}{in.OldString, in.NewString})
		}
		if len(ops) == 0 {
			return nil, false
		}
		var windows []lineRange
		for _, op := range ops {
			windows = append(windows, opWindows(op.OldString, op.NewString, m.OldContent, m.NewContent)...)
		}
		if len(windows) == 0 {
			return nil, false
		}
		return windows, true
	case "write":
		var in struct {
			Content string `json:"content"`
		}
		if json.Unmarshal([]byte(input), &in) != nil || in.Content == "" {
			return nil, false
		}
		// A write rewrites the whole file — the post-write region is
		// everything it wrote.
		return []lineRange{{1, strings.Count(in.Content, "\n") + 1}}, true
	default:
		// lsp_rename/lsp_replace_symbol/download mutate content whose
		// span isn't recorded in the artifact.
		return nil, false
	}
}

// opWindows returns the ±postEditPad windows one edit op left: each
// new_string occurrence in the post-edit file (new_content), or the
// old_string's position in old_content for deletions.
func opWindows(oldString, newString, oldContent, newContent string) []lineRange {
	var windows []lineRange
	hay, needle := newContent, newString
	if needle == "" {
		// Deletion — the region is where old_string sat.
		hay, needle = oldContent, oldString
	}
	if needle == "" {
		return nil
	}
	for i := 0; i < len(hay); {
		j := strings.Index(hay[i:], needle)
		if j < 0 {
			break
		}
		start := 1 + strings.Count(hay[:i+j], "\n")
		end := start + strings.Count(needle, "\n")
		windows = append(windows, lineRange{max(1, start-postEditPad), end + postEditPad})
		i += j + len(needle)
	}
	return windows
}

// probePostEditWindow answers the #98 served-class question: for every
// view call, was the viewed range inside the ±10-line window of the
// previous successful mutation on the same path? in_window+overlap is
// the class a post-edit region feature could have served — the floor
// the issue asks the probe to reproduce.
func probePostEditWindow(cm *CallMetrics, pl map[string]callPayload) *ProbeReport {
	rep := &ProbeReport{
		Name:    "post-edit-window",
		Columns: []string{"seq", "turn", "path", "view_lines", "prior_edit", "edit_window", "class"},
		Counts:  map[string]int{},
	}
	sites := map[string]editSite{}
	for i := range cm.ToolCalls {
		c := &cm.ToolCalls[i]
		if !realCall(c) || len(c.Files) == 0 {
			continue
		}
		path := c.Files[0]
		p := pl[c.ID]
		switch {
		case mutatingCall(c.Name):
			if c.IsError || c.NoResult {
				continue // A failed mutation leaves no region.
			}
			windows, ok := editSpans(c.Name, p.Input, p.Meta)
			sites[path] = editSite{Name: c.Name, Seq: c.Seq, Windows: windows, Known: ok}
		case tools.ReadToolNames[c.Name]:
			if c.IsError || c.NoResult {
				continue // A failed read never fetched content.
			}
			lo, hi := viewRange(p.Input)
			site, ok := sites[path]
			class := "no_prior_edit"
			editRef, winRef := "-", "-"
			if ok {
				editRef = fmt.Sprintf("%s@%d", site.Name, site.Seq)
				if !site.Known {
					class = "span_unknown"
				} else {
					class = "out_of_window"
					overlap := false
					var w []string
					for _, r := range site.Windows {
						w = append(w, r.String())
						if r.contains(lo, hi) {
							class = "in_window"
						} else if r.overlaps(lo, hi) {
							overlap = true
						}
					}
					winRef = strings.Join(w, ",")
					if class == "out_of_window" && overlap {
						class = "overlap"
					}
				}
			}
			rep.Counts[class]++
			rep.Rows = append(rep.Rows, []string{
				fmt.Sprint(c.Seq), fmt.Sprint(c.Turn), shortenPath(path),
				fmt.Sprintf("%d-%d", lo, hi), editRef, winRef, class,
			})
		}
	}
	rep.Counts["views"] = rep.Counts["in_window"] + rep.Counts["overlap"] +
		rep.Counts["out_of_window"] + rep.Counts["no_prior_edit"] + rep.Counts["span_unknown"]
	rep.Counts["servable"] = rep.Counts["in_window"]
	rep.Notes = append(rep.Notes,
		fmt.Sprintf("window = ±%d lines around the latest successful mutation's new-content span", postEditPad),
		"servable = in_window (strict containment) — a partially-overlapping view fetched lines the region can't supply",
		"bash-carried mutations (sed -i, redirects) leave no prior_edit site — same blind spot as the analyzer")
	return rep
}

// probeViewEditSameFile answers the window-free version of the same
// question: how often does the model re-open a file it already
// mutated? Per-path table over paths with ≥1 successful mutation.
func probeViewEditSameFile(cm *CallMetrics, _ map[string]callPayload) *ProbeReport {
	rep := &ProbeReport{
		Name:    "view-edit-same-file",
		Columns: []string{"path", "mutations", "views_after_first", "last_edit_turn"},
		Counts:  map[string]int{},
	}
	type stat struct {
		muts, viewsAfter, lastMutTurn int
		mutated                       bool
	}
	stats := map[string]*stat{}
	for i := range cm.ToolCalls {
		c := &cm.ToolCalls[i]
		if !realCall(c) || len(c.Files) == 0 || c.IsError || c.NoResult {
			continue
		}
		path := c.Files[0]
		s := stats[path]
		if s == nil {
			s = &stat{}
			stats[path] = s
		}
		switch {
		case mutatingCall(c.Name):
			s.muts++
			s.mutated = true
			s.lastMutTurn = c.Turn
		case tools.ReadToolNames[c.Name]:
			if s.mutated {
				s.viewsAfter++
			}
		}
	}
	for _, path := range sortedKeys(stats) {
		s := stats[path]
		if s.muts == 0 {
			continue
		}
		rep.Counts["views_on_edited_files"] += s.viewsAfter
		rep.Rows = append(rep.Rows, []string{
			shortenPath(path), fmt.Sprint(s.muts),
			fmt.Sprint(s.viewsAfter), fmt.Sprint(s.lastMutTurn),
		})
	}
	for i := range cm.ToolCalls {
		c := &cm.ToolCalls[i]
		if realCall(c) && tools.ReadToolNames[c.Name] && !c.IsError && !c.NoResult {
			rep.Counts["views_total"]++
		}
	}
	rep.Counts["edited_files"] = len(rep.Rows)
	return rep
}

// probeTurnStartReread measures the collapse signature: the first
// successful view of a path in a turn, where that path was already
// mutated in an earlier turn. Each (path, turn) pair counts once —
// the re-open the model performs when context no longer carries the
// file. Rows are the per-file heat map the file-retention design
// needs.
func probeTurnStartReread(cm *CallMetrics, _ map[string]callPayload) *ProbeReport {
	rep := &ProbeReport{
		Name:    "turn-start-reread",
		Columns: []string{"turn", "seq", "path", "last_edit_turn", "calls_into_turn"},
		Counts:  map[string]int{},
	}
	lastMutTurn := map[string]int{}     // Path → latest mutation turn seen so far.
	firstViewInTurn := map[string]int{} // "path\x00turn" → first view seq.
	turnCalls := map[int]int{}          // Turn → real calls so far (position within turn).
	for i := range cm.ToolCalls {
		c := &cm.ToolCalls[i]
		if !realCall(c) {
			continue
		}
		turnCalls[c.Turn]++
		path := ""
		if len(c.Files) > 0 {
			path = c.Files[0]
		}
		if path != "" && tools.ReadToolNames[c.Name] && !c.IsError && !c.NoResult {
			key := path + "\x00" + fmt.Sprint(c.Turn)
			if _, seen := firstViewInTurn[key]; !seen {
				firstViewInTurn[key] = c.Seq
				if mutTurn, ok := lastMutTurn[path]; ok && mutTurn < c.Turn {
					rep.Rows = append(rep.Rows, []string{
						fmt.Sprint(c.Turn), fmt.Sprint(c.Seq), shortenPath(path),
						fmt.Sprint(mutTurn), fmt.Sprint(turnCalls[c.Turn]),
					})
					rep.Counts["turn_start_rereads"]++
				}
			}
		}
		if path != "" && mutatingCall(c.Name) && !c.IsError && !c.NoResult {
			lastMutTurn[path] = c.Turn
		}
	}
	rep.Notes = append(rep.Notes,
		"a (path,turn) counts once — the first successful view of a file mutated in an earlier turn",
		"calls_into_turn is the view's ordinal among real calls in its turn")
	return rep
}

// shortenPath trims a normalized absolute path for the table — the
// run-workdir prefix is noise once paths are comparable.
func shortenPath(p string) string {
	const maxLen = 60
	if len(p) <= maxLen {
		return p
	}
	return "…" + p[len(p)-maxLen:]
}
