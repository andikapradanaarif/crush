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
	// verdict the run last produced before tripping. A decorator-minted
	// check outcome is the fallback — a verify-loop can trip on a check
	// the write tools reported, which never ran as a bash verdict.
	failed, pending, observed := scanVerification(t.steps)
	if len(observed) > 0 {
		last := observed[len(observed)-1]
		t.lastCheck = &last
	} else if len(failed) > 0 {
		// The latest entry — scanVerification appends in step order,
		// so the tail is the run's most recent verdict.
		t.checkFallback = &failed[len(failed)-1]
	} else if len(pending) > 0 {
		t.checkFallback = &pending[len(pending)-1]
	}
	// Match pullReplanHandoff — write-set paths render relative to the
	// working dir, not as absolute /Users/… leaks.
	workingDir := a.configStore.WorkingDir()
	for i, p := range t.writeSet {
		t.writeSet[i] = relPlanPath(workingDir, p)
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
		shown := t.writeSet
		more := ""
		if len(shown) > 8 {
			more = fmt.Sprintf(" +%d more", len(shown)-8)
			shown = shown[:8]
		}
		fmt.Fprintf(&b, "- Progress: %d steps; wrote: %s%s\n", len(t.steps), strings.Join(shown, ", "), more)
	} else {
		fmt.Fprintf(&b, "- Progress: %d steps; no file writes\n", len(t.steps))
	}
	if tried := stepToolCallSummary(t.steps); tried != "" {
		b.WriteString("- Tried: " + tried + "\n")
	}
	switch {
	case t.lastCheck != nil:
		state := "passed"
		if t.lastCheck.isError {
			state = "failed"
		}
		// tailSafeText flattens newlines — a multi-line command or
		// output must not shatter the bullet's line structure.
		fmt.Fprintf(&b, "- Verification: `%s` %s",
			truncateTailText(tailSafeText(t.lastCheck.command), 120), state)
		if tail := tailSafeText(lastRunes(strings.TrimSpace(t.lastCheck.output), 160)); tail != "" {
			b.WriteString("\n  tail: " + tail)
		}
		b.WriteString("\n")
	case t.checkFallback != nil:
		chk := t.checkFallback.check
		fmt.Fprintf(&b, "- Verification: check `%s` %s",
			truncateTailText(tailSafeText(chk.Check), 80), chk.State)
		if chk.Detail != "" {
			b.WriteString(" — " + truncateTailText(tailSafeText(chk.Detail), 120))
		}
		b.WriteString("\n")
	default:
		b.WriteString("- Verification: no check ran this run\n")
	}
	if t.report != "" {
		b.WriteString("- Blocker: " + t.report + "\n")
	}
	return b.String()
}

// lastRunes keeps the trailing maxRunes runes — the failure summary
// sits at the end of check output, so the tail is the informative end
// (unlike truncateTailText, which keeps the head).
func lastRunes(s string, maxRunes int) string {
	if r := []rune(s); len(r) > maxRunes {
		return string(r[len(r)-maxRunes:])
	}
	return s
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
