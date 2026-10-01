# 2026-09-23 — Notebook checkpoint + stub regime at scale

| Invocation | Experiment | n | Outcomes | Note |
|---|---|---|---|---|
| `085837Z-7c31` | notebook-checkpoint | 36 | 36 pass | primary: `prompt_tokens_peak` |
| `181638Z-8171` | notebook-checkpoint | 4 | mixed (timeout/err) | infra |
| `183916Z-74eb` | notebook-checkpoint | 60 | 60 pass | checkpoint regime at scale |
| `203622Z-531d` | notebook-prior-turns-stub | 120 | 120 pass | stub regime replicated |

**Established:** the checkpoint and prior-turns-stub regimes both produce
clean powered-scale batches (96 + 120 conclusive records in a day).

Artifacts: `eval/results/notebook-checkpoint/`,
`eval/results/notebook-prior-turns-stub/`.
