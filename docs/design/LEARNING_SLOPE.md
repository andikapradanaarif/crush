# Learning-Slope Protocol — N3 (Temporal Held-Out, Mean Not Best)

> **Status:** Protocol for #454 (roadmap 2026-11-09, R3.6 / N3).
> Pre-registered before the first round runs. Changing a threshold,
> split rule, or estimand after seeing data requires a dated
> amendment. Depends on #399 for replay-task supply and on the R0
> corpus for learn material.

## Claim and honest calibration

The question the memory loop exists for: *does held-out performance
rise as the store accumulates items learned from real use?*

The trap this protocol is built against is PILOT's shape: its +14.6
was the **best of 20 in-sample runs** — a selection over noise, not a
learning effect. Every number this protocol reports is a **mean over
held-out rounds**; no best-of-round, no in-sample scoring, no
cherry-picked task subset.

What the literature gives the expectation side: Cortex's measured
slope family is *small-positive when corrections recur, ~0 otherwise*.
The honest prior is therefore "near zero, possibly positive" — the
protocol must be able to *resolve* a small positive slope, and must
be able to kill the store when the slope is ≤ 0. Both directions are
designed, not incidental.

## Estimand

Primary and secondary, symmetric:

- **Pass slope**: per-task regression slope of the 0/1 held-out
  outcome on round index, pooled across the frozen test set.
- **Tokens slope**: per-task regression slope of
  `log(tokens_to_done)` on round index, pooled identically.

Verdict (pre-registered, from the roadmap §4 row N3):

- **Positive**: 95% one-sided CI lower bound > 0 on either slope,
  with the other non-inferior — pass within −δ_expected, tokens
  within +c_min_expected, both from `eval/noise.json`'s `cells`
  block (measured, never retuned).
- **Kill**: point estimate ≤ 0 on pass *and* tokens after ≥ 3
  rounds → prune the store, freeze additions.
- Anything else reports as **inconclusive**, never null.

## The temporal split

One freeze date **D** is chosen at protocol activation and recorded
in the wave manifest (hash + ISO timestamp). Everything partitions
on D:

- **Learn corpus** — settled `turn_outcomes` and replay-harvestable
  sessions with `settled_at < D`. The R3.2/R3.3 item miners may
  consume only this side.
- **Test corpus** — a frozen list of replay tasks harvested from
  sessions with `settled_at ≥ D`, plus the manifest SHA-256 of the
  task list and each task's snapshot pin. The list freezes before
  round 1 and does not grow.

A test task's artifacts are **never** learn material: a task in the
frozen set is excluded from mining even if its settled turn would
qualify on other grounds. The exclusion is enforced by task id in
the harvest filter, not by convention.

## Round protocol

Rounds are the temporal unit (R3.2's "one item at a time" batches
*inside* a round — the round is when the accumulated items get
measured, not a per-item read):

1. **Learn window closes.** All items mined from the learn corpus
   since the last round are applied under the **G1 bound** (≤ 8 KB
   rendered, zero new always-on bytes) — an item that can't fit
   waits for the next round, it doesn't force the measurement off
   budget.
2. **Run the frozen test set once.** Every task, one trial each,
   pinned model + locked binary (see below), same manifest hash.
   k = 1 per task per round: the slope estimator uses rounds as
   replicates, so reps inside a round would shrink the wrong
   variance term at equal spend.
3. **Record, don't read.** Round results are appended to the wave
   record but the slope is not computed until the declared final
   round ends — the roadmap's "test side read only at round ends"
   means no mid-course peeking that could steer the next learn
   window.
4. Repeat for **n ≥ 3** declared rounds (n declared at freeze;
   the power table below prices the choice).

Cadence is material-boxed, not clock-boxed: a round ends when the
learn window accrued a declared minimum of new applied items (≥ 5
suggested). An empty round produces no measurement and doesn't
count toward n.

## The pin

**The binary and model are locked for the whole series** —
`harness_revision` + model pin frozen in the wave manifest. This is
the mirror of the lever reads: there, the store is fixed and the
code varies; here the code is fixed and the **store is the only
factor that varies**. A binary bump mid-series confounds slope with
version drift and voids the read — if a bump is unavoidable, the
series restarts under a new D (the old rounds stay recorded for
audit, they don't mix into the new slope).

Model pin follows the standing D1 pin
(`opencode-go/deepseek-v4.1-flash`, temperature 0) unless the owner
amends the series pin at freeze.

## Statistics

- **Slope**: OLS of the per-task round outcomes on round index
  `x ∈ {0,…,n−1}`, fit per task then pooled — the per-task fits
  keep task difficulty out of the slope (each task is its own
  baseline).
- **CI**: task-level (cluster) bootstrap over the pooled slopes,
  B = 10,000, matching the corpus-wide convention (#407). One-sided
  95% lower bound decides positive; the non-inferiority check on
  the sibling metric uses the recorded δ_expected / c_min_expected.
- **Resolution bound** (from `noise.json` cells:
  σ_within = 0.343 pass / 0.568 log-tokens):

  | rounds n | tasks N | pass slope se | detectable slope (95%, 1-sided) |
  |---|---|---|---|
  | 3 | 30 | 4.4pp/round | 7.3pp/round |
  | 3 | 48 | 3.5pp/round | 5.8pp/round |
  | 4 | 30 | 2.8pp/round | 4.6pp/round |
  | 4 | 48 | 2.2pp/round | 3.6pp/round |
  | 5 | 30 | 2.0pp/round | 3.3pp/round |

  Tokens resolve tighter (log σ): n = 4, N = 30 detects a 7.6%/
  round token decline at equal pass.

  **Consequence**: n = 3 rounds cannot resolve the honest-prior
  effect size (small-positive). The declared series should target
  **n = 4, N ≥ 48** where the harvest allows; n = 3 is admissible
  only as an early-kill check (slope clearly ≤ 0) — a 3-round null
  reports underpowered-inconclusive, not "no learning".

## Anti-gaming clauses (all enforced by the harness, not convention)

- The frozen test list + SHA-256 is in the wave manifest; a rerun
  against a different list is a different wave.
- Round results are append-only; the slope computation runs once,
  at the declared end.
- A task dropped mid-series (snapshot rot, infra) stays in the
  denominator with its last verdict noted — silently shrinking N
  inflates the CI bound.
- Learn-window material is filtered by `settled_at < D` at the
  miner, and the frozen test ids are excluded by id — two
  independent checks, since a timestamp-only rule would let a
  late-settled test turn leak into the corpus.
- Every round's manifest records which item ids were applied —
  the slope is auditable to "which rules were live" per round.

## Feasibility gates before round 1

1. #399 harvest supplies ≥ N replay tasks post-D (N declared at
   freeze, ≥ 30 required, ≥ 48 target).
2. Learn corpus has ≥ 20 settled real-user rows pre-D (else the
   miners have nothing to work with — measured 17 accepted rows at
   2026-11-21; accrues with use).
3. G1 headroom: the rendered-rules budget has ≥ 4 KB free for
   accumulated items.
4. Binary + model pin recorded; spend projection for
   `n × N` trials approved by the owner (n·N ≈ 192 trials at the
   target schedule ≈ $7 at the measured $0.0368/trial).

## What this does not do

- Does not decide *which* items get learned — that's R3.2/R3.3's
  mining and keep/prune discipline.
- Does not measure in-domain *absolute* performance — only the
  slope. An absolute N1-style read on owner tasks is a different
  estimand.
- Does not gate individual memory channels — G2 does that; N3 is
  the loop-level "is any of this working" question.
