package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/agent/tools"
	"github.com/charmbracelet/crush/internal/event"
	"github.com/charmbracelet/crush/internal/filetracker"
	"github.com/charmbracelet/crush/internal/question"
	"github.com/charmbracelet/crush/internal/session"
	"github.com/charmbracelet/crush/internal/toolclass"
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

// scopeGateEvidenceBounceBudget bounds evidence-condition bounces
// before escalation — the same stuck-loop safety valve the plan
// budget provides, for the model that keeps writing files it has
// not read.
const scopeGateEvidenceBounceBudget = 3

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
	// evBounces counts this run's rejected writes for missing read
	// evidence — the evidence-condition arm's loop counter.
	evBounces int
	// authored holds the canonical paths this run itself wrote —
	// write-tool targets and bash edit targets that passed. A file
	// the run authored needs no read evidence: its content IS the
	// run's own work.
	authored map[string]bool
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
	// gateRejectEvidence bounces a mutating bash call whose concrete
	// write targets lack held read evidence — the fix is in the
	// model's hands (view the targets), so the miss costs one call,
	// not a question.
	gateRejectEvidence
	// gateEscalateEvidence is the evidence-budget-exhausted verdict —
	// a write that keeps missing evidence gets the scope question
	// rather than another bounce, mirroring the plan loop.
	gateEscalateEvidence
)

// gateObservation is one call's verdict plus the bookkeeping the
// caller needs to act on it: the exploration and bounce counters
// feed the confirm prompt, and missing carries the evidence-lacking
// write targets of a gateRejectEvidence/gateEscalateEvidence
// verdict.
type gateObservation struct {
	verdict   gateVerdict
	explore   int
	bounces   int
	evBounces int
	missing   []string
	// targets are the call's resolved write paths — recorded into
	// the authored set only after the wrapped tool succeeds, so a
	// rejected write confers no evidence.
	targets []string
}

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

// scopeGate wraps the tool list to hold evidence conditions at the
// run's write boundary (#441): before a gated action, the run must
// hold the evidence the condition names. The armed-once condition is
// scope-confirm — deep exploration makes the first mutating call
// checkpoint scope with the user. In interactive runs the gate asks
// one structured question — proceed, narrow, or stop — and then
// resolves for the rest of the run: the confirmation is a
// checkpoint, not a toll booth. Tools the model uses to externalize
// a plan (todos, question) satisfy the gate without asking. In
// non-interactive runs the same boundary degrades to
// proceed-with-logged-assumption — a question nobody can answer must
// never stall.
//
// The per-call condition, behind the evidence_gate arm, holds a
// mutating bash call's concrete write targets to read evidence: a
// file the session already read, a path the run itself authored, or
// a file that does not exist yet all pass; an existing file the run
// never saw bounces — the same read-before-write the file tools
// enforce at their own layer, applied to the bash writes that
// bypass it. Bash is the arm's coverage because the file tools
// already enforce the condition themselves; the gate adds no
// redundant check on top of theirs.
//
// Known routes around the checkpoint, accepted by design: delegating
// the write to a task agent (the agent tool isn't a mutating call and
// sub-agent toolsets are unwrapped), and mutating MCP tools whose
// effects can't be classified. The gate is a heuristic checkpoint for
// scope confirmation, not a security boundary.
type scopeGate struct {
	svc         question.Service
	sessions    session.Service
	filetracker filetracker.Service
	interactive bool
	// scope arms the armed-once scope-confirm condition — the
	// deep-exploration checkpoint. It is resolved at construction so
	// the two conditions can arm independently: evidence_gate alone
	// must not start asking scope questions.
	scope bool
	// evidence arms the per-call evidence condition — the #441
	// experiment arm, off unless the option explicitly enables it.
	evidence bool
	// workdir resolves relative call targets — the same base the
	// bash tool runs commands under.
	workdir string
	mu      sync.Mutex
	states  map[string]*scopeGateState
	// approved holds sessions whose plan-mode plan the user approved
	// between runs. The approval resolves the scope check for the next
	// run — the flag is consumed when that run's stamp first creates
	// state, because resolve() on the finishing run's stamp would die
	// with the turn that produced the plan. The flag is unbounded-stale
	// by design: a session resumed much later still carries its approved
	// plan in history, so the approval stays valid — it just must not
	// count twice, which the consume-once semantics guarantee.
	approved map[string]bool
}

