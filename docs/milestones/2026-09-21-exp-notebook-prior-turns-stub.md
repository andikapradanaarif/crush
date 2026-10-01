# 2026-09-21 — Notebook prior-turns-stub regime

First large batch of the notebook-stack era. Four invocations on
`notebook-prior-turns-stub` (6 trajectories, control vs treatment):

| Invocation | n | Outcomes | Note |
|---|---|---|---|
| `071231Z-6dda` | 120 | 120 error | infra — harness still stabilizing |
| `074532Z-a8ad` | 120 | 120 error | infra |
| `100005Z-4273` | 78 | 55 pass / 23 inconclusive | first clean partial read |
| `180511Z-7fd3` | 120 | 120 pass | regime green end-to-end |

**Established:** the prior-turns-stub regime runs cleanly at scale once the
harness stabilized; the two all-error invocations are debugging iterations,
not results.

Artifacts: `eval/results/notebook-prior-turns-stub/` (jsonl + `artifacts/`
session DBs).
