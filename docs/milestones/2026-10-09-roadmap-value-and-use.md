# Roadmap — value and use (2026-10-09)

This supersedes `2026-10-04-roadmap-fixed-skeleton.md` as the live
plan. The skeleton stays as the record of the measurement era —
what was built, what the powered reads found.

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
  thresholds, no admit margins, and no learner. #228 is the substrate
  (provenance + `param_version`); learning itself is unbuilt.
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
  weak — that feeds directly into P5.

## Defects the review found — disposition

1. **`eval/flags.json` and `flagCodeDefaults` disagreed with code
   defaults** (stale pre-#205 `notebook_enabled`/`notebook_checkpoint`
   `true` in both; the child resolves `false`). Mislabeled baseline
   keys and under-rejected gated predicates. **Fixed** — values
   synced; `TestFlagsDefaultsMatchCodeDefaults` pins every declared
   key to the Options resolvers so the next default flip fails in CI.
2. **Referent learning counted unparseable follow-ups as acceptance**
   (`referentJudgedVerdict` = cue-miss → accepted). **Fixed** —
   `ReferentUnknown` verdict for follow-ups the English cues cannot
   read (non-ASCII letters or no lexical content); recorded for
   provenance and future relabeling, never promotion evidence. Known
   residual: Latin-script languages the cues don't cover still slip —
   the real fix is P2's artifact signals.
3. **Roadmap trailed the code** — fixed by this document.
4. **10-08 writeup overstated** ("real dose-response" on a
   non-monotone, n=12, multi-metric read) — fixed: reworded to
   suggestive/non-monotone with an explicit multiplicity note.
5. **Package concentration** (`agent.go` 3,897; `failure_select.go`
   1,719; `run_edges.go` 1,799; `turn_context.go` 1,148 — every new
   channel lands in `internal/agent`) — owned by #232 (table-driven
   dispatch) plus a channel interface, sequenced before the next
   channel lands.

## The phase plan

Ordered by what unlocks the most evidence per unit of work.

**P1 — Use it (no code).** Daily-drive this fork on real projects
with `failure_memory`, `failure_memory_edges`, `referent_memory`,
`session_memory`, and memory telemetry on. (Enabled in the author's
global config 10-09 — the reviewer's "zero real sessions" evidence
predates that change by hours.) A sustained stretch of real prompts
— including code-switched ones the resolver and referent learner
have never seen — is the only way to get: data for #228, the
≥2-projects evidence bar for #229, and a replay corpus for P3. P1
gates P3 — it ends when the replay corpus is there, not on a date.

**P2 — Acceptance signals from artifacts, not words.** Judge accepted
vs. revised by what happened to the code: did the edit survive to the
next session (hash unchanged), was it committed or reverted
(`git log`/reflog), were the tests green at session end (cmdlog rows)?
Language-neutral, hard to fool — the real labels for referents and
#228, and the closing fix for defect 2's residual.

**P3 — Close the learning loop on real-session replay.** Snapshot
real sessions at start (#206 already captures start SHA + prompt) →
proposed parameter change → offline selector curves + #108 replay of
real prompts → acceptance gate (#197/#152) → versioned rollout with
the existing randomized holdout → re-verify (#198). RRSI's evolution
loop applied to learned variables. First candidate parameter:
`open_render_limit` per ambiguity class.

**P4 — Procedural memory: skills learned from verified runs.** The
largest functional gap. Candidate source: a run where the verify gate
passed, nothing was revised, and the step sequence repeats across
sessions → staged project skill after the injection screen and the
contamination check → curator-style lifecycle (usage counts, stale/
archive, pin). Frontier systems (and Hermes) get most of their
compounding here; our acceptance signal — verify gate plus artifacts
— is stronger than model self-judgment.

**P5 — Unified context budget.** Today every channel (open, command,
resolved, referent, digest, heat, intent) holds its own fixed cap.
Replace with one token budget channels compete for, allocated by
measured marginal value — the LOO read supplies those values, and
it's a legitimate learned variable inside the skeleton. Also the
direct fix for the distractor finding: fewer rows rendered when
relevance is weak.

**P6 — Tasks where memory should matter.** #227 sub-ceiling corpus
(30–100 steps, expensive discovery, control pass ~40–70%) — the only
class where "memory improves capability, not just speed" can be shown.
Then #226 SWE-bench Verified run repo-by-repo in chronological order
— natural staleness + a public comparison point. Both ahead of any
further ladder cells.

**P7 — Generalize, then ship defaults.** #225 (second model) and
#224 (sealed pool) before any default flip; `failure_memory` on for
everyone only if it clears the gate. A frontier harness is one people
actually run.

**P8 — Table stakes.** #78 sandbox phase D first — learned memory
plus autonomous edges make containment necessary. #3 background
subagents — isolation is a proven alternative to compression, and
the notebook data already argued for it.

**P9 — Code shape before the next channel.** #232 table-driven
dispatch plus a minimal channel interface (select → render → record
→ verdict) so the next channel lands as a row in a table, not a
ninth edit site in `internal/agent`.

## Sequencing

- **Step 1** — defects 1–2 + this doc; memory + telemetry on for
  daily use. #290 (seed behavioral evidence) landed.
- **Step 2** — P2 artifact acceptance signals; P9 dispatch/interface.
- **Step 3** — #227 first slice (3–5 tasks), then one powered read on
  it.
- **Step 4** — replay corpus from dogfooding data; first closed
  parameter-learning cycle (P3) on one parameter.
- **Step 5** — procedural-memory MVP behind a flag (P4); #225 qwen
  replication; sandbox phase D (P8).

## Standing rules carried forward

- Pre-register the primary comparison and the decision rule before
  any powered spend; name the primary metric so multiplicity stays
  visible (defect 4 is the cautionary tale).
- Learned labels and learned parameters fail closed: unparseable
  evidence is `unknown`, never a positive; a gate that can't fire
  resolves as absent, not silent.
- Default flips ship through the release policy with a powered
  on/off — P7 or nothing.
- `memory_params` changes get a `param_version`; evidence carries the
  version it was measured under.
- Notebook stays parked — dormant unless powered evidence re-opens it.
