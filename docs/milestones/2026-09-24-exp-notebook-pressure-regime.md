# 2026-09-24 → 09-27 — Notebook pressure-regime saga

The context-pressure/compaction regime — the hardest experiment of the
notebook era, and the one where the gate's alarms earned their keep.

| Date | Invocation | n | Outcomes | Gate |
|---|---|---|---|---|
| 09-24 | `150536Z`, `151401Z`, `151858Z` | 6 | all error | 3× FAIL |
| 09-24 | `152244Z-552c` | 20 | 20 pass | FAIL |
| 09-24 | `170149Z-9c97` | 6 | 1 pass / 5 err | FAIL |
| 09-24 | `173315Z-61c6` | 80 | 78 err | FAIL |
| 09-25 | `041605Z-224a` | 38 | 36 err | FAIL |
| 09-25 | `055901Z-6838` | 30 | 8 pass / 20 err / 2 timeout | FAIL |
| 09-25 | `115506Z-84b8` | 32 | 7 pass / 22 err / 2 fail / 1 timeout | FAIL |
| 09-25 | `165404Z-d326` | 2 | 2 err | inconc |
| 09-25 | `173045Z-ea2e` | 30 | 8 pass / 20 err / 2 timeout | inconc |
| 09-26 | `064101Z-e3c8` | 30 | 9 pass / 20 err / 1 timeout | inconc |
| 09-26 | `190836Z-88c3` (smoke) | 10 | 8 pass | pass |
| 09-27 | `043129Z-1045` (mask-regime smoke) | 9 | 4 pass / 4 timeout / 1 err | pass |

~290 records total, majority error — this era was debugging the pressure
pipeline itself. Primary metric: `weighted_cost`. The gate's `error-saturated`
/ `coverage-starved` alarms fired on every broken batch instead of letting
the noise pass as results — the mechanism later proven deliberately by
#194's negative control.

**Established:** the pressure regime never produced a clean powered read;
the honest record is "unstable, alarms held." The two closing smoke runs
went green.

Artifacts: `eval/results/notebook-pressure-regime/`,
`-smoke/`, `notebook-mask-regime-smoke/`.
