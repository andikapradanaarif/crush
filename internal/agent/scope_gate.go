package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/agent/tools"
	"github.com/charmbracelet/crush/internal/event"
	"github.com/charmbracelet/crush/internal/question"
	"github.com/charmbracelet/crush/internal/session"
)

// scopeGateMinExploration is the explore→execute boundary: a first
// mutating call that arrives only after this many exploration calls
// marks a task whose scope is worth confirming. Routine-size tasks —
// a handful of reads before the edit — pass un-gated.
const scopeGateMinExploration = 8

// scopeGatePlanBounceBudget bounds how many times a non-validating
// plan declaration may bounce before the gate escalates to the real
// scope question. An unbounded bounce is a zero-cost stall — the
// rejection only exists inside the tool result — and an unrepairable
// one (the evidence vocabulary never surfaced to the model) is a
// guaranteed loop, so the budget doubles as its safety valve.
const scopeGatePlanBounceBudget = 3

// scopeGateState is the per-session gate bookkeeping for one turn — one
// entry per session ID, negligible growth. A new run stamp resets it;
// repair retries share the turn's stamp via SessionAgentCall.RunStamp
// carried through the retry clone, so each user turn gets one boundary
// check.
type scopeGateState struct {
	stamp    uint64
	explore  int
	asking   bool
	resolved bool
	// bounces counts this run's rejected plan declarations — the
	// conformance signal exported to telemetry on each bounce.
	bounces int
}

// gateVerdict is observe's verdict: pass the call through, hold it
// while a scope question is already in flight, confirm scope with the
// user before the write executes, bounce a non-declaring plan call,
// or escalate an exhausted bounce loop to the scope question.
// gateWait is defensive — parallel
// tools (download, fetch, web_*, MCP reads) run concurrently under
// parallelSem, and a parallel-classified mutating tool like download
// could observe st.asking mid-question if the fantasy executor ever
// pipelines a step's calls; it stays as insurance.
type gateVerdict int

const (
	gatePass gateVerdict = iota
	gateWait
	gateConfirm
	// gateRejectPlan bounces a todos call that cannot count as a
	// declaration — the gate is armed and the submitted list is empty
	// or carries an item with no evidence bound.
	gateRejectPlan
	// gateEscalatePlan is the bounce-budget-exhausted verdict: the
	// declaration loop gets the real scope question with stuck-loop
	// context instead of another bounce. Escalation, never
	// pass-through — a free pass after N rejections would teach the
	// spam-bypass.
	gateEscalatePlan
)

// parsePlanCall extracts the submitted list — the gate checks a
// declaration's shape, not its tool-level validity.
func parsePlanCall(input string) (tools.TodosParams, error) {
	var params tools.TodosParams
	err := json.Unmarshal([]byte(input), &params)
	return params, err
}

// planParamsResolve reports whether a submitted list declares a plan
// the gate accepts: a non-empty list where every item binds evidence —
// checks or paths. A plan of bare strings, or an empty list, is a
// legal write but not a declaration.
func planParamsResolve(params tools.TodosParams) bool {
	if len(params.Todos) == 0 {
		return false
	}
	for _, item := range params.Todos {
		if len(item.EvidenceChecks) == 0 && len(item.EvidencePaths) == 0 {
			return false
		}
	}
	return true
}

// planCallResolves reports whether a todos call's input is a parseable,
// evidence-bound declaration.
func planCallResolves(input string) bool {
	params, err := parsePlanCall(input)
	return err == nil && planParamsResolve(params)
}

