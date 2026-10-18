// Turn outcome recording (#340): one row per (session, user turn) —
// the acceptance unit procedural memory mines, and the label layer
// Layer B rates and future LTT consume. The referent channel labels
// only vague-prompt single-edit turns; this channel labels every
// turn that produced edits or commands, so the census has an input
// that exists.
//
// Two record paths share one segmentation rule — a turn is the span
// after a real user message (not a repair prompt, not the rewritten
// summary message) up to the next one, keyed by that message's ID:
//   - live: the turn under judgment is the last real user message in
//     msgs (msgs predates this run's prompt); the arriving prompt is
//     its next-turn verdict.
//   - backfill: at a session's first turn, recent sessions'
//     stored messages segment the same way and record missing rows.
//     The corpus is the project's own — ListRecentSessionIDs admits
//     only sessions with a project-keyed ledger row (sessions carry
//     no project column), so a shared store's foreign sessions never
//     mint turns under this project's key. A session-final turn's
//     outcome only ever arrives this way — no in-session turn ever
//     judges it.
//
// Settlement is the store's job; this file only opens rows.
package agent

import (
	"context"
	"encoding/json"
	"log/slog"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"github.com/charmbracelet/crush/internal/agent/tools"
	"github.com/charmbracelet/crush/internal/cmdlog"
	"github.com/charmbracelet/crush/internal/csync"
	"github.com/charmbracelet/crush/internal/diff"
	"github.com/charmbracelet/crush/internal/fsext"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/params"
	"github.com/charmbracelet/crush/internal/redact"
	"github.com/charmbracelet/crush/internal/session"
)

// turnOutcomeBackfillLimit bounds how many past sessions a session's
// first turn scans for missing outcome rows — the corpus is additive
// across sessions, so a small bound still converges: every new
// session's first turn drains another slice of history.
const turnOutcomeBackfillLimit = 25

// turnOutcomeDeps snapshots every agent field the outcome pass
// touches, captured on the caller's goroutine before the detached
// slot spawns — tests re-stamp agent fields mid-flight, and a
// detached read races them (the same hazard the settle pass
// documents at its spawn site). workdir is relWorkdir's basis,
// resolved now because configStore lives on the agent. reqStats and
// mp serve the referent backfill (#368): the episode mint reads the
// judged turn's cumulative token cost and the vagueness/promotion
// parameters — snapshotted alongside for the same off-goroutine
// reason. headSHA is a lazily memoized mint-time HEAD probe: every
// copy of the struct shares the once, so a sweep minting K episodes
// runs one rev-parse, and a deps that never mints runs none.
type turnOutcomeDeps struct {
	messages message.Service
	sessions session.Service
	cmdlog   cmdlog.Service
	workdir  string
	reqStats *csync.Map[string, requestStats]
	mp       params.Memory
	headSHA  func() string
}

// turnOutcomeDeps snapshots the fields on the caller's goroutine —
// the detached slot's copy.
func (a *sessionAgent) turnOutcomeDeps() turnOutcomeDeps {
	d := turnOutcomeDeps{
		messages: a.messages,
		sessions: a.sessions,
		cmdlog:   a.cmdlog,
		reqStats: a.reqStats,
		mp:       a.memoryParams(),
	}
	if a.configStore != nil {
		d.workdir = a.configStore.WorkingDir()
	}
	d.headSHA = sync.OnceValue(func() string {
		if d.workdir == "" {
			return ""
		}
		return headSHA(d.workdir)
	})
	return d
}

// recordTurnOutcome schedules the judged-turn record + corpus
// backfill on a detached slot — none of it feeds the render, and
// settlement tolerates late-arriving rows, so the transcript lists
// and ledger writes must not serialize into the prompt path. The
// guard and the field snapshot run on this goroutine; the detached
// closure reads only its parameters.
func (a *sessionAgent) recordTurnOutcome(ctx context.Context, call SessionAgentCall, msgs []message.Message, full []message.Message) {
	if a.cmdlog == nil || a.isSubAgent || call.RepairAttempts > 0 {
		return
	}
	// A prompt-less run supplies no cue, so it skips the judged-turn
	// record — "unknown" is a real verdict and would freeze wrong —
	// but it still sweeps: folded boundaries' verdicts come from
	// stored next prompts regardless of this call's prompt.
	deps := a.turnOutcomeDeps()
	detachedCtx := context.WithoutCancel(ctx)
	a.spawnDetached(func() {
		recordTurnOutcome(detachedCtx, deps, call, msgs, full)
	})
}