// newScopeGate builds the gate. Returns nil when an interactive run
// has no question service to ask through.
func newScopeGate(svc question.Service, interactive bool, sessions session.Service, tracker filetracker.Service, workdir string, scope, evidence bool) *scopeGate {
	if interactive && svc == nil {
		return nil
	}
	return &scopeGate{
		svc:         svc,
		sessions:    sessions,
		filetracker: tracker,
		interactive: interactive,
		scope:       scope,
		evidence:    evidence,
		workdir:     workdir,
		states:      map[string]*scopeGateState{},
		approved:    map[string]bool{},
	}
}

// planDeclared reports whether the session already holds a plan the gate
// can lean on — the armed gate bounces first declarations only, and a
// stored list only counts when every item binds evidence. Bare items —
// handwritten or seeded without evidence — are bookkeeping, not a
// declaration: counting them would let a plan that never named its scope
// disarm the bounce for the rest of the session.
func (g *scopeGate) planDeclared(ctx context.Context, sessionID string) bool {
	if g.sessions == nil {
		return false
	}
	sess, err := g.sessions.Get(ctx, sessionID)
	return err == nil && session.PlanItemsBound(sess.Todos)
}

// ApprovePlan records that the session's plan-mode plan was approved.
// The approval is consumed by the next run's stamp in observe: the
// approved plan IS the scope confirmation, so the run that executes it
// must not double-confirm on its first mutating call.
func (g *scopeGate) ApprovePlan(sessionID string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.approved[sessionID] = true
}