// scopeGate wraps the tool list to intercept the first mutating call of
// a run when deep exploration suggests a non-routine scope. In
// interactive runs the gate asks one structured question — proceed,
// narrow, or stop — and then resolves for the rest of the run: the
// confirmation is a checkpoint, not a toll booth. Tools the model uses
// to externalize a plan (todos, question) satisfy the gate without
// asking. In non-interactive runs the same boundary degrades to
// proceed-with-logged-assumption — a question nobody can answer must
// never stall.
//
// Known routes around the checkpoint, accepted by design: delegating
// the write to a task agent (the agent tool isn't a mutating call and
// sub-agent toolsets are unwrapped), and mutating MCP tools whose
// effects can't be classified. The gate is a heuristic checkpoint for
// scope confirmation, not a security boundary.
type scopeGate struct {
	svc         question.Service
	sessions    session.Service
	interactive bool
	mu          sync.Mutex
	states      map[string]*scopeGateState
}

// newScopeGate builds the gate. Returns nil when an interactive run
// has no question service to ask through.
func newScopeGate(svc question.Service, interactive bool, sessions session.Service) *scopeGate {
	if interactive && svc == nil {
		return nil
	}
	return &scopeGate{svc: svc, sessions: sessions, interactive: interactive, states: map[string]*scopeGateState{}}
}

// planDeclared reports whether the session already holds a plan — the
// armed gate bounces first declarations only; bookkeeping writes to an
// existing (bare) plan are not declarations and must not be hostage
// to evidence binding.
func (g *scopeGate) planDeclared(ctx context.Context, sessionID string) bool {
	if g.sessions == nil {
		return false
	}
	sess, err := g.sessions.Get(ctx, sessionID)
	return err == nil && len(sess.Todos) > 0
}

// wrap decorates every tool so the gate sees exploration calls as well
// as writes. Only mutating calls are ever intercepted. The gate object
// is long-lived — it survives SetTools rebuilds so per-turn
// exploration bookkeeping and the resolved mark persist across wraps.
func (g *scopeGate) wrap(all []fantasy.AgentTool) []fantasy.AgentTool {
	out := make([]fantasy.AgentTool, len(all))
	for i, tool := range all {
		out[i] = &scopeGateTool{inner: tool, gate: g}
	}
	return out
}

// observe records one tool call against the session's run state and
// reports the gate's verdict plus the exploration and bounce counts
// the verdict was reached at. A new run stamp resets the state — the
// boundary is per turn, not per session. gateConfirm claims the one
// in-flight question slot (the service supports a single pending
// question); a parallel gated write in the same step gets gateWait and
// is told to re-issue after the question resolves.
func (g *scopeGate) observe(ctx context.Context, call fantasy.ToolCall) (gateVerdict, int, int) {
	sessionID := tools.GetSessionFromContext(ctx)
	if sessionID == "" {
		return gatePass, 0, 0
	}
	stamp := tools.GetRunStampFromContext(ctx)

	// Phase one: under the lock, decide only whether the armed-bounce
	// path could need the stored plan. The session read itself stays
	// outside the mutex so a slow Get can't serialize unrelated
	// sessions' gates — and a resolved gate skips the read entirely,
	// since bookkeeping writes are never bounced anyway.
	g.mu.Lock()
	st, ok := g.states[sessionID]
	if !ok || st.stamp != stamp {
		st = &scopeGateState{stamp: stamp}
		g.states[sessionID] = st
	}
	needsPlanCheck := call.Name == tools.TodosToolName &&
		!st.resolved && !st.asking && st.explore >= scopeGateMinExploration
	g.mu.Unlock()

	declared := needsPlanCheck && g.planDeclared(ctx, sessionID)

	// Phase two: the verdict re-reads state under the lock — a
	// concurrent resolve between phases is authoritative there, and
	// `declared` only ever feeds the armed-bounce branch.
	g.mu.Lock()
	defer g.mu.Unlock()
	st, ok = g.states[sessionID]
	if !ok || st.stamp != stamp {
		st = &scopeGateState{stamp: stamp}
		g.states[sessionID] = st
	}
	if tools.IsMutatingCall(call.Name, call.Input) {
		switch {
		case st.resolved || st.explore < scopeGateMinExploration:
			return gatePass, st.explore, st.bounces
		case st.asking:
			return gateWait, st.explore, st.bounces
		default:
			st.asking = true
			return gateConfirm, st.explore, st.bounces
		}
	}
	// An in-flight question already externalized the scope decision —
	// the gate is satisfied for the rest of the run.
	if call.Name == tools.QuestionToolName {
		st.resolved = true
		return gatePass, st.explore, st.bounces
	}
	// A plan call resolves the gate only once a validating plan has
	// landed (checked post-run in Run). While the gate is armed, a
	// non-validating declaration gets bounced with the reason so the
	// model fixes the list instead of hitting the question cold.
	if call.Name == tools.TodosToolName {
		// Deliberate hole, pre-existing: while a question is in
		// flight (st.asking), a concurrent todos call falls through
		// to gatePass unchecked — a parallel non-validating
		// declaration lands a bare plan. Parallel tool calls make
		// the window rare, and the alternative (queuing plan calls
		// behind a pending question) serializes the common case.
		if !st.resolved && !st.asking && st.explore >= scopeGateMinExploration {
			// Only a parseable, non-validating list bounces — malformed
			// input passes through so the tool's own parse error
			// explains the failure instead of a misleading plan
			// rejection.
			if params, err := parsePlanCall(call.Input); err == nil && !planParamsResolve(params) && !declared {
				if st.bounces >= scopeGatePlanBounceBudget {
					// Budget exhausted: escalate to the real scope
					// question with stuck-loop context rather than
					// bounce forever. The escalating call is not
					// itself a bounce — bounces counts rejections.
					st.asking = true
					return gateEscalatePlan, st.explore, st.bounces
				}
				st.bounces++
				return gateRejectPlan, st.explore, st.bounces
			}
		}
		return gatePass, st.explore, st.bounces
	}
	st.explore++
	return gatePass, st.explore, st.bounces
}

