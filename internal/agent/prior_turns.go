package agent

import (
	"fmt"

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
	if a.priorTurns != priorTurnsStub || !a.notebookEnabled || a.notebook == nil {
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
// rides inside the payload rather than replacing it.
func collapsedCallInput(turn int64) string {
	return fmt.Sprintf(`{"_collapsed":"prior turn %d"}`, turn)
}

// collapsedResultText is the constant stub text a collapsed tool
// result renders. It names the turn and the recovery path and — per
// the stub contract — never claims a digest exists.
func collapsedResultText(turn int64, toolCallID string) string {
	return fmt.Sprintf("[prior turn %d — collapsed; recall(\"result:%s\") to recover]", turn, toolCallID)
}

// collapseAssistantForTurn returns m with prior-turn detail removed:
// reasoning parts drop with the turn (signature requirements are
// in-flight-turn-scoped — keeping a signature while mutating the
// tool_use.input it bound is the fragile combo, so both go together)
// and tool-call inputs are replaced by collapsedCallInput. question
// calls are exempt — they carry user decisions, not execution detail.
// IDs, names, and finish state are preserved. Returns m unchanged
// when nothing needs collapsing.
func collapseAssistantForTurn(m message.Message, turn int64) message.Message {
	needs := false
	for _, part := range m.Parts {
		switch p := part.(type) {
		case message.ReasoningContent:
			needs = true
		case message.ToolCall:
			needs = needs || p.Name != tools.QuestionToolName
		}
		if needs {
			break
		}
	}
	if !needs {
		return m
	}
	parts := make([]message.ContentPart, 0, len(m.Parts))
	for _, part := range m.Parts {
		switch p := part.(type) {
		case message.ReasoningContent:
			continue
		case message.ToolCall:
			if p.Name != tools.QuestionToolName {
				p.Input = collapsedCallInput(turn)
				part = p
			}
		}
		parts = append(parts, part)
	}
	out := m
	out.Parts = parts
	return out
}

// collapseToolMessageForTurn returns m with each tool result's stored
// content replaced by collapsedResultText — media payloads included
// (Data cleared so the text stub is what emits). Results answering a
// question call stay verbatim with their call. Returns the message and
// how many results were collapsed.
func collapseToolMessageForTurn(m message.Message, turn int64, callNames map[string]string) (message.Message, int) {
	var parts []message.ContentPart
	count := 0
	for i, part := range m.Parts {
		tr, ok := part.(message.ToolResult)
		if !ok || callNames[tr.ToolCallID] == tools.QuestionToolName {
			if parts != nil {
				parts = append(parts, part)
			}
			continue
		}
		tr.Content = collapsedResultText(turn, tr.ToolCallID)
		tr.Data = ""
		tr.MIMEType = ""
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
