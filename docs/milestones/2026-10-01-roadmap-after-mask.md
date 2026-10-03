# 2026-10-01 — Roadmap after the mask verdict

> **Superseded 2026-10-04 by
> [the fixed-skeleton roadmap](2026-10-04-roadmap-fixed-skeleton.md).**
> This file is kept as a dated log — frozen, not maintained. Its
> evidence corrections and stage work items were carried into the new
> plan of record; what remains below is the historical record of how
> the plan was derived.

Re-checking the raw artifacts behind the earlier readings (session DBs,
JSONL records, fixtures, `internal/cmdlog`, `turn_context.go`,
`compare.go`) surfaced corrections to several claims. The corrections are
recorded first, because the plan below only makes sense with the corrected
evidence base.

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

> The harness reuses verified project experience to reduce repeated work,
> while detecting uncertainty, respecting current intent, and bounding harm
> from stale or irrelevant memory.

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
| **Zero-safe estimator (#203, done 10-01)** | `(t−c)/μ_c` per trajectory (ratio-of-means); zero-side counted, no-baseline excluded (+`NoBaselinePos` for invisible regressions), `min-baseline` surfaced; bootstrap re-normalizes each replicate's baseline so Var(c̄) enters the CI; log-ratio kept as secondary view. All four preserved invocations re-scored — mask steering doubled (−64% vs −48% reported) |
| **Real-usage telemetry (#206, start now)** | opt-in local logging: tail fired? first actions touched referents? failure age when used? user revised after? Value = lead time — every idle day is data never collected |
| **Separate planning-MDE from shipping-MDE** | design effect (sizing) ≠ minimum worthwhile benefit (ship gate) ≠ non-inferiority margin ≠ cost ceiling ≠ stopping rule. CI crossing the bound = "inconclusive", not "underpowered" |
| **Unified verdict fields** | `execution_validity` / `mechanism_exposure` / `quality_guardrail` / `benefit_estimate` / `cost_guardrail` → single `acceptance`. "Pass + guardrail violated" was ambiguous |
| **Coverage contract fix (done 10-02)** | `max_tail` abstention now satisfiable — armed-but-empty renders record a zero-section `TailAudit` (#212), so "checked, nothing rendered" ≠ "never ran". Relevant-injected / correct-reject / ambiguous-surfaced still distinct per arm predicates |
| **Stale-seed fidelity (#213)** | `prior_sessions` are instructed, not verified — a seed that never lands the stale edit silently degenerates the cell into a decoy cell while `min_tail` still fires (contaminates conclusives; the verify-anyway direction fails safe). Post-hoc signature: conclusive run with `check_detail.decoy: fail`. Direction: `post_seed` state assertion or `check_detail.*` coverage predicate. **Must land before a powered mask run interprets stale cells** — does not gate #207 construction |
| **Tokens-to-done (#151)** | all token classes per attempt with CIs; unknown cost stays unknown (never estimate as 0); seed/acquisition spend tracked separately — report both marginal next-task cost and amortized lifecycle cost |
| **Offline decision tests** | synthetic/preserved records: noisy-null, known pass regression, lower-calls-worse-pass, cost inflation, missing telemetry, valid abstention — calibrate the gate beyond the noop alarm (#152, #159 folded in) |
| **#138** | Zero CI runs ever; `TestClientServerSpawnRace` flake already bit us |
| **#109 probe tier** | Narrow scope: package the manual decoy-forensics (session-DB queries) as reusable probes |
| **#115 split** | Pull forward decision-provenance/request-identity needed for replay; defer expensive cache attribution |

## Stage B — Make memory selective (with abstention)

*The mask's real lesson: the failure was task-binding, not retrieval.*
*Runs in parallel with A — A gates interpretation, not construction.*

| Work | Detail |
|---|---|
| **Mask corpus repair (#204, done 10-02)** | 14 cells, explicit/ambiguous × correct/decoy/stale/placebo/none over one shared fixture+check per family; EVAL_JSON dual scoring (declared referent scored, all scopes reported); staleness by construction (`-count=1` cmd keying); placebo = off-domain open failure; scored abstention via `max_tail` — required the armed-empty `TailAudit` fix. Probe validation caught 2 real bugs; residual seed-fidelity → #213 |
| **Smallest deterministic selector (#207, merged 10-03 in #214)** | Implemented: `failure_select.go` — explicit scope (span-based polarity: positive scope requires affirmative signal — directive verb, failure cue, referent, or bare-path prompt — and unrecognized text defaults to exclusion, so a non-English or unparseable veto suppresses rather than minting scope) → referent-kind binding (all `the-N` matches, adjective fall-through) → narrow-scope/path-gone/stale-suspect validity → admit/abstain. Per-candidate verdicts in `tail.decisions`; arm predicates `tail.decisions.candidates`/`.admitted`. Corpus split by expected outcome: mask (inject) / reject / abstain manifests. Probe-verified on corpus prompt shapes: admit renders correct row (Δ−17% tokens), stale+placebo reject with recorded reasons. Known phrasing limits: scope extraction is regex-based; `path_gone` stays unit-test-only, `stale_suspect` now has a corpus cell (`mask-ft-explicit-stalefile`: explicit scope + post-record file touch) pending a probe run. **Accepted tradeoffs:** `stale_suspect` is an mtime proxy — any write to a hinted file (formatter, generate, `git checkout`, an unverified fix, a comment edit) hides the open row until the exact command is re-run; the row stays open in cmdlog but is permanently suppressed at the prompt. We accept this false-negative — the cell encodes it deliberately — because a stale row anchoring the task is the worse direction. The same accept-loss direction governs language: the English lexicon may only *grant* scope, so positive binding in a language it can't read is lost recall — the model's own multilingual understanding carries intent instead of the parser guessing |
| **Layered language-neutral resolver (#216; #215 = its artifact layer)** | The selector makes three decisions — relevance (is this about a failure?), selection (which row?), veto (excluded?) — and prompt-text parsing is English-only, so two of three currently depend on language. The fix shrinks what relevance/veto must infer: L0 candidate-set structure (singleton top-level vs multi-candidate needs no language) → L1 language-neutral identifiers (paths, basenames, `Test\w+` vs headline, attachments — also closes "headline never binds") → L2 artifacts (#215: working set + cmd recency; promotes *unmentioned* candidates only, never resurrects a typed-but-unparseable token) → L3 small-model resolver for the residue (closed output `{about_failure, include_paths, exclude_paths, kind}`, deterministic validation, cached, abstains on failure — reuses the title-model slot) → L4 question tool when interactive. English lexicon shrinks from decision-maker to fast path. Methodology gate: offline binding benchmark over `selectOpenFailures` (labeled prompt×candidate cells by language; metrics = admit precision/recall, veto violations target 0, abstain rate, per layer) before any powered agent run. Sequenced: L0–L1 + benchmark first, L3 only if the deterministic layers leave measurable recall on the table. Known structural gap folded in: `toolclass.CommandKind` can't see through a leading `-C` (`go -C decoy test .` → kind other → `kind_mismatch` everywhere — fail-closed; flag-aware subcommand scanning belongs to toolclass, while `cmdTargets` already treats the `-C` value as the effective CWD for later relative targets) |
| **Decision-level observability** | Partially shipped with the selector: candidate signatures, admit flag, rejection reason in `tail.decisions`. Still open: source session/tool call, action targets, post-run verification outcome |
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
| #166 | **Split issue**: map-skeleton ranking (as filed) ≠ failure-selection (now #207). Separate acceptance criteria |
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

## Issue disposition changes

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
3. ~~Zero-safe estimator (#203)~~ — **done 10-01**: normalized-diff
   estimator shipped; all four preserved invocations re-scored. Remaining:
   unified verdict fields (#152).
4. ~~Repair the mask corpus (#204)~~ — **done 10-02** (#212): full matrix
   shipped + probe-validated; residual seed-fidelity assertion → #213.
5. Reconciliation edge + deterministic selector w/ abstention (#207) →
   validate on repaired mask corpus (bar: pass ~1.00 *and* effort savings
   retained).
6. Layered scope resolver (#216) → L0 structure + L1 identifiers +
   offline binding benchmark first; L2 artifacts (#215) next; L3
   small-model only if the benchmark shows deterministic layers leave
   real recall unclaimed.
7. Decision-level observability records.
8. Command/convention memory rendering (capacity fix for Stage C).
9. Notebook default-off decision (#205).
10. Sealed held-out set + cheap qwen pilot.
11. Depth + distractor ladders (post A–B).

## Standing risks

- **Measurement before mechanism**: every stage-A item must land before
  stage-B/C numbers mean anything.
- **Abstention invisibility**: a selector that returns nothing must be
  scored as success, not missing coverage — baked into the contract change.
- **Self-reinforcing heat**: `read_files` can't distinguish user-initiated
  from memory-suggested reads — attribution needed before file-heat informs
  ranking, or heat reinforces itself (suggest → read → hotter → suggest).
- **Corpus ceiling**: all-warm fixtures sit at pass 1.00 — efficiency-only.
  Harder tasks (control ~40–70%) needed before "smarter" can mean
  *capability*, not just speed.
- **Absolute magnitude**: current fixtures have *cheap* discovery (~3 calls).
  −27% of 3 calls is mechanism-true but user-invisible. Need tasks where
  discovery is genuinely expensive before the efficiency claim means
  anything to a user.
