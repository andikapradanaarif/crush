# Plan Artifact — Typed Plan Items

> **Status:** Spec. Split from `HARNESS_TOPOLOGY.md` — the typed
> plan object the harness can check.
>
> **Depends on:** nothing — `session.Todos` exist today and the
> gate reads them. `phase-confirm` already shipped (#43, calibrated
> autonomy); this doc is what upgrades it.
> **Ship when:** the confirmation needs structure flat todos can't
> carry — evidence binding ("item done ⇔ its checks green"), open
> items blocking the gate — or fan-out needs `DependsOn` dispatch
> units.
> **Measured by:** one definition of done (unified gate trigger),
> eval trajectory assertions on plan state, replan-turn quality.

## Goal

Give the harness a plan it can _check_: typed items with
dependencies and evidence, so "is this done" and "may this execute"
are structural questions about an artifact — not self-reported
model judgment. The model keeps writing plans; the harness gains
the ability to verify them.

`PlanItem` is unrelated to upstream's plan _mode_ (`AgentPlan`,
`plan.md.tpl`): the plan agent produces human-facing prose for
user approval and cannot write `PlanItem`s (`resolvePlanTools`
excludes `todos`). Two systems named "plan" coexist — one the
user confirms, one the harness checks. Seeding typed items from
an approved plan-mode plan is a deliberate follow-up, not part of
this work.

## Problem

`session.Todo` is `{Content, Status, ActiveForm}`
(`session.go:34-38`) — flat strings, no dependencies,
model-managed. The run-end todos edge (`run_edges.go:334`) can
ask "are items open?" and nothing more structured. `phase-confirm`
is weaker than "render the list, ask yes/no" implies: the gate
asks a fixed question and treats **any** `todos` or `question`
call as resolved without inspecting content
(`scope_gate.go:125`) — plan-declaration is plan-confirmation. The
gap is not that the gate can't render structure; it is that the
gate cannot _validate_ what a plan declares — no evidence
binding, no "which item is unresolved", no structural confidence.
`BACKGROUND_SUBAGENTS.md` needs a dispatch unit; today the dispatch
spec is a prompt string the model composes inline in the `agent`
tool call, with no handle the harness can track, dedup against, or
verify.

## Design

`session.Todo` → `PlanItem{ID, Key, Content, DependsOn []ID, Status,
EvidenceChecks []string, EvidencePaths []string}`:

- `DependsOn` is the dispatch unit for fan-out: independent
  subtrees are what a background subagent may be handed, and the
  join edge (`BACKGROUND_SUBAGENTS.md`) reads "outstanding" off
  the same structure.
