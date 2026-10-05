# 2026-10-06 — Mask-reject powered rerun: verdict FAIL, root cause #218

First powered reject read after five mechanism changes landed between
runs (#233 wider candidate pool, #219 headline screening, #218
reconciliation edge, #242 L1 identifier layer, #243 stalefile seed
rework). Invocation `20261005T141152Z-7491`, 9 cells × 2 arms,
270 records (99 control / 171 treatment).

Artifacts: `eval/results/failure-memory-mask-reject-powered/`
(the directory also holds invocations `9783` and `2332` — records are
per-invocation).

## Gate verdict: FAIL

Report `report-20261005T141152Z-7491.json` alarms:

- excluded-differential: `mask-bb-ambig-stale`, `mask-build-break`,
  `mask-fix-failing-test`, `mask-ft-ambig-placebo`, `mask-ft-ambig-stale`,
  `mask-ft-explicit-stalefile`.
- diffuse p = 0.0005.
- coverage-starved treatment arms: `mask-bb-ambig-stale`,
  `mask-fix-failing-test`, `mask-ft-ambig-placebo`, `mask-ft-ambig-stale`,
  `mask-ft-explicit-stalefile` (run stdout also printed "catastrophic
  coverage: 0/0 stable trajectories eligible").

On the conclusive subset the report printed control 99/99 vs treatment
47/54 — but the comparison is biased (treatment's denominator excludes
its 116 inconclusives) and all 7 treatment non-passes were **timeouts**,
not wrong fixes. Tokens: treatment +69.2% prompt-side — consistent with
reconciliation retries burning the 15-step budget, not with a
per-request cost change.

## Root cause: the reconciliation edge (#218), treatment-only

The edge is gated on `failure_memory`, so it fires **only in the
treatment arm** — in both seed sessions and measured sessions. Session
DBs show its retry prompt ("Observed failures still open — a run is
not done while command(s) it ran still have open failure rows: `- cmd`
— headline") arriving as a user message: 8/9 seed cells and all
measured-session cells in treatment; 0/99 control runs.

### Mechanism A — the edge perturbs the seed (stale-file cells)

Seed prompts say e.g. "run `go test ./decoy` … report — do not modify
any files". In treatment the edge then tells the seed agent the run
isn't done while open failure rows exist, so it keeps working the
failure: it opens the implicated file (`row_older_than_touch: false`)
or resolves the row (`open_stale_rows: 0`) → `seed_check` rejects.
On 10-04, pre-#218, `bb-ambig-stale` and `ft-ambig-stale` seeded
11/11 each — the harness change broke seedability, not the powered
driver's curiosity.

### Mechanism B — the selector reads the retry prompt (all other cells)

A reconciliation retry is a new `Run` whose `call.Prompt` is the retry
text. `selectOpenFailures` unwraps `interruptedRequestRe` (the
summarization resume wrapper) but has no handling for the
`reconcileRetryPrefix` text — so L1 mines the retry's literal command
and headline tokens (`go test ./decoy`, `--- FAIL: TestValue`) as
identifier mentions and admits the decoy rows. Evidence:

- 85 of 96 admits are `settled_by: identifier`.
- Under "the build is broken — fix it" (no identifier in the user
  prompt), `go test ./decoy` was admitted 8 times — the identifier can
  only have come from the injected retry text.
- 111 of 112 inconclusive treatment runs that reached the measured
  session had a retry in the session DB.

### Mechanism C — retries burn budget

Each retry pushes the agent to also fix the decoy inside a 15-step
budget → timeouts (7) and the +69% prompt-token delta.

## Two harness bugs this exposes

1. **Seeds run under the arm's configuration.** `prior_sessions` get
   the arm's options (`failure_memory` on → edges + tail active during
   seeding), so arms start from different states before measurement
   begins. Fix: run seeds under one fixed seed config — edges and tail
   off, memory write path on — and apply arm options only to the
   measured session.
2. **The tail recomputes every `Run`, including repair retries**, and
   the tail audit is last-write-wins — first-turn decisions get
   overwritten by retry renders, and the selector reads harness text
   as if the user typed it. Fix: select once per user turn from the
   originating user prompt, reuse across retries, and keep a per-Run
   audit history.

## What changed vs the 10-04 reject read

Same corpus, pre-#218/#242: gate PASS, 8/9 cells powered, 176
conclusive runs, 88/88 seeded candidates rejected with named reasons.
This rerun: 116/171 treatment runs inconclusive. The delta is
attributable to the mechanisms merged in between — dominantly #218's
treatment-only edge; L1 behaves as designed on the input it's given.

## Established

- The regression is a harness-level interaction, not a selector or
  corpus-design failure: arm-asymmetric seeding plus retry-prompt
  binding. Fixes 1+2 restore a controlled baseline; only then does a
  reject rerun certify L1.
- Process lesson: five mechanisms shipped between powered reads, and
  nothing re-ran earlier verdicts after #218 merged. Regression rule:
  after any mechanism merge, re-run previous verdict experiments with
  earlier mechanisms held at their verified state.
- The earlier "self-seeding / coverage-mis-scope" read of this data
  was superseded after artifact re-review — record counts, settled_by
  fractions, the retry text in session DBs, and the commit range were
  re-verified directly.
