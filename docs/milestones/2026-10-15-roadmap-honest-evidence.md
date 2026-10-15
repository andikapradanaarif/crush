# 2026-10-15 — Roadmap: honest evidence before new capability

**Plan of record.** Supersedes
[2026-10-09-roadmap-value-and-use](2026-10-09-roadmap-value-and-use.md),
which stays as the record of the learning-method design era — the
stack it specified was built and is correct machinery. This plan
reorders around what two reviews found wrong with *what feeds and
certifies* that machinery.

## Product claim

> The harness reuses verified project experience to reduce repeated
> work, while detecting uncertainty, respecting current intent, and
> bounding harm from stale or irrelevant memory.

## Why this rewrite

Two reviews on 10-15 audited what the 10-09 plan produced:

- **The learning loop as built** (#302 labels+propensity, #305 rates,
  #306 calibrate/LTT). Finding: the statistics are implemented
  correctly — LTT follows the paper, gating is on the posterior lower
  bound, one outcome rule is shared across fold/prior/calibrator.
  What fails is the **input**: the acceptance label is unstable and
  biased (open-ended commit window, whole-file survival hashing,
  agent's own commits self-certifying, revised-then-committed counted
  as acceptance), and the loop has **no data** — `referent_episodes`
  holds 0 rows across all 25 nexus dogfooding sessions because the
  episode shape (vague prompt, single edit) is one real usage rarely
  takes.
- **The procedural-memory design** (PR #339 v1). Finding: its mining
  filter depended on that empty label, so Phase A's census would have
  returned empty *by construction* — a vacuous falsification measuring
  the label gap, not the channel. Plus whole-session exact-hash
  clustering that cannot collide, path-stripping that erases the
  procedure, and a bijection forcing skills to narrate exploration.

Both reviews were verified against code and `crush.db` before acting
on them — every blocking claim held.

## The corrected record

Set down honestly, since several claims in this directory drifted
ahead of the evidence:

- **The held-memory pool's seal is broken.** Per #324's own protocol
  ("run as-is; divergences get analyzed, not tuned"): the verify-loop
  edge was designed from harm diagnosed on held-memory run #2, then
  validated on the same cells; coverage was tuned (#337) after a held
  cell starved. The 86/88 and the −12.5%/−33.3% discovery reads are
  **validation-pool results — in-sample, not held-out evidence.**
  The pool is relabeled a validation pool; "Proven on verify-loop"
  downgrades to "validated in-sample." A new blind pool is required
  for any sealed claim (#343).
- **Cross-invocation comparisons are informal.** "86/88 vs 78/88"
  spanned different binaries and invocations. Edge claims need
  same-invocation paired contrasts (#343).
- **First replication is partial, not failed-on-merit.** DeepSeek run
  (invocation `20261015T013755Z-4ea7`) coverage-FAILed: 4/8 cells
  starved on a provider bug — `reasoning_content` not round-tripped
  multi-turn (#348). The conclusive slice: **121/121 pass on both
  arms, zero timeouts; discovery −14.4%** at powered n on the
  completed cells. Directional replication of the muse-spark shape
  (−12.5% pooled), not the accepted verdict. Decision pending:
  fix #348 and rerun, re-pin, or accept (#225).
- **The loop's data claim was aspirational.** All memory flags
  default off; telemetry is opt-in; the label pipeline produces
  `unknown`, not evidence, on the dominant turn shapes. Layer B has
  the machinery and nothing to fold.

## What doesn't change

The 10-09 stack survives review intact — keep it:

- LTT with Hoeffding–Bentkus p-values, fixed-sequence testing,
  honest ≥78-admit data requirement.
- Lower-bound (not mean) gating, per-session decay, leave-one-out
  prior, fail-closed on uncomputable bounds.
- One shared outcome rule, NULL-unobserved, propensity fields
  reserved for the bandit layer.
- The route-space contract — now extended to the rate gate itself
  (#341, PR #347: `tail.referent_gate` with closed verdicts
  `admit | rate_below_floor | gate_error` + propensity slot).
- Pre-registration, sealed-pool discipline, honest-null posture.
  The reviews' sharpest cuts were places we violated our *own*
  stated rules — the rules were right, the discipline slipped.

## What learns (revised)

| Variable | Evidence source | Issue |
|---|---|---|
| Render limits, floors, TTLs — `params.Memory` fields | certified values, fixed-sequence tested | #295 (merged #306; first cert pending #342) |
| Per-project rates | Beta-Binomial + decay, lower-bound gating | #296 (merged #305) — inputs fixed by #340 |
| **Turn-level acceptance labels** | settled verdicts at fixed window: hunk survival, agent-commit exclusion, divergence-beats-commit | **#340 — the critical path** |
| Selector admit decisions | first LTT certificate — labeled corpora give known verdicts today, no production wait | #342 |
| Referent phrase→target maps | settled labels; per-mapping posterior refinement noted | #294 → #340; P6 on #229 |
| Procedural skills | mined accepted **turns**, core clustering, ground-truth census | #298 (design v2, PR #339) |
| Channel context allocation | LOO marginal values | #299 — data-blocked |
| Online arm selection | propensity-logged, few hundred events/arm | #297 — data-blocked |
| User-level priors | ≥2 projects screened; dispersion + capped strength when built | #229 |

## Execution plan

Milestones mirror phases; new issues from the reviews are folded in.

### Phase 1 — Real use and honest labels

| Work | Status |
|---|---|
| **#340 labels + `turn_outcomes`** | open — **the critical path**: fixed observation window, hunk-level survival, agent-commit exclusion, divergence ordering, re-label pass, plus turn-level labels at the unit mining needs |
| **#293 dogfooding** | running — 25 sessions / 662 command rows / 135 failures / 45 telemetry lines on nexus; episodes 0 → proves #340's necessity |

### Phase 2 — The learning loop

| Work | Status |
|---|---|
| **#341 gate decision records** | **PR #347 open, checks green** — `tail.referent_gate` + coverage + starvation guards |
| **#342 first LTT cert on selector decisions** | open — labeled corpora supply known verdicts at volume today |
| #296 rates / #295 calibrate | machinery merged; blocked on #340 labels for referents |
| #297 bandits | data-blocked — needs propensity-logged volume |
| #229 user-level tier | parked; carries P2 (dispersion, capped strength, decay) + P6 (per-mapping posterior) notes |

### Phase 3 — New memory capability

| Work | Status |
|---|---|
| **#298 procedural memory** | design v2 under review (PR #339): turn episodes, procedure-core clustering, two-level signatures, ops + gates G1–G6. Phase A = labels + miner + census scored on nexus ground truth (`backend-add-entity`, `shared-ts-sync-api`); recall-or-stop |
| #299 unified context budget | data-blocked |
| #216 resolver | L3 merged (#328); follow-ups closed |
| #166 file_heat map | parked |

### Phase 4 — Evidence where memory should matter

| Work | Status |
|---|---|
| **#343 seal discipline** | open — relabel pool as validation, author new blind pool, same-invocation paired rerun, downgrade claims |
| **#344 manifests** | **PR #346 open, clean** — three manifests + invocation IDs |
| **#345 env allowlist** | open — eval subprocesses get only manifest-referenced vars (the #333 ambient-key incident generalized) |
| **#332 abstain split** | open — failure vs budget-exhaustion sub-causes |
| #227 sub-ceiling corpus | open — ahead of ladder spend |
| #226 SWE-bench | open |
| #198 re-verification | standing |
| #223 ladder rerun | parked |

### Phase 5 — Ship and parity

| Work | Status |
|---|---|
| **#348 deepseek provider bug** | open — blocks replication reruns |
| #225 replication | partial directional read; verdict pending rerun decision |
| #224 sealed pool | open — now strictly required (current pool is validation) |
| Default flips | gated on a genuinely sealed read |
| #78 sandbox / #3 subagents | open |
| #232 dispatch | merged (#327) |

## Sequencing

| Step | Contents | Gates on |
|---|---|---|
| **1** | Merge open PRs (#346 manifests, #347 gate records); **#340** labels + `turn_outcomes` | — |
| **2** | **#345** env allowlist; **#343** seal + new blind pool + paired rerun | manifests in |
| **3** | **#342** first LTT cert on selector corpus | — |
| **4** | **#298 Phase A** — miner + census vs nexus ground truth | #340 (G1) |
| **5** | #225 verdict: fix #348 + rerun, re-pin, or accept directional | pool decision |
| **6** | Procedural B/C; #297; #229 | Steps 1–5, data volume |

## Standing rules (carried forward, two added)

- Pre-register primary comparison + decision rule before powered
  spend; name the primary metric.
- Learned labels and parameters fail closed: unparseable evidence is
  `unknown`; a gate that can't fire resolves absent.
- Default flips ship through the release policy with a powered
  on/off — Phase 5 or nothing.
- `memory_params` changes get `param_version`; evidence carries its
  version.
- Notebook stays parked.
- **NEW: a sealed pool stays sealed.** Cells used to design or tune
  an edge are validation cells thereafter; claims restate as
  in-sample. New claims need a blind pool nobody iterated on.
- **NEW: contrasts are same-invocation or they're informal.** Edge
  validations pair arms within one manifest/invocation/binary;
  cross-invocation comparisons are narrative, not evidence.
- **NEW: a falsification must be able to find something.** An
  experiment whose input filter provably yields empty is not a test —
  Phase A-type censuses get scored against known ground truth first.
