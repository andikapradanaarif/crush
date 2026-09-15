package eval

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/charmbracelet/crush/internal/agent"
	"github.com/charmbracelet/crush/internal/agent/tools"
	"github.com/charmbracelet/crush/internal/db"
)

// CallMetrics is the per-call sequence analysis merged into RunRecord
// as call_metrics — the gate metrics RunRecord aggregates can't
// express: which calls ran, in what order, with what outcome.
//
// Flag-invariance: fields derivable in both arms are
// coverage-registerable (see coverage.go). map_*, question_*, and
// wrong_pointer_events are structurally flag-dependent — map isn't
// registered in the control arm and question isn't registered headless
// — and can never be coverage predicates; they stay forensic fields.
//
// Documented blind spot: bash-carried discovery (cat/find/rg/go doc)
// is invisible to tool-name classification, so
// discovery_calls_before_write undercounts systematically.
type CallMetrics struct {
	// SessionID is the analyzed parent session. Sub-agent child
	// sessions are excluded — their work is charged to the parent's
	// `agent` call.
	SessionID string `json:"session_id"`
	// Requests counts non-summary assistant messages — one per
	// PrepareStep, i.e. one per model request. Divergence from
	// RunRecord.Steps flags the edge rows (canceled pre-request turns,
	// mid-request errors) the correctness rules exist for.
	Requests int `json:"requests"`
	// Calls counts real tool calls — canceled mid-stream placeholders
	// are labeled and excluded.
	Calls int `json:"calls"`
	// ToolCalls is the reconstructed call sequence, in order.
	ToolCalls []CallRecord `json:"tool_calls,omitempty"`

	// FirstWriteIndex is the call-sequence index of the first
	// write-class call (tools.WriteToolNames), -1 when none ran.
	// Blind spot: bash mutations (sed -i, redirects) and download
	// writes are invisible to the tool-name vocabulary — a run that
	// mutates only through bash keeps accruing discovery calls and
	// shows -1 here. The wider mutation vocabulary lives in the
	// scope gate (isMutatingCall); this metric deliberately tracks
	// the stub-machinery write class the gates assert on.
	FirstWriteIndex int `json:"first_write_index"`
	// RequestsToFirstEdit is the 1-based request count through the
	// request carrying the first write call, -1 when none ran.
	RequestsToFirstEdit int `json:"requests_to_first_edit"`
	// DiscoveryCallsBeforeWrite counts discovery-class calls before
	// the first write: grep/glob/ls, the LSP read tools, sourcegraph,
	// agent delegation, and view/read of a not-yet-viewed file.
	// Attempts count — the gate measures roundtrips spent before
	// acting, so a failed view or bad-regex grep still counts (a
	// failed read never joins the seen-set, so it can't become a
	// reread; view-on-directory also lands in ViewDirectoryErrors).
	// map is deliberately excluded — the metric is the
	// traditional-discovery roundtrip count map is meant to replace.
	DiscoveryCallsBeforeWrite int `json:"discovery_calls_before_write"`
	// FilesViewed is the reconstructed seen-set size — paths with at
	// least one successful view/read. Cross-checkable against the
	// read_files table.
	FilesViewed int `json:"files_viewed"`

	// EditFailures counts is_error results on write-class calls,
	// bucketed by cause: a hook halt, a hallucinated tool name, a
	// cancellation, and a real edit error are different signals one
	// flat rate would hide.
	EditFailures           int `json:"edit_failures"`
	EditFailuresHook       int `json:"edit_failures_hook"`
	EditFailuresNotFound   int `json:"edit_failures_not_found"`
	EditFailuresCancelled  int `json:"edit_failures_cancelled"`
	EditFailuresPermission int `json:"edit_failures_permission"`
	EditFailuresOther      int `json:"edit_failures_other"`

	// MapCalls counts attempted `map` calls — the control arm has no
	// map registered, so attempts there persist as is_error
	// "tool not found" results; MapCallsOK counts non-error calls.
	MapCalls   int `json:"map_calls"`
	MapCallsOK int `json:"map_calls_ok"`
	// MapResultBytes is the injected-context size of successful map
	// results — the map tax. Per-call input tokens don't exist (usage
	// is per-request); the compounding cost of re-reading the result
	// on later steps shows up in tokens.cache_read instead.
	MapResultBytes int64 `json:"map_result_bytes"`

	// QuestionCalls counts `question` tool calls; errored includes
	// unanswered calls (no persisted result). Headless runs never have
	// the tool in schema, so eval question calls are hallucinations
	// landing as is_error "tool not found" — the degradation signal.
	QuestionCalls        int `json:"question_calls"`
	QuestionCallsErrored int `json:"question_calls_errored"`

	// Rereads counts view/read calls on an already-viewed path, split
	// by process turn: a same-turn re-view is the in-turn loop
	// re-reading; a cross-turn re-view is a fresh `crush run` process
	// on the same session — partly structural (rebuilt prompt).
	Rereads          int `json:"rereads"`
	RereadsSameTurn  int `json:"rereads_same_turn"`
	RereadsCrossTurn int `json:"rereads_cross_turn"`

	// WrongPointerEvents counts map(symbol=S) calls followed within
	// wrongPointerWindow calls by a grep whose pattern contains S —
	// the model re-searched what it already asked the index.
	// Conservative by design: false negatives are acceptable, false
	// positives poison the gate.
	WrongPointerEvents int `json:"wrong_pointer_events"`
	// CanceledCalls counts user-cancel placeholders (finished=true,
	// input="{}", cancel-marked result); InterruptedCalls counts
	// never-executed placeholders from other causes (generic cleanup
	// error or no result); TruncatedCalls counts input=""/
	// finished=false rows — a stream cut mid-call. All are labeled in
	// the sequence and none count as real calls.
	CanceledCalls    int `json:"canceled_calls"`
	InterruptedCalls int `json:"interrupted_calls"`
	TruncatedCalls   int `json:"truncated_calls"`
	// ViewDirectoryErrors counts view/read calls that failed with
	// "is a directory" — classified explicitly rather than counted
	// as reads.
	ViewDirectoryErrors int `json:"view_directory_errors"`
}

