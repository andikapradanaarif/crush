# Route Space — Enumerated Paths, Learned Dispatch

> **Status:** Reference. Defines the contract every harness decision
> path must satisfy: routes are enumerable, exits carry
> closed-vocabulary reasons, and dispatch — not topology — is what
> learns. Sibling to `HARNESS_TOPOLOGY.md` (local-only — untracked;
> the control-flow skeleton:
> loop kernel + named edges); this doc owns the *decision* skeleton —
> what the harness injects, gates, or resolves, and which legal path
> each input travels. The four-tier learning model and what may adapt
> inside these routes: `docs/milestones/2026-10-04-roadmap-fixed-skeleton.md`.

## The contract

Five rules. All five are load-bearing for auditability:

1. **Routes are enumerable.** Every context-and-memory decision
   path's legal routes — the checks, their order, the exits — are
   listed here or in a doc this one links. A path that exists in code
   but not here is a bug in one of them; the doc is diffable precisely
   so that drift is visible. Scope, stated plainly: mid-run decision
   paths (permissions/hooks, the pressure gate, the summarization
   trigger) are not yet enumerated — the contract applies to them when
   they land, not retroactively. Enumerability itself is today
   enforced by review discipline; making it a property of the dispatch
   code's shape (a table-driven check list) is #232.
2. **Exits carry a closed-vocabulary reason.** A decision record
   answers "which check fired" with a stable string evals and
   dashboards can group on (`tail.decisions.reason`). A new exit
   means a new vocabulary entry, added deliberately — never a free
   string.
3. **Guards are deterministic or validated-model.** A guard is either
   a pure function of observable inputs, or a model call with closed
   output + deterministic validation + abstain-on-failure (the L3
   pattern, below). Never an unbounded scorer ranking arbitrary
   paths — "the weights said so" is not an auditable reason.
4. **Dispatch tunes; topology doesn't.** Learned params adjust guard
   thresholds — *when* a branch fires (per project, per the #228
   store). Adding, removing, or reordering a check is a mechanism
   change and must travel the evolution loop: proposal → offline
   benchmark → powered read → acceptance gate.
5. **Every exit fails closed.** Unparseable input, missing data,
   unsupported language, guard error — the default is always the
   conservative exit (suppress / abstain / inconclusive), never the
   permissive one.

## Who may choose a branch

| Dispatch mechanism | Verdict |
|---|---|
| Deterministic guard — pure function of observable inputs | Auditable always; preferred |
| Validated model layer — closed output schema + deterministic validation + abstain on failure | Allowed; the model picks among *enumerated* options and the guard checks its answer |
| Learned scorer ranking arbitrary paths | Forbidden — no deterministic check can catch "the weights drifted wrong" |

## Route space 1 — open-failure selection (live)

`internal/agent/failure_select.go` — per-candidate dispatch over the
open-failure set, once per turn per row. The route forks once
(explicit scope vs ambiguous prompt), then runs an ordered check list
where the **first disqualifying check wins** and its reason is
recorded:

```
fetch: ListOpenFailures(ctx, 50) ── candidate pool = freshest 50 rows
     (bounded for fetch cost; wide enough that a relevant row past
      the render cap still earns a decision record)

prompt ──→ scope extraction ──┬── explicit scope ──────────┐
                              └── ambiguous (no scope) ────┤
per candidate row:            │                            │
  1. negatedByAny        → negated_scope   (veto — first,  │
     outranks everything; unrecognized text defaults to    │
     exclusion, not grant)                                 │
  2. explicit && miss    → out_of_scope                    │
  3. explicit && kind ∉  → kind_mismatch*                  │
     {test,build,lint,run}                                 │
  4. ambiguous && no     → referent_none                   │
     failure referent                                      │
  5. ambiguous && kind   → kind_mismatch*                  │
     ∉ referent kinds                                      │
  6. ambiguous && non-   → narrow_scope                    │
     top-level && ≠run                                     │
  7. all paths absent    → path_gone                       │
  8. implicated path     → stale_suspect                   │
     newer than last_seen                                  │
  else                   → admit                           │

render cap (post-selection, recent-first order):
  bound rows ≤ cap       → renders into <open_failures>
  bound rows beyond cap  → render_capped (admit=false —
                           cut by budget, not by the prompt)

* kind_mismatch is emitted by two different checks (3, 5), so
  tail.decisions can't distinguish which site rejected — split into
  explicit_kind_mismatch / referent_kind_mismatch tracked in #232.
```

