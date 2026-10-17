# Milestones

Dated snapshots of where this fork stands against its goal: an agent harness
that helps the user create a good product — one that gets smarter every time
it is used, and can *prove* it.

Each dated file captures one position: what was proven, what shipped, what is
parked, and the plan from that point. This README tracks only the current
position and the index; the history lives in the dated files.

## Current position

**Scope: "prove it learns" — the integrity wave shipped; labels and
certification machinery now exist.** Crush already wrote substantial
information to `crush.db` but never effectively read it back across
sessions — memory was a write-only ledger. This milestone series exists
to turn "it learns" from a claim into a measurement.

**Direction (10-17): regularized harness evolution.** Plan of record:
[2026-10-17-roadmap-regularized-evolution](2026-10-17-roadmap-regularized-evolution.md).
The learning-loop machinery is correct but has no data at single-user
volume (348 real turns, 0 `referent_episodes`, 0 `project_rates`), and
the eval corpus sits at the pass ceiling. The bet moves to an offline
RRSI-style loop that evolves prompts, tool descriptions, edges, and
memory settings, judged on tasks it never tuned on, with every kept
change landing as a reviewed PR. Gate G1 — Terminal-Bench 2.1 evolve
set, SWE-bench Verified OOD gate, δ ≤ 3pp — decides whether transfer
is measurable at all; if not, the fallback keeps the ledger discipline
without self-evolving claims.

**Failure memory — existence proof: DONE and armored.** A durable failure
record written in session N provably changes behavior in session N+1, measured
across two powered runs (−27% total calls, ~25–30% discovery savings,
replicated p<0.01 ×2, 68/68 treatment runs fired, zero pass harm). The
instrumentation cannot be fooled (composite-command verdict recovery), the
data ages out honestly (30-day TTL), and the evaluation gate provably detects
a guaranteed-null experiment.

