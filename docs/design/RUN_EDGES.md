# Run Edges — Deterministic Transitions at the Run Boundary

> **Status:** Partially shipped. The `runEdge` seam, verification,
> todos-reconcile, `escalate-human`, and `phase-confirm` landed via
> #43 — this doc's remaining work is the unimplemented catalog rows
> (stall-replan, burn-watch, summarize-continue, join-subagents) and
> edge-firing records. Split from `HARNESS_TOPOLOGY.md` — that doc
> is the analysis of why this shape; this doc is the work.
> **Ship when:** per edge — the catalog names each trigger.
> **Measured by:** edge-firing records per turn (PR 3 below);
> `EVAL_HARNESS` trajectory assertions on named transitions.

## Goal

Make every deterministic transition in the agent loop a declared
edge — named, budgeted, aggregated — instead of hand-rolled
pipeline code at one hardcoded site. New transitions become
declarations on the seam; the loop kernel stays the router for
everything the edges don't claim.

## The edge type

Promote the gate's generic half into a declarative edge:

```go
// A runEdge is a deterministic transition evaluated at the run
// boundary, after Stream returns and before the queue dequeues.
type runEdge struct {
	name string
	// scan inspects the finished run; nil trigger means no fire.
	scan func(ctx context.Context, call SessionAgentCall, in edgeInput) *edgeTrigger
	// resolve runs the deterministic part (shell checks, ledger
	// queries). May be nil for prompt-only edges.
	resolve func(ctx context.Context, call SessionAgentCall, t *edgeTrigger)
	// prompt builds the retry message from resolved evidence.
	prompt func(t *edgeTrigger) string
}
```

Evaluated in sequence at the `agent.go:1550` site (`runEdges` —
the seam itself shipped in #43; this doc's remaining work is the
unimplemented catalog rows). Aggregation rule,
already precedented: all firing edges merge evidence into **one**
retry prompt, one prepend, one budget increment — two edges each
enqueueing a turn would double every repair. Budget becomes a shared
repair counter on `SessionAgentCall` (rename `VerificationAttempts`
→ `RepairAttempts`; per-edge sub-budgets only if a thrash pattern
demands it). `maxRepairAttempts = 2` (`run_edges.go:19`)
stays the initial shared bound.

**Field traps are inherited, not optional.** Every edge obeys the
constraints listed in `HARNESS_TOPOLOGY.md` — run-boundary only,
RunID fold exemption, budgets on `SessionAgentCall`,
flush-before-notebook, union stored metadata. They are not restated
here because the list must have exactly one home.

## Catalog

