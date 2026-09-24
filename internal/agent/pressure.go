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
// cw - estimate <= margin. The legacy auto-summarize shape (a flat
// 20K for windows over 200K, else 20% of the window) is the floor;
// the step-jump reserve raises it so a single step's growth rarely
// leaps the margin between renders — rarely, not never, since the
// jump bound is heuristic (see pressureParallelResults). Small
// windows therefore engage almost immediately — honest, since a
// 64K window genuinely cannot absorb one capped-results batch plus
// a completion.
func pressureMargin(cw, outputReserve int64) int64 {
	legacy := int64(float64(cw) * smallContextWindowRatio)
	if cw > largeContextWindowThreshold {
		legacy = largeContextWindowBuffer
	}
	stepJump := outputReserve + pressureParallelResults*int64(toolResultMaxContentBytes/4)
	return max(legacy, stepJump)
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
// would double-count it. With no usage recorded — the first
// request, or a cold process resuming a session — the estimate is
// the whole verbatim render; fixed overhead (system prompt, tools)
// rides inside the margin.
//
// Once tripped the gate latches for the session: stored history is
// append-only, so a verbatim render that overflowed once never fits
// again — the latch is monotonicity, not just anti-flap. Callers
// holding no evidence (missing stats, unknown window) keep the
// machinery on: deactivating a safety mechanism needs positive
// proof of headroom.
func (a *sessionAgent) pressureEngaged(sessionID string, msgs []message.Message) bool {
	cw := int64(a.largeModel.Get().CatwalkCfg.ContextWindow)
	if cw == 0 || sessionID == "" || a.reqStats == nil {
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
	if !rs.pressureEngaged && est >= cw-pressureMargin(cw, a.outputReserve()) {
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