// CallRecord is one reconstructed tool call, in sequence order.
type CallRecord struct {
	ID   string `json:"id"`
	Seq  int    `json:"seq"`  // position in the call sequence
	Step int    `json:"step"` // request index (0-based) of the containing assistant message
	// Turn is the process-turn index (0-based): a new `crush run`
	// process on the session, not a user-message count — repair turns
	// enqueued by the run edges stay inside the firing process.
	Turn     int    `json:"turn"`
	Name     string `json:"name"`
	IsError  bool   `json:"is_error"`
	NoResult bool   `json:"no_result,omitempty"` // Call persisted, result never landed (hard kill).
	// Canceled is the user-cancel placeholder (finished=true,
	// input="{}", cancel-marked result); Interrupted is the
	// never-executed placeholder from any other cause (generic cleanup
	// error, or no result at all); Truncated is a stream cut mid-call
	// (input="" or finished=false). None count as a real call.
	Canceled    bool     `json:"canceled,omitempty"`
	Interrupted bool     `json:"interrupted,omitempty"`
	Truncated   bool     `json:"truncated,omitempty"`
	Cause       string   `json:"cause,omitempty"` // Error bucket: hook|not_found|cancelled|permission|other.
	Files       []string `json:"files,omitempty"` // Normalized paths referenced by the call input.
}

// AnalyzeOptions scopes one analysis pass.
type AnalyzeOptions struct {
	// SessionID selects the session to analyze; empty picks the most
	// recently created parent session.
	SessionID string
	// Workdir anchors relative tool-call paths for normalization — the
	// run's working dir when known. The analyzer's own CWD is the wrong
	// anchor (agent-path relativity is to the materialized workdir), so
	// when empty, relative paths compare in relative spelling: same
	// spellings match, mixed relative/absolute spellings undercount
	// rereads rather than fabricate them.
	Workdir string
	// Turns are the trajectory's turn prompts when known — a user
	// message equal to the next expected turn starts a new process
	// turn; any other user message is an in-process repair turn. Empty
	// falls back to repair-prompt fingerprinting.
	Turns []string
}

// wrongPointerWindow bounds the call lookahead for wrong-pointer
// detection.
const wrongPointerWindow = 10