| Edge               | Trigger                                            | Status today                                                 |
| ------------------ | -------------------------------------------------- | ------------------------------------------------------------ |
| verification       | failed/pending checks                              | implemented (the extraction source)                          |
| todos-reconcile    | open plan items at clean stop                      | implemented (same site)                                      |
| stall-replan       | loop-detector / no-progress                        | new — inserts a model replan _before_ the shipped escalation |
| escalate-human     | loop-detector stop                                 | implemented (#43) — question turn                            |
| phase-confirm      | first write-class call after ≥N exploration events | implemented (#43) — plan gate                                |
| join-subagents     | outstanding dispatch ledger                        | lives in `BACKGROUND_SUBAGENTS.md`                           |
| summarize-continue | context pressure at run end                        | new — reframes auto-summarize                                |
| burn-watch         | run spent >T tokens with zero write-class calls    | new — the unnoticed-spend tripwire                           |

The stall-replan row is the tell that this abstraction earns its
keep: `hasRepeatedToolCalls` today _stops_ a thrashing turn — the
information is right and the action is wrong. As an edge it becomes
"evidence of no progress → one replan turn within budget."

**Edges can target the user, not just the model.** Two rows —
`escalate-human`, `phase-confirm` — resolve into a `question`-tool
turn instead of a retry prompt: the trigger and evidence collection
stay deterministic, but the boundary slot is spent asking the human
rather than prepending for the model. This is the human-in-the-loop
half of the calibrated-autonomy work (#39): the harness decides
_when_ to ask (loop-stop with no progress, first-write boundary on
a large-scope plan), the prompt decides how. A spent budget produces
a terminal note, not a question — see the exhaustion path.
Escalation frequency is budgeted like repairs — one per distinct
blocker — because an unbudgeted escalation edge is nagging with a
type signature. Headless (`crush run`) degrades both rows to
"state the blocker + chosen option, proceed": the edge still fires,
the question becomes a logged assumption.

**Ordering and scope.** `runEdgeSet` order is load-bearing —
verification's resolve must precede todos' scan (resolved checks
feed the done-definition); new rows declare where they sit rather
than appending blindly. Edges evaluate on the session's own run;
for child/task sessions, firing follows the checkpoint precedent —
the machinery is shared, the decision is per-edge (a child's stall
is the parent's problem via the child's report, not a second
question turn).

`escalate-human` ships in the same series as #39's clause work, with
the `runEdge` extraction folded in as its enabling step — the seam
lands first within the series, the edge second. The ordering is not
scheduling convenience — it is the anti-accretion argument applied
to itself. There is exactly one hardcoded edge site today
(`runVerificationGate`); adding a second hand-rolled transition
beside it — its own trigger scan, budget accounting, clone-call
enqueue, RunID handling — would be the accretion pattern the edge
type exists to prevent. Once the seam exists, the edge is a
declaration, not a bolt-on. `phase-confirm` needs no such
predecessor: its trigger is not a run-boundary transition at all but
a mid-run tool-call gate at the first `writeToolNames` call — a
different seam (permissions/hook territory), unblocked today.

With the project index (`CONTEXT_PREFETCH.md`, #34), `resolve`
gains a deterministic evidence source: a repair prompt can carry a
`map` slice over the run's touched dirs (the write set is scanned
from `result.Steps` — filetracker records writes as reads too, so
it can't distinguish), so the
forced turn starts re-oriented at zero model cost
instead of the model re-gathering in its first calls back — the
re-read loop the index exists to kill, closed at the run boundary
rather than left to the model remembering a tool exists.

Mid-step control is explicitly out of scope for edges — hooks and
permissions own that boundary (`hooked_tool.go`); edges only ever
fire between turns.

## Edge specs — the unimplemented rows

### stall-replan

The stop-only signal was _already_ repurposed — #43's `stall`
edge (`scanStallEdge`, `run_edges.go:474`) _is_ the escalate-human
row: question-escalation when interactive, blocker report
headless, drawn from the shared repair budget. The new work is a
**model-directed replan turn inserted before that escalation** —
the edge's prompt branches on budget, not a second edge.

- **scan:** the run ended via `hasRepeatedToolCalls` (`in.stalled`,
  plumbed at `agent.go:1553`) — plus a cheap fallback re-scan of
  `result.Steps` inside the edge, since `in.stalled` only reflects
  the StopWhen callback — and fantasy's `isStopConditionMet`
  **short-circuits at the first true condition**, with the
  context-window summarize check first in the `StopWhen` slice
  (`agent.go:1378` before the detector at `1399`): context
  pressure can mask a real stall and leave `loopStopped` false.
  **Coverage, stated:** the masked case only exists when
  `!notebookEnabled && !disableAutoSummarize` (`agent.go:1393`)
  — notebook-on or summarize-off configs can't mask, so the
  fallback's coverage is narrower than "every stall"; and
  reordering `StopWhen` (detector first) would be the wrong fix
  anyway — the masked case currently gets _both_ a replan and a
  summarize+continue, the better recovery order.
  Corollary interaction to handle: a masked stall +
  `shouldSummarize` produces a replan retry _and_ a post-summary
  continue call queued behind it — `[replan, …, continue]` since
  `runEdges` prepends and the summarize path appends. The
  continue fires **unconditionally** whenever
  `shouldSummarize && tool calls > 0` — even if the replan turn
  resolved the stall and finished the task, the continue still
  re-prompts "resume the original request." (Masking also
  requires `cw > 0`, `agent.go:1382` — an unknown context
  window can't mask either.) Decision: accept the
  occasional redundant turn for v1 (the replan's own stop doesn't
  cancel queued calls); conditioning the continue on the replan's
  outcome is follow-up, not this PR. **Budget reset, named:** the
  continue re-queues `call` itself — carrying _its_
  `RepairAttempts` — so a masked-stall sequence can burn
  replan+escalate (2 turns) and then the continue arrives with a
  fresh budget — bounded at ~6 runs _per summarize cycle_, not
  per session (original → replan → escalate → continue →
  replan → escalate; every subsequent context-pressure trip
  produces another summarize+continue with another fresh
  budget), and only reachable with the notebook off (the
  masked case's precondition).
  Pre-existing for all edges; the replan makes the
  sequence likelier.
- **Headless behavior changes — stated:** today a headless stall
  sets `fire=false` → blocker report, no retry. Under
  replan-first the replan branch fires unconditionally (no
  question tool needed), so `crush run` gains one bounded repair
  turn on loop-stop where it previously stopped — intended, but
  it is a headless cost/latency change, and the blocker report
  still lands via the existing `reports` slice on exhaustion.
  `retry.NonInteractive` carries through the clone, so the replan
  turn is a full-priced extra LLM call on every headless
  loop-stop — the eval flag arm must surface this as cost, not
  just flips. **Concretely, that means chain-summed usage:**
  `emitEvalTelemetry` reports the _terminal_ run's
  `result.TotalUsage`/`len(result.Steps)` only — a replan turn's
  tokens land in session DB totals but not the record's
  `tokens`/`steps` fields, so the arm comparison under-reports
  exactly the spend the flag adds. The record needs chain-summed
  usage (session counters), not the final `Run`'s result —
  boundary-delta, not session-lifetime: session counters are
  cumulative, accurate for single-turn eval trajectories but
  over-counting multi-turn eval sessions; snapshot the counter
  at chain start and diff at chain end.
- **Mechanics:** `prompt func(t)` doesn't receive `call` —
  `scan` stashes `call.RepairAttempts` (and the branch decision)
  onto the `edgeTrigger`. `resolve` doesn't receive `in` either —
  step-bound evidence (steps for the signature re-scan, write
  set, checkpoint inputs) is stashed on the trigger by `scan`
  the same way, or the signature extends. The replan prompt's leading literal
  joins `RepairPromptPrefixes` (`run_edges.go:298`) — **distinct
  from `stallRetryPrefix`** or the eval analyzer can't split
  `replan`/`escalate` variants, the edge's most interesting
  firing stat. Caveat: merged prompts fingerprint only by their
  _leading_ prefix — a verification+replan merge reads as
  `verification` to the analyzer; `edge_firings` rows carry the
  real per-edge stats, so don't expect prefix fingerprinting to
  see the second section. The `fire` condition
  splits by branch: replan fires without `hasTool(question)` —
  that check belongs only to the escalation branch.
- **Mechanism — the stall edge's prompt branches on attempts:**
  first stall → the replan prompt below (works headless AND
  interactive — it's a model retry, not a question); stall on the
  repair turn → the shipped escalation prompt
  (`stallRetrySection`, one question-tool call — interactive-only,
  `fire: a.interactive && hasTool(question)`; headless keeps the
  blocker-report resolve. More precisely the condition wants
  `!call.NonInteractive` — interactivity is per-call (the task
  path sets `NonInteractive: true`, `coordinator.go:2010`);
  `hasTool(question)` is the real subagent safeguard either way).
  A third stall falls to the terminal
  exhaustion note. Both turns live _inside_ `RepairAttempts` —
  escalation consumes the last retry slot rather than bypassing
  the budget (the shipped comment's "one per distinct blocker,
  drawn from the shared repair budget" stays true). The
  alternative — escalation bypassing the budget with per-blocker
  suppression keyed on the repeated-signature hash — is rejected:
  more machinery, and an unbudgeted escalation edge is nagging
  with a type signature.
- **"First stall" means "first repair slot," stated plainly:** the
  branch is `call.RepairAttempts`-driven and the budget is shared
  across edges — a run that burned a verification repair and
  _then_ stalls skips the replan and goes straight to escalation.
  Intended: the model already spent its cheap retry. Do not
  implement a per-stall counter.
- **Why replan-first, not escalate-directly:** the loop detector
  trips mostly on _environmental_ stalls — permission denied,
  identical output, unchanged state — where a meta-prompt ("state
  which assumption failed") unsticks for one cheap turn. The
  objection is real — the model that just thrashed is the least
  trustworthy replanner — which is exactly why the replan is
  bounded to one slot and escalation still fires if it stalls
  again. It also makes the eventual question better: "tried X,
  it didn't work" is a more answerable ask than "we're stuck."
  Bonus coherence: subagent runs can't ask (no question tool), so
  escalate-directly degrades them to a blocker report — under
  replan-first, a stalled child gets a replan turn and its parent
  sees the outcome through the agent result.
- **Handoff payload:** pull the session's latest checkpoint into
  resolve — #48 shipped the machinery but nothing wires it into
  an edge yet; this is where "the forced turn starts re-oriented
  at zero model cost" becomes literal. **Timing, stated:** the
  run-end checkpoint generates in an async goroutine spawned
  _after_ `runEdges` (`agent.go:1563+`), so resolve sees the
  previous turn's (or a mid-run) checkpoint — never this run's.
  The accessor exists in pieces: `notebook.LatestCheckpointIDs`
  over `GetEntries` already does the selection
  (`retrieve.go:349`), so resolve composes two calls — no new
  service method strictly needed, a smaller item than the
  signature-returning detector.
  That's fine for re-orientation: the stall evidence itself
  comes from `result.Steps`. When the notebook is off, the edge
  degrades to evidence-only — no payload.
- **`stallBlockerReport` reuse:** it's computed in `scan`
  unconditionally today but discarded whenever a retry is
  enqueued (reports only reach the message at exhaustion).
  Under replan-first its content — repeated tool, evidence —
  should feed the replan prompt rather than being
  computed-then-dropped.
- **resolve:** collect the repeated call signature — **unlisted
  work:** `hasRepeatedToolCalls` returns `bool`, the winning
  signature is computed and discarded (`loop_detection.go:33`),
  and `repeatedToolName` only approximates the dominant tool
  _name_; resolve needs a signature-returning detector variant
  or a re-run of the signature computation over the window.
  Also collect the write set (**scan `result.Steps` for
  write-class calls** — filetracker
  can't serve it: writes also `RecordRead` for staleness tracking,
  so the tracker can't distinguish reads from writes, and bash
  mutations bypass it entirely; the `scanVerification`
  (`verify_gate.go:51`) steps-scan precedent), and — with
  `project_index` on — a `map` slice over the touched dirs.
- **prompt:** "you stopped making progress: <evidence>. State which
  assumption failed and revise the approach." One turn within
  `RepairAttempts`.
- **Gating:** rides `ambiguity_clarification` with the stall edge
  it extends — a retry turn on every loop stop changes behavior
  for everyone, so the default-on flip is a separate
  evidence-gated decision (#38/#54), not part of this row.

### burn-watch

The spend tripwire the loop detector can't provide: it catches
repeated calls; burn-watch catches _monotone progress that never
produces a write_ — 40 steps, 2M tokens, zero edits, run ends
"cleanly" and nobody noticed. This is the "60M tokens and I didn't
notice" failure as a declared transition.

- **scan:** the finished run consumed >T tokens (or >S steps),
  **ended in a clean stop** (a cancelled or errored 40-step read
  run must not nag), **didn't stall** (`in.stalled` — the stall
  edge owns that boundary's signal), and
  produced zero **mutating calls** — `toolclass.IsMutatingCall`,
  the shared write-boundary vocabulary the scope gate and
  checkpoint boundary already agree on (write-tools + `download`
  - mutating bash), not the raw `writeToolNames` map — a run
    that wrote only via `bash > f` must not false-trip.
    (`eval/analyze.go` uses the narrow map deliberately for gate
    metrics — a documented blind spot burn-watch must not inherit.)
    **Token metric:** `result.TotalUsage.InputTokens` — already the
    per-step sum, the true billed spend since each step re-sends the
    prompt; **cache-read excluded** — cached tokens are ~free and
    would trip the tripwire far too early. A second already-
    persisted arm exists: **`session.Cost` accumulates per-run
    billed spend** (`updateSessionUsage`, `agent.go:2841`) — a
    pre/post-run diff yields a real dollar figure with no
    `stepMessages` plumbing; use it alongside or instead of the
    token arm where cost precision matters. (It inherits the
    delegate-all-writes blind spot: `updateParentSessionCost`
    folds child spend into the parent while the child's writes
    never appear in the parent's `result.Steps` — a run that
    delegated every write can fire "N dollars, zero writes."
    Same accepted category as the `IsMutatingCall` blind spots,
    and the cost arm makes it likelier.)
    `fallbackStepUsage` is
    **out of scope**, harder than it first looks: `stepMessages`
    is _overwritten per step_ (`agent.go:1197`), so the boundary
    holds only the last step's messages — an estimated arm needs a
    per-step accumulator plumbed through `edgeInput`, and zero-
    usage providers are mostly local/free models where billed
    spend is ~zero anyway; the step arm covers their burn shape.
    T/S are
    generous tripwire thresholds, not a governor — the edge exists
    to surface, not to throttle. "Configurable" is real work:
    threshold + step count need an `optionSpec` entry, schema, and
    `Options` field. **The token arm is conjunctive, not
    disjunctive:** Σ `InputTokens` counts the re-sent prompt per
    step, so on a non-caching provider any few-step read-only turn
    in a mature session crosses a flat ~200K — the arm would fire
    on session size, not unnoticed spend. Fire on `steps > S`
    (~30), or `steps ≥ Smin` (~10) **and** `tokens > T` — the step
    floor keeps the token arm honest. T defaults get tuned from
    `edge_firings` data before any default-on flip, since firing
    rate on cache-less providers otherwise just measures context
    size.
- **resolve:** collect the evidence — steps, tokens, exploration
  event count, files-read-without-write **this run**: scan
  `result.Steps` for `ReadToolNames` (the `scanVerification`
  steps-scan precedent — `stubs.go:189` iterates stored
  _messages_, not steps) and subtract the scanned write set —
  `filetracker.ListRecentReadFiles` is session-scoped, not
  run-scoped.
- **prompt:** escalation-family — a `question` turn ("spent N
  tokens over M steps with no writes — continue / replan /
  stop?"), not a retry prompt. Headless degrades to a logged
  assumption per the shared rule.
- **note (exhaustion path):** "N tokens over M steps, no writes"
  — needed for the _interactive_ exhaustion path only:
  `writeRepairExhaustion` is only reached when `len(prompts)>0`,
  so a pure headless-degraded boundary (`fire=false` alone)
  never renders notes — the resolve-appended assumption is the
  only terminal signal there. At `attempts=2` with a firing
  interactive edge alongside, the note is what carries the
  write-less signal to the terminal message.
- **Gating:** rides `ambiguity_clarification` — it's the same
  calibrated-autonomy family as stall escalation (a user-targeted
  "is this intended?" prompt); the default-on flip is a separate
  evidence-gated decision informed by its own firing-rate
  records. Update the flag's `optionSpec` description when this
  lands — it currently describes clarification only, not the
  spend tripwire.
- **`runEdgeSet` position: last.** It reads `in.stalled` (run
  input), not the stall edge's trigger, so ordering is cosmetic —
  declared here because the doc requires new rows to say where
  they sit.
- **Budget crowding — stated decision:** crowding bites
  in-chain. `StopTurn` is set only when the user _cancels_
  (`tools/question.go:128`) — a successful answer returns a
  normal result, the model continues, and the escalate run ends
  `stop` → `cleanStop` holds. So the carrier merges at the
  escalate run's _own_ boundary, on the same clone whose
  `RepairAttempts` is already incremented — the carried retry
  trigger re-fires there and consumes the next shared slot,
  not a fresh call (a fresh call has no `deferred` field at
  all). Crowding thus occurs two ways: same-boundary co-fire
  at an already-spent budget, and a carried re-fire consuming
  the last slot → note, not turn. Accepted — the alternative
  (escalation bypassing the budget) is the rejected nagging-
  with-a-type-signature, and the once-per-crossing marker
  bounds frequency. Records will show whether crowding
  actually occurs before any threshold is tuned.
- **Fires once per crossing, resets on write.** The marker is a
  `SessionAgentCall` field, and the seam owns both halves:
  `runEdges` **stamps** it on the retry clone at the existing
  `retry := call` site when the trigger fires, and **clears** it
  when the just-finished run contained a mutating call —
  otherwise a repair turn that writes then burns again stays
  suppressed forever. No `amendRetry` type extension; the edge
  never touches the clone. The clearing scan is skippable when
  no marker is set (flag-off never stamps → clear is a no-op),
  so the `IsMutatingCall` sweep only runs on stamped chains. (Alternative considered: derive
  suppression from the persisted `edge_firings` records — rejected
  for v1, a per-boundary DB read for what a field does free;
  records remain the accounting layer, not the suppression
  signal.) Marker scope is **per run-chain**: it propagates only
  through the retry-clone chain, so a write-less streak spanning
  fresh user turns re-fires at each turn end — intended (each
  turn's spend is a fresh decision worth surfacing); widen to
  session scope only if the re-fire proves nagging in practice.
  **Degraded path has no carrier — stated decision:** the headless
  degrade appends a logged assumption, not a retry clone, so no
  marker stamps; a headless repair chain that burns write-less
  again can re-fire and append repeated degraded notes —
  at most once per boundary, i.e. ≤3 per chain (boundaries at
  attempts 0/1/2, and `resolve` runs _before_ the budget check);
  across fresh turns it's a cheap log line, and `edge_firings`
  makes the repetition visible. Accepted for v1; revisit records-
  derived suppression only for this path if records show spam.
  **Marker-stamp rule, pinned:** the seam stamps only when the
  recorded outcome is `fired` — a headless-degraded trigger
  alongside another edge's retry does NOT stamp that clone
  (degrade → no carrier → re-fire allowed, per above).
- **Precedence — and the merge-rule gap:** when an
  escalation-family prompt co-fires with retry-family triggers,
  the user-targeted prompt wins the slot outright and retry
  triggers **defer** — but deferral only works for
  session-state evidence: `scanTodosEdge` reads `planVerdicts`
  (stored, survives the escalate turn) while `scanVerificationEdge`
  scans `in.result.Steps` — and the escalate run's steps don't
  carry the prior run's evidence (only the question call on the
  cancel path; an answered question lets the model keep working
  — writes included, but they're _new_ steps), so a deferred
  verification trigger is
  _dropped_, not deferred (a deferred stall-replan's signature
  evidence is likewise step-bound). **Carrier — unlisted work:**
  `SessionAgentCall` gains a `deferred` field carrying the
  deferred trigger(s), stamped on the escalate clone by the seam;
  at the next boundary `runEdges` merges them back — prompt
  section if a retry slot is free, note at exhaustion. The
  merge runs _unconditionally on `cleanStop`_, before any
  scan short-circuits — the `StopTurn`-ended boundary it exists
  for is precisely the case where cleanStop-gated scans never
  run. Read-back
  from `edge_firings` rejected: it couples prompt rendering to a
  DB read. Blast radius is small today (verification ~never
  co-fires with stall, never with burn-watch) but the
  cancel-hole rule below has no carrier without it. What they
  land in depends on budget: if the escalate turn consumed the
  last slot, the re-fire renders an exhaustion note, not a
  repair turn (consistent with the crowding acceptance).
  **Intra-family rule — escalate-family only:** first-in-
  `runEdgeSet` wins, losers defer via the same carrier.
  Scoped to escalate on purpose: retry-family triggers keep
  _concatenating_ — verification+todos is a routine co-fire
  today merged into one prompt, and applying first-wins there
  would regress the merge the seam shipped for (todos deferring
  to a second turn). Needed for future edges, but
  **dead code for the shipped set**: stall-escalate +
  burn-watch can never co-fire (burn-watch requires
  `cleanStop && !in.stalled`; stall requires `in.stalled` — the
  only overlap, a provider `stop` on the terminal tool-call
  step, is exactly what `!in.stalled` excludes). **The carrier's
  real load-bearing case:** burn-watch + todos co-fire, then the
  user cancels the question → `StopTurn` → `cleanStop` fails →
  todos never re-scans and open items are silently unmet.
  Todos' evidence is session-state (re-scannable on the next
  clean-stop boundary), but its _terminal note_ at the cancelled
  boundary needs the carried trigger to render — that's the
  carrier's justification, not the impossible co-fires.
  **Cancel hole, decided:** if the user cancels the
  escalation question, its result carries `StopTurn`
  (`question.go:126-128`) → `cleanStop` fails → deferred
  cleanStop-gated triggers never even re-scan, and the chain
  ends with failed checks silently unmet. Rule: a
  `StopTurn`-ended escalation boundary still renders deferred
  triggers' **notes** on the terminal assistant message — the
  firing is recorded and the terminal signal survives even
  though the retry doesn't. Deferred triggers' `note` funcs also
  still render at a budget-exhausted boundary (notes collect per
  firing trigger regardless of who won the prompt slot).
  Carry-as-context is
  rejected: inlining failed-check evidence into a prompt whose
  instruction is "ask the user" muddies the turn's semantics.
  This rule is **not implemented today**: merged prompts
  concatenate sections (`run_edges.go`). The real co-fire pair is
  **burn-watch + todos** — verification can't co-fire with
  burn-watch: entries attach only to `WriteToolNames` results
  (`verify_gate.go:111`), so a zero-mutating-call run produces
  none and `scanVerification` returns nil. A clean-stopping run
  that both left items open and burned write-less tokens hands
  the model "finish these items" _and_ "ask the user" and lets
  it pick. (verification/
  todos + stall ~never co-fire — a stalled run's terminal step is
  normally `FinishReasonToolCalls`, so `cleanStop` fails; a
  provider _can_ return `stop` on a tool-call step, rare but real,
  and exactly the case `family` handles — mildly supportive of
  shipping `family` in the stall-replan PR, which the ordering
  already does.) Mechanism:
  `edgeTrigger` gains a `family` field (retry vs escalate), set
  by `scan` — it must live on the trigger, not the edge: the
  stall edge's replan branch is retry-family (model-directed,
  fires headless) while its escalate branch is escalate-family,
  and one edge-level field can't express both. When both
  families fire, `runEdges` renders only the escalate-family
  prompt and marks retry triggers deferred.
- **Per-run scope means chains evade it — stated:** steps/tokens
  evaluate per finished run, so a 3-run repair chain burning 45
  steps / 600K tokens in 15-step/200K slices never trips either
  arm. Bounded by `maxRepairAttempts`; records show whether
  chain-level spend matters before any chain-aggregated
  threshold is considered.
- **`IsMutatingCall` blind spots = burn-watch false positives,
  stated:** `mutatingBashRe` is deliberately conservative —
  `make`, `go generate`, `python -c` writes, heredoc scripts
  pass un-gated, so a run that wrote via an unclassified command
  reports "N tokens, no writes" incorrectly. The once-per-
  crossing marker bounds it to one nag per chain; widening the
  regex is a separate decision from this edge.
- **Headless degrade pins to the final assistant message** —
  same carrier as stall's blocker report so it reaches
  `RunComplete.Text` for `crush run`; a slog-only degrade would
  hide a spend tripwire in exactly the unattended context where
  unnoticed spend happens.
- **Limit, stated plainly:** edges fire at run boundaries only —
  a single giant turn mid-flight is not caught. Mid-run spend
  pressure is `CONTEXT_WINDOW_SAFETY.md` territory (or a future
  hook), not this edge's job.

### summarize-continue

Reframes auto-summarize as an edge: context pressure at the run
boundary triggers a continue-after-summary turn instead of a
mid-loop condense. Deferred — the interaction with `StopWhen`'s
existing auto-summarize path needs designing first (who owns the
threshold; does the edge replace or wrap it). Not before the other
rows prove the type.

### edge-firing records

Log every edge firing per turn — name, trigger, outcome — into the
run record. **Store decision — records need a real table.** There
is no runtime run-record store today (the eval `RunRecord` is
post-hoc JSONL analysis of the session DB), and the cheaper
alternative — message metadata like verification outcomes —
doesn't fit: suppressed-by-marker, budget-exhausted, and
headless-degraded firings have no natural message home, yet
those are exactly the states that make firing rates interpretable
("never fires" must be distinguishable from "never evaluated").
Follow the `collapsed_turns` precedent: migration + sqlc query +
service method + stats section + eval analyzer read — an
`edge_firings` table. **PK: `(session_id, turn_seq, edge)`** —
`turn_seq` is the ordinal of the run's initiating user message
among **all** user messages — repair retries persist their
prompts as user messages (`createUserMessage` runs
unconditionally per `Run`), so each attempt's initiating
message naturally differs and `repair_attempts` is **demoted to
a column** (still queryable — disambiguates replan-vs-escalate
stats). **Source: a `COUNT(*)` query, not the working view.**
`getSessionMessages` returns the full transcript only when
`notebookEnabled`; notebook-off it returns the `ListFromSummary`
tail (`agent.go:2518-2531`, and the code comment says why —
turn numbers must stay absolute), so `messageTurns` ordinals
are tail-relative there: post-summary firings get _smaller_
ordinals, and a relative `turn_seq=3` can `INSERT OR IGNORE`-
collide with a real pre-summary row at absolute turn 3 —
the dedup mechanism silently eating a firing. But the obvious
fix is wrong too: `ListUserMessages` (`messages.sql:59-65`) is
`ORDER BY created_at DESC LIMIT 200` — a _prompt-history_
query, not an absolute count (underivable past 200, DESC can't
yield the ASC ordinal, `created_at` is 1-second resolution so
same-second ordinals are ambiguous). **Unlisted work:** a
`CountUserMessagesBySession` sqlc query — `COUNT(*)` over
`role='user'`, unbounded — or an absolute user-turn counter on
the session (more robust, needs backfill). **Correlation
plumbing, also unlisted:** "ordinal of the run's initiating
user message" presumes identifying _which_ user message
initiated the run — nothing on `SessionAgentCall` carries its
created message's ID; stash it from `createUserMessage`
(`agent.go:976`) or snapshot the count at `Run` start. Note
folded queued prompts create _non-initiating_ user messages
mid-run (`agent.go:1115`), so the ordinal isn't "number of
turns" — fine for PK purposes, but name it. It needs plumbing
into `edgeInput` either way. (`collapsed_turns` never hit this
because its rows are written and deduped within one numbering
view; `edge_firings` spans summarize boundaries.) **`call.RunStamp` is
rejected as the key** — it's `runStampGen.Add(1)` (`agent.go:892`)
seeded from a random **per-agent-instance** epoch
(`runStampEpoch`, `agent.go:496-508` — the code comment says
per-build; it's really per-instance/per-process, which only
strengthens the argument: a restart reseeds): non-deterministic
per logical turn, orders nothing durably. (Dedup rationale,
corrected: the repair queue is in-memory — a resumed session
can't re-fire the same logical boundary at all. `INSERT OR
IGNORE` exists for idempotent writes _within_ a boundary —
e.g. a mid-write crash — not cross-restart re-fires.) It earns a plain _column_ instead — it still
joins to persisted `run:<stamp>` checkpoint tags. Columns: edge
name, `variant` (stall records `replan`/`escalate` — the single
most interesting firing stat for that edge), trigger detail,
**outcome enum**: fired / suppressed / exhausted /
headless-degraded / **gated** (flag-off — the flag gates
`t.fire`, not the scan; the row records the would-have-fired
verdict. A boundary with no row means _clean_, which stays
distinguishable precisely because gated rows exist) /
**deferred** (lost the prompt slot to an escalate-family
trigger — recorded at the boundary where it lost, real
outcome at the next) / **cleared** (scan fired but `resolve`
dropped `t.fire` — a pending→clean verification isn't
"suppressed"; nothing suppressed it) / **cancelled** (`ctx.Err()`
mid-loop — see "Cancel partial rows" below), `created_at`
(firing-rate-over-time queries).
Precision note: records distinguish _triggered-but-not-fired_ vs
_fired_ — "evaluated" is guaranteed by construction since
`runEdgeSet` statically scans every edge at every boundary; the
one coverage hole is the entry guard
(`a.configStore == nil || in.result == nil ||
len(in.result.Steps) == 0` — three conditions, `run_edges.go:106`),
named here so nobody reads zero rows as dead code. One more
caveat on the invariant: hard-cancelled/errored runs return err
_before_ `runEdges` (`agent.go:1538`) — boundary never evaluated,
zero rows, distinguishable from "clean" only by the run's own
record. `cancelled` covers the mid-loop `ctx.Err()` case only;
acceptable for firing-rate stats, but named since no-row=clean
is load-bearing.
**Scan-contract extension — unlisted work:** today a nil scan
return is the only "no" signal, so `gated` and `suppressed`
can't be recorded. **The flag gates `t.fire`, not the scan** —
a gated edge still evaluates its predicate and the row's trigger
detail carries the would-have-fired verdict: flag-off means
"don't act," _not_ "don't measure." That's the numerator the
default-on flip decision exists on — a `gated` row that only
records "we didn't look" throws away exactly the firing-rate
data #38/#54 need. (Cost, stated: the scan work runs even
flag-off — cheap: the stall signature re-scan and the
burn-watch step/token walk are both in-memory; no gated edge
does a service read.) The contract: `scan` returns a non-nil
trigger carrying an outcome hint (`gated`/`suppressed`) instead
of nil when the predicate is off or suppressed — no hoisted
predicate needed. **Gated/suppressed triggers skip `resolve`
(and the note/report/deferred-merge paths) — only the firing
row is written.** This is load-bearing, not tidiness:
`runEdges` calls `resolve` for every non-nil trigger and
`resolveStallEdge` appends the blocker report whenever
`!t.fire` (`run_edges.go:489-498`) — without the skip, a
gated stall trigger would emit blocker reports with the flag
off, and the flag would stop gating output entirely. Volume: flag default-off means a `gated` row
per gated edge per boundary — ~2 rows/turn for most users;
cheap, but the table's dominant write pattern from day one.
**Cancel partial rows:** `ctx.Err()` at `run_edges.go:118-124` exits
mid-loop — a cancelled boundary leaves rows only for already-
scanned edges, and un-scanned ones would read as _clean_.
Record a `cancelled` outcome row for each un-scanned edge on
that path so the no-row=clean invariant stays true. `crush stats`
reads the table; the eval doc reads the same counts via
`SessionTelemetry` + `emitEvalTelemetry`. **Write path — not `notebook.Service`:**
firings are harness telemetry, not notebook concepts, and adding
a record method churns every `notebook.Service` mock. (The nil-
check trap is a myth, corrected: `app.Notebook` is constructed
unconditionally at `app.go:168` and always threaded —
`a.notebook` is never nil in production; the `prior_turns.go:249`
early-return is defensive dead-code, and notebook-off sessions
produce no collapse rows because `collapsedEvents` stays empty,
not because the service is nil. Route firings through a
dedicated service or a `db.Queries` handle held by the agent
anyway — the mock-churn reason stands alone.) **Instrumentation point: inside `runEdges`' scan
loop**, written per-edge right after scan/resolve — the
`len(prompts)==0` early return precedes the budget check
(`run_edges.go:145` vs `149`), so a firing edge that renders no
prompt section skips `writeRepairExhaustion` entirely today;
records written after those gates would miss suppressed and
exhausted outcomes. (Lighter alternative considered: a
`ContentPart` type on the boundary assistant message — `Parts` is
a JSON blob, no migration — but firing-rate queries and eval
assertions want structured columns, not JSON-part scans; the
table follows the `collapsed_turns` precedent for exactly that
consumer pair.)
**Name stability lands first** — records become eval assertion
targets, so edge names must be final before the table exists:
the shipped edge is `stall` (the doc's "stall-replan" was the
delta it gained; "escalate-human" is fused into it, not a
distinct edge). Renaming post-baseline churns eval baselines.
This is what
`EVAL_HARNESS` consumes as trajectory assertions: named
transitions give the corpus stable checkpoints, and asserting on
emergent loop behavior does not. Also surfaced in `crush stats` —
an edge that fires constantly is a tuning signal, an edge that
never fires is dead code.

## PR ordering

1. Extract `runEdge` from `runVerificationGate` — pure refactor;
   verification + todos become the first two instances. Ships
   inside #39's series — `escalate-human` needs this seam.
2. Edge-firing records — `edge_firings` table + stats + eval
   field. Lands first among the remaining rows: the three shipped
   edges get recorded immediately, and stall-replan's new branches
   are captured from day one. Requires name stability up front
   (the `stall` name stays).
3. `stall-replan` — the stall edge's branch on `RepairAttempts`;
   implement the `family` precedence mechanism.
4. `burn-watch` — conjunctive thresholds + once-per-crossing
   marker; records exist so its firing rate is measurable from
   day one.
5. `summarize-continue` — only after the `StopWhen` interaction is
   designed.

(The join edge is not an item here — it lives in
`BACKGROUND_SUBAGENTS.md` with its dependencies.)