// recordTurnOutcome is the synchronous core — the judged turn is
// the last real user message before this run's prompt; the arriving
// prompt is its next-turn verdict. A session with no prior real
// user message is in its first turn — the backfill trigger.
func recordTurnOutcome(ctx context.Context, d turnOutcomeDeps, call SessionAgentCall, msgs []message.Message, full []message.Message) {
	lastUser := lastRealUserIdx(msgs, call.currentUserMsgID)
	if lastUser < 0 {
		// Two distinct shapes land here. A genuinely new session's
		// first turn triggers the corpus backfill. A compacted
		// session's first post-summary turn has no real user message
		// left in the tail — the judged turn lives before the
		// summary, so the full transcript locates it and this prompt
		// still supplies its verdict. The transcript comes from the
		// caller (fetched once for both judgment passes); it returns
		// the in-flight prompt too — it was persisted before msgs
		// was sliced — so it must be excluded by ID: landing on it
		// would mint a phantom row keyed to this turn's own message,
		// and the unique key would then block the real evidence row.
		if len(full) > 0 {
			if call.Prompt != "" {
				if idx := lastRealUserIdx(full, call.currentUserMsgID); idx >= 0 {
					o := extractTurnOutcome(ctx, d, call.SessionID, full, idx)
					o.NextVerdict = referentJudgedVerdict(call.Prompt)
					if err := d.cmdlog.RecordTurnOutcome(ctx, o); err != nil {
						slog.Warn("Failed to record turn outcome", "error", err)
					}
				}
			}
			// Same-session sweep on the full transcript —
			// compacted sessions drop pre-summary turns from
			// the tail, and queued prompts folded mid-run
			// (drainQueueForStep) close turns no per-run pass
			// ever judged.
			backfillSession(ctx, d, call.SessionID, full, call.currentUserMsgID)
			// The referent channel's same-session analog
			// (#368): vague turns judged by prompts that
			// never got a live pass — folded receipts,
			// pre-compaction boundaries — mint their
			// episodes here, judged by the next stored
			// prompt exactly as the live pass would.
			backfillReferentEpisodes(ctx, d, call.SessionID, full, call.currentUserMsgID)
		}
		// The corpus scan does up to backfillLimit message-list
		// reads — the caller already runs detached.
		backfillTurnOutcomes(ctx, d, call)
		return
	}
	if call.Prompt != "" {
		o := extractTurnOutcome(ctx, d, call.SessionID, msgs, lastUser)
		o.NextVerdict = referentJudgedVerdict(call.Prompt)
		if err := d.cmdlog.RecordTurnOutcome(ctx, o); err != nil {
			slog.Warn("Failed to record turn outcome", "error", err)
		}
	}
	// The live path records only the last boundary per run — a
	// prompt folded into an earlier step persists a real boundary
	// that closed a turn without a record pass, and prompt-less
	// runs skip the pass entirely. Sweep the tail's unrecorded
	// boundaries the same way the cross-session backfill does —
	// idempotent via HasTurnOutcome and the unique key — so the
	// row exists while the session is live instead of waiting for
	// another session's first turn to drain it.
	backfillSession(ctx, d, call.SessionID, msgs, call.currentUserMsgID)
	// Same sweep for referent episodes (#368) — a folded prompt
	// can close a vague turn without its judgment receipt ever
	// running the live mint.
	backfillReferentEpisodes(ctx, d, call.SessionID, msgs, call.currentUserMsgID)
}

// lastRealUserIdx returns the index of the last turn boundary in ms
// — repair prompts, the rewritten summary message, and the
// in-flight prompt itself (excludeID) don't count. The live tail
// predates this run's prompt so the exclusion is a no-op there; the
// full-transcript fallback reads storage after the prompt persists
// and needs it.
func lastRealUserIdx(ms []message.Message, excludeID string) int {
	for i := len(ms) - 1; i >= 0; i-- {
		if ms[i].ID == excludeID {
			continue
		}
		if isRealUserMsg(ms[i]) {
			return i
		}
	}
	return -1
}

// backfillTurnOutcomes scans recent sessions' stored messages and
// opens rows for turns that were never recorded — every session's
// final turn, plus every turn predating this channel. Bounded per
// call; already-present rows skip, so the corpus converges across
// sessions instead of one pass owning all of history.
func backfillTurnOutcomes(ctx context.Context, d turnOutcomeDeps, call SessionAgentCall) {
	if d.messages == nil {
		return
	}
	sessions, err := d.cmdlog.ListRecentSessionIDs(ctx, call.SessionID, turnOutcomeBackfillLimit)
	if err != nil || len(sessions) == 0 {
		return
	}
	for _, sess := range sessions {
		msgs, err := d.messages.List(ctx, sess.ID)
		if err != nil || len(msgs) == 0 {
			continue
		}
		backfillSession(ctx, d, sess.ID, msgs, call.currentUserMsgID)
	}
}

