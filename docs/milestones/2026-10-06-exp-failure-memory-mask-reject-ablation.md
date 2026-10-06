# 2026-10-06 — Mask-reject ablation: #218 off → on under neutral seeds + once-per-turn selector

Two-stage ablation on the reject corpus isolating the mechanism that
starved invocation `20261005T141152Z-7491`. Stage 1 gates the #218
reconcile edge off (`failure_memory_edges: false`) with everything
else armed; stage 2 restores it under the two fixes (#248 neutral
seed config, #249 once-per-turn selection + per-Run audit).

Artifacts: `eval/results/failure-memory-mask-reject-ablation-edgeoff/`,
`eval/results/failure-memory-mask-reject-ablation-edgeon/`.
Binary: `exp/ablation-218-249` (main + #248 + #249 +
`failure_memory_edges` toggle).

## Verdicts

| run | invocation | records | treatment | control | decisions (treatment) |
|---|---|---|---|---|---|
| 7491 broken stack | `20261005T141152Z-7491` | 270 | 47p/116inc/7to/1err | 99p | **96 admits, 85 identifier-settled** |
| edgeoff aborted | `20261005T215128Z-c9b0` | 91 (5 cells) | 44p/2err | 43p/1to/1err | 0 admits, all lexicon/state |
| **edgeoff rerun** | `20261006T064413Z-f3ec` | 180 | **90p/0inc/0err** | 90p | **0 admits**, 90 designed rejects |
| **edgeon** | `20261006T033704Z-acea` | 198 | **97p/2to/0inc/0err** | 99p | **0 admits**, 99 designed rejects |

Both ablation arms: gate verdict **PASS**. edgeoff control 90/90,
treatment 90/90; edgeon control 99/99, treatment 97/99 (steps μ10.7
vs μ8.2), zero inconclusive, zero fail.

## What the three-way comparison establishes

The 7491 contamination signature — 85/96 admits settled on the
identifier layer mining reconcile-retry text — is **absent in both
ablation arms**:

- **Edge off** (edgeoff): reconcile `gated` on 70/90 treatment runs —
  the join evaluated and found triggers, but no retry prompt exists
  to contaminate. Selection still binds once per turn on the user
  prompt; every cell's decoy row is evaluated and rejected with its
  designed reason.
- **Edge on** (edgeon): reconcile `fired` 96× on treatment, 31
  suppressed, 2 exhausted — retry chains ran at full strength. Still
  **zero identifier-settled admits**: retry Runs replay the turn's
  cached selection, so harness-authored retry text never reaches
  `selectOpenFailures`. The `non_user_prompt`/`harness` guard never
  fired — as designed, it's unreachable through the wired path and
  remains only as the tripwire if a future caller hands the selector
  harness text.

Treatment decision tally on edgeon is exactly one reject per run —
the designed reject reason per cell (`kind_mismatch` on ambig,
`negated_scope` on explicit-decoy, `narrow_scope` on fix/stale,
`stale_suspect` on stalefile). That is the mask-reject corpus
working as intended: decoys evaluated, decoys rejected, tasks pass
because the memory wasn't needed.

## Neutral seeds (#248)

Seed sessions ran under `WriteSeedConfig` — shared pin + harness
invariants, no arm options. Both arms therefore started from
identical warm state; the 7491 asymmetry (treatment seeds working
the decoy under an active edge) is closed. Every edgeon/edgeoff
record's `warm_start.session_ids` joins to the preserved db.

## Audit coverage caveat

The ablation binary predates `1908c6be` (`tail_runs → RunRecord`),
so records carry `tail` + `edge_firings` but not the per-Run audit
array. Replay correctness on this data is indirect — retries fired,
no retry-shaped verdicts exist — while the identical-decisions unit
test pins the mechanism. Future powered runs export `tail_runs`
directly.

## Discovery / cost deltas

- `discovery_calls_before_write` μ: edgeon treatment 6.47 vs control
  6.11 (+0.36); edgeoff treatment 6.56 vs control 6.12 (+0.44) — the
  delta is the tail's discovery reads, identical with or without the
  edge. The edge adds nothing to pre-write discovery.
- The edge's real cost shows on triggered runs: edgeon runs with
  `reconcile.fired > 0` (n=73) averaged **9.8 steps** vs 5.5
  untriggered and 6.4 on edgeoff's matched would-have-fired set —
  the reconcile retry turn costs ~3.4 steps. Both edgeon timeouts
  landed on fired runs (~2.7% of triggered runs) — the extra turn
  occasionally pushes a session over the deadline.
- The reject corpus scores only harm-avoidance, so the edge's
  intended benefit (fix-forward on real open failures) is unmeasured
  here — read the stratified delta as the edge's price, not its
  value proposition.
- 7491's treatment μ 4.67 was depressed by inconclusive runs that
  never wrote — not a real efficiency signal.

## Process lesson (recorded)

After any mechanism merge, prior verdict experiments must be re-run
with earlier mechanisms held at their verified state. The 7491
FAIL was attributed to self-seeding/coverage mis-scope before the
#218 retry-prompt path was identified — the ablation is the
re-run that resolves which mechanism caused which damage.

## Follow-ups

- `failure_memory_edges` merged to main via #252 — the flag stays
  armed by default; this corpus certifies it costs ~3.4 steps and
  ~2.7% timeout risk on triggered runs with zero admits, while its
  fix-forward benefit awaits a corpus that scores it.
- `failure_binding.jsonl` `non_user_prompt` cell — bench-level pin
  for the guard (deferred from #251 review).
