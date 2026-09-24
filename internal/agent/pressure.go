package agent

import (
	"log/slog"

	"github.com/charmbracelet/crush/internal/message"
)

// pressureParallelResults bounds the capped tool results a single
// step can append — the step-jump term in the gate's margin. The
// estimate is one render stale: a step that writes several 50KB
// results plus a max-output completion can leap a smaller margin
// before the next evaluation, so the reserve must exceed the
// largest plausible jump. "Plausible" is a heuristic, not a bound:
// nothing caps tool calls per step, so a fan-out beyond this count
// (or a binary attachment — the estimate counts only text, call,
// result, and reasoning parts) can still leap any margin. The gate
// is overflow insurance priced for the common jump, not a
// guarantee.
const pressureParallelResults = 4

// defaultOutputReserve is the output-token floor when neither the
// call nor the catalog declares one — the same fallback
// CONTEXT_WINDOW_SAFETY.md specifies.
const defaultOutputReserve = 4_096

// pressureMargin returns the engage threshold expressed as a
// remaining-window reserve: the gate engages when
// cw - estimate <= margin. The reserve is the output reserve plus a
// bounded single-step jump — priced to absorb one render-stale step
// of capped tool results, rarely leapt but not never (the jump is
// heuristic, not a bound — see pressureParallelResults). No
// window-proportional term: it was always dominated by this sum and
// is dropped rather than carried as dead arithmetic. A window smaller
// than the margin therefore engages on the first render — honest,
// since a 64K window genuinely cannot absorb one capped-results batch
// plus a completion.
func pressureMargin(outputReserve int64) int64 {
	return outputReserve + pressureParallelResults*int64(toolResultMaxContentBytes/4)
}

// outputReserve is the completion headroom the margin carries —
// call-site max output isn't reachable from the render path, so the
// catalog default stands in, floored at defaultOutputReserve.
func (a *sessionAgent) outputReserve() int64 {
	if v := a.largeModel.Get().CatwalkCfg.DefaultMaxTokens; v > 0 {
		return v
	}
	return defaultOutputReserve
}

// pressureEngaged reports whether the notebook render path runs
// compacted for this render. Below the margin the request renders
// verbatim — no boundary eviction, prefix, or collapse — because
// the machinery's value is overflow insurance and its steady-state
// cost is prefix-cache churn.
//
// The estimate anchors on the provider-reported size of the last
// request (requestStats.LastPromptTokens) plus a chars/4 delta over
// the messages appended since. The last response persists as a
// message and lands inside that delta, so adding CompletionTokens
// would double-count it. Two known under-counts, both margin-
// absorbed: msgs predates the run's own user prompt (the estimate
// misses one incoming turn), and a request failing before usage
// leaves the anchor stale one render. With no usage recorded — the
// first request, or a cold process resuming a session — the estimate
// is the whole verbatim render; fixed overhead (system prompt,
// tools) rides inside the margin.
//
// Once tripped the gate latches for the session: stored history is
// append-only, so a verbatim render that overflowed once never fits
// again — the latch is monotonicity, not just anti-flap. A
// successful Summarize is the one write that shrinks history, so it
// clears the latch (see its save path). Callers holding no evidence
// (missing stats, unknown window) keep the machinery on:
// deactivating a safety mechanism needs positive proof of headroom.
// An unknown window still records the estimate — only the latch
// decision has nothing to check against.
func (a *sessionAgent) pressureEngaged(sessionID string, msgs []message.Message) bool {
	if sessionID == "" || a.reqStats == nil {
		return true
	}
	rs, _ := a.reqStats.Get(sessionID)
	var est int64
	if rs.LastPromptTokens > 0 {
		wm := min(rs.renderedMsgs, len(msgs))
		est = rs.LastPromptTokens + int64(estimateRawMessageTokens(msgs[wm:]))
	} else {
		est = int64(estimateRawMessageTokens(msgs))
	}
	rs.pressureEstimate = est
	// The watermark advances at render-eval time, so a request that
	// fails before reporting usage leaves LastPromptTokens stale
	// while renderedMsgs moved — the next estimate misses one
	// render's growth. Self-correcting on the next success and
	// priced into the step-jump margin.
	rs.renderedMsgs = len(msgs)
	cw := int64(a.largeModel.Get().CatwalkCfg.ContextWindow)
	if cw == 0 {
		// No declared window means no margin to cross — machinery
		// stays on without latching or counting an activation
		// (engaged means a measured crossing, not the fail-safe
		// default). The estimate and watermark still record so an
		// unknown-window run audits as measured, not absent.
		a.reqStats.Set(sessionID, rs)
		return true
	}
	if !rs.pressureEngaged && est >= cw-pressureMargin(a.outputReserve()) {
		rs.pressureEngaged = true
		rs.pressureActivations++
		slog.Info("Notebook pressure gate engaged",
			"session_id", sessionID,
			"estimate_tokens", est,
			"context_window", cw,
		)
	}
	a.reqStats.Set(sessionID, rs)
	return rs.pressureEngaged
}

// clearPressureState drops the gate's session state after a write
// that shrinks stored history — Summarize is the only one. The
// latch and its anchors describe pre-compact history, so the next
// render re-derives engagement from the shrunken list. Activations
// keep counting: a re-engage post-compact is a real transition.
func (a *sessionAgent) clearPressureState(sessionID string) {
	if a.reqStats == nil {
		return
	}
	a.reqStats.Update(sessionID, func(rs *requestStats) {
		rs.pressureEngaged = false
		rs.pressureEstimate = 0
		rs.renderedMsgs = 0
		rs.LastPromptTokens = 0
	})
}
