# 2026-10-15 — Verify-loop edge validated; resolver chain fixed; coverage made engagement-conditional

> **Status note (10-17, #343):** the 8-cell `held-*` pool this record
> calls "held" is development-contaminated — the verify-loop edge was
> designed from harm diagnosed on its run #2 and validated on the same
> cells; resolver fixes iterated across its l3 runs 3–5; coverage rules
> changed (#337) after one of its cells starved. The pool is relabeled a
> **validation pool**: every result below is **in-sample** evidence, and
> "86/88 vs 78/88" is a **cross-invocation, cross-binary contrast —
> informal, not paired**. A sealed blind pool is #224's scope; edge
> claims get re-run as same-invocation contrasts once it exists.
> The #225 replication resolved to a partial DeepSeek read (invocation
> `20261015T013755Z-4ea7`: 121/121 pass both arms, discovery −14.4% on
> completed cells, coverage-FAILed by the #348 provider bug — now fixed
> via #381, rerun unblocked but deprioritized: a cross-model read on a
> validation pool carries validation-pool weight).

Experiment record: held-memory verify-loop arm + held-memory-l3 runs
3–5. Companion to
[2026-10-09-roadmap-value-and-use](2026-10-09-roadmap-value-and-use.md),
which stays the plan of record.

## What was measured

**held-memory-verifyloop** (`ambiguity_clarification` added to the
held-memory treatment, 8 cells, powered): gate **PASS**, treatment
86/88 vs run-#2's 78/88. The verify-loop edge (#329/#331) fired
exactly where the harm was diagnosed — `verify-loop.fired` ×10 in
`en-bb-ambig`, ×10 in `en-decoy`, ×6 in `en-ft-ambig`, ×1 in
`en-veto` — converting the run-#2 timeouts into passes. One
`headless-degraded` exercised the escalation-degrade path on decoy.
This is the first full harm→edge→recovery loop closed on evidence.

**held-memory-l3** (4 cells, `resolver_calls: 4`, `turn_context` off):

- Run-3: all 88 treatment records coverage-starved — the resolver
  abstained on every consult. The #330 instrumentation worked as
  designed: a dead layer reported inconclusive, not PASS.
- Run-4 (#334/#335/#336 merged): **33/33 consults resolved**, all
  engaging cells conclusive and clean; discovery −33.3% (6.86→4.58)
  vs the 35% MDE — a real but under-bar effect.
- `held-mix-cs-ambig` starved *structurally*: its prompts resolve at
  L0–L2, so the gate never opens and `tail.resolver` is absent — a
  coverage-design flaw, not a resolver failure. Fixed by #337
  (`tail.resolver.dead` — engagement-conditional health coverage).
- Run-5: rerun under the corrected coverage.

**held-memory-qwen** (#225): second-model replication launched on
`opencode-go/qwen3.8-max` — the protocol is now frozen (edges merged,
resolver deterministic, coverage honest), satisfying the issue's
gate.

## Root cause — the resolver dead-layer saga (#333)

Three stacked defects, three fixes:

1. **10s consult timeout vs ~20s reasoning-model latency** — every
   consult died on deadline (#334 → 60s).
2. **No small→large fallback** — the resolver's small model
   auto-resolved to `claude-haiku-4-5` from an ambient, zero-credit
   `ANTHROPIC_API_KEY` leaking through eval subprocess env; every
   consult returned 400 instantly. `GenerateTitle` already had the
   fallback; the resolver now mirrors it (#335).
3. **Ambient small-model selection** — manifests pinned only
   `model large`; `small` resolved from operator env, so the same
   experiment differed by machine. `small_model` manifest field
   renders `model small` into the pinned `.crushrc` (#336); l3 +
   verifyloop manifests pin `opencode-go/deepseek-v4.1-flash`.

## Corrected record — Phase 2 mostly shipped

The 10-09 roadmap predates the merges. Actually closed since:

- #294 acceptance labels + propensity (PR #302)
- #295 `crush eval calibrate` (LTT)
- #296 per-project Beta-Binomial shrinkage (`project_rates`)
- #321 selector silence (rescue×veto)
- #227 sub-ceiling corpus

Open Phase-2+: #297 bandits (needs propensity-logged volume from
#293), #298 procedural memory, #299 unified context budget.

## Claim-hierarchy updates

| Claim | Status |
|---|---|
| Run-loop pathologies are detectable and repairable mid-run | **Validated in-sample** (#343) — designed from attributed harm on the same pool it was measured on; recovered 8/10 prior timeouts, firings attributed per-cell; sealed claim awaits #224 + same-invocation rerun |
| A dead consult layer is visible, not silent | **Proven** — `tail.resolver` stamps made run-3's 125-abstain corpse legible; `dead` coverage now fails closed on it |
| L3 verdicts steer selection when the layer works | **First evidence** — resolver-engaged cells show −33% discovery; under the 35% bar, mix-cs adds placebo stratum |
| Generalizes across models | **Partial directional read** — DeepSeek replication: 121/121 pass both arms, discovery −14.4% on completed cells, coverage-FAILed by the #348 provider bug (fixed via #381; rerun unblocked, deprioritized — a validation-pool read carries validation-pool weight) |
