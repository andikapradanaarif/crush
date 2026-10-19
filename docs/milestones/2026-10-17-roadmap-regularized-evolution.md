# 2026-10-17 — Roadmap: regularized harness evolution

**Superseded by [2026-10-19-roadmap-evidence-calibrated-evolution](2026-10-19-roadmap-evidence-calibrated-evolution.md)**
— same direction; a full-text read of the literature replaced the OOD
gate (SWE-bench Verified → SWE-bench Pro V2), the primary endpoint
(pass rate → tokens-to-done at pass non-inferiority), the v1 mutable
surface, and added a matched-budget baseline arm.

**Plan of record.** Supersedes
[2026-10-15-roadmap-honest-evidence](2026-10-15-roadmap-honest-evidence.md),
whose integrity rules all carry forward. This plan changes *what the
fork bets on*: from certifying per-project memory knobs (correct
statistics, no data at single-user volume) to an offline, regularized
loop that evolves the harness itself and is judged on tasks it never
tuned on. Memory channels become components that loop can tune or
delete.

Reviewed before adoption: a proposal draft was fact-checked against
the repo and RRSI v3; the review's corrections are folded in and
listed under [Review corrections](#review-corrections).

## Product claim

> The harness improves itself under evidence: it proposes changes to
> its own prompts, tools, run edges, and memory from traces, keeps
> only changes that clear a noise-adjusted, cost-justified gate and
> hold on tasks it never tuned on, and lands each as a reviewed PR.
> Per-turn memory reuses verified project experience; the user's own
> edits are captured as the substrate for user-level memory.

The bounded-autonomy principle from `VISION.md` applies to the
harness itself: evolution runs free inside the loop, and every change
that persists crosses a human confirmation (the PR).

## Why this rewrite

### The record as of 10-17 (verified against `crush.db` and results)

| Fact | Evidence |
|---|---|
| Real usage does not feed the learning loop | 348 user turns across nexus, valo, soulmate, agora; `referent_episodes` = 0 and `project_rates` = 0 in all four |
| `turn_outcomes` has no data yet | Binary at `5c9ec18` installed 10-18 00:14; nexus has the table, 0 rows; newest nexus session predates the build and settlement waits `created_at + 24h` |
| Certification cannot be fed | #342: 55 draws split 10/16/29 by class vs ~78 needed per class |
| The corpus sits at the pass ceiling | Control arm: expdisc 29/32, 21/21, 21/21; held-memory 22/22 or 22/23; held-memory-l3 64/64, 55/55, 46/48, 44/45 |
| #227 closed without meeting its acceptance band | Stated control ~40–70%; closed 10-13 on a powered null with control ≈ 1.00 |
| The verify-loop edge is in-sample evidence | Designed and validated on the same held cells (#343); control already 22/22; the edge recovered timeouts the memory treatment caused |
| Eval history size | 3,759 run records, 3,551 preserved artifacts across 34 result dirs |

### What the literature says

- **Harness evolution improves coding agents** with a frozen model —
  reproduced by independent groups: SICA (17% → 53% on a SWE-bench
  Verified subset), DGM (SWE-bench 20% → 50%), Meta-Harness (beats
  hand-engineered harnesses on TerminalBench-2), RRSI (+6.0 on
  Terminal-Bench 2.1).
- **Unregularized evolution mostly does not transfer.** RRSI Table 1
  (agentic workspace domain): four prior methods all gain on the
  evolve split; out of distribution Meta-Harness adds +0.9, HarnessX
  lands on the base, AHE and TTHE end below it. Removing RRSI's
  regularizers (Table 2) drops the OOD average 43.6 → 40.3 and raises
  tokens/trial 2.42M → 3.80M.
- **Regularized gains on strong models are small but real.** RRSI
  coding: SWE-bench Verified +1.8, 95% CI [+1.1, +4.0] (paired
  task-level bootstrap, Table 6); two independent search runs agree
  within the CI (Table 7); gains hold across two policy families
  (Table 3) and transfer to an unseen *weaker* model (Table 4).
- **What survives regularization is our kind of mechanism.** RRSI's
  accepted coding edit is "a bounded pre-completion verification
  audit" (Table 8, R0-A, +3.93 on the evolve set, a round-0 bundle);
  rejected candidates include accumulated prompt text fitted to the
  evolve scorer. That is the class of our verify-loop and reconcile
  edges — built by hand, one round at a time.

### What we already own

| RRSI component | Exists here | Gap |
|---|---|---|
| Noise band δ from repeated base runs | `eval/noise.json`, power gate, baseline runs | Express δ in passes per evolve set (RRSI E.1 definition) |
| Floor `Ŝ(H′) ≥ S* − δ` | Same-invocation paired contrasts, `decision_rule` pre-registration | — |
| Cost rule `ΔC ≤ β₀ + β₁·ΔS` | #197 rule (`EVAL_HARNESS.md` ~L1694–1715) | Revisit: the circularity objection held while S was an efficiency metric; with S = pass rate, β₁ is a token allowance per extra pass (RRSI Table 5) and is coherent |
| Leakage critic before scoring | Sealed pools, injection + seed-contamination screens | Diff-reading critic |
| Structural pruning | #198 re-verification, notebook default-off precedent | Automated deletion targets |
| Evidence ledger | `tail.decisions`, preserved session DBs, manifests + invocation IDs, `param_version` | Candidate-edit ledger |
| Attributable edits | One mechanism per PR, route-space contract | Annealed edit budget |
| Proposer | The owner, by hand | **Core build** |
| Evolve set / OOD split | Corpus at ceiling; no public-benchmark adapters | **Core build** |

## Confidence — what we bet on and what we do not

| Claim | Confidence |
|---|---|
| Automated harness evolution yields gains on coding tasks | High — four independent groups |
| Without regularization those gains mostly fail to transfer | High — RRSI Tables 1–2 (same base, budget, policy) |
| We own most of the selection side | High — mapped above |
| Evolution removes the single-user data-volume problem | High — the sample is a benchmark, not usage |
| We can *measure* transfer on our setup | **Unknown — Gate G1 decides** |
| Evolution on the owner's personal task mix works | Unproven — no published result found |
| It beats frontier products on raw capability | Low — expect single-digit points on strong models |
| Mined procedural skills help on coding | Low–medium — SkillsBench: self-generated skills no benefit; curated +4.5pp in software engineering |

## Architecture — three loops

### Loop 1 — per-turn memory (exists; scope frozen)

Failure memory stays on for the owner (proven efficiency effect).
Other channels (`referent_memory`, `session_memory`, notebook) are no
longer developed as learners; they become components Loop 2 may
enable, tune via `params.Memory`, or prune. The LTT calibrator,
`project_rates`, and bandit design stay in-tree, parked.

### Loop 2 — regularized evolution (`crush evolve`; new core)

**Mutable surface (allow-list, enforced in code):**

- v1 — text and config only: system prompt templates
  (`internal/agent/templates/*.md.tpl`), tool descriptions
  (`internal/agent/tools/*.md`), `params.Memory` values, memory channel
  flags, skill bodies.
- v2 — after 2–3 clean manual rounds and #78: run-edge Go code
  (`RUN_EDGES.md` mechanisms) under the sandbox.

**Immutable skeleton (never proposable):** safety ordering, veto
precedence, project partition, verdict vocabulary, `internal/permission`,
`internal/hooks`, `internal/eval`, measurement contracts. This is the
fixed-skeleton rule from 10-04; RRSI has no equivalent and we keep it.

**Round structure (RRSI §3, App. D):**

1. *Analyze* — failure feedback from the incumbent's evolve-set traces.
2. *Propose* — Crush as a task agent with filesystem access to code,
   scores, and full traces of every prior candidate (Meta-Harness)
   plus the ledger, where rejected hypotheses persist as negative
   evidence. Edit budget anneals `b_max = 3 → b_min = 1`
   (RRSI Eq. 4); `m = 2` candidates per round, one reserved for
   unexercised components when progress over `w = 3` rounds stays
   within δ.
3. *Screen* — critic rejects diffs encoding task IDs, fixture paths,
   verifier contents, expected outputs, or inert machinery — before
   scoring, so a leak never earns a score.
4. *Score* — evolve set, `k = 2` trials per task, same invocation as
   the incumbent.
5. *Select* — S = pass rate, C = tokens-to-done (the `VISION.md`
   context metric). Floor `Ŝ(H′) ≥ S* − δ`. If `ΔS > δ`:
   `ΔC ≤ β₀ + β₁·ΔS`. If `|ΔS| ≤ δ`: accept only if `ΔC < 0` (the
   existing within-band rule; RRSI D.3's novelty term not adopted
   in v1). β₀, β₁ fixed from opening rounds, never retuned.
6. *Prune* — components with no positive measured gain over
   `n_prune = 4` rounds become deletion targets for the proposer.
7. *Land* — each accepted edit becomes a PR to the fork with its
   ledger entry; the owner merges or rejects.

**Splits:**

- **Evolve set:** Terminal-Bench 2.1 (89 tasks, containerized,
  verifier-scored; RRSI's coding evolve set). Primary route.
- **OOD gate:** SWE-bench Verified, sized by the paired-binary power
  calculation below for a pre-registered MDE. The only credible gate
  for gains.
- **Blind pool (#224):** regression and leakage guard only. At tens of
  tasks it cannot gate a gain (CI half-width ≈ ±5pp at 40 tasks).
- Holdouts are **never consulted during search** (RRSI E.1) and are
  read once per pre-registered decision. No Thresholdout — its
  guarantees weaken at small holdout sizes, and single use is simpler
  and matches the sealed-pool rule.

**Statistics:**

- Uncertainty is a paired task-level bootstrap (resample tasks, not
  trials; RRSI E.2).
- Holdout size: `N ≈ (z₁₋α/₂ + z₁₋β)² · σ_d² / Δ²`, with σ_d the SD of
  the per-task paired difference. Planning prior from RRSI's SWE-bench
  CI (assuming the full 500): σ_d ≈ 0.165 ⇒ N ≈ 534 for Δ = 2pp,
  ≈ 134 for 4pp, ≈ 86 for 5pp (α = 0.05, power 0.8). Replace the prior
  with σ_d measured base-vs-base on our harness before G1.
- The existing CV-based power gate sizes continuous metrics; the
  paired-binary sizing is new code (N3).
- **Underpowered is inconclusive, never null.** A CI that spans zero
  at the pre-registered MDE says "no effect ≥ MDE detected", not
  "the method fails".

**Search vs production model:** if search runs on a cheap policy,
every accepted edit is re-validated on the production model at the
OOD read. RRSI shows transfer to a weaker model, not from a weaker
search model to a stronger one.

### Loop 3 — personal (capture now; build last)

- **Now:** at settlement, store per hunk the agent's added lines and
  the post-window content of the same region (bounded, redacted via
  `internal/redact`, project-local). Edits not captured now are lost
  for good; this is the input user-level memory needs (N8).
- **Phase 5:** CIPHER-style preference tier (#229 rescoped): infer
  natural-language preference descriptions from (agent output, user's
  final) pairs, retrieve by context, promote across ≥2 projects with
  #229's screen. Metric: revision rate and edit cost over weeks.
- **Phase 5:** revised or failed real turns become evolve-task
  candidates via snapshot replay (#108) where tests exist.

## Gate G1 — can we measure transfer at all?

Pre-registered before any Phase 1 spend. All must hold to take the
**commit** branch; any failure takes the **fallback** branch.

| Criterion | Threshold |
|---|---|
| Terminal-Bench 2.1 adapter runs Crush headless end-to-end | ≥ 85/89 tasks without infra error |
| Base pass rate under the chosen search policy | 0.30–0.80 |
| δ from 3 base passes (RRSI E.1 spread) | ≤ 3pp (RRSI coding: 1.7pp) |
| SWE-bench Verified adapter at N sized for pre-registered MDE ≤ 5pp | Feasible (images, runtime, spend) |
| Projected spend (pilot + OOD read) | Within a budget the owner sets before Phase 1 |

**Commit branch:** Phases 2–5 below.

**Fallback branch:** no self-evolving or transfer claims. Keep the
ledger + selector + critic as engineering discipline for hand-made
mechanism changes on the existing corpus, Loop 1 as is, and Loop 3
capture. Revisit G1 when benchmark access or budget changes.

## Phases

| Phase | Work | Exit |
|---|---|---|
| **0 — now** | Dogfood ~1 week on `5c9ec18`+ before reading `turn_outcomes`; freeze the label pipeline (fix only bugs seen in real rows); stale RRSI v1/v2 readings annotated in the 10-01/10-04 records (this PR; #343's relabel already landed in `c26b892ce`); N8 edit capture | `turn_outcomes` settles rows on nexus |
| **1 — feasibility** | 1a: run the existing corpus under a cheap policy as a diagnostic only (correlated fixture families, weak→strong risk); 1b: Terminal-Bench 2.1 adapter spike, 3 base passes → δ; 1c: SWE-bench Verified adapter scope (#226); 1d: paired-binary sizing (N3), σ_d measured | **Gate G1** |
| **2 — ledger + manual rounds** | Ledger, round runner, critic, allow-list, selector with S/C as above; 2–3 rounds with owner-written candidates. Selector validation with candidates whose effect is *measured on the evolve set*: a planted leak (critic must reject before scoring), a measured-harmful edit (must fail the floor), a no-op edit (must not be accepted as gain) | Selector verdicts agree with measured effects; ledger clean for 2–3 rounds |
| **3 — automated pilot** | Proposer automated; `T = 10`, `m = 2`, `b 3 → 1`, `w = 3`, `n_prune = 4`. One OOD read at the end | **Positive:** OOD CI lower bound > 0 → Phase 4. **Inconclusive:** CI spans 0, no regression → Phase 4 replicate before any conclusion. **Negative:** OOD regression → stop automated search, keep the ledger discipline |
| **4 — replicate + transfer** | Independent second search run; production-model validation if searched on a cheap policy; cross-model read (replaces #225) | Two runs both inconclusive at the MDE → publish "no effect ≥ MDE at this budget" and take the fallback branch |
| **5 — personal loop** | Preference tier from captured edits (#229); real failures → evolve-task candidates | Revision rate / edit cost trend over ~4 weeks; flat → keep Loop 2, drop the preference tier |

**Spend estimate (commit branch):** Terminal-Bench evaluation =
89 × k=2 = 178 trials. Base δ: 3 × 178 = 534. Pilot: 10 rounds × 2
candidates × 178 = 3,560. OOD read at N = 500, k = 1, two arms:
1,000. Total ≈ 5,100 trials — more than the entire preserved eval
history (3,759 records). Terminal-Bench and SWE-bench trials are
long; the per-trial cost is measured in 1b before G1.

## Issue dispositions

| Issue | Disposition |
|---|---|
| Label-pipeline follow-ups | Frozen — only bugs observed in real rows |
| #297 bandits, #299 context budget | Parked — volume-blocked |
| #342 LTT selector cert | Parked — machinery stays (may later certify evolved values) |
| #298 procedural memory | Rescoped — skill bodies are a Loop 2 component; curated vs mined vs none experiment once Loop 2 exists; Phase A as designed dropped (census ground truth too thin: 5 nexus sessions mention the hand-written skills, which live in `.devin/skills`, a path Crush does not scan) |
| #229 user-level tier | Rescoped — CIPHER-style preferences from captured edits; Phase 5 |
| #227 sub-ceiling corpus | Closed without meeting its band; superseded by the Terminal-Bench evolve set |
| #226 SWE-bench Verified | **Critical path** — the OOD gate |
| #224 sealed pool | Re-scoped — regression and leakage guard |
| #343 seal discipline | Docs half landed (`c26b892ce`); pool half → #224 |
| #198 re-verification | Becomes Loop 2 pruning |
| #78 sandbox | Promoted — required before v2 surface and unattended search |
| #225 replication | Replaced by Phase 4 cross-model read |
| #293 dogfooding | Continues on current binary |
| #360 worktree facet | Unchanged, low |
| #3 subagents, #166, #216 follow-ups | Parked |
| Notebook stack (#107, #110, #139, #140) | Stays parked |

**New issues to file:**

- **N1** Terminal-Bench 2.1 adapter for `crush eval`.
- **N2** SWE-bench Verified adapter (fold into #226).
- **N3** Paired-binary holdout sizing in the power gate.
- **N4** `crush evolve` ledger + round runner.
- **N5** Leakage critic (pre-score diff screen).
- **N6** Mutable-surface allow-list, enforced.
- **N7** Automated proposer with trace filesystem (after Phase 2).
- **N8** Capture agent-vs-final hunk content in `turn_outcomes`.
- **N9** #197 cost rule: proportional β₁ when S is pass rate.

## Standing rules

Carried forward:

- Pre-register the primary comparison and decision rule before
  powered spend; name the primary metric.
- Learned labels and parameters fail closed.
- Default flips ship through the release policy with a powered
  on/off.
- `memory_params` changes carry `param_version`.
- A sealed pool stays sealed.
- Contrasts are same-invocation or they are informal.
- A falsification must be able to find something.
- Notebook stays parked.

Replaced:

- ~~No adaptive search on dev fixtures~~ (10-09) → **Adaptive search
  only on a declared evolve set, under RRSI's regularizers (edit
  budget, pre-score critic, noise floor, cost rule, pruning); holdouts
  are never consulted during search and are read once per
  pre-registered decision.** The original rule's intent — the sealed
  pool is the only gate for anything adaptive — is preserved.

New:

- **Underpowered is inconclusive.** Never close an effect as null at a
  size that could not detect the pre-registered MDE.
- **Issues close on their acceptance criteria**, not on a run verdict
  (#227 closed on a powered null without meeting its band).
- **Search model ≠ production model ⇒ production validation** of every
  accepted edit before merge.
- **Every persistent harness change crosses a human confirmation.**

## Review corrections

The proposal draft that preceded this roadmap was reviewed against the
repo and RRSI v3. Folded in:

- The binary was already current (`5c9ec18`); the "165 commits behind"
  claim was stale.
- #227/expdisc and held cells are at the ceiling; the evolve set must
  come from a public benchmark or new tasks, not a widened corpus.
- δ ≤ 5pp on ~120 trials was underpowered; accepted RRSI edits are
  worth ≈ +2.5–3.9 points, inside such a band. G1 now requires
  δ ≤ 3pp and sizes the OOD gate by power calculation.
- The verify-loop edge is not a known-good candidate on a sub-ceiling
  evolve set; Phase 2 uses candidates measured on the evolve set.
- RRSI's "4 of 32 accepted" and the Table 2 ablation are agentic
  workspace results; the recovery-hint case is engineering design.
- Thresholdout dropped for single-use sealed holdouts.
- C = tokens-to-done made explicit to match `VISION.md`.
- The 10-09 "no adaptive search" rule is explicitly replaced, above.
- Loop 3 deferred except edit capture, which cannot be recovered later.

## References

Read in full:

- Xia et al. "RRSI: Regularized Recursive Self-Improvement of Agent
  Harnesses." arXiv:2609.24972, v3 (2026-10-04).
  github.com/google-research/rrsi.

Abstract / search level:

- Lee et al. "Meta-Harness: End-to-End Optimization of Model
  Harnesses." arXiv:2603.28052 (2026).
- Zhang et al. "Darwin Gödel Machine: Open-Ended Evolution of
  Self-Improving Agents." ICLR 2026, arXiv:2505.22954.
- Robeyns, Szummer, Aitchison. "A Self-Improving Coding Agent." ICLR
  2025 SSI-FM workshop, arXiv:2504.15228.
- Agrawal et al. "GEPA: Reflective Prompt Evolution Can Outperform
  Reinforcement Learning." ICLR 2026, arXiv:2507.19457.
- Chen et al. "HarnessX." arXiv:2606.14249 (OOD figures via RRSI
  Table 1).
- Dwork et al. "The reusable holdout." *Science* 349 (2015),
  doi:10.1126/science.aaa9375 (considered, not adopted).
- Angelopoulos et al. "Prediction-powered inference." *Science* 382
  (2023), doi:10.1126/science.adi6000 (optional label validation).
- Angelopoulos et al. "Learn then Test." *Ann. Appl. Stat.* (2025),
  arXiv:2110.01052 (parked machinery).
- Gao et al. "Aligning LLM Agents by Learning Latent Preference from
  User Edits." NeurIPS 2024, arXiv:2404.15269.
- "SkillsBench." arXiv:2602.12670 (2026); self-generated condition
  in v1.
- Xiong et al. "How Memory Management Impacts LLM Agents." ACL 2026,
  arXiv:2505.16067.
- "SWE-Exp: Experience-Driven Software Issue Resolution."
  arXiv:2507.23361.
- Ziegler et al. "Productivity Assessment of Neural Code Completion."
  MAPS 2022, doi:10.1145/3520312.3534864.