// backfillSession segments one stored session into turns and records
// each missing row. Turn identity is the boundary message's ID — an
// ordinal would shift between this full transcript and the live
// path's post-summary tail. Mid-session turns read their verdict off
// the following stored prompt — the same cue judgment the live path
// stamps; a final turn carries "". excludeID is the in-flight
// prompt's message ID: it still supplies the previous segment's
// verdict but is itself no closed turn, so it records no row.
func backfillSession(ctx context.Context, d turnOutcomeDeps, sessionID string, msgs []message.Message, excludeID string) {
	var segs []int
	for i, m := range msgs {
		if isRealUserMsg(m) {
			segs = append(segs, i)
		}
	}
	for i, start := range segs {
		if msgs[start].ID == excludeID {
			continue
		}
		has, err := d.cmdlog.HasTurnOutcome(ctx, sessionID, msgs[start].ID)
		if err != nil || has {
			continue
		}
		o := extractTurnOutcome(ctx, d, sessionID, msgs, start)
		if i+1 < len(segs) {
			// The verdict is the next stored user prompt's cue
			// judgment — identical to what the live path stamps.
			o.NextVerdict = referentJudgedVerdict(msgs[segs[i+1]].JoinedText())
		}
		if err := d.cmdlog.RecordTurnOutcome(ctx, o); err != nil {
			slog.Warn("Failed to backfill turn outcome", "error", err)
		}
	}
}

// extractTurnOutcome builds the evidence columns for the turn
// starting at msgs[userIdx]: the files it mutated with their
// added-line fingerprints, and the bash commands it ran in
// ledger-normal form. Turn identity is the boundary message's ID,
// so the tail and the full transcript agree on which turn a row
// names. Child sessions' mutations attribute to the parent turn
// through the same delegation walk the referent channel uses.
func extractTurnOutcome(ctx context.Context, d turnOutcomeDeps, sessionID string, msgs []message.Message, userIdx int) cmdlog.TurnOutcome {
	o := cmdlog.TurnOutcome{
		SessionID: sessionID,
		TurnID:    msgs[userIdx].ID,
		EditHunks: map[string][]string{},
		HunkAdded: map[string][]cmdlog.HunkEdit{},
		HunkSpans: map[string][]cmdlog.HunkSpan{},
	}
	if msgs[userIdx].CreatedAt > 0 {
		// Messages stamp seconds; the store's clocks are millis.
		o.CreatedAt = msgs[userIdx].CreatedAt * 1000
	}
	hunkLines := map[string]map[string]struct{}{}
	var scan func(ms []message.Message, from int)
	visited := map[string]struct{}{}
	// scan walks one message list for evidence. It never checks turn
	// boundaries: the caller pre-slices the parent's span, and inside
	// a child session a user-role message is delegation text, not a
	// prompt — boundary-checking child slices would return on the
	// delegation prompt itself and drop every delegated edit.
	scan = func(ms []message.Message, from int) {
		clean := cleanCallResults(ms[from:])
		for _, m := range ms[from:] {
			if m.Role != message.Assistant {
				continue
			}
			for _, tc := range m.ToolCalls() {
				res, executed := clean[tc.ID]
				switch {
				case tc.Name == AgentToolName && d.sessions != nil && d.messages != nil:
					childID := d.sessions.CreateAgentToolSessionID(m.ID, tc.ID)
					if _, ok := visited[childID]; ok {
						continue
					}
					visited[childID] = struct{}{}
					if childMsgs, err := d.messages.List(ctx, childID); err == nil {
						scan(childMsgs, 0)
					}
				case referentMutationTools[tc.Name] && executed:
					path, added, edits, spans := editEvidence(tc.Name, tc.Input, failedEditIndexes(res.Metadata), res.Metadata)
					if path == "" {
						continue
					}
					rel := relWorkdir(d.workdir, path)
					seen, ok := hunkLines[rel]
					if !ok {
						seen = map[string]struct{}{}
						hunkLines[rel] = seen
						o.EditedPaths = append(o.EditedPaths, rel)
					}
					// Same-file edits concatenate fingerprints —
					// deduped, or a repeated surviving line inflates
					// numerator and denominator together (and a
					// pathological edit loop grows the JSON).
					o.EditHunks[rel] = appendNewLines(o.EditHunks[rel], seen, added)
					// The verbatim edits append the same way
					// (#394) — the pair's agent-output half is
					// everything the turn wrote into the path,
					// kept per-edit so self-corrections split.
					o.HunkAdded[rel] = append(o.HunkAdded[rel], edits...)
					// And the execution-time spans the same —
					// anchors for the settle-time window slice.
					o.HunkSpans[rel] = append(o.HunkSpans[rel], spans...)
				case tc.Name == tools.BashToolName && executed:
					if cmd := bashCommand(tc.Input); cmd != "" {
						o.Commands = append(o.Commands, cmdlog.NormalizeOutcomeCommand(cmd))
					}
				}
			}
		}
	}
	// The turn's span ends at the next real user prompt — summary
	// and repair messages inside it stay part of the span (a
	// compaction artifact or a retry still belongs to this turn).
	end := len(msgs)
	for i := userIdx + 1; i < len(msgs); i++ {
		if isRealUserMsg(msgs[i]) {
			end = i
			break
		}
	}
	scan(msgs[userIdx+1:end], 0)
	return o
}

