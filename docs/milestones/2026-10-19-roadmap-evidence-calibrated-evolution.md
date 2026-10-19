# 2026-10-19 — Roadmap: evidence-calibrated harness evolution

**Plan of record.** Supersedes
[2026-10-17-roadmap-regularized-evolution](2026-10-17-roadmap-regularized-evolution.md).
The direction is unchanged — an offline, gated loop that evolves the
harness and lands every kept change as a reviewed PR, with memory as
one evolvable component. What changes is *how it is measured and what
it may touch*: a read of the 2026 harness-evolution literature
(19 papers, 17 in full text, listed under [References](#references))
showed five places where the 10-17 plan would have spent its budget
measuring the wrong thing.

## The five corrections

| # | 10-17 plan | Correction | Evidence |
|---|---|---|---|
| 1 | OOD gate on SWE-bench Verified | **SWE-bench Pro V2 public** (642 tasks, 11 repos, Python/Go/JS/TS, GPL-sourced) | OpenAI (2026-02-23): 59.4% of 138 audited hard Verified tasks have flawed tests; every frontier model tested reproduced gold patches. OpenAI stopped reporting Verified |
| 2 | Primary endpoint = pass rate (MDE ≤ 5pp) | **Primary = tokens-to-done at pass-rate non-inferiority**; pass gain is secondary | Held-out pass gains on mature seeds are ~0–2pp (RRSI +1.8; Rethinking +0.6; AHE SWE-V +0.4). Token savings transfer (AHE −12% at equal pass). Efficiency cannot be explained by extra sampling |
| 3 | v1 surface = prompts + tool descriptions | **v1 = gated lessons file, tool descriptions + tool behavior, hook configs, `params.Memory`; system prompt last** | AHE component ablation: memory +5.6, tools +3.3, middleware +2.2, system prompt **−2.3**. Prompt-only evolvers (ACE, TF-GRPO) regressed below seed on SWE-V with +11–29% tokens |
| 4 | No baseline arm | **Matched-budget best-of-k arm** before any pass-rate claim | Rethinking (TB2.1): parallel sampling 72.3 vs harness evolution 67.4 (no tests); 86.0 vs 75.8 (with tests) |
| 5 | Personal loop as a single growing profile | **Context-conditioned, applicability-gated, size-capped, evaluator-filtered** | CIPHER: context-agnostic preferences did worse than no learning. Adaptive Auto-Harness: dense single harness peaks then declines (prompt 2→68 KB). Xiong: add-all memory degrades |

Two further adjustments follow from the deeper read:

- **Two separate adapters become one.** Terminal-Bench 2.x and
  SWE-bench Pro V2 both ship as Harbor task directories. One Harbor
  installed agent (`crush run` in the container) covers the evolve
  set and the OOD gate.
- **The solver is the production model.** The owner's daily driver
  is `deepseek-v4.1-flash` (3,765 assistant messages in nexus;
  `qwen3.8-max` as large). Mid-tier models gain most from harness
  evolution (Harness-Updating Table 1: Qwen3-235B +19.3 vs Opus 4.6
  +2.6 on SWE; Evo-Bench: DeepSeek-V4-Flash policy 29.7 → 41.5–43.5;
  AHE: deepseek-v4-flash +10.1 on TB2). Searching with the production
  model removes the weak→strong transfer question entirely.

## Calibrated expectations

What a successful program looks like, stated before any spend:

| Quantity | Expected range | Basis |
|---|---|---|
| OOD tokens-to-done reduction | 5–15% | AHE −12% at equal pass on SWE-V; ReasoningBank −6 to −9% steps |
| OOD pass-rate change, mid-tier solver, mature seed | 0 to +5pp | Mature seeds (RRSI, Meta-Harness) +1.7–1.8; weak seeds +8.8 (HarnessCompass) to +10.1 (AHE cross-model); Crush sits between |
| Evolve-set gain | +3 to +10pp | AHE +7.3, RRSI +6.0, Meta-Harness +1.7 (strong seed) |
| Accepted edits per 10 rounds | 2–5 | RRSI: 4 accepted of 32 evaluated over 20 rounds (workspace domain) |

A program that delivers the first row with pass non-inferiority is
a **success**; the second row is upside, not the claim.

## Evidence ledger — what each paper changes

Read in full unless marked. "→" is the design consequence.

| Paper | Key finding | → Design consequence |
|---|---|---|
| **RRSI** (2609.24972) | Regularizers (edit budget, critic, noise floor, cost rule, pruning) are what make gains transfer: ablating both groups drops OOD 43.6→40.3, tokens 2.42M→3.80M. Two independent searches agree within CI | Keep all five regularizers; replicate the search (P4) |
| **Rethinking** (2607.12227) | On TB2.1, harness evolution < parallel sampling at matched budget; disjoint 45/10/34 split: Opus +1.2, GPT-5.4 +0.0. TB may be harness-insensitive for strong models | Matched-budget arm (#405); efficiency endpoint; mid-tier solver |
| **AHE** (2604.25850) | TB2 69.7→77.0 (in-sample); SWE-V transfer +0.4 pass, −12% tokens; prompt-only component −2.3; proposer predicts fixes (33.7% precision) but not regressions (11.8%) | Surface v1 (correction 3); record predictions, never trust them |
| **HarnessCompass** (2608.01918) | 50/450 split, bash-only seed: held-out 51.6→60.4. Generalization gate alone: held-out 58.4 in 2 turns. Self-feedback without merge: held-out fell to 55.8. Ungated memory = answer keys; gated = principles with applicability clauses | Critic becomes a generalization gate; every lesson carries an applicability clause |
| **Harness-Updating ≠ Benefit** (2605.30621) | Evolver capability flat (≤3.1pp spread; 9B ≈ Opus) **when evolving skills**; benefit peaks at mid-tier solvers; weak models fail to load/follow artifacts | Mid-tier production solver; measure artifact load/adherence |
| **Better Harnesses, Smaller Models** (2607.08938) | Full-harness optimizer: cheaper meta-agent gave *worse* harnesses; raw JSON traces beat summaries; several independent searches beat one long one; gains correlate with low task diversity (ρ = −0.96) | Strong proposer for full-harness edits (cheap proposer only as ablation); raw traces; repetitive personal workflows are the P5 sweet spot |
| **Evo-Bench** (2608.09096) | DeepSeek-V4-Flash policy 29.7→41.5/43.5; early saturation then harmful edits; failure modes: bundled edits, no smoke checks, no best-snapshot restore, regressions misread as noise. Integrity: immutable harness snapshot to policy, answer fields stripped, semantic audit | Ledger gains preflight, best-revision restore, reset trigger; gate gains snapshot isolation + audit |
| **Meta-Harness** (2603.28052) | TB2 search and eval on the same 89 tasks (explicitly in-sample); Opus 76.4 vs Terminus-KIRA 74.7. Held-out evidence only for text classification and math | Proposer reads full prior traces via filesystem; never cite its TB2 number as transfer |
| **DGM** (2505.22954) | 20→50% in-sample; cross-benchmark 14.2→28.9; ~$22k/run; discovered "multiple attempts + select" (test-time scaling); objective hacking when the checker was visible | Hide verifiers from proposer; cost rule catches baked-in sampling |
| **SICA** (2504.15228) | 17→53% on a SWE-V subset; ~$7k for 15 iterations; much of the gain was speed/cost; path dependency on early ideas | Efficiency is a legitimate objective; independent runs to break path dependency |
| **GEPA** (2507.19457) | Reflective prompt evolution, train/val/test splits; single-module prompts, not coding harnesses | Supports reflection on traces; not evidence for harness-level coding gains |
| **SkillsBench v4** (2602.12670) | Curated skills +16.6 avg (+11.6 SWE); self-generated skills −8.1 to −11.5 vs none (unused, displaced work, confident errors) | Procedural memory only from verified traces; never authored from the task description |
| **ReasoningBank** (2509.25140) | SWE-V bash-only +3.4/+4.6; failures improve memory; 1 retrieved item best (49.7) vs 4 (44.4); judge accuracy 70–90% tolerated | Render ≤ 1–2 items; mine failures too |
| **Experience-following** (2505.16067) | Agents copy retrieved experience; add-all degrades; strict evaluator improves; a judge tuned on ~300 trajectories suffices | Every memory/lesson addition passes an evaluator |
| **CIPHER** (2404.15269) | Edit cost −31% / −73% with context-conditioned preferences; context-agnostic preferences worse than no learning; simulated users only | User tier = context-retrieved preferences with applicability |
| **Adaptive Auto-Harness** (2606.01770) | Single dense harness peaks then declines on streams; routing + human steering help; HITL helps only when it injects missing external signal | Size caps, pruning, routing over accumulation in P5 |
| **SWE-Exp** (2507.23361) | Experience bank must exclude same-repo, later-in-time trajectories to avoid leakage | Temporal + repo exclusion for any experience retrieval in eval |
| SWE-bench Pro (2509.16941; abstract + V2 README) | 642 public V2 tasks, Harbor format, public images, HARD-51 subset; GPL repos resist contamination | OOD gate (correction 1) |
| Terminal-Bench 2 (2601.11868; abstract) | 89 verified tasks, Harbor; TB2.1 repairs 28 | Evolve set |

## Architecture — three loops (unchanged shape, revised contents)

### Loop 1 — per-turn memory

Failure memory stays on for the owner. Other channels are components
Loop 2 may enable, tune via `params.Memory`, or prune. LTT,
`project_rates`, and bandit code stay in-tree, unused.

### Loop 2 — offline regularized evolution (`crush evolve`)

**Mutable surface v1 (enforced allow-list):**

1. A gated **lessons file** loaded as context (the AHE "long-term
   memory" analogue): every entry has a rule, an applicability
   clause, and provenance; no task IDs, paths, or symbols from the
   evolve set.
2. **Tool descriptions and tool behavior parameters** that are config,
   not code (timeouts, output caps, defaults).
3. **Hook configurations** (`PreToolUse`, `PreCompact` via crushrc —
   the middleware analogue); the hook *engine* stays immutable.
4. **`params.Memory`** values and memory-channel flags.
5. **Skill bodies** (verified-trace provenance only).
6. System prompt templates — allowed, deprioritized; any prompt edit
   is its own single-edit candidate (never bundled).

**v2 (after #78 phase D+A and 2–3 clean rounds):** run-edge Go code.

**Immutable:** safety ordering, veto precedence, project partition,
verdict vocabulary, `internal/permission`, `internal/hooks` (engine),
`internal/eval`, verifiers, measurement contracts.

**Round:** analyze (raw JSON traces) → propose (one falsifiable
hypothesis per edit, with predicted fixes and predicted regressions)
→ **preflight smoke** (5 fixed tasks; reject crashes before spending
a full evaluation) → **generalization gate** → score (evolve set,
k = 2, same invocation as incumbent) → select (rules below) → prune
→ restore best revision on regression → land as PR.

**Selection rules** (S = pass rate, C = tokens-to-done):

- Floor: `Ŝ(H′) ≥ S* − δ`.
- `ΔS > δ`: accept if `ΔC ≤ β₀ + β₁·ΔS`.
- `|ΔS| ≤ δ`: accept only if `ΔC ≤ −c_min` (a real efficiency gain,
  `c_min` = the measured noise in C, not zero).
- β₀, β₁, δ, `c_min` fixed in P1 from base runs; never retuned.

**Integrity (Evo-Bench C.2, DGM):** the policy receives an immutable
harness snapshot and a task view with verifier fields stripped; the
proposer can read traces and scores but never verifier or test
content; holdouts return no evidence to the search; a semantic audit
reads every accepted candidate's diff and trajectories for leakage or
detector evasion.

**Splits:** evolve = Terminal-Bench 2.1 (89). OOD gate = SWE-bench
Pro V2 public, N sized in P1, read once per pre-registered decision.
Guard = Crush-specific memory-channel pool (#224), regression only.

**Baseline arm:** best-of-k with the base harness at equal total
tokens. Any *pass-rate* claim must beat it; the *efficiency* claim is
immune to it by construction.

### Loop 3 — personal (capture done; build in P5)

Captured `hunk_added`/`hunk_after` (#400) feed: (a) context-retrieved
preferences with applicability clauses (#229); (b) real
failures → evolve-task candidates (#399); (c) procedural memory from
verified traces (#298). All additions pass an evaluator; all stores
are size-capped and pruned.

## Engine fit (code audit, 10-19)

The plan was checked against the code. Every evolution candidate must
be expressible as an **arm delta the existing eval engine can run on
one build** — `ArmConfig.Options` materialized into `.crush.json`
(`internal/eval/materialize.go`), both arms on the same binary
(`driver.go:126`, `RunRecord.Invocation`).

| Plan element | Engine reality | Fit |
|---|---|---|
| Lessons file | `options.context_paths` → rendered in `<project_context>` (stable cache class) by `prompt.promptData` | **Native** — options delta, no code |
| Skill bodies | `options.skills_paths` → `skills.Discover`, user overrides builtin | **Native** |
| `params.Memory`, channel flags | `options.memory_params` overlay + `optionSpecs` bools | **Native** |
| Primary endpoint | `DecisionRule.Primary.Metric = "tokens_to_done"`; `Guardrail{Metric: "pass_rate", MinDelta}` = non-inferiority | **Native** |
| Middleware | Run-boundary edges already exist (`runEdgeSet`: verification, todos, stall, verify-loop, burn-watch, reconcile) — the AHE finish-hook layer. Thresholds are Go constants (`maxRepairAttempts=2`, `burnWatchStepsThreshold=30`, `verifyLoopMinSteps=8`, `loopDetectionMaxRepeats=5`, `scopeGateMinExploration=8`, …) | **Gap → #408**: move thresholds into a bounded, versioned `params` block so edges become an options-delta surface without opening Go code |
| Hooks | `PreToolUse` (deny/allow/`updated_input`/context), `PreCompact`; config is top-level `hooks`, not `options` | **Gap → #409**: arms can only carry `options` |
| Tool behavior | Top-level `tools.{ls,grep,glob}` limits/timeouts exist; bash `MaxOutputLength=30000` and `toolResultMaxContentBytes=50_000` are constants | **Gap → #409** (tools block); bash cap optional |
| Prompts, tool descriptions | `//go:embed` + `template.Must` at package init (`prompts.go`, `tools/*.go`) — compile-time | **Gap → #406**: a runtime override dir set by an option keeps one binary per invocation and keeps upstream-owned embedded files untouched until a change lands |
| OOD statistics | `compare.go`: BCa over pairs **resampled within trajectory**; `requiredPairs` skips trajectories with < 2 pairs. SWE-Pro OOD runs k = 1 per task → within-stratum variance is degenerate | **Gap → #407**: task-level (cluster) resampling mode, required before any OOD read |
| Best-of-k arm | Comparator pairs only `treatment` vs `control` | **Gap → #405**: run as its own treatment-vs-control comparison or add multi-arm compare |
| Harbor trials | Runner only runs local git-fixture trajectories + `check.sh`; containers can't go through it | **Plan**: Harbor drives, an ingester writes `RunRecord`s (Experiment, TrajectoryID = task id, Arm, Invocation, RunIndex, Outcome, Tokens, Steps, SessionDB, BaselineKey) so `LoadExperimentRecords` / compare / gate run unchanged (#388) |
| Headless run | `RunNonInteractive` auto-approves the session (`app.go:494`); `--data-dir` persistent; `CGO_ENABLED=0` → static `GOOS=linux` build | **Native** |

**Consequence for sequencing:** P2 manual rounds can start on the
native surfaces (lessons file, skills, `memory_params`, channel flags)
while the four gaps land; prompt/tool-description candidates wait for
the override layer, edge-threshold candidates for the params move,
hook/tool-block candidates for the arm-config extension, and **no OOD
read happens before the cluster bootstrap**.

## Phases, targets, exits

Every phase has numbered targets. A phase exits when all its targets
are met or its kill criterion fires. Targets are pre-registered —
changing one after seeing data requires a dated amendment.

### P0 — Dogfood + label closeout

*Milestone 9. Gate for: P5 inputs, label trust.*

| ID | Target | Issue |
|---|---|---|
| T0.1 | ≥ 1 nexus session on a binary ≥ `6494cc0`; `turn_outcomes.hunk_added` present; ≥ 20 turns with non-empty captured hunks | #293 |
| T0.2 | ≥ 250 settled `turn_outcomes` on nexus; verdict mix reported (baseline 10-19: 196 settled — failed 89 / accepted 61 / unknown 42 / revised 4) | #293 |
| T0.3 | 50 settled turns hand-labeled; agreement with `turn_outcomes` reported per verdict; **≥ 80% precision on `accepted`** before #298 may mine it | #402 |
| T0.4 | Weekly personal trend report runs from `crush.db` (edit cost, revision rate, tokens/turn, repeat-failure rate); first 2 weekly baselines recorded | #403 |
| T0.5 | Label-pipeline freeze holds: no label PRs except fixes for defects seen in real rows | — |

**Kill:** T0.3 accepted-precision < 60% → `turn_outcomes` is not
trusted for mining; #298 stays blocked; Loop 3 relies on captured
hunks only.

### P1 — Feasibility → Gate G1

*Milestone 10. Decides commit vs fallback.*

| ID | Target | Issue |
|---|---|---|
| T1.1 | Harbor installed agent runs `crush run` headless: ≥ 85/89 TB2.1 tasks and ≥ 95% of a 100-task SWE-bench Pro V2 sample complete without infra error; per-trial session DB returned and analyzed | #388 |
| T1.2 | Base characterization on `deepseek-v4.1-flash`: TB2.1 × 3 passes (k = 2): pass rate, δ, tokens-to-done CV, `c_min`; SWE-Pro sample × 2: σ_d, tokens CV | #404 |
| T1.3 | TB2.1 base pass in **0.30–0.80**; **δ ≤ 3pp** | #404 |
| T1.4 | Sizing: N_OOD for (a) 10% tokens-to-done reduction at 80% power and (b) pass non-inferiority margin −3pp; **both ≤ 542** (642 minus the 100-task characterization sample) | #389 |
| T1.5 | Matched-budget best-of-k arm runs once on TB2.1 (k = 3) | #405 |
| T1.6 | Contamination probe: issue-text-only file localization on 30 SWE-Pro vs 30 TB tasks for the production solver; recorded, not gating | #226 |
| T1.7 | Projected spend for P2–P4 ≤ owner budget written in #397 | #397 |
| T1.8 | Task-level (cluster) bootstrap mode implemented and required by the gate for OOD decision rules | #407 |

**Gate G1 (#397):** commit iff T1.1, T1.3, T1.4, T1.7, T1.8 hold.
**Fallback:** ledger + gate as discipline for hand-made mechanism
changes; no self-evolving or transfer claims; Loop 3 proceeds.

### P2 — Ledger, gate, manual rounds

*Milestone 11.*

| ID | Target | Issue |
|---|---|---|
| T2.1 | `crush evolve` ledger records per candidate: component, hypothesis, diff, predicted fixes/regressions, preflight result, gate verdict, S, C, decision | #390 |
| T2.2 | Generalization gate rejects 100% of 3 planted leaks (task ID, path, verifier string) **before scoring**; semantic audit flags a planted detector-evasion | #391 |
| T2.3 | Selector validation on measured candidates: harmful edit fails floor; no-op not accepted; a measured efficiency edit accepted within band — **3/3 correct** | #390 |
| T2.4 | Allow-list enforced in code: an out-of-surface edit is rejected without evaluation | #392 |
| T2.5 | 2–3 owner-written rounds through the ledger with **zero manual overrides** of gate/selector decisions | #390 |
| T2.6 | Cost rule constants (β₀, β₁, δ, `c_min`) committed from P1 data | #395 |
| T2.7 | #78 phase D (workspace confinement) merged before any unattended proposer run | #78 |
| T2.8 | Engine enablers merged: prompt/description override layer (#406), edge thresholds → params (#408), arm config carries hooks + tools (#409); each surface opens to candidates only after its enabler lands | #406 #408 #409 |

**Kill:** T2.3 fails twice after fixes → the selector cannot separate
known effects; stop before automation.

### P3 — Automated pilot

*Milestone 12.*

| ID | Target | Issue |
|---|---|---|
| T3.1 | Proposer (strongest available model; raw JSON traces; filesystem access to prior candidates) completes T = 10, m = 2, budget 3 → 1 with no infra abort | #393 |
| T3.2 | Evolve-set gain on S or C beyond δ / `c_min` (the search found something) | #393 |
| T3.3 | **Primary (pre-registered):** OOD tokens-to-done 95% CI upper bound < 0 **and** OOD pass CI lower bound > −3pp | #226 |
| T3.4 | Secondary: OOD pass CI lower bound > 0 **and** pass ≥ best-of-k at equal tokens | #226 |
| T3.5 | Proposer prediction calibration reported (fix/regression precision and recall) | #390 |

**Outcomes:** T3.3 met → P4. T3.3 inconclusive (CI spans 0, no
regression) → P4 replicate before any conclusion. OOD regression
(pass CI upper < −3pp or tokens CI lower > 0) → stop automated
search; fallback branch.

### P4 — Replicate, transfer, land

*Milestone 13.*

| ID | Target | Issue |
|---|---|---|
| T4.1 | Independent second search (fresh proposer context) reaches an OOD tokens effect within the first run's CI | #396 |
| T4.2 | Accepted edits scored with `qwen3.8-max` as solver on the OOD sample: pass non-inferior (−3pp), tokens not worse | #396 |
| T4.3 | Guard pool (#224): no regression beyond δ on memory-channel cells | #224 |
| T4.4 | Accepted edits merged as PRs with ledger links; defaults flipped through the release policy | #396 |

**Kill:** two runs both inconclusive at the MDE → publish "no effect ≥
MDE at this budget"; fallback branch.

### P5 — Personal loop

*Milestone 14. Starts after P0; build gated on P4 or the fallback decision.*

| ID | Target | Issue |
|---|---|---|
| T5.1 | Preference tier: context-retrieved, applicability-gated, evaluator-filtered; store capped (≤ 40 entries/project, ≤ 4 KB rendered) | #229 |
| T5.2 | Edit cost per accepted turn (captured hunks) **−20%** over 4 weeks vs the T0.4 baseline window, with revision rate not rising | #229 |
| T5.3 | ≥ 5 real-failure-derived tasks admitted to the evolve pool through the gate | #399 |
| T5.4 | Procedural memory: mined-skill arm ≥ no-skill arm on tokens-to-done for replayed nexus tasks, render ≤ 2; curated vs mined vs none reported | #298 |
| T5.5 | Harness growth bounded: lessons + preferences rendered ≤ 8 KB total; pruning report each cycle | #229 |

Single-user trends are reported as trends, not certified claims.

## Budget (trials)

| Phase | Trials | Note |
|---|---|---|
| P1 | ≈ 1,000 | TB 3 × 178, SWE-Pro 2 × 100, best-of-k 267 |
| P2 | ≈ 1,100 | validation 3 × 178 + 3 rounds × 178 + preflight |
| P3 | ≈ 4,200–4,900 | 10 × 2 × 178 + OOD N × 2 (N 300–642) |
| P4 | ≈ 4,500–5,200 | second search + cross-model OOD + guard |
| **Total** | **≈ 10,800–12,200** | per-trial cost measured in T1.2 |

Reference points: DGM ~$22k/run; SICA ~$7k/15 iterations; Evo-Bench
evolver cost <$1 (DeepSeek-V4-Pro) to <$40 (Qwen3.7-Max) per run,
excluding policy rollouts.

## Issue dispositions

| Issue | Disposition |
|---|---|
| #293 | P0 — targets T0.1, T0.2 |
| #388 | P1 — rescoped: one Harbor installed agent for TB2.1 + SWE-Pro V2 |
| #226 | P1 — rescoped: SWE-bench Pro V2 OOD gate (was Verified) |
| #389 | P1 — sizing for both endpoints |
| #397 | P1 — Gate G1 revised thresholds |
| #390, #391, #392, #395, #78 | P2 — rewritten per corrections 3/4 and Evo-Bench/DGM integrity |
| #393 | P3 — strong proposer, raw traces, cheap-proposer ablation |
| #396, #224 | P4 — #224 rescoped to a Crush-specific memory-channel regression guard |
| #229, #298, #399 | P5 — constrained designs |
| #360 | Open, low — affects only shared `data_directory` setups |
| New | #402 label validation set (P0), #403 personal trend report (P0), #404 base characterization (P1), #405 matched-budget baseline arm (P1); engine fit: #407 cluster bootstrap (P1), #406 override layer, #408 edge params, #409 arm config (P2) |
| Closed (not planned) | #297, #299, #342, #223, #216, #166, #140, #139, #110, #107, #3, #398 — reasons on each issue |

## Standing rules

Carried forward from 10-17 (all), plus:

- **Efficiency is a first-class claim.** A change that holds pass rate
  and cuts tokens-to-done beyond `c_min` is a win.
- **No pass-rate claim without the matched-budget arm.**
- **Abstracts are not evidence.** A number enters this directory only
  after its table and split were read.
- **One hypothesis per candidate.** Bundled edits only in the first
  rounds, and never a prompt edit bundled with anything.
- **Verifiers are invisible to the proposer.**

## References

Read in full:

- Xia et al. RRSI. arXiv:2609.24972 (v3).
- Wang et al. Rethinking the Evaluation of Harness Evolution for Agents. arXiv:2607.12227 (v2).
- Lin et al. Agentic Harness Engineering. arXiv:2604.25850 (v4).
- Zhang et al. HarnessCompass. arXiv:2608.01918.
- Lin et al. Harness Updating Is Not Harness Benefit. arXiv:2605.30621.
- Yang et al. Better Harnesses, Smaller Models. arXiv:2607.08938.
- Huang et al. Evo-Bench. arXiv:2608.09096 (v2).
- Lee et al. Meta-Harness. arXiv:2603.28052.
- Zhang et al. Darwin Gödel Machine. ICLR 2026, arXiv:2505.22954 (v3).
- Robeyns et al. A Self-Improving Coding Agent. arXiv:2504.15228.
- Agrawal et al. GEPA. ICLR 2026, arXiv:2507.19457.
- Li et al. SkillsBench. arXiv:2602.12670 (v4).
- Ouyang et al. ReasoningBank. ICLR 2026, arXiv:2509.25140 (v2).
- Xiong et al. How Memory Management Impacts LLM Agents. arXiv:2505.16067.
- Gao et al. CIPHER / PRELUDE. NeurIPS 2024, arXiv:2404.15269.
- Liu et al. Adaptive Auto-Harness. arXiv:2606.01770.
- Chen et al. SWE-Exp. arXiv:2507.23361.
- OpenAI. Why SWE-bench Verified no longer measures frontier coding capabilities. 2026-02-23.

Abstract / README level:

- Deng et al. SWE-Bench Pro. arXiv:2509.16941; ScaleAI/SWE-bench_Pro-os V2.
- Merrill et al. Terminal-Bench. ICLR 2026, arXiv:2601.11868.
- Harbor framework docs (custom installed agents).
