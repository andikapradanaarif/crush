# 2026-09-28 → 09-29 — Warm-baseline + noise characterization

The era that made failure-memory possible: `prior_sessions` seeding
mechanics proven, then baseline characterization.

| Invocation | Experiment | n | Outcomes | Gate |
|---|---|---|---|---|
| `101923Z-6344` (09-28) | warm-baseline | 8 | 8 pass | FAIL — bad config caught |
| `184526Z-32a3` | warm-baseline | 8 | 8 pass | FAIL |
| `185528Z-7a98` | warm-baseline | 8 | 8 pass | pass |
| `022103Z-bbef` (09-29) | warm-baseline | 8 | 8 pass | pass |
| `022502Z-9cf9` | warm-baseline | 8 | 8 pass | inconc |
| `022945Z-a2e8` | warm-baseline | 24 | 24 pass | inconc |
| `024415Z-9fe6` | warm-baseline | 25 | 24 pass / 1 err | pass |
| `105846Z-dda2` | warm-baseline | 24 | 24 pass | pass |
| `char-…-3d84` (09-29) | _characterize | 20 | 20 err | — |
| `char-…-2fea` | _characterize | 20 | 20 pass | — |

~145 records across 4 warm trajectories. The two early FAILs were config
bugs the gate caught; the characterize run seeded the CVs in
`eval/noise.json` that the power gate now requires.

**Established:** warm sessions seed reliably, control-arm behavior is stable
enough to measure against, and noise floors are calibrated — the three
preconditions for the powered failure-memory run.

Artifacts: `eval/results/warm-baseline/`, `eval/results/_characterize/`.
