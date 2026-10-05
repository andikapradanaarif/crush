# 2026-10-06 — Mask-reject powered rerun: L1 live, verdict FAIL

First powered reject read with the L1 identifier layer live (#242) and
the reworked stalefile seed (#243). Invocation `20261005T141152Z-7491`,
9 cells × 2 arms, 454 records (189 control / 265 treatment).

Artifacts: `eval/results/failure-memory-mask-reject-powered/`.

## Gate verdict: FAIL

- coverage-starved treatment arms: `mask-bb-ambig-stale`,
  `mask-fix-failing-test`, `mask-ft-ambig-placebo`, `mask-ft-ambig-stale`,
  `mask-ft-explicit-stalefile`.
- excluded-differential on 6 cells → catastrophic coverage (0/0 stable
  trajectories eligible).
- diffuse p = 0.0005.
- Guardrails: treatment pass 47/54 vs control 99/99 on the
  metric-eligible subset — the 13% drop alone breaches
  `max_pass_drop` (10%).
- Prompt tokens +69.2% (treatment 136.6K/run vs control 80.8K,
  conclusive runs).

## Two distinct starvation mechanisms

| Mechanism | Cells | Evidence |
|---|---|---|
| `seed_check` rejects | all 3 stale-file-seed cells | bb-ambig-stale 22/22, ft-ambig-stale 24/24, ft-explicit-stalefile 14/22 of inconclusives are seed rejects |
| coverage/gate inconclusive | the rest | non-seed `check_detail` (e.g. `max_tail.decisions.admitted` coverage) |

## Stalefile rework outcome — still unseedable under a powered driver

0/22 treatment runs reached measurement. Seed diagnostics:

- `row_older_than_touch: false` — the seeded run refreshed `last_seen`
  past the touch mtime: the powered agent opened the implicated file.
  The bare-`touch` rework removed the *incentive* to look, not the
  *behavior*; a curious seed agent still invalidates the premise.
  Exactly the observability hole #244 tracks — the gate sees state,
  not whether the agent viewed the file.
- `open_stale_rows: 0` on the other rejects — no stale row open at all.

Design conclusion: a stale-file premise cannot survive a powered seed
agent that verifies its workspace. Either the fixture needs a shape
the agent cannot invalidate (e.g. seed the stale row via fixture DB
writes rather than a real on-disk file the agent can inspect), or
#244's `files_viewed` scoring lands first so the peek can be detected
rather than prevented.

## What changed vs the 10-04 reject read

10-04 (pre-L1): gate PASS, 8/9 cells powered, 176 conclusive runs,
pass 1.00 vs 1.00, 88/88 seeded candidates rejected with named
reasons. This rerun: FAIL with coverage collapse plus a real pass drop
on the eligible subset. Whether L1 selection itself degraded
reject-side behavior is unreadable at this starvation level — needs a
rerun once the stale-file seed class is fixed.

## Established

- The mask corpus does not currently power under the powered driver:
  stale-file seeds are unseedable (premise invalidated by the seed
  agent itself) and non-seed cells also starve on the coverage gate.
- The 10-04 reject-verdict evidence still stands for the pre-L1
  selector; this run is evidence that the corpus/harness pair needs
  work before L1's reject behavior can be certified.
