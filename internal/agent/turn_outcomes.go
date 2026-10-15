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

	"github.com/charmbracelet/crush/internal/agent/tools"
	"github.com/charmbracelet/crush/internal/cmdlog"
	"github.com/charmbracelet/crush/internal/message"
)

// turnOutcomeBackfillLimit bounds how many past sessions a session's
// first turn scans for missing outcome rows — the corpus is additive
// across sessions, so a small bound still converges: every new
// session's first turn drains another slice of history.
const turnOutcomeBackfillLimit = 25

// recordTurnOutcome opens the outcome row for the judged turn — the
// last real user message before this run's prompt. Repair retries
// carry harness text, not a verdict, so the row waits for the next
// real prompt. A session with no prior real user message is in its
// first turn — the backfill trigger.
func (a *sessionAgent) recordTurnOutcome(ctx context.Context, call SessionAgentCall, msgs []message.Message) {
	if a.cmdlog == nil || a.isSubAgent || call.RepairAttempts > 0 || call.Prompt == "" {
		return
	}
	lastUser := lastRealUserIdx(msgs, call.currentUserMsgID)
	if lastUser < 0 {
		// Two distinct shapes land here. A genuinely new session's
		// first turn triggers the corpus backfill. A compacted
		// session's first post-summary turn has no real user message
		// left in the tail — the judged turn lives before the
		// summary, so the full transcript locates it and this prompt
		// still supplies its verdict. The fresh List returns the
		// in-flight prompt too — it was persisted before msgs was
		// sliced — so it must be excluded by ID: landing on it would
		// mint a phantom row keyed to this turn's own message, and
		// the unique key would then block the real evidence row.
		if a.messages != nil {
			if full, err := a.messages.List(ctx, call.SessionID); err == nil {
				if idx := lastRealUserIdx(full, call.currentUserMsgID); idx >= 0 {
					o := a.extractTurnOutcome(ctx, call.SessionID, full, idx)
					o.NextVerdict = referentJudgedVerdict(call.Prompt)
					if err := a.cmdlog.RecordTurnOutcome(ctx, o); err != nil {
						slog.Warn("Failed to record turn outcome", "error", err)
					}
				}
			}
		}
		// The corpus scan does up to backfillLimit message-list
		// reads — detached, off the prompt path.
		backfillCtx := context.WithoutCancel(ctx)
		a.spawnDetached(func() {
			a.backfillTurnOutcomes(backfillCtx, call)
		})
		return
	}
	o := a.extractTurnOutcome(ctx, call.SessionID, msgs, lastUser)
	o.NextVerdict = referentJudgedVerdict(call.Prompt)
	if err := a.cmdlog.RecordTurnOutcome(ctx, o); err != nil {
		slog.Warn("Failed to record turn outcome", "error", err)
	}
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
func (a *sessionAgent) backfillTurnOutcomes(ctx context.Context, call SessionAgentCall) {
	if a.messages == nil {
		return
	}
	sessions, err := a.cmdlog.ListRecentSessionIDs(ctx, call.SessionID, turnOutcomeBackfillLimit)
	if err != nil || len(sessions) == 0 {
		return
	}
	for _, sess := range sessions {
		msgs, err := a.messages.List(ctx, sess.ID)
		if err != nil || len(msgs) == 0 {
			continue
		}
		a.backfillSession(ctx, sess.ID, msgs)
	}
}

// backfillSession segments one stored session into turns and records
// each missing row. Turn identity is the boundary message's ID — an
// ordinal would shift between this full transcript and the live
// path's post-summary tail. Mid-session turns read their verdict off
// the following stored prompt — the same cue judgment the live path
// stamps; a final turn carries "".
func (a *sessionAgent) backfillSession(ctx context.Context, sessionID string, msgs []message.Message) {
	var segs []int
	for i, m := range msgs {
		if isRealUserMsg(m) {
			segs = append(segs, i)
		}
	}
	for i, start := range segs {
		has, err := a.cmdlog.HasTurnOutcome(ctx, sessionID, msgs[start].ID)
		if err != nil || has {
			continue
		}
		o := a.extractTurnOutcome(ctx, sessionID, msgs, start)
		if i+1 < len(segs) {
			// The verdict is the next stored user prompt's cue
			// judgment — identical to what the live path stamps.
			o.NextVerdict = referentJudgedVerdict(msgs[segs[i+1]].JoinedText())
		}
		if err := a.cmdlog.RecordTurnOutcome(ctx, o); err != nil {
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
func (a *sessionAgent) extractTurnOutcome(ctx context.Context, sessionID string, msgs []message.Message, userIdx int) cmdlog.TurnOutcome {
	o := cmdlog.TurnOutcome{
		SessionID: sessionID,
		TurnID:    msgs[userIdx].ID,
		EditHunks: map[string][]string{},
	}
	if msgs[userIdx].CreatedAt > 0 {
		// Messages stamp seconds; the store's clocks are millis.
		o.CreatedAt = msgs[userIdx].CreatedAt * 1000
	}
	pathSet := map[string]struct{}{}
	var scan func(ms []message.Message, from int)
	visited := map[string]struct{}{}
	// scan walks one message list for evidence. It never checks turn
	// boundaries: the caller pre-slices the parent's span, and inside
	// a child session a user-role message is delegation text, not a
	// prompt — boundary-checking child slices would return on the
	// delegation prompt itself and drop every delegated edit.
	scan = func(ms []message.Message, from int) {
		for _, m := range ms[from:] {
			if m.Role != message.Assistant {
				continue
			}
			for _, tc := range m.ToolCalls() {
				switch {
				case tc.Name == AgentToolName && a.sessions != nil && a.messages != nil:
					childID := a.sessions.CreateAgentToolSessionID(m.ID, tc.ID)
					if _, ok := visited[childID]; ok {
						continue
					}
					visited[childID] = struct{}{}
					if childMsgs, err := a.messages.List(ctx, childID); err == nil {
						scan(childMsgs, 0)
					}
				case referentMutationTools[tc.Name]:
					path, added := editEvidence(tc.Input)
					if path == "" {
						continue
					}
					rel := a.relWorkdir(path)
					o.EditHunks[rel] = append(o.EditHunks[rel], added...)
					if _, ok := pathSet[rel]; !ok {
						pathSet[rel] = struct{}{}
						o.EditedPaths = append(o.EditedPaths, rel)
					}
				case tc.Name == tools.BashToolName:
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

// editEvidence pulls the mutated path and the added-lines
// fingerprint out of a mutation tool call's input — edit's
// new_string, multiedit's concatenated new_strings, write's content.
// The fingerprint is per-file: lines added by any of the turn's
// edits to that file join one survival set.
func editEvidence(input string) (path string, added []string) {
	var in struct {
		FilePath  string `json:"file_path"`
		NewString string `json:"new_string"`
		Content   string `json:"content"`
		Edits     []struct {
			NewString string `json:"new_string"`
		} `json:"edits"`
	}
	if json.Unmarshal([]byte(input), &in) != nil || in.FilePath == "" {
		return "", nil
	}
	var text string
	switch {
	case in.Content != "":
		text = in.Content
	case len(in.Edits) > 0:
		for _, e := range in.Edits {
			text += e.NewString + "\n"
		}
	default:
		text = in.NewString
	}
	return in.FilePath, cmdlog.NormalizeAddedLines(text)
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
