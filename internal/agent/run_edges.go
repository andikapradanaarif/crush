package agent

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/agent/tools"
	"github.com/charmbracelet/crush/internal/db"
	"github.com/charmbracelet/crush/internal/index"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/notebook"
	"github.com/charmbracelet/crush/internal/toolclass"
)

// maxRepairAttempts bounds the shared repair-turn budget: the number of
// harness-initiated follow-up turns permitted after the original turn.
// Every firing edge draws from this one counter — unbudgeted escalation
// is nagging. The loop detector is not a second bound: it resets per Run
// and sees a different signature each retry anyway.
const maxRepairAttempts = 2

// edgeOutcome is the recorded per-boundary result of one edge's
// evaluation — the edge_firings.outcome enum. A boundary with no row
// for an edge means clean, which stays distinguishable precisely
// because the non-firing outcomes (gated, suppressed, cleared,
// cancelled) are still recorded.
type edgeOutcome string

const (
	// edgeOutcomeFired means the trigger's prompt made the enqueued
	// retry — the marker stamp for once-per-crossing suppression
	// attaches only to this outcome.
	edgeOutcomeFired edgeOutcome = "fired"
	// edgeOutcomeSuppressed means the predicate held but a once-per-
	// crossing marker suppressed the firing.
	edgeOutcomeSuppressed edgeOutcome = "suppressed"
	// edgeOutcomeExhausted means the trigger fired at a boundary whose
	// repair budget was already spent — its note rendered instead of a
	// retry.
	edgeOutcomeExhausted edgeOutcome = "exhausted"
	// edgeOutcomeDegraded means the trigger escalated on a run that
	// can't ask the user — the resolve-side blocker report or logged
	// assumption is the terminal signal.
	edgeOutcomeDegraded edgeOutcome = "headless-degraded"
	// edgeOutcomeGated means the predicate held but the feature flag is
	// off — the flag gates acting, not measuring.
	edgeOutcomeGated edgeOutcome = "gated"
	// edgeOutcomeDeferred means the trigger lost the boundary's single
	// prompt slot to an escalation-family winner. Session-state
	// triggers ride the retry clone's deferred carrier to the next
	// boundary; step-bound losers record deferred and are dropped —
	// the escalate run's steps can't carry their evidence.
	edgeOutcomeDeferred edgeOutcome = "deferred"
	// edgeOutcomeCleared means the scan produced a trigger but resolve
	// dropped t.fire — a pending→clean verification — or a deferred
	// trigger's session-state evidence cleared during the escalation
	// turn.
	edgeOutcomeCleared edgeOutcome = "cleared"
	// edgeOutcomeCancelled covers the mid-scan-loop ctx.Err() case
	// (rows are written for every edge the loop didn't reach) and a
	// carried trigger landing on a StopTurn-ended boundary, where the
	// carrier can't take a retry slot.
	edgeOutcomeCancelled edgeOutcome = "cancelled"
)

// edgeFamily splits prompt contention: an escalation-family prompt wins
// the boundary's single retry slot outright and every other trigger
// defers. It lives on the trigger, not the edge — the stall edge's
// replan branch is retry-family (a model-directed turn that fires
// headless) while its escalate branch is escalation-family.
type edgeFamily int

const (
	edgeFamilyRetry edgeFamily = iota
	edgeFamilyEscalate
)

// EdgeFiringStore is the narrow sqlc handle the run-edge seam writes
// firing rows and reads absolute turn ordinals through — *db.Queries
// satisfies it. Firings are harness telemetry, not notebook concepts,
// so the handle is threaded directly instead of riding a service.
type EdgeFiringStore interface {
	InsertEdgeFiring(ctx context.Context, arg db.InsertEdgeFiringParams) (int64, error)
	CountUserMessagesBySession(ctx context.Context, sessionID string) (int64, error)
}

// edgeInput is the finished-run state every edge's scan inspects. It is
// assembled once at the run boundary — after Stream returns, before the
// queue dequeues.
type edgeInput struct {
	result           *fantasy.AgentResult
	currentAssistant *message.Message
	// stalled records that the run ended because the loop detector's
	// StopWhen fired — the signature repetition signal the edge layer
	// turns into a replan/escalation turn instead of a silent stop.
	stalled bool
	// turnSeq is the absolute ordinal of the run's initiating user
	// message among all user messages — the edge_firings key column.
	// Repair retries persist their prompts as user messages, so each
	// attempt's boundary gets a distinct ordinal.
	turnSeq int64
}

// edgeTrigger is one edge's scan output: the evidence a retry prompt or
// exhaustion note renders from. The verification edge fills the check
// fields, the todos edge fills plan, the stall edge fills assistant and
// report, and fire marks whether the edge still wants a repair turn
// after resolve ran.
type edgeTrigger struct {
	failed    []gateCheckOutcome
	pending   []gateCheckOutcome
	observed  []observedBash
	plan      []planVerdict
	assistant *message.Message
	report    string
	fire      bool
	// family decides prompt contention between firing triggers; the
	// zero value is retry.
	family edgeFamily
	// variant disambiguates branches of one edge in the firing record
	// — the stall edge records replan vs escalate.
	variant string
	// hint is the scan-level outcome when the predicate held but the
	// edge can't or shouldn't act: gated and suppressed record the row
	// and skip resolve entirely; degraded still runs resolve (the
	// blocker-report/assumption append is the degrade path's work).
	hint edgeOutcome
	// detail is the trigger_detail column — compact structured
	// evidence for the firing record.
	detail string
	// sessionState marks evidence that re-scans from session state
	// rather than the finished run's steps — a deferred trigger whose
	// session-state evidence cleared during the escalation turn
	// records cleared instead of standing for a retry slot.
	sessionState bool
	// steps/inputTokens stash step-bound run evidence for resolve and
	// prompt rendering — resolve doesn't receive edgeInput.
	steps       []fantasy.StepResult
	inputTokens int64
	// writeSet is the run's write-class call targets — scanned from
	// the steps because filetracker records writes as reads and bash
	// mutations bypass it entirely.
	writeSet []string
	// checkpoint/mapSlice are the stall replan's re-orientation
	// payload — the latest notebook checkpoint and a project-index
	// slice over the touched dirs, pulled in resolve.
	checkpoint string
	mapSlice   string
}

// deferredTrigger is a firing trigger that lost its boundary's prompt
// slot to an escalation-family winner. It rides the retry clone's
// deferred field and merges back at the next boundary — prompt section
// if a retry slot is free, exhaustion note if the budget ran out,
// terminal note if the boundary ended on StopTurn.
type deferredTrigger struct {
	edge    runEdge
	trigger *edgeTrigger
}