// stateFor returns the session's state for the given run stamp, creating
// it when missing or stale. A pending plan approval is consumed here —
// exactly once, on the first stamp to observe it. Caller must hold g.mu.
func (g *scopeGate) stateFor(sessionID string, stamp uint64) *scopeGateState {
	st, ok := g.states[sessionID]
	if !ok || st.stamp != stamp {
		st = &scopeGateState{stamp: stamp}
		g.states[sessionID] = st
		if g.approved[sessionID] {
			st.resolved = true
			delete(g.approved, sessionID)
		}
	}
	return st
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
// reports the gate's verdict plus the bookkeeping the caller needs
// to act on it. A new run stamp resets the state — the boundary is
// per turn, not per session. gateConfirm and gateEscalateEvidence
// claim the one in-flight question slot (the service supports a
// single pending question); a parallel gated write in the same step
// gets gateWait and is told to re-issue after the question resolves.
func (g *scopeGate) observe(ctx context.Context, call fantasy.ToolCall) gateObservation {
	sessionID := tools.GetSessionFromContext(ctx)
	if sessionID == "" {
		return gateObservation{verdict: gatePass}
	}
	stamp := tools.GetRunStampFromContext(ctx)
	mutating := tools.IsMutatingCall(call.Name, call.Input)

	// Phase one: under the lock, decide which reads the verdict might
	// need — the stored plan and the authored-path snapshot. The
	// reads themselves stay outside the mutex so a slow Get, Stat, or
	// DB query can't serialize unrelated sessions' gates — and a
	// resolved gate skips them entirely.
	g.mu.Lock()
	st := g.stateFor(sessionID, stamp)
	needsPlanCheck := g.scope && call.Name == tools.TodosToolName &&
		!st.resolved && !st.asking && st.explore >= scopeGateMinExploration
	// The evidence condition holds wherever the armed boundary
	// cannot fire — the whole run when scope is off, or the
	// sub-threshold regime while it can.
	needsEvidence := g.evidence && mutating && !st.resolved && !st.asking &&
		(!g.scope || st.explore < scopeGateMinExploration)
	authored := make(map[string]bool, len(st.authored))
	for p := range st.authored {
		authored[p] = true
	}
	g.mu.Unlock()

	declared := needsPlanCheck && g.planDeclared(ctx, sessionID)

	// The evidence scan binds the call's concrete write targets. A
	// mutating bash call's targets get checked for held read
	// evidence; a write tool's file_path only joins the authored set
	// — the file tools enforce read-before-write at their own layer,
	// so the gate adds no redundant check on top of theirs.
	var targets, missing []string
	if needsEvidence {
		switch {
		case call.Name == tools.BashToolName:
			targets, missing = g.scanBashEvidence(ctx, sessionID, authored, call)
		case toolclass.WriteToolNames[call.Name]:
			if p := tools.ToolCallFilePath(call.Input); p != "" {
				if abs := g.absPath(p); abs != "" {
					targets = []string{abs}
				}
			}
		}
	}

	// Phase two: the verdict re-reads state under the lock — a
	// concurrent resolve between phases is authoritative there, and
	// `declared` only ever feeds the armed-bounce branch.
	g.mu.Lock()
	defer g.mu.Unlock()
	st = g.stateFor(sessionID, stamp)
	if mutating {
		// An allowed call carries its targets through so Run marks
		// them authored only after the wrapped tool succeeds — a
		// write the tool layer rejects confers no evidence.
		allow := func(v gateVerdict) gateObservation {
			return gateObservation{verdict: v, explore: st.explore, bounces: st.bounces, evBounces: st.evBounces, targets: targets}
		}
		switch {
		case st.resolved:
			return allow(gatePass)
		case st.asking:
			return gateObservation{verdict: gateWait, explore: st.explore, bounces: st.bounces}
		case g.scope && st.explore >= scopeGateMinExploration:
			st.asking = true
			return allow(gateConfirm)
		default:
			// The routine regime — the scope boundary cannot fire
			// here, but the per-call evidence condition still holds
			// (#441): a mutating bash call whose concrete targets
			// lack held read evidence bounces until the evidence
			// arrives.
			if len(missing) > 0 {
				if st.evBounces >= scopeGateEvidenceBounceBudget {
					// Budget exhausted: escalate to the scope
					// question rather than bounce forever — the
					// same stuck-loop valve the plan budget gives.
					st.asking = true
					return gateObservation{verdict: gateEscalateEvidence, explore: st.explore, evBounces: st.evBounces, missing: missing}
				}
				st.evBounces++
				return gateObservation{verdict: gateRejectEvidence, explore: st.explore, evBounces: st.evBounces, missing: missing}
			}
			return allow(gatePass)
		}
	}
	// An in-flight question already externalized the scope decision —
	// the gate is satisfied for the rest of the run.
	if call.Name == tools.QuestionToolName {
		st.resolved = true
		return gateObservation{verdict: gatePass, explore: st.explore, bounces: st.bounces}
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
		if g.scope && !st.resolved && !st.asking && st.explore >= scopeGateMinExploration {
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
					return gateObservation{verdict: gateEscalatePlan, explore: st.explore, bounces: st.bounces}
				}
				st.bounces++
				return gateObservation{verdict: gateRejectPlan, explore: st.explore, bounces: st.bounces}
			}
		}
		return gateObservation{verdict: gatePass, explore: st.explore, bounces: st.bounces}
	}
	st.explore++
	return gateObservation{verdict: gatePass, explore: st.explore, bounces: st.bounces}
}

// absPath resolves a call target against the gate's working
// directory — the same base the bash tool runs commands under. A
// relative target with no configured base can't be bound and
// returns "".
func (g *scopeGate) absPath(p string) string {
	if !filepath.IsAbs(p) {
		if g.workdir == "" {
			return ""
		}
		p = filepath.Join(g.workdir, p)
	}
	return filepath.Clean(p)
}