// cleanCallResults pairs tool calls with their results and keeps
// only the ones that landed clean: a denied permission's ToolResult
// is IsError, an errored call's is too, and a call with no result
// never ran — all three would mint evidence for work that never
// happened otherwise (#358). An error result is sticky: it wins over
// a success even when it arrives second. The map carries the result
// itself so callers can read metadata — a partial multiedit still
// reports which of its edits failed.
func cleanCallResults(ms []message.Message) map[string]message.ToolResult {
	failed := map[string]bool{}
	ok := map[string]message.ToolResult{}
	for _, m := range ms {
		for _, tr := range m.ToolResults() {
			if tr.ToolCallID == "" {
				continue
			}
			if tr.IsError {
				failed[tr.ToolCallID] = true
				delete(ok, tr.ToolCallID)
				continue
			}
			if !failed[tr.ToolCallID] {
				ok[tr.ToolCallID] = tr
			}
		}
	}
	return ok
}

// failedEditIndexes returns the edits[] positions a partial
// multiedit reported as failed in its result metadata — those
// edits' new_string lines never landed and must not mint
// fingerprint lines (#358).
func failedEditIndexes(metadata string) map[int]bool {
	if metadata == "" {
		return nil
	}
	var meta tools.MultiEditResponseMetadata
	if json.Unmarshal([]byte(metadata), &meta) != nil || len(meta.EditsFailed) == 0 {
		return nil
	}
	skip := map[int]bool{}
	for _, f := range meta.EditsFailed {
		// FailedEdit.Index is 1-based (applyEditsToContent emits
		// startIndex+i+1); edits[] is 0-based.
		skip[f.Index-1] = true
	}
	return skip
}

