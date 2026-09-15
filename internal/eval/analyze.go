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
	// write-class call, -1 when none ran.
	FirstWriteIndex int `json:"first_write_index"`
	// RequestsToFirstEdit is the 1-based request count through the
	// request carrying the first write call, -1 when none ran.
	RequestsToFirstEdit int `json:"requests_to_first_edit"`
	// DiscoveryCallsBeforeWrite counts discovery-class calls before
	// the first write: grep/glob/ls, the LSP read tools, sourcegraph,
	// agent delegation, and view/read of a not-yet-viewed file. map is
	// deliberately excluded — the metric is the traditional-discovery
	// roundtrip count map is meant to replace.
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
	// CanceledCalls counts mid-stream placeholders (finished=true,
	// input="{}") — labeled, never counted as real calls.
	CanceledCalls int `json:"canceled_calls"`
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
	Turn     int      `json:"turn"`
	Name     string   `json:"name"`
	IsError  bool     `json:"is_error"`
	NoResult bool     `json:"no_result,omitempty"` // Call persisted, result never landed (hard kill).
	Canceled bool     `json:"canceled,omitempty"`  // finished=true input="{}" placeholder — not a real call.
	Cause    string   `json:"cause,omitempty"`     // Error bucket: hook|not_found|cancelled|permission|other.
	Files    []string `json:"files,omitempty"`     // Normalized paths referenced by the call input.
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

// repairPromptPrefixes identify harness-authored retry prompts the run
// edges enqueue inside the same `crush run` process (run_edges.go) —
// they carry a user role but do not start a new process turn. Keep in
// sync with verificationRetrySection/todosRetrySection/stallRetrySection.
var repairPromptPrefixes = []string{
	"Verification failed.",
	"The todo list still has",
	"The previous attempt was stopped",
}

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

	// created_at is second-granularity and steps routinely share a
	// second — rowid (insertion order) is the tiebreaker. Ordering by
	// created_at alone would silently corrupt first_write_index.
	rows, err := conn.QueryContext(ctx,
		`SELECT role, parts, is_summary_message FROM messages
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
			if summary == 0 {
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
				pending = append(pending, pendingCall{rec: rec, input: tc.Input})
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
		if isCanceledCall(c.input, hasResult, &res) {
			// Labeled in the sequence, never counted as a real call.
			c.rec.Canceled = true
			cm.CanceledCalls++
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

// pendingCall is a call record mid-reconstruction — the input is kept
// off the emitted record but is needed for canceled/symbol checks.
type pendingCall struct {
	rec   CallRecord
	input string
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
	ID    string `json:"id"`
	Name  string `json:"name"`
	Input string `json:"input"`
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
	for _, p := range repairPromptPrefixes {
		if strings.HasPrefix(text, p) {
			return false
		}
	}
	return true
}

// isCanceledCall identifies the placeholder rows a mid-stream cancel
// persists: the error path stamps finished=true, input="{}" and writes
// a synthetic is_error result. A real map{} skeleton call shares the
// input shape, so the result content (or its absence) is the
// discriminator.
func isCanceledCall(input string, hasResult bool, res *rawToolResult) bool {
	if input != "{}" && input != "" {
		return false
	}
	if !hasResult {
		return true
	}
	if !res.IsError {
		return false
	}
	return strings.Contains(res.Content, "cancelled") ||
		strings.Contains(res.Content, "canceled") ||
		res.Content == "There was an error while executing the tool"
}

// errorCause buckets an errored tool result. The hook bucket reads the
// persisted metadata, not content — hooked_tool stamps
// {"hook":{"decision":"deny"|"halt":true}} onto blocked calls. The rest
// discriminate on stable content markers; permission denials are
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
	case strings.Contains(strings.ToLower(res.Content), "permission denied"),
		strings.Contains(strings.ToLower(res.Content), "permission request"):
		return "permission"
	default:
		return "other"
	}
}

// isDiscoveryCall reports whether a call belongs to the
// discovery-before-write class. Read-class calls qualify only on a
// path not yet successfully viewed — a re-read is a reread.
func isDiscoveryCall(name string, isRead bool, path string, seen map[string]int) bool {
	if discoveryToolNames[name] {
		return true
	}
	if !isRead {
		return false
	}
	if path == "" {
		// No extractable path — unseen-ness unknowable, but the call
		// is still a discovery act.
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
		if c.rec.Name != "map" || c.rec.Canceled || c.rec.IsError {
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
