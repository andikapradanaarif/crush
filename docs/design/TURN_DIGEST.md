# Turn Digest — Fidelity Drop at the Turn Boundary

> **Status:** Shipped. `stub` mode — the `prior_turn` render
> predicate, frozen collapse set, and `notebook-prior-turns` option —
> shipped (#58, closing #49); `digest` mode — turn-granularity
> digest generation, the demotion rule, and the run-end trigger
> absorption — shipped (#63, closing #50). The default stays `verbatim` pending
> the paired-eval gates below. Splits
> context into two planes: the conversation
> (user messages, assistant answers, question/answer pairs — full
> continuity) and the execution transcript (tool calls + results —
> collapsed at turn end). Composes with `TOOL_RESULT_PRUNING.md`
> (the `prior_turn` predicate rides stub machinery), `SESSION_KNOWLEDGE.md`
> (the digest is a checkpoint-shaped entry at turn granularity),
> `CONTEXT_NOTEBOOK.md` (generator, selection, recall), and
> `EVAL_HARNESS.md` (three-arm paired experiment).
>
> **Depends on:** no unshipped prerequisites — but this spec
> introduces new render-path code itself (the `ToolCallPart.Input`
> collapse, the coverage predicate, the selection demotion). Digest
> generation reuses the post-run pipeline slot that
> `SESSION_KNOWLEDGE.md` already reserves for run-end checkpoints.
>
> **Ship when:** `stub` mode after implementation; `digest` mode and
> any default change only behind the paired-eval gates below.

## Goal

Cap context growth **across** turns. Within-turn stubbing shrinks
stale results; it does nothing about prior turns' share of the
rendered window — covered segments inside the raw window render
raw **and** as notebook entries, the double render. At each turn
boundary the execution plane drops a fidelity level: raw tool
events stop rendering and a checkpoint-shaped **turn digest**
stands in their place.

```text
current turn      → full fidelity (calls + results, stubs as today)
completed turns   → collapsed: stub pairs + turn digest in notebook
closed session    → session checkpoint        (SESSION_KNOWLEDGE)
older sessions    → mem0 hydration            (SESSION_KNOWLEDGE)
```

## Problem

Two symptoms, one missing boundary:

1. **Prior turns double-render inside the raw window.** Prior turns
   beyond the raw-window boundary already don't render — the
   boundary caps them. What remains is the _double render_: covered
   prior-turn segments inside the ~25K raw window render raw **and**
   as notebook entries. Collapse dedupes the raw window's
   prior-turn share against the notebook — savings scale with
   raw-window composition, not session age.
2. **Nothing consolidates a finished turn.** The notebook logs
   events; the assistant's final text is written for the user, not
   for the model's next turn — it may be terse ("done") or omit
   which files were touched. A turn that ended in exploration
   leaves _no_ usable trace once its events collapse.

## What exists

- **The render seam.** `preparePrompt` (`agent.go:1848`) is where
  stored events become rendered context; `promoteSupersededStubs`
  (`stubs.go:441`) already applies marks there. `prior_turn` is
  another mark applied at the same point.
- **Turn numbers are derived, not stored — and can move mid-run.**
  `db.Message` has no `TurnNumber` (`db/models.go:41,63` are
  `NotebookEntry`/`ProcessedSegment`); message turns are computed
  positionally by `messageTurns` — cheap, already computed in
  `flagPrunableToolResults`. Consequence: `drainQueueForStep` folds
  a queued user prompt into the run mid-flight, advancing
  `currentTurn` _while the run executes_ — naive
  `turn < currentTurn` would collapse the active run's own earlier
  events. The predicate is `turn < turnThatStartedThisRun`, not
  `turn < currentTurn`.
- **The stub contract fits.** Invariant 6 — "stub beats drop" —
  extends cleanly: a collapsed pair keeps call/result structure and
  IDs (provider protocols require pairing) and names its recovery
  path (`recall("result:<tool_call_id>")`, `recall.go`).
- **The generator slot is reserved.** The post-run pipeline
  (`agent.go:1507`) is where `SESSION_KNOWLEDGE.md` already plans
  run-end checkpoints; the small-model generator (`generator.go`)
  turns classified events into entry prose. A turn digest is the
  same generation over the finished turn's events.
- **Tri-state options are supported.** `optString`
  (`shellconfig/options.go:156`) — precedent: `option turn-context`.
  The `notebook-*` `optionSpecs` keys exist post-#45, so
  `notebook-prior-turns` is a one-line map entry plus a new
  `Options` field (`notebook_prior_turns`, enum).
- **Stored-vs-rendered split is established.** Stubs never rewrite
  `ToolResult.Content`; edit-safety (`filetracker.LastReadTime`,
  populated at tool-execution time) and notebook coverage are
  independent of rendering. Collapse inherits the same discipline.

## Design

### 1. `prior_turn` — the render predicate

At `preparePrompt`, every tool call/result pair in a **fully
covered, completed** turn renders as a minimal stub pair:

```text
call:   {"_collapsed":"prior turn 3"}
result: [prior turn 3 — collapsed; recall("result:a91f") to recover]
```

(`digest`-mode result text may instead read "consolidated in turn
digest — recall(\"result:a91f\") to recover" — constant per mode,
and it **keeps the recall pointer**: the digest may lag async or
never land on generator failure, while `result:` recall resolves
against stored events regardless.)

- **The predicate is coverage, not age.** `turn < runStartTurn`
  **and** every segment of that turn extent-matched to a processed
  row — segment generation lags (in-flight, backed-off, or
  burst-limited), and collapsing an uncovered turn produces stubs
  with no entries behind them: "drop with extra steps." **All**
  segments, not "all non-open": the freeze is computed on the
  run-start `msgs` fetched before `createUserMessage` — on that
  slice the last prior turn's tail is `open` only because it is the
  list-final segment (`notebook_segments.go:206`), while in reality
  it is closed the moment the new user message lands. Exempting it
  would let turn `runStartTurn−1` collapse while its tail — the
  previous run's final assistant steps — has no committed coverage
  if `generateRunEndSegments` failed or is still in flight. The
  tail's recorded extent `[start, len(msgs))` is final, so a
  processed row for it is checkable despite the `open` flag.
  Coverage is the _whole_ gate — the stub's recovery pointer
  resolves against stored events and needs no digest, so `digest`
  mode is exactly `stub` + generation; gating collapse on the
  digest would make generator latency drive collapse timing (async
  prefix churn). A persistent small-model outage degrades to
  `verbatim`-like behavior — safe direction.
- **Run-start turn, not current turn.** `drainQueueForStep` can fold
  a queued prompt mid-run, advancing `currentTurn` under the active
  run — the predicate uses the turn that _started_ the run so its
  own earlier events can't collapse mid-flight.
- **Both sides collapse — new machinery, wire constraint.** Call
  args are often the largest payload (edit `old_string`/
  `new_string` are stale write material once the turn ends), but
  `Superseded` marks live on `ToolResult` only — collapsing
  `ToolCallPart.Input` at render is a new code path
  (`ToAIMessage` emits stored input verbatim, `content.go:762`).
  And the collapsed input must stay **valid JSON** — providers
  require `tool_use.input` to be an object; a prose label fails
  late on Anthropic. The shape is minimal —
  `{"_collapsed":"prior turn 3"}` with no per-tool preview (the
  tool name already renders from `ToolCallPart.ToolName`, and the
  paired result carries the recall pointer).
- **Structure is preserved.** IDs, roles, and pairing survive; only
  content is replaced.
- **Ordering:** `prior_turn` is evaluated first at render. Other
  stub kinds are irrelevant inside a collapsed turn — flagging
  passes may keep marking stored events harmlessly, but render
  checks the turn boundary before consulting them.
- **Stub text is constant _per mode_ — and doesn't over-promise.**
  `stub` mode never says "consolidated in turn digest" — no digest
  exists; mode-specific constant text costs nothing (mode flips
  re-render anyway). Never reflects digest availability — async
  state must not churn the prefix. The recovery pointer is honest
  about its bounds: `recall("result:<id>")` returns the result's
  stored content capped at `MaxOutputLength` (`recall.go:175`), and
  the call _input_ is unrecoverable — for write-class tools
  (`toolclass.IsMutatingCall`) the payload was the input, so their
  stub reads "call arguments collapsed; re-view the file to
  reconstruct" instead of implying `result:` recall restores the
  write material. Recovery path is re-`view` — which is exactly
  what the edit-failure eval arm measures.
- **`ProviderExecuted` calls stay verbatim.** Server-side tools
  carry provider replay semantics a synthetic input may violate.
  (Vacuous in-repo today — `ProviderExecuted` is hardcoded `false`
  at `agent.go:1182,1218` and set by fantasy during provider-side
  streaming — but the exemption is one check and the matrix case
  costs nothing.)

**What stays verbatim** — the conversation plane:

- User messages.
- Assistant text parts (the end-of-turn answer is itself a summary;
  collapsing it would break conversational continuity).
- `question` tool call/result pairs — they are user input in
  disguise and carry decisions the model must not lose.

### 2. The turn digest — checkpoint at `granularity: turn`

A notebook entry generated at run end, over the finished turn's
events:

```text
## Turn 4 digest — <topic>
Established:
- <claim> — evidence: file:internal/agent/agent.go:1401
Files touched:
- internal/agent/stubs.go (edited), eval/flags.json (read)
Open:
- <question> — blocked on / next probe
```

- **Same entry machinery.** `Entry` schema holds it unchanged; the
  `Established/Open` shape is the `checkpoint` contract from
  `SESSION_KNOWLEDGE.md` with `Files touched` added — a turn digest
  records _work done_, not just knowledge established.
- **Granularity is a tag, not a type.** `granularity:turn` on a
  checkpoint-shaped entry; the axis is `turn | boundary | session`.
- **Coexists, with selection demotion — the duplicate-
  representation decision.** The turn still gets per-segment
  entries (coverage requires segment processing, and they're the
  substrate for `PinnedFileTags` — which only fires on
  `EventFileEdit` — plus per-segment `recall` granularity). A digest
  on top means the same work twice in the notebook. Rule: **post-
  selection**, within the selected set — when a `granularity:turn`
  entry for turn N rendered, drop selected entries with
  `TurnNumber == N && EventType != EventCheckpoint` (a boundary
  checkpoint keyed into turn N via `checkpointSegmentKey` is a
  different granularity and survives). Not candidate-set removal:
  a digest that loses selection to budget must leave its segment
  entries eligible, or the turn loses representation entirely.
  Demotion keys on the digest being _rendered in this prefix_, not
  existing — a compressed-away digest demotes nothing. The freeze
  captures **eligibility, not application**: the set of turns
  holding a digest freezes at the run's first render — a digest
  landing mid-run must not demote already-rendered entries (a
  bigger mid-window delta than the freeze tolerates anywhere
  else) — while each render still requires the digest actually
  selected that render, so an evicted digest can't leave turn N
  with neither representation. Mechanically it is a lazy-once
  field on `turnCollapse` populated inside `renderNotebookPrefix`
  — `collapse.Set` freezes in `preparePrompt` where no entries are
  fetched, so "alongside" means the same struct, not the same
  line; `collapse` must be threaded through (`notebookPrefix`'s
  signature doesn't take it today). The prefix-cache interplay is
  safe: `prefixFingerprint` includes entries, so a mid-run digest
  busts the cache and renders — it just must not demote. Demotion
  runs **before** the `files`/`CoveredReViews` loop
  (`notebook_segments.go:1054`) — demoted entries' `file:` tags
  must not inflate coverage for content that never rendered — and
  **before** `dropSupersededReads`, so a read superseded only by a
  demoted entry isn't dropped with no rendered superseder.
  Demotion additionally requires the digest to carry content
  (`CompressionLevel < CompressionTagsOnly`) — a tags-only digest
  renders as a bare tag line and must not strip the turn's real
  entries. Dropped entries' already-accounted tokens aren't
  refunded — a bounded under-fill of the 12k cap, acceptable; an
  optional fill-after-demotion pass could reclaim it later.
  `maybeAutoInject` (`agent.go:2210`) queries `file:` tags
  independently and **checkpoints never inject** (`agent.go:2264`),
  so demotion there only removes — it receives the rendered-digest
  turn set, and only then do its same-turn candidates drop.
  (Replacing segment entries was the alternative; rejected: it
  loses file pins and per-segment recall.)
- **Not self-pinned.** Boundary checkpoints pin because they are
  rare; a pinned entry per turn floods the working set. Turn
  digests are ordinary entries — selectable, compressible, eligible
  for the summary/tags tiers. If a digest compresses away, the
  stub's `recall` pointer still resolves (stored events are
  untouched), so no dead pointer is created.
- **Trigger and dedup identity: the turn, not the run.** Fires at
  run end for **each finished turn lacking a `granularity:turn`
  entry** — catch-up semantics, not "turns this run finished." The
  run-end goroutine only runs on the success path (`agent.go:1513`
  returns before :1534 on cancel/error), so an aborted run's end
  pass never executes and a this-run-only rule would leave its
  turn undigested forever; the same rule covers generator-failure
  retries and `drainQueueForStep` folds (one run finishing two
  turns) uniformly. Oldest-first, **bounded by a per-pass cap**
  (~4 digests) — first enabling `digest` mode mid-session leaves
  every prior turn undigested, and an unbounded catch-up is a
  burst of small-model calls; uncovered turns fill over
  successive runs (segment entries carry their coverage
  regardless). The "interrupted" headline needs
  `FinishReasonCanceled` plumbed into the input builder —
  `EntryInput` carries no finish state today. Dedup keys on
  `(turn_number, granularity:turn)` — does a turn digest for turn N
  already exist — **not** `run:<stamp>`, re-checked inside the
  commit transaction like `errCheckpointExists` so concurrent or
  retried generations can't write two digests for one turn. The
  entry keys to **that turn's own last segment**, not
  `checkpointSegmentKey`'s session-wide last closed segment —
  demotion joins on `TurnNumber == N`, so a digest keyed to an
  earlier turn would demote the wrong turn. The run tag's identity is
  the run's consolidated _position_, and `GenerateCheckpoint`'s
  tag check is granularity-blind by design: reusing it would let a
  mid-run boundary checkpoint suppress the same run's digests for
  exactly the write-heavy turns that need them most. The two
  dedup scopes coexist: `run:<stamp>` for boundary/session
  positions, per-turn for digests. This absorbs
  `SESSION_KNOWLEDGE.md`'s run-end trigger — **explicitly, not via
  dedup**: digests carry no `run:` tag, so
  `generateRunEndCheckpoint`'s existence check never sees them and
  would fire alongside — double consolidation, not absorption.
  Under `digest` mode the run-end boundary trigger is disabled
  outright; the turn digest _is_ the run-end consolidation. The
  **mid-run boundary trigger stays** — it is the only within-turn
  consolidation (the dominant-turn machinery this doc's non-goals
  assign to "stubs + boundary checkpoint," and the payload
  `RUN_EDGES`' stall-replan will consume). A boundary checkpoint
  consolidates the position at the write crossing; the turn
  digest consolidates the turn's whole work at run end —
  different moments, not the same position paid twice. Under
  `stub`/`verbatim` both triggers stand unchanged.
- **Floor units:** ≥1 classified event of _either_ bucket on the
  turn's own events — the floor exists to skip _empty_ turns
  (pure conversation: "thanks"-style turns with zero tool calls
  produce zero events), not to filter exploration depth. A
  pure-`question` turn does produce a digest — `question` calls
  classify significant — harmless, since its pairs stay
  collapse-exempt anyway. Trivial events feed
  the digest input too — a grep-only turn's trail is still the
  turn's evidence. A decision-only turn (assistant answers with no
  tool calls) likewise produces no digest — correct: its text
  stays verbatim in the transcript anyway. Do **not** reuse `GenerateCheckpoint`'s
  `gathered` count: it tallies post-cutoff entries plus
  non-mutating tail events, so `MinExploration: 1` against it is
  async-timing-dependent (a write-only turn passes only if its
  segment entries committed first).
- **Generator input:** the finished turn's _classified events_,
  not its entries — **and not `GenerateCheckpoint`'s cumulative
  input** (all committed entries + uncovered tail restates the
  session position; a digest wants only the turn's evidence). The
  implementation is a sibling service method, not a
  `CheckpointRequest` flag: input builder, floor, and dedup all
  differ. `buildCheckpointInput`/`checkpoint_entry.md` emit
  `Established/Open` with no `Files touched` section — the digest
  needs a prompt variant carrying that shape. Entries never enter
  classification, so no exclusion filter is needed.
- **Gated on the mode, not the checkpoint flag.** Digest
  generation checks `priorTurns == "digest"`, not
  `a.notebookCheckpoint` — sharing the flag would silently degrade
  digest mode to stub when `notebook_checkpoint=false`, an
  undocumented interaction. The flag owns boundary/session
  consolidation only.
- **Placement:** the notebook splice (`PrepareStep`), like every
  entry — selection (with the demotion rule), compaction, recall,
  auto-inject. The only new render-path logic is the demotion skip.
- **Not synced to mem0 — structurally, not coincidentally.** Turn
  digests are session-internal work logs — one per turn would
  flood the cross-session memory pool with fragments.
  Consolidated positions (`boundary`/`session`) remain the
  hydration surface. Exclusion is enforced by filtering
  `CheckpointGranularity(e) == GranularityTurn` at the sync site —
  relying on `req.RunTag == ""` making the tag check miss is
  coincidental (any future dedup tag passed as `RunTag` would
  leak digests into sync).
- **Interrupted runs annotate the digest.** The spec edge case
  stands: an aborted turn's digest headline notes "interrupted" so
  a partial turn isn't read as finished work.

### 3. Flag: `notebook-prior-turns` (tri-state)

```text
option notebook-prior-turns verbatim   # today: full transcript (default)
option notebook-prior-turns stub       # collapse to labeled pairs, no digest
option notebook-prior-turns digest     # collapse + generated turn digest
```

- Three values map exactly onto the three eval arms — one knob, no
  mode pair to keep in sync.
- **Gated by the notebook _and_ the recall path.** The coercion
  lives in `buildAgent` next to `StubSuperseded` (`coordinator.go:
848`), with a warn line like :865: notebook disabled → `verbatim`,
  **and** `recall` in `options.disabled_tools` (`config.go:391`) →
  `verbatim` — a stub pointing at a tool that isn't in the toolset
  is a dead pointer either way (invariant 3).
- **crushrc wiring:** `optionSpecs` already carries the
  `notebook-*` keys post-#45; `notebook-prior-turns` is a one-line
  `optString` entry plus the `Options` field.
- **`digest` resolves to `digest` end-to-end.** With generation
  shipped, `NotebookPriorTurnsMode` returns `digest` verbatim,
  `newTurnCollapse` accepts it as a collapse mode in its own right,
  and the mode threads into
  `collapsedResultText`/`collapsedCallInput` so `digest` mode's
  stub text says "consolidated in turn digest" (§1).
- **Escape hatch:** set back to `verbatim` — takes effect on the
  next agent build (options are captured at agent construction,
  `coordinator.go:848`), after which the next `PrepareStep` shows
  the full transcript again.

### 4. Safety invariants (preserved from TOOL_RESULT_PRUNING)

1. Stored events are never rewritten — collapse is render-time
   only.
2. Read-before-write holds — `filetracker.LastReadTime`
   (`edit.go:292`) is populated at tool-execution time and never
   scans stored message events, so the check is independent of
   rendering entirely; collapse can't weaken it.
3. No dead pointers: `stub`/`digest` modes never ship without
   `recall` available (notebook gate).
4. Size accounting uses **pre-collapse** sizing:
   `findSegmentBoundaryByTokenBudget` keeps measuring stored
   content, so the window boundary doesn't move — collapse is a
   pure within-window transform and the savings are real, not
   reinvested in pulling more prior turns in as stubs.
5. Digests never feed back: the generator reads classified _events_
   (which can't contain entries — `classifyEvents` runs over
   messages, so no skip is needed there), and coverage gating counts
   only segment processing, never digest presence.

### 5. Edge cases

- **Async race.** The digest generates post-run; a fast next turn
  can render before it lands. Fine — stubs render regardless; the
  digest appears in the notebook when ready and the constant stub
  text means no re-render.
- **Aborted runs.** Their events belong to a completed turn next
  turn and collapse normally; the digest headline notes
  "interrupted" so a partial turn isn't read as finished work.
- **The dominant-turn case.** Exploration _and_ execution inside
  one long turn (the token-drain scenario) is not helped by this
  doc — within-turn machinery (stubs, mid-turn checkpoint) owns
  that. Turn digests cap growth _between_ turns; the two compose.
- **Headless/queued runs** collapse identically — the predicate is
  turn numbers, not UI state. Queued prompts folded mid-run by
  `drainQueueForStep` can't trigger collapse of the active run's
  events: the predicate compares against the run-start turn, not
  the (advancing) current turn.
- **Reasoning parts drop with the turn.** Providers require
  thinking signatures only within the in-flight tool-use turn
  (Anthropic strips prior-turn thinking anyway), so prior-turn
  `Thinking` collapses with everything else — often the largest
  prior-turn payload on extended-thinking sessions. The fragile
  combo to avoid is _keeping_ a signature while mutating the
  `tool_use.input` it signed (Gemini `ThoughtSignature` binds to
  `ToolID`, `content.go:750`) — collapse removes both together.
  Provider matrix must include a signed-thinking model; if one
  rejects it, that provider falls back to keeping prior-turn
  reasoning and inputs verbatim.
- **Turn N−1 concentrates the risk.** The all-or-nothing boundary
  collapses the _most recently completed_ turn — the one a
  follow-up most likely references. The edit-failure eval arm is
  the gate; graduated recency windows stay deferred to
  `CONTEXT_WINDOW_SAFETY.md`.
- **`Summarize` renders verbatim — decided, not accidental.** It
  calls `preparePrompt` (`agent.go:1692`), but its ctx carries no
  run state, so a ctx-scoped freeze never reaches it. One-shot
  call, no cache reuse, fidelity is free.
- **`runStartTurn` and the frozen set ride the run context.** The
  freeze — `runStartTurn` plus the set of covered turns — is
  computed once at run start on the pre-`createUserMessage` `msgs`
  (`agent.go:886`) and stored as a ctx value parallel to
  `RunStampContextKey` (:827). `rebuildStepMessages` is not
  context-free — `PrepareStep`'s `callContext` is `genCtx`-derived,
  so the value is readable there; the run-start `preparePrompt`
  call at :978 passes the outer `ctx`, so the freeze must be set
  before it — either on `runCtx` ahead of `createUserMessage`, or
  by switching that call site to `runCtx`. Write-once per run;
  `rebuildStepMessages` reads it, never writes. (Alternative:
  a coordinator-owned map keyed by `(sessionID, RunStamp)`, the
  `stubBoundary`/`segmentTrackers` injection pattern — survives an
  agent rebuild mid-session. The ctx form is preferred because
  Summarize-verbatim falls out for free.)

## Measurement

`EVAL_HARNESS` paired experiment, three arms on the multi-turn
corpus slice: `verbatim` / `stub` / `digest`.

| Metric                                              | Question                                                       |
| --------------------------------------------------- | -------------------------------------------------------------- |
| Cost-normalized tokens-to-done                      | Does collapse actually pay, after generation cost?             |
| Edit-failure rate                                   | Does losing prior-turn write args cause phantom `old_string`s? |
| `result:` recall into prior turns                   | Is the digest losing needed content?                           |
| Re-read rate (files the digest claims were touched) | Does the model trust the digest or re-verify?                  |
| Verdict parity                                      | Same task outcomes across arms?                                |

Ship gates, mirroring `TOOL_RESULT_PRUNING` acceptance criteria:

- `stub` mode: zero flip-flops, recall rate bounded, structure
  valid on all providers in the matrix — explicitly including a
  signed-thinking model (thought signatures on collapsed inputs).
- `digest` mode over `stub`: meaningful token win **and** flat
  re-read rate — if digests don't reduce re-reading vs. bare stubs,
  the per-run generation call isn't earning its tokens and `stub`
  is the mode to ship.
- Default change (if any) follows the #38 evidence process; the
  default stays `verbatim` until then.

## Non-goals

- **No selection of which turns to keep verbatim.** All-or-nothing
  per turn boundary. Graduated recency windows ("keep last two
  turns full") are a pressure-escalation question —
  `CONTEXT_WINDOW_SAFETY.md` owns it.
- **No mid-turn collapse.** Within-turn compression is stubs +
  boundary checkpoint (`SESSION_KNOWLEDGE.md`); the checkpoint is
  the review payload for **post-checkpoint** gates — the
  first-boundary `phase-confirm` resolves pre-write, before its
  own checkpoint can exist.
- **No cross-session scope.** Session close and hydration stay in
  `SESSION_KNOWLEDGE.md`; turn digests are session-scoped inputs to
  the session checkpoint, which summarizes digests rather than raw
  events — and it's the _checkpoint's_ sync that touches mem0; the
  `working_dir` partition (shipped in #56) keeps consolidated
  digests from bleeding cross-project downstream. **Forward note
  for #53:** under `digest` mode the run-end boundary trigger is
  absorbed, so "latest boundary checkpoint = session position"
  becomes "latest boundary checkpoint + subsequent turn digests" —
  hydration must not assume a run-end checkpoint always exists.
- **No model-chosen collapse.** The predicate is deterministic;
  letting the model exempt its own transcript is how pruning
  becomes optional.

## PR ordering

1. `prior_turn` predicate + `stub` mode + `optionSpecs` entry —
   pure render, no generation, but not tiny: the predicate is
   `turn < runStartTurn` **and** turn fully covered, and call-side
   collapse needs the `ToolCallPart.Input` render path with the
   `{"_collapsed": ...}` JSON constraint. Exercises the collapse
   path and its invariants first.
2. Turn-digest generation + `digest` mode + `granularity:turn` tag
   - the same-turn demotion rule; generator reads the turn's
     classified events.
3. Reconciliation with `SESSION_KNOWLEDGE.md`: run-end checkpoint
   trigger folds into the digest path when mode is `digest`.
4. Three-arm eval + ship gates; default stays `verbatim`.
5. Telemetry: turns collapsed, events collapsed, digest
   present-at-render rate (measures the async race), and `result:`
   recalls targeting a call ID in a prior turn — the feasible
   approximation of "recall into collapsed turns" (collapse leaves
   no stored mark, so `recallToolResult` can't distinguish a
   collapsed-turn recall from a superseded-stub one; if even that
   isn't worth wiring, the existing `ResultRecalls` counter stands
   in).

## Risks

- **Edit-failure regression is the gate that matters.** Collapsing
  prior-turn write args removes the content that made a file
  "read"; the model must re-`view` before editing. Storage-side
  safety is intact, but rendered-context staleness is exactly what
  the edit-failure arm measures. If it moves, `stub`/`digest` don't
  ship — same standard as supersession.
- **Summary-of-summaries quality decay.** A session checkpoint over
  turn digests is a third compression pass. The
  `Established/Open/Files touched` shape with evidence handles is
  the mitigation —
  claims degrade but pointers don't; if eval shows digest detail
  loss, the session checkpoint can reach raw events on demand.
- **Per-run generation cost.** One small-model call per turn is the
  price of `digest` mode; the eval arm exists to prove it pays for
  itself. `stub` mode is the zero-cost fallback that still captures
  most of the savings.