// contender is a firing trigger awaiting its recorded outcome — fresh
// from scan or merged back from the deferred carrier.
type contender struct {
	edge     runEdge
	t        *edgeTrigger
	prompted bool
}

// A runEdge is a deterministic transition evaluated at the run boundary.
// scan inspects the finished run and returns a trigger candidate, or nil
// when the edge cannot apply — a non-nil trigger with a gated or
// suppressed hint records the would-have-fired verdict without acting.
// resolve performs the edge's deterministic work — running checks,
// persisting outcomes — and runs even when the trigger will not produce
// a retry (resolved evidence must land regardless); it may clear t.fire
// when resolution shows nothing to repair. prompt renders the
// retry-prompt section; note renders the budget-exhausted terminal line.
type runEdge struct {
	name    string
	scan    func(ctx context.Context, call SessionAgentCall, in edgeInput) *edgeTrigger
	resolve func(ctx context.Context, call SessionAgentCall, t *edgeTrigger)
	prompt  func(t *edgeTrigger) string
	note    func(t *edgeTrigger, attempts int) string
}

// runEdgeSet is the evaluated order: verification and todos are the
// original gate halves; stall converts a loop-detector stop into a
// replan-or-escalate turn; burn-watch is the write-less spend tripwire.
// All firing retry-family edges merge into ONE retry prompt — two edges
// each enqueueing a turn would double every repair. Escalation-family
// triggers win the slot outright and the rest defer.
//
// The order is load-bearing: verification resolves before todos
// scans. scanTodosEdge reads check verdicts from STORED tool-result
// metadata — final state, not the mark-time snapshot — which is only
// true because resolveVerificationEdge runs pending checks and
// FlushAlls their outcomes first. Reordering or parallelizing the
// set would make every pending check read as "has not resolved" and
// false-block evidence-bound items. The escalation-family first-wins
// rule is also order-sensitive: first in this order takes the slot.
func (a *sessionAgent) runEdgeSet() []runEdge {
	return []runEdge{
		{
			name:    "verification",
			scan:    a.scanVerificationEdge,
			resolve: a.resolveVerificationEdge,
			prompt:  a.verificationRetrySection,
			note:    verificationExhaustNote,
		},
		{
			name:   "todos",
			scan:   a.scanTodosEdge,
			prompt: todosRetrySection,
			note:   todosExhaustNote,
		},
		{
			name:    "stall",
			scan:    a.scanStallEdge,
			resolve: a.resolveStallEdge,
			prompt:  stallPromptSection,
			note:    stallExhaustNote,
		},
		{
			name:    "burn-watch",
			scan:    a.scanBurnWatchEdge,
			resolve: a.resolveBurnWatchEdge,
			prompt:  burnWatchRetrySection,
			note:    burnWatchExhaustNote,
		},
	}
}

