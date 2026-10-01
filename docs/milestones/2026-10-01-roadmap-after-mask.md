# 2026-10-01 — Roadmap after the mask verdict (rev 2)

Revised after two external reviews that re-checked the raw artifacts (session
DBs, JSONL records, fixtures, `internal/cmdlog`, `turn_context.go`,
`compare.go`). Several of this document's original claims did not survive
that re-check — the corrections are recorded first, because the plan below
only makes sense with the corrected evidence base.

## Corrections to the evidence record

| Earlier claim | Corrected claim |
|---|---|
| "Mask proved wrong memory harms" | The mask tested *true* memory under an *ambiguous prompt*: `TestValue` really failed; the prompt never said which test; `check.sh` silently scored only the root package. The 3 fails were **early termination after too-narrow verification**, not retrieval error |
| "Presence ≠ content (part of gain is generic)" | **Not isolated.** A wrong-target memory is actionable content, not a placebo — it can shorten discovery by steering to a specific wrong target. Presence needs a *non-actionable* placebo arm to isolate |
| "Prompt text can't fix it" | The *tested framing* didn't fix it under this budget/fixture. Stronger claim unproven |
| "#166 file-heat proven necessary" | File heat would have picked the decoy — it was the file the seed session touched. What's needed is **task binding, selection, and abstention**; heat is one unproven ranking signal |
| "Zero harm" | No harm observed *on the tested positive corpus*. A general harm bound is unestablished |
| "Gate proven honest" | The `noop-flag` alarm works. Statistical calibration, cost enforcement, and regression sensitivity are unmeasured |
| "PR #201 framed rows as historical context" | #201 closed **unmerged**; the code still says "the likely referents". (README corrected) |
| "RRSI converged on our mechanisms" | RRSI Table 6 is four hand-picked decisions, not a convergence analysis. "Consistent with our verify-gate" is fair; "converged" overclaims |
| #197's `ΔC ≤ β₀ + β₁·|Δprimary|` | Units are circular — efficiency isn't quality. RRSI's rule fires only when ΔS > δ; our corpus sits at pass 1.00, so *everything* routes to the within-band rule. #197 needs rewrite: pass-rate floor + cost rule on quality + within-band cost tie-breaker |

**Estimator bug (verified, affects all powered reads):** `compare.go:377`
drops a pair when either side is ≤0 — and on `discovery_calls_before_write`
the dropped pairs are mostly *treatment* zeros (5–9/run vs 0–5 control).
Those zeros are memory's **biggest wins** — and the mask's most decisive
decoy-direct moves. The headline effect is understated *and* the mask's
effort number excludes its most-steered runs. As memory depth grows,
treatment zeros grow → the depth-ladder slope would be biased toward flat.
Must fix before Phase 2.

**Capacity bug (verified):** failure memory can't compound as designed —
only *open* failures render (resolved ones vanish), `command_memory` is
written but never rendered, the tail caps at 5. The ladder's quirks (odd
build command, layout conventions) aren't failures — they can't even be
stored. A flat slope would prove capacity ceiling, not "doesn't learn."

## Corrected product claim

> Crush reuses verified project experience to reduce repeated work, while
> detecting uncertainty, respecting current intent, and bounding harm from
> stale or irrelevant memory.

(replaces "smarter every time / cannot be misdirected" — an implementable
contract, not an unbounded promise)

## The three-question frame for every memory candidate

1. **Truth** — was it actually observed? (nonzero exit ≠ task failure)
2. **Validity** — does it still apply to this repo state/branch?
3. **Relevance** — does it help *this* request?

The mask decoy was true and fresh but task-irrelevant. A 30-day TTL handles
none of these completely. This is the frame the selection layer must answer.

---

## Stage A — Make decisions trustworthy

*Before more experiments, fix what the numbers mean.*

