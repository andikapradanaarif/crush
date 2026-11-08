# 2026-11-09 — Roadmap: a long-horizon harness that measurably improves with use

**Plan of record.** Supersedes
[2026-10-19-roadmap-evidence-calibrated-evolution](2026-10-19-roadmap-evidence-calibrated-evolution.md),
including both of its amendments.

The 10-19 machinery stays: the RRSI-style ledger, gate, selector, cost
rule, replication and integrity controls. What changes is **order and
target**:

- Learning-signal integrity becomes the first exit.
- Cheap run-control and context levers with strong external evidence
  ship before any evolutionary search.
- The endpoint and the out-of-distribution (OOD) gate are fixed.
- Learning from use is defined as scoped, checked, replay-gated
  behavior, not a growing memory.

Inputs:

- [11-08 report, including §13 addendum](2026-11-08-report-harness-direction.md)
  — competitive position, 30+ papers by evidence level, risks.
- [11-08 P0 data audit](2026-11-08-exp-p0-data-audit.md) — real-user
  volume after excluding repair prompts.
- #429 gold labels — accepted precision 95%; non-accepted verdicts
  unreliable.
- #432 run-edge fixes; #433 worktree-aware capture.

---

## 1. Direction in one paragraph

Crush does not try to out-feature frontier products. It aims to be
**the best harness for a fixed mid-tier model on long-horizon coding
work** — highest pass rate and lowest tokens-to-done against other
harnesses on the same model — **with performance that rises, on
held-out tasks, as the owner uses it.**

It gets there in two steps:

1. **Deterministic structure at the run boundary**, with a model called
   only when that structure fires, and **reversible context**. This
   improves every session now.
2. **A learning loop that turns the owner's own work into scoped,
   checked, replay-verified behavior.** This improves future sessions,
   and every addition is measured where it actually reached the agent.

This is VISION's "deterministic at the boundaries, autonomous inside
them," applied to both execution and learning.

## 2. Why a new roadmap (what changed since 10-19)

