# 2026-10-04 — Roadmap: fixed skeleton, learned variables

**Plan of record.** Supersedes
[2026-10-01-roadmap-after-mask](2026-10-01-roadmap-after-mask.md), which
is kept as a dated log — its artifact archeology stays there for
provenance and will not be maintained further. Everything still
load-bearing is carried into this document.

## Product claim

> The harness reuses verified project experience to reduce repeated
> work, while detecting uncertainty, respecting current intent, and
> bounding harm from stale or irrelevant memory.

(an implementable contract — not "smarter every time, cannot be
misdirected")

## What the evidence actually showed

Carried forward from the 10-01 re-check of raw artifacts — these are
the corrected conclusions the plan depends on:

- **The mask failure was early termination, not retrieval error.** The
  decoy's test really failed (true memory); the prompt was ambiguous
  ("the test fails"); `check.sh` scored only the root package. The 3
  fails were narrow-verification stops — and file-heat would have
  picked the decoy anyway (it was the seed session's file). **Memory
  anchors referent choice under ambiguity; selection/abstention is
  correctness-relevant.**
- **Presence ≠ content is unisolated.** A wrong-target memory is
  actionable content, not a placebo. Needs a non-actionable placebo
  arm.
- **The estimator dropped zero pairs** — and zeros were mostly
  *treatment wins*. Fixed in #203 (zero-safe normalized-diff, ratio
  of means); all powered reads were re-scored.
- **Capacity bug:** only *open* failures render (resolved vanish),
  `command_memory` is written but never read, tail caps at 5. Depth
  ladders are untestable until #222 — a flat slope would measure the
  cap, not learning.
- **No harm observed on the tested positive corpus** — a general harm
  bound is unestablished.
- **RRSI Table 6 is four hand-picked decisions**, not a convergence
  analysis. "Consistent with our verify-gate" is fair; "converged"
  overclaims.
- **#197's `ΔC ≤ β₀ + β₁·|Δprimary|` was circular** — efficiency
  isn't quality. Corrected: pass-rate floor; flat cost allowance only
  when ΔS > δ; within-band `ΔC ≤ 0` + efficiency tie-break; cost
  includes summarizer/generator tokens.

The three-question frame every memory candidate must answer:

1. **Truth** — was it actually observed? (nonzero exit ≠ task failure)
2. **Validity** — does it still apply to this repo state/branch?
3. **Relevance** — does it help *this* request?

The mask decoy was true and fresh but task-irrelevant. A TTL handles
none of these completely — selection must.

## The architecture — fixed skeleton, learned variables

Three forces drove the re-frame: multi-project installations (project
memory must not leak; user knowledge should transfer), the adaptivity
question (a learned router forfeits auditability; a frozen harness
forfeits per-project fit), and the 2026 literature (survey + HarnessX,
below).

| Tier | Fixed/learned | Contents | Store |
|---|---|---|---|
| Global skeleton | **Fixed** | Route shape, safety ordering, veto precedence, abstention, verdict taxonomy, partition rule, provenance + measurement contracts | Code |
| User level | **Slow learned** | Preferences, correction style, parameter *priors* | User-level store, screened promotion only |
| Project level | **Fast learned** | Memory rows + learned parameter *values* | `crush.db` (per project) |
| Session | Ephemeral | Working set, artifacts | Session state |

Merge rule: a cold project initializes from user-level priors (which
initialize from global defaults); project evidence overrides the prior
as it accumulates — per-project posterior, not shared mutation.
Evidence never flows back to user level without screened promotion.

## What the skeleton owns (unlearnable)

- **Safety ordering** — veto before inference; abstention is always a
  valid outcome; fail-closed on unsupported language/unparseable input.
- **Verdict taxonomy** — the closed vocabulary of decision reasons and
  outcome classes. Fixed vocabulary is what makes `tail.decisions`
  comparable across runs.
- **Project partition** — `project_key(candidate) = project_key(current)`
  is an admissibility clause decided *before* relevance logic: a join
  condition, not a ranking signal. The key is a stable repo identity —
  the canonical git common-dir path (linked worktrees fold into the
  owning repo, sharing one partition) plus the normalized remote when
  one exists — never repo state: the session-start SHA lives in
  provenance as `repo_state` for validity checks, not partitioning.
  Leakage is a measurable, veto-class failure.
- **Provenance** — every recorded decision carries session, tool call,
  repo state, and the parameter version that produced it.
- **Measurement contracts** — paired evidence, coverage contract, gate
  alarms, sealed pools. Telemetry may retire a *mechanism*; it may not
  relax any of the above.
- **Learned-param asymmetry** — learned values may only tighten freely:
  more abstention, shorter TTLs, more veto words. Any relaxation
  requires the full evolution loop including the sealed pool (#224)
  and stays inside skeleton-defined bounds (TTL ∈ [1d, 30d]; the
  lexicon may only gain veto words, never grant words). Otherwise a
  learned value relaxes fail-closed safety through the side door.

The enumerated route space — every decision path's checks, exits, and
guards, as a diffable document — is
[ROUTE_SPACE.md](../design/ROUTE_SPACE.md).

## What learns (per-project variables, user-level priors)

| Variable | Evidence source | Issue |
|---|---|---|
| Selector thresholds, admit margins | Binding benchmark + #206 telemetry | #228 |
| TTL / staleness windows | Resolution outcomes | #228 |
| Referent promotion counts, phrase→referent maps | #165 acceptance events | #165, #228 |
| Artifact-recency windows (L2) | Binding benchmark | #216, #228 |
| File-heat weights | Attribution-corrected reads | #228 |

All of it hangs off two items:

- **#228 learned-params substrate** — a design contract first, a store
  when the first real parameter needs it (today `failure_select.go`
  holds ~6 numeric constants and no tunable thresholds — there's
  almost nothing to learn yet, and little data to learn it from).
  Learned state never lives in user config (config is authored intent;
  learned params are harness state). Decay is required — params
  without decay become another stale-memory channel. And a param
  update is itself an evidence decision: updates travel the same
  noise-adjusted acceptance rule as mechanism changes — windowed
  evidence, never single-turn outcome reactions — else tuning becomes
  adaptive evaluation over the corpus, the channel #224's sealed pool
  exists to control. Tuning ladder: tune globally first (binding
  benchmark + #206 telemetry pooled across projects); promote a param
  to per-project only when it demonstrably differs across projects
  *and* earns enough events per window to move under the acceptance
  rule — a hierarchical shrinkage estimate, honest that sparse
  evidence stays at the prior.
- **#229 user-level memory** — the slow tier: what transfers
  (preferences, correction style) vs what never does (code/work memory).
  Promotion requires evidence across ≥2 projects plus a contamination
  screen. Global defaults must be safe alone — priors only nudge, so a
  fresh install degrades gracefully to defaults, not to abstention.
  That is rule-5 compatible, not an exception: the defaults *are* the
  fail-closed state.

## Literature anchors

| Source | What transfers | What doesn't |
|---|---|---|
| **Survey** (ETCLOVG; OpenReview `eONq7FdiHa`) | The layer map — this fork builds in its thinnest layers (context/memory: 9 projects vs lifecycle: 47; observability + governance mostly commercial). Open problems #2/#3/#5 ≈ our #220 provenance/staleness, trace-native diagnosis, #198 re-verification | — |
| **RRSI** | The fixed *evolution* loop — attributable edits, noise-adjusted acceptance, cost-justified gain, periodic pruning. It governs which mechanisms stay installed; it says nothing about per-turn routing | The "converged" reading (Table 6 = four hand-picked decisions); the circular cost rule (#197, already corrected) |
| **HarnessX** (arXiv:2606.14249) | Typed primitives + substitution over a slot schema — the honest ceiling if slot *contents* ever need structural evolution. "Gains largest where baselines lowest" (+14.5% avg, up to +44%) is **in-distribution** evidence — candidates were tested on the same adaptation batch; RRSI's independent Table 1 shows HarnessX's OOD scores ≈ the base harness's (36.3/48.5/34.3 vs 36.0/48.8/34.2 — no transfer). Confirms mechanism value is conditional on baseline gaps, not that structure evolution transfers | Structural rewriting of the skeleton itself — un-auditable learned routing, worse in multi-project where it can smuggle cross-context associations no deterministic check catches. Trajectory→model-training loop is out of scope |

The convergence worth stating: all three treat **the trace as the
primary object**. Our preserved session DBs, `AnalyzeSessionDB`, and
`tail.decisions` are exactly the infrastructure a principled evolution
loop needs — per-decision attribution is what separates evolution from
blind search.

## Execution plan

### Stage A — Make decisions trustworthy

| Work | Detail | Status |
|---|---|---|
| **Zero-safe estimator (#203)** | `(t−c)/μ_c` ratio-of-means; zero-side counted; bootstrap re-normalizes baseline; log-ratio secondary | done 10-01 |
| **Coverage contract** | armed-but-empty renders record a zero-section `TailAudit` — "checked, nothing rendered" ≠ "never ran" (d2fcfee1, shipped inside #212) | done 10-02 |
| **Stale-seed fidelity (#213)** | `check.seed_script` gate after last seed, before measured run; rejects wrong state → `inconclusive`; gates assert `failure_memory` row state via sqlite, not just worktree | merged 10-04 in #217 |
| **Real-usage telemetry (#206)** | opt-in logging: tail fired? actions touched referents? failure age? user revised? — **plus** randomized on/off holdout + session-start snapshots, or it shows use, not cause | implemented in #231 (open) — lead time is the cost |
| **Unified verdict fields (#152)** | `execution_validity`/`mechanism_exposure`/`quality_guardrail`/`benefit_estimate`/`cost_guardrail` → single `acceptance` | open |
| **Planning-MDE vs shipping-MDE** | sizing ≠ minimum-worthwhile ≠ non-inferiority ≠ cost ceiling ≠ stopping rule; CI crossing bound = "inconclusive" | open |
| **Tokens-to-done (#151)** | all token classes per attempt w/ CIs; unknown stays unknown; seed spend reported separately (marginal + amortized) | open |
| **Offline decision tests** | synthetic/preserved records: noisy-null, known regression, lower-calls-worse-pass, cost inflation, missing telemetry, valid abstention (#159 folded) | open |
| **CI (#138)** | zero runs ever; selector PRs went 5 review rounds ungated | open |
| **Probe tier (#109)** | package the manual session-DB forensics as reusable probes | narrow scope |
| **#115 split** | decision-provenance/request-identity forward; cache attribution deferred | staged |

### Stage B — Make memory selective (with abstention)

*Runs in parallel with A — A gates interpretation, not construction.*

| Work | Detail | Status |
|---|---|---|
| **Mask corpus repair (#204)** | the explicit/ambiguous × memory-type matrix over bb + ft families — 15 trajectory dirs (13 matrix cells + the legacy pair); EVAL_JSON dual scoring; staleness by construction; non-actionable placebo; scored abstention via `max_tail` | done 10-02 |
| **Deterministic selector (#207)** | `failure_select.go`: explicit scope → span polarity → referent-kind binding → validity (`narrow_scope`/`path_gone`/`stale_suspect`) → admit/abstain; per-candidate verdicts in `tail.decisions`. Accepted tradeoffs: `stale_suspect` is an mtime proxy (formatter/generate/checksum hides the row — deliberate false-negative); English lexicon may only *grant* scope | merged 10-03 in #214 |
| **Layered language-neutral resolver (#216)** | L0 candidate-set structure → L1 language-neutral ids (paths, `Test\w+`, attachments — closes "headline never binds") → L2 artifacts (promotes *unmentioned* candidates only — never resurrects typed-but-unparseable tokens; #215 closed, subsumed) → L3 small-model resolver (closed output, validated, cached, abstains on failure) → L4 ask when interactive. Methodology gate: offline binding benchmark (per-language veto violations target 0) before any powered run. The English-lexicon coverage hole is silent today — a `lang_unsupported`-class reason should make it measurable before L1 lands (#232) | L1 + benchmark landed (`settled_by`, identifier mentions, `lang_unsupported`, `tail.decisions.settled.*`); L3 gated on bench recall gap |
| **Reconciliation edge (#218)** | `reconcile` run-edge: open `failure_memory` rows joined on `command_memory.last_session_id` — the session (or its task-tool children) last ran and last failed → bounded retry names the resolving commands; `edge_firings` row is the decision record (`open=N introduced=M`). Accepted bound: last-writer session key — a concurrent session's re-run lifts the row | implemented |
| **Candidate-pool cap (#233)** | fetch pool (50) and render cap (5) are separate stages: bound rows beyond the cap record `render_capped`, so "admitted but not rendered" is a named exit, not silence | implemented; named exits assertable via `tail.decisions.reasons.*` (#235) |
| **Injection screening (#219)** | `screenHeadline` at persist: ANSI/format-rune strip + phrase-level override/role/exfiltration scrub; cut spans leave `[filtered]` markers, all-payload lines persist as a placeholder | implemented |
| **Provenance (#220)** | per-observation: session/tool call, repo state, expected-negative vs real failure, resolving observation, memory-suggested flag — **+ `project_key` (stable repo identity, 10-05) + `param_version`** | open — gates #165 |
| **Decision observability (#221)** | shipped partially w/ selector (signatures, admit, reason); open: source session/tool call, action targets, post-run outcome | partial |
| **Heat-feedback caution** | `read_files` can't distinguish user interest from memory-suggested reads — attribution before heat informs ranking, or it reinforces itself | standing constraint |

### Stage C — Accumulation and interference, separately

| Work | Detail |
|---|---|
| **Capacity fix (#222)** | render `command_memory` + keep resolved-failure knowledge — else the ladder measures the cap |
| **Depth + distractor ladders + LOO ablation (#223)** | depth 0/1/2/4 relevant experience (quirk randomized per stage); distractors 0/5/20 (wrong-target actions, injected-row precision, abstention); all-K vs K−1 marginal value |
| **#108 snapshot replay (narrow)** | settled snapshot → next task under alternative memory selections |
| **#117 staged** | deterministic reconstruction → persistence pilot → powered interaction only if material |

### Stage D — Broader memory types, evidence-gated

| Item | Disposition |
|---|---|
| **#229 user-level memory** | The slow tier — starts earning promotion only once ≥2 projects of screened evidence exist |
| **#165 referents** | Deferred until provenance + abstention exist; split: episodic storage / promotion / contamination screen |
| **#164 digest + FTS5** | FTS5 is lexical not semantic — test structured cmd/pkg/path matching first |
| **#166** | Map-skeleton ranking only (retitled; `memory` label dropped) |
| **Notebook stack** | `parked`: #86/#90/#91/#153 dormant while default-off; #205 = the default-off decision itself |
| **#110/#139/#107** | Same issues, second hat: parked *qua notebook mechanism*, but their context-economics content is schedulable by production impact independent of the notebook — not cross-session learning |

### Stage E — Release under continuing evidence

| Work | Detail |
|---|---|
| **Sealed held-out pool (#224)** | dev / validation / sealed-blind pools; adaptive re-eval of the same fixtures is already mild overfitting |
| **Qwen pilot (#225)** | cheap second-model replication once protocol frozen |
| **SWE-bench Verified (#226)** | OOD regression check before default flips; repo-chronological runs = natural-staleness test |
| **Sub-ceiling corpus (#227)** | control ~40–70% pass + genuinely expensive discovery — else "smarter" means only speed and savings stay user-invisible |
| **Re-verify by class (#198)** | safety controls → failure-scenario tests; utility → powered on/off vs minimum-useful-benefit |
| **Default flips policy** | standing release policy in `EVAL_HARNESS.md` (#38/#54 closed — each flip its own issue) |
| **#78 containment** | parallel track — memory-induced wrong action makes sandboxing directly relevant |

## Next steps, in order

1. **Notebook default-off (#205)** — implemented: `notebook_enabled`
   and `notebook_checkpoint` (which follows it) resolve false when
   unset; the resolved value is materialized into the options
   projection, so each invocation's effective default is recorded in
   the config the run reports.
2. **Write-side injection screening (#219)** — persistent prompt
   channel; security item, cheap.
3. **Real-usage telemetry (#206)** — implemented in #231 (open):
   randomized holdout + session-start snapshots. Lead time is the
   cost, and #228's learning needs the data — merge lands the
   collection path.
4. **Trustworthy CI (#138)**.
5. **Powered selector run on the repaired mask corpus** — #217
   merged 10-04, so the stale cells now assert the premise they
   claim (bar: pass ~1.00 *and* effort savings retained).
   **Reject verdict (10-04, invocation 20261004T174057Z):** 8 of 9
   cells powered — 176 conclusive runs, 0 fails, pass 1.00 vs 1.00;
   88/88 seeded candidates rejected with named reasons
   (`kind_mismatch`/`negated_scope`/`narrow_scope`). Primary metric
   read "no MDE effect" (−10.9%, CI crosses 0) — the expected shape
   when memory correctly refuses to bind. `mask-ft-explicit-stalefile`
   starved both arms: the seed's don't-fix instruction doesn't
   survive this model family's verify instinct, so the stale premise
   is unseedable — quarantined as a corpus-design limitation
   (rework filed as #238, landed: seed touch changed to a bare
   `touch main_test.go` — mtime only, the seeded agent never opens
   the file — keeping the row genuinely stale for `anyPathNewer`
   while the bug stays out of view; cell restored to the powered
   manifest, seedability to be confirmed on the next reject read).
   Re-derived view committed at
   `eval/experiments/failure-memory-mask-reject-powered.json`
   (provenance: post-hoc, no run-time snapshot).
   **Reject rerun (10-06, invocation `20261005T141152Z-7491`)** —
   first powered read with L1 live: gate **FAIL**, 116/171 treatment
   runs inconclusive. Root cause (artifact-verified): the #218
   reconciliation edge is `failure_memory`-gated, so it fires only in
   treatment — it nudges seed agents to keep working the seeded
   failure (`row_older_than_touch: false` / `open_stale_rows: 0` →
   `seed_check` rejects), and its retry prompt becomes `call.Prompt`
   on retry Runs, so L1 identifier-binds the literal cmd+headline
   tokens in it (85/96 admits `settled_by: identifier`; `go test
   ./decoy` admitted 8× under a zero-identifier user prompt).
   `selectOpenFailures` unwraps `interruptedRequestRe` but not the
   reconcile retry prefix. Harness bugs filed as #248 (seeds run under
   arm config — need a fixed neutral seed config) and #249 (tail
   selects per `Run` incl. retries; select once per user turn from the
   user prompt, per-Run audit). Full writeup:
   `docs/milestones/2026-10-06-exp-failure-memory-mask-reject-rerun.md`.
   **Ablation verdict (10-06):** fixes merged (#248→#250 neutral
   seeds, #249→#251 once-per-turn selection + `tail_runs` audit,
   `failure_memory_edges` flag via #252, Windows flake fixes
   #253/#254). `edgeoff` `20261006T064413Z-f3ec` **PASS**
   (90/90 + 90/90); `edgeon` `20261006T033704Z-acea` **PASS**
   (97p/2to treatment, 99/99 control) — reconcile `fired` 96×
   with **zero admits** and zero identifier-layer settlements vs
   `7491`'s 85. Stratified cost: fired runs 9.8 steps vs 6.4 on
   edgeoff's matched gated set (~3.4 steps/retry; both timeouts on
   fired runs). `ft-explicit-stalefile` reseeds
   (`row_older_than_touch` 11/11 — #238 closed). Reject side of
   L1 certified; the edge's *benefit* remains unmeasured (reject
   corpus scores harm-avoidance only) — powered read in #256.
   Writeup: `docs/milestones/2026-10-06-exp-failure-memory-mask-reject-ablation.md`.
6. **Reconciliation edge (#218)** — implemented; eval read recorded
   (10-05, invocation `20261005T115552Z`,
   `eval/experiments/failure-memory-reconcile.json`):
   PASS on `reconcile-open-failure` — control recorded
   `reconcile.gated` 3/3, treatment `reconcile.fired` + `suppressed`
   per open epoch 3/3. Machinery/firing check at n=3/3, no primary
   metric; informational deltas were +14% prompt tokens and higher
   discovery (4.3 vs 2.7 calls/run). Powered read (10-06, invocation
   `20261006T111611Z-c5bc`, `failure-memory-reconcile-powered.json`):
   gate PASS on the exposure signature — fired 21/21, steps +3.0%,
   no MDE. The confirmatory conversion read was invalid by design
   (suite-green on this cell requires disobeying the prompt).
   Post-hoc: the merged retry's explain-escape held explicit-
   constraint violations to 0/20 vs 6/20 under the bare verification
   nudge (exploratory, p=.020 — wording effect, not reconcile
   judgment). Verification-prompt bug filed as #259. Fix-forward
   benefit still unmeasured — needs a legitimately-fixable cell,
   paired with intentional-red. Writeup:
   `docs/milestones/2026-10-06-exp-failure-memory-reconcile-powered.md`.
7. **Layered resolver (#216)** — L1 identifier layer + offline
   binding benchmark landed (`failure_binding.jsonl`,
   `settled_by`/`lang_unsupported` observability); L3 only if the
   benchmark shows measurable recall unclaimed. First powered
   exercise (10-06 reject rerun) is unreadable — the #218 edge's
   retry prompt fed L1 identifier-bearing text, so L1 bound decoy
   rows (it behaved as designed on the input). Reject side
   **certified 10-06** by the ablation rerun (step 5): zero admits
   on both arms, all 198 edgeon rejects carrying the designed
   reason.
8. **#222 capacity → #220 provenance (+`project_key`/`param_version`)
   → #221 → #228 params substrate → #223 ladders.**
9. **#224 sealed pool + #225 qwen + #226 SWE-bench + #227
   sub-ceiling corpus.**
10. **#229 user-level memory** — Stage D; promotion starts only once
    ≥2 projects of screened evidence exist.

## Standing risks

- **Measurement before mechanism** — Stage-A items must land before
  B/C numbers mean anything.
- **Abstention invisibility** — a selector returning nothing must be
  scored as success, not missing coverage (baked into the coverage
  contract).
- **Self-reinforcing heat** — `read_files` can't distinguish
  user-initiated from memory-suggested reads; attribution precedes
  ranking use.
- **Corpus ceiling + absolute magnitude** — all-warm fixtures at pass
  1.00 with ~3-call discovery: −27% of 3 calls is mechanism-true but
  user-invisible → #227 fixes both.
- **Partition errors in either direction** — *leakage* is the future
  risk (a foreign-project row anchors a referent that can never bind;
  becomes possible with #229's user tier or a multi-workspace server
  process; the partition clause is the mitigation). *Fragmentation* is
  the present one: `crush.db` already partitions per-project via the
  nearest ancestor `.crush/` — a first launch from a subdirectory
  before a root `.crush/` exists creates `subdir/.crush` and splits
  the repo's memory. Both directions are scored benchmark metrics.
- **Parameter staleness** — learned params without decay are the next
  stale-memory channel, one layer down. Repo-shape change must erode
  evidence support automatically.
- **Prior poisoning** — user-level memory reaches *every* project; the
  ≥2-project + contamination-screen bar is the mitigation. Failure
  mode to watch: one eccentric project minting a global prior.