// editEvidence pulls the mutated path, the added-lines fingerprint,
// the raw written text, and the execution-time line spans out of a
// mutation tool call. The fingerprint keeps its introduced-lines
// rule: edit and multiedit count new_string minus old_string, so
// context lines carried over from the replaced text don't pad the
// survival numerator; write fingerprints its whole content, the old
// file being unknowable from the call. The fingerprint is per-file:
// lines added by any of the turn's edits to that file join one
// survival set. skip excludes edits[] positions a partial multiedit
// reported as failed.
//
// The edits return is the unreduced agent output for the hunk_added
// column (#394): per-edit {old, new} pairs verbatim — not
// new-minus-old — because the preference pair is (what the agent
// wrote, what the region became); re-emitted context lines are part
// of what it wrote, and old both preserves the pre-edit text and
// helps locate the region. A write contributes {new: content}.
//
// The spans return anchors the region at execution time: the tool
// result metadata carries the file's before/after content (edit,
// multiedit) or a unified diff (write), so the touched lines are
// diffed — never string-searched in a file the user may have
// already changed. Spans survive whitespace correction,
// replace_all, deletions, and edits the user made before the row
// recorded; a tool result without metadata yields no spans and the
// settle side marks the region unknown.
func editEvidence(tool, input string, skip map[int]bool, metadata string) (path string, added []string, edits []cmdlog.HunkEdit, spans []cmdlog.HunkSpan) {
	var in struct {
		FilePath  string `json:"file_path"`
		OldString string `json:"old_string"`
		NewString string `json:"new_string"`
		// A write detects by key presence, not emptiness — an
		// empty-content write truncates the file, which is a real
		// (if rare) mutation, not an absent field.
		Content *string `json:"content"`
		Edits   []struct {
			OldString string `json:"old_string"`
			NewString string `json:"new_string"`
		} `json:"edits"`
	}
	if json.Unmarshal([]byte(input), &in) != nil || in.FilePath == "" {
		return "", nil, nil, nil
	}
	var meta struct {
		OldContent string `json:"old_content"`
		NewContent string `json:"new_content"`
		Diff       string `json:"diff"`
	}
	if metadata != "" {
		_ = json.Unmarshal([]byte(metadata), &meta)
	}
	// The tools store new_content CRLF-restored against an
	// LF-normalized old_content — diff raw and every line differs
	// textually, degenerating spans to whole-file on CRLF files.
	// Normalize both sides into the same space first.
	oldContent, _ := fsext.ToUnixLineEndings(meta.OldContent)
	newContent, _ := fsext.ToUnixLineEndings(meta.NewContent)
	var unified string
	// Only the write tool owns the content key — a hallucinated
	// content field on an edit call must not mint a wrong-arm pair.
	write := in.Content != nil && tool == tools.WriteToolName
	switch {
	case write:
		// A write's metadata ships a ready-made unified diff;
		// the written content is the new file itself.
		unified = meta.Diff
		newContent, _ = fsext.ToUnixLineEndings(*in.Content)
	case meta.OldContent != "" || meta.NewContent != "":
		unified, _, _ = diff.GenerateDiff(oldContent, newContent, in.FilePath)
	}
	spans = hunkSpansFromUnified(unified, newContent)
	if len(spans) == 0 && write {
		// Write with no diff metadata — the file IS the region.
		spans = []cmdlog.HunkSpan{
			{Start: 0, End: len(strings.Split(newContent, "\n"))},
		}
	}
	switch {
	case write:
		return in.FilePath, cmdlog.NormalizeAddedLines(*in.Content),
			[]cmdlog.HunkEdit{{New: *in.Content}}, spans
	case len(in.Edits) > 0:
		for i, e := range in.Edits {
			if skip[i] {
				continue
			}
			added = append(added, introducedLines(e.NewString, e.OldString)...)
			edits = append(edits, cmdlog.HunkEdit{Old: e.OldString, New: e.NewString})
		}
		return in.FilePath, added, edits, spans
	default:
		return in.FilePath, introducedLines(in.NewString, in.OldString),
			[]cmdlog.HunkEdit{{Old: in.OldString, New: in.NewString}}, spans
	}
}

// unifiedHunkHeader matches a unified diff's hunk header — the
// new-side range "+ns,nc" is the span the hunk covers.
var unifiedHunkHeader = regexp.MustCompile(`^@@ -\d+(?:,\d+)? \+(\d+)(?:,(\d+))? @@`)

// hunkSpansFromUnified converts a unified diff into line spans in
// the new file — the changed lines inside each hunk, not the hunk's
// whole context range — each carrying the nearest non-blank
// boundary lines as re-anchors. An anchor is stored trimmed and
// scrubbed — settle compares in the same space — and a line that
// scrubs whole is skipped: "[REDACTED]" matches every secret line.
func hunkSpansFromUnified(unified, newContent string) []cmdlog.HunkSpan {
	if unified == "" {
		return nil
	}
	lines := strings.Split(newContent, "\n")
	var out []cmdlog.HunkSpan
	// pos tracks the hunk's new-side cursor (0-based); the span is
	// the run of new lines the hunk actually adds — context lines
	// around a small change must not stretch it.
	var pos, spanStart, spanEnd int
	spanStart, spanEnd = -1, -1
	flush := func() {
		if spanStart < 0 {
			return
		}
		start, end := spanStart, max(spanEnd, spanStart)
		out = append(out, cmdlog.HunkSpan{
			Start:  start,
			End:    min(end, len(lines)),
			Before: spanAnchor(lines, start-1, -1),
			After:  spanAnchor(lines, min(end, len(lines)), +1),
		})
		spanStart, spanEnd = -1, -1
	}
	// inHunk gates the body lines: only after an @@ header is a "+"
	// an added line — before it, "---"/"+++" name the diff's files,
	// and inside it "++foo" really is added content.
	inHunk := false
	for _, l := range strings.Split(unified, "\n") {
		if m := unifiedHunkHeader.FindStringSubmatch(l); m != nil {
			flush()
			ns, _ := strconv.Atoi(m[1])
			nc := 1
			if m[2] != "" {
				nc, _ = strconv.Atoi(m[2])
			}
			// "+N,0" is a pure deletion — its position is after
			// line N in the new file, not at N-1.
			pos = max(ns-1, 0)
			if nc == 0 {
				pos = ns
			}
			inHunk = true
			continue
		}
		if !inHunk || l == "" {
			continue
		}
		switch l[0] {
		case '+':
			if spanStart < 0 {
				spanStart = pos
			}
			pos++
			spanEnd = pos
		case '-':
			// A removed line advances nothing in the new file —
			// a deletion-only hunk records an empty span at pos.
			if spanStart < 0 {
				spanStart = pos
			}
		case ' ':
			pos++
		}
		// "\" lines ("\ No newline at end of file") carry no
		// position; anything else is ignored.
	}
	flush()
	return out
}

