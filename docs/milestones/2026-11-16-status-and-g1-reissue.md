# 2026-11-16 — Status against the 11-09 roadmap, measurement audit, proposed G1′ re-issue

**Type:** status + proposed amendment to the plan of record
[2026-11-09-roadmap-improves-with-use](2026-11-09-roadmap-improves-with-use.md).
It does **not** supersede it: phases, targets and standing rules stay.

**Status:**

- §2, §3 and §5 are a record (status, findings, work plan).
- §4 is a **proposal**. It takes effect only when the owner signs off on
  decisions D1–D5, recorded on #397.
- Per the roadmap's §4 rule, thresholds changed after seeing data take
  effect only through this kind of dated amendment.

Inputs:

- GitHub state on 11-16: milestones R0–R5, issues, PRs #459–#503.
- Code at `58836e8` (the installed daily-driver build).
- Raw pilot records in `eval/results/tb21-pilot-beam/` (148 records).
- [11-12 temp-0 probe](2026-11-12-exp-temp0-determinism-probe.md).
- #489 and its merged slices #500 and #501.

---

## 1. Summary

1. **Code is far ahead of evidence.**
   - R1's building blocks are all merged: the Harbor adapter, split
     freeze, cluster bootstrap, noise and sizing tooling, best-of-k, and
     all six lever arms.
   - **No lever has been read.**
   - The lever issues #440–#445 were closed when their code merged, but
     R1 exits only when "each lever [is] accepted or dropped". The
     tracker is now #512.
2. **The only external measurement so far, the pilot, measured a
   different system.**
   - Every pilot record ran with small model `deepseek-v4.1-flash`.
   - Temperature was declared but never sent.
   - pilot-b2's `treatment` arm is a duplicate of its `control` arm.
