# Milestones

Dated snapshots of where this fork stands against its goal: an agent harness
that helps the user create a good product — one that gets smarter every time
it is used, and can *prove* it.

Each dated file captures one position: what was proven, what shipped, what is
parked, and the plan from that point. This README tracks only the current
position and the index; the history lives in the dated files.

## Current position

**Scope: "prove it learns."** Crush already wrote substantial information to
`crush.db` but never effectively read it back across sessions — memory was a
write-only ledger. This milestone series exists to turn "it learns" from a
claim into a measurement.

**Failure memory — existence proof: DONE and armored.** A durable failure
record written in session N provably changes behavior in session N+1, measured
across two powered runs (−27% total calls, ~25–30% discovery savings,
replicated p<0.01 ×2, 68/68 treatment runs fired, zero pass harm). The
instrumentation cannot be fooled (composite-command verdict recovery), the
data ages out honestly (30-day TTL), and the evaluation gate provably detects
a guaranteed-null experiment.

**Mask arm resolved (mixed verdict):** wrong-referent memory produced the
same effort reduction as correct memory (−14.5% calls, −57% files viewed) —
so part of the powered gain is a *presence effect*, not content. But wrong
memory also dropped pass to 0.86 vs 1.00 by redirecting work onto the decoy:
content correctness is what makes the confidence warranted. Retrieval
precision is now correctness machinery, not an optimization.

**Framing fix failed validation (`382f`):** cautionary header text did not
recover pass (0.77, worse) and collapsed the effort benefit — agents verified
referents first, spending the calls memory saves. Prompt text cannot fix a
precision problem; #166 relevance-ranked retrieval is now *measured
necessary*. PR #201 closed unmerged.

**The claim hierarchy:**

| Claim | Status |
|---|---|
| Memory exists and persists | Proven (tables, tail rendering) |
| One session's memory helps the next | **Proven** — replicated powered runs |
| The *content* does the work (not token presence) | **Partially falsified** — effort gain is substantially presence; content determines whether it's net-positive or harmful |
| Wrong memory is harmful | **Proven** — pass 0.86 vs 1.00, decoy-chasing signature |
| Smarter *every time* (compounding with depth) | Unproven — needs the depth ladder |
| Selectivity survives scale | Unproven — needs noise-seeded ledger |
| Generalizes (models, task classes, memory types) | Unproven — single model so far |

## The machine today — quantified

**Memory (durable, per-project, in `crush.db`):**

| Component | Detail |
|---|---|
| `command_memory` | `(cmd, cwd)`-keyed; kind, last exit, ok/fail counts, last session |
| `failure_memory` | Open failures; resolved on clean re-run; 30-day TTL + `Nd ago` age hints |
| Write path | `RecordRun` instrumented at **7 call sites** (bash, job_output, job_kill, verify gate, workspace, backend); `ComponentExits` recover real verdicts inside pipelines/lists/substitutions — composites can't launder |
| Read path | `<open_failures>` tail: freshest **5** rows, ≤3 file hints, cmd ≤200 runes, headline ≤140 runes, envelope-neutralized; referent-gated by failure-noun detection; framed as *historical context* (#201) |

**Evaluation machinery:**

| Component | Count / detail |
|---|---|
| Corpus trajectories | 23 (`eval/corpus/`) |
| Experiment manifests | 18 (`eval/experiments/`) |
| Preserved run records | ~1,300+ across 12 result dirs, incl. per-run session DBs for forensics |
| Metrics per record | 32 fields; ~60 metric families in compare (calls, tokens, requests, steps, warm-start, edge firings) |
| Gate alarm kinds | 8 — catastrophic, coincident-collapse, coverage-starved, error-saturated, excluded-differential, expected-exclusion-missed, noop-flag, smoke |
| Power gate | CV-seeded sizing (`eval/noise.json`); refuses underpowered manifests |

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
| 2026-10-01 | roadmap | [Roadmap after the mask verdict](2026-10-01-roadmap-after-mask.md) — the five-phase plan and its decision points |
