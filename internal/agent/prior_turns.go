package agent

import (
	"cmp"
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/charmbracelet/crush/internal/agent/tools"
	"github.com/charmbracelet/crush/internal/csync"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/notebook"
)

// Prior-turn collapse (options.notebook_prior_turns = "stub",
// "digest", or "summarize") renders tool call/result pairs in
// completed, fully covered turns as minimal stub pairs: structure —
// IDs, roles, pairing — is preserved and only the content is
// replaced. Collapse is a pure render-time transform; stored events
// are never rewritten, so recall("result:<id>") still resolves the
// original. Under "digest" each finished turn additionally
// consolidates into a granularity:turn notebook entry at run end —
// see notebook_digest.go and docs/design/TURN_DIGEST.md. Under
// "summarize" the turn's executable span is instead replaced by one
// synthetic message carrying the turn's generated segment entries —
// content, not pointers — so no recall tool is required.

const (
	// priorTurnsVerbatim renders the full transcript — the default.
	priorTurnsVerbatim = "verbatim"
	// priorTurnsStub collapses covered prior turns to stub pairs.
	priorTurnsStub = "stub"
	// priorTurnsDigest collapses like stub and additionally
	// consolidates each finished turn into a granularity:turn
	// notebook entry — the turn digest.
	priorTurnsDigest = "digest"
	// priorTurnsSummarize replaces a covered turn's executable span
	// with its segment entries rendered inline.
	priorTurnsSummarize = "summarize"
)

// turnCollapse carries prior-turn collapse state across one run's
// renders. Before is the turn that started the run — NOT the current
// turn: drainQueueForStep can fold a queued prompt mid-run, advancing
// the positional turn count under the active run, so comparing
// against a recomputed current turn would collapse the run's own
// earlier events. Set is the resolved collapsible-turn set, computed
// once on the run's first render and then frozen — a mid-run coverage
// commit must not flip a turn from raw to stub mid-window (the same
// prefix-stability rule stub promotion follows). digestTurns is the
// set of turns holding a granularity:turn digest, frozen lazily on
// the run's first prefix render — eligibility, not application: a
// digest landing mid-run must not demote already-rendered entries.
// summaries is the summarize mode's render payload — turn → joined
// entry text — frozen with Set on the run's first render so a mid-run
// entry commit cannot flip a turn from summary to raw mid-window.
type turnCollapse struct {
	Before      int64
	Set         map[int64]bool
	digestTurns map[int64]bool
	summaries   map[int64]string
}

// newTurnCollapse returns the collapse state for one render pipeline,
// or nil when collapse is off — verbatim mode, or no notebook to back
// the stubs' recall pointer. The coordinator already coerces the
// option to verbatim when the notebook is disabled; this is the belt.
func (a *sessionAgent) newTurnCollapse(before int64) *turnCollapse {
	switch a.priorTurns {
	case priorTurnsStub, priorTurnsDigest, priorTurnsSummarize:
	default:
		return nil
	}
	if !a.notebookEnabled || a.notebook == nil {
		return nil
	}
	return &turnCollapse{Before: before}
}

// coveredPriorTurns resolves the collapsible-turn set: turns strictly
// below before whose every segment has committed coverage. An
// uncovered turn stays raw — collapsing it would leave stubs with no
// notebook entries behind them. The open tail counts when a processed
// row with a matching extent exists (detectSegments marks it), which
// is what lets the just-finished turn collapse at the next run's
// start.
func coveredPriorTurns(segs []segment, processed map[segmentKey]bool, before int64) map[int64]bool {
	fully := make(map[int64]bool)
	for _, s := range segs {
		if s.turn >= before {
			continue
		}
		if _, seen := fully[s.turn]; !seen {
			fully[s.turn] = true
		}
		if !processed[s.key()] {
			fully[s.turn] = false
		}
	}
	out := make(map[int64]bool, len(fully))
	for turn, ok := range fully {
		if ok {
			out[turn] = true
		}
	}
	return out
}

