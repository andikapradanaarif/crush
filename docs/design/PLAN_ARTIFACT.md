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

`session.Todo` → `PlanItem{ID, Content, DependsOn []ID, Status,
Evidence []string}`:

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
    path→dir). Binding is to the **latest instance** of each
    name. An evidence name that never materializes is a third
    state — **evidence unmet**, distinct from failed — reported
    to the model as "no check of that name has run" rather than
    a failure it can retry against.
  - `EvidencePaths []string` — the model-known kind, and it does
    carry a weak done-ness rule: **write landed on the path AND
    no check covering it failed**. The path→covering-check map
    (`package-test:<dir>` included) is the harness's job, not the
    model's vocabulary. Same paths double as annotation: an item
    bound to `internal/agent/` lets a repair/replan edge render
    the current symbols of the files it names
    (`CONTEXT_PREFETCH.md`) into the prompt — the plan carries
    its own map.
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
- `ID` ownership: the model rewrites the whole list per call and
  identity today is keyed on mutable `Content`. **The harness
  mints IDs on write** (model-authored IDs collide); rewrites
  must preserve IDs for unchanged/matched items — match by
  content hash, so the legacy `session.Todos` shim can mint the
  same way. `DependsOn` is validated on write: reject cycles,
  self-deps, and dangling refs — validation, not the excluded
  DAG scheduler. Whatever consumes `DependsOn` later snapshots
  it at dispatch — the model can rewrite the plan (and DAG)
  mid-run.
- **Structural confidence — mapped to the right seams.**
  `phase-confirm` graduates from "any `todos` call resolves" to a
  gate that validates _what_ the plan declares: at declaration
  time (tool validation + the scope-gate resolution check) every
  item must bind evidence (files or checks) — a plan of bare
  strings no longer satisfies the gate. **Open items do not block
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
  `client_workspace.go`, `ui/chat/todos.go`, `ui/model/pills.go` —
  `PlanItem` fields need the same path (or the UI keeps the flat
  view and drops the new fields). `tools.TodosToolName` is
  hard-coded in `incompleteTodos` (`verify_gate.go:410`) and
  `scopeGate.observe` (`scope_gate.go:125`) — a `plan` rename
  touches both.
- The model still writes the plan (the `todos` tool graduates, or
  a `plan` tool replaces it with a migration shim reading old
  `session.Todos`). The harness gains structure to _check_; it
  does not gain a planner that replaces the model. That is the
  side of the line the production tools landed on, and it keeps
  prompt-mode cost at zero for tasks too small to plan.

## Migration

- `todos` graduates vs. `plan` replaces — decide at implementation;
  either way a shim reads legacy `session.Todos` so in-flight
  sessions don't orphan.
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