// runEdges evaluates every edge at the run boundary. Firing edges merge
// their evidence into one budgeted retry call, prepended ahead of queued
// prompts; every evaluated edge records an edge_firings row so "no row"
// keeps meaning "clean". Returns true when a retry was queued so the
// caller can suppress the finished notification.
func (a *sessionAgent) runEdges(ctx context.Context, call SessionAgentCall, in edgeInput) bool {
	if a.configStore == nil || in.result == nil || len(in.result.Steps) == 0 {
		return false
	}
	// A mutating call in the just-finished run resets the write-less
	// streak the burn-watch marker suppresses — the marker lives on
	// the call, so clearing it here keeps the clone unstamped.
	if call.burnWatched && runHasMutatingCall(in.result.Steps) {
		call.burnWatched = false
	}

	edges := a.runEdgeSet()
	clean := cleanStop(in)

	// Carried triggers merge back before any scan short-circuits,
	// keyed by edge so a fresh trigger can supersede them.
	carried := make(map[string]deferredTrigger, len(call.deferred))
	for _, d := range call.deferred {
		carried[d.edge.name] = d
	}

	if ctx.Err() != nil {
		// The boundary is already dead before any edge evaluated —
		// every edge records cancelled, carried triggers keep their
		// detail, and nothing enqueues.
		for _, edge := range edges {
			var t *edgeTrigger
			if d, ok := carried[edge.name]; ok {
				t = d.trigger
			}
			a.recordEdgeFiring(ctx, call, in, edge.name, t, edgeOutcomeCancelled)
		}
		return false
	}

	var contenders []contender
	// stillDeferred holds carried triggers that land on a non-clean
	// boundary — they can't take a retry slot here, but they ride the
	// retry clone forward if this boundary produces one, and only die
	// (cancelled row + terminal note) when it doesn't.
	var stillDeferred []deferredTrigger

	for i, edge := range edges {
		t := edge.scan(ctx, call, in)
		if t != nil && (t.hint == edgeOutcomeGated || t.hint == edgeOutcomeSuppressed) {
			// Flag-off or marker-suppressed: the predicate was still
			// evaluated — record the verdict and skip resolve, notes,
			// and the deferred merge entirely.
			a.recordEdgeFiring(ctx, call, in, edge.name, t, t.hint)
			delete(carried, edge.name)
			continue
		}
		if t != nil {
			// A fresh trigger supersedes any carried one for this edge.
			delete(carried, edge.name)
			if edge.resolve != nil {
				edge.resolve(ctx, call, t)
				if ctx.Err() != nil {
					// Cancelled mid-resolution: enqueue nothing and
					// record cancelled rows for this edge (with its
					// trigger detail), every pending contender, every
					// carried trigger, and every un-scanned edge so
					// no-row=clean stays true.
					a.recordEdgeFiring(ctx, call, in, edge.name, t, edgeOutcomeCancelled)
					a.recordCancelledBoundary(ctx, call, in, edges[i+1:], contenders, stillDeferred, carried)
					return false
				}
			}
			if !t.fire {
				outcome := edgeOutcomeCleared
				if t.hint == edgeOutcomeDegraded {
					outcome = edgeOutcomeDegraded
				}
				a.recordEdgeFiring(ctx, call, in, edge.name, t, outcome)
				continue
			}
			contenders = append(contenders, contender{edge: edge, t: t})
			continue
		}
		// The fresh scan produced nothing — a carried trigger may still
		// stand for this edge.
		d, ok := carried[edge.name]
		if !ok {
			continue
		}
		delete(carried, edge.name)
		if !clean {
			// A non-clean boundary (StopTurn, loop stop) offers no
			// retry slot — the trigger stays pending until the
			// boundary's own retry decision is known.
			stillDeferred = append(stillDeferred, d)
			continue
		}
		// Clean boundary, fresh scan nil: the deferral resolved
		// itself — for session-state evidence the fresh scan IS the
		// re-check (nil = cleared), and a step-bound trigger that
		// reached the carrier anyway has stale evidence it cannot
		// re-fire on.
		a.recordEdgeFiring(ctx, call, in, edge.name, d.trigger, edgeOutcomeCleared)
	}
	// Any carried trigger left over names an edge no longer in the
	// set — record it cleared rather than dropping it silently.
	for _, d := range carried {
		a.recordEdgeFiring(ctx, call, in, d.edge.name, d.trigger, edgeOutcomeCleared)
	}

	// collectDeadDeferred records the cancelled row for each carried
	// trigger whose boundary produced no retry to ride and returns
	// its terminal note edges/lines/reports for a single merged
	// exhaustion write.
	collectDeadDeferred := func() (noteEdges, notes, reports []string) {
		for _, d := range stillDeferred {
			a.recordEdgeFiring(ctx, call, in, d.edge.name, d.trigger, edgeOutcomeCancelled)
			if d.trigger.report != "" {
				reports = append(reports, d.trigger.report)
			}
			if d.edge.note == nil {
				continue
			}
			if n := d.edge.note(d.trigger, call.RepairAttempts); n != "" {
				notes = append(notes, n)
				noteEdges = append(noteEdges, d.edge.name)
			}
		}
		return noteEdges, notes, reports
	}
	if len(contenders) == 0 {
		edges2, notes2, reports2 := collectDeadDeferred()
		a.writeRepairExhaustion(ctx, call, in.currentAssistant, edges2, notes2, reports2)
		return false
	}

	// Terminal evidence collects per firing trigger regardless of who
	// wins the prompt slot — exhaustion shows every edge's note.
	var notes, noteEdges, reports []string
	for _, c := range contenders {
		if c.edge.note != nil {
			if n := c.edge.note(c.t, call.RepairAttempts); n != "" {
				notes = append(notes, n)
				noteEdges = append(noteEdges, c.edge.name)
			}
		}
		// A firing trigger can still carry a structured report (the
		// stall edge's blocker summary) — budget exhaustion shows the
		// terse note but must not lose it.
		if c.t.report != "" {
			reports = append(reports, c.t.report)
		}
	}

	if call.RepairAttempts >= maxRepairAttempts {
		for _, c := range contenders {
			a.recordEdgeFiring(ctx, call, in, c.edge.name, c.t, edgeOutcomeExhausted)
		}
		// Carried triggers die with the budget too — merge their
		// notes into the same terminal write.
		deadEdges, deadNotes, deadReports := collectDeadDeferred()
		noteEdges = append(noteEdges, deadEdges...)
		notes = append(notes, deadNotes...)
		reports = append(reports, deadReports...)
		// Budget exhausted: surface the terminal state on the final
		// assistant message — it is the last assistant message of the
		// run, so the text reaches RunComplete.Text for `crush run`.
		a.writeRepairExhaustion(ctx, call, in.currentAssistant, noteEdges, notes, reports)
		return false
	}

	// Escalation-family prompts win the single retry slot outright —
	// first in runEdgeSet order. Retry-family triggers keep
	// concatenating when no escalation fired; losers defer onto the
	// clone.
	winnerIdx := -1
	for i, c := range contenders {
		if c.t.family == edgeFamilyEscalate {
			winnerIdx = i
			break
		}
	}
	var winners, losers []contender
	if winnerIdx >= 0 {
		winners = contenders[winnerIdx : winnerIdx+1]
		losers = append(losers, contenders[:winnerIdx]...)
		losers = append(losers, contenders[winnerIdx+1:]...)
	} else {
		winners = contenders
	}

	var prompts []string
	for i := range winners {
		if p := winners[i].edge.prompt(winners[i].t); p != "" {
			prompts = append(prompts, p)
			winners[i].prompted = true
		}
	}
	if len(prompts) == 0 {
		// Defensive: every shipped firing edge renders a non-empty
		// prompt. An empty render records cleared rather than a
		// phantom firing; losers that lost the slot still record
		// deferred even though nothing carries them.
		for _, c := range winners {
			a.recordEdgeFiring(ctx, call, in, c.edge.name, c.t, edgeOutcomeCleared)
		}
		for _, c := range losers {
			a.recordEdgeFiring(ctx, call, in, c.edge.name, c.t, edgeOutcomeDeferred)
		}
		deadEdges, deadNotes, deadReports := collectDeadDeferred()
		a.writeRepairExhaustion(ctx, call, in.currentAssistant, deadEdges, deadNotes, deadReports)
		return false
	}

	// Clone the caller's call — ProviderOptions, sampling params,
	// NonInteractive, and OnAuthRefresh all carry through — with the
	// prompt replaced and the budget incremented. The same RunID keeps
	// the retry non-foldable and suppresses the premature RunComplete;
	// Accepted/acceptSeq are cleared so a cancel mark drops it.
	retry := call
	retry.Prompt = strings.Join(prompts, "\n")
	retry.RepairAttempts++
	retry.Accepted = nil
	retry.acceptSeq = 0
	retry.deferred = nil
	for _, c := range losers {
		// Only session-state evidence survives an escalation turn:
		// the escalate run's steps are new, so a step-bound trigger
		// riding the carrier would re-fire on stale evidence. Those
		// losers are dropped — still recorded deferred at the
		// boundary where they lost. Verification is the one loser
		// whose evidence is durable (failed verdicts persist on the
		// stored tool results), but re-firing on stored metadata
		// would re-litigate checks the escalation turn may already
		// have resolved — the verdict stays on the tool result the
		// transcript shows, so the drop is the right call.
		if c.t.sessionState {
			retry.deferred = append(retry.deferred, deferredTrigger{edge: c.edge, trigger: c.t})
		}
	}
	// Carried triggers that found no slot at this non-clean boundary
	// ride forward — they survive to the next clean boundary.
	retry.deferred = append(retry.deferred, stillDeferred...)

	for _, c := range winners {
		outcome := edgeOutcomeFired
		if !c.prompted {
			outcome = edgeOutcomeCleared
		}
		a.recordEdgeFiring(ctx, call, in, c.edge.name, c.t, outcome)
		// The marker stamps only on a fired burn-watch outcome — a
		// headless-degraded or deferred trigger leaves no carrier.
		if outcome == edgeOutcomeFired && c.edge.name == "burn-watch" {
			retry.burnWatched = true
		}
	}
	for _, c := range losers {
		a.recordEdgeFiring(ctx, call, in, c.edge.name, c.t, edgeOutcomeDeferred)
	}

	if ctx.Err() != nil {
		// Cancelled after the verdicts were recorded — do not enqueue
		// a retry behind clearQueueAndNotify's back.
		return false
	}
	mu := a.sessionMu(call.SessionID)
	mu.Lock()
	existing, _ := a.messageQueue.Get(call.SessionID)
	a.messageQueue.Set(call.SessionID, append([]SessionAgentCall{retry}, existing...))
	mu.Unlock()
	return true
}

