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
	"sync"

	"github.com/charmbracelet/crush/internal/agent/tools"
	"github.com/charmbracelet/crush/internal/cmdlog"
	"github.com/charmbracelet/crush/internal/csync"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/params"
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
					path, added, edits := editEvidence(tc.Input, failedEditIndexes(res.Metadata))
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
		skip[f.Index] = true
	}
	return skip
}

// editEvidence pulls the mutated path, the added-lines fingerprint,
// and the raw written text out of a mutation tool call's input. The
// fingerprint keeps its introduced-lines rule: edit and multiedit
// count new_string minus old_string, so context lines carried over
// from the replaced text don't pad the survival numerator; write
// fingerprints its whole content, the old file being unknowable
// from the call. The fingerprint is per-file: lines added by any of
// the turn's edits to that file join one survival set. skip
// excludes edits[] positions a partial multiedit reported as
// failed.
//
// The edits return is the unreduced agent output for the hunk_added
// column (#394): per-edit {old, new} pairs verbatim — not
// new-minus-old — because the preference pair is (what the agent
// wrote, what the region became); re-emitted context lines are part
// of what it wrote, and old both preserves the pre-edit text and
// helps locate the region. A write contributes {new: content}.
func editEvidence(input string, skip map[int]bool) (path string, added []string, edits []cmdlog.HunkEdit) {
	var in struct {
		FilePath  string `json:"file_path"`
		OldString string `json:"old_string"`
		NewString string `json:"new_string"`
		Content   string `json:"content"`
		Edits     []struct {
			OldString string `json:"old_string"`
			NewString string `json:"new_string"`
		} `json:"edits"`
	}
	if json.Unmarshal([]byte(input), &in) != nil || in.FilePath == "" {
		return "", nil, nil
	}
	switch {
	case in.Content != "":
		return in.FilePath, cmdlog.NormalizeAddedLines(in.Content),
			[]cmdlog.HunkEdit{{New: in.Content}}
	case len(in.Edits) > 0:
		for i, e := range in.Edits {
			if skip[i] {
				continue
			}
			added = append(added, introducedLines(e.NewString, e.OldString)...)
			edits = append(edits, cmdlog.HunkEdit{Old: e.OldString, New: e.NewString})
		}
		return in.FilePath, added, edits
	default:
		return in.FilePath, introducedLines(in.NewString, in.OldString),
			[]cmdlog.HunkEdit{{Old: in.OldString, New: in.NewString}}
	}
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
