# 2026-10-01 — Roadmap after the mask verdict

Roadmap milestone: the plan going forward, as it stood once the
`failure-memory-mask` arm resolved. Companion to the `exp-*` dated records —
those say *what happened*; this says *what follows from it*.

## Where the decision tree stands

```
Phase 0: fix-or-require     → #201 referent-caution + mask validation rerun
                              (invocation 382f — PENDING at this writing)
Phase 1: honest machinery   → acceptance rules that future claims must clear
Phase 2: COMPOUNDS?         → depth ladder decides the product claim
Phase 3: SELECTIVITY?       → noise stress decides whether retrieval is needed
Phase 4: BEYOND FAILURES    → referent/prior-turn/notebook memory types
Phase 5: GENERALIZES?       → cross-model + task-class diversity + flips
```

## Phase 0 — the open node

The mask arm (invocation `4965`) proved two things: presence alone cuts
discovery-type work (−14.5% calls, −57% files viewed — same direction as
correct memory), and wrong referents actively misdirect (pass 0.86 vs 1.00,
all 3 fails = decoy-chasing). PR #201 reframes `<open_failures>` as
historical context and puts referent-matching on the model.

Validation = rerun of the identical mask corpus on the patched binary:

| Outcome | Decision |
|---|---|
| Pass ~1.00 + effort savings persist | **Merge #201** — framing fixed it; mask corpus becomes a standing regression arm |
| Pass ~1.00 but effort savings collapse | Fix works, but the powered headline was mostly misplaced confidence |
| Pass stays ~0.86 | Framing insufficient → **#166 relevance filtering is mandatory**, promoted from roadmap to requirement |

## Phase 1 — armored acceptance (cheap, gates everything below)

| Issue | Purpose |
|---|---|
| #152 | Pre-registered decision rules in the manifest — verdicts declared before runs |
| #197 | Cost-justified acceptance: `Δtokens ≤ β₀ + β₁·Δprimary` as a gate alarm |
| #151 | tokens-to-done metric — the cost variable #197 rules on |
| #159 | Corpus sizing — powered runs' primaries kept landing underpowered |
| #138 | Trustworthy `go test ./...` — meta-infra |
| #109 | Offline probe tier — session-DB forensics as a tool (the decoy-chasing proof was done by hand) |
| #198 | Periodic feature re-verification — mechanisms must keep earning their place |

*Exit: every future claim comes from a pipeline that auto-enforces cost/pass
gates, pre-registered stops, and correct sizing.*

## Phase 2 — compounding (the product claim)

| What | Decides |
|---|---|
| Depth-ladder experiment (new corpus: K quirks over K seeding sessions) | Δ vs memory-depth slope > 0 = "smarter every time" as a number |
| #108 counterfactual replay | Fork at turn k under identical history — strongest causal instrument |
| #117 process-model fidelity | Whether restart-per-turn eval sessions behave like real persistent ones |

*Flat slope → we built "remembers last session," not learning → scope
redesign before any tier-2 spend.*

## Phase 3 — selectivity at scale

| Issue | Decides |
|---|---|
| Noise-ledger stress (new experiment: ~20 seeded failures, 1 relevant) | Whether the dumb freshest-first tail survives growth |
| #166 file-heat ranking | The relevance mechanism — load-bearing if Phase 0's framing fix fails |
| #164 session digest + FTS5 | Retrieval brain; gated on the stress test showing the tail isn't enough |

## Phase 4 — richer memory types

| Issue | Note |
|---|---|
| #165 referent memory | With leakage screen — mask result is the empirical backing |
| #86 / #90 / #91 | Prior-turns coverage, notebook-stack payoff, lifecycle contract — the other memory layer |
| #110 / #139 / #107 | Compaction retention, window-relative budget, pinned prefix — context-budget mechanics |

## Phase 5 — generality

| What | Decides |
|---|---|
| Second-model rerun | Is the memory benefit backbone-specific? (RRSI: harness mechanisms transfer) |
| Task-class diversity | Corpus is 7 same-shaped fix-the-bug trajectories — needs refactor/feature/multi-file classes |
| #54 / #38 | Paired evidence as standing policy; flag flips only after powered reads |

## Parked / separate track

- #3 background subagents, #78 sandbox runtime — product features, orthogonal.
- #140 `sessionRuntime` refactor, #115 telemetry v2 — hygiene, fold in when
  the subsystem next gets touched.
- #153 notebook-generator mask — resumes with notebook-stack work; distinct
  from #196's failure-memory mask.

## Current write markers

- Validation run `382f` in flight at this writing — this doc's Phase-0 cell
  is the only cell that changes based on its result.
