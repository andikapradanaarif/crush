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

## In flight

**`failure-memory-mask` run** (~39/44 records at snapshot): treatment renders
`<open_failures>` seeded with a *real* failure whose referent is an inert
decoy package — memory armed but pointing at the wrong thing.

| Δ_mask reads | Interpretation |
|---|---|
| ≈ 0 | Content-specific — the information did the work |
| ≈ −27% (matches real) | Presence effect — extra tokens, not memory content |
| > 0 | Wrong memory actively misleads — quantifies the harm bound |
| Ambiguous CI | Inconclusive — add reps; no forced narrative |

## The plan, in steps

1. ~~Prove memory written in session N changes session N+1~~ — **done**,
   replicated.
2. ~~Make the write path un-launderable~~ — **done** (#192, #195).
3. ~~Bound staleness~~ — **done** (#193).
4. ~~Prove the gate detects a null~~ — **done** (#194).
5. **Content vs presence** — mask run, in flight.
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
harness evolution. The validation that matters: their search — free to edit
anything — converged on our tier-1 mechanism classes (verification gate,
background-job polling, remembered tool errors), and their acceptance rules
(noise floor, cost-justified gain, structural pruning, leakage screening) are
the ones we've now mapped onto our own loop via #197/#198/#165.