// recordCancelledBoundary writes cancelled rows for the edges a
// mid-loop ctx.Err() prevented from reaching a verdict — the trigger
// whose resolve died, the pending contenders, the still-pending
// carried triggers, and every un-scanned edge. Writes run on a
// detached context: the insert must succeed even though the run
// context is dead.
func (a *sessionAgent) recordCancelledBoundary(ctx context.Context, call SessionAgentCall, in edgeInput, remaining []runEdge, pending []contender, deferred []deferredTrigger, carried map[string]deferredTrigger) {
	for _, c := range pending {
		a.recordEdgeFiring(ctx, call, in, c.edge.name, c.t, edgeOutcomeCancelled)
	}
	for _, d := range deferred {
		a.recordEdgeFiring(ctx, call, in, d.edge.name, d.trigger, edgeOutcomeCancelled)
	}
	for _, edge := range remaining {
		// A trigger parked on the carrier for an un-scanned edge
		// still carries its detail.
		var t *edgeTrigger
		if d, ok := carried[edge.name]; ok {
			t = d.trigger
		}
		a.recordEdgeFiring(ctx, call, in, edge.name, t, edgeOutcomeCancelled)
	}
}

// recordEdgeFiring writes one edge_firings row for an evaluated edge —
// INSERT OR IGNORE makes the write idempotent within a boundary, and
// the in-memory counter only advances on a real insert. Writes run on
// a detached context: a cancel landing between the mid-loop ctx.Err()
// check and the outcome writes must not lose the row — an unrecorded
// fired edge reads as clean and breaks the invariant.
func (a *sessionAgent) recordEdgeFiring(ctx context.Context, call SessionAgentCall, in edgeInput, edgeName string, t *edgeTrigger, outcome edgeOutcome) {
	if a.edgeStore == nil {
		return
	}
	turnSeq := in.turnSeq
	if turnSeq == 0 && call.RunStamp != 0 {
		// The user-message count failed — key the row off the run
		// stamp instead of collapsing every edge's rows onto
		// turn_seq=0 and letting INSERT OR IGNORE eat them. The
		// negative keeps it out of the real ordinal range.
		turnSeq = -int64(call.RunStamp)
	}
	params := db.InsertEdgeFiringParams{
		SessionID:      call.SessionID,
		TurnSeq:        turnSeq,
		Edge:           edgeName,
		RepairAttempts: int64(call.RepairAttempts),
		Outcome:        string(outcome),
		RunStamp:       int64(call.RunStamp),
	}
	if t != nil {
		params.Variant = t.variant
		params.TriggerDetail = t.detail
	}
	n, err := a.edgeStore.InsertEdgeFiring(context.WithoutCancel(ctx), params)
	if err != nil {
		slog.Warn("Failed to record edge firing", "error", err, "session_id", call.SessionID, "edge", edgeName)
		return
	}
	if n == 0 || a.edgeStats == nil {
		return
	}
	// Copy-on-write: SessionTelemetry iterates the stored map from
	// another goroutine, so the counter is never mutated in place.
	m, _ := a.edgeStats.Get(call.SessionID)
	next := make(map[string]int, len(m)+1)
	for k, v := range m {
		next[k] = v
	}
	next[edgeName+":"+string(outcome)]++
	a.edgeStats.Set(call.SessionID, next)
}

// edgeTurnSeq returns the absolute ordinal of the user message that
// initiated this run — the count of user-role messages taken just after
// the run's own message lands, before folded queued prompts add
// non-initiating user messages mid-run. Zero when the firing store is
// absent or the count fails.
func (a *sessionAgent) edgeTurnSeq(ctx context.Context, sessionID string) int64 {
	if a.edgeStore == nil {
		return 0
	}
	n, err := a.edgeStore.CountUserMessagesBySession(ctx, sessionID)
	if err != nil {
		slog.Warn("Failed to count user messages for edge firing turn sequence", "error", err, "session_id", sessionID)
		return 0
	}
	return n
}

// cleanStop reports whether the run ended on a genuine completion claim:
// the terminal step finished with reason stop, and no tool result halted
// the turn (a hook halt, permission denial, or question-tool answer is
// not a completion claim and is never gated).
func cleanStop(in edgeInput) bool {
	if in.result == nil || len(in.result.Steps) == 0 {
		return false
	}
	terminal := in.result.Steps[len(in.result.Steps)-1]
	if terminal.FinishReason != fantasy.FinishReasonStop {
		return false
	}
	for _, tr := range terminal.Content.ToolResults() {
		if tr.StopTurn {
			return false
		}
	}
	return true
}

// writeRepairExhaustion appends the merged exhaustion notes to the
// final assistant message so the terminal state is visible in the TUI
// and reaches RunComplete.Text for `crush run`. The label names the
// edges whose budget ran out.
func (a *sessionAgent) writeRepairExhaustion(ctx context.Context, call SessionAgentCall, currentAssistant *message.Message, edgeNames, notes, reports []string) {
	if currentAssistant == nil || len(notes) == 0 {
		return
	}
	for i, name := range edgeNames {
		edgeNames[i] = strings.ToUpper(name[:1]) + name[1:]
	}
	currentAssistant.AppendContent("\n\n" + strings.Join(edgeNames, ", ") + ": " + strings.Join(notes, " "))
	for _, report := range reports {
		currentAssistant.AppendContent("\n\n" + report)
	}
	// Detached like the firing rows: the terminal line must land even
	// when the run context is already dead.
	ctx = context.WithoutCancel(ctx)
	if err := a.messages.Update(ctx, *currentAssistant); err != nil {
		slog.Error("Failed to record repair exhaustion", "error", err, "session_id", call.SessionID)
	} else if err := a.messages.FlushAll(ctx); err != nil {
		// Same flush race as the outcome writes: a fast notebook
		// goroutine would generate this turn's entries without
		// the exhaustion line.
		slog.Error("Failed to flush repair exhaustion", "error", err, "session_id", call.SessionID)
	}
}

// --- verification edge ---

