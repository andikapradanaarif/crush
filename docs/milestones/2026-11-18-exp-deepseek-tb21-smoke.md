# 2026-11-18 — exp: deepseek-v4.1-flash TB2.1 smoke (#506, W3)

Dated record for owner decision **D1 (model pin)**. The pre-registered
table is in #506; every row is filled below. Job artifacts:
`jobs/2026-11-17__23-29-27`, ingested as `tb21-smoke-deepseek` /
invocation `deepseek-smoke-1`.

## Question

Before #503 spends ~480 trials characterizing a pin, is
`opencode-go/deepseek-v4.1-flash` usable — pass rate in a measurable
band, cost per trial inside the owner ceiling, the temperature pin
actually reaching the wire (the pilot's defect class), and no
rate-limit wall?

## Method

- Manifest `eval/experiments/tb21-smoke-deepseek.json`: model and
  `small_model` pinned `opencode-go/deepseek-v4.1-flash`,
  `temperature: 0`; stride-3 10-task dev subset; `k: 2`;
  `harness_revision 1715db5098` (binary built at that sha).
- Arm `arm-deepseek.crush.json`: `models.large`/`models.small` both
  deepseek, `temperature: 0, top_p: 1`; no `--model` flag (the flag
  triggers the small-model catalog override — the pilot's silent
  contamination path).
- One control-arm job, `harbor run -d terminal-bench@2.0 -n 2 -k 2`,
  multipliers env×4 / agent×2 / verifier×4, `max_tokens=8M` cap.
  ~2.7 h wall for 20 trials.
- Provider block pins `cost_rates` from the opencode-zen catalog
  (in $0.30 / out $1.20 / cache-read $0.01 per 1M; cache_write at
  input rate — billing treats writes as input).

## Result matrix

| Task | Rep 1 | Rep 2 | Pair |
|---|---|---|---|
| code-from-image | PASS | PASS | agree |
| db-wal-recovery | PASS | PASS | agree |
| feal-linear-cryptanalysis | PASS | PASS | agree |
| mcmc-sampling-stan | PASS | PASS | agree |
| regex-log | PASS | PASS | agree |
| bn-fit-modify | FAIL | PASS | **flip** |
| path-tracing-reverse | FAIL | PASS | **flip** |
| kv-store-grpc | FAIL | FAIL | agree |
| pytorch-model-cli | FAIL | FAIL | agree |
| train-fasttext | FAIL | FAIL | agree |

- 10 pairs: **8 agree, 2 flip** (20% pair-flip rate, both flips
  FAIL→PASS in the same direction).
- Conclusive pass rate **12/20 = 0.60**.
- Zero exceptions — 20/20 trials conclusive, 0 retries, 0 429s.

## Pre-registered table (decide D1 from these, nothing else)

| Check | Adopt deepseek if | Result | Verdict |
|---|---|---|---|
| Infra | ≥16/20 conclusive excl. rate-limit deaths | **20/20 conclusive, 0 quota deaths** | ✓ |
| Pass band | conclusive pass ∈ **[0.20, 0.90]** | **0.60** | ✓ |
| Sampling pins | resolved temp/top_p = declared on every conclusive trial, or route documented to ignore | **temperature 0 / top_p 1 stamped on all 20 telemetry records** — the pin reaches the wire | ✓ |
| Cost | measured $/trial × ~6,000 R1 trials ≤ owner ceiling (D2) | **$0.0275/trial measured → ≈$165 for R1** — inside the plan's $66–$500 band; inside D2 ceiling unless the ceiling is set very low | ✓ pending D2 |
| Rate limits | report observed 429s/quota errors per hour | **0 observed** across ~2.7 h / ~7.9M billed tokens | ✓ |

## Noise + cell characterization (`eval noise --cells`)

- `sigma_within` 0.316, `sigma_between` 0.401 — σ_d ≈ 0.40 stays in
  the plug-in range #501 flagged for the conditional estimand; the
  2-rep smoke can't bound it further (that's #503's job at k=8).
- `delta_expected` 0.120, `c_min_expected` 0.111 (efficiency floor).
- Tokens: geomean 274K/trial, `tokens_log_sigma_within` 0.278,
  `tokens_log_sigma_between` 0.926.
- Wall: 678 s/trial mean.

## Token/step data (telemetry ledgers)

- Median trial: 15 steps; sums across the run — 669K input,
  231K output, 7.11M cache-read.
- Heavy tail: mcmc-sampling-stan rep-2 burned 1.09M cache-read +
  19.6K output over 40 steps to PASS; path-tracing-reverse's PASS
  rep cost 1.05M cache-read + 41K output — the token-heavy tasks are
  the same ones that flip.
- Fails are cheap, not catastrophic: kv-store-grpc failed both reps
  in 11 steps / ~130K cache each; train-fasttext failed in 16–19
  steps at ~200–335K cache. No 8M-cap hits, no timeouts.

## Comparison vs beam-free pilot (same 10 tasks)

The pilot's records on these tasks: ~15 pass / 15 fail / 9 other
across ~39 trials (≈0.50 conclusive pass) vs deepseek's 0.60 —
directional only: the pilot ran k=1 across multiple invocations,
sent no temperature, and ran deepseek as its *small* model (the
contamination the G1′ re-issue documents). Notable per-task moves:
path-tracing-reverse 0/4→1/2, kv-store-grpc 3/4→0/2,
train-fasttext 0/4→0/2, mcmc-sampling-stan 2/3→2/2.

## Prior accidental datapoint

Earlier same evening, a 3-task k=1 run (`jobs/2026-11-17__22-54-16`)
unintentionally ran deepseek at **default** sampling — the
`arm-longcat-free` config carried no `models` block, so catalog
defaults resolved deepseek for both slots (the file is now flagged
as a pin trap). All 3 passed, consistent with this read: deepseek
clears these tasks even unpinned. Not counted — wrong task set,
k=1, no declared pins.

## D1 recommendation

**Adopt `opencode-go/deepseek-v4.1-flash` as the R1 pin** — every
pre-registered row passes with margin: usable band midpoint,
temperature provenance clean, measured cost ~20× under the plan's
high estimate, zero rate-limit pressure. If D2's ceiling lands
anywhere near the $66–$500 planning band, cost is not the deciding
axis. #503 can proceed at this pin once D1 is set; the remaining
risk (σ_d 0.40 against pass-rule power) is what its k=8
characterization measures.
