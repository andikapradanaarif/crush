# Experiment ledger

Every recorded invocation in `eval/results/`, grouped by era. Columns:
invocation (truncated), records, arm outcomes, gate verdict, and what the run
established. Runs where all records errored are harness/debugging iterations —
provenance, not results; they are marked `infra`. Preserved session DBs live
under each result dir's `artifacts/`.

## Notebook-stack era — 2026-09-21 → 09-27

Prior-turn collapse, notebook stubs/checkpoints, and context-pressure regimes.
Many invocations are all-error — this era was simultaneously building the
harness and evaluating it.

| Date | Invocation | Experiment | n | Outcomes | Gate | Established |
|---|---|---|---|---|---|---|
| 09-21 | `071231Z-6dda` | notebook-prior-turns-stub | 120 | 120 err | — | infra |
| 09-21 | `074532Z-a8ad` | notebook-prior-turns-stub | 120 | 120 err | — | infra |
| 09-21 | `100005Z-4273` | notebook-prior-turns-stub | 78 | 55 pass / 23 inconc | — | first clean partial read |
| 09-21 | `180511Z-7fd3` | notebook-prior-turns-stub | 120 | 120 pass | — | regime runs green end-to-end |
| 09-22 | `022430Z-aec6` | prior-turns-long | 7 | 6 pass / 1 err | — | long-session probe |
| 09-22 | `095712Z-55c9` | prior-turns-long-summarize | 6 | 6 pass | — | summarize variant probe |
| 09-22 | `151435Z-8570` | edit-region | 10 | 10 pass | — | edit-region instrumentation |
| 09-23 | `085837Z-7c31` | notebook-checkpoint | 36 | 36 pass | — | checkpoint primary: `prompt_tokens_peak` |
| 09-23 | `181638Z-8171` | notebook-checkpoint | 4 | mixed | — | infra |
| 09-23 | `183916Z-74eb` | notebook-checkpoint | 60 | 60 pass | — | checkpoint regime at scale |
| 09-23 | `203622Z-531d` | notebook-prior-turns-stub | 120 | 120 pass | — | stub regime replicated |
| 09-24 | `150536Z`→`165404Z` | notebook-pressure-regime ×8 | ~110 | mostly err/fail | 7× FAIL, 1× inconc | pressure regime unstable — alarms fired as designed |
| 09-24 | `173315Z-61c6` | notebook-pressure-regime | 80 | 78 err | FAIL | infra — largest failed batch |
| 09-25 | `041605Z`→`173045Z` | notebook-pressure-regime ×4 | ~130 | ~59 err | 2× FAIL, 2× inconc | infra/debug iterations |
| 09-26 | `064101Z-e3c8` | notebook-pressure-regime | 30 | 9 pass / 20 err | inconc | last pressure attempt of the era |
| 09-26 | `190836Z-88c3` | notebook-pressure-regime-smoke | 10 | 8 pass | pass | smoke tier green |
| 09-27 | `043129Z-1045` | notebook-mask-regime-smoke | 9 | mixed | pass | mask-regime smoke |

## Warm-start era — 2026-09-28 → 09-29

Established the warm-session mechanics (`prior_sessions` seeds) that
failure-memory experiments depend on, plus noise characterization.

| Date | Invocation | Experiment | n | Outcomes | Gate | Established |
|---|---|---|---|---|---|---|
| 09-28 | `101923Z`, `184526Z`, `185528Z` | warm-baseline ×3 | 24 | 24 pass | 2× FAIL, 1× pass | warm-seed mechanics; early alarms caught bad configs |
| 09-29 | `022103Z`→`105846Z` | warm-baseline ×5 | 105 | 104 pass / 1 err | 3× pass, 2× inconc | warm baseline stable and characterized |
| 09-29 | `char-…-3d84` / `-2fea` | _characterize ×2 | 40 | 20 err / 20 pass | — | noise CV seeds → `eval/noise.json` |

## Failure-memory era — 2026-09-29 → 10-01

The "prove it learns" arc. Full analysis in
[2026-10-01 milestone](2026-10-01-failure-memory-existence-proof.md) and
issue #160.

| Date | Invocation | Experiment | n | Outcomes | Gate | Established |
|---|---|---|---|---|---|---|
| 09-29 | `152329Z-354a` | failure-memory | 12 | 12 pass | inconc | smoke — mechanism armed |
| 09-30 | `043411Z`…`043542Z` | failure-memory ×3 | 6 | 6 err | 3× inconc | infra |
| 09-30 | `043554Z-bc5a` | failure-memory | 52 | 52 pass | **pass** | **Powered run: discovery −41.1% (CI [−48.9, −33.7], p<0.001); 26/26 fired** |
| 09-30 | `055555Z-f0b6` | failure-memory | 84 | 84 pass | **pass** | **Top-up: discovery −27.6% (p=0.002), calls −27.2%, files_viewed −43%; 42/42 fired; MDE ≥35% not certified** |
| 09-30 | `101010Z`, `101028Z` | gate-negative-control ×2 | 4 | 4 err | inconc | infra |
| 09-30 | `101035Z-85ee` | gate-negative-control | 2 | 2 pass | **fail** | **Gate honesty: identical arms → `ALARM noop-flag` → FAIL (the desired detection)** |
| 10-01 | `081327Z-4965` | failure-memory-mask | 44 | 41 pass / **3 fail** | pass + guardrail violated | **Mask verdict: presence effect on effort (−14.5% calls, −57% files) + wrong-referent memory actively misdirects (pass 0.86)** |
| 10-01 | `125941Z-382f` | failure-memory-mask | in flight | — | — | Validation rerun on referent-caution framing (#201) |

## Totals

- **46 invocations**, **~1,340 run records**, 13 experiment dirs.
- Measured results (post-infra era): warm-baseline characterization,
  failure-memory powered ×2, negative control, mask, plus notebook-era regime
  reads.
- Every row above resolves to `eval/results/<experiment>/<trajectory>.jsonl`
  filtered by the invocation ID — the artifacts are the evidence.
