# 2026-10-01 — Roadmap after the mask verdict

## What this document is

The forward plan for the fork, written the day the failure-memory program
produced its first honest negative result. Read it together with the `exp-*`
records — those document what experiments *produced*; this documents what we
*do next because of them*.

## Where we actually are — the honest summary

Three powered experiments on the same mechanism produced a sharper picture
than any of them alone:

1. **Memory helps** — two replicated powered runs (issue #160): a failure
   recorded in session N, surfaced in session N+1, cuts ~25–30% of
   discovery work with zero pass harm. 68/68 treatment runs provably saw
   the memory.
2. **Presence ≠ content** — the mask arm (`4965`) showed *wrong*-referent
   memory produces the same effort reduction (−14.5% calls). Part of the
   powered headline is a generic "warm repo → act decisively" effect, not
   the information itself.
3. **Wrong memory harms, and prompts can't fix it** — the mask also dropped
   pass to 0.86 (decoy-chasing), and the framing-fix validation (`382f`)
   made it *worse* (0.77): asking the model to "verify the referent first"
   burns the exact discovery calls memory exists to save, and still
   misdirects 2/11. PR #201 closed unmerged.

**The load-bearing lesson:** referent precision must be enforced at
*retrieval/write time*, not prompt time. That single measurement reshapes
everything below.

## The claim we're building toward

> The agent gets measurably smarter every time it is used — and cannot be
> misdirected by its own memory.

Two halves, both required: compounding benefit (Phase 2) AND bounded harm
under wrong input (Phase 3).

---

## Phase 1 — Armored acceptance machinery

**Question it answers:** *Can our own evaluation pipeline be trusted to say
no?*

**Why first:** every subsequent claim is only as good as the gate that
produced it. Today's gaps are known and mechanical: primaries keep landing
underpowered (measurable-pair attrition), token cost is reported but never
enforced, and verdict rules are applied by judgment instead of pre-registry.

| Issue | What it adds | Effort |
|---|---|---|
| #152 | `decision_rule` block in experiment.json — stop rules declared before the run, not narrated after | small |
| #197 | Cost-justified acceptance alarm: `Δtokens ≤ β₀ + β₁·Δprimary` — the RRSI Eq. 7 rule as code | ~30 lines + manifest field |
| #151 | tokens-to-done metric — the cost variable #197 rules on | small |
| #159 | Corpus sizing — compute reps from measured CVs so primaries land powered | doc + config |
| #138 | Trustworthy `go test ./...` — quarantine the load-sensitive flakes | medium |
| #109 | Offline probe tier — answer mechanism questions over preserved session DBs without rerunning models (the decoy-chase forensic was done by hand; make it a tool) | medium |
| #198 | Periodic feature re-verification — shipped mechanisms must keep clearing the powered bar or get flagged for removal (RRSI's structural pruning applied to *features*) | zero code — doc + cadence |

**Exit criteria:** an experiment manifest can't reach "powered" status
without pre-registered rules, sized reps, and a cost gate; every shipped
feature carries a re-verification date.

---

## Phase 2 — Does it compound?

**Question it answers:** *Is "smarter every time" a curve or a single step?*

**Why this decides the product claim:** everything proven so far is *one*
memory deep. The product promise is that session 3 beats session 2. That
requires the **depth ladder**:

- Build a fixture with K independently-planted quirks (odd build command,
  flaky test, unconventional layout, non-obvious naming…)
- K seeding sessions, each exposing one quirk into memory
- A final task whose completion touches several
- Measure Δ vs memory depth; fit the slope with a CI

**Readings:**

- Slope > 0 with significance → "smarter every time" is a measured fact;
  tier-2 work is justified
- Flat slope → we built "remembers last session," not learning → redesign
  memory *scope* before spending on tier-2 machinery
- Negative slope → accumulation actively hurts (interference) → the whole
  direction needs rethinking

| Issue | Serves |
|---|---|
| #108 counterfactual replay | Fork at turn k, replay with/without memory on identical history — the strongest causal instrument we can build |
| #117 process-model fidelity | Verifies eval sessions (restart-per-turn) behave like real persistent sessions — validity check on every number above |

---

## Phase 3 — Selectivity at scale

**Question it answers:** *Can memory survive having more of it?*

**Why the mask promoted this:** the harm wasn't hypothetical — wrong rows
redirected real work. A real project accumulates dozens of open failures;
dumping freshest-5 unfiltered is already the precision ceiling the mask
violated.

| Issue | What it adds |
|---|---|
| Noise-ledger stress (new experiment) | Seed ~20 failures, only 1 relevant to the task. Does the dumb tail still help, or does noise swamp it? Sets the acceptance bar |
| #166 file-heat ranking | Relevance-scored open failures — **proven necessary by `382f`**, not optional |
| #164 session digest + FTS5 | Retrieval over session digests — gated on the stress test showing the tail alone insufficient |

**Decision point:** if the stress test shows the tail alone survives noise,
defer #164's complexity — the mask already taught us that adding machinery
ahead of evidence is how harnesses get bloated (RRSI's whole thesis).

---

## Phase 4 — Memory beyond failures

**Question it answers:** *Can the agent learn things that aren't errors?*

| Issue | What it adds |
|---|---|
| #165 referent memory | Phrase→target mappings from accepted outcomes — **requires the leakage screen** (mask result is the empirical argument: wrong referents cost pass) |
| #86/#90/#91 | Prior-turns coverage contract, notebook-stack cost study — the *other* memory layer, evaluated with the same rigor |
| #110/#139/#107 | Compaction retention, window-relative budget, pinned prefix — context-budget mechanics |

Gated on Phases 2–3: new memory types only earn their complexity if depth
compounds and precision holds.

---

## Phase 5 — Does it generalize?

**Question it answers:** *Is any of this portable?*

- **Second-model rerun** (qwen3.8 already configured) — is the memory
  benefit backbone-specific? RRSI showed harness mechanisms transfer across
  frozen policies; ours should too, but assume nothing
- **Task-class diversity** — corpus is 7 same-shaped fix-the-bug
  trajectories; add refactor / feature-add / multi-file classes
- **#54 + #38** — paired evidence as standing policy; flags flip to default
  only behind powered reads — this is how the tier-2 features actually ship

---

## Parked — deliberately out of scope

| Issue | Why parked |
|---|---|
| #3 background subagents | Product feature, orthogonal to memory program |
| #78 sandbox runtime | Product feature, orthogonal |
| #140 sessionRuntime refactor | Hygiene — fold in when the subsystem is next touched |
| #115 telemetry v2 | Same — pull forward when attribution questions exceed current coverage |
| #153 notebook-generator mask | A *different* mask than #196 — resumes with notebook-stack work |

## Standing risks being tracked

- **Measurable-pair attrition** — both powered runs lost ~35% of pairs to
  nonpositive controls; #159's sizing must price this in or primaries stay
  underpowered
- **Presence-effect leakage** — every future memory claim needs a mask arm
  in its design by default; content-vs-presence is now a standard confound
- **Corpus reuse** — adaptive re-evaluation of the same trajectories
  inflates apparent gains (RRSI's core warning); corpus refresh is scheduled
  hygiene, not optional