// collapsedCallInput is the replacement tool-call input for a
// collapsed turn. It must stay a valid JSON object — providers
// (Anthropic tool_use.input, Gemini args) reject prose — so the label
// rides inside the payload rather than replacing it. For file-write
// calls the payload was the input itself — recall("result:<id>")
// cannot recover it — so the stub names the real recovery path:
// re-viewing the file. Other calls (reads, and bash mutations whose
// output remains recallable) keep the generic marker, and digest
// mode's names the consolidation that stands in for the turn.
func collapsedCallInput(mode string, turn int64, write bool) string {
	if write {
		return fmt.Sprintf(`{"_collapsed":"prior turn %d — write args dropped; re-view the file to reconstruct"}`, turn)
	}
	if mode == priorTurnsDigest {
		return fmt.Sprintf(`{"_collapsed":"prior turn %d — consolidated in turn digest"}`, turn)
	}
	return fmt.Sprintf(`{"_collapsed":"prior turn %d"}`, turn)
}

// collapsedResultText is the constant stub text a collapsed tool
// result renders. It names the turn and the recovery path; digest
// mode's names the turn digest while keeping the same recall pointer
// — the digest may lag async or never land on generator failure, so
// the pointer never depends on it. The pointer stays honest about its
// bounds: recall returns the stored result capped at
// MaxOutputLength. For file-write calls the result is only the write
// confirmation — the args lived in the input — so the stub leads with
// re-view and qualifies what recall holds.
func collapsedResultText(mode string, turn int64, toolCallID string, write bool) string {
	if write {
		return fmt.Sprintf("[prior turn %d — collapsed; write args dropped — re-view the file to reconstruct; recall(\"result:%s\") holds only the confirmation]", turn, toolCallID)
	}
	if mode == priorTurnsDigest {
		return fmt.Sprintf("[prior turn %d — consolidated in turn digest; recall(\"result:%s\") recovers the stored result, capped]", turn, toolCallID)
	}
	return fmt.Sprintf("[prior turn %d — collapsed; recall(\"result:%s\") recovers the stored result, capped]", turn, toolCallID)
}

// collapseAssistantForTurn returns m with prior-turn detail removed:
// reasoning parts drop with the turn and tool-call inputs are replaced
// by collapsedCallInput under the given mode. question and
// provider-executed calls are
// exempt — the former carry user decisions rather than execution
// detail, and the latter carry typed inputs providers may
// schema-validate on replay (a stubbed server-tool input can 400 the
// whole request). Reasoning bound via ToolID to an exempt call is kept:
// replaying a signed function call without its thought signature is a
// provider rejection mode, so the signature survives with the call it
// signs. Reasoning bound to a collapsed call drops — the signature is
// invalid against the mutated input either way. IDs, names, and finish
// state are preserved. Returns m unchanged when nothing needs
// collapsing, plus the number of collapsed calls — the per-turn event
// count the collapse telemetry records.
func collapseAssistantForTurn(m message.Message, turn int64, mode string) (message.Message, int) {
	exempt := make(map[string]bool)
	for _, part := range m.Parts {
		if tc, ok := part.(message.ToolCall); ok && callIsExempt(tc) {
			exempt[tc.ID] = true
		}
	}
	needs := false
	for _, part := range m.Parts {
		switch p := part.(type) {
		case message.ReasoningContent:
			needs = needs || !exempt[p.ToolID]
		case message.ToolCall:
			needs = needs || !exempt[p.ID]
		}
		if needs {
			break
		}
	}
	if !needs {
		return m, 0
	}
	parts := make([]message.ContentPart, 0, len(m.Parts))
	collapsed := 0
	for _, part := range m.Parts {
		switch p := part.(type) {
		case message.ReasoningContent:
			if exempt[p.ToolID] {
				parts = append(parts, part)
			}
			continue
		case message.ToolCall:
			if !exempt[p.ID] {
				p.Input = collapsedCallInput(mode, turn, tools.WriteToolNames[p.Name])
				part = p
				collapsed++
			}
		}
		parts = append(parts, part)
	}
	out := m
	out.Parts = parts
	return out, collapsed
}

