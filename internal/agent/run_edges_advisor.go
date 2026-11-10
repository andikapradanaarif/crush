package agent

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"charm.land/fantasy"
)

const (
	// advisorCallTimeout bounds the advisor's model call — an edge
	// boundary must not hang on advice, which is advisory only. It
	// wraps model resolution too: a named model's fire-time provider
	// build must not stall the run boundary.
	advisorCallTimeout = 10 * time.Second
	// advisorMinTokens is the output floor for the note. Reasoning
	// models spend output tokens on reasoning_content before any
	// text — a small cap returns empty (the scope resolver documents
	// the observed truncation on deepseek-flash), so they get their
	// own default budget below.
	advisorMinTokens = 1024
)

// advisorSystemPrompt frames the advisor's job (#440): read the
// deterministic tripwire's evidence and propose ONE next step. The
// judge stays deterministic — the advisor never fires on its own.
const advisorSystemPrompt = "You are an advisor reviewing a coding agent's run that a deterministic " +
	"tripwire just stopped. Given the run-state evidence, propose the single highest-leverage next " +
	"step — name the file, command, or hypothesis to try. Never restate the evidence and never hedge " +
	"across options. Reply in 2-3 sentences, no code blocks."

// adviseOnEdge calls the configured advisor model with the winning
// triggers' evidence and returns the rendered advisor note for the
// repair prompt, or "" when the advisor is off, unbuilt, or fails —
// advice is advisory: failures never block the deterministic repair.
func (a *sessionAgent) adviseOnEdge(ctx context.Context, call SessionAgentCall, winners []contender) string {
	if a.advisorModel == nil {
		return ""
	}
	// The timeout starts before model resolution: a named model's
	// first provider build runs inside the resolver, and the run
	// boundary must not stall on it (the wiring-time pre-warm makes
	// this the fallback path, not the common one).
	actx, cancel := context.WithTimeout(ctx, advisorCallTimeout)
	defer cancel()
	model := a.advisorModel(actx, call.SessionID)
	if model.Model == nil {
		return ""
	}
	evidence := a.advisorEvidence(actx, call, winners)
	if evidence == "" {
		return ""
	}

	// Reasoning models spend output tokens on reasoning_content
	// before any text — a small cap returns empty (the scope
	// resolver documents the observed truncation on deepseek-flash).
	// Mirror its floor: the model's own default budget, so the note
	// survives a reasoning preamble.
	tok := int64(advisorMinTokens)
	if model.CatwalkCfg.CanReason() {
		tok = max(tok, model.CatwalkCfg.DefaultMaxTokens)
	}
	ag := fantasy.NewAgent(
		model.Model,
		fantasy.WithSystemPrompt(advisorSystemPrompt),
		fantasy.WithMaxOutputTokens(tok),
		fantasy.WithUserAgent(userAgent),
	)
	resp, err := ag.Stream(actx, fantasy.AgentStreamCall{
		Prompt:  evidence,
		Headers: sessionHeaders(call.SessionID),
	})
	// The advisor call is a real model invocation — count its usage
	// like the run's own steps.
	a.recordUsage(call.SessionID, resp)
	if err != nil {
		slog.Warn("Edge advisor call failed; proceeding without advice", "error", err, "session_id", call.SessionID)
		return ""
	}
	// Defensive parity with the scope resolver — a nil or
	// length-truncated response is no advice worth annotating.
	if resp == nil || resp.Response.FinishReason == fantasy.FinishReasonLength {
		return ""
	}
	advice := strings.TrimSpace(resp.Response.Content.Text())
	if advice == "" {
		return ""
	}
	return "Advisor note — a reviewer model read the run state above and recommends: " + advice
}

// advisorEvidence assembles the advisor's input from the winning
// triggers: edge identity, the firing detail, and the deterministic
// run-state handoff (#444) — goal, progress, tried, verification,
// blocker — plus any failed/pending gate checks. The handoff pull
// fills goal/check evidence for retry-family winners whose resolve
// never needed it (escalate winners arrive already pulled).
func (a *sessionAgent) advisorEvidence(ctx context.Context, call SessionAgentCall, winners []contender) string {
	var b strings.Builder
	b.WriteString("A deterministic tripwire stopped this coding run. The run's evidence follows.\n\n")
	for _, c := range winners {
		t := c.t
		if t.goal == "" && t.lastCheck == nil && t.checkFallback == nil {
			a.pullEscalationHandoff(ctx, call, t)
		}
		fmt.Fprintf(&b, "Edge: %s", c.edge.name)
		if t.variant != "" {
			fmt.Fprintf(&b, " (%s)", t.variant)
		}
		b.WriteString("\n")
		if t.detail != "" {
			fmt.Fprintf(&b, "Detail: %s\n", t.detail)
		}
		b.WriteString(escalationHandoffLines(t))
		// The handoff's verification line already names the latest
		// verdict — skip its duplicate in the check lists below.
		fbID := ""
		if t.checkFallback != nil {
			fbID = t.checkFallback.check.Identity()
		}
		for _, f := range t.failed {
			if fbID != "" && f.check.Identity() == fbID {
				continue
			}
			fmt.Fprintf(&b, "- Failed check: `%s` — %s\n",
				truncateTailText(tailSafeText(f.check.Check), 80),
				truncateTailText(tailSafeText(f.check.Detail), 120))
		}
		for _, p := range t.pending {
			if fbID != "" && p.check.Identity() == fbID {
				continue
			}
			fmt.Fprintf(&b, "- Pending check: `%s`\n",
				truncateTailText(tailSafeText(p.check.Check), 80))
		}
		b.WriteString("\n")
	}
	b.WriteString("Propose the single highest-leverage next step — name the file, command, or hypothesis " +
		"to try, not a restatement of this evidence. 2-3 sentences, no code blocks.")
	return b.String()
}
