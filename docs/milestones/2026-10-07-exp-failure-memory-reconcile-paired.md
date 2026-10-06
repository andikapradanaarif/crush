# 2026-10-07 — Paired reconcile read: the verification escape replicates; the benefit question has no denominator

Two-cell paired run of the #218 reconcile edge under the revised
verification prompt (#260, merged as `8c82f116`):
`reconcile-open-failure` (the agent must report a failing spec and
stop) paired with the new `reconcile-fix-forward` cell (a seeded
paste-bug failure it *should* fix — the conversion read reconcile's
first powered read could not produce). Same run set, same arms:
control = `failure_memory` on, edge gated; treatment = edge live.

Manifests: `failure-memory-reconcile-paired.json` (open-failure) and
`failure-memory-reconcile-fixforward.json` — split after the
`min_reconcile.gated` coverage was shown structurally unsatisfiable
on a successful fix-forward run (a clean fix leaves no open row at
the boundary, so gating on it selects only non-fixing runs — PR
#262). Invocations `20261006T171105Z-b864` (open-failure) and
`20261006T165446Z-091f` (fix-forward); binary `crush_sha bdd97b9e`
carrying both the revised retry prompt and working `tail_runs`
export. Artifacts: `eval/results/failure-memory-reconcile-paired/`,
`eval/results/failure-memory-reconcile-fixforward/`.

## Cell 1 — open-failure: the #259 fix replicates at power

| | control | treatment |
|---|---|---|
| conclusive / pass | 20/20 | 20/20 |
| explicit-constraint violations | **0/20** | **0/20** |
| reconcile events | gated 40 (2/run) | fired 20 + suppressed 20 |
| verification.fired | 20 | 20 |
| steps | 8.0 [7-9] | 8.0 [7-9] — zero cost |

The revised retry prompt — "explicit instructions in the user's
request take precedence; if the failure is intentional, explain why
it stays failing" — held violations at **0/20 on both arms**, down
from 6/20 under the bare verification nudge. The earlier
exploratory finding (the escape preserves instruction-following)
replicates confirmatorily, and symmetrically: on the control arm
the escape must come from the verification retry itself, with no
reconcile machinery supplying it. Suite stayed red on all 40 runs
by design — every agent reported and stopped as instructed.

Reconcile remains reliable *and* free: fired+suppressed on every
treatment epoch at identical step cost to control.

## Cell 2 — fix-forward: an empty denominator, structurally

| | control | treatment |
|---|---|---|
| suite green (conversion) | 20/20 | 20/20 |
| verification.cleared | 20/20 | 20/20 |
| reconcile events | 1 `gated` | **0** |
| steps | 9.2 [7-13] | 9.3 [7-11] |

Every agent ran `go test` mid-run — the task instructs it — the
verification edge pushed the `Triple` fix, and no row reached the
done boundary open. **Reconcile never got a trigger to fire.** The
conversion-among-fired metric has no denominator: not a failed
read, a structurally impossible one on cells where the task itself
names the check.

The mechanism question this answers: reconcile's trigger requires a
stop-open moment that verification doesn't preempt. Verification
engages on the failing command's own evidence; reconcile engages
only at a done boundary the agent reaches with the row still open.
When the task says "run the tests," verification wins the race
every time — 40/40.

## What the pair says about reconcile's value

Fires reliably when the premise exists (20/20 epochs on
open-failure) at ~zero marginal cost — but its *benefit* is
backstopped: on cells where verification can engage, it never gets
to. The populations reconcile could uniquely serve:

- agents that stop-open *despite* a mid-run verification nudge
  (didn't happen in 40 runs), or
- cells where the failing observation comes from a command the task
  doesn't ask to re-run — a failure seen via `go build` while the
  task names `go test`, or a failure in output verification can't
  fingerprint — where verification structurally can't engage and
  reconcile is the only edge left.

That second cell is the shape any future marginal-benefit read
needs. Until it exists, reconcile's honest status: reliable,
free, unpriced.

## Verification-prompt status

#259's bug is fixed at power: 0 violations across 40 runs on the
cell designed to induce them. The escape's other predicted abuse —
explaining away a *legitimately* fixable failure — did not occur:
fix-forward converted 40/40, zero stopped-open runs, so the
explain-path was never used as a rationalization channel on a
fixable failure. Both directions of the wording risk cleared in
one read.