// callIsExempt reports whether a tool call stays verbatim inside a
// collapsed turn — question pairs and provider-executed calls.
func callIsExempt(tc message.ToolCall) bool {
	return tc.Name == tools.QuestionToolName || tc.ProviderExecuted
}

// collapseToolMessageForTurn returns m with each tool result's stored
// content replaced by collapsedResultText under the given mode —
// media payloads included (Data cleared so the text stub is what
// emits). Results answering an exempt call stay verbatim with it;
// results answering a file-write call get the re-view-led stub.
// Returns the message and how many results were collapsed.
func collapseToolMessageForTurn(m message.Message, turn int64, mode string, exemptCalls map[string]bool, callNames map[string]string) (message.Message, int) {
	var parts []message.ContentPart
	count := 0
	for i, part := range m.Parts {
		tr, ok := part.(message.ToolResult)
		if !ok || exemptCalls[tr.ToolCallID] {
			if parts != nil {
				parts = append(parts, part)
			}
			continue
		}
		tr.Content = collapsedResultText(mode, turn, tr.ToolCallID, tools.WriteToolNames[callNames[tr.ToolCallID]])
		tr.Data = ""
		tr.MIMEType = ""
		// The stub is informational, not the failure it replaces —
		// rendering is_error=true on collapsed text is structurally
		// valid but semantically wrong.
		tr.IsError = false
		if parts == nil {
			parts = make([]message.ContentPart, 0, len(m.Parts))
			parts = append(parts, m.Parts[:i]...)
		}
		parts = append(parts, tr)
		count++
	}
	if parts == nil {
		return m, 0
	}
	out := m
	out.Parts = parts
	return out, count
}

// turnSummaries renders each covered turn's segment entries into the
// text summarize mode substitutes for the turn's executable span.
// Entries join per turn ordered by (segment, event); checkpoint and
// digest rows share notebook_entries under EventCheckpoint and are
// excluded, as are hydration seeds. The uncompressed EntryTextFull is
// preferred — the mode's premise is information sufficiency. A
// covered turn with no entries is absent from the result: it renders
// verbatim. The fetch runs once per run at collapse-set freeze, so a
// mid-run entry commit cannot flip a rendered turn.
func (a *sessionAgent) turnSummaries(ctx context.Context, sessionID string, covered map[int64]bool) map[int64]string {
	if sessionID == "" || len(covered) == 0 {
		return nil
	}
	detCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	entries, err := a.notebook.GetEntries(detCtx, sessionID)
	cancel()
	if err != nil {
		slog.Warn("Failed to load entries for turn summaries", "session_id", sessionID, "error", err)
		return nil
	}
	byTurn := make(map[int64][]notebook.Entry)
	for _, e := range entries {
		if !covered[e.TurnNumber] || e.EventType == notebook.EventCheckpoint || isHydratedSeed(e) {
			continue
		}
		byTurn[e.TurnNumber] = append(byTurn[e.TurnNumber], e)
	}
	summaries := make(map[int64]string, len(byTurn))
	for turn, list := range byTurn {
		slices.SortFunc(list, func(x, y notebook.Entry) int {
			if x.SegmentNumber != y.SegmentNumber {
				return cmp.Compare(x.SegmentNumber, y.SegmentNumber)
			}
			return cmp.Compare(x.EventNumber, y.EventNumber)
		})
		var sb strings.Builder
		fmt.Fprintf(&sb, "[Summary of turn %d]\n", turn)
		for _, e := range list {
			sb.WriteString(cmp.Or(e.EntryTextFull, e.EntryText))
			sb.WriteString("\n")
		}
		summaries[turn] = strings.TrimRight(sb.String(), "\n")
	}
	return summaries
}