// scanVerificationEdge collects this run's write-tool verification
// entries. Pending checks start unfired: resolve decides whether any of
// them resolve to failed.
func (a *sessionAgent) scanVerificationEdge(_ context.Context, _ SessionAgentCall, in edgeInput) *edgeTrigger {
	if !cleanStop(in) {
		return nil
	}
	failed, pending, observed := scanVerification(in.result.Steps)
	if len(failed) == 0 && len(pending) == 0 {
		return nil
	}
	t := &edgeTrigger{
		failed:   failed,
		pending:  pending,
		observed: observed,
		fire:     len(failed) > 0,
	}
	t.detail = verificationDetail(t)
	return t
}

// verificationDetail is the trigger_detail for a verification firing —
// the failed/pending counts at record time. resolve recomputes it so
// the row reflects post-resolution state.
func verificationDetail(t *edgeTrigger) string {
	return fmt.Sprintf("failed=%d pending=%d", len(t.failed), len(t.pending))
}

// resolveVerificationEdge runs the pending checks harness-side,
// persists the resolved outcomes onto the stored tool results, and
// refires the edge when any pending check resolved to failed.
func (a *sessionAgent) resolveVerificationEdge(ctx context.Context, call SessionAgentCall, t *edgeTrigger) {
	if len(t.pending) == 0 {
		return
	}
	unique := map[string]bool{}
	for _, p := range t.pending {
		unique[p.check.Identity()] = true
	}
	a.notifyVerifying(call, len(unique))
	resolved := a.runGateChecks(ctx, a.configStore.WorkingDir(), t.pending, t.observed)
	if ctx.Err() != nil {
		// Cancelled mid-gate: leave pending entries pending (the
		// notebook maps them to unverified) rather than writing
		// failed verdicts for checks that never completed.
		return
	}
	for i := range t.pending {
		out, ok := resolved[t.pending[i].check.Identity()]
		if !ok {
			continue
		}
		t.pending[i].check.State = out.state
		t.pending[i].check.Detail = out.detail
		t.pending[i].output = out.output
	}
	// Persist the resolved states onto the originating results, then
	// flush: the post-run goroutine's List reads storage directly and
	// would otherwise miss a debounced metadata-only update.
	a.writeVerificationOutcomes(ctx, call.SessionID, t.pending)
	if err := a.messages.FlushAll(ctx); err != nil {
		slog.Error("Failed to flush verification outcomes", "error", err, "session_id", call.SessionID)
	}
	for _, p := range t.pending {
		if p.check.State == message.VerificationFailed {
			t.failed = append(t.failed, p)
		}
	}
	t.fire = len(t.failed) > 0
	t.detail = verificationDetail(t)
}

// Repair-prompt leading literals. Repair turns are enqueued inside the
// firing `crush run` process, so they never start a new process-turn —
// the eval analyzer (internal/eval/analyze.go) fingerprints these
// prefixes to segment turns when the trajectory isn't known.
const (
	verificationRetryPrefix = "Verification failed."
	todosRetryPrefix        = "The todo list still has"
	stallRetryPrefix        = "The previous attempt was stopped"
	// stallReplanPrefix must stay distinct from stallRetryPrefix —
	// the eval analyzer fingerprints the leading literal to split the
	// stall edge's replan/escalate variants.
	stallReplanPrefix = "The previous attempt stalled"
	burnWatchPrefix   = "This run spent"
)

// RepairPromptPrefixes is every repair-prompt leading literal — the
// eval analyzer fingerprints these to distinguish harness-authored
// repair turns from process-boundary user messages.
var RepairPromptPrefixes = []string{
	verificationRetryPrefix,
	todosRetryPrefix,
	stallRetryPrefix,
	stallReplanPrefix,
	burnWatchPrefix,
}

// failedCheckGroups merges failed entries minted by one write into a
// single renderable failure — a write whose diagnostics delta broke
// three files recorded three per-file entries but is one broken write
// sharing one tool output, not three checks.
func failedCheckGroups(failed []gateCheckOutcome) [][]gateCheckOutcome {
	var order []string
	groups := map[string][]gateCheckOutcome{}
	for _, f := range failed {
		key := f.toolCallID + "\x00" + f.check.Check
		if _, ok := groups[key]; !ok {
			order = append(order, key)
		}
		groups[key] = append(groups[key], f)
	}
	out := make([][]gateCheckOutcome, 0, len(order))
	for _, k := range order {
		out = append(out, groups[k])
	}
	return out
}

// verificationRetrySection renders the failed checks' raw output
// (truncated to the tool-result cap) — evidence the turn claimed done
// prematurely. Per-file paths render workingDir-relative, matching the
// plan reasons in the same prompt.
func (a *sessionAgent) verificationRetrySection(t *edgeTrigger) string {
	if len(t.failed) == 0 {
		return ""
	}
	workingDir := ""
	if a.configStore != nil {
		workingDir = a.configStore.WorkingDir()
	}
	var b strings.Builder
	b.WriteString(verificationRetryPrefix + " The following check(s) did not pass — fix the underlying issue; do not restate success.\n")
	for _, group := range failedCheckGroups(t.failed) {
		f := group[0]
		fmt.Fprintf(&b, "\n<check name=%q>\n", f.check.Check)
		// Per-file entries name each affected path, then the shared
		// tool output renders once — for a single-entry group this is
		// the only place the attributed path appears.
		for _, g := range group {
			if g.check.Path == "" {
				continue
			}
			b.WriteString(relPlanPath(workingDir, g.check.Path))
			if g.check.Detail != "" {
				b.WriteString(": " + g.check.Detail)
			}
			b.WriteString("\n")
		}
		out := f.output
		if out == "" {
			out = f.check.Detail
		}
		b.WriteString(tools.TruncateOutput(out))
		b.WriteString("\n</check>\n")
	}
	return b.String()
}

// verificationExhaustNote renders the budget-exhausted line for checks
// that never went green.
func verificationExhaustNote(t *edgeTrigger, attempts int) string {
	if len(t.failed) == 0 {
		return ""
	}
	last := t.failed[len(t.failed)-1]
	headline := last.check.Detail
	if headline == "" {
		headline = firstLine(last.output)
	}
	return fmt.Sprintf("%d check(s) still failing after %d attempt(s). Last failure: %s",
		len(failedCheckGroups(t.failed)), attempts, headline)
}

// --- todos edge ---

// scanTodosEdge fires when a clean stop leaves plan items unresolved —
// open marks, or completed marks the evidence contradicts. Done-ness
// evaluates on final state: a check green at mark-time that regressed
// on a later write reopens the item here.
func (a *sessionAgent) scanTodosEdge(ctx context.Context, call SessionAgentCall, in edgeInput) *edgeTrigger {
	if !cleanStop(in) {
		return nil
	}
	open := a.planVerdicts(ctx, call.SessionID)
	if len(open) == 0 {
		return nil
	}
	return &edgeTrigger{
		plan: open,
		fire: true,
		// Plan evidence re-scans from session state, so a deferred
		// todos trigger records cleared when the escalation turn
		// closed the items.
		sessionState: true,
		detail:       fmt.Sprintf("open=%d", len(open)),
	}
}

