package agent

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/charmbracelet/crush/internal/agent/tools"
	"github.com/charmbracelet/crush/internal/message"
)

// Prior-turn collapse (options.notebook_prior_turns = "stub") renders
// tool call/result pairs in completed, fully covered turns as minimal
// stub pairs: structure — IDs, roles, pairing — is preserved and only
// the content is replaced. Collapse is a pure render-time transform;
// stored events are never rewritten, so recall("result:<id>") still
// resolves the original.

const (
	// priorTurnsVerbatim renders the full transcript — the default.
	priorTurnsVerbatim = "verbatim"
	// priorTurnsStub collapses covered prior turns to stub pairs. The
	// "digest" option value resolves here until turn-digest
	// generation ships — the collapse is identical and the stub text
	// never claims a digest exists.
	priorTurnsStub = "stub"
)

// turnCollapse carries prior-turn collapse state across one run's
// renders. Before is the turn that started the run — NOT the current
// turn: drainQueueForStep can fold a queued prompt mid-run, advancing
// the positional turn count under the active run, so comparing
// against a recomputed current turn would collapse the run's own
// earlier events. Set is the resolved collapsible-turn set, computed
// once on the run's first render and then frozen — a mid-run coverage
// commit must not flip a turn from raw to stub mid-window (the same
// prefix-stability rule stub promotion follows).
type turnCollapse struct {
	Before int64
	Set    map[int64]bool
}

// newTurnCollapse returns the collapse state for one render pipeline,
// or nil when collapse is off — verbatim mode, or no notebook to back
// the stubs' recall pointer. The coordinator already coerces the
// option to verbatim when the notebook is disabled; this is the belt.
func (a *sessionAgent) newTurnCollapse(before int64) *turnCollapse {
	// "digest" resolves to stub until turn-digest generation ships —
	// the coordinator already normalizes via NotebookPriorTurnsMode,
	// but a directly-set field gets the same belt.
	switch a.priorTurns {
	case priorTurnsStub, "digest":
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
// output remains recallable) keep the generic marker.
func collapsedCallInput(turn int64, write bool) string {
	if write {
		return fmt.Sprintf(`{"_collapsed":"prior turn %d — write args dropped; re-view the file to reconstruct"}`, turn)
	}
	return fmt.Sprintf(`{"_collapsed":"prior turn %d"}`, turn)
}

// collapsedResultText is the constant stub text a collapsed tool
// result renders. It names the turn and the recovery path and — per
// the stub contract — never claims a digest exists. The pointer stays
// honest about its bounds: recall returns the stored result capped at
// MaxOutputLength, and for file-write calls it holds nothing of the
// args — those stubs point at re-view instead.
func collapsedResultText(turn int64, toolCallID string, write bool) string {
	if write {
		return fmt.Sprintf("[prior turn %d — collapsed; write args dropped, re-view the file to reconstruct]", turn)
	}
	return fmt.Sprintf("[prior turn %d — collapsed; recall(\"result:%s\") to recover]", turn, toolCallID)
}

// collapseAssistantForTurn returns m with prior-turn detail removed:
// reasoning parts drop with the turn and tool-call inputs are replaced
// by collapsedCallInput. question and provider-executed calls are
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
func collapseAssistantForTurn(m message.Message, turn int64) (message.Message, int) {
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
				p.Input = collapsedCallInput(turn, tools.WriteToolNames[p.Name])
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
// content replaced by collapsedResultText — media payloads included
// (Data cleared so the text stub is what emits). Results answering an
// exempt call stay verbatim with it; results answering a file-write
// call get the re-view stub instead of the recall pointer. Returns the
// message and how many results were collapsed.
func collapseToolMessageForTurn(m message.Message, turn int64, exemptCalls, writeCalls map[string]bool) (message.Message, int) {
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
		tr.Content = collapsedResultText(turn, tr.ToolCallID, writeCalls[tr.ToolCallID])
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
	var recorded map[int64]bool
	if a.collapseRecorded != nil {
		recorded, _ = a.collapseRecorded.Get(sessionID)
	}
	for turn, events := range byTurn {
		if recorded[turn] {
			continue
		}
		inserted, err := a.notebook.RecordCollapsedTurn(ctx, sessionID, turn, events)
		if err != nil {
			slog.Warn("Failed to record collapsed turn", "session_id", sessionID, "turn", turn, "error", err)
			continue
		}
		if recorded == nil {
			recorded = make(map[int64]bool)
		}
		recorded[turn] = true
		if inserted && a.stubStats != nil {
			stats, _ := a.stubStats.Get(sessionID)
			stats.TurnsCollapsed++
			stats.EventsCollapsed += events
			a.stubStats.Set(sessionID, stats)
		}
	}
	if recorded != nil && a.collapseRecorded != nil {
		a.collapseRecorded.Set(sessionID, recorded)
	}
}
