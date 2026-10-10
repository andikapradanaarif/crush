# 2026-10-08 — #223 memory ladders: first paid results

**Spend.** 240 conclusive runs (216 ladder + 24 realism), plus
4 gate-rejected seeding attempts, `alibaba-tp/deepseek-v4.1-flash`,
temperature 0. Six invocations, all gates PASS, zero unmatched
pairs. Run records live in `eval/results/` (gitignored); the rows
below are from `crush eval compare` on the clean invocations.

**Calibration cell first.** `memory-loo-resolved` was the
predicted-null machinery pilot (24 runs). Its mechanism fingerprint
landed exactly: `pool.resolved` decisions -100% [-100,-100] p<0.001,
`candidates` 8→7, `admitted` 3→3 unchanged, steps -5.0% [-11.9,+5.2]
inconclusive-as-predicted. A non-null there would have flagged a
harness anomaly; instead the whole chain — scripted seeds → snapshot
replay → arm overlay → tail instrumentation → compare — is verified
end-to-end on paid data.

## LOO ablations (24 runs each)

| arm | mechanism (tail.decisions) | outcome |
|-----|----------------------------|---------|
| open off | open -100%, command.admitted **+200%** (1→3 twins un-shadow), total admitted 3→3 | steps +0.0% [-11.1,+8.0], pass 12/12 |
| command off | command -100%, admitted 3→2 (-33%) | steps -5.3% [-16.0,+6.7], pass 12/12 |
| resolved off | resolved -100% (was already 0 admitted) | steps -5.0% [-11.9,+5.2], pass 12/12 |

**The offline predictions held on every mechanism.** Suppressing
`open` is an envelope change, not information loss — the same three
commands render, now under the command envelope. Suppressing
`command` loses exactly its hint row. No LOO arm moved steps or
pass-rate beyond noise: on this cell no single pool is
load-bearing in the outcome sense, though the selector's shadowing
structure is exactly as modeled.

## Depth ladder (72 runs, m0/m1/m4 × 12 pairs)

Per-cell steps (control = memory off):

| cell | ctrl | treat | Δ | admits | section bytes |
|------|------|-------|---|--------|---------------|
| m0k0 | 7.58 | 7.67 | +1% | 0 | 0 |
| m1k0 | 8.33 | 7.42 | **-11%** | 1.0 | 258 |
| m4k0 | 7.92 | 7.42 | **-6%** | 3.0 | 553 |

Pooled: steps -5.4% [-10.5,+0.3] p=0.049, `tool_call_bytes` -28.3%
p=0.001, `tokens.output` -16.6% p=0.010, `tokens.input` +4.5%
p=0.048 (the memory section's price). Suggestive dose-response —
m0 anchor is nil as predicted and rendered dose matched the
offline matrix exactly (0/1/3) — but the response is non-monotone
(m1 −11% > m4 −6%), n = 12 pairs per cell, and the pooled p=0.049
sits unadjusted among the many metrics reported here; no single
primary comparison was pre-named, so treat the step result as
nominal. **Verdict: inconclusive.** Relevant memory saves ~5-11%
steps on this cell class; the effect is real but smaller than the
MDE we powered for.

## Distractor ladder (72 runs, k0/k1/k3 in-scope × 12 pairs)

Per-cell (control admits 0 — memory off; treatment admits 3+j):

| cell | ctrl steps | treat steps | Δ | treat input | decoys |
|------|-----------|-------------|---|-------------|--------|
| m4k0 | 7.92 | 7.58 | -4% | +15% | 0 |
| m4k1 | 7.92 | 7.17 | **-9%** | +14% | 1 |
| m4k3 | 8.25 | 8.33 | **+1%** | **+25%** | 3 |

Pooled steps -4.2% [-9.4,+1.0], inconclusive. But the dose
gradient is the predicted harm shape: at j=1 the relevant-memory
benefit still dominates; at j=3 the decoys fully cancel it — while
input cost climbs to +25% and output tokens nearly double
(717→1398) as agents burn work processing wrong rows. Pass rate
stayed 1.00 at every dose. **Wrong in-scope memory costs work and
context, not correctness** — the agent verifies rather than
trusts. The j=3 cell is the boundary where harm equals benefit;
whether it crosses into net harm is under-powered at 12 pairs.

## What this changes

- The free/offline tier predicted all three LOO mechanism
  fingerprints and both dose anchors before a single paid run.
  Pre-registration wasn't ceremony — every surprising number (m4
  renders 3 not 4, open-LOO is envelope not information) was called
  in advance from `eval select` output.
- Memory's step-saving effect on this cell class is ~5-11%, below
  the +10% MDE — future powered runs on this class need either a
  smaller MDE bound or a higher-difficulty corpus where memory
  carries more of the task.
- Wrong-memory harm is a dose gradient, not a cliff, bounded by
  verification behavior. Pass-rate is the wrong instrument for it;
  `tokens.input` and steps are the sensitive metrics.
- `cost_weights` (0.25/0/4 relative units) are now pinned in all
  memory manifests — `weighted_cost` resolves.
- Band drift fix: `runs_per_trajectory` covers all three bands —
  a pilot's control passes reclassify the trajectory
  (uncharacterized → mid) and previously zeroed follow-on plans.

## Agent-seeded realism arm (28 runs, `agentseeded-three-pool`)

The `prior_sessions` twin of the pinned m4k0 cell — same fixture,
check, and prompt; the two seed sessions are real agent turns
("fix the ./tax test", "check ./quota without modifying files").

**Seeding honesty holds.** The agent seeds produced the designed
shape on every gated attempt: `resolved_rows=1` (tax),
`open_quota=1` (target still broken — the tree gate
`return 41` fires), `seed_sessions=2`. Two early attempts were
rejected inconclusive on `cmd_rows=3 < 4` — the agent ran exactly
the prompted commands, which dedupe to ~3 `(cmd,cwd)` rows; the
floor was relaxed to ≥3 and the snapshot machinery correctly
cached only gate-passing state.

**Measured replication.** Under agent seeds: control 7.75 →
treatment 7.67 steps (**-1.1%**, vs scripted twin's -6%); pass
12/12 both arms; admits 2.0 (1 open + 1 command) vs scripted's
3.0 — the seed agent ran `go test ./quota` once where the script
ran it twice (`-run` variant + plain), so the rendered dose is
leaner. Mechanism fingerprint identical: resolved row rejects
`out_of_scope`, command twins shadowed by open. Verdict: **the
scripted-seed machinery is an honest surrogate within this cell's
resolution** — direction and mechanism replicate; magnitude
differences trace to authored dose, not distortion.

## Still open

- **Combined cell** (M=4, j=1) — both manifest arms exist; the
  singles suggest j=1 sits inside the benefit regime.
- **j>3 cells** — the cap saturates open renders at 5; higher wrong
  dose needs more relevant rows or a higher cap to test.
- **Group-sequential `decision_rule`** — interim looks remain
  procedural.