// todosRetrySection renders the unresolved plan items left behind by a
// turn that reported done: ready work first (DependsOn satisfied), then
// evidence-blocked items with their reasons, then dep-blocked items —
// the ready-before-blocked ordering DependsOn's first consumer reads.
func todosRetrySection(t *edgeTrigger) string {
	if len(t.plan) == 0 {
		return ""
	}
	keyByID := map[string]string{}
	for _, v := range t.plan {
		if v.item.Key != "" {
			keyByID[v.item.ID] = v.item.Key
		}
	}
	var b strings.Builder
	b.WriteString(todosRetryPrefix + " unresolved item(s) — a turn is not done while its declared tasks are open:\n")
	const maxListedTodos = 20
	listed := 0
	render := func(v planVerdict, suffix string) bool {
		if listed >= maxListedTodos {
			return false
		}
		listed++
		fmt.Fprintf(&b, "- [%s] %s%s\n", v.item.Status, v.item.Content, suffix)
		return true
	}
	for _, v := range t.plan {
		if v.state == planOpen && v.ready {
			render(v, "")
		}
	}
	for _, v := range t.plan {
		if v.state == planEvidenceBlocked {
			render(v, " — marked completed but "+v.reason)
		}
	}
	for _, v := range t.plan {
		if v.state == planOpen && !v.ready {
			var names []string
			for _, dep := range v.item.DependsOn {
				if k := keyByID[dep]; k != "" {
					names = append(names, k)
				}
			}
			suffix := ""
			if len(names) > 0 {
				suffix = " (blocked by: " + strings.Join(names, ", ") + ")"
			}
			render(v, suffix)
		}
	}
	if len(t.plan) > listed {
		fmt.Fprintf(&b, "- … and %d more\n", len(t.plan)-listed)
	}
	b.WriteString("Finish the remaining work, or reconcile the list with the todos tool (mark genuinely done items completed; drop abandoned ones). An evidence-blocked item needs its evidence resolved — run the check or rebind it — not a repeated completed mark. Do not report the task finished while the list says otherwise.\n")
	return b.String()
}

// todosExhaustNote renders the budget-exhausted line for unresolved
// plan items.
func todosExhaustNote(t *edgeTrigger, attempts int) string {
	if len(t.plan) == 0 {
		return ""
	}
	return fmt.Sprintf("%d todo item(s) still unresolved after %d attempt(s).",
		len(t.plan), attempts)
}

// --- stall edge ---

// scanStallEdge fires when the loop detector stopped the run — the
// repeated-signature signal is evidence of no progress, and a silent
// stop is the wrong action for it. The edge's prompt branches on the
// shared repair budget: the first stall produces a model-directed
// replan turn (retry family — it fires headless, no question tool
// needed); a stall on a repair turn escalates to the interactive
// question (escalation family), degrading to the blocker report when
// the run can't ask. Opt-in via options.ambiguity_clarification —
// flag-off records a gated row but does not act.
func (a *sessionAgent) scanStallEdge(_ context.Context, call SessionAgentCall, in edgeInput) *edgeTrigger {
	// in.stalled covers the detector trip; the fallback re-scan covers
	// the context-pressure mask — the summarize StopWhen runs before
	// the detector and short-circuits it, so a stalled run that also
	// crossed the context threshold ends with loopStopped false.
	sig, tool, repeats := repeatedToolSignature(in.result.Steps, loopDetectionWindowSize, loopDetectionMaxRepeats)
	if !in.stalled && sig == "" {
		return nil
	}
	t := &edgeTrigger{
		assistant: in.currentAssistant,
		report:    stallBlockerReport(tool),
		detail:    fmt.Sprintf("tool=%q repeats=%d signature=%s", tool, repeats, sig),
		steps:     in.result.Steps,
		writeSet:  stepWriteSet(in.result.Steps),
	}
	if call.RepairAttempts == 0 {
		// First stall — the cheap model-directed replan. "First" means
		// first repair slot, not first stall ever: a run that burned a
		// verification repair and then stalls skips straight to
		// escalation. The variant lands before the gate so a gated row
		// still records which branch would have fired.
		t.variant = "replan"
		t.family = edgeFamilyRetry
	} else {
		t.variant = "escalate"
		t.family = edgeFamilyEscalate
	}
	if !a.ambiguityClarification {
		t.hint = edgeOutcomeGated
		return t
	}
	if t.variant == "replan" {
		t.fire = true
		return t
	}
	t.fire = a.interactive && !call.NonInteractive && a.hasTool(tools.QuestionToolName)
	if !t.fire {
		t.hint = edgeOutcomeDegraded
	}
	return t
}

// resolveStallEdge performs the edge's resolve-side work. For the
// replan branch it pulls the re-orientation payload — the session's
// latest checkpoint and, with project_index on, a map slice over the
// run's touched dirs — so the forced turn starts re-oriented at zero
// model cost. For the headless degrade it lands the structured blocker
// report on the final assistant message — it reaches RunComplete.Text
// for `crush run` — instead of a retry nobody can answer.
func (a *sessionAgent) resolveStallEdge(ctx context.Context, call SessionAgentCall, t *edgeTrigger) {
	if t.variant == "replan" && t.fire {
		a.pullStallHandoff(ctx, call, t)
		return
	}
	if t.fire || t.assistant == nil || t.report == "" {
		return
	}
	t.assistant.AppendContent("\n\n" + t.report)
	if err := a.messages.Update(ctx, *t.assistant); err != nil {
		slog.Error("Failed to record stall blocker report", "error", err, "session_id", call.SessionID)
	} else if err := a.messages.FlushAll(ctx); err != nil {
		slog.Error("Failed to flush stall blocker report", "error", err, "session_id", call.SessionID)
	}
}