// scanBashEvidence splits a mutating bash call's concrete write
// targets into the authored set and the evidence-missing list: an
// existing file the session never read and the run did not itself
// write. It runs outside the gate mutex — per-target checks stat
// the filesystem and read the filetracker DB. Targets needing shell
// expansion and files that do not exist yet hold no evidence
// expectation — a definite path is required to bind evidence, and
// creating a file needs no read.
func (g *scopeGate) scanBashEvidence(ctx context.Context, sessionID string, authored map[string]bool, call fantasy.ToolCall) (targets, missing []string) {
	if g.filetracker == nil {
		return nil, nil // No evidence store — the arm cannot bind paths.
	}
	var params struct {
		Command string `json:"command"`
	}
	if err := json.Unmarshal([]byte(call.Input), &params); err != nil || params.Command == "" {
		return nil, nil
	}
	for _, target := range toolclass.BashEditTargets(params.Command) {
		abs := g.absPath(target)
		if abs == "" {
			continue
		}
		targets = append(targets, abs)
		if authored[abs] {
			continue // The run wrote it — authoring is the evidence.
		}
		if _, err := os.Stat(abs); err != nil {
			continue // Creation — no read evidence needed.
		}
		if g.filetracker.LastReadTime(ctx, sessionID, abs).IsZero() {
			missing = append(missing, target)
		}
	}
	return targets, missing
}

