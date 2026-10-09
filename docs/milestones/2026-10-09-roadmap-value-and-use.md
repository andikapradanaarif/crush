# 2026-10-09 — Roadmap: value and use

**Plan of record.** Supersedes
[2026-10-04-roadmap-fixed-skeleton](2026-10-04-roadmap-fixed-skeleton.md),
which stays as the record of the measurement era — what was built,
what the powered reads found.

## Product claim

> The harness reuses verified project experience to reduce repeated
> work, while detecting uncertainty, respecting current intent, and
> bounding harm from stale or irrelevant memory.

## Why this rewrite

An external review (10-09) audited the roadmap, the 10-08 ladder
writeup, the open issues, `params`, the referent-memory code, the
eval loader, and the author's real `crush.db` files. Its core finding:
**the measurement machinery is frontier-grade; the memory isn't yet,
because nothing uses it.** Every channel defaults off, no production
project has generated a single memory row, and the powered reads show
~5–11% fewer steps on ~8-step tasks — below what a user notices.
The phase that follows moves effort from instrument-building to
value and use.

## The corrected record

The review found the docs had drifted ahead of the facts. Set down
honestly:

- **#223 ladders ran and were inconclusive** (writeup `2026-10-08`):
  depth −5.4% steps pooled (nominal p=0.049, non-monotone m1 −11% >
  m4 −6%, n=12 pairs/cell); distractor −4.2% inconclusive with the
  predicted harm shape at j=3 (+25% input tokens). The skeleton still
  listed #223 as "next item" days after the verdict landed.
- **#206 is implemented, never enabled.** The collection path merged
  in #231, but every memory channel defaults off and no production
  session has written a row. "Done" meant the machinery exists;
  the dataset it exists to produce is empty.
- **"What learns" listed variables that don't exist.** `params.Memory`
  is 17 fields — `working_set_limit`, `vague_prompt_max_words`,
  `intent_max_bytes`, `file_heat_limit`, `fetch_limit`,
  `open_render_limit`, `resolved_render_limit`, `command_render_limit`,
  `failure_file_hints`, `failure_cmd_runes`, `failure_headline_runes`,
  `open_failure_ttl`, `referent_render_limit`, `referent_promote_hits`,
  `digest_render_limit`, `digest_refresh_limit`, `digest_file_hints`.
  All caps, limits, TTLs, and text bounds. There are no selector
  thresholds, no admit margins, and no learner. #228 landed the
  substrate (provenance + `param_version`); learning itself was
  unbuilt until this plan.
- The measured effect size (~5–11% steps on ~8-step tasks) is under
  the +10% MDE bound and under what a user would feel. The corpus
  ceiling the skeleton named is the live constraint, not a caveat.

Consequence: **no more ladder spend until the task class can carry
a bigger effect.** #227's sub-ceiling corpus moves ahead of any
further dose/distractor cells.

## What doesn't change

These are the things worth keeping — most harnesses can't claim them:

- Pre-registration + `decision_rule` on every powered spend, with the
  offline tier predicting fingerprints before paid runs (LOO and both
  dose anchors were called in advance and landed).
- Fail-closed selectors, injection screens at the write path,
  contamination screens (`suggested`), per-observation provenance
  (`project_key`, `param_version`).
- The honest-null posture: an inconclusive read is a result, and a
  channel that can't carry weight gets shrunk, not justified.
- The ladders' real finding stands: wrong in-scope memory costs work
  and context, not correctness (pass rate held at 1.00; the agent
  verifies rather than trusts). Render fewer rows when relevance is
  weak — that feeds directly into #299.

## What learns (per-project variables, user-level priors)

| Variable | Evidence source | Issue |
|---|---|---|
| Render limits, promote floors, TTLs — the 17 `params.Memory` fields | certified values on labeled episodes, fixed-sequence tested | #295 (substrate #228 landed) |
| Per-project rates (referent acceptance, row usefulness) | Beta-Binomial posterior with session decay; sparse projects stay at prior | #296 |
| Online arm selection among certified values | propensity-logged decisions, conservative exploration | #297 |
| Referent phrase→target maps | #165 episodes, artifact-labeled | #294, #165 |
| Procedural skills + project conventions | accepted-run mining, replay verification | #298 |
| Channel context allocation vector | LOO marginal values | #299 |
| User-level priors (preferences, correction style) | ≥2 projects of screened evidence | #229 |

## The learning method stack

Discrete knobs + scarce data + asymmetric safety: methods that
certify before shipping, not ones that maximize an average.

| Layer | Method | Decides | Data needed | Issue |
|---|---|---|---|---|
| **A — offline calibration** | Learn-then-Test: risk estimate + Hoeffding–Bentkus p-values, fixed-sequence most-conservative → least | Which parameter values ship, certified harm ≤ α | ~60 labeled episodes per binary harm | #295 |
| **B — per-project rates** | Empirical-Bayes Beta-Binomial, γ≈0.95 decay; decide on posterior lower bound | Per-render admit/abstain | Live event rows (no corpus needed) | #296 |
| **C — online choice** | Conservative Thompson sampling over 2–4 certified values; always-valid acceptance test | Which certified value serves | Propensity-logged traffic, few hundred events/arm, pooled | #297 |
| **Gate over all three** | Seldonian-shaped: no certificate → keep default; sealed pool for relaxing changes | Whether a learned value ships | Held-out / sealed data | #152, #224 |

Design constraints settled in the 10-09 design pass:

- **Binary harms first** — a wrong-referent admit or a stale row
  rendered certifies cleanly; efficiency harms (steps/tokens) are
  continuous and confounded by task difficulty.