// discoveryToolNames are calls whose purpose is locating context
// before acting — the class discovery_calls_before_write counts.
// Deliberately not tools.CommandToolNames: that set exists for
// stub-supersession semantics and omits the LSP read tools,
// sourcegraph, and agent delegation, all of which are discovery in
// eval runs (auto_lsp is default-on and the sub-agent is read-only).
// view/read join conditionally — a read of an unseen file is
// discovery, a re-read is a reread.
var discoveryToolNames = map[string]bool{
	"grep": true, "glob": true, "ls": true,
	"lsp_definition": true, "lsp_references": true, "lsp_symbols": true,
	"lsp_call_hierarchy": true, "lsp_diagnostics": true,
	"sourcegraph": true, "agent": true,
}

// Repair-prompt fingerprints live in agent.RepairPromptPrefixes —
// the run_edges section builders render from the same constants.

// AnalyzeSessionDB reconstructs the tool-call sequence of a session DB
// and derives the call-level gate metrics from it. dbPath is a
// preserved artifact or a live session DB.
func AnalyzeSessionDB(ctx context.Context, dbPath string, opts AnalyzeOptions) (*CallMetrics, error) {
	conn, err := db.ConnectReadOnly(ctx, dbPath)
	if err != nil {
		return nil, fmt.Errorf("open session db: %w", err)
	}
	defer conn.Close()

	sessionID := opts.SessionID
	if sessionID == "" {
		sessionID, err = latestParentSession(ctx, conn)
		if err != nil {
			return nil, err
		}
	}

	// is_summary_message postdates early schemas — a preserved DB from
	// before that migration must still analyze.
	summaryCol := "0"
	if hasColumn(ctx, conn, "messages", "is_summary_message") {
		summaryCol = "is_summary_message"
	}

	// created_at is second-granularity and steps routinely share a
	// second — rowid (insertion order) is the tiebreaker. Ordering by
	// created_at alone would silently corrupt first_write_index.
	rows, err := conn.QueryContext(ctx,
		`SELECT role, parts, `+summaryCol+` FROM messages
		 WHERE session_id = ? ORDER BY created_at, rowid`, sessionID)
	if err != nil {
		return nil, fmt.Errorf("list messages: %w", err)
	}
	defer rows.Close()

	var (
		cm       = &CallMetrics{SessionID: sessionID, FirstWriteIndex: -1, RequestsToFirstEdit: -1}
		pending  []pendingCall
		results  = map[string]rawToolResult{}
		request  = -1
		turn     = -1
		nextTurn = 0
	)
	for rows.Next() {
		var (
			role    string
			partsJS string
			summary int
		)
		if err := rows.Scan(&role, &partsJS, &summary); err != nil {
			return nil, fmt.Errorf("scan message: %w", err)
		}
		parts, err := decodeParts(partsJS)
		if err != nil {
			return nil, fmt.Errorf("decode parts: %w", err)
		}
		switch role {
		case "user":
			if isProcessBoundary(messageText(parts), opts.Turns, nextTurn) {
				nextTurn++
				turn++
			}
		case "assistant":
			if summary != 0 {
				// Summary rows carry no calls — don't scan parts, so a
				// stray part can't land a call on the wrong request.
				continue
			}
			// persistCanceledTurn writes a Finish{reason:"canceled"}-only
			// row — the canceled turn never produced a model response, so
			// it is not a request. A mid-stream cancel keeps its streamed
			// parts alongside the finish marker; that WAS a billed request
			// and its calls need this request's index.
			if !hasCanceledFinish(parts) || hasNonFinishPart(parts) {
				request++
			}
			for _, p := range parts {
				if p.Type != "tool_call" {
					continue
				}
				var tc rawToolCall
				if err := json.Unmarshal(p.Data, &tc); err != nil {
					continue // Tolerate a part shape newer than the analyzer.
				}
				rec := CallRecord{
					ID:   tc.ID,
					Seq:  len(pending),
					Step: request,
					Turn: max(turn, 0),
					Name: tc.Name,
				}
				if f := tools.ToolCallFilePath(tc.Input); f != "" {
					rec.Files = []string{normalizeCallPath(opts.Workdir, f)}
				}
				pending = append(pending, pendingCall{rec: rec, input: tc.Input, finished: tc.Finished})
			}
		case "tool":
			for _, p := range parts {
				if p.Type != "tool_result" {
					continue
				}
				var tr rawToolResult
				if err := json.Unmarshal(p.Data, &tr); err != nil {
					continue
				}
				results[tr.ToolCallID] = tr
			}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	cm.Requests = request + 1

	// Pass 2 — join results, then walk the sequence in order: seen-set
	// updates, discovery counting, and first-write detection all depend
	// on call position.
	seenTurn := map[string]int{}
	for i := range pending {
		c := &pending[i]
		res, hasResult := results[c.rec.ID]
		c.rec.NoResult = !hasResult
		if hasResult {
			c.rec.IsError = res.IsError
			c.rec.Cause = errorCause(&res)
		}
		// Labeled in the sequence, never counted as a real call.
		switch placeholderKind(c.input, c.finished, hasResult, &res) {
		case callCanceled:
			c.rec.Canceled = true
			cm.CanceledCalls++
			cm.ToolCalls = append(cm.ToolCalls, c.rec)
			continue
		case callInterrupted:
			c.rec.Interrupted = true
			cm.InterruptedCalls++
			cm.ToolCalls = append(cm.ToolCalls, c.rec)
			continue
		case callTruncated:
			c.rec.Truncated = true
			cm.TruncatedCalls++
			cm.ToolCalls = append(cm.ToolCalls, c.rec)
			continue
		}
		cm.Calls++
		cm.ToolCalls = append(cm.ToolCalls, c.rec)

		switch c.rec.Name {
		case "map":
			cm.MapCalls++
			if hasResult && !res.IsError {
				cm.MapCallsOK++
				cm.MapResultBytes += int64(len(res.Content))
			}
		case "question":
			cm.QuestionCalls++
			if !hasResult || res.IsError {
				cm.QuestionCallsErrored++
			}
		}

		isRead := tools.ReadToolNames[c.rec.Name]
		path := ""
		if len(c.rec.Files) > 0 {
			path = c.rec.Files[0]
		}
		if isRead && hasResult && res.IsError && strings.Contains(res.Content, "is a directory") {
			cm.ViewDirectoryErrors++
		}

		// Discovery and rereads classify against the seen-set as it
		// stood at this call — updates happen after classification.
		// Both count attempts (roundtrips spent); only the seen-set
		// update is success-gated, so a failed read is a spent
		// discovery attempt that can't later become a reread.
		if cm.FirstWriteIndex < 0 && isDiscoveryCall(c.rec.Name, isRead, path, seenTurn) {
			cm.DiscoveryCallsBeforeWrite++
		}
		if isRead && path != "" {
			if lastTurn, seen := seenTurn[path]; seen {
				cm.Rereads++
				if c.rec.Turn == lastTurn {
					cm.RereadsSameTurn++
				} else {
					cm.RereadsCrossTurn++
				}
			}
			if hasResult && !res.IsError {
				seenTurn[path] = c.rec.Turn
			}
		}

		if tools.WriteToolNames[c.rec.Name] && cm.FirstWriteIndex < 0 {
			cm.FirstWriteIndex = c.rec.Seq
			cm.RequestsToFirstEdit = c.rec.Step + 1
		}
		if c.rec.IsError && tools.WriteToolNames[c.rec.Name] {
			cm.EditFailures++
			switch c.rec.Cause {
			case "hook":
				cm.EditFailuresHook++
			case "not_found":
				cm.EditFailuresNotFound++
			case "cancelled":
				cm.EditFailuresCancelled++
			case "permission":
				cm.EditFailuresPermission++
			default:
				cm.EditFailuresOther++
			}
		}
	}
	cm.FilesViewed = len(seenTurn)
	cm.WrongPointerEvents = wrongPointerEvents(pending)
	return cm, nil
}

// pendingCall is a call record mid-reconstruction — the input and
// finished state are kept off the emitted record but needed for
// placeholder/symbol checks.
type pendingCall struct {
	rec      CallRecord
	input    string
	finished bool
}

// hasColumn reports whether a table carries a column, for analyzing
// artifacts produced before a migration landed.
func hasColumn(ctx context.Context, conn *sql.DB, table, col string) bool {
	rows, err := conn.QueryContext(ctx, `SELECT name FROM pragma_table_info(?)`, table)
	if err != nil {
		return false
	}
	defer rows.Close()
	var found bool
	for rows.Next() {
		var name string
		if rows.Scan(&name) == nil && name == col {
			found = true
		}
	}
	return found && rows.Err() == nil
}

// hasCanceledFinish detects a finish marker with reason "canceled".
// Combined with hasNonFinishPart it distinguishes the finish-only
// persistCanceledTurn placeholder (not a request) from a mid-stream
// cancel (a real request that was cut off).
// hasNonFinishPart reports whether a row carries anything besides a finish
// marker — a mid-stream-canceled request is still a real billed request and
// its calls need their own request index.
func hasNonFinishPart(parts []rawPart) bool {
	for _, p := range parts {
		if p.Type != "finish" {
			return true
		}
	}
	return false
}

func hasCanceledFinish(parts []rawPart) bool {
	for _, p := range parts {
		if p.Type != "finish" {
			continue
		}
		var f struct {
			Reason string `json:"reason"`
		}
		if json.Unmarshal(p.Data, &f) == nil && f.Reason == "canceled" {
			return true
		}
	}
	return false
}

// latestParentSession picks the most recently created top-level
// session — sub-agent child sessions share the DB but are out of scope.
func latestParentSession(ctx context.Context, conn *sql.DB) (string, error) {
	var id string
	err := conn.QueryRowContext(ctx,
		`SELECT id FROM sessions WHERE parent_session_id IS NULL
		 ORDER BY created_at DESC, rowid DESC LIMIT 1`).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", errors.New("no parent session in database")
	}
	return id, err
}

// rawPart is the stored parts wrapper; unknown part types are skipped,
// not errors — the analyzer must tolerate part shapes newer than itself.
type rawPart struct {
	Type string          `json:"type"`
	Data json.RawMessage `json:"data"`
}

type rawToolCall struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Input    string `json:"input"`
	Finished bool   `json:"finished"`
}