// pullStallHandoff loads the replan turn's re-orientation payload.
// The run-end checkpoint generates asynchronously after runEdges, so
// the latest committed checkpoint is always the previous turn's (or a
// mid-run) one — never this run's. Every pull is best-effort: a
// notebook-off or index-off session degrades to evidence-only.
func (a *sessionAgent) pullStallHandoff(ctx context.Context, call SessionAgentCall, t *edgeTrigger) {
	if a.notebook != nil {
		if entries, err := a.notebook.GetEntries(ctx, call.SessionID); err == nil {
			latest := map[string]bool{}
			for _, id := range notebook.LatestCheckpointIDs(entries) {
				latest[id] = true
			}
			// Boundary and session granularities can both be "latest"
			// — take the newest by (turn, event) so the pick doesn't
			// depend on GetEntries ordering.
			var best *notebook.Entry
			for i := range entries {
				e := &entries[i]
				if !latest[e.ID] {
					continue
				}
				if best == nil ||
					e.TurnNumber > best.TurnNumber ||
					(e.TurnNumber == best.TurnNumber && e.EventNumber > best.EventNumber) {
					best = e
				}
			}
			if best != nil {
				// Prefer the uncompressed text — the replan prompt
				// gets one shot at re-orientation, matching
				// buildCheckpointInput's precedence.
				t.checkpoint = best.EntryTextFull
				if t.checkpoint == "" {
					t.checkpoint = best.EntryText
				}
			}
		}
	}
	opts := a.configStore.Config().Options
	if opts == nil || !opts.ProjectIndexEnabled() {
		return
	}
	workingDir := a.configStore.WorkingDir()
	dirs := relTouchedDirs(workingDir, t.steps)
	if len(dirs) == 0 {
		return
	}
	svc := index.Shared(opts.DataDirectory, workingDir)
	if err := svc.Ready(); err != nil {
		return
	}
	var b strings.Builder
	for _, dir := range dirs {
		slice, err := svc.Subtree(ctx, dir, 150)
		if err != nil || slice == "" {
			continue
		}
		// Subtree reports guidance ("Path ... is absolute", "No
		// indexed files under ...") as a nil-error string — never a
		// map. Keep those out of the prompt.
		if strings.HasPrefix(slice, "Path ") || strings.HasPrefix(slice, "No indexed files") {
			continue
		}
		b.WriteString(slice)
		b.WriteString("\n")
	}
	t.mapSlice = strings.TrimSpace(b.String())
}

// relTouchedDirs is touchedDirs' output translated into the
// project-relative form Subtree requires: absolute dirs under the
// working dir are relativized, dirs outside the root (or that fail to
// relativize) are skipped.
func relTouchedDirs(workingDir string, steps []fantasy.StepResult) []string {
	var out []string
	for _, dir := range touchedDirs(steps) {
		if filepath.IsAbs(dir) {
			rel, err := filepath.Rel(workingDir, dir)
			if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				continue
			}
			dir = rel
		}
		out = append(out, dir)
	}
	return out
}

// stepWriteSet scans a run's steps for write-class call targets —
// the file_path inputs of mutating tool calls. Bash mutations carry
// no structured target, so they contribute nothing here.
func stepWriteSet(steps []fantasy.StepResult) []string {
	seen := map[string]bool{}
	var out []string
	for _, step := range steps {
		for _, tc := range step.Content.ToolCalls() {
			if !toolclass.IsMutatingCall(tc.ToolName, tc.Input) {
				continue
			}
			var params struct {
				FilePath string `json:"file_path"`
				Path     string `json:"path"`
			}
			if json.Unmarshal([]byte(tc.Input), &params) != nil {
				continue
			}
			p := cmp.Or(params.FilePath, params.Path)
			if p != "" && !seen[p] {
				seen[p] = true
				out = append(out, p)
			}
		}
	}
	return out
}

// touchedDirs is the deduped directory set of every file-path-bearing
// call in the run — the map slice's render targets.
func touchedDirs(steps []fantasy.StepResult) []string {
	seen := map[string]bool{}
	var out []string
	for _, step := range steps {
		for _, tc := range step.Content.ToolCalls() {
			var params struct {
				FilePath string `json:"file_path"`
				Path     string `json:"path"`
			}
			if json.Unmarshal([]byte(tc.Input), &params) != nil {
				continue
			}
			p := cmp.Or(params.FilePath, params.Path)
			if p == "" {
				continue
			}
			dir := filepath.Dir(p)
			if dir == "." {
				dir = ""
			}
			if !seen[dir] {
				seen[dir] = true
				out = append(out, dir)
			}
		}
	}
	return out
}

// stallBlockerReport builds the structured blocker report for a
// loop-stopped run: what was tried, what's blocking, and the minimal
// way forward. The tool name comes from the signature-returning
// detector — the exact repeated interaction, not the window's dominant
// name.
func stallBlockerReport(toolName string) string {
	tool := "the same tool call"
	if toolName != "" {
		tool = fmt.Sprintf("%q", toolName)
	}
	return fmt.Sprintf("Stopped: %s repeated without making progress. "+
		"What's blocking: identical input produced identical output, so no state changed. "+
		"Ways forward: restate the task with a concrete file or command, narrow the scope, or grant the missing access.",
		tool)
}

// stallPromptSection dispatches the stall edge's prompt on the
// trigger's variant: replan is a model-directed retry, escalate is the
// interactive question turn.
func stallPromptSection(t *edgeTrigger) string {
	if t.variant == "replan" {
		return stallReplanSection(t)
	}
	return stallRetrySection(t)
}

// stallReplanSection renders the model-directed replan prompt for a
// first-stall run — the blocker report's evidence feeds the prompt
// instead of being computed and dropped, and the handoff payload
// re-orients the forced turn at zero model cost.
func stallReplanSection(t *edgeTrigger) string {
	var b strings.Builder
	b.WriteString(stallReplanPrefix + " — the same tool calls repeated without making progress.\n\n")
	if t.report != "" {
		b.WriteString(t.report + "\n\n")
	}
	if len(t.writeSet) > 0 {
		b.WriteString("Files written before the stall:\n")
		for _, p := range t.writeSet {
			b.WriteString("- " + p + "\n")
		}
		b.WriteString("\n")
	}
	if t.checkpoint != "" {
		b.WriteString("Latest session checkpoint:\n" + t.checkpoint + "\n\n")
	}
	if t.mapSlice != "" {
		b.WriteString("Project map over the touched directories:\n" + t.mapSlice + "\n\n")
	}
	b.WriteString("State which assumption failed in one line, then take a materially different approach — " +
		"a different file, command, or strategy. Do not repeat the same calls; if the task is genuinely " +
		"blocked, name what is missing instead of retrying.")
	return b.String()
}

// stallRetrySection renders the escalation prompt for a loop-stopped
// interactive run — what was tried, what's blocking, options with
// tradeoffs via the question tool's single_choice.
func stallRetrySection(_ *edgeTrigger) string {
	return stallRetryPrefix + ": the same tool calls repeated without making progress. " +
		"Do not retry the same approach. Escalate with ONE question-tool call — a single_choice question " +
		"naming what you tried and what is blocking, with each choice describing the tradeoff of that " +
		"way forward. If the user cannot answer, proceed with your stated-best option."
}

