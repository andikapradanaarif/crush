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
	scan func(steps []fantasy.StepResult, sess *session.Session) *edgeTrigger
	// resolve runs the deterministic part (shell checks, ledger
	// queries). May be nil for prompt-only edges.
	resolve func(ctx context.Context, t *edgeTrigger) error
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
| escalate-human     | loop-detector stop, or repair budget spent         | implemented (#43) — question turn                            |
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
a large-scope plan, repair budget spent), the prompt decides how.
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
from `result.Steps` — filetracker tracks reads only), so the
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
edge (`scanStallEdge`, `run_edges.go:473`) _is_ the escalate-human
row: question-escalation when interactive, blocker report
headless, drawn from the shared repair budget. The new work is a
**model-directed replan turn inserted before that escalation** —
the edge's prompt branches on budget, not a second edge.

- **scan:** the run ended via `hasRepeatedToolCalls` (`in.stalled`,
  plumbed at `agent.go:1553`) — plus a cheap fallback re-scan of
  `result.Steps` inside the edge, since `in.stalled` only reflects
  the StopWhen callback.
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
  just flips.
- **Mechanics:** `prompt func(t)` doesn't receive `call` —
  `scan` stashes `call.RepairAttempts` (and the branch decision)
  onto the `edgeTrigger`. The replan prompt's leading literal
  joins `RepairPromptPrefixes` (`run_edges.go:298`) or the eval
  analyzer won't fingerprint replan turns. The `fire` condition
  splits by branch: replan fires without `hasTool(question)` —
  that check belongs only to the escalation branch.
- **Mechanism — the stall edge's prompt branches on attempts:**
  first stall → the replan prompt below (works headless AND
  interactive — it's a model retry, not a question); stall on the
  repair turn → the shipped escalation prompt
  (`stallRetrySection`, one question-tool call — interactive-only,
  `fire: a.interactive && hasTool(question)`; headless keeps the
  blocker-report resolve). A third stall falls to the terminal
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
  at zero model cost" becomes literal.
- **resolve:** collect the repeated call signature, the write set
  (**scan `result.Steps` for write-class calls** — filetracker
  can't serve it: writes also `RecordRead` for staleness tracking,
  so the tracker can't distinguish reads from writes, and bash
  mutations bypass it entirely; the `scanVerification`/
  `scanPlanEvidence` precedent), and — with `project_index` on —
  a `map` slice over the touched dirs.
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
    **Token metric:** sum `step.Response.Usage.InputTokens` across
    steps — the true billed spend, since each step re-sends the
    prompt — with `fallbackStepUsage` for zero-usage providers
    (flagged `estimated`); **cache-read excluded** — cached tokens
    are ~free and would trip the tripwire far too early. T/S are
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
  event count, files-read-without-write (filetracker's read set
  minus the scanned write set).
- **prompt:** escalation-family — a `question` turn ("spent N
  tokens over M steps with no writes — continue / replan /
  stop?"), not a retry prompt. Headless degrades to a logged
  assumption per the shared rule.
- **Gating:** rides `ambiguity_clarification` — it's the same
  calibrated-autonomy family as stall escalation (a user-targeted
  "is this intended?" prompt); the default-on flip is a separate
  evidence-gated decision informed by its own firing-rate records.
- **`runEdgeSet` position: last.** It reads `in.stalled` (run
  input), not the stall edge's trigger, so ordering is cosmetic —
  declared here because the doc requires new rows to say where
  they sit.
- **Fires once per crossing, resets on write.** The marker is a
  `SessionAgentCall` field, and the seam owns both halves:
  `runEdges` **stamps** it on the retry clone at the existing
  `retry := call` site when the trigger fires, and **clears** it
  when the just-finished run contained a mutating call —
  otherwise a repair turn that writes then burns again stays
  suppressed forever. No `amendRetry` type extension; the edge
  never touches the clone. (Alternative considered: derive
  suppression from the persisted `edge_firings` records — rejected
  for v1, a per-boundary DB read for what a field does free;
  records remain the accounting layer, not the suppression
  signal.) Marker scope is **per run-chain**: it propagates only
  through the retry-clone chain, so a write-less streak spanning
  fresh user turns re-fires at each turn end — intended (each
  turn's spend is a fresh decision worth surfacing); widen to
  session scope only if the re-fire proves nagging in practice.
- **Precedence — and the merge-rule gap:** when an
  escalation-family prompt co-fires with retry-family triggers,
  the user-targeted prompt wins the slot and retry evidence
  carries as context inside it (or defers — conditions re-fire
  next boundary anyway). This rule is **not implemented today**:
  merged prompts concatenate sections (`run_edges.go`). The real
  co-fire pair is **burn-watch + verification/todos** — all three
  gate on `cleanStop`, so a clean-stopping run that both failed
  checks and burned write-less tokens hands the model "fix these
  checks" _and_ "ask the user" and lets it pick. (verification/
  todos + stall can't co-fire — a stalled run's terminal step
  isn't `FinishReasonStop`, so `cleanStop` fails.) Mechanism:
  `runEdge` gains a `family` field (retry vs escalate); when both
  fire, `runEdges` renders the escalate-family prompt and appends
  retry-family evidence as context — never two competing
  instruction sections.
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
`edge_firings` table (session_id, turn seq, edge name, trigger
detail, **outcome enum**: fired / suppressed / exhausted /
headless-degraded). `crush stats` reads the table; the eval doc
reads the same counts via `SessionTelemetry` +
`emitEvalTelemetry`. **Write path — not `notebook.Service`:**
`RecordCollapsedTurn` rides `a.notebook`, which is nil when the
notebook is disabled (`recordCollapsedTurns` early-returns) —
edge firings are harness telemetry, not notebook concepts, and
routing them through it would silently produce zero records for
exactly the notebook-off configs the "never fires" signal exists
to audit. Use a dedicated service or a `db.Queries` handle held
by the agent; the turn seq needs its own derivation at the run
boundary (`collapsed_turns`' `byTurn` comes from message
positions — no equivalent exists for edges). (Lighter alternative considered: a
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