// resolve marks the session's current run gated — the checkpoint fired
// once, whatever the answer.
func (g *scopeGate) resolve(ctx context.Context) {
	sessionID := tools.GetSessionFromContext(ctx)
	if sessionID == "" {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if st, ok := g.states[sessionID]; ok {
		st.asking = false
		st.resolved = true
	}
}

// confirm asks the scope question. The choice descriptions are where
// the tradeoffs live. The question service keeps a single pending
// question globally — a concurrent Ask from another session would
// clobber it; that hazard pre-dates the gate (the question tool
// exposes it too) and same-session serialization keeps it from
// firing here. Prior plan bounces join the prompt as stuck-loop
// context — an escalation asks the same question, but the user
// deserves to know the model could not conform.
func (g *scopeGate) confirm(ctx context.Context, explore, bounces int) (proceed bool, err error) {
	text := fmt.Sprintf("This task has explored %d steps without a declared plan", explore)
	switch {
	case bounces > 1:
		text += fmt.Sprintf(", and %d plan declarations were rejected for missing evidence binding — the model may be stuck in a bounce loop", bounces)
	case bounces == 1:
		// A write-triggered confirm after one bounce is not a loop —
		// name the rejection without the stuck-loop framing.
		text += ", and a plan declaration was rejected for missing evidence binding"
	}
	text += ". Confirm scope before the first write?"
	answers, err := g.svc.Ask(ctx, question.Request{
		SessionID: tools.GetSessionFromContext(ctx),
		Questions: []question.Question{{
			Type:  question.TypeSingleChoice,
			Label: "Scope check",
			Text:  text,
			Description: "The exploration so far suggests a non-routine change. " +
				"Confirming means the plan is worth a checkpoint; you can also narrow the scope or stop to restate the task.",
			Choices: []question.Choice{
				{ID: "proceed", Label: "Proceed", Description: "Scope looks right — run the planned writes."},
				{ID: "narrow", Label: "Narrow scope", Description: "Do less: the agent restates a smaller plan before writing."},
				{ID: "stop", Label: "Stop", Description: "End the turn so you can restate the task."},
			},
		}},
	})
	if err != nil {
		return false, err
	}
	for _, a := range answers {
		for _, id := range a.SelectedIDs {
			if id == "proceed" {
				return true, nil
			}
			if id == "stop" {
				return false, question.ErrCancelled
			}
		}
	}
	return false, nil
}

// scopeGateTool is the decorator half of scopeGate: it feeds calls to
// observe and blocks the gated write on the user's answer.
type scopeGateTool struct {
	inner fantasy.AgentTool
	gate  *scopeGate
}

// Unwrap returns the wrapped tool, matching hookedTool's convention.
func (t *scopeGateTool) Unwrap() fantasy.AgentTool {
	return t.inner
}

func (t *scopeGateTool) Info() fantasy.ToolInfo {
	return t.inner.Info()
}

func (t *scopeGateTool) ProviderOptions() fantasy.ProviderOptions {
	return t.inner.ProviderOptions()
}

func (t *scopeGateTool) SetProviderOptions(opts fantasy.ProviderOptions) {
	t.inner.SetProviderOptions(opts)
}

func (t *scopeGateTool) Run(ctx context.Context, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
	verdict, explore, bounces := t.gate.observe(ctx, call)
	switch verdict {
	case gatePass:
		resp, err := t.inner.Run(ctx, call)
		// A plan declaration satisfies the gate only once a
		// validating list has actually landed — a write the tool
		// rejected must not count as a declaration.
		if err == nil && !resp.IsError &&
			call.Name == tools.TodosToolName && planCallResolves(call.Input) {
			t.gate.resolve(ctx)
		}
		return resp, err
	case gateRejectPlan:
		// The rejection lives only inside this tool result — export
		// it so telemetry and logs see the loop a bare-plan model
		// would otherwise burn turns inside.
		sessionID := tools.GetSessionFromContext(ctx)
		event.PlanDeclarationBounced("session id", sessionID, "bounces", bounces)
		slog.Info("Scope gate: plan declaration bounced",
			"session_id", sessionID,
			"bounces", bounces,
		)
		msg := "This todos call does not count as declaring the plan: every item must bind " +
			"evidence via evidence_checks or evidence_paths, and the list must not be empty. " +
			"Resubmit with evidence bound to each item"
		if t.gate.interactive {
			msg += ", or proceed and answer the scope-check question when the first write triggers it."
		} else {
			msg += " — the scope check resolves on the first write."
		}
		return fantasy.NewTextErrorResponse(msg), nil
	case gateWait:
		return fantasy.NewTextErrorResponse(
			"Scope check in progress — re-issue this call after the pending question resolves.",
		), nil
	}

	if verdict == gateEscalatePlan {
		sessionID := tools.GetSessionFromContext(ctx)
		event.PlanDeclarationEscalated("session id", sessionID, "bounces", bounces)
		slog.Warn("Scope gate: plan bounce budget exhausted, escalating to the scope question",
			"session_id", sessionID,
			"bounces", bounces,
		)
	}

	if !t.gate.interactive {
		// Headless degrade: proceed with a logged assumption — the
		// boundary is observed and recorded, never asked.
		t.gate.resolve(ctx)
		slog.Info("Scope gate: proceeding on stated assumptions",
			"tool", call.Name,
			"session_id", tools.GetSessionFromContext(ctx),
		)
		return t.inner.Run(ctx, call)
	}

	proceed, err := t.gate.confirm(ctx, explore, bounces)
	t.gate.resolve(ctx)
	switch {
	case err == nil && proceed:
		return t.inner.Run(ctx, call)
	case errors.Is(err, question.ErrCancelled):
		resp := fantasy.NewTextErrorResponse("User stopped at the scope check")
		resp.StopTurn = true
		return resp, nil
	case err != nil:
		// A degraded question path must not stall the run: treat the
		// failure as "you decide" and let the write through.
		return t.inner.Run(ctx, call)
	default:
		return fantasy.NewTextErrorResponse(
			"Scope check: the user asked to narrow the plan. Restate a smaller scope " +
				"with the todos tool, then re-issue the call.",
		), nil
	}
}