3. **The statistical reasoning in #489 needs correcting, and the merged
   sizing fix (#501) blocks every pass-rate rule on TB2.1.** Repeated
   cells are still required, and the arithmetic changes what can be
   afforded:

   | Claim type | Cost | Verdict |
   |---|---|---|
   | Token savings (efficiency) | −16% MDE for 480 trials | affordable |
   | Pass rate (capability), 5pp | about 1,900 trials per lever | not affordable |
4. **L6 (reversible folding) cannot fire on Harbor.** `fold_turns` folds
   finished user turns, and a Harbor trial is a single prompt. The L6
   read designed for #493 would be an A/A. The fix is #509.
5. **Model pin.** The owner intends to return to `deepseek-v4.1-flash`
   (the daily driver and the 11-09 roadmap's solver), pending a 20-trial
   smoke (#506).
6. **CI has not run since 11-03** (#511).

## 2. Status by phase (11-16)

Legend: ✓ met · ◐ built, not measured · ✗ not started · ⊘ blocked.

### R0 — signal integrity (exit: R0.1 + R0.2 + R0.3)

| ID | Target | State | Evidence |
|---|---|---|---|
| R0.1 | H0 ≥ 80% `ok` on every active project | ◐ | Fleet tooling: #436, #466/#467, #474. 11-14: 14 registered, **4 pass**, 0 fail, 10 pending. #496 closed: pending rows are stale-binary data and designed abstentions, not capture defects. Daily driver is now `58836e8` (post-#481). **"Active project" is undefined (D4)** |
| R0.2 | Edge-authored messages never mint turns | ✓ | #437 (#468), #458 (#459); 11-09 audit found 7 repair messages and 0 repair-authored outcomes |
| R0.3 | Blind label pass, per-class precision (L1) | ◐ | #438 (r03): accepted **8/8**; revised 2/8, failed 0/2, unknown 1/14 (definition mismatch) → fix round 1 is #481. Re-measure #482 needs ≥ 8 non-accepted post-#481 rows; none yet (D5) |
| R0.4 | Volume on clean rows (tracking) | ◐ | #293. Pre-fix: 202 settled / 43 accepted; post-fix still accumulating |
| R0.5 | Two weekly v2 trend rows with edit cost on ≥ 2 projects | ◐ | #403 shipped (`crush stats trend`); edit cost is null fleet-wide until post-#433 turns settle |
| R0.6 | Correct the 11-07 comment; register aurum and borneo | ✓ | #293, 11-13 |
| R0.7 | Label-pipeline freeze | ✓ | — |

**R0 has not exited.** R3 stays blocked on it.

### R1 — anchor + cheap levers

| ID | Target | State | Evidence |
|---|---|---|---|
| R1.1 | ≥ 85/89 TB2.1 trials without infra error | ◐ | Adapter #388/#477, CA fix #485, timeout multipliers #486. Measured ceiling about 93% (`mteb-retrieve` image defect) → exclusion path #500. Never run at fleet scale |
| R1.2 | Base characterization: pass, δ, tokens CV, `c_min` | ◐/⊘ | Compute: #478, #500, #501. Data: pilot 4×30 on `beam-free` (**superseded**, §3.1) plus the temp-0 probe (4 conclusive pairs). k=8 manifest #503 **on hold** (#504, #506) |
| R1.3 | TB2.1 dev/sealed split frozen | ✓ | #439/#473: seed 439, 59 dev / 30 sealed, hash-verified |
| R1.4 | N1a: Crush vs Terminus 2 | ✗ | No tracker since #388 closed → **#510** |
| R1.5 | Six lever arms, each accepted or dropped | ◐ | Built: #465, #469, #470, #471, #464, #472. All off by default except L5, which has **no switch**. 0 read → **#512**. L6 blocked → **#509** |
| R1.6 | Matched-budget best-of-k arm | ◐ | #497–#499 merged; #405 open; not run |
| R1.7 | Contamination probe (gates SWE-Pro) | ✗ | #226. Moot unless SWE-Pro is wanted |
| R1.8 | Projected spend within budget | ⊘ | #397 deferred 11-11. Cost is never recorded → **#505** |
| G1′ | Gate record | ⊘ | #397's criteria table is still the 10-19 version → §4 |

### R2 — long-horizon core

| ID | State | Notes |
|---|---|---|
| R2.1 sandbox (#78) | ✗ | Promoted 10-17; no work since |
| R2.2 LoopsBench adapter (#446) | ✗ | — |
| R2.3 edge params (#408) | ✓ | #494 |
| R2.4 arm hooks/tools blocks (#409) | ✓ | #495 |
| R2.5 observed/inferred tags (#447) | ✓ | #492 |
| R2.6 EGM experiment (#448) | ✗ | — |
| R2.7 engine-fit refresh (#449) | ✓ | #491 |
| #493 notebook keep/excise | ⊘ | Framing corrected on the issue; decision read is #509 C2 |

### R3–R5

Not started; all of them are gated:

- **R3** (#450–#454, #399, #298, #229) waits on R0's exit.
- **R4** (#390–#393, #406) waits on G1′.
- **R5** (#396, #224) waits on R4.

## 3. Findings (11-16 audit)

### 3.1 The pilot measured a different system

| Defect | Evidence | Consequence |
|---|---|---|
| Wrong small model | `env.model_small = opencode-go/deepseek-v4.1-flash` on every pilot record; the pin was `beam-free` | Compaction, titles and other auxiliary calls ran on another model |
| Temperature provenance | `env.temperature` comes from the manifest (`ingest.go` L285 → L626). Records say `"0"`, but per #489 the pilot sent no temperature (fixed in #486) | The provenance field is false |
| One trial in two arms | pilot-b2 `treatment` = `control`: 29/29 identical `trial_dir`. The dedup key includes the arm and covers only the current invocation | Any cross-arm read on b2 has σ_d = 0. #503 has two identical arms, so the same mistake would yield a fake perfect A/A |
| Infra drift between passes | b1/b2: 7 `NonZeroAgentExitCodeError` each (CA-less images, before #485). b3/b4: 0 | Each pass's conclusive set covers a different set of tasks |
| Cost not recorded | `cost_usd` comes only from Harbor's `AgentResult`; `crush_agent.py` never sets it | `cost_trials: 0`. R1.8 can't be measured on a paid pin |

Fixes: #504 (dedup, pin gate, temperature provenance, b2 purge,
superseded noise block) and #505 (cost).

### 3.2 What the pilot data does say (control arm)

| Quantity | Value |
|---|---|
| Tasks with ≥ 2 conclusive reps | 27: **7 always-fail, 9 always-pass, 11 mixed** |
| p̄ | 0.51 |
| σ_within (mean unbiased pᵢ(1−pᵢ)) | 0.373 (w̄ = 0.139) |
| σ_between | 0.342 |
| Within-task SD of log total tokens | **0.69** |
| Legacy `c_min` / `tokens_cv` | 0.87 / 1.41: dominated by runaway trials (max 11.5M tokens) |

Corrections to #489:

- **The Bernoulli term is a ceiling, not a floor.** For two identical
  runs, Var(dᵢ) = 2pᵢ(1−pᵢ) ≤ 2p̄(1−p̄). The measured σ_d (0.50–0.56)
  matches √(2·0.139) = 0.53.
- **δ = 23pp is ordinary sampling noise at n ≈ 21.**
  - σ ≈ 8.1pp per pass.
  - The expected range of 4 passes is 16.8pp.
  - The measured `delta_solve` is 17.2pp.
- **δ ≤ 3pp is reachable.** The expected 3-pass range is
  1.693·√(w̄/(n·k)):

  | (n, k) | Expected range |
  |---|---|
  | (30, 8) | 4.1pp |
  | (59, 8) | 2.9pp |
  | (88, 5) | 3.0pp |

  That's about 1,400 trials. #489's "n ≈ 1,000" assumed p(1−p) = 0.25
  for every task.
- **The temp-0 probe is uninformative.**
  - 1 flip in 4 pairs has a 95% CI of 0.6%–81%.
  - The tasks were selected on flip history.
  - Temperature and the small model changed at the same time.
- **Of tasks with ≥ 3 conclusive runs, 8/16 flipped**, not 11/19.

### 3.3 Sizing: the estimand, not the noise, is the problem

#501 plugs σ_between in as effect heterogeneity. On the pilot matrix:

| k | N tasks, +5pp superiority | N tasks, −3pp non-inferiority |
|---|---|---|
| 8 | 477 | 1,043 |
| ∞ | ≥ 367 | ≥ 820 |

TB2.1 has 89 tasks. Lever selection on the fixed dev split is
**conditional** inference: the average effect on those tasks, where only
within-task noise enters. Heterogeneity matters only for generalizing to
new tasks (the sealed split).

The ≤ 542 cap was "642 − 100 SWE-Pro" and is obsolete under the 11-09
held-out design. Fix: #508, reusing the existing `stratified` resample
mode.

**MDE** (minimum detectable effect) under the conditional estimand on
pilot noise, two-sided α = 0.05, power 0.8:

| Protocol | Pass MDE | Token MDE | Trials (2 arms) |
|---|---|---|---|
| n = 30, k = 4 | 13.5pp | −22% | 240 |
| n = 30, k = 8 | 9.5pp | −16% | 480 |
| n = 59, k = 8 | 6.8pp | −11% | 944 |
| n = 59, k = 16 | 4.8pp | — | 1,888 |

The 11-09 §4 expectations were:

- advisor +2–8pp
- fresh-context verifier 0–5pp
- folding −15% to −40% tokens

So only folding clears a 480-trial read. Pass-rate levers need either
screening reads at about 10pp MDE or roughly 1,900 trials each.

Token noise is not yet decomposed in `harbor.cells`, and `c_min` must
move to a log scale (#507).

### 3.4 L6 and #493

- **`fold_turns` never fires in a single-prompt run.**
  `coveredPriorTurns` takes only turns below `countUserMessages`, with
  every segment covered, and the set is frozen at the first render.
  Harbor runs one `crush run` prompt, and repair rows stay inside that
  turn. ReFold folds sub-tasks *inside* one trajectory, which is the
  segment grain.
- **`fold_turns` is built on the notebook** (notebook + recall +
  coverage), so "excise the notebook" would delete L6. The real choice
  is the generative tier vs the mechanical tier. `notebook_generate=never`
  keeps deterministic coverage.
- **Turning the notebook on enables the generative tier by default:**
  `notebook_generate` defaults to `always`, `notebook_checkpoint`
  follows `notebook_enabled`, and hydration defaults to `true`.
- **TB2.1 can't test pressure** (the models declare about 1M context).
  #493's decision needs the 64K internal-corpus read.

Fix and read design: #509. The decision framing is on #493.

### 3.5 Process

- CI stopped on 11-03 (#511). About 40 PRs merged on local tests only.
  `go test ./internal/eval -short` is green at `58836e8` (335s).
- #489 and #482 had no milestone; they are now R1 and R0.
- Untracked `arm-longcat-free.crush.json` has no `models` pin. Using it
  would repeat the small-model contamination.

## 4. Proposed G1′ re-issue (owner sign-off required)

This replaces #397's 10-19 criteria table and the #489 amendments with
one gate aligned to the 11-09 §8 G1′ (R1.1, R1.2, R1.3, R1.8).

| ID | Criterion | Threshold | Source |
|---|---|---|---|
| G1′.a | Infra completion on the k-rep batch | ≥ 90% of scheduled trials on **non-excluded** tasks conclusive; exclusions recorded with reasons (`harbor.exclude`); rate-limit deaths requeued, not counted | R1.1, #500 |
| G1′.b | Base pass rate (conclusive-normalized, cell matrix) | 0.30 – 0.80 | R1.2 |
| G1′.c | Model-based δ̂ at the **declared scoring protocol (n, k)** | ≤ 3pp; every scored candidate must use the same (n, k) | R1.2, #507 |
| G1′.d | Token MDE at the scoring protocol | ≤ 20% (`c_min_expected` recorded) | R1.2, #507/#508 |
| G1′.e | Split frozen | met | R1.3 |
| G1′.f | Projected spend R1–R4 from measured $/trial | ≤ owner ceiling (D2) | R1.8, #505 |
| G1′.g | Estimators | `cluster` for population (sealed) claims; `stratified` with k ≥ 2 for conditional (dev) decisions | #407, #508 |

Removed:

- **G1.b (SWE-Pro)**: returns only through R1.7.
- **G1.e/f (≤ 542)**: replaced by G1′.c and G1′.d.
- **G1.g**: merged into G1′.f.

Verdict branches:

- **Commit — full:** all criteria hold **and** pass MDE ≤ 5pp at an
  affordable protocol. R4 runs with capability and efficiency claims.
- **Commit — efficiency:** a, b, d, e, f, g hold but pass MDE > 5pp.
  - R4 selects on the §6 efficiency path, with a pass guardrail at the
    measurable margin.
  - Capability claims come only from screening wins replicated on the
    sealed split.
  - The 11-09 R2 kill branch ("claim narrows to efficiency only")
    becomes the planned branch.
- **Fallback:** d or f fails → the 11-09 fallback (levers ship on paired
  dev reads plus replay; no transfer claims).

### Owner decisions

| ID | Decision | Recommendation |
|---|---|---|
| D1 | Model pin (and route) | `deepseek-v4.1-flash`, adopted only if #506 passes its pre-registered table. Large and small pinned to the same model. Route chosen for cached-read pricing (see #505) |
| D2 | Spend ceiling, R1 and R4 | Set in USD once #506 measures $/trial. R1 ≈ 4,100 trials (§6) ≈ $66–$500 at catalog rates |
| D3 | Claim posture for pass levers | Efficiency-first. L1, L3 and L4 as pre-declared **screening** reads (MDE ≈ 10pp; inconclusive is not null). Upgrade a screening win to a 5pp read only if it shows |
| D4 | Define "active project" for R0.1 | e.g. ≥ 3 real-user sessions in the trailing 14 days on a post-#433 binary. Others are reported, not gating |
| D5 | Deadline for #482 | If ≥ 8 non-accepted post-#481 rows haven't accrued by the chosen date, close R0.3 by **retiring** non-accepted classes (11-09 kill rule). Learning then uses `accepted` + captured hunks only (accepted precision 8/8), and R3 can start |

## 5. Work plan

Each workstream has one issue and one PR. **Agent** means an
implementing agent can do it end to end. **Owner** means it needs a
Docker host, credentials, paid spend, or a decision.

| W | Issue | What | Who | Depends on | Can start |
|---|---|---|---|---|---|
| W1 | #504 | Ingest integrity: cross-arm/invocation dedup, pin gate, temperature provenance, b2 purge, superseded noise block | Agent | — | **now** |
| W2 | #505 | Per-trial cost from pinned route rates | Agent | — | **now** |
| W3 | #506 | deepseek TB2.1 smoke (10 tasks × k=2): manifest + runbook → owner run → dated record → D1 | Agent → Owner → Agent | W1, W2 (preferred) | prep now |
| W4 | #507 | Log-token cell decomposition, model-based δ̂ / `c_min_expected` | Agent | — | **now** |
| W5 | #508 | Conditional estimand, retire 542, MDE report | Agent | W4 for token terms | **now** (pass part) |
| W6 | #509 | L6: verify the no-op, segment-grain fold, pinned mechanical arms, 64K pressure manifests | Agent | — | **now** |
| W7 | #510 | N1a: foreign-agent ingest, model parity, manifest + runbook | Agent → Owner | W1, D1, W9 | prep after W1 |
| W8 | #511 | CI not running since 11-03 | Agent (+ owner settings) | — | **now (first)** |
| W9 | #503 / #404 | k-rep characterization on the D1 pin: 30 tasks × k=8 × 2 identical arms (480), both arms in every daily slice | Owner run, agent analysis | W1, W3/D1, W2 | after D1 |
| W10 | #397 / #489 | G1′ verdict under §4 with every input number; close #489 | Owner + agent | W4, W5, W9 | after W9 |
| W11 | #512 | Lever reads in order L6 → L3 → L4 → L1 → L2; L5 decided on design grounds | Owner runs, agent analysis | W6, W10 | after W10 |
| W12 | #493 | Keep/excise the generative tier from the #509 C2 read; dispose of #453 | Owner + agent | W6 (C2 run) | after C2 |
| W13 | #482 / #293 / #403 | R0: label wave when rows accrue; D4/D5 | Agent (analysis) | D4, D5 | ongoing |

### Critical path

```
W8 CI ─┐
W1 ────┼─► W3 smoke (owner) ─► D1 ─► W9 k-rep (owner) ─► W10 G1′ ─► W11 levers ─► R1 exit
W2 ────┘                                  ▲                            ▲            (N1a W7 +
W4 ─► W5 ─────────────────────────────────┘                            │             sealed read)
W6 ─────────────────────────────── (C1 TB arm, C2 pressure read) ──────┘─► W12 #493
R0: W13 (independent; gates R3)
```

**Parallel now (agents):** W1, W2, W4, W5, W6, W8. Keep the work in
separate files: W1/W2 both touch `ingest.go`, so the second rebases on
the first; W4/W5 both touch `noise*.go`, so the same applies.

**Parked until R1 exits:** R2.1 sandbox (#78), R2.2 LoopsBench (#446),
R2.6 EGM (#448), and all of R3–R5. Exceptions: R3 may start early on
the D5 branch; LoopsBench adapter prep may start once W9 is running,
since it's needed for R2's exit.

## 6. Budget (trials, under the new protocol)

| Item | Trials | Note |
|---|---|---|
| W3 smoke | 20 | 10 × k=2 |
| W9 k-rep characterization | 480 | 30 × 8 × 2 (A/A, also checks the estimator) |
| W7 N1a | 480 | 30 × 8 × 2 |
| L6 C1 (TB, efficiency) | 480 | MDE −16% |
| L3, L4, L1, L2 screening | 1,920 | 4 × 480, MDE ≈ 9.5pp |
| R1.6 best-of-k | ≈ 270 | per 11-09 |
| R1 exit sealed read | 480 | 30 × 8 × 2 |
| **R1 total** | **≈ 4,130** | vs ≈ 1,800 in 11-09 §9; the increase is the k reps |

At `deepseek-v4.1-flash` catalog rates and the pilot's token mix,
that's ≈ $0.016–$0.12 per trial (route- and cache-dependent; #505), so
**≈ $66–$500 for R1**. The #509 C2 pressure read runs on the internal
corpus and is budgeted separately. Throughput depends on the route's
rate limits, which W3 measures.

## 7. Risks and kill rules (in addition to 11-09 §8)

| Risk | Signal | Response |
|---|---|---|
| deepseek's pass rate on TB2.1 is outside 0.30–0.80 | W3 / W9 | > 0.80: little headroom, so consider the harder half of dev. < 0.30: floor effects. Either way, record and re-decide D1 |
| Temperature ignored by the route | W3 provenance | Plan for stochastic cells (already the design); no threshold change |
| Day-level drift or quota bias between arms | W9 per-slice arm balance | Both arms in every slice; requeue quota deaths |
| Screening reads all inconclusive | W11 | Expected for small effects; don't relabel as null. D3 decides whether to buy 5pp reads |
| Segment-grain fold hurts pass | #509 C1 guardrail | Drop L6 for TB; #493 decides on the C2 read alone |
| Merges without CI regress eval code | W8 | No eval-code merges after W8 lands without green CI |

## 8. Issue dispositions (11-16)

| Issue | Disposition |
|---|---|
| #504, #505, #506, #507, #508, #509, #510, #511, #512 | **New**, filed from this audit |
| #503 | On hold (comment): blocked on #504 and D1; run discipline added |
| #489 | Audit comment; milestone R1; closes when §4 is signed off |
| #397 | Pointer comment; the verdict is recorded here under §4 |
| #493 | Framing corrected (comment); decision read is #509 C2 |
| #482 | Milestone R0; deadline per D5 |
| #440–#445 | Stay closed (code); decisions tracked in #512 |
| #388 | Stays closed (adapter); R1.1 fleet acceptance moves to #510 and W9 |
| #389 | Re-scoped by #508 |
| #395 | `c_min` input from #507 |
| #453 | Decided with #493 |

## 9. Protocol for an implementing agent

1. Read `AGENTS.md` and this file. Pick the lowest-numbered **Agent**
   workstream in §5 whose dependencies are met. Comment on its issue
   before starting to avoid collisions.
2. One issue, one branch, one PR against `andikapradanaarif/crush`
   (`gh pr create --repo andikapradanaarif/crush`). Use semantic commits
   and `gofumpt -w`.
3. Run `go test ./internal/eval -short` (≈ 6 min) plus the tests of any
   touched package.
4. **Never start Harbor runs or other paid trials.** Prepare manifests,
   runbooks, and exact commands; the owner executes them. Analysis after
   a run is agent work.
5. Results go to dated `docs/milestones/*-exp-*.md` records and the
   README index. Don't edit this file's findings. Status changes go on
   the issues, and the next dated record supersedes this one.
6. Don't change a threshold in §4 or in 11-09 without an owner-signed
   dated amendment.