type rawToolResult struct {
	ToolCallID string `json:"tool_call_id"`
	Content    string `json:"content"`
	Metadata   string `json:"metadata"`
	IsError    bool   `json:"is_error"`
}

func decodeParts(partsJS string) ([]rawPart, error) {
	var parts []rawPart
	if err := json.Unmarshal([]byte(partsJS), &parts); err != nil {
		return nil, err
	}
	return parts, nil
}

// messageText concatenates a message's text parts — user prompts are
// plain text.
func messageText(parts []rawPart) string {
	var b strings.Builder
	for _, p := range parts {
		if p.Type != "text" {
			continue
		}
		var t struct {
			Text string `json:"text"`
		}
		if json.Unmarshal(p.Data, &t) == nil {
			b.WriteString(t.Text)
		}
	}
	return b.String()
}

// isProcessBoundary reports whether a user message starts a new
// process turn. With the trajectory's turns known, an exact match
// against the next expected turn is authoritative — unmatched user
// messages are repair-turn prompts enqueued inside the same process.
// Without turns, the fixed repair-prompt prefixes are the fingerprint.
func isProcessBoundary(text string, turns []string, nextTurn int) bool {
	if len(turns) > 0 {
		if nextTurn < len(turns) && text == turns[nextTurn] {
			return true
		}
		return false
	}
	for _, p := range agent.RepairPromptPrefixes {
		if strings.HasPrefix(text, p) {
			return false
		}
	}
	return true
}