- `Evidence` is **two kinds, distinct fields** — not one
  `[]string` the done-definition sniffs prefixes on:
  - `EvidenceChecks []string` — gate-bearing: the item is
    `completed` only when the named checks resolved green.
    **Only configured check names are bindable** — `verify:<name>`
    comes from config, so the model can know it at plan time.
    `package-test:<dir>` is minted per write from the edited
    file's path (`verify_checks.go:59`) — the model can only
    _predict_ it, so it is never bound by name; its coverage is
    derived from `EvidencePaths` instead (the harness maps
    path→dir). **The names must be surfaced before they can be
    bound:** configured check names live in tool-result
    `ClientMetadata` the model never sees — binding is impossible
    until they render somewhere visible (tool description or a
    config-rendered block). That's a PR 1 requirement: the tool
    schema ships with the vocabulary visible or the model invents
    unbindable names and eats validation errors. Binding is to the
    **latest instance** of each name, session-scoped — the
    done-scan reads _stored_ `verification` metadata
    (`writeVerificationOutcomes` maintains it), not run-local
    `in.result.Steps`, so a check that ran two turns ago covers an
    item and a repair retry's fresh steps don't reset the clock.
    This is also how the two edges unify: the done-definition
    consults persisted check outcomes directly — no resolved-map
    threading through `edgeInput` needed (today
    `resolveVerificationEdge`'s resolved verdicts never reach the
    todos edge's scan). An evidence name that never materializes is a third
    state — **evidence unmet**, distinct from failed — reported
    to the model as "no check of that name has run" rather than
    a failure it can retry against.
  - `EvidencePaths []string` — the model-known kind, and it does
    carry a weak done-ness rule: **write landed on the path AND
    no check covering it failed**. The path→covering-check map
    (`package-test:<dir>` **and `diagnostics`** — the third check
    kind, minted per write when LSP covers the file
    (`verifying_tool.go:164`); a new-error delta on the path is
    exactly per-path evidence) is the harness's job, not the
    model's vocabulary. `unverified` (LSP doesn't cover the
    file) is not failed — it doesn't block under the weak rule;
    keep `unverified` / `unmet` / `failed` distinct in gate
    feedback — three near-synonyms that must not collapse.
    Supersession is per-write-path: a later write to the same
    path with a `diagnostics` entry replaces the verdict, and a
    write carrying _no_ `diagnostics` entry (a bash redirect
    rewrite) clears it — optimistic by design, since latching
    until a checked write would make bash-heavy fixes
    unresolvable; the cost is a `cat > a.go` that preserves the
    errors reads as resolved until the next checked write.
    Known bounds, same as the verify gate's: `package-test` is
    Go-only, so non-Go trees reduce to "write landed"; mutating
    bash (`sed -i`, redirects, `go generate`) and multi-file
    workspace edits (`lsp_rename`, `lsp_replace_symbol`) leave no
    path metadata an `EvidencePaths` binding can observe. Same
    paths double as annotation: an item bound to `internal/agent/`
    lets a repair/replan edge render the current symbols of the
    files it names (`CONTEXT_PREFETCH.md`) into the prompt — the
    plan carries its own map.
    Bash evidence bounds: the redirect scan masks quoted spans,
    `[[ ]]` tests, arithmetic, and heredoc bodies before reading
    `>` operators, and only records concrete targets — `~/out`,
    `$OUT`, globs, and substitutions mutate but yield no path
    (they still count as mutation for the scope gate; the two
    vocabularies deliberately differ on expansion targets).
    Residual blind spots: nested-paren arithmetic, `]` inside a
    `[[ ]]` body, and heredoc delimiters outside `[A-Za-z0-9_]`.
    Bindings outside the working directory (`../x`, absolute) are
    legal — evidence binding is not a permission — and a bash
    redirect there satisfies them.
    Two accepted loosenesses: the evidence scan is
    **session-lifetime** — a write from an earlier turn can
    satisfy a binding declared later, so the evidence proves "a
    write happened," not "this item's work happened"; and binding
    is a **checkpoint-time nudge, not an invariant** — once the
    scope gate resolves, a bare rewrite can strip `evidence_*`
    fields and unbound completed marks count as done. Both are
    deliberate: the gate pressures declaration-time structure,
    it does not police post-resolution plan hygiene.
    Two more bounds, same deliberate kind: only **successful**
    tool results record writes — a failed bash call's redirect
    may still have created its target, but failed work is not
    evidence; and the UI's incomplete-todo pill is **mark-only** —
    an evidence-blocked completed item counts as done there even
    while the run-end gate queues a repair turn, a cosmetic
    divergence the retry prompt explains.
    **Diagnostics attribute to the write's path only** —
    `VerificationCheck` carries no path field, so a write to
    `a.go` that introduces errors in `b.go` never blocks an
    `evidence_paths: ["b.go"]` item. The run-level verification
    edge still catches the failure, so this is per-item
    granularity, not a silent miss — fixing it needs a path field
    plus a `detail` parse contract (or per-affected-file
    diagnostics entries) in the LSP delta computation.
    This unifies today's two gate triggers (failed checks, open
    todos) into one definition of done instead of two scans of the
    same run — and **done-ness evaluates on final state, not
    latch**: a check that went green then regressed on a later
    write reopens the item at the run-end edge. "Completed" is a
    property of the clean-stop state, not the mark-time snapshot.
- **Status is model-marked; effective state is derived.** The
  model marks `completed`; the effective state is `marked ∧
evidence` — a completed mark with pending/failed/unmet
  evidence is _evidence-blocked_, not rejected (rejecting the
  write would hide the plan from the gate). The run-end edge
  reports the override with its reason ("marked completed but
  `verify:build` failed / has not run") so the model sees the
  divergence — an invisible override would re-mark every turn:
  thrash.
- `ID` ownership — and the authoring handle it requires: the
  model rewrites the whole list per call and identity today is
  keyed on mutable `Content`. **The harness mints IDs on write**
  (model-authored IDs collide), but minted IDs alone make
  `DependsOn` unauthorable: on the first write no IDs exist, so
  "B depends on A" can't be declared — exactly the moment
  declaration-time validation inspects the plan — and rewording
  a depended-upon item would re-mint its ID and strand inbound
  refs the model can't repair (it can't predict the new ID).
  The model-side handle is an optional **`key` field** — a
  model-authored slug, validated unique within the submitted
  list. `DependsOn` in the tool input references **keys**, which
  the write maps to minted IDs. Preservation: key match first
  (a kept key survives rewording → same ID → inbound refs stay
  valid), content-hash fallback for legacy/keyless items — the
  shim mints the same way. Validation rejects duplicate keys,
  identical-content items (ambiguous dep targets), cycles,
  self-deps, and dangling refs — with the offending key named so
  the model can repair in the same call. Whatever consumes
  `DependsOn` later snapshots it at dispatch — the model can
  rewrite the plan (and DAG) mid-run.
