# 2026-11-21 — exp: tb21-krep-deepseek A/A calibration, final (#503, G1′.a)

Closing record for the k=8 repeated-cell characterization. Issue:
#503. Job artifacts: `jobs-krep/s{1..8}-{control,treatment}/`,
ingested as `tb21-krep-deepseek` / invocation `krep`.
Full 464-row trial accounting:
https://gist.github.com/andikapradanaarif/3cc5c722a0f6de9f7dc511b5cd96fd8b

## What this run was (and was not)

Deliberate **A/A** design: both arms ran the identical pinned binary
(`baa8bdf3af`), `arm-deepseek.crush.json`, `opencode-go/deepseek-v4.1-flash`
in both model slots, temperature 0, 8M token cap — verified
byte-identical arm configs across all 464 trials. The experiment
measures how much pass rate and tokens move when the same harness
runs again: a repeatability calibration, not a lever read. The
reported arm differences (−2.96 pp pass, +15% input tokens) are
noise-floor observations. No feature changed between arms; nothing
here supports accepting or dropping a lever.

## What happened along the way

- **Ingest defect found and fixed** (#535): `harborOutcome` dropped
  `reward` whenever `exception_info` existed. 63 trials carried
  exceptions; 56 also had rewards (3 at 1.0). The reward-based
  denominator (457/464) and the ingest denominator (401/464)
  disagreed by exactly those 56 records. Ingest now retains reward,
  exception, and executed as separate fields with explicit
  `score_eligible`/`requeue_eligible` stamps.
- **Connectivity contamination**: 26 trials died on dial-tcp/TLS/DNS
  signatures (15 control, 11 treatment — not one-sided as first
  reported), plus 1 verifier timeout. Owner approved a ~$0.81
  requeue; all 27 cells were re-run and re-ingested under their
  original `--run-index`, pairing intact. 26/27 returned conclusive;
  the 27th re-died OOM (exit 137) — a genuine outcome, kept as error.
- **Timeout policy ratified**: the 28 `AgentTimeoutError`s stay
  errors for this read (no post-hoc rescoring; 3 carry reward-1 as
  retained evidence). Predeclaration for future batches:
  budget-exhaustion → error not auto-requeued; infra interruption →
  error + requeue replaces the cell; reward+exception → exception
  wins for scoring, reward retained.

## Final numbers

- **G1′.a: PASS — 427/464 conclusive = 92.0%** (threshold 418 = 90%,
  margin +9). Posted on #397.
- Reward-present denominator: 458/464 (98.7%).
- Pass counts: control 131, treatment 125 — a −2.6 pp A/A gap,
  inside the expected spread.
- Exceptions remaining: 28 AgentTimeout + 9 genuine crashes/OOM;
  connectivity signature count now zero.

## Noise characterization (`--cells krep`, 29 tasks × up to 8 reps)

| Measure | Control | Treatment |
|---|---:|---:|
| Task-equal pass rate | 59.8% | 56.1% |
| σ_within / σ_between | 0.343 / 0.357 | 0.366 / 0.343 |
| δ_expected | 3.81 pp | 4.06 pp |
| c_min_expected | 6.52% | 6.37% |
| Token log-σ within / between | 0.568 / 1.002 | 0.556 / 1.022 |
| Detectable token reduction | ~13.5% | ~13.5% |

Post-requeue estimates moved <0.1 pp from the provisional values —
the contamination was a denominator problem, not a dispersion
problem. Both arms inside the 30–80% pass band; δ slightly above the
3 pp full-gate target.

## What this calibration feeds

- **#510 (N1a anchor)**: `min_delta` pinned −0.08 = 2× δ_expected —
  the paired-run spread, not the −2.96 pp signed difference (PR
  #536).
- **#512 (L6 first lever read)**: token MDE ~13.5% vs expected
  −15~−40%; `min_improvement` pinned 0.14, guardrail −0.10 pass
  (PR #536).
- **#405 (matched-budget best-of-k)**: offline operating point —
  last-attempt pass@1 ≈ 59.8%, pass@3 oracle ceiling ≈ 76%, k=3
  budget ≈ 2.12M weighted tokens/task.
- **R1 posture**: repeated runs resolve practically interesting
  token savings at this budget; ~5 pp pass claims still need
  ~1,900 trials (screening reads or nothing) — the ratified
  efficiency-first order stands.