// Placeholder dispositions — none count as real calls.
const (
	// callCanceled is the user-cancel placeholder: the cancel path
	// stamps finished=true, input="{}" and writes a cancel-marked
	// is_error result.
	callCanceled = "canceled"
	// callInterrupted is the never-executed placeholder from any other
	// cause — the generic "There was an error while executing the
	// tool" cleanup result (written for any missing-result cleanup,
	// not just cancels) or no result at all.
	callInterrupted = "interrupted"
	// callTruncated is a different artifact shape: input="" or
	// finished=false — OnToolInputStart persisted the part and
	// OnToolCall never ran, i.e. a stream cut mid-call (hard-kill).
	callTruncated = "truncated"
)

// placeholderKind classifies rows that never executed a tool,
// discriminating on shape and result content. An input="{}" call with
// a real result is a genuine zero-argument call (e.g. the map
// skeleton) — never a placeholder.
func placeholderKind(input string, finished bool, hasResult bool, res *rawToolResult) string {
	if input == "" || !finished {
		return callTruncated
	}
	if input != "{}" {
		return ""
	}
	if hasResult && res.IsError &&
		(strings.Contains(res.Content, "cancelled") || strings.Contains(res.Content, "canceled")) {
		return callCanceled
	}
	if !hasResult || (res.IsError && res.Content == "There was an error while executing the tool") {
		return callInterrupted
	}
	return ""
}

