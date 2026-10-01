# Milestones

Dated snapshots of where this fork stands against its goal: an agent harness
that helps the user create a good product — one that gets smarter every time
it is used, and can *prove* it.

Each dated file captures one position: what was proven, what shipped, what is
parked, and the plan from that point. This README tracks only the current
position and the index; the history lives in the dated files.

## Current position

**Scope: "prove it learns."** Crush already wrote substantial information to
`crush.db` but never effectively read it back across sessions — memory was a
write-only ledger. This milestone series exists to turn "it learns" from a
claim into a measurement.

**Failure memory — existence proof: DONE and armored.** A durable failure
record written in session N provably changes behavior in session N+1, measured
across two powered runs (−27% total calls, ~25–30% discovery savings,
replicated p<0.01 ×2, 68/68 treatment runs fired, zero pass harm). The
instrumentation cannot be fooled (composite-command verdict recovery), the
data ages out honestly (30-day TTL), and the evaluation gate provably detects
a guaranteed-null experiment.

**Mask arm resolved (mixed verdict):** wrong-referent memory produced the
same effort reduction as correct memory (−14.5% calls, −57% files viewed) —
so part of the powered gain is a *presence effect*, not content. But wrong
memory also dropped pass to 0.86 vs 1.00 by redirecting work onto the decoy:
content correctness is what makes the confidence warranted. Retrieval
precision is now correctness machinery, not an optimization.

**The claim hierarchy:**

| Claim | Status |
|---|---|
| Memory exists and persists | Proven (tables, tail rendering) |
| One session's memory helps the next | **Proven** — replicated powered runs |
| The *content* does the work (not token presence) | **Partially falsified** — effort gain is substantially presence; content determines whether it's net-positive or harmful |
| Wrong memory is harmful | **Proven** — pass 0.86 vs 1.00, decoy-chasing signature |
| Smarter *every time* (compounding with depth) | Unproven — needs the depth ladder |
| Selectivity survives scale | Unproven — needs noise-seeded ledger |
| Generalizes (models, task classes, memory types) | Unproven — single model so far |

## Index

| Date | Milestone |
|---|---|
| [2026-10-01](2026-10-01-failure-memory-existence-proof.md) | Failure-memory existence proof complete; mask arm in flight; RRSI-derived acceptance rules filed |
