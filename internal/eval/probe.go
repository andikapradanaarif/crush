package eval

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"runtime"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/charmbracelet/crush/internal/agent/tools"
	"github.com/charmbracelet/crush/internal/db"
	"github.com/charmbracelet/crush/internal/toolclass"
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

// RunProbe runs one named probe over a session DB.
func RunProbe(ctx context.Context, dbPath, name string, opts AnalyzeOptions) (*ProbeReport, error) {
	reps, err := RunProbes(ctx, dbPath, []string{name}, opts)
	if err != nil {
		return nil, err
	}
	return reps[0], nil
}

// RunProbes runs several probes over a session DB, sharing the
// analysis pass — the DB is read once regardless of probe count. The
// opts are the analyzer's — SessionID picks the session (default
// latest parent), Workdir anchors relative call paths, Turns supply
// authoritative process-turn boundaries, GOOS normalizes foreign-OS
// path spellings.
func RunProbes(ctx context.Context, dbPath string, names []string, opts AnalyzeOptions) ([]*ProbeReport, error) {
	for _, n := range names {
		if _, ok := probes[n]; !ok {
			return nil, fmt.Errorf("unknown probe %q (known: %s)", n, strings.Join(ProbeNames(), ", "))
		}
	}
	cm, err := AnalyzeSessionDB(ctx, dbPath, opts)
	if err != nil {
		return nil, err
	}
	payloads, err := loadCallPayloads(ctx, dbPath, cm.SessionID)
	if err != nil {
		return nil, err
	}
	reps := make([]*ProbeReport, 0, len(names))
	for _, n := range names {
		rep := probes[n](cm, payloads, opts)
		rep.Session = cm.SessionID
		reps = append(reps, rep)
	}
	return reps, nil
}

// probes is the registry of named probes. Probes are additive —
// register new mechanism questions here rather than widening
// CallMetrics for one-off checks.
var probes = map[string]func(*CallMetrics, map[string]callPayload, AnalyzeOptions) *ProbeReport{
	"post-edit-window":    probePostEditWindow,
	"view-edit-same-file": probeViewEditSameFile,
	"turn-start-reread":   probeTurnStartReread,
}

// callPayload is the per-call raw material the labeled CallRecord
// doesn't carry: the call's input JSON and its result's metadata JSON
// (edit results persist old_content/new_content there — the probe's
// edit-span source — and view results persist the fetched content,
// which clamps the requested range to what was actually delivered).
type callPayload struct {
	Input     string
	Meta      string
	IsError   bool
	HasResult bool
}

// loadCallPayloads scans the session's parts a second time for inputs
// and result metadata, keyed by tool-call ID. Ordering and labels come
// from AnalyzeSessionDB — this pass is a dumb id→payload map and
// deliberately re-derives nothing about the sequence. A probe on a
// live DB can see a message land between this scan and the analyzer's;
// the payload map is keyed by call ID, so the worst case is a call
// with no payload, never a cross-call skew.
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
	// lsp_rename/lsp_replace_symbol workspace edits, bash redirects
	// (the path is known, the written range isn't), missing result
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

// mutationPaths resolves the normalized paths a mutating call wrote.
// file_path-carried tools give one path; bash binds its redirect
// targets ("> out", ">> out"). unbound marks a mutation with no
// recoverable path — sed -i/tee/cp arguments and lsp_rename's
// search-scope path don't bind a file — a call that mutates
// something but can't arm a site. Workdir/goos normalize bash
// targets the same way the analyzer normalized Files.
func mutationPaths(c *CallRecord, p callPayload, workdir, goos string) (paths []string, unbound bool) {
	if len(c.Files) > 0 {
		return c.Files[:1], false
	}
	if c.Name != "bash" {
		return nil, true
	}
	var in struct {
		Command string `json:"command"`
	}
	if json.Unmarshal([]byte(p.Input), &in) == nil {
		for _, t := range toolclass.BashRedirectTargets(in.Command) {
			paths = append(paths, normalizeCallPath(workdir, t, goos))
		}
	}
	return paths, len(paths) == 0
}

// viewRange resolves a read-class call's line range in 1-based
// coordinates: offset is the tool's 0-based start line, limit
// resolves non-positive/absent to the tool default — the same
// defaults viewWindow canonicalizes — and the fetched content in the
// result metadata clamps the requested range to what the file
// actually delivered (a 1-200 view on a 17-line file fetched 17).
// An absent or empty metadata content can't distinguish a short
// read from unpersisted data, so the requested range stands.
func viewRange(input, meta string) (lo, hi int) {
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
	lo = off + 1
	if n := fetchedLines(meta); n > 0 {
		return lo, min(off+lim, off+n)
	}
	return lo, off + lim
}

