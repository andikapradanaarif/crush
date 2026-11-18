# 2026-11-18 — G1′ re-issue decision record (#397)

This document is the pre-registered gate record the 11-16 re-issue
proposed. It replaces #397's 10-19 criteria table and the #489
amendments with one gate aligned to the 11-09 §8 G1′ (R1.1, R1.2,
R1.3, R1.8).

**Status: PROPOSED — takes effect on merge.** The D-decisions are
owner authority; merging this document ratifies §B as written. Until
then nothing below is a recorded decision and #503's run stays gated.

## A. Criteria table

Thresholds verbatim from the 11-16 re-issue §4. The status column is
evidence bookkeeping, not a criterion change.

| ID | Criterion | Threshold | Status 2026-11-18 |
|---|---|---|---|
| G1′.a | Infra completion on the k-rep batch | ≥ 90% of scheduled trials on **non-excluded** tasks conclusive; exclusions recorded with reasons (`harbor.exclude`); rate-limit deaths requeued, not counted | **pending #503** — smoke indicative (20/20 conclusive, 0 errors/retries/429s over ~2.7h) but the criterion keys on the k-rep batch |
| G1′.b | Base pass rate (conclusive-normalized, cell matrix) | 0.30 – 0.80 | **pending #503** — smoke 0.60 on 10 tasks is inside the band; the criterion reads the 29-task cell matrix |
| G1′.c | Model-based δ̂ at the **declared scoring protocol (n, k)** | ≤ 3pp; every scored candidate must use the same (n, k) | **pending #503** — smoke's δ̂ 0.120 was n≈21 sampling noise per the #489 correction; the k-rep σ_within/σ_between decomposition produces the protocol δ̂ |
| G1′.d | Token MDE at the scoring protocol | ≤ 20% (`c_min_expected` recorded) | **pending #503** — smoke: 274K tokens geomean/trial, c_min 0.111 measured; protocol MDE lands from k-rep σ |
| G1′.e | Split frozen | met | **met** — dev split vendored (`eval/tb21-tasks.json`, version 2.0); every manifest's `task_ids` carries the same 30-task set with `mteb-retrieve` recorded in `harbor.exclude` |
| G1′.f | Projected spend R1–R4 from measured $/trial | ≤ owner ceiling (D2) | **holds at D2's proposed values** — see §C |
| G1′.g | Estimators | `cluster` for population (sealed) claims; `stratified` with k ≥ 2 for conditional (dev) decisions | **met** — cluster bootstrap merged (#407); `estimand`/`resample` enforcement merged (#508): conditional OOD rules require stratified, population OOD rules require cluster |

Removed from the 10-19 table (per §4): **G1.b** (SWE-Pro — returns
through R1.7 only), **G1.e/f** (≤ 542 — replaced by G1′.c/d),
**G1.g** (merged into G1′.f).

### Verdict branches

- **Commit — full:** all criteria hold **and** pass MDE ≤ 5pp at an
  affordable protocol. R4 runs with capability and efficiency claims.
- **Commit — efficiency:** a, b, d, e, f, g hold but pass MDE > 5pp.
  R4 selects on the §6 efficiency path with a pass guardrail at the
  measurable margin; capability claims come only from screening wins
  replicated on the sealed split.
- **Fallback:** d or f fails → the 11-09 fallback (levers ship on
  paired dev reads plus replay; no transfer claims).

## B. Recorded decisions

| ID | Decision | Recorded value | Basis |
|---|---|---|---|
| D1 | Model pin and route | **`opencode-go/deepseek-v4.1-flash`**, large and small pinned identical | #506 passed every pre-registered row: 20/20 conclusive, pass 0.60 ∈ [0.20, 0.90] (and inside G1′.b), `temperature: 0` / `top_p: 1` verified on the wire on all 20 trials, zero 429s. Route confirmed for cached-read pricing ($0.01/1M — the dominant token class). Record: `2026-11-18-exp-deepseek-tb21-smoke.md` |
| D2 | Spend ceiling | **R1: $200**; **R4: $400 provisional** — re-set at R4 protocol lock when (n, k) is known | Measured $0.0275/trial (#506). R1 projection $113–$165 at 4,100–6,000 trials → $200 is ~1.7× projected, protocol-growth headroom. R4's ceiling stays provisional: don't size unmeasured scope |
| D3 | Claim posture for pass levers | **Efficiency-first** — L1/L3/L4 run as pre-declared *screening* reads (MDE ≈ 10pp; inconclusive ≠ null). A screening win upgrades to a 5pp read only if it shows | §4 recommendation; #508's MDE report showed 5pp pass claims need ~1,900 trials — outside D2 — while efficiency claims are affordable (−16% MDE / 480 trials) |
| D4 | "Active project" for R0.1 | **≥ 3 real-user sessions in the trailing 14 days on a post-#433 binary** — non-qualifying projects reported, not gating | §4 recommendation |
| D5 | Deadline for #482 | **2026-12-31** — if ≥ 8 non-accepted post-#481 rows haven't accrued, retire non-accepted classes per the 11-09 kill rule | ~6 weeks to accrue; learning proceeds on `accepted` + captured hunks (precision 8/8), R0.3 closes, R3 unblocks |

## C. Spend projection (G1′.f input)

At the smoke's measured **$0.0275/trial** (pinned route rates,
`cache_write` priced at input — OpenCode publishes no write rate):

| Run | Trials | Projected |
|---|---|---|
| #503 k-rep characterization (29 tasks × k=8 × 2 arms) | 464 | ≈ $13 |
| #510 N1a (29 × k=8 × 2 agents) | 464 | ≈ $13 |
| #405 best-of-k baseline (29 × k=3 × 3 arms) | 261 | ≈ $7 |
| #512 lever smokes (L1–L4, L6 — 5 reads, ~232–464 trials each) | ~1,400–2,300 | ≈ $40–$65 |
| R1 remainder (sizing margin, re-runs, sealed prep) | ~1,500–2,600 | ≈ $40–$72 |
| **R1 total** | **≈ 4,100–6,000** | **≈ $113–$165 vs $200 ceiling** |

R4 cannot be projected — its (n, k) is unknown until the scoring
protocol locks; D2's provisional $400 covers the expectation that R4
reads run smaller n at the same $/trial.

## D. Open inputs and what feeds them

| Criterion | Feeding run | Owner action |
|---|---|---|
| G1′.a–d | #503's 8-slice k-rep batch (~$13; runbook on main) | run slices → agent ingests + decomposes σ |
| G1′.f | D2 (above) | ratified by merging this document |
| W10 verdict | #503 analysis → this document's criteria table filled | owner + agent, after #503 |

Once #503 lands: δ̂ and token MDE fill a–d, the verdict lands under
§A's branches, #489 closes with it, and W11 (#512 lever reads) is
cleared in order L6 → L3 → L4 → L1 → L2.

## Refs

#397 (this record's home), #489 (amendments folded in), #503, #506
(`2026-11-18-exp-deepseek-tb21-smoke.md` — D1/D2 evidence), #407/#508
(G1′.g), #500/#501 (a, σ plug-in), #505 (cost), proposal:
`2026-11-16-status-and-g1-reissue.md` §4.