// summarizeAssistantForTurn returns m with the turn's executable
// detail dropped whole — reasoning and every non-question tool call —
// the synthetic turn summary stands in their place. Question pairs
// survive verbatim: their results carry user decisions, the one
// payload a summary cannot safely compress, and a signed question
// call keeps its bound reasoning — replaying a signed call without
// its thought signature is a provider rejection mode. Unlike stub
// collapse nothing is mutated, so no provider replays a falsified
// call. Returns the message and the number of dropped calls.
func summarizeAssistantForTurn(m message.Message) (message.Message, int) {
	keep := make(map[string]bool)
	for _, part := range m.Parts {
		if tc, ok := part.(message.ToolCall); ok && tc.Name == tools.QuestionToolName {
			keep[tc.ID] = true
		}
	}
	needs := false
	for _, part := range m.Parts {
		switch p := part.(type) {
		case message.ReasoningContent:
			needs = needs || !keep[p.ToolID]
		case message.ToolCall:
			needs = needs || !keep[p.ID]
		}
		if needs {
			break
		}
	}
	if !needs {
		return m, 0
	}
	parts := make([]message.ContentPart, 0, len(m.Parts))
	dropped := 0
	for _, part := range m.Parts {
		switch p := part.(type) {
		case message.ReasoningContent:
			if keep[p.ToolID] {
				parts = append(parts, part)
			}
			continue
		case message.ToolCall:
			if !keep[p.ID] {
				dropped++
				continue
			}
		}
		parts = append(parts, part)
	}
	out := m
	out.Parts = parts
	return out, dropped
}

// summarizeToolMessageForTurn keeps only the results answering the
// turn's surviving question calls — every other result drops with its
// call, which leaves the prompt entirely rather than rendering a
// stub. Returns the message and the dropped-result count.
func summarizeToolMessageForTurn(m message.Message, callNames map[string]string) (message.Message, int) {
	var parts []message.ContentPart
	dropped := 0
	for i, part := range m.Parts {
		tr, ok := part.(message.ToolResult)
		if !ok || callNames[tr.ToolCallID] == tools.QuestionToolName {
			if parts != nil {
				parts = append(parts, part)
			}
			continue
		}
		dropped++
		if parts == nil {
			parts = make([]message.ContentPart, 0, len(m.Parts))
			parts = append(parts, m.Parts[:i]...)
		}
	}
	if parts == nil {
		return m, 0
	}
	out := m
	out.Parts = parts
	return out, dropped
}

// recordCollapsedTurns persists newly collapsed turns and accumulates
// the session's collapse telemetry. byTurn maps each collapsed turn to
// its collapsed call/result pair count. A turn is counted once: the
// collapsed_turns row's existence dedupes across renders and across
// the processes a resumed session passes through, and the in-memory
// recorded set skips the write entirely once this process has logged
// the turn. Counter bumps follow the DB row — a turn another process
// already recorded doesn't double-count.
func (a *sessionAgent) recordCollapsedTurns(ctx context.Context, sessionID string, byTurn map[int64]int) {
	if sessionID == "" || a.notebook == nil {
		return
	}
	var recorded *csync.Map[int64, bool]
	if a.collapseRecorded != nil {
		recorded, _ = a.collapseRecorded.Get(sessionID)
		if recorded == nil {
			recorded = csync.NewMap[int64, bool]()
			a.collapseRecorded.Set(sessionID, recorded)
		}
	}
	for turn, events := range byTurn {
		if recorded != nil {
			if _, ok := recorded.Get(turn); ok {
				continue
			}
		}
		inserted, err := a.notebook.RecordCollapsedTurn(ctx, sessionID, turn, events)
		if err != nil {
			slog.Warn("Failed to record collapsed turn", "session_id", sessionID, "turn", turn, "error", err)
			continue
		}
		if recorded != nil {
			recorded.Set(turn, true)
		}
		if inserted && a.stubStats != nil {
			stats, _ := a.stubStats.Get(sessionID)
			stats.TurnsCollapsed++
			stats.EventsCollapsed += events
			a.stubStats.Set(sessionID, stats)
		}
	}
}