// stallExhaustNote renders the budget-exhausted line for a run that
// stalled again after its repair turns.
func stallExhaustNote(_ *edgeTrigger, attempts int) string {
	return fmt.Sprintf("Stopped after repeating identical tool calls; %d repair attempt(s) exhausted.", attempts)
}

// --- burn-watch edge ---

// Burn-watch thresholds: generous tripwires, not a governor — the edge
// surfaces write-less spend, it doesn't throttle it. The token arm is
// conjunctive: InputTokens counts the re-sent prompt per step, so on a
// non-caching provider any few-step turn in a mature session crosses a
// flat threshold — the step floor keeps the arm honest.
const (
	// burnWatchStepsThreshold fires on step count alone.
	burnWatchStepsThreshold = 30
	// burnWatchMinSteps is the step floor for the token arm.
	burnWatchMinSteps = 10
	// burnWatchInputTokens is the conjunctive token arm's spend
	// threshold — cache-read tokens are excluded (cached tokens are
	// ~free and would trip the tripwire far too early).
	burnWatchInputTokens = 200_000
)

// scanBurnWatchEdge fires when a clean-stopping, non-stalled run spent
// heavily while producing zero mutating calls — monotone progress that
// never writes is the "60M tokens and I didn't notice" failure as a
// declared transition. Mutation uses the shared IsMutatingCall
// vocabulary (write tools + download + mutating bash/redirects), not
// the narrow write-tool map — a run that wrote only via `bash > f`
// must not false-trip. Opt-in via options.ambiguity_clarification;
// the once-per-crossing marker suppresses a re-fire inside one
// run-chain.
func (a *sessionAgent) scanBurnWatchEdge(_ context.Context, call SessionAgentCall, in edgeInput) *edgeTrigger {
	if !cleanStop(in) || in.stalled {
		return nil
	}
	steps := len(in.result.Steps)
	mutating, explorations, filesRead := scanStepCallClasses(in.result.Steps)
	if mutating > 0 {
		return nil
	}
	tokens := in.result.TotalUsage.InputTokens
	if steps <= burnWatchStepsThreshold &&
		(steps < burnWatchMinSteps || tokens <= burnWatchInputTokens) {
		return nil
	}
	t := &edgeTrigger{
		assistant:   in.currentAssistant,
		family:      edgeFamilyEscalate,
		detail:      fmt.Sprintf("steps=%d input_tokens=%d explorations=%d files_read=%d writes=0", steps, tokens, explorations, filesRead),
		steps:       in.result.Steps,
		inputTokens: tokens,
	}
	if !a.ambiguityClarification {
		t.hint = edgeOutcomeGated
		return t
	}
	if call.burnWatched {
		t.hint = edgeOutcomeSuppressed
		return t
	}
	t.fire = a.interactive && !call.NonInteractive && a.hasTool(tools.QuestionToolName)
	if !t.fire {
		t.hint = edgeOutcomeDegraded
	}
	return t
}

// resolveBurnWatchEdge performs the edge's headless degrade: a run that
// can't ask gets a logged assumption appended to the final assistant
// message — the degrade path has no carrier, so no marker stamps and a
// later write-less run may append another note.
func (a *sessionAgent) resolveBurnWatchEdge(ctx context.Context, call SessionAgentCall, t *edgeTrigger) {
	if t.fire || t.hint != edgeOutcomeDegraded || t.assistant == nil {
		return
	}
	t.assistant.AppendContent("\n\n" + burnWatchAssumption(t))
	if err := a.messages.Update(ctx, *t.assistant); err != nil {
		slog.Error("Failed to record burn-watch assumption", "error", err, "session_id", call.SessionID)
	} else if err := a.messages.FlushAll(ctx); err != nil {
		slog.Error("Failed to flush burn-watch assumption", "error", err, "session_id", call.SessionID)
	}
}

// scanStepCallClasses sweeps a run's steps for the call-class counts
// burn-watch evidence needs: mutating calls, non-mutating
// explorations, and file reads — all classified by the shared
// vocabulary so the write boundary and the tripwire agree.
func scanStepCallClasses(steps []fantasy.StepResult) (mutating, explorations, filesRead int) {
	for _, step := range steps {
		for _, tc := range step.Content.ToolCalls() {
			if toolclass.IsMutatingCall(tc.ToolName, tc.Input) {
				mutating++
				continue
			}
			explorations++
			if toolclass.ReadToolNames[tc.ToolName] {
				filesRead++
			}
		}
	}
	return mutating, explorations, filesRead
}

// runHasMutatingCall reports whether the finished run made a mutating
// call — the write that clears the burn-watch marker on a stamped
// run-chain.
func runHasMutatingCall(steps []fantasy.StepResult) bool {
	for _, step := range steps {
		for _, tc := range step.Content.ToolCalls() {
			if toolclass.IsMutatingCall(tc.ToolName, tc.Input) {
				return true
			}
		}
	}
	return false
}

// burnWatchRetrySection renders the escalation prompt for a write-less
// high-spend run — a single_choice question reporting the spend and
// asking how to proceed.
func burnWatchRetrySection(t *edgeTrigger) string {
	return fmt.Sprintf("%s %s over %d steps without producing any file writes. "+
		"Escalate with ONE question-tool call — a single_choice question reporting the spend and asking "+
		"how to proceed, with choices covering continue as-is, replan toward a concrete write target, and "+
		"stop. If the user cannot answer, proceed with your stated-best option.",
		burnWatchPrefix, humanTokens(t.inputTokens), len(t.steps))
}

// burnWatchAssumption is the headless degrade line — the run spent
// write-less budget and couldn't ask, so the visible assumption is the
// terminal signal.
func burnWatchAssumption(t *edgeTrigger) string {
	return fmt.Sprintf("Burn-watch: %s over %d steps with no file writes — "+
		"assuming the exploration was intended and continuing.",
		humanTokens(t.inputTokens), len(t.steps))
}

// burnWatchExhaustNote renders the budget-exhausted line for a
// write-less spend run — needed for the interactive exhaustion path
// where a firing edge alongside carries the signal to the terminal
// message.
func burnWatchExhaustNote(t *edgeTrigger, attempts int) string {
	return fmt.Sprintf("Spent %s over %d steps with no writes after %d attempt(s).",
		humanTokens(t.inputTokens), len(t.steps), attempts)
}

// humanTokens renders a token count compactly for prompt and note
// text — 200000 becomes "200K tokens".
func humanTokens(n int64) string {
	if n >= 1000 {
		return fmt.Sprintf("%dK tokens", n/1000)
	}
	return fmt.Sprintf("%d tokens", n)
}