- **Structural confidence — mapped to the right seams.**
  `phase-confirm` graduates from "any `todos` call resolves" to a
  gate that validates _what_ the plan declares: at declaration
  time (tool validation + the scope-gate resolution check) every
  item must bind evidence (files or checks) — a plan of bare
  strings no longer satisfies the gate, and an **empty list does
  not resolve it either**: `todos: []` vacuously satisfies "every
  item binds evidence," so without the non-empty requirement
  "declare nothing" becomes the cheapest gate-resolution. Bounced
  declarations are **bounded, not infinite**: a ~2–3-bounce budget
  then escalates to the real scope question with stuck-loop
  context ("repeatedly declared plans that don't resolve —
  proceed without a declared plan?") — the edge-exhaustion shape,
  escalation never pass-through (passing after N bounces would
  teach the spam-bypass). The budget is also the safety valve for
  an _unrepairable_ bounce — a requirement the model can't see
  (evidence vocabulary unsurfaced) is a guaranteed loop. Bounce
  count is exported telemetry: a high rate means the model can't
  conform — vocabulary invisible, schema too strict — versus a
  weak model; silent bouncing hides the difference. **Open
  items do not block
  `phase-confirm`** — at the first-write boundary every item is
  open by definition, so blocking there deadlocks every plan.
  Open-items-block lives at the **run-end todos edge** (PR 2's
  unified done-definition) where "all items resolved" is a
  coherent demand. Executor confidence becomes a property of the
  artifact — the harness inspects the plan, it never asks the
  model to self-report a percentage.
- Persistence: open items already survive — `session.Todos` lives
  on the session row and re-renders into every turn's context
  (`<open_todos>`, `turn_context.go:115`). What compaction loses
  is _history_: completed items, dependencies, evidence bindings —
  a `plan` notebook entry type preserves the plan's structure as
  ground truth for replan and hydration, instead of re-deriving it
  from rendered todo tool calls.
- **Plumbing surface is wider than the tool.** `session.Todo`
  fans out to `proto.Todo`, `server/events.go`,
  `client_workspace.go`, `ui/chat/todos.go`, `ui/model/pills.go`,
  `ui/chat/tools.go` (three `TodosToolName` routes) —
  `PlanItem` fields need the same path (or the UI keeps the flat
  view and drops the new fields). `tools.TodosToolName` is
  hard-coded in `incompleteTodos` (`verify_gate.go:410`),
  `scopeGate.observe` (`scope_gate.go:125`), the default
  allowed-tools list (`config.go:1053` — a rename that misses it
  silently strips the tool), the coder system prompt
  (`coder.md.tpl:21`), and `buildSummaryPrompt`
  (`agent.go:3134`). `todosRetryPrefix` is fingerprinted by
  `RepairPromptPrefixes` (`eval/analyze.go:687`) — changing the
  retry-prompt text, or adding an `evidence-blocked` report
  section, breaks eval turn segmentation without a fingerprint
  update. Synergy worth noting: `notebookRelevanceRefs`
  (`notebook_selection.go`) already reads open todos for file
  refs — `EvidencePaths` can feed it directly.
- The model still writes the plan (the `todos` tool graduates, or
  a `plan` tool replaces it with a migration shim reading old
  `session.Todos`). The harness gains structure to _check_; it
  does not gain a planner that replaces the model. That is the
  side of the line the production tools landed on, and it keeps
  prompt-mode cost at zero for tasks too small to plan.

## Migration

- `todos` graduates vs. `plan` replaces — decide at implementation;
  either way a shim reads legacy `session.Todos` so in-flight
  sessions don't orphan. **`plan` is a loaded name**: upstream's
  `plan` agent/mode (`plan.md.tpl`, v0.95.0) already claims it —
  a `plan` tool inside a `plan` agent is confusing in prompts and
  logs. The same upstream arrival is the integration point: plan
  mode produces an _approved_ plan as text while `resolvePlanTools`
  excludes `todos`, so its plan never becomes checkable —
  seeding `PlanItem`s from the approved plan on handoff makes the
  human-confirmed plan the artifact the gate then checks
  (tracked separately — text→typed conversion is lossy enough to
  need its own design pass, not a follow-up fix).
- The plan format is model-visible — expect `bands.json`
  re-characterization after merge (same confound class as the #39
  clause: corpus authored under the old surface).

## Non-goals

- No planner model — the model writes the plan; the harness checks.
- No mandatory planning — `PlanItem` must never make a one-line
  fix pay a planning turn. The plan-free path stays below the same
  scope threshold `phase-confirm` uses.
- No DAG evaluation — `DependsOn` is a dispatch/readiness input,
  not a scheduler; the loop kernel still routes.

## PR ordering

1. `PlanItem` schema + `todos`/`plan` tool + migration shim —
   harness-minted IDs (content-hash match on rewrite), DependsOn
   validation, `EvidenceChecks`/`EvidencePaths` split.
2. Gate reads the typed list — unify failed-checks and open-items
   into one done-definition **at the run-end edge** (final-state
   evaluation; regressed checks reopen items). The todos-edge
   retry prompt can order ready-before-blocked items off
   `DependsOn` — its first consumer, before any fan-out.
3. `phase-confirm` upgrade: declaration-time validation —
   evidence-bound items required for the gate to resolve.
   Open-items-block stays at the run-end edge, never here.
4. `plan` notebook entry type — compaction-surviving ground truth.

Note: PR 1 makes the new fields model-visible before PR 2 reads
them — two `bands.json` perturbation points. Landing 1+2
together costs one; if they ship separately, mark the new fields
experimental until the gate reads them.