// errorCause buckets an errored tool result. The hook bucket reads the
// persisted metadata, not content — hooked_tool stamps
// {"hook":{"decision":"deny"|"halt":true}} onto blocked calls. The rest
// discriminate on stable content markers. All permission denials funnel
// through tools.NewPermissionDeniedResponse ("User denied permission");
// matching that literal avoids the lsp_rename infra string
// "permission request failed" misreading as a denial. Denials are
// vacuous under eval (non-interactive sessions auto-approve) but real
// sessions produce them.
func errorCause(res *rawToolResult) string {
	if res == nil || !res.IsError {
		return ""
	}
	var meta struct {
		Hook *struct {
			Decision string `json:"decision"`
			Halt     bool   `json:"halt"`
		} `json:"hook"`
	}
	if res.Metadata != "" &&
		json.Unmarshal([]byte(res.Metadata), &meta) == nil &&
		meta.Hook != nil && (meta.Hook.Halt || meta.Hook.Decision == "deny") {
		return "hook"
	}
	switch {
	case strings.HasPrefix(res.Content, "tool not found:"):
		return "not_found"
	case strings.Contains(res.Content, "cancelled"), strings.Contains(res.Content, "canceled"):
		return "cancelled"
	case strings.Contains(res.Content, "denied permission"):
		return "permission"
	default:
		return "other"
	}
}

// isDiscoveryCall reports whether a call belongs to the
// discovery-before-write class. Attempts count uniformly: a failed
// read still spent the roundtrip — it just never joins the seen-set.
func isDiscoveryCall(name string, isRead bool, path string, seen map[string]int) bool {
	if discoveryToolNames[name] {
		return true
	}
	if !isRead {
		return false
	}
	if path == "" {
		// A read-class call with no extractable path is still a
		// discovery act.
		return true
	}
	_, ok := seen[path]
	return !ok
}

// normalizeCallPath keys a tool-call path for seen-set comparison.
// Unlike the agent's normalizedPath it never resolves against the
// analyzer's CWD — relative spellings were relative to the run's
// workdir. Same-case-folding rule as the agent (darwin/windows) so a
// spelling variant can't dodge the seen-set.
func normalizeCallPath(workdir, p string) string {
	if !filepath.IsAbs(p) && workdir != "" {
		p = filepath.Join(workdir, p)
	}
	p = filepath.Clean(p)
	if runtime.GOOS == "darwin" || runtime.GOOS == "windows" {
		p = strings.ToLower(p)
	}
	return p
}

// wrongPointerEvents counts map(symbol=S) calls followed within
// wrongPointerWindow calls by a grep whose pattern contains S. Symbol
// and pattern both come from call input JSON — never from rendered
// output, which would couple the metric to render.go's format.
func wrongPointerEvents(calls []pendingCall) int {
	var n int
	for i := range calls {
		c := &calls[i]
		// The source map call must have delivered a result — a call
		// whose result never persisted gave the model no pointer to
		// re-search.
		if c.rec.Name != "map" || c.rec.Canceled || c.rec.Interrupted ||
			c.rec.IsError || c.rec.NoResult {
			continue
		}
		var in struct {
			Symbol string `json:"symbol"`
		}
		if json.Unmarshal([]byte(c.input), &in) != nil || len(in.Symbol) < 3 {
			continue
		}
		for j := i + 1; j < len(calls) && j <= i+wrongPointerWindow; j++ {
			g := &calls[j]
			if g.rec.Name != "grep" || g.rec.Canceled {
				continue
			}
			var gin struct {
				Pattern string `json:"pattern"`
			}
			if json.Unmarshal([]byte(g.input), &gin) == nil && strings.Contains(gin.Pattern, in.Symbol) {
				n++
				break
			}
		}
	}
	return n
}
