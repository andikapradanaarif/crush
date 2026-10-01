# 2026-10-01 — Failure-memory existence proof

Motivation: Crush's `crush.db` already accumulated command/failure history,
but nothing read it back — "the agent has memory" was a write-only claim.
"Prove it learns" is the narrow scope that had to come first: show that
durable memory written in one session measurably changes behavior in a later
session, with the mechanism proven (not inferred) and the verdict machinery
proven honest.

Position snapshot: the "prove it learns" scope is complete and armored. The
product-level claim ("smarter every time") is now falsifiable engineering
rather than a slogan — one tier of evidence deep.

## What was proven

A durable failure record written in session N, rendered into context in
session N+1, measurably reduces discovery work.

| Evidence | Result |
|---|---|
| Powered run 1 | 26 conclusive pairs, `discovery_calls_before_write` −41.1% (CI [−48.9, −33.7], p<0.001), 26/26 fired `<open_failures>` |
| Top-up run | 42 conclusive pairs, discovery −27.6% (CI [−40.6, −14.8], p=0.002), bash-inclusive `calls` −27.2%, `files_viewed` −43.0%, `tokens.output` −22.3%, 42/42 fired |
| Guardrail | Pass rate 1.00 vs 1.00 in both runs — zero harm |
| MDE honesty | Pre-registered ≥35% bound NOT certified — true effect ~25–30%; reported as such |

Consolidated report: issue #160.

## What shipped (all merged)

| PR | What | Why it matters |
|---|---|---|
| #191 | `calls` metric column | Mechanism attribution — showed the savings are skipped re-runs |
| #192 | Component verdict recovery | Composite commands (`go test \| head`, `x; echo $?`, `$(..)`, backticks, multiline) can't launder a failing component into a clean record; all 7 `RecordRun` sites instrumented; signal kills = no-verdict |
| #193 | 30-day TTL + `Nd ago` age hints | Stale failures age out instead of misleading forever |
| #194 | Gate negative control | Identical arm options → `ALARM noop-flag` → `verdict: FAIL` → exit 1 — the verdict machine provably can say no |
| #195 | `TestBashTool_RecordsComponentExits` | e2e pin on the tool→ledger wiring — the seam most likely to silently regress |
| #196 | `failure-memory-mask` experiment | Control arm for the content-vs-presence confound |

Issues closed: #183 (staleness), #185 (laundering), #188 (calls metric), #154
(gate detection). Filed from the RRSI analysis: #197 (cost-justified
acceptance, `ΔC ≤ β₀ + β₁ΔS` as a gate alarm), #198 (periodic feature
re-verification — the structural pruner, zero code), design note on #165
(leakage screen requirement for referent memory).

## Mask arm — resolved (invocation `20261001T081327Z-4965`)

44/44 records, gate pass; all 22 treatment runs fired `<open_failures>`.

| Metric | Δ_mask | vs real Δ |
|---|---|---|
| `calls` | −14.5% (p=0.033) | −27% |
| `files_viewed` | −57.3% (p=0.001) | −43% |
| `discovery_calls_before_write` | −47.9% on 11 pairs (underpowered) | −27.6% |
| **pass rate** | **0.86 — guardrail violated** | 1.00 |

Forensics: all 3 treatment failures (in `mask-fix-failing-test`) spent their
steps fixing the decoy's failing test — which `check` doesn't run — and never
edited the real bug. Wrong-referent memory actively redirected the work.

**Verdict: a two-part finding, neither clean read.** (a) *Presence effect is
real* — any `<open_failures>` content cuts discovery-type work in the same
direction and similar magnitude as correct memory; part of the powered
headline is generic "warm repo → act decisively" behavior. (b) *Content
decides whether the confidence is warranted* — correct referent kept pass at
1.00; wrong referent dropped it to 0.86 at +30% output tokens. Wrong memory
is measured-harmful, not neutral. Full report: PR #196 comment; addendum on
#160.

