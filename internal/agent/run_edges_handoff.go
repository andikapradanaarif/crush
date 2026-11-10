package agent

import (
	"context"
	"fmt"
	"strings"

	"charm.land/fantasy"
)

// pullEscalationHandoff fills the trigger's escalate-handoff evidence
// (#444) — goal and lastCheck — from the stored transcript and the
// run's own steps. Pulled in resolve (like pullReplanHandoff) so the
// escalate prompt can relay run state the model would otherwise
// freehand or omit entirely.
func (a *sessionAgent) pullEscalationHandoff(ctx context.Context, call SessionAgentCall, t *edgeTrigger) {
	// Goal: the last real-user prompt on the transcript. No excludeID —
	// the repair prompt row driving this turn isn't a real user message
	// (isRealUserMsg filters it), and the one case where currentUserMsgID
	// IS the goal is burn-watch firing on the original turn, which must
	// not exclude it.
	if a.messages != nil {
		if msgs, err := a.messages.List(ctx, call.SessionID); err == nil {
			if idx := lastRealUserIdx(msgs, ""); idx >= 0 {
				t.goal = truncateTailText(strings.TrimSpace(firstLine(msgs[idx].JoinedText())), 200)
			}
		}
	}
	// Verification state: the last verdict-bearing bash/job result the
	// run observed — the verify-loop's kept-failing check, or whatever
	// verdict the run last produced before tripping.
	_, _, observed := scanVerification(t.steps)
	if len(observed) > 0 {
		last := observed[len(observed)-1]
		t.lastCheck = &last
	}
}

// escalationHandoffSection renders the prompt-side framing plus the
// deterministic run-state payload every escalation-family prompt
// carries (#444). The escalate instruction follows it — the model's
// question relays this block.
func escalationHandoffSection(t *edgeTrigger) string {
	return "Run state for the handoff — assembled deterministically; relay it to the user:\n" +
		escalationHandoffLines(t)
}

// escalationHandoffLines renders the payload itself — goal, progress,
// what was tried, verification state, and the concrete blocker — as
// terminal-safe lines. The headless degrade path appends them to the
// final assistant message so the same report reaches RunComplete.Text
// when there is no question to carry it.
func escalationHandoffLines(t *edgeTrigger) string {
	if t == nil {
		return ""
	}
	var b strings.Builder
	if t.goal != "" {
		b.WriteString("- Goal: " + t.goal + "\n")
	}
	if len(t.writeSet) > 0 {
		fmt.Fprintf(&b, "- Progress: %d steps; wrote: %s\n", len(t.steps), strings.Join(t.writeSet, ", "))
	} else {
		fmt.Fprintf(&b, "- Progress: %d steps; no file writes\n", len(t.steps))
	}
	if tried := stepToolCallSummary(t.steps); tried != "" {
		b.WriteString("- Tried: " + tried + "\n")
	}
	if t.lastCheck != nil {
		state := "passed"
		if t.lastCheck.isError {
			state = "failed"
		}
		fmt.Fprintf(&b, "- Verification: `%s` %s", t.lastCheck.command, state)
		if tail := truncateTailText(strings.TrimSpace(t.lastCheck.output), 160); tail != "" {
			b.WriteString("\n  tail: " + tail)
		}
		b.WriteString("\n")
	} else {
		b.WriteString("- Verification: no check ran this run\n")
	}
	if t.report != "" {
		b.WriteString("- Blocker: " + t.report + "\n")
	}
	return b.String()
}

// stepToolCallSummary renders the run's tool calls as "bash ×6, view ×3"
// — the handoff's what-was-tried line. First-seen order keeps the
// summary stable and readable.
func stepToolCallSummary(steps []fantasy.StepResult) string {
	counts := make(map[string]int)
	var order []string
	for _, step := range steps {
		for _, tc := range step.Content.ToolCalls() {
			if _, ok := counts[tc.ToolName]; !ok {
				order = append(order, tc.ToolName)
			}
			counts[tc.ToolName]++
		}
	}
	parts := make([]string, 0, len(order))
	for _, name := range order {
		parts = append(parts, fmt.Sprintf("%s ×%d", name, counts[name]))
	}
	return strings.Join(parts, ", ")
}