**Mask arm resolved — interpretation corrected on artifact re-check:** the mask
seeded *true* memory (the decoy's test really failed) under an ambiguous
prompt ("the test fails" — check.sh scored only the root package). The 3
treatment fails were early termination after too-narrow verification, not
retrieval error — and file-heat would have picked the decoy anyway (it was
the seed session's file). The `382f` framing-fix rerun made pass *worse*
(0.77) and collapsed effort savings — the tested framing didn't fix it;
prompt-level fixes remain unproven, not disproven. **Real lesson: memory
anchors referent choice under ambiguity; selection/abstention is
correctness-relevant.** Presence-vs-content stays unisolated until a
non-actionable placebo arm exists.

**Seal status — the `held-*` 8-cell pool is a validation pool (#343).**
Its seal broke during development: the verify-loop edge was designed
from its own harm read, resolver fixes iterated on its l3 runs, coverage
was tuned when one of its cells starved. Everything measured on it is
**in-sample evidence** — the 86/88 verify-loop read, the −33.3% resolver
discovery read, and the partial DeepSeek replication (−14.4% on
completed cells) are validation-pool numbers, and 86/88-vs-78/88 is a
cross-invocation contrast, not paired. A genuinely sealed blind pool is
#224's remaining scope; sealed claims wait for it.

**The integrity wave landed (10-16/10-17).** The honest-evidence plan's
machinery is now on main: `turn_outcomes` label settlement at a fixed
window (hunk survival, agent-commit exclusion, settle anchored to
`created_at + 24h`, indexed settle scans, serialized folds), commit
attribution (pre-run HEAD stamp, `--all` ref coverage, per-session run
history, orphaned-range drain, non-git producers), referent-episode
backfill for pre-compaction vague turns, eval env allowlist +
manifest `env_passthrough`, `tail.referent_gate` decision records,
abstain cause split, `options.scope_gate`, the DeepSeek
reasoning-collapse fix (#381), write-path mutator field preservation
(#385), and the selector LTT arm (`crush eval calibrate`, #380 — ran
the labeled corpus to an honest null: 55 draws, under the ~78 floor,
no overlay emitted).

**The claim hierarchy:**

| Claim | Status |
|---|---|
| Memory exists and persists | Proven (tables, tail rendering) |
| One session's memory reduces measured discovery work | **Proven** — replicated powered runs on the failure-memory corpus (predates the held pool); *behavior change* shown, useful-information vs presence not yet decomposed |
| The *content* does the work (not token presence) | **Unisolated** — wrong-target memory is actionable content, not placebo; needs a non-actionable placebo arm |
| True-but-task-irrelevant memory can redirect work | **Observed** — pass 0.86 vs 1.00 via early termination on an ambiguous prompt (mask); mechanism = referent anchoring + narrow verify, not retrieval error |
| Estimator reports the true effect | **Corrected** — zero-count pairs (mostly treatment wins) dropped by log-ratio; headline understated; zero-safe estimator queued |
| Run-loop pathologies are detectable and repairable mid-run | **Validated in-sample** (#343) — verify-loop edge; sealed claim awaits #224 + same-invocation rerun |
| Small-model verdicts steer selection (L3) | **First evidence, in-sample** — −33.3% discovery on resolver-engaged cells, under the 35% bar |
| Turn-level acceptance labels | **Machinery landed** — `turn_outcomes` settlement at a fixed window; production volume pending |
| Selector admits certifiable | **Machinery landed, honest null** — LTT arm (#380): corpus under the 78-draw floor, no overlay emitted; volume path is `tail.decisions` telemetry |
| Smarter *every time* (compounding with depth) | **Inconclusive** — capacity fix landed (#222); depth ladder inconclusive with distractor harm at j=3 (10-08) |
| The corpus can measure capability, not just efficiency | **No** — control arms at the ceiling on every powered cell (expdisc 29/32, 21/21, 21/21; held 22/22–22/23); #227 closed without meeting its 40–70% band |
| Harness evolution yields held-out gains here | **Untested** — Gate G1 of the 10-17 roadmap decides measurability |
| Selectivity survives scale | Unproven — needs distractor ladder; abstention must be scoreable first |
| Generalizes (models, task classes, memory types) | **Partial directional read only** — DeepSeek replication on the validation pool: 121/121 pass, −14.4% discovery on completed cells; #348 fixed, sealed-pool rerun is what counts |

## The machine today — quantified

**Memory (durable, per-project, in `crush.db`):**

| Component | Detail |
|---|---|
| `command_memory` | `(cmd, cwd)`-keyed; kind, last exit, ok/fail counts, last session |
| `failure_memory` | Open failures; resolved on clean re-run; 30-day TTL + `Nd ago` age hints |
| `turn_outcomes` | Per-turn settled labels — hunk survival, agent-commit exclusion, divergence-beats-commit ordering |
| `referent_episodes` / `referent_memory` | Vague-prompt phrase→target episodes + promoted maps |
| `session_digests` | Per-session FTS5-indexed digests backing `<session_memory>` |
| `project_rates` | Beta-Binomial per-project rates with decay, lower-bound gating |
| `crush_commits` / `pending_attribution` / `session_command_runs` | Commit attribution + per-session run history for settlement evidence |
| Write path | `RecordRun` instrumented at **10 call sites** (bash, job_output, job_kill, verify gate, workspace, backend); `ComponentExits` recover real verdicts inside pipelines/lists/substitutions — composites can't launder |
| Read path | `<open_failures>` tail: freshest **5** rows, ≤3 file hints, cmd ≤200 runes, headline ≤140 runes, envelope-neutralized; task-bound by the deterministic selector (#207): prompt scope/referent → admit or abstain, per-candidate verdicts in `tail.decisions`. (Header still reads "the likely referents" — #201's reframe was closed unmerged after failing validation) |
| Gates | `ambiguity_clarification` bundle (vagueness pre-filter, first-write scope gate, loop-stop, burn-watch) with `scope_gate` as its own override (#384); `failure_memory_edges` reconcile edge on by default; `referent_memory`, `session_memory`, `memory_telemetry` opt-in |

**Evaluation machinery:**

| Component | Count / detail |
|---|---|
| Corpus trajectories | 62 (`eval/corpus/`) |
| Experiment manifests | 50 (`eval/experiments/`) |
| Preserved run records | ~3,600 artifacts across 34 result dirs, incl. per-run session DBs for forensics |
| Metrics per record | 32 fields; ~60 metric families in compare (calls, tokens, requests, steps, warm-start, edge firings) |
| Gate alarm kinds | 8 — catastrophic, coincident-collapse, coverage-starved, error-saturated, excluded-differential, expected-exclusion-missed, noop-flag, smoke |
| Power gate | CV-seeded sizing (`eval/noise.json`); refuses underpowered manifests |
| Calibration | `crush eval calibrate --param …` — per-class LTT walks emitting versioned `memory_params` overlays, honest-null on insufficient draws (#380) |

**Evidence produced so far:** 68 conclusive powered pairs (existence), 44 mask
records (confound isolation), 2 negative-control records (gate honesty) —
every claim above has a preserved invocation behind it.

## Index — dated records

`*-exp-*` files record what an experiment produced. `*-roadmap-*` files
record the plan that followed from it.

| Date | Type | File |
|---|---|---|
| 2026-09-21 | exp | [Notebook prior-turns-stub regime](2026-09-21-exp-notebook-prior-turns-stub.md) — first clean batch after harness debugging |
| 2026-09-22 | exp | [Small probes](2026-09-22-exp-probes.md) — prior-turns-long, summarize, edit-region |
| 2026-09-23 | exp | [Notebook checkpoint + stub at scale](2026-09-23-exp-notebook-checkpoint.md) — 96+120 clean records |
| 2026-09-24→27 | exp | [Notebook pressure-regime saga](2026-09-24-exp-notebook-pressure-regime.md) — ~290 records, mostly infra; alarms held |
| 2026-09-28→29 | exp | [Warm-baseline + noise characterization](2026-09-28-exp-warm-baseline.md) — seeding proven, CVs calibrated |
| 2026-09-30 | exp | [Failure-memory powered runs + negative control](2026-09-30-exp-failure-memory-powered.md) — the existence proof |
| 2026-10-01 | exp | [Existence-proof position + mask verdict](2026-10-01-exp-failure-memory-existence-proof.md) — current snapshot |
| 2026-10-01 | roadmap | [Roadmap after the mask verdict](2026-10-01-roadmap-after-mask.md) — log only; superseded 10-04 |
| 2026-10-04 | roadmap | [Fixed skeleton, learned variables](2026-10-04-roadmap-fixed-skeleton.md) — superseded 10-09: four-tier memory, per-project learned params, user-level priors |
| 2026-10-08 | exp | [Memory ladders](2026-10-08-exp-memory-ladders.md) — LOO ablations held; depth ladder inconclusive (distractor harm at j=3) |
| 2026-10-09 | roadmap | [Value and use](2026-10-09-roadmap-value-and-use.md) — superseded 10-15: phases mirror milestones; memory off-instrument-building onto real use |
| 2026-10-15 | exp | [Verify-loop + resolver chain](2026-10-15-exp-verify-loop-and-resolver.md) — edge validated in-sample (86/88 vs 78/88, cross-invocation), resolver made live, engagement-conditional coverage — pool relabeled validation per #343 |
| 2026-10-15 | roadmap | [Honest evidence before new capability](2026-10-15-roadmap-honest-evidence.md) — superseded 10-17: two reviews found label instability + in-sample claims; integrity work (#340–#348) precedes procedural Phase A |
| 2026-10-17 | roadmap | [Regularized harness evolution](2026-10-17-roadmap-regularized-evolution.md) — **plan of record**: learning loop parked (no data at single-user volume); RRSI-style evolution loop with Gate G1 (Terminal-Bench evolve, SWE-bench Verified OOD); memory channels become evolvable components; edit capture for user-level memory |