| Work | Detail |
|---|---|
| **Zero-safe estimator (#203)** | Chosen estimand: paired absolute difference (or Hodges–Lehmann) + zero-rate reported as its own metric; log-ratio demoted to positive-only secondary. Re-score `bc5a`/`f0b6`/`4965`/`382f` — data is preserved |
| **Real-usage telemetry (#206, start now)** | opt-in local logging: tail fired? first actions touched referents? failure age when used? user revised after? Value = lead time — every idle day is data never collected |
| **Separate planning-MDE from shipping-MDE** | design effect (sizing) ≠ minimum worthwhile benefit (ship gate) ≠ non-inferiority margin ≠ cost ceiling ≠ stopping rule. CI crossing the bound = "inconclusive", not "underpowered" |
| **Unified verdict fields** | `execution_validity` / `mechanism_exposure` / `quality_guardrail` / `benefit_estimate` / `cost_guardrail` → single `acceptance`. "Pass + guardrail violated" was ambiguous |
| **Coverage contract fix** | `min_tail.sections.open_failures: 1` currently penalizes *correct abstention* as unexposed. Distinguish relevant-injected / correct-reject / ambiguous-surfaced. Injection rate ≠ retrieval quality |
| **Offline decision tests** | synthetic/preserved records: noisy-null, known pass regression, lower-calls-worse-pass, cost inflation, missing telemetry, valid abstention — calibrate the gate beyond the noop alarm (#152, #159 folded in) |
| **#138** | Zero CI runs ever; `TestClientServerSpawnRace` flake already bit us |
| **#109 probe tier** | Narrow scope: package the manual decoy-forensics (session-DB queries) as reusable probes |
| **#115 split** | Pull forward decision-provenance/request-identity needed for replay; defer expensive cache attribution |

## Stage B — Make memory selective (with abstention)

*The mask's real lesson: the failure was task-binding, not retrieval.*
*Runs in parallel with A — A gates interpretation, not construction.*

| Work | Detail |
|---|---|
| **Mask corpus repair (#204)** | Same task/fixture across no-memory / correct / irrelevant / stale-contradicted arms; split prompts into *explicit-target* ("fix the root package's test") vs *genuinely ambiguous* ("the test fails") where clarify-or-declare-scope is the correct behavior. Ambiguous tasks need a scorer a script can't provide — user-oracle or accept-any-declared-scope check. Add suite-wide check alongside the scored check — with `go test ./...` as the score, control would have failed for *not* fixing the decoy |
| **Smallest deterministic selector** | eligibility → validation → ranking → inject/offer/abstain. Signals: current user scope, command/package/CWD match, provenance, repo-state compat, resolution state, recency. **No embeddings, no LLM critic** until measured failure cases justify them |
| **Decision-level observability** | record per decision: candidate IDs, selected, rejection reasons, source session/tool call, validity + task-match evidence, rendered bytes, action targets, verification outcome — the missing bridge between "tail rendered" and "memory helped" |
| **End-of-turn reconciliation edge** | deterministic: failures observed this run still open? → don't report done. Re-run broad check or name what's open. Targets the exact mask failure signature (early termination); hypothesis to test on the repaired corpus |
| **Write-side injection screening** | failure headlines come from tool output = repo content = attacker text. `tailSafeText` neutralizes brackets only. Memory is now a persistent prompt-injection channel |
| **Provenance records before #165** | per-observation: which session/tool call, against which repo state, expected-negative vs real failure, which later observation resolved it, was this interaction memory-suggested |
| **Heat-feedback caution** | `read_files` can't distinguish user interest from memory-suggested reads — unfiltered heat reinforces itself. Heat is a prior, never authority over explicit user paths/scope/branch/fresh observations |

## Stage C — Accumulation and interference, separately

*A single depth slope can't attribute failure; split it.*

| Work | Detail |
|---|---|
| **Capacity fix first** | render `command_memory` ("how this project builds/tests") + keep resolved-failure knowledge ("X failed because Y; fixed in Z") — partial Phase-4 pull-forward; without it the ladder measures the cap, not learning |
| **Depth ladder (useful knowledge)** | fixed noise, depth 0/1/2/4 of *relevant* experience; randomize which quirk lands at each stage (else depth confounds with quirk identity); measure quality + amortized cost + repeated-command rate + novel-combination use |
| **Distractor ladder (interference)** | fixed useful knowledge, distractors 0/5/20; measure wrong-target actions, injected-row precision, abstention, ambiguity cost, tail latency |
| **Leave-one-out ablation** | all-K vs K−1 per memory — marginal value + interference, more causal and cheaper than the slope alone |
| **#108 snapshot replay (narrow)** | settled workspace+memory snapshot → run next task under alternative memory selections; full turn-replay later if notebook work needs it |
| **#117 staged** | deterministic reconstruction tests → persistent-vs-restart pilot → powered interaction study only if pilot shows material difference |

## Stage D — Broader memory types, evidence-gated

| Issue | Disposition |
|---|---|
| #165 referents | **Defer until provenance + abstention exist.** "No correction next turn" is weak feedback. Split: episodic storage / promotion-to-reusable / contamination screen — three mechanisms, not one |
| #164 digest + FTS5 | Gated — FTS5 is lexical, not semantic. Test simpler structured cmd/pkg/path matching first |
| #166 | **Split issue**: map-skeleton ranking (as filed) ≠ failure-selection (what the mask needs). Separate acceptance criteria |
| Notebook stack (#86/#90/#91, #153; flip decision #205) | **Prune candidate**: `notebook_enabled`/`notebook_checkpoint` still default-on with zero powered positive reads — contradicts our own evidence gate. Turn off by default or freeze until a comparator clears the floor |
| #110/#139/#107 | Context-economics, not cross-session learning — schedule by production impact, don't gate on depth slope |

## Stage E — Release under continuing evidence

| Work | Detail |
|---|---|
| **Three evidence pools now** | dev corpus (free) / validation corpus (recorded-use) / **sealed held-out** (blind-authored, other languages, opened only at release decisions). Adaptive re-eval of the same 7 fixtures across 46 invocations is already mild overfitting — no literal leakage found, but iteration is the risk |
| **Second-model pilot early** | cheap qwen replication once protocol frozen — catch model-specific behavior before building on it (not Phase-5-late) |
| **SWE-bench Verified subset** | OOD regression check before default flips (~120GB Docker disk locally, or sb-cli/Modal remotely). Bonus idea: repo-chronological runs with `.crush/` retained = natural-staleness cross-session test |
| **Re-verify by feature class** | invariants & safety controls need failure-scenario tests, not mean deltas; only utility optimizations need periodic powered evidence. Don't prune a rare-failure safeguard for flat means (#198 amendment) |
| **#54/#38** | standing release policy, not a late phase — every default flip behind its own powered read |
| **#78 containment** | parallel track — a memory-induced wrong action makes sandboxing directly relevant, not orthogonal |

## Issue disposition changes from the reviews

- **#197** — rewrite required: floor on pass rate; cost rule only when
  ΔS > δ; within-band → ΔC ≤ 0 + efficiency tie-break; cost includes
  summarizer/generator tokens. Current form rewards cost-reduction with
  token growth — circular.
- **#166** — relink: it describes map-skeleton ranking; failure-memory
  selection is a different problem with different acceptance criteria.
- **#198** — keep cadence; replace CI-overlap alarm with current-harness
  on/off comparison vs minimum-useful-benefit (non-overlap can mean
  improvement, not just decay).
- **#3, #140, #115(partial)** — stay parked/staged per above.
- **Notebook default-off** — new issue or fold into #38: flip
  `notebook_enabled`/`notebook_checkpoint` off until powered evidence.

## Next steps, in order

1. Fix README/code drift + correct these claims (done in this rev).
2. **Start real-usage telemetry now (#206)** — lead time is the cost; every
   idle day is data never collected.
3. Zero-safe estimator (#203) + unified verdict fields (#152); re-score
   `bc5a`, `f0b6`, `4965`, `382f` under the new estimator.
4. Repair the mask corpus (#204) — explicit vs ambiguous tasks, dual check;
   parallel with 3.
5. Reconciliation edge + deterministic selector w/ abstention → validate on
   repaired mask corpus (bar: pass ~1.00 *and* effort savings retained).
6. Decision-level observability records.
7. Command/convention memory rendering (capacity fix for Stage C).
8. Notebook default-off decision (#205).
9. Sealed held-out set + cheap qwen pilot.
10. Depth + distractor ladders (post A–B).

## Standing risks

- **Measurement before mechanism**: every stage-A item must land before
  stage-B/C numbers mean anything.
- **Abstention invisibility**: a selector that returns nothing must be
  scored as success, not missing coverage — baked into the contract change.
- **Self-reinforcing heat**: attribution needed before file-heat informs
  ranking (review 2's feedback loop).
- **Corpus ceiling**: all-warm fixtures sit at pass 1.00 — efficiency-only.
  Harder tasks (control ~40–70%) needed before "smarter" can mean
  *capability*, not just speed.
- **Absolute magnitude**: current fixtures have *cheap* discovery (~3 calls).
  −27% of 3 calls is mechanism-true but user-invisible. Need tasks where
  discovery is genuinely expensive before the efficiency claim means
  anything to a user.