// fetchedLines counts delivered lines from a view result's metadata
// content — 0 when content is absent or empty, which callers treat
// as "requested range stands" rather than a zero-line read.
func fetchedLines(meta string) int {
	var m struct {
		Content string `json:"content"`
	}
	if json.Unmarshal([]byte(meta), &m) != nil || m.Content == "" {
		return 0
	}
	n := strings.Count(m.Content, "\n")
	if !strings.HasSuffix(m.Content, "\n") {
		n++
	}
	return n
}

// editSpans derives the post-edit line windows of a successful
// mutating call, in NEW-file coordinates: the viewed file is the
// post-edit file, so the touched region is where each op landed.
// ok=false means the span isn't derivable.
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
		// bash redirects, lsp_rename/lsp_replace_symbol/download
		// mutate content whose span isn't recorded in the artifact.
		return nil, false
	}
}

// opWindows returns the ±postEditPad windows one edit op left.
// Resolution prefers the pre-edit position: for a unique-match edit
// the old_string site IS the true location, and for replace_all
// every occurrence is a real site — new-side search can't inflate
// on coincidental duplicates of new_string. Ops that can't resolve
// old-side (sequential multiedit on text an earlier op produced,
// file creation with an empty old_string) fall back to locating
// new_string in new_content. Post-edit height is new_string's line
// count either way; a deletion's new_string is empty, so its span
// is the zero-height join point.
func opWindows(oldString, newString, oldContent, newContent string) []lineRange {
	var windows []lineRange
	if oldString != "" {
		for i := 0; i < len(oldContent); {
			j := strings.Index(oldContent[i:], oldString)
			if j < 0 {
				break
			}
			start := 1 + strings.Count(oldContent[:i+j], "\n")
			end := start + strings.Count(newString, "\n")
			windows = append(windows, lineRange{max(1, start-postEditPad), end + postEditPad})
			i += j + len(oldString)
		}
	}
	if len(windows) == 0 && newString != "" {
		for i := 0; i < len(newContent); {
			j := strings.Index(newContent[i:], newString)
			if j < 0 {
				break
			}
			start := 1 + strings.Count(newContent[:i+j], "\n")
			end := start + strings.Count(newString, "\n")
			windows = append(windows, lineRange{max(1, start-postEditPad), end + postEditPad})
			i += j + len(newString)
		}
	}
	return windows
}