| # | 10-19 position | New position | Reason |
|---|---|---|---|
| 1 | P0 exits on raw volume (≥ 250 settled, ≥ 80 accepted) | **R0 exits on signal integrity**: capture health and measured precision for every verdict class used. Volume is tracked, not gating | 142 of 344 rows were harness prompts (audit). 100% `out_of_workspace` capture on worktree projects (fixed by #433, unverified). Every sampled `revised` / `failed` turn was owner-accepted (#429) |
| 2 | No product phase; all open issues are eval / evolve infrastructure | **R1 ships cheap levers first**: advisor on edge fire, evidence gates, fresh-context (Goldfish) verifier, verify framing, escalation report, reversible folding | Strongest external evidence per unit of effort: LivePlan +9.9pp avg; ECLoop +4.8–11.8pp; ReFold −18–60% tokens. Crush already has the deterministic judge (run edges) and the stub machinery |
| 3 | Primary endpoint = tokens-to-done reduction at pass non-inferiority, with a selector that admits token growth | **Endpoint resolved** (§6): pass rate is the selection score with RRSI's cost rule. Efficiency-only edits get their own within-band path. Claims are reported as one of two pre-declared types | RRSI's own evolved harness cost +55% tokens vs base. The 10-19 selector could accept the very edits that fail its success criterion |
| 4 | OOD gate = SWE-Bench Pro V2 public | **Held-out = sealed TB2.1 split + long-horizon LoopsBench subset + in-domain replay.** SWE-Pro V2 only after a gating contamination probe, and never primary | Public V2: Opus 5 at 99.4% vs 81.6% private; Scale attributes the gap to training-time exposure |
| 5 | Learning unit = lessons file + memory params, then an automated proposer | **Learning unit = path-scoped rules with checks** and RSR-verified procedures, kept by exposure-aware replay. The automated proposer is gated on a positive manual slope | Cortex (+55pp held-out; 16 of 18 kept items were path rules; an always-on skill was opened 0 of 198 times), TellOnce, Terminal-RSI's RSR recipe, HarnessDev (unregularized evolution fails) |
| 6 | Sandbox only as a prerequisite for unattended proposer runs | **Sandbox is part of the long-horizon core (R2)** and must understand linked worktrees | Unattended long runs need containment. Codex and Claude Code use sandboxes to reduce approval prompts (VISION: attention is the scarce resource) |

## 3. Current position (2026-11-09)

| Area | State |
|---|---|
| Capture | #433 merged: worktree-aware capture and settlement. **0 turns recorded since**, so H0 is unverified. aurum and borneo have no `crush.db` |
| Labels | Real-user settled 202 / accepted 43 (pre-fix rows). Accepted precision 19/20 = 95%. Settled `revised` 10/10, `failed` 10/10 and `unknown` 9/10 were gold-accepted |
| Run edges | #432: owner `stop` suppresses edge retries; premature summaries restated. Edge layer makes no LLM calls |
| Context | Notebook and superseded-result stubs exist, **off by default**. No folding of the agent's own turns |
| Eval | Power gate, alarms, LTT, blind label tooling. No public benchmark number yet (Harbor agent #388 not built) |
| Learning data | 1 `referent_episodes` row, 1 `project_rates` row fleet-wide. Failure memory has a replicated effect (−27% calls) |
| Sandbox / parallelism / plan artifact | Parked / synchronous only / parked |

## 4. Targets — north star and scoreboard

Pre-registered by this roadmap. Changing a threshold after seeing data
requires a dated amendment.

| ID | Metric | Protocol | Target |
|---|---|---|---|
| **H0** | Capture health | Per active project: share of real-user edited paths with `ok` hunk cells | **≥ 80%** on every active project, including worktree workflows |
| **L1** | Verdict integrity | Blind gold labels per settled class on post-#432/#433 rows | Each verdict class used by a metric or learner has **precision ≥ 70%** (accepted already 95%) |
| **N1a** | Anchor | TB2.1 dev split, `deepseek-v4.1-flash`, k = 2, Crush vs Terminus 2, task-level paired bootstrap | Crush ≥ Terminus 2 − δ |
| **N1b** | Lever gain | Sealed TB2.1 split, same protocol, after R1 + R2 levers | **+≥ 5pp pass at tokens ≤ +25%**, OR **tokens-to-done −≥ 20% at pass ≥ −δ** |
| **N2** | Long horizon | LoopsBench subset (20–30 tasks) | Baseline recorded; then resolve rate non-inferior and **regression events ≤ baseline** |
| **N3** | Learning slope | In-domain replay set, temporal split, ≥ 3 learning rounds, **mean not best** | Slope CI lower bound > 0 on pass or tokens, the other non-inferior |
| **N4** | Personal trend | `crush stats trend` v2, weekly, after H0 and L1 | Edit cost per accepted turn −20% over 4 weeks vs the first clean baseline; revision rate not rising |
| **G1** | Harness growth | Rendered rules + lessons + preferences | ≤ 8 KB rendered; **0 bytes of new always-on text** |
| **G2** | Memory safety | Each memory channel vs memory-off, per model tier in use | No channel net-negative on any tier in use |

**Calibrated expectations** (stated before spend):

| Quantity | Expected | Basis |
|---|---|---|
| Advisor on edge fire, pass | +2 to +8pp | LivePlan +9.9 avg over vanilla SWE-agent; discounted because Crush's edges already intervene |
| Reversible folding, tokens | −15% to −40% at equal pass | ReFold −18 to −60% |
| Goldfish verifier, pass | 0 to +5pp; false-done rate down | Self-verification catches 61%; withholding self-report +13–43pp detection |
| Regularized evolution, sealed held-out | 0 to +5pp | RRSI +1.8 (OOD); Self-HPO harness-only +2.4 |
| Learning slope on own work | Small but positive if corrections recur | Cortex: large with a weaker model, ~0 with a stronger model or no recurring corrections |

## 5. Splits and measurement

| Split | Composition | Use | Read rule |
|---|---|---|---|
| TB2.1 **dev** | 59 tasks (seeded, frozen list) | Lever development, evolution scoring, characterization | Unlimited |
| TB2.1 **sealed** | 30 tasks (seeded, frozen list + hash) | N1b; R1, R2 and R4 exit reads | **Only at phase exits**; returns no evidence to search |
| **LoopsBench subset** | 20–30 tasks, mixed languages | N2 long-horizon; regression obligations | Phase exits |
| **In-domain replay** | Owner tasks from nexus, vishnu, kiosk, aurum, borneo; snapshot replay (#399); learn before date D, test after D | N3 learning slope; R3 gate | Test side read only at round ends |
| SWE-Pro V2 | Public split; Hard-51 secondary | Optional guard | Only if the contamination probe passes; **never primary** |
| Crush guard | #224 memory-channel cells | Regression guard | R5 |

Statistics:

- Task-level (cluster) bootstrap (#407) for every split.
- Matched-budget best-of-k arm (#405) before any pass-rate claim.
- δ and `c_min` measured in R1.2, never retuned.

## 6. Endpoint and selection (resolved)

- **Selection score:** pass rate `S` on the dev split; cost `C` =
  tokens-to-done.
- **Floor:** `Ŝ(H′) ≥ S* − δ`.
- **Gain path:** if `ΔS > δ`, accept when `ΔC ≤ β₀ + β₁·ΔS` (RRSI
  form). The constants are fixed in R1.2 from measured noise.
- **Efficiency path:** if `|ΔS| ≤ δ`, accept only when `ΔC ≤ −c_min`.
- **Claims are pre-declared as one of two types**, and each is reported
  with its own test on the sealed split:
  - **Capability claim:** pass gain beyond δ *and* ≥ the matched
    best-of-k arm.
  - **Efficiency claim:** tokens-to-done reduction beyond `c_min` *and*
    pass non-inferior (−δ).
- No phase requires tokens to fall *and* pass to rise at the same time.

## 7. Architecture

| Layer | Role | Crush seam |
|---|---|---|
| Judge (deterministic) | Detect drift: stall, loops, burn, missing evidence, unverified completion, open failures | Run edges, `scope_gate`, verify gate |
| Advisor (LLM, on fire only) | One high-level next-step correction or an escalation; configurable model | **New**, called from edge fires |
| Goldfish verifier | Fresh context; sees task + diff + test output, **never the worker's transcript or completion claim** | **New**, pre-completion check |
| Context renderer | Reversible stubs and folds; cache-aware chunking; observed/inferred tagging | Notebook, superseded-result stubs |
| Plan anchor | Goal + phases the judge checks; owner approval points | EGM experiment → `PLAN_ARTIFACT.md` decision |
| Capture | Edit content + survival across main checkout and linked worktrees | `cmdlog` (#433) |
| Learning store | Path-scoped rules with checks; RSR-verified procedures; failure memory; phased digests | `context_paths`, `skills_paths`, `failure_memory`, `session_digests` |
| Learning gate | Exposure-aware replay A/B; RRSI regularizers; pruning | #390–#392, #399 |
| Containment | Workspace confinement + OS sandbox, worktree-aware | #78 |

## 8. Phases

```
R0 signal integrity ──────────────┐ (gates R3)
R1 anchor + cheap levers ──► R2 long-horizon core
        │ (Gate G1′ gates R4)          │
        ▼                              ▼
R4 regularized evolution ◄──── R3 learning from use
        └────────────► R5 replicate + land ◄──┘
```

R0 and R1 run in parallel. R1 needs no dogfood volume.

### R0 — Signal integrity

*Milestone: R0 (re-scope of P0, #9).*

| ID | Target | Issue |
|---|---|---|
| R0.1 | **H0 ≥ 80% `ok`** on every active project, read by a fleet capture-health command (real-user settled / A / R / F / U / `ok` share per project) | new |
| R0.2 | Edge-authored messages (stall-stop, todo-edge, reconcile, verification retry) never mint `turn_outcomes`; audited on real rows | new (from #430 item 4) |
| R0.3 | **Second blind label pass:** ≥ 30 post-#432/#433 rows (10 revised, 10 failed, 10 unknown); report precision per class (**L1**) | new |
| R0.4 | Volume tracked on clean rows: ≥ 150 real-user settled and ≥ 40 accepted post-fix, across projects (tracking; gates R3, not R1) | #293 |
| R0.5 | Two consecutive weekly v2 trend rows with measured edit cost on ≥ 2 projects | #403 |
| R0.6 | Correct the 11-07 #293 status comment; register aurum and borneo | #293 |
| R0.7 | Label-pipeline freeze holds (bugs from real rows only) | — |

- **Exit:** R0.1, R0.2 and R0.3 met (L1 decided per class).
- **Kill:** a non-accepted class still below 50% precision after two
  fix rounds is **retired**. No metric or learner reads it; learning
  uses `accepted` + captured hunks only.

### R1 — Anchor + cheap levers

*Milestone: R1 (re-scope of P1, #10).*

| ID | Target | Issue |
|---|---|---|
| R1.1 | Harbor installed agent runs `crush run` headless; ≥ 85/89 TB2.1 complete without infra error; per-trial session DB returned | #388 |
| R1.2 | Base characterization on `deepseek-v4.1-flash`: TB2.1 dev × 3 (k = 2) — pass, δ, tokens CV, `c_min`; cost rule constants fixed | #404, #395 |
| R1.3 | TB2.1 dev/sealed split frozen (seed + hash); cluster bootstrap required | new, #407 |
| R1.4 | **N1a anchor:** Crush vs Terminus 2, same model | #388 |
| R1.5 | **Lever arms**, each one hypothesis, paired on dev, decided by §6: L1 advisor on edge fire · L2 evidence-conditioned gates · L3 Goldfish verifier · L4 "find bugs" verify framing · L5 escalation handoff report · L6 reversible folding of finished agent turns + cache-aware chunking (and notebook/stub defaults) | new × 6 |
| R1.6 | Matched-budget best-of-k arm runs once | #405 |
| R1.7 | Contamination probe on the production solver (TB vs SWE-Pro, issue-text-only localization); decides whether SWE-Pro may be used at all | #226 (re-scoped) |
| R1.8 | Projected spend for R1–R5 within owner budget | #397 (re-scoped) |

- **Gate G1′ (#397):** R1.1, R1.2 (base pass 0.30–0.80, δ ≤ 3pp), R1.3
  and R1.8 hold → evolution is measurable and R4 may start. Otherwise
  fallback: levers ship on paired dev reads + replay; no transfer
  claims.
- **Exit:** N1a measured; each lever accepted or dropped; accepted
  levers on by default.
- **Kill:** Crush < Terminus 2 − δ → fix the basics before more levers.

### R2 — Long-horizon core

*Milestone: R2 (new).*

| ID | Target | Issue |
|---|---|---|
| R2.1 | Sandbox phase D (workspace confinement, linked worktrees allowed) + phase A (OS sandbox on exec) | #78 |
| R2.2 | LoopsBench subset adapter; **N2 baseline** | new |
| R2.3 | Run-edge thresholds → bounded, versioned params | #408 |
| R2.4 | Arm config carries `hooks` + `tools` blocks | #409 |
| R2.5 | Notebook and compaction entries tagged observed vs inferred; unsupported entries dropped | new |
| R2.6 | **EGM dogfood experiment** (skills via `skills_paths`, Goldfish review through the `agent` tool); decide whether to unpark `PLAN_ARTIFACT.md` | new |
| R2.7 | Engine-fit table refreshed (13 hook events) | new (docs) |

- **Exit:** **N1b** on the sealed split; N2 baseline + one improvement
  read.
- **Kill:** no N1b after two lever rounds → claim narrows to efficiency
  only.

### R3 — Learning from use

*Milestone: R3 (re-scope of P5, #14). Starts after R0 exit.*

| ID | Target | Issue |
|---|---|---|
| R3.1 | Replay-task harvest from owner sessions (worktree-aware), temporal split | #399 |
| R3.2 | **Corrections → path-scoped rules with checks**; exposure-aware keep/prune, one item at a time; **G1** holds | new |
| R3.3 | **Procedural memory via the RSR recipe:** accepted-only source → runbook without harness-control messages → leakage review on public task + runbook → fresh replay must pass → keep | #298 (unpark) |
| R3.4 | Memory reform: capability-conditioned channel enablement (**G2**); phased-document digests; struggle-gated recall; `notebook_sync_mem0` audited against memory-off | new × 3 |
| R3.5 | Preferences from captured hunks (needs `revised` to pass L1) | #229 |
| R3.6 | **N3 learning-slope protocol** (temporal held-out, ≥ 3 rounds, mean not best) | new |

- **Exit:** N3 slope CI lower > 0; N4 trend reported.
- **Kill:** slope ≤ 0 after 3 rounds → prune the store and freeze
  additions.

### R4 — Regularized evolution

*Milestone: R4 (re-scope of P2, #11; absorbs P3, #12).*

| ID | Target | Issue |
|---|---|---|
| R4.1 | Ledger + round runner (component, hypothesis, diff, predicted fixes/regressions, preflight, gate verdict, S, C, decision) | #390 |
| R4.2 | Generalization gate rejects 3/3 planted leaks before scoring; semantic audit flags a planted detector evasion | #391 |
| R4.3 | Allow-list v1 enforced: rules, lessons, skills, `memory_params`, edge params (after #408), hooks/tools (after #409); prompt overrides last | #392, #406 |
| R4.4 | Selector validation 3/3 on measured candidates; cost constants from R1.2 | #390, #395 |
| R4.5 | 2–3 owner-written rounds, zero manual overrides | #390 |
| R4.6 | Automated proposer (T = 10, m = 2, budget 3 → 1) **only if** R4.5 accepted ≥ 1 edit or R3 slope > 0 | #393 |

- **Exit:** a sealed-split read on accepted edits.
- **Kill:** selector fails 3/3 twice → stop before automation.

### R5 — Replicate + land

*Milestone: R5 (re-scope of P4, #13).*

| ID | Target | Issue |
|---|---|---|
| R5.1 | Independent second search lands within the first run's CI | #396 |
| R5.2 | Cross-model read (`qwen3.8-max` solver): pass non-inferior, tokens not worse | #396 |
| R5.3 | Guard pool shows no regression beyond δ | #224 |
| R5.4 | Accepted edits merged as PRs with ledger links; defaults flipped through release policy | #396 |

- **Kill:** two inconclusive runs → publish "no effect ≥ MDE at this
  budget".

## 9. Budget (trials, estimate)

| Phase | Trials | Note |
|---|---|---|
| R1 | ≈ 1,800 | Characterization 354, Terminus 2 anchor 178, six levers ≈ 700, best-of-k ≈ 270, sealed read ≈ 120, slack |
| R2 | ≈ 900 | Lever round 2, LoopsBench baseline + read (long tasks), sealed read |
| R3 | ≈ 600–1,000 | Replay-based, owner tasks |
| R4 | ≈ 1,100 manual; ≈ 3,500 automated (conditional) | |
| R5 | ≈ 3,000 (conditional) | |
| **Total** | **≈ 4,400 unconditional; ≈ 11,000 if R4.6 and R5 run** | Per-trial cost is measured in R1.2 |

The unconditional spend roughly halves vs 10-19 (~10.8–12.2k), and every
unconditional trial buys a shipped default or a decision.

## 10. Issue dispositions

| Issue | Disposition |
|---|---|
| #293 | R0 — tracking; R0.4, R0.6 |
| #403 | R0.5 |
| #388 | R1.1, R1.4 |
| #404 | R1.2 |
| #395 | R1.2 (constants) + R4.4 |
| #407 | R1.3 |
| #405 | R1.6 |
| #226 | R1.7 — **re-scoped**: contamination probe + optional guard; no longer the OOD gate |
| #389 | R1 — **re-scoped**: size N for sealed TB2.1, LoopsBench and replay; SWE-Pro only if R1.7 passes |
| #397 | Gate G1′ record (re-scoped) |
| #78 | R2.1 |
| #408 | R2.3 |
| #409 | R2.4 |
| #406 | R4.3 (last surface) |
| #399 | R3.1 |
| #298 | R3.3 — **unparked**; RSR recipe; `accepted` source only |
| #229 | R3.5 — stays parked until `revised` passes L1 |
| #390, #391, #392 | R4.1–R4.5 |
| #393 | R4.6 (moved from P3) |
| #396, #224 | R5 |
| Closed since 10-19 | #360 (#433), #402 (#429), #424, #430 (#432) |

**New issues** (titles; bodies follow the template in §12):

- **R0.1** stats: fleet capture-health report (real-user settled /
  verdict mix / `ok` share per project)
- **R0.2** cmdlog: edge-authored messages never mint turns (stall, todo,
  reconcile, verify retries) — audit on real rows
- **R0.3** eval: second blind label pass — non-accepted verdict
  precision on post-#432/#433 rows
- **R1.3** eval: freeze TB2.1 dev/sealed split (seed + hash)
- **R1.5** six lever issues:
  - edges: LLM advisor on edge fire (judge stays deterministic)
  - edges: evidence-conditioned gates (generalize `scope_gate`)
  - verify: Goldfish fresh-context verifier (no transcript, no
    completion claim)
  - verify: "find bugs" framing A/B
  - edges: structured `escalate-human` handoff report
  - context: fold finished agent turns + cache-aware chunked rendering;
    decide notebook/stub defaults
- **R2.2** eval: LoopsBench subset adapter (regression obligations)
- **R2.5** notebook: tag entries observed vs inferred; drop unsupported
- **R2.6** docs: EGM dogfood experiment → plan-artifact decision
- **R2.7** docs: refresh engine-fit table (13 hook events)
- **R3.2** learn: corrections → path-scoped rules with checks;
  exposure-aware gate
- **R3.4** three memory issues:
  - memory: capability-conditioned channel enablement
  - memory: phased digests + struggle-gated recall
  - memory: audit `notebook_sync_mem0` vs memory-off
- **R3.6** eval: learning-slope protocol (temporal held-out, mean not
  best)

## 11. Standing rules

Carried forward from 10-17 and 10-19:

- Efficiency is a first-class claim.
- No pass-rate claim without the matched-budget arm.
- Abstracts are not evidence.
- One hypothesis per candidate.
- Verifiers are invisible to the proposer.

New:

- **Integrity before volume.** No metric or learner reads a verdict
  class without measured precision (L1).
- **Deterministic judge, model advises.** No LLM judges the run
  boundary unless a deterministic trigger fired.
- **Fresh eyes verify.** A completion check never sees the worker's own
  success claim.
- **Exposure-aware measurement.** An item that never reached the agent
  is neither kept nor deleted on that evidence.
- **No always-on growth.** New learned text is path- or
  trigger-scoped.
- **Mean, not best; held-out, not in-sample.** Learning curves report
  the mean over rounds on held-out tasks.
- **Memory needs a memory-off arm** on the model actually in use.
- **Sealed means sealed.** The sealed split is read only at phase exits
  and never informs edits.

## 12. Issue template for new and re-scoped issues

```
Roadmap 2026-11-09 — target <ID> (<phase>)

## Why
<one paragraph; cite the evidence row from the roadmap / report>

## Scope
- <bulleted deliverables>

## Acceptance
- <measurable criteria tied to the target ID and split>

## Evidence / references
- <papers with evidence level; prior issues/PRs>

## Out of scope
- <explicit exclusions>
```

## 13. Superseded

- **10-19:** P0–P5 map to R0–R5 as in §10.
- **T0.1** is satisfied in code by #433 but re-verified as R0.1.
- **T0.2** is replaced by R0.4 (tracking).
- **T0.3** is met (#429).
- **T0.4** becomes R0.5.
- The 10-19 phase targets T1–T5 are replaced by §8.

## References

- [2026-11-08 report](2026-11-08-report-harness-direction.md) §4 and
  §13 for all external evidence and evidence levels. Key sources: RRSI
  2609.24972; LivePlan 2608.06701; ReFold 2610.07863; Cortex
  2610.14216; REM 2610.27377; PILOT 2608.26530; Terminal-RSI
  2610.22192; dynamic concurrency 2610.10263; SWE-Bench Pro V2 (Scale,
  2026-09-22); Elephant-Goldfish Model (google).
- [2026-10-19 roadmap](2026-10-19-roadmap-evidence-calibrated-evolution.md)
  for the regularized-evolution design, integrity controls and the
  earlier literature ledger.