- **Exchangeability is honored** — episodes within a session are
  correlated; calibration stratifies to one episode per session.
- **Certificates are scoped honestly** — "harm ≤ α at 1−δ" holds on
  the calibration distribution, not universally.
- **Rewards are delayed** — edit-survival is known next session;
  the online layer's safety check runs on the early label (session-
  end test state) or throttles exploration by the lag.
- **No adaptive search on dev fixtures** — Bayesian optimization on
  the 48 corpus cells is adaptive overfitting; the sealed pool is
  the only gate for anything adaptive.

## Execution plan

### Phase 1 — Real use and honest labels

| Work | Detail | Status |
|---|---|---|
| **Dogfooding (#293)** | `failure_memory`, `failure_memory_edges`, `referent_memory`, `session_memory` + telemetry on in the author's global config (set 10-09); real prompts incl. code-switched; feeds #228's learning, #229's ≥2-project bar, and the replay corpus | running — exits on data volume, not dates |
| **Artifact acceptance labels + propensity (#294)** | accepted vs. revised judged by artifacts: edit survived to next session (hash unchanged), committed/reverted (`git log`/reflog), tests green at session end (cmdlog), wrong-target edits; plus `propensity` on each logged decision for later off-policy eval | open — the shared critical path |

### Phase 2 — The learning loop

| Work | Detail | Status |
|---|---|---|
| **Layer B — shrinkage rates (#296)** | per-project Beta-Binomial posteriors with γ≈0.95 decay; decide on posterior lower bound; scope = per-render admits, not flips | open — needs only live event rows; likely first shippable learner |
| **Replay corpus** | session-start snapshots (#206) + #108 replay machinery replay real prompts under candidate params | machinery merged; corpus pending Phase 1 volume |
| **Layer A — LTT calibration (#295)** | `crush eval calibrate`: certify param values against binary harm metrics on labeled episodes; least-conservative certified value ships as a `param_version` overlay; no cert → keep default | open — blocked on ~60 labeled episodes per harm |
| **Acceptance + re-verify** | gate: #197/#152 acceptance rule; monitor: #198 re-verification by class | merged machinery |
| **Layer C — bandits (#297)** | conservative Thompson sampler over certified values; propensity logs enable IPS/doubly-robust off-policy eval; global pooling only | open — last; needs few hundred events/arm |

### Phase 3 — New memory capability

| Work | Detail | Status |
|---|---|---|
| **Procedural memory (#298)** | mine accepted runs (verify gate passed, no revert, task shape repeats ≥2 sessions, cross-project signature) → induce SKILL.md (steps as pointers+commands, not narrative) → replay-verify (source-replay = sufficiency; held-out generalization needs ≥3 instances) → injection + leakage screen → stage for approval → retrieve ≤2, abstain → durable `skill_usage` lifecycle (decay, deprecate-on-fail, never auto-revise). Sibling tier: project conventions — inspection-gated, no replay cost | open — largest functional gap; substrate exists (`internal/skills`, verify gate, cmdlog, replay, screens) |
| **Unified context budget (#299)** | one token budget channels compete for, allocated by LOO marginal values; fixes the distractor finding (fewer rows when relevance is weak); allocation is a learned variable under the asymmetry rule | open |

### Phase 4 — Evidence where memory should matter

| Work | Detail | Status |
|---|---|---|
| **#227 sub-ceiling corpus** | 30–100 steps, expensive discovery, control pass ~40–70% — the only class where "memory improves capability, not just speed" can be shown | open — ahead of any further ladder cells |
| **#226 SWE-bench Verified** | repo-by-repo chronological order — natural staleness + public comparison point | open |

### Phase 5 — Ship and parity

| Work | Detail | Status |
|---|---|---|
| **#225 qwen replication** | second-model generalization before any default flip; procedural skills' strong→weak transfer is itself testable here | open |
| **#224 sealed pool** | blind-authored held-out corpus — the gate for every adaptive method | open |
| **Default flips** | `failure_memory` on for everyone only if it clears the gate; each flip its own issue per the release policy | gated on #224+#225 |
| **#78 sandbox** | phase D first — learned memory + autonomous edges make containment necessary | open |
| **#3 background subagents** | isolation is a proven alternative to compression; the notebook data argued for it | open |
| **#232 dispatch + channel interface** | table-driven selector + minimal channel interface (select → render → record → verdict) so the next channel lands as a table row | open — before the next channel |

## Sequencing

| Step | Contents | Gates on |
|---|---|---|
| **1** | this doc; dogfooding running (#293); labels + propensity (#294) | — |
| **2** | Layer B (#296); procedural mining pass — offline, tells us how sparse organic repetition really is (#298); #232 dispatch | label events flowing |
| **3** | #227 first slice (3–5 tasks), then one powered read on it | corpus cells authored |
| **4** | Layer A (#295) on the first binary-harm param; first closed parameter-learning cycle on the replay corpus | ~60 labeled episodes; Phase-1 volume |
| **5** | procedural MVP behind a flag (#298); #225; #78 phase D | Steps 2–4 |
| **6** | Layer C (#297) | few hundred events/arm pooled |

## Standing rules carried forward

- Pre-register the primary comparison and the decision rule before
  any powered spend; name the primary metric so multiplicity stays
  visible.
- Learned labels and learned parameters fail closed: unparseable
  evidence is `unknown`, never a positive; a gate that can't fire
  resolves as absent, not silent.
- Default flips ship through the release policy with a powered
  on/off — Phase 5 or nothing.
- `memory_params` changes get a `param_version`; evidence carries the
  version it was measured under.
- Notebook stays parked — dormant unless powered evidence re-opens it.