// probePostEditWindow answers the #98 served-class question: for every
// view call, was the viewed range inside the ±10-line window of the
// previous successful mutation on the same path? in_window is the
// class a post-edit region feature could have served — the floor the
// issue asks the probe to reproduce.
func probePostEditWindow(cm *CallMetrics, pl map[string]callPayload, opts AnalyzeOptions) *ProbeReport {
	rep := &ProbeReport{
		Name:    "post-edit-window",
		Columns: []string{"seq", "turn", "path", "view_lines", "prior_edit", "edit_window", "class"},
		Counts:  map[string]int{},
	}
	goos := opts.GOOS
	if goos == "" {
		goos = runtime.GOOS
	}
	sites := map[string]editSite{}
	// unboundMutation marks a successful mutation with no recoverable
	// path (sed -i, tee, cp — toolclass.IsMutatingCall detects, the
	// path extractor can't bind). A view with no known site after one
	// can't be proven pre-edit — span_unknown, not no_prior_edit.
	unboundMutation := false
	for i := range cm.ToolCalls {
		c := &cm.ToolCalls[i]
		if !realCall(c) {
			continue
		}
		p := pl[c.ID]
		if tools.IsMutatingCall(c.Name, p.Input) {
			if c.IsError || c.NoResult {
				continue // A failed mutation leaves no region.
			}
			paths, unbound := mutationPaths(c, p, opts.Workdir, goos)
			if unbound {
				unboundMutation = true
				rep.Counts["unbound_mutations"]++
			}
			for _, path := range paths {
				windows, ok := editSpans(c.Name, p.Input, p.Meta)
				sites[path] = editSite{Name: c.Name, Seq: c.Seq, Windows: windows, Known: ok}
			}
			continue
		}
		if len(c.Files) == 0 || !tools.ReadToolNames[c.Name] || c.IsError || c.NoResult {
			continue // No path or a read that never fetched content.
		}
		path := c.Files[0]
		lo, hi := viewRange(p.Input, p.Meta)
		site, ok := sites[path]
		class := "no_prior_edit"
		editRef, winRef := "-", "-"
		switch {
		case !ok && unboundMutation:
			class = "span_unknown"
		case !ok:
		case !site.Known:
			class = "span_unknown"
		default:
			editRef = fmt.Sprintf("%s@%d", site.Name, site.Seq)
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
		if ok && editRef == "-" {
			editRef = fmt.Sprintf("%s@%d", site.Name, site.Seq)
		}
		rep.Counts[class]++
		rep.Rows = append(rep.Rows, []string{
			fmt.Sprint(c.Seq), fmt.Sprint(c.Turn), shortenPath(path),
			fmt.Sprintf("%d-%d", lo, hi), editRef, winRef, class,
		})
	}
	rep.Counts["views"] = rep.Counts["in_window"] + rep.Counts["overlap"] +
		rep.Counts["out_of_window"] + rep.Counts["no_prior_edit"] + rep.Counts["span_unknown"]
	rep.Counts["servable"] = rep.Counts["in_window"]
	rep.Notes = append(rep.Notes,
		fmt.Sprintf("window = ±%d lines around the latest successful mutation's touched span", postEditPad),
		"servable = in_window (strict containment) — a partially-overlapping view fetched lines the region can't supply",
		"view_lines clamps to delivered content when the result metadata carries it; span_unknown = prior mutation's range not derivable (or an unbound bash mutation precedes)")
	return rep
}

// probeViewEditSameFile answers the window-free version of the same
// question: how often does the model re-open a file it already
// mutated? Per-path table over paths with ≥1 successful mutation.
func probeViewEditSameFile(cm *CallMetrics, pl map[string]callPayload, opts AnalyzeOptions) *ProbeReport {
	rep := &ProbeReport{
		Name:    "view-edit-same-file",
		Columns: []string{"path", "mutations", "views_after_first", "last_edit_turn"},
		Counts:  map[string]int{},
	}
	goos := opts.GOOS
	if goos == "" {
		goos = runtime.GOOS
	}
	type stat struct {
		muts, viewsAfter, lastMutTurn int
		mutated                       bool
	}
	stats := map[string]*stat{}
	for i := range cm.ToolCalls {
		c := &cm.ToolCalls[i]
		if !realCall(c) || c.IsError || c.NoResult {
			continue
		}
		p := pl[c.ID]
		if tools.IsMutatingCall(c.Name, p.Input) {
			paths, unbound := mutationPaths(c, p, opts.Workdir, goos)
			if unbound {
				rep.Counts["unbound_mutations"]++
			}
			for _, path := range paths {
				s := stats[path]
				if s == nil {
					s = &stat{}
					stats[path] = s
				}
				s.muts++
				s.mutated = true
				s.lastMutTurn = c.Turn
			}
			continue
		}
		if len(c.Files) == 0 || !tools.ReadToolNames[c.Name] {
			continue
		}
		if s := stats[c.Files[0]]; s != nil && s.mutated {
			s.viewsAfter++
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
func probeTurnStartReread(cm *CallMetrics, pl map[string]callPayload, opts AnalyzeOptions) *ProbeReport {
	rep := &ProbeReport{
		Name:    "turn-start-reread",
		Columns: []string{"turn", "seq", "path", "last_edit_turn", "calls_into_turn"},
		Counts:  map[string]int{},
	}
	goos := opts.GOOS
	if goos == "" {
		goos = runtime.GOOS
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
		p := pl[c.ID]
		if tools.IsMutatingCall(c.Name, p.Input) {
			if c.IsError || c.NoResult {
				continue
			}
			paths, unbound := mutationPaths(c, p, opts.Workdir, goos)
			if unbound {
				rep.Counts["unbound_mutations"]++
			}
			for _, path := range paths {
				lastMutTurn[path] = c.Turn
			}
			continue
		}
		if len(c.Files) == 0 || !tools.ReadToolNames[c.Name] || c.IsError || c.NoResult {
			continue
		}
		path := c.Files[0]
		key := path + "\x00" + fmt.Sprint(c.Turn)
		if _, seen := firstViewInTurn[key]; seen {
			continue
		}
		firstViewInTurn[key] = c.Seq
		if mutTurn, ok := lastMutTurn[path]; ok && mutTurn < c.Turn {
			rep.Rows = append(rep.Rows, []string{
				fmt.Sprint(c.Turn), fmt.Sprint(c.Seq), shortenPath(path),
				fmt.Sprint(mutTurn), fmt.Sprint(turnCalls[c.Turn]),
			})
			rep.Counts["turn_start_rereads"]++
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