// spanAnchor picks the nearest non-blank line in direction dir from
// position pos (a few lines at most), stored trimmed, scrubbed, and
// length-bounded. A line that scrubs whole is skipped — a
// "[REDACTED]" anchor would re-anchor to any secret line. Within
// the window a line unique in the file is preferred over a generic
// repeated one ("}", "return") — a duplicated anchor can silently
// re-attach to the wrong occurrence at settle.
func spanAnchor(lines []string, pos, dir int) string {
	var fallback string
	for i := pos; i >= 0 && i < len(lines) && abs(i-pos) < 8; i += dir {
		cand := strings.TrimSpace(lines[i])
		if cand == "" {
			continue
		}
		a := redact.Secrets(cand)
		if a == "[REDACTED]" {
			continue
		}
		if r := []rune(a); len(r) > cmdlog.HunkAnchorRunes {
			a = string(r[:cmdlog.HunkAnchorRunes])
		}
		if fallback == "" {
			fallback = a
		}
		switch {
		case a == cand && anchorUnique(lines, cand):
			// A clean line unique in the file — the settle match
			// names one position unambiguously.
			return a
		case a != cand && len([]rune(a)) == cmdlog.HunkAnchorRunes &&
			anchorPrefixUnique(lines, a):
			// A capped anchor re-matches by prefix — uniqueness
			// must hold for the prefix, not the full raw line.
			return a
			// A partially scrubbed line ("a != cand" under the cap)
			// stores redacted text whose collisions can't be
			// counted on raw lines — fallback only.
		}
	}
	return fallback
}

// anchorUnique reports whether the trimmed line occurs exactly once
// in the file — the property that lets a settle-time anchor match
// name one position unambiguously.
func anchorUnique(lines []string, cand string) bool {
	n := 0
	for _, l := range lines {
		if strings.TrimSpace(l) == cand {
			if n++; n > 1 {
				return false
			}
		}
	}
	return n == 1
}

// anchorPrefixUnique is anchorUnique for a capped anchor, which
// re-matches by prefix rather than equality.
func anchorPrefixUnique(lines []string, prefix string) bool {
	n := 0
	for _, l := range lines {
		if strings.HasPrefix(strings.TrimSpace(l), prefix) {
			if n++; n > 1 {
				return false
			}
		}
	}
	return n == 1
}

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}

// introducedLines reduces one edit's new_text to the lines it
// introduced — new minus old in normalized space. A line the edit
// moved or kept is not evidence it added anything; only genuinely
// new lines should count in the survival check's numerator.
func introducedLines(newText, oldText string) []string {
	removed := map[string]struct{}{}
	for _, l := range cmdlog.NormalizeAddedLines(oldText) {
		removed[l] = struct{}{}
	}
	var out []string
	for _, l := range cmdlog.NormalizeAddedLines(newText) {
		if _, ok := removed[l]; ok {
			continue
		}
		out = append(out, l)
	}
	return out
}

// appendNewLines appends only lines not already in the per-path seen
// set — same-file edits concatenate fingerprints, and duplicated
// surviving lines would inflate numerator and denominator together.
func appendNewLines(dst []string, seen map[string]struct{}, added []string) []string {
	for _, l := range added {
		if _, ok := seen[l]; ok {
			continue
		}
		seen[l] = struct{}{}
		dst = append(dst, l)
	}
	return dst
}

// bashCommand pulls the command string out of a bash tool call's
// input — the run command, before ledger normalization.
func bashCommand(input string) string {
	var in struct {
		Command string `json:"command"`
	}
	if json.Unmarshal([]byte(input), &in) != nil {
		return ""
	}
	return in.Command
}
