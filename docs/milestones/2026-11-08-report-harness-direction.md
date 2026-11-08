# 2026-11-08 — Report: position, research, and direction toward a top-tier long-horizon, self-improving harness

**Type:** report (analysis + recommendations). **Not a plan of record.**
The plan of record remains
[2026-10-19-roadmap-evidence-calibrated-evolution](2026-10-19-roadmap-evidence-calibrated-evolution.md),
as amended 2026-11-07 and corrected 2026-11-08. Dogfood figures here
come from the
[11-08 P0 data audit](2026-11-08-exp-p0-data-audit.md). Targets in this
report are *proposed*; per the roadmap's standing rule they become
pre-registered only through a dated roadmap amendment.

**Status changes after publication** are recorded in
[§13 Addendum](#13-addendum--status-after-publication-egm-and-terminal-rsi):
- R8 was fixed by #433.
- T0.3 met at 95% accepted precision, but with low accepted recall.
- Run-edge defects were fixed in #432.
- Analyses of the Elephant-Goldfish Model and Terminal-RSI were added.

**Question answered:** how does this fork become a top-tier harness that
(a) completes long-horizon tasks and (b) gets measurably smarter every
time the owner uses it?

**Evidence levels used below:**

- **[full]** — results tables, protocol, and ablations read.
- **[abs]** — abstract, introduction, or a detailed secondary summary
  only. Per the roadmap rule "abstracts are not evidence", these are
  leads, not evidence.
- **[secondary]** — third-party aggregator or blog; directional only.

---

## 1. Executive summary

1. **Don't compete on frontier capability.** At the frontier the best
   production harness adds only ~3–5pp over a neutral bash scaffold on
   Terminal-Bench 2.1, and METR found Claude Code and Codex did not
   beat simple scaffolds on time horizon. The winnable position is
   **the best harness for a mid-tier model on long tasks, measurably
   improving on the owner's own work.** Harness gains are largest for
   mid-tier models (RRSI: Gemini 3.5 Flash +14.1 on TB2.1; LivePlan:
   DeepSeek-V3 +12.3 on SWE-Pro) and for low-diversity personal
   workflows.
2. **The strongest, cheapest lever is run control, and Crush already
   has half of it.** LivePlan's design — a *deterministic* monitor
   judges, an LLM advisor is called *only when it fires* — gave +9.9pp
   on average for $0.08 per task. Crush's run edges are that
   deterministic judge today, with no LLM call in the edge layer. The
   missing half is the advisor call.
3. **Long-horizon context should be reversible, not predictive.**
   ReFold's reversible stubs and folds cut tokens 18–60% with resolve
   rate within noise. Crush's tool-result pruning is the same family of
   mechanism but ships **off by default** and doesn't fold the agent's
   own turns.
4. **"Smarter with use" should mean path-scoped rules with checks,
   gated by replayed tasks**, not growing memories or skills. Cortex
   (+55pp held-out) kept 16 of 18 items as path-scoped rules. An
   always-on skill was opened in 0 of 198 runs. TellOnce: Mem0 still
   violates 57.5% of preferences, while scoped rules with checks reach
   70.1% compliance.
5. **Memory is an intervention whose sign depends on the model.** REM:
   memory helped models at ≤31% unaided resolution and hurt models at
   ≥47% (Opus 4.8: 47.5→39.4). Generic memory systems underperformed
   memory-off in 11 of 12 pairings (VibeMemBench). Memory channels need
   a capability gate, and the mem0 sync path needs scrutiny.
6. **The current roadmap has three risks to resolve before spending on
   P1:**
   - The OOD gate (SWE-Bench Pro V2 public) is near-saturated and, per
     Scale, plausibly memorized.
   - The copied RRSI selector admits token growth, while the
     pre-registered P3 success criterion requires token reduction.
   - The engine-fit audit is stale on hooks.
7. **The learning loop is blind on the owner's main projects.** On
   nexus, kiosk and valo-backend, **100% of hunk captures are
   `out_of_workspace`** (19 / 15 / 9 rows, zero `ok`). Their AGENTS.md
   mandate editing in sibling worktrees (`../nexus-<slug>`), and the
   capture scope gate is the main checkout. **aurum and borneo — where
   work is moving — use the same workflow.** Until capture follows
   linked worktrees, edit cost (T5.2), preference mining (#229) and
   revision signal cannot accumulate there, however much the harness
   is used. **This is the top-priority fix.**
8. **P0 is not met after the 11-08 audit.** 142 of 344 raw settled rows
   were harness-authored reconcile repair prompts. Real-user data: 202
   settled (<250), 43 accepted (<80), 13 revised, 37 turns with
   captured content (all vishnu).

---

## 2. Current condition

### 2.1 Vision and plan of record

- **Vision** (`VISION.md`):
  - Good planning and documentation.
  - Correct context, not complete context.
  - Bounded autonomy: deterministic at the boundaries, autonomous inside
    them.
  - Success metrics: tokens-to-done per task, post-checkpoint revision
    rate per gate class, and doc artifacts that get read.
- **Plan of record** (10-19 roadmap): offline RRSI-style regularized
  harness evolution.
  - Evolve set: Terminal-Bench 2.1. OOD gate: SWE-Bench Pro V2.
  - Primary endpoint: tokens-to-done at pass non-inferiority.
  - Solver: `deepseek-v4.1-flash`.
  - Phases P0–P5, with Gate G1 deciding commit vs. fallback.

### 2.2 What ships today

| Area | Mechanism | Default |
|---|---|---|
| Run boundary | Run edges: verification, todos-reconcile, `escalate-human`, `phase-confirm`, stall-replan, burn-watch, verify-loop, reconcile. Thresholds are Go constants (`maxRepairAttempts=2`, `burnWatchStepsThreshold=30`, `verifyLoopMinSteps=8`, `loopDetectionMaxRepeats=5`, `scopeGateMinExploration=8`). **No LLM call in the edge layer** | On |
| Verification | `verifyingTool` decorator, `verify` crushrc builtin, end-of-turn gate with bounded retries (#28) | On when configured |
| Memory | `failure_memory` (30-day TTL), `command_memory`, `turn_outcomes` (settled labels + hunk capture), `referent_*`, `session_digests` (FTS5), `project_rates` | Failure memory on; others opt-in |
| Edit capture | `hunk_added` / `hunk_after` cells; scope gate = main checkout (`workspacePathSafe`, `pathEscapesWorkspace` in `cmdlog/turns.go`); outside paths store state only | On — **blind to sibling worktrees** |
| Context | Notebook (per-event summaries, checkpoint of "established facts vs open questions"), superseded-result stubs with `result:` recall, `map` project index, LSP tools | **Notebook and stubs off** |
| Cross-session | `notebook_sync_mem0` to an MCP mem0 server | Off |
| Hooks | 13 events: Pre/PostToolUse, PostToolUseFailure, PermissionRequest/Result, Session/Turn Start/End, StopFailure, Interrupt, Pre/PostCompact | — |
| Permissions | Modes `normal` / `yolo` / `sysadmin`; no OS sandbox | — |
| Subagents | `agent` tool, synchronous only | — |
| Eval | 62 trajectories, 50 manifests, ~3,600 preserved runs, power gate, 8 alarm kinds, LTT calibration; blind label tooling (`eval/labels/`, #427) | — |
| Measurement | `crush stats trend` with the repair-prompt filter (v2 ledgers, 11-08) | Run, but no usable baseline yet (1 measured week, vishnu only) |

### 2.3 Parked or unshipped design pillars

| Design | Status |
|---|---|
| `SANDBOX_RUNTIME.md` (#78) | Parked — a prerequisite for unattended proposer runs (T2.7) |
| `PLAN_ARTIFACT.md` | Parked — VISION calls planning and docs "least mature" |
| `BACKGROUND_SUBAGENTS.md` / `SWARM_DEDUP.md` | Plan drafts; #3 closed not planned |
| `SEMANTIC_INDEX.md`, `TURN_CONTEXT.md` | Spec |
| `PROCEDURAL_MEMORY.md` (#298) | Design proposal, blocked on T0.3 |

### 2.4 Evidence position (from `docs/milestones/README.md`)

- **Proven:** failure memory changes next-session behavior (−27% total
  calls, replicated p<0.01 ×2, zero pass harm).
- **In-sample only:** verify-loop edge (86/88 vs 78/88,
  cross-invocation); resolver −33.3% discovery.
- **Corpus limitation:** control arms sit at the pass ceiling, so the
  corpus measures efficiency, not capability.
- **Untested:** whether harness evolution yields held-out gains here.
- **Learning-loop data is negligible:** one `referent_episodes` row and
  one `project_rates` row across the four active DBs (11-08).

### 2.5 Dogfooding data (11-08 audit; repair prompts excluded)

| Project | Rows | Raw settled | Real-user settled | A | R | F | U | Captured content | Hunk cells `out_of_workspace` / `ok` |
|---|---:|---:|---:|---:|---:|---:|---:|---:|---|
| vishnu | 177 | 177 | 108 | 34 | 13 | 31 | 30 | 37 | 19 / 40 |
| nexus | 102 | 100 | 58 | 9 | 0 | 11 | 38 | 0 | **19 / 0** |
| valo-backend | 48 | 45 | 20 | 0 | 0 | 7 | 13 | 0 | **9 / 0** |
| kiosk | 24 | 22 | 16 | 0 | 0 | 11 | 5 | 0 | **15 / 0** |
| niceday-webportal | 6 | 0 | 0 | 0 | 0 | 0 | 0 | 0 | — |
| **Total** | **357** | **344** | **202** | **43** | **13** | **60** | **86** | **37** | — |

The last column counts rows containing each state, and is measured for
this report.

Observations:

- **Unknown verdicts: 43% of real-user settled turns** (86/202).
- **All captured edit content and all revised verdicts come from
  vishnu.** vishnu is the only project without a mandatory
  sibling-worktree workflow.
- In nexus, kiosk and valo-backend, captured paths look like
  `…/lmv/nexus-pg18-upgrade/README.md`: sibling worktrees, rejected by
  the scope gate.
- aurum (`~/Documents/p/lmv/aurum`) and borneo
  (`~/Documents/p/personal/borneo`) both prescribe
  `git worktree add ../nexus-<slug>`. Neither is registered yet.
- The harness repository itself is not dogfooded.
- The nexus `.crush` was recreated on Oct 28, which voided the 10-19
  baseline.

### 2.6 Milestone status

| Milestone | Open / closed | State |
|---|---|---|
| P0 dogfood + labels | 3 / 2 | T0.1 met (37 ≥ 20). **T0.2 not met** (202 < 250 settled; 43 < 80 accepted). T0.3 blind sample frozen (hash `e7686a42…`), not yet labeled. T0.4 trend v2 rows exist but no comparable two-week edit-cost baseline. T0.5 holding |
| P1 feasibility → G1 | 7 / 1 | Not started (Harbor agent #388 is the critical path) |
| P2 ledger + rounds | 8 / 0 | Not started; engine enablers #406, #408, #409 open |
| P3 automated pilot | 1 / 0 | — |
| P4 replicate | 1 / 0 | — |
| P5 personal loop | 2 / 0 | Gated on P4 or fallback |

---

## 3. Competitive position

Leaderboard figures are from a secondary aggregator of tbench.ai (as of
mid-July 2026) **[secondary]**. Feature claims are from vendor docs.

| Axis | Top tier (Claude Code / Codex) | This fork | Verdict |
|---|---|---|---|
| TB2.1 pass | CC + Fable 5 **83.8%**; Codex + GPT-5.5 **83.1%** | Never measured | Unknown |
| Harness lift over Terminus 2 (same model) | +3.4pp (CC), +5.1pp (Codex) | Unknown — the comparison that matters | Unknown |
| Sandbox | Codex: OS-level (Seatbelt / bwrap + seccomp), workspace-write and no network by default. CC: `/sandbox` with file and network isolation | Permission modes only | **Behind** |
| Parallelism | CC: background subagents by default, nesting, forks that keep the cache, scripted workflows | Synchronous `agent` tool | Behind (but see §4.6 — low priority) |
| Hooks | CC: 33 events, `onFailure: block` | 13 events | Adequate |
| Memory | Auto-memory and AGENTS.md / CLAUDE.md; no published effect | Structured, provenance-tracked; replicated effect | **Ahead** |
| Run control | Stop hooks, `/loop`, auto-reviewer | Deterministic run edges | **Distinctive** |
| Eval discipline | Internal | Power gate, pre-registration, alarms, blind labels | **Ahead** |
| Model breadth | Mostly tied to one vendor | Many providers; tuned for mid-tier | **Strategic niche** |

---

## 4. Research findings

### 4.1 Harness evolution

| Paper | Level | Finding | Consequence |
|---|---|---|---|
| **RRSI** 2609.24972 (Google) | [full] | Regularized evolution (annealed edit budget 4→1, leakage critic, noise floor δ, cost rule, windowed pruning). Evolving on TB2.1 transferred to SWE-V +1.8 (CI [+1.1, +4.0]). With Flash: +14.1 evolve, +2.2 OOD. Unregularized evolution: highest evolve score, OOD ≈ base, 3.80M tokens. A second independent search landed within 1.8pts. **4 of 32 candidates kept, all at the pre-finalize check** (verification audit + non-blocking polling of long jobs; a recovery hint for a recurring tool error). **The evolved harness used +55% tokens vs. base (2.42M vs 1.56M)** | Validates the roadmap's design. Points evolution at the run-boundary seam. Contradicts a token-reduction-only claim |
| HarnessDev 2609.01437 | [abs] | Only 34 of 64 evolved changes generalized. Held-out scores fell while the feedback score rose. Gains depend on the executing model | Unregularized evolution fails. Select by held-out results |
| Self-HPO 2610.22791 | [abs] | Harness-only change: +2.4 on Pro; adding RL gives more | API models can't be RL-trained, so expect harness-only magnitudes |
| PILOT 2608.26530 | [full] | One-shot supervisor–worker: +4.4–5.3 over Pi/OpenCode on TB2.0 and SWE-Pro. **The self-improvement +14.6 is in-sample**: the same 89 tasks every iteration, the verifier choosing which skills to keep, and the *best of 20* iterations reported | Trust the one-shot result. The learning-slope protocol must be held-out and report the mean, not the best |
| TTHE 2607.08124 | [abs] | Test-time evolution from unlabeled traces | In RRSI's comparison it ended 1.7 below base on OOD |

### 4.2 Run control and verification

| Paper | Level | Finding | Consequence |
|---|---|---|---|
| **LivePlan** 2608.06701 | [full] | Deterministic monitor plus an LLM advisor called only on trigger. DeepSeek-V3 on SWE-Pro: 21.8→34.1; Gemini 2.5 Flash 13.2→28.4; SWE-V +10.6–11.2. Periodic advisor scored lower; a post-hoc re-planner (SAGE) was worse than vanilla. Advisor: GPT-5.2-Codex | **Add an advisor call to run-edge fires.** Keep judging deterministic |
| ECLoop 2607.28815 | [abs] | Evidence conditions before edit or submit: SWE-V +4.8–11.8pp, tokens −12%. Self-Refine hurt | Generalize `scope_gate` into evidence conditions |
| Self-verification 2609.38812 | [abs] | On TB2.1, agents verify 99.5% of the time but catch only 61% of wrong candidates and repair only 49% of those | Improve detection, not frequency |
| Agentic overconfidence 2602.06948 | [abs] | 22% actual success vs 77% predicted. A "find bugs" framing cuts overconfidence by up to 15pp | A/B the verify-gate prompt framing |
| ImpasseBench 2610.31895 | [abs] | Escalation tool: budget exhaustion 34%→6.7%. Handoff tickets left out progress and verification status | Structured `escalate-human` report |
| Cadence 2610.12269, HiSentinel 2609.39957 | [abs] | Adaptive inspection frequency; small pre-execution gates | Later edge candidates |

### 4.3 Long-horizon context

| Paper | Level | Finding | Consequence |
|---|---|---|---|
| **ReFold** 2610.07863 | [full] | Reversible dedup stubs plus folding of turns the agent marked finished, with chunked re-rendering (k=3). Tokens −18 to −60%, cost −2 to −45%, resolve rate within noise (n=50 per benchmark). Masking dropped cache hits 84%→23% and raised cost 31%. Tool outputs are ~59% of the footprint, so the agent's own turns must be folded too | Extend superseded-result stubs with agent-turn folding and cache-aware chunking; measure; enable by default |
| Source-Judged Compaction 2610.20927 | [abs] | 83% of failed runs trace back to an unsupported summary entry | Tag notebook and compaction entries as observed vs inferred (the checkpoint's "facts vs open questions" split is a start) |
| Context Folding, AgentFold, ACM | [abs] | Need RL-trained models | Not applicable |
| GitHub Copilot telemetry (cited in ReFold) | [abs] | 7.8% of sessions compact, yet they account for 44.2% of tokens | Long sessions dominate cost |

### 4.4 Memory and experience

| Paper | Level | Finding | Consequence |
|---|---|---|---|
| **REM** 2610.27377 | [full] | A ~270-token phased document (localize / understand / fix / verify) beats summaries and raw trajectories, even oracle-retrieved ones. **Sign depends on the model:** +1 to +4pp for models at ≤31% unaided, −2 to −9pp for models at ≥47%. Struggle-gated recall and one-shot eviction reduce harm (not significant at n=297) | Capability gate on memory channels; phased digests; stall and burn-watch as struggle signals |
| VibeMemBench 2609.23570 | [abs + review] | Verified 10-line records: +0–4.5pp (deepseek-v4-pro +0.0), fewer steps on all 5 solvers. Mem0 / A-MEM / SimpleMem / MemoryOS: 11 of 12 pairings ≤ memory-off (Mem0 −5.5 on glm-5). 69% of failures were form degradation | Small verified records only. Audit `notebook_sync_mem0` |
| MemDev-Bench 2610.27524 | [abs] | Memory breaks otherwise-solvable tasks when the agent doesn't reassess whether it applies | An applicability clause on every item |
| Subtask-level memory 2602.21611 | [abs] | Memory by subtask beats memory by whole task: +4.7pp on SWE-V | Supports phase-structured storage |

### 4.5 Learning from use and personalization

| Paper | Level | Finding | Consequence |
|---|---|---|---|
| **Cortex** 2610.14216 | [full results section] | Session fixes become replayable tasks. Recurring corrections become one rule at a time. A rule is kept only if it helps without breaking any task, measured only in runs where it reached the agent. **+55pp held-out (Haiku 4.5)**; control family −0.8. 16 of 18 kept items were path-scoped rules; always-on skill opened 0 of 198 times; zero always-on text added. **Sonnet 5: +5.6 (not significant). A real repo had nothing to learn because corrections didn't recur.** Known-harm detection was weak | Blueprint for the slow loop. Recurring signal *and working capture* are prerequisites (see R8) |
| TellOnce 2606.13174 | [abs + intro] | Mem0: 57.5% violations. Plain rules: 55% compliance. Scoped rules with checks: 70.1% | Corrections become scoped rules plus checks |
| VibeTrace 2611.04466 | [abs] | Trace and feedback distilled into skills: input tokens −23%, pass 65→68% | Supporting |

### 4.6 Parallelism

| Source | Level | Finding |
|---|---|---|
| Dynamic concurrency 2610.10263 | [full results] | 1.41–3.31× tokens. Up to +14.3pp on long-horizon tasks; **Claude Code −24pp on bounded SWE-V**. Benefits only for complementary work, exploring alternatives, or independent validation |
| Vals agent teams | [secondary] | 1.8–5.1× cost; 1 of 4 differences significant |
| Subagent-search critique | [secondary] | An index beat delegation 65% vs 46%. 42% of delegation failures happen at the handoff; 91% of those still return confident answers |

**Consequence:** parallelism is the lowest priority. Use the `map` index
for read-only questions. Any delegation must return evidence (paths,
line ranges), not prose.

### 4.7 Benchmarks and measurement

| Source | Level | Finding | Consequence |
|---|---|---|---|
| SWE-Bench Pro V2 (Scale blog, 2026-09-22) | [full post] | Public split: Opus 5 99.4%, Kimi K3 97.7%, GLM-5.3 95.6%. Opus 5 private 81.6%, a 17.8pp gap attributed to training-time exposure. Reflection's study: harness changes moved accuracy little but cost a lot | **The OOD gate is compromised.** Supports a cost endpoint |
| LoopsBench 2608.00267 | [abs] | 112 long-horizon tasks built as dependency DAGs of units; completed units keep running as regression checks. Best configuration: 25% | Long-horizon anchor |
| METR scaffold note (2026-02-13) | [secondary] | CC / Codex didn't beat ReAct / Triframe on time horizon (Opus 4.5, GPT-5) | Harness matters less at the frontier |

---

## 5. Risks to the current roadmap

| # | Risk | Severity | Resolution |
|---|---|---|---|
| R8 | **Edit capture is blind to sibling worktrees** — the mandated workflow on nexus, kiosk, valo-backend, aurum and borneo. 0 `ok` hunk cells on three active projects. Settlement survival and revision checks read the main checkout, not where the edit happened (related: #360, #424/#425) | **Critical** | Treat linked worktrees of the same repository (same git common dir) as in-scope capture roots. Settle against the worktree while it exists; after merge, settle against the merged commit on the default branch. Keep redaction and sensitive-path gates unchanged. Add a fleet health check: `ok` share per project |
| R1 | **OOD gate saturated or contaminated** (SWE-Pro V2 public near ceiling). T3.3 pass non-inferiority becomes trivial, T3.4 impossible, and transfer claims rest on recalled tasks | High | Make T1.6 (contamination probe) **gating**. Measure the production solver on the 100-task sample. If >90%, move OOD to V2 Hard (51 tasks) plus a held-out TB2.1 split, or limit the claim to tokens only |
| R2 | **Endpoint vs. selector conflict.** The RRSI cost rule admits token growth for gains > δ, while T3.3 requires OOD tokens to fall. RRSI's own harness cost +55% vs. base | High | Choose before T2.6: (a) pass primary with a token guardrail, or (b) an efficiency-only selector (ΔC ≤ −c_min) |
| R5 | **Learning-signal starvation.** 202 real-user settled; 13 revised (all vishnu); 0 accepted on 2 of 4 projects; 1 `referent_episodes` row; Cortex's real-repo null | High (largely caused by R8) | Fix R8 first, then label T0.3, register aurum and borneo, and harvest replay tasks continuously |
| R3 | **Mature-seed discount.** The mechanisms RRSI found (pre-completion audit, long-job polling, recovery hints) already exist in Crush | Medium | Calibrate expectations downward; prioritize mechanisms Crush lacks (advisor-on-fire, evidence conditions, folding) |
| R6 | **Budget.** ~11–12k trials for P1–P4 on a single owner's budget | Medium | Run cheap levers (§7.3, Phase 1) before big searches; G1 budget check (T1.7) |
| R7 | **Product parity drift.** Sandbox, planning and parallelism stay parked while measurement work dominates | Medium | Sandbox moves onto the critical path (Phase 2) |
| R4 | **Engine-fit audit is stale.** It says only PreToolUse and PreCompact exist; there are 13 events | Low | Refresh the table; #409's hook surface is larger |
| R9 | **Data-integrity regressions.** The 11-07 read overcounted by 148 rows (repair prompts, unsettled rows, capture attempts counted as content) | Medium | Every volume or label report goes through the audited filter; a fleet-health command reports real-user settled, accepted, revised and `ok` capture per project |

---

## 6. Targets (proposed)

### 6.1 North star

> **The best harness for a fixed mid-tier model on long-horizon tasks —
> highest pass and lowest tokens-to-done against other harnesses on the
> same model — with performance that rises, on held-out tasks, as the
> owner uses it.**

### 6.2 Scoreboard

| ID | Metric | Protocol | Proposed target | Basis |
|---|---|---|---|---|
| H0 | **Capture health** (precondition) | Per project: share of edited paths with `ok` hunk cells, real-user turns | **≥ 80% `ok`** on every active project, including worktree workflows | R8 |
| N1a | TB2.1 pass and tokens-to-done, Crush vs. Terminus 2 | `deepseek-v4.1-flash`, k=2, task-level paired bootstrap (#407) | Crush ≥ Terminus 2 − δ (baseline sanity) | RRSI δ ≈ 3 passes / 178 trials |
| N1b | Same, after Phases 1–2 | Same | **+≥5pp pass at tokens ≤ +25%**, OR **tokens-to-done −≥20% at pass ≥ −δ** | LivePlan +9.9 avg; ReFold −18 to −60% tokens; RRSI cost rule (25% per pass) |
| N2 | Long-horizon | LoopsBench subset (20–30 tasks, mixed languages) | Baseline first; then resolve non-inferior and **regression events ≤ baseline** | LoopsBench best 25% |
| N3 | **Learning slope** | In-domain replay set from the owner's projects; temporal split (learn on earlier, test on later); ≥3 learning rounds; **mean, not best** | Slope CI lower bound > 0 on pass or tokens, the other non-inferior | Cortex, RRSI; PILOT caveat |
| N4 | Personal trend | `crush stats trend` weekly (v2, repair-filtered) | Existing T5.2 (edit cost −20% over 4 weeks vs a T0.4 baseline taken *after* H0 holds; revision rate not rising) | CIPHER, 10-19 roadmap |
| G1 | Harness growth | Rendered lessons + rules + preferences | ≤ 8 KB rendered (T5.5); **0 bytes of new always-on text** | RRSI Table 9, Cortex |
| G2 | Memory safety | Per model-capability tier | No memory channel net-negative on any tier in use | REM |

---

## 7. Suggestions

### 7.1 Direction

Combine four elements:

1. **Deterministic run control with a triggered advisor**: LivePlan
   pattern on the existing edges.
2. **Reversible context**: ReFold pattern on the existing pruning.
3. **A regularized learning loop fed by the owner's own replayed tasks**:
   RRSI gate plus Cortex measurement.
4. **Path-scoped, checkable rules as the unit of learned behavior**:
   Cortex and TellOnce.

This is VISION's "deterministic at the boundaries, autonomous inside
them," applied to both execution and learning. **None of the learning
half works until capture sees the worktrees where the owner actually
edits.**

### 7.2 Architecture

| Layer | Role | Crush seam |
|---|---|---|
| Judge (deterministic) | Detects drift: stall, loops, burn, missing evidence, unverified completion | Run edges, `scope_gate`, verify gate |
| Advisor (LLM, on fire only) | One high-level next-step correction; may escalate to the user | **New** — called from edge fires; model configurable (stronger than the solver, as in LivePlan) |
| Context renderer | Reversible stubs and folds; cache-aware chunking; observed/inferred tagging | Notebook, superseded-result stubs |
| Plan anchor | Goal and phases the judge checks against; human confirmation points | `PLAN_ARTIFACT.md` (unpark after Phase 1 results) |
| Capture | Edit content and survival across the main checkout **and linked worktrees** | `cmdlog/turns.go` scope gate (R8) |
| Learning store | Path-scoped rules with applicability and checks; failure memory; phased digests | `failure_memory`, `session_digests`, lessons via `context_paths` |
| Learning gate | Replay, exposure-aware A/B, RRSI regularizers, prune | #390–#392, #399 |
| Containment | Workspace confinement + OS sandbox (must also understand linked worktrees) | #78 |

### 7.3 Build order, exits and kill criteria

| Phase | Build | Exit | Kill |
|---|---|---|---|
| **0 — Fix capture, close P0, fix the plan** | **Worktree-aware capture and settlement (R8)** + capture-health report (H0); T0.3 labels on the frozen sample (#402); roadmap amendment for R1, R2, R4; register aurum and borneo | H0 ≥ 80% on active projects; labels reported; amendment merged | Accepted precision < 60% → #298 stays blocked (existing rule) |
| **1 — Anchor + cheap levers** | Harbor agent (#388), N1a. Then each as its own arm: advisor-on-fire, evidence conditions, "find bugs" verify framing, escalation report, agent-turn folding with cache-aware chunking | N1a measured; each lever has a paired read | Crush < Terminus 2 − δ → fix the basics first. A lever that fails its paired read is dropped |
| **2 — Long-horizon core** | Sandbox D+A (#78); LoopsBench subset (N2); edges → params (#408); notebook and stubs on by default if Phase 1 reads hold | N1b met; N2 baseline recorded | No N1b after two lever rounds → re-scope to an efficiency-only claim |
| **3 — Learning from use** | Replay-task harvest (#399); corrections → path-scoped rules with checks; exposure-aware keep/prune; phased digests; capability-gated memory; mem0 audit; ledger + manual rounds (#390–#392) | N3 slope reported (held-out, mean); T0.2 / T0.4 met on clean data | Slope ≤ 0 after 3 rounds → prune the store, stop automation |
| **4 — Automation + replication** | Automated proposer (#393) only if Phase 3 shows gains; second independent search (#396); plan anchor unparked | Replication within CI | Two flat runs → publish the null (existing P4 rule) |
| **Last** | Background workers, for independent validation only | — | — |

### 7.4 Proposed new issues

| Title | Phase | Evidence |
|---|---|---|
| **cmdlog: capture and settle edits in linked worktrees (same git common dir)** | 0 | R8, §2.5 |
| **stats: fleet capture-health report (real-user settled / A / R / `ok` share per project)** | 0 | R8, R9 |
| roadmap: amend OOD gate (contamination probe gating), endpoint, engine-fit | 0 | §5 |
| edges: LLM advisor on edge fire (judge stays deterministic) | 1 | LivePlan |
| edges: evidence-conditioned gates (generalize `scope_gate`) | 1 | ECLoop |
| verify: adversarial "find bugs" framing A/B | 1 | Overconfidence study |
| edges: structured `escalate-human` handoff report (progress + verification state) | 1 | ImpasseBench |
| context: fold finished agent turns + cache-aware chunked rendering | 1 | ReFold |
| notebook: tag summary entries observed vs inferred; drop unsupported | 2 | SJC |
| eval: LoopsBench subset adapter (regression obligations) | 2 | LoopsBench |
| memory: capability-conditioned channel enablement | 3 | REM |
| memory: phased-document digests + struggle-gated recall | 3 | REM, subtask memory |
| memory: audit `notebook_sync_mem0` against memory-off | 3 | VibeMemBench, TellOnce |
| learn: corrections → path-scoped rules with checks; exposure-aware gate | 3 | Cortex, TellOnce |
| eval: learning-slope protocol (temporal held-out, mean-not-best) | 3 | PILOT caveat, RRSI |

### 7.5 Existing issues reprioritized

| Issue | Change |
|---|---|
| #360 (commit evidence walks checked-out HEAD only; worktrees read wrong checkout) | **Raised from low to Phase 0** — same root cause family as R8 |
| #388 Harbor agent | Highest priority after Phase 0 — gates every public number |
| #78 sandbox | Phase 2 critical path; confinement must allow linked worktrees |
| #408 edges → params | Raised — every edit RRSI kept lives at this seam |
| #399 replay tasks | Raised — harvest from kiosk, aurum, borneo once capture works |
| #226 SWE-Pro OOD | Re-scoped per R1 |
| #393 automated proposer | Deferred until the Phase 3 slope is positive |
| #3 background subagents | Remains closed; revisit only for independent validation |

---

## 8. Stop doing

- Counting volume or labels without the audited filter (repair prompts,
  unsettled rows, capture attempts vs content).
- Treating SWE-Bench Pro V2 public as the headline OOD claim.
- Running an autonomous proposer before manual rounds show gains.
- Adding always-on context: every new item must be path- or
  trigger-scoped.
- Adding label infrastructure beyond defects seen in real rows (T0.5).
- Chasing surface parity: plugins, mods, cloud, IDE.
- Adding memory without a memory-off comparison on the model actually
  in use.

## 9. Immediate next actions

1. File and fix **worktree-aware capture** (R8), together with #360.
   Verify with a capture-health read showing `ok` cells on nexus or
   kiosk after one worktree session.
2. Label the frozen T0.3 sample (`eval/labels/`, #402).
3. Roadmap amendment: R1 (OOD gate), R2 (endpoint), R4 (engine-fit
   refresh), R8 (capture precondition), plus a pointer to this report.
4. Register aurum and borneo (open each once in Crush) after the
   capture fix lands, so their first rows are usable.
5. File the Phase 1 issues in §7.4 and start #388.

## 10. Decisions for the owner

| Decision | Options | Recommendation |
|---|---|---|
| Worktree capture scope | (a) linked worktrees of the same repo; (b) any path under the owner's project roots; (c) keep the main checkout only | (a) — same repository identity, smallest privacy surface |
| Primary endpoint | (a) pass primary + token guardrail; (b) efficiency-only selector | (a) for P2–P3; report efficiency-only edits separately |
| OOD gate | V2 Hard (51) + held-out TB2.1 split; tokens-only claim; or private-set access | Decide after the contamination probe |
| Advisor model | Same as solver vs. stronger | Stronger, as in LivePlan (GPT-5.2 advising DeepSeek-V3); measure cost |
| mem0 sync | Keep / drop / gate | Gate behind a memory-off A/B |
| Autonomous search budget | Full P3/P4 vs. manual rounds only | Manual rounds until N3 > 0 |

---

## 11. Corrections log (analysis history)

- **11-07 dogfood counts were wrong.** The 11-07 read (PR #426 and the
  #293 status comment) reported 350 settled / 86 accepted / 103
  hunk-captured and declared T0.1 and T0.2 met. The 11-08 audit found:
  - six unsettled niceday rows counted as settled;
  - 142 harness-authored repair-prompt rows counted as user turns;
  - kiosk's 16 failed rows omitted from the pooled mix;
  - capture *attempts* (including `out_of_workspace`) counted as
    captured content.

  Corrected: 202 real-user settled, 43 accepted, 37 with content. T0.2
  is not met. This report additionally found that the
  `out_of_workspace` cells are 100% of capture on three projects (R8).
- An earlier recommendation to demote offline evolution was
  **withdrawn** after reading RRSI in full: regularized evolution
  transfers and replicates.
- PILOT's self-improvement result was initially presented as evidence
  of learning with use. **On reading the protocol it is in-sample and
  best-of-20.** Only the one-shot result is cited as evidence here.
- A continuous LLM supervisor was initially suggested. **Replaced by the
  LivePlan pattern** (deterministic judge, advisor on trigger), which
  has stronger evidence and lower cost.
- Background workers were initially in the core build. **Downgraded to
  last** after the concurrency studies.

## 12. Limitations of this report

- Most cited papers are 2026 preprints without independent replication.
- Leaderboard figures are secondary.
- Several findings are marked [abs] and must be read in full before
  they inform a pre-registered target.
- All personal-usage numbers are single-user trends, not certified
  claims.
- The worktree-capture diagnosis rested on stored path strings and the
  scope-gate code. #433 pinned it with a linked-worktree repro test
  before fixing it. Confirmation on real turns is pending (§13.1).
- No new experiments were run for this report.

## 13. Addendum — status after publication, EGM, and Terminal-RSI

### 13.1 Status since publication

| Item | Was (§2, §5) | Now | Evidence |
|---|---|---|---|
| R8 worktree capture | Critical, open | **Fixed in code.** Capture roots are every worktree of the repo. Settlement reads the recorded checkout and falls back to the default branch after merge. Commit evidence is keyed by repo-relative paths. #360 closed | #433 (merged 2026-11-08), installed binary `c5567a31b` |
| H0 capture health | Not met (0 `ok` on 3 projects) | **Unverified.** 0 turns recorded on any project since #433 landed; aurum and borneo still have no `crush.db` | Live DB read, 2026-11-08 |
| T0.3 label validation | Sample frozen | **Met: accepted precision 19/20 = 95%** (≥ 80% bar). #298 mining may proceed | #429, `eval/labels/turn_outcomes_gold.jsonl` |
| Run-edge defects | Not identified | Found while labeling: premature completion summaries, stall-stop killing turns with no closing text, the todos edge overriding an explicit owner `stop`. Fixed in #432 | #430, #432 |
| T0.4 trend baseline | No usable baseline | Still open. Needs post-#433 / post-#432 data | — |

**New finding: accepted recall is low, and the non-accepted verdicts
are unreliable.** In the 50-row gold sample (stratified 20 accepted /
10 revised / 10 failed / 10 unknown):

| Settled verdict | Gold accepted | Gold unclear | Gold not-accepted |
|---|---:|---:|---:|
| accepted (20) | 19 | 1 | 0 |
| revised (10) | **10** | 0 | 0 |
| failed (10) | **10** | 0 | 0 |
| unknown (10) | **9** | 1 | 0 |

Every sampled `revised` and `failed` turn, and 9 of 10 `unknown` turns,
was owner-accepted. #430 traces most of these to stall-killed turns,
premature summaries, and harness-authored messages. The artifacts
shipped regardless.

The strata hold only 10 rows each, so read these consequences as
directional:

1. **T0.2's accepted floor is limited by label recall, not usage.**
   Settled-accepted is 43 of 202. If the stratum rates hold, most of the
   159 non-accepted real-user turns are owner-accepted.
2. **Revision rate currently measures nothing.** It is VISION's
   gate-calibration metric, T5.2's guard, and #229's weighting input. In
   this sample, no settled `revised` turn was a real revision.
3. **Precision is fine; negatives are not.** `accepted` is safe to mine
   (#298). `revised` and `failed` are not safe to learn from until a
   second label pass on post-#432 rows shows non-accepted precision.

### 13.2 Elephant-Goldfish Model (google/elephant-goldfish-model) [repo docs]

**What it is.** A plugin of skills and reviewer personas for Claude
Code, Codex, Antigravity and others. It is not a harness.

- **Elephant** holds project context and drafts a specification.
- **Goldfish** reviewers run in fresh subagent contexts and see only the
  document and the source it references, never the earlier
  conversation.
- **The owner** resolves trade-offs and approves the spec before
  implementation.
- **Skills:** brainstorm, draft, resolve, review (five stages), review
  loop, bootstrap (docs for an existing codebase), and drift report
  (checked-in docs vs current code).
- **Outputs:** design doc, implementation checklist, and README
  hierarchies kept next to the code.

**Evidence: none measured.** The FAQ states that completing the
workflow "does not demonstrate a productivity improvement." The
verification folder covers skill discovery only. EGM also notes that
fresh conversation context does not exclude host instruction files:
Codex supplied AGENTS.md to reviewers (issue #237).

| Crush VISION / design | EGM | Fit |
|---|---|---|
| Pillar 1: "documentation is an output, not a side effect" (least mature; `PLAN_ARTIFACT.md` parked) | Design doc + checklist + context docs as primary outputs | **Direct** |
| "Ask at commitment points — confirm before the first write" | Spec approval required before implementation | **Direct** |
| Confirmation is where the owner's taste enters the artifact | Owner makes the architectural trade-offs | Same principle |
| Docs "must name who reads what, or it rots" | Drift report | Addresses rot |
| Verify gate runs in the worker's context and sees its self-report | Fresh-context review | **Fills a measured weakness** (below) |

**Why the Goldfish principle has support even though EGM is
unmeasured:**

- Same-context self-verification catches only 61% of wrong candidates
  (2609.38812 [abs]).
- Post-execution self-assessment is badly overconfident (2602.06948
  [abs]).
- Withholding the agent's own success report from the judge improved
  detection of false success claims by **13–43pp**
  (2610.29609 [abs]).

**Adoption:**

1. **EGM dogfood experiment (cheap, parallel to Phase 1).** Add EGM's
   `skills/`, `agents/` and `resources/` to `options.skills_paths` and
   run `/egm-review` through the synchronous `agent` tool. EGM's
   general-purpose fallback reads the persona file. **Not yet
   verified:** whether Crush's task agent inherits parent context or
   context files, and whether skill-relative resource reads resolve.
2. **Measure with existing instruments.** An approved spec is a
   checkpoint, and post-approval revision rate inside its
   implementation is VISION's calibration metric. This only becomes
   valid once §13.1 point 3 is fixed and H0 holds.
3. **Goldfish verifier arm (Phase 1).** A fresh-context reviewer that
   sees the task, the diff and the test output, but not the worker's
   transcript or completion claim. A/B it against the current
   same-context verify gate. It is a natural implementation of the
   "advisor on edge fire" for the verification and premature-completion
   edges — the defect class #430 found.

### 13.3 Terminal-RSI (arXiv:2610.22192) [full]

**What it is.** RL training of Qwen3.8-27B. It alternates Recursive
Self-Rewrite (verified trajectories → runbook → leakage review → fresh
re-execution → actor-only supervision) with critic-inheriting PPO
refinement. It adds Turn-Boundary Skip-Observation GAE, family-calibrated
dense reward, and a success-prioritized reward.

- **Self-reported results:** TB2.1 86.52%, TB4.0 19.84%, TB3.0 16.95%,
  TB-Hard 79.00%; LHTB strict 5/46; SWR100 3%→6%.

**Direct fit: low.** Crush consumes API models and does not train
weights.

**Read the headline cautiously:**

- Mean@3 over three evaluations.
- The kept checkpoint (RPR-2 step 18) was chosen among branches with
  visible benchmark curves. The limitations section says selection
  "requires protected validation."
- Training used 3,000 terminal tasks. Only SWR100 is confirmed
  training-disjoint; benchmark and pretraining overlap is "incompletely
  audited."
- Continuing training kept TB2.1 but lost 2.4–3.0 points on TB3.0, TB4.0
  and Hard.

| Idea | Crush application | Seam |
|---|---|---|
| **RSR recipe:** keep a trajectory only if the verifier passes; rewrite it into a runbook without harness-control messages; a reviewer seeing only the public task and runbook screens it for leakage; fresh re-execution under the target harness must pass | The strictest published recipe for turning experience into reusable procedure. It matches Cortex's replay gate and SkillsBench's verified-trace rule. Adopt it as the #298 pipeline | #298, Phase 3 gate |
| **Partial credit masks failure.** Under the early reward, failed runs received 50.09% of reward mass, and some passed every scenario check but missed the final gate | Rank strict end-to-end success above partial progress in labels, selectors and the learning gate | `turn_outcomes`, verify gate, #390 |
| **Cross-suite trade-offs** | Select on several suites and held-out sets, never one | Same as RRSI / HarnessDev |
| A 27B open model at this level | A *possible* mid-tier solver if weights are released (not confirmed) | Model choice |

### 13.4 Changes to §7

| Section | Change |
|---|---|
| §7.3 Phase 0 | R8 is done; **verify H0 on real worktree turns.** Add **non-accepted verdict precision:** a second blind label pass on post-#432 rows (revised / failed / unknown strata) before revision rate or failed rows feed any metric or learner |
| §7.3 Phase 1 | Add the **Goldfish verifier** arm. Run the **EGM dogfood experiment** in parallel |
| §7.3 Phase 3 | #298 adopts the **RSR recipe** (verified → runbook → leakage review → fresh replay → keep) |
| §7.4 new issues | (1) eval: second label pass — non-accepted verdict precision on post-#432 rows; (2) stats: H0 capture-health read after #433; (3) verify: Goldfish fresh-context verifier arm (no self-report); (4) docs: EGM dogfood experiment via `skills_paths`; (5) memory: #298 pipeline per the RSR recipe |
| §7.5 | #360: closed by #433. #298: unblocked on precision; sources only `accepted` |

## References

Read in full or results-level:

- Xia et al. RRSI. arXiv:2609.24972 (v3).
- AllSpark Team. PILOT in the Loop. arXiv:2608.26530.
- Liu et al. LivePlan: Online Monitoring and Corrective Steering of
  Programming Agents. arXiv:2608.06701.
- Su et al. ReFold. arXiv:2610.07863.
- Chen et al. REM. arXiv:2610.27377.
- Giampieri. Cortex. arXiv:2610.14216.
- Li et al. When Sub-Agents Work in Parallel. arXiv:2610.10263.
- Scale Labs. SWE-Bench Pro V2 (blog, 2026-09-22).
- Yang et al. Terminal-RSI. arXiv:2610.22192 (v3).
- Google. Elephant-Goldfish Model (github.com/google/elephant-goldfish-model):
  README, harnesses, FAQ.

Abstract-level or secondary:

- HarnessDev arXiv:2609.01437
- Self-HPO arXiv:2610.22791
- TTHE arXiv:2607.08124
- HarnessX arXiv:2606.14249
- ECLoop arXiv:2607.28815
- Self-verification in terminal agents arXiv:2609.38812
- Agentic overconfidence arXiv:2602.06948
- ImpasseBench arXiv:2610.31895
- Self-report leakage in completion judging arXiv:2610.29609
- Cadence arXiv:2610.12269
- HiSentinel arXiv:2609.39957
- Source-Judged Compaction arXiv:2610.20927
- Context Folding (ICML 2026)
- AgentFold (ICLR 2026)
- ACM arXiv:2607.23809
- VibeMemBench arXiv:2609.23570
- MemDev-Bench arXiv:2610.27524
- Subtask-level memory arXiv:2602.21611
- SWE-ContextBench arXiv:2602.08316
- TellOnce arXiv:2606.13174
- VibeTrace arXiv:2611.04466
- Echo arXiv:2605.21984
- LoopsBench arXiv:2608.00267
- METR, Measuring Time Horizon using Claude Code and Codex (2026-02-13)
- Vals AI, Do Agent Teams Pay Off?
- tbench.ai leaderboard (via secondary aggregator)

Carried from the 10-19 roadmap (read there): AHE, Rethinking,
HarnessCompass, Harness-Updating ≠ Benefit, Better Harnesses Smaller
Models, Evo-Bench, Meta-Harness, DGM, SICA, GEPA, SkillsBench,
ReasoningBank, Experience-following, CIPHER, Adaptive Auto-Harness,
SWE-Exp.
