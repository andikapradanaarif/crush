# 2026-11-12 — exp: temp-0 determinism probe (beam-free)

Dated record for the Sunday G1′ amendment decision (#489). Supersedes
nothing; extends the pilot noise record in `eval/noise.json`.

## Question

Was the pilot's 58% task-level flip rate (σ_d 0.50–0.56, δ 23pp)
self-inflicted sampling noise — the arm config never sent a
temperature — or residual model entropy that survives a temp-0 pin?
The answer prices how much repeated-cell machinery #489's amendments
actually require.

## Method

- 11 tasks chosen for flip history across b1–b4 (bn-fit-modify,
  build-pov-ray, code-from-image, crack-7z-hash, dna-insert,
  feal-linear-cryptanalysis, kv-store-grpc, log-summary-date-ranges,
  pytorch-model-cli, schemelike-metacircular-eval, write-compressor).
- `-k 2` (two reps per task), `-n 2`, one job: `jobs/2026-11-12__20-57-00`.
- Arm `arm-beam-free.crush.json`: `models.large` and `models.small`
  both pinned `beam-free` @ `temperature:0, top_p:1`; no `--model`
  flag (the flag triggers the small-model catalog override — the
  pilot's silent contamination path).
- Binary `crush-linux-amd64` @ `93184cc33` — first field run carrying
  the orphan reaper, cumulative token cap, and pending-tolerant ingest
  (#487, #488).
- Multipliers: environment ×4, agent ×2, verifier ×4 (new this run).
- Fill-in re-run for quota-lost trials at `-n 1`:
  `jobs/2026-11-12__22-25-16` — all six died on the same quota error.

## Result matrix

| Task | Rep 1 | Rep 2 | Pair |
|---|---|---|---|
| log-summary-date-ranges | PASS | PASS | agree |
| write-compressor | PASS | PASS | agree |
| crack-7z-hash | FAIL | FAIL | agree |
| pytorch-model-cli | FAIL | PASS | **flip** |
| feal-linear-cryptanalysis | EXC (agent timeout 3600s) | FAIL | infra |
| bn-fit-modify | FAIL | EXC (rate limit) | lost |
| build-pov-ray | EXC (rate limit) | FAIL | lost |
| code-from-image | FAIL | EXC (rate limit) | lost |
| dna-insert | FAIL | EXC (rate limit) | lost |
| kv-store-grpc | EXC (rate limit) | FAIL | lost |
| schemelike-metacircular-eval | EXC (rate limit) | FAIL | lost |

- 4 conclusive pairs: **3 agree, 1 flip** (25% pair-flip rate).
- Conclusive pass rate 5/15 = 33% (within the pilot's flaky-set band).
- Telemetry confirms `model` and `model_small` both `opencode-go/beam-free`
  on every landed trial — the dual-model pin worked as designed.
- Zero CA/verifier/env exceptions — verifier×4 recovered the
  pytorch/query-optimize failure class from batch 2.

## System-vs-LLM decomposition (the decision input)

| Component | Evidence | Class |
|---|---|---|
| 3/4 pairs identical under temp-0 | temp pin collapsed most flip mass | system (was self-inflicted) |
| pytorch-model-cli FAIL→PASS across identical reps | residual entropy survives the pin | **LLM** |
| 6 trials died at first API call (`too many requests`) | free-tier quota, not model behavior | system (infra) |
| feal timeout at 3600s; agent spawned a 98%-CPU compiled attack program | task exceeds budget for this model | model-bound |

## New finding — free-tier daily quota

The rep-2 wave and all six fill-in trials (`-n 1`) died on
`ApiRateLimitError` at first call. After ~170 trials × ~500k tokens
today, the endpoint hard-caps. Concurrency doesn't matter; token
volume does. **The free tier appears to carry a daily quota — an
operational ceiling on characterization throughput (~150–170
trials/day observed), independent of cost.**

## Token/step data (telemetry ledgers)

- Median conclusive trial: ~40k input, ~290k cache-read, ~20 steps.
- Heavy tail: write-compressor rep-2 ran **127 steps / 8.6M
  cache-read** to PASS (rep-1 passed in 18 steps) — same task,
  same config, wildly different trajectory length. feal rep-2 burned
  823k cache-read + 36k output to FAIL.
- Rate-limit deaths show 0–2 steps — quota checkable from telemetry.

## Implications for #489 (Sunday)

1. **Temp-0 does not fully determinize beam-free.** Residual pair-flip
   rate ~1-in-4 on this sample (n=4 pairs — wide CI, but nonzero).
   Single-rep measurement remains unreliable; repeated cells are still
   needed — though k can plausibly be smaller than modeled.
2. **The 6 lost pairs cap the confidence of this read.** Options:
   retry after quota reset (likely daily window), or re-run as a
   muse-spark-1.3 determinism probe (~$1) — muse-spark's own flip
   rate is the missing comparison either way.
3. **Quota changes the sizing math.** At ~150–170 free trials/day,
   a k=8 × 30-task × 2-arm design (480 trials) is ~3 days of
   continuous free burn — or ~$25 on muse-spark ($0.10/$0.20 per M,
   ~$0.05–0.10/trial; whole-G1-program est. ≤$30).
4. **feal-class tasks are model-bound**, not harness-bound — budget
   multipliers recovered everything recoverable; what remains is the
   model's own capability/time frontier.
5. **δ redefinition (solved/conclusive) is validated by this run:**
   rate-limit deaths would otherwise read as solve variance.

## Raw artifacts

- `jobs/2026-11-12__20-57-00` — main probe (22 trials)
- `jobs/2026-11-12__22-25-16` — quota fill-in (6 trials, all EXC)
- Per-trial telemetry: `<trial>/agent/crush-data/telemetry.json`
- Build: `crush-linux-amd64` @ `93184cc33`
