# 2026-09-30 — Failure-memory powered runs + gate negative control

The day "prove it learns" stopped being a question. All powered numbers are
on issue #160; artifacts in `eval/results/failure-memory/` and
`gate-negative-control/`.

## Smoke + infra

| Invocation | n | Note |
|---|---|---|
| `152329Z-354a` (09-29) | 12 pass | smoke — mechanism armed, too small to read |
| `043411Z`, `043439Z`, `043542Z` | 6 err | infra — provider errors, discarded |

## Powered run — `043554Z-bc5a` (52 records, 26 conclusive pairs)

| Metric | Δ (treatment − control) | 95% CI | p |
|---|---|---|---|
| `discovery_calls_before_write` | **−41.1%** | [−48.9, −33.7] | <0.001 |
| `calls` | −26.8% | — | — |

- 26/26 treatment runs fired `<open_failures>`; pass 1.00 vs 1.00.
- Gate: **pass**.
- Pre-registered ≥35% MDE: **not certified** — CI edge at −33.7%.

## Top-up — `055555Z-f0b6` (84 records, 42 conclusive pairs)

| Metric | Δ | 95% CI | p |
|---|---|---|---|
| `discovery_calls_before_write` | **−27.6%** | [−40.6, −14.8] | 0.002 |
| `calls` | −27.2% | — | — |
| `files_viewed` | −43.0% | — | — |
| `first_write_index` | −43.8% | — | — |
| `tokens.output` | −22.3% | — | — |

- 42/42 fired; pass 1.00 vs 1.00; gate **pass**.
- Replication across independent invocations: both significant at p<0.01.
- Honest read: true effect ~25–30%, below the registered MDE — replication
  yes, magnitude smaller than the powered estimate.

## Negative control — `101035Z-85ee` (#154)

| Invocation | n | Gate |
|---|---|---|
| `101010Z`, `101028Z` | 4 err | infra |
| `101035Z-85ee` | 2 pass | **FAIL — `ALARM noop-flag`, exit 1** |

Identical control/treatment option maps → the gate detects a guaranteed-null
experiment and fails closed. The verdict machine provably can say no.

## Established

Existence proof complete: durable failure memory written in session N
measurably reduces discovery work in session N+1, replicated, with provable
firing and zero pass harm — and the instrument measuring it is proven honest.