// markAuthored records a successful write's targets into the run's
// authored set — paths the run itself wrote, which later evidence
// checks accept as held evidence. Called post-run so a failed or
// rejected write confers nothing.
func (g *scopeGate) markAuthored(ctx context.Context, targets []string) {
	if len(targets) == 0 {
		return
	}
	sessionID := tools.GetSessionFromContext(ctx)
	if sessionID == "" {
		return
	}
	stamp := tools.GetRunStampFromContext(ctx)
	g.mu.Lock()
	defer g.mu.Unlock()
	st := g.stateFor(sessionID, stamp)
	if st.authored == nil {
		st.authored = map[string]bool{}
	}
	for _, p := range targets {
		st.authored[p] = true
	}
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
// deserves to know the model could not conform. An evidence-miss
// escalation asks the evidence variant instead: the stuck loop was
// writes to unread files, not plan declarations.
func (g *scopeGate) confirm(ctx context.Context, obs gateObservation) (proceed bool, err error) {
	label := "Scope check"
	description := "The exploration so far suggests a non-routine change. " +
		"Confirming means the plan is worth a checkpoint; you can also narrow the scope or stop to restate the task."
	var text string
	switch {
	case len(obs.missing) > 0:
		label = "Evidence check"
		text = fmt.Sprintf("The agent tried to write files it had not read — %s — and bounced %d times on the read-evidence requirement. Allow the write anyway?",
			strings.Join(obs.missing, ", "), obs.evBounces)
		description = "An evidence-conditioned write kept missing its evidence — the files were never read this session. " +
			"Allowing runs the write anyway; you can also narrow the scope or stop to restate the task."
	default:
		text = fmt.Sprintf("This task has explored %d steps without a declared plan", obs.explore)
		switch {
		case obs.bounces > 1:
			text += fmt.Sprintf(", and %d plan declarations were rejected for missing evidence binding — the model may be stuck in a bounce loop", obs.bounces)
		case obs.bounces == 1:
			// A write-triggered confirm after one bounce is not a
			// loop — name the rejection without the stuck-loop
			// framing.
			text += ", and a plan declaration was rejected for missing evidence binding"
		}
		text += ". Confirm scope before the first write?"
	}
	answers, err := g.svc.Ask(ctx, question.Request{
		SessionID: tools.GetSessionFromContext(ctx),
		Questions: []question.Question{{
			Type:        question.TypeSingleChoice,
			Label:       label,
			Text:        text,
			Description: description,
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
	obs := t.gate.observe(ctx, call)
	switch obs.verdict {
	case gatePass:
		resp, err := t.inner.Run(ctx, call)
		if err == nil && !resp.IsError {
			// A plan declaration satisfies the gate only once a
			// validating list has actually landed — a write the tool
			// rejected must not count as a declaration.
			if call.Name == tools.TodosToolName && planCallResolves(call.Input) {
				t.gate.resolve(ctx)
			}
			t.gate.markAuthored(ctx, obs.targets)
		}
		return resp, err
	case gateRejectPlan:
		// The rejection lives only inside this tool result — export
		// it so telemetry and logs see the loop a bare-plan model
		// would otherwise burn turns inside.
		sessionID := tools.GetSessionFromContext(ctx)
		event.PlanDeclarationBounced("session id", sessionID, "bounces", obs.bounces)
		slog.Info("Scope gate: plan declaration bounced",
			"session_id", sessionID,
			"bounces", obs.bounces,
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
	case gateRejectEvidence:
		// Same export discipline as the plan bounce — the rejection
		// lives only inside this tool result, so the loop must be
		// visible to telemetry and logs.
		sessionID := tools.GetSessionFromContext(ctx)
		event.EvidenceGateBounced("session id", sessionID, "bounces", obs.evBounces)
		slog.Info("Scope gate: write bounced for missing read evidence",
			"session_id", sessionID,
			"targets", strings.Join(obs.missing, ","),
			"bounces", obs.evBounces,
		)
		msg := "This bash call writes files the run has not read: " + strings.Join(obs.missing, ", ") +
			". Read evidence is required before writing — view each target first, " +
			"or declare them in the plan via todos evidence_paths."
		return fantasy.NewTextErrorResponse(msg), nil
	case gateWait:
		return fantasy.NewTextErrorResponse(
			"Scope check in progress — re-issue this call after the pending question resolves.",
		), nil
	}

	sessionID := tools.GetSessionFromContext(ctx)
	if obs.verdict == gateEscalatePlan {
		event.PlanDeclarationEscalated("session id", sessionID, "bounces", obs.bounces)
		slog.Warn("Scope gate: plan bounce budget exhausted, escalating to the scope question",
			"session_id", sessionID,
			"bounces", obs.bounces,
		)
	}
	if obs.verdict == gateEscalateEvidence {
		event.EvidenceGateEscalated("session id", sessionID, "bounces", obs.evBounces)
		slog.Warn("Scope gate: evidence bounce budget exhausted, escalating to the scope question",
			"session_id", sessionID,
			"targets", strings.Join(obs.missing, ","),
			"bounces", obs.evBounces,
		)
	}

	if !t.gate.interactive {
		// Headless degrade: proceed with a logged assumption — the
		// boundary is observed and recorded, never asked.
		t.gate.resolve(ctx)
		slog.Info("Scope gate: proceeding on stated assumptions",
			"tool", call.Name,
			"session_id", sessionID,
		)
		resp, err := t.inner.Run(ctx, call)
		if err == nil && !resp.IsError {
			t.gate.markAuthored(ctx, obs.targets)
		}
		return resp, err
	}

	proceed, err := t.gate.confirm(ctx, obs)
	t.gate.resolve(ctx)
	switch {
	case err == nil && proceed:
		resp, err := t.inner.Run(ctx, call)
		if err == nil && !resp.IsError {
			t.gate.markAuthored(ctx, obs.targets)
		}
		return resp, err
	case errors.Is(err, question.ErrCancelled):
		resp := fantasy.NewTextErrorResponse("User stopped at the scope check")
		resp.StopTurn = true
		return resp, nil
	case err != nil:
		// A degraded question path must not stall the run: treat the
		// failure as "you decide" and let the write through.
		resp, err := t.inner.Run(ctx, call)
		if err == nil && !resp.IsError {
			t.gate.markAuthored(ctx, obs.targets)
		}
		return resp, err
	default:
		return fantasy.NewTextErrorResponse(
			"Scope check: the user asked to narrow the plan. Restate a smaller scope " +
				"with the todos tool, then re-issue the call.",
		), nil
	}
}