**Roadmap consequence:** retrieval precision (#166) is promoted to
correctness-adjacent; the #165 leakage screen now has empirical backing;
TTL (#193) is load-bearing. Tier-2 work should be designed around referent
accuracy, not just recall.

## Framing-fix validation — failed (invocation `20261001T125941Z-382f`)

PR #201 re-rendered `<open_failures>` as "historical context, not the task"
and reran the identical mask corpus:

| | Baseline `4965` | With framing `382f` |
|---|---|---|
| Gate | pass + guardrail violated | **FAIL** (diffuse regression) |
| Pass | 0.86 | **0.77** — worse |
| `calls` | −14.5% | −6.1% — collapsed |
| Failures | 3× decoy-chase | 2× decoy-chase + 3× referent-verify timeouts |

The caution made agents *verify the referent first* — spending the discovery
calls memory exists to skip — and still misdirected 2/11. **Prompt text
cannot fix a precision problem; precision must live at retrieval/write time.**
#201 closed unmerged. #166 (relevance-ranked failures) is now *proven
necessary by measurement*, not just promoted.

## Post-review corrections (artifact re-check)

The mask sections above are preserved as written at the time. Re-checks of
the session DBs, fixtures, and `compare.go` corrected three readings:

1. **The mask tested *true* memory under ambiguity, not wrong memory.** The
   decoy's `TestValue` genuinely failed; the prompt never said which test;
   `check.sh` scored only the root package. The 3 fails were **early
   termination after too-narrow verification** — each run saw `TestAdd`
   failing via `go test ./...`, fixed only the decoy, re-ran `go test
   ./decoy`, and stopped. With `go test ./...` as the check, *control*
   would have failed for not fixing the decoy. "Wrong referent" is
   mislabeled — it was referent anchoring under a hidden oracle.
2. **Presence vs content is still unisolated.** A wrong-target row is
   *actionable* content, not a placebo — it can cut discovery by steering
   decisively to the wrong target. Isolating presence needs a
   non-actionable neutral envelope arm.
3. **"#166 proven necessary" doesn't follow as stated.** File-heat would
   have picked the decoy — it was the file the seed session touched. What
   is proven necessary is *task binding + selection + abstention*; the
   mechanism (heat, lexical match, or other) is unselected. Also verified:
   `compare.go` drops zero-value pairs — mostly *treatment* zeros, i.e.
   memory's biggest wins — so powered effect sizes are understated.

## Re-scored under the zero-safe estimator (PR #208)

**Why the old numbers were wrong.** The compare estimator scored each pair
as `log(t/c)` — and a log is undefined at zero, so any pair where either
arm recorded 0 was silently discarded. But on
`discovery_calls_before_write`, zero is the *result*, not missing data: a
treatment run that went straight to writing did 0 discovery calls. The
pairs most influenced by memory — for better (correct steer) or worse
(decoy steer) — were exactly the pairs thrown away.

**What the new estimator computes.** For each pair: `d = (t − c) / μ_c`,
where `μ_c` is the trajectory's mean control value — i.e. each trajectory
first establishes its own baseline, then every pair's change is expressed
relative to it (a per-trajectory ratio-of-means). A treatment zero
contributes `d = −c/μ_c` instead of vanishing. Only a trajectory whose
entire control baseline is ≤0 is excluded, since relative change is
undefined without one. CI, p, and trajectory weighting are unchanged —
only the per-pair transform.

**How to read the new report lines.** Each metric now prints
`ctrl≈x → treat≈y` (absolute means — the magnitude Δ% is relative to),
`N zero-side` (pairs with a 0 arm — kept), `N no-baseline` (pairs in
all-zero-control trajectories — excluded), `N absent` (telemetry missing),
and a `legacy log-ratio` line showing the old estimand's answer on the
positive-only pairs for continuity.

**What changed when the dropped pairs came back:**

| Invocation | Reported (log-ratio, zeros dropped) | Re-scored (all pairs) |
|---|---|---|
| `bc5a` powered | −41.1% on 18 prs | −41.2% [−67.4,−15.9] p=0.003 on **26 prs** |
| `f0b6` top-up | −27.6% on 27 prs | −30.3% [−47.9,−5.3] p=0.009 on **42 prs** |
| mask `4965` | −47.9% on 11 prs | **−64.1%** [−85.2,−41.7] p<0.001 on **22 prs** |
| mask `382f` | −8.2% on 15 prs | −27.2% [−60.2,+10.3] p=0.082 on **22 prs** |

**What the re-score teaches.** The powered conclusions *hold* (−41%/−30%
stand) — the dropped pairs there were wins consistent with the headline.
The mask picture *changes*: its steering effect nearly doubles (−47.9% →
−64.1%), because the discarded pairs were the decisive decoy-anchored runs
— the ones that wrote immediately, to the wrong file. Wrong-referent
memory steered *harder* than correct memory did (−64% vs −30/−41%), which
is the sharpest single datum for "a plausible-but-irrelevant target makes
the agent more decisive, not more careful." And `382f` moves too: the
framing fix retained real steering (−27.2%, not the reported −8.2%) — it
just bought pass regression instead of safety.

Corrected claim hierarchy and revised roadmap live in `README.md` and
`2026-10-01-roadmap-after-mask.md`.

## The plan, in steps

1. ~~Prove memory written in session N changes session N+1~~ — **done**,
   replicated.
2. ~~Make the write path un-launderable~~ — **done** (#192, #195).
3. ~~Bound staleness~~ — **done** (#193).
4. ~~Prove the gate detects a null~~ — **done** (#194).
5. ~~Content vs presence~~ — **resolved, mixed**: presence effect confirmed
   on effort metrics; wrong content proven harmful (pass −0.14). See above.
6. **Compounding ladder** — fixture with K quirks exposed over K seeding
   sessions; measure Δ vs memory depth. Monotone slope = "smarter every time"
   as a number. *Decides whether the product claim survives.*
7. **Selectivity at scale** — 20 seeded failures, 1 relevant; does the dumb
   tail survive or does noise swamp? *Decides whether retrieval ranking
   (#166) is worth building.*
8. **Generality** — second model, non-failure memory types.
9. **Tier-2 build** — #164 session digest + FTS5, #165 referent memory (with
   leakage screen), #166 file-heat — gated on steps 6–7 evidence.

## Parked / watch list

- Tier-2/3 memory issues: #164, #165, #166 — parked pending the depth and
  selectivity evidence.
- Eval infra: #151 (tokens-to-done), #152 (pre-registered rules), #153
  (notebook-generator mask — *different* mask than #196), #159 (corpus
  sizing).
- RRSI-derived acceptance machinery: #197, #198.
- Known flake: `TestClientServerSpawnRace` (clientserverrace, load-sensitive,
  unrelated to this work); broad `go test ./...` otherwise green on
  `dc1ec10e`.

## External anchor

RRSI (arXiv:2609.24972, google-research/rrsi): regularized recursive
harness evolution. Correction per review: Table 6 is four hand-picked
decisions, not a convergence analysis — "consistent with our verify-gate
class" is fair; "converged on our mechanisms" overclaimed. What transfers
cleanly is the *process discipline*: attributable candidate edits,
noise-adjusted acceptance, cost-justified gain, periodic pruning, leakage
critic. And #197 needs a rewrite — RRSI's cost rule applies only when
ΔS > δ (task score), not to efficiency metrics; our all-at-ceiling corpus
routes everything to the within-band rule.