Guards: all deterministic today — regex scope extraction, lexicon
polarity, kind table, path existence, mtime. The English lexicon may
only *grant* scope; text it cannot read defaults to exclusion.

Learned-param slots (once #228 exists): referent-kind table,
stale-suspect window, scope lexicon entries. The check order and the
exit set are topology — not params.

Observability: every candidate's exit is in `tail.decisions`
(`{signature, cmd, admit, reason}`); eval arm predicates group on the
reason vocabulary.

## Route space 2 — eval lifecycle (live)

`internal/eval/runner.go` — fixed positions; **order is semantics**:

```
materialize → arm config → prior_sessions (seeds) → seed gate →
measured run → preserve artifacts → check.sh → coverage/verdict
```

| Position | What may conclude there |
|---|---|
| seed failure | `error` — fixture could not be built |
| seed gate reject (clean exec, assertion failed) | `inconclusive` — wrong starting state, measured run never launched |
| seed gate infra failure | `error` — `seed_check_error`, feeds the fixture-config breaker |
| measured run | `pass`/`fail`/`error`/`timeout` via check + verdict fields |

A gate that ran after the measured session would not be a gate. The
positions are the skeleton; each gate's *script* is a corpus variable.

## Route space 3 — layered resolver (#216, L1 landed)

The selector's three decisions (relevance / selection / veto) shrink
into an escalation cascade — each layer may only resolve what earlier
layers left open:

```
L0 candidate-set structure ──→ resolve singleton/top-level, or escalate
L1 language-neutral ids ─────→ paths, basenames, Test\w+ vs headline
L2 artifact promotion ───────→ working set + cmd recency; promotes
                               unmentioned candidates only — never
                               resurrects a typed-but-unparseable token
L3 small-model resolver ─────→ closed output {about_failure,
                               include_paths, exclude_paths, kind},
                               deterministic validation, cached,
                               abstains on failure
L4 question tool ────────────→ interactive sessions only
```

Every layer has the same exit set: resolve / veto / escalate. L3 is
the sole model guard and obeys rule 3 — closed schema, validated,
abstain-on-failure. The binding benchmark scores per-layer: admit
precision/recall, veto violations (target 0), abstain rate.

Landed: each decision carries `settled_by` (`identifier` / `lexicon` /
`state`) so per-layer volume is countable via
`tail.decisions.settled.*`; L1 binds headline identifiers
(`Test\w+`-shaped tokens) whose mention span carries recognized
signal, vetoes negated mentions, and leaves typed-but-unparseable
mentions for L3/L4 — never binding them. The lexicon coverage hole is
measured: `lang_unsupported` replaces `referent_none` when the prompt
carries letters outside English. The offline benchmark is
`internal/agent/testdata/failure_binding.jsonl` scored by
`TestBindingBenchmark` — per-language admit precision/recall, veto
violations hard-gated at 0.

## Route space 4 — run edges (control flow)

Catalog lives in `RUN_EDGES.md`; the "why specified transitions" is
`HARNESS_TOPOLOGY.md` (local-only — untracked). Same contract,
different layer: edges are named transitions the model cannot route
around (verification, todos-reconcile, stall-replan, escalate-human,
phase-confirm, burn-watch, reconcile shipped; join-subagents,
summarize-continue specified). The loop kernel itself — the
model's own tool choices inside a turn — is deliberately *not* in the
route space: the skeleton wraps the model, it does not replace its
autonomy.

## Adding a route — checklist

- [ ] Enumerate the new path: where it forks, which checks run, in
      which order.
- [ ] Extend the closed reason vocabulary — one stable string per new
      exit.
- [ ] Record it: decision records must carry the new reason (the
      vocabulary is the audit contract).
- [ ] Add offline benchmark cells covering each new exit — including
      the fail-closed defaults.
- [ ] If any guard is learned: param slot in the #228 store, with
      provenance + version.
- [ ] Topology change → evolution-loop acceptance, not a drive-by.

## Non-goals

- No learned routing over unbounded path sets.
- No runtime topology change — new branches are mechanism changes.
- The route space constrains *harness-injected* structure (what to
  inject, when to gate, which layer resolves). It does not script the
  model's intra-turn choices — that is the kernel's domain and the
  reason the kernel stays a loop.
