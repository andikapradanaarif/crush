# 2026-10-07 — #222 capacity fix: the write-only ledger learns to read

Three memory pools existed; one rendered. `command_memory` rows were
written by cmdlog on every run — never read. Resolved
`failure_memory` rows vanished from the tail the moment a fix landed
— "what worked" knowledge deleted by success. The entire memory
surface was a 5-row `<open_failures>` envelope: warnings only, no
knowledge. The harness remembered and could not use it.

This writeup consolidates the capacity argument (why this was the
ladder blocker), the mechanism that landed (PR #263), the smoke
result, and what it does *not* prove. Prior mentions: the capacity
bug's one-liner in `2026-10-01-roadmap-after-mask.md` ("a flat
slope would measure the cap, not learning") and the Stage C table
entry in `2026-10-04-roadmap-fixed-skeleton.md`.

## Why the ladders were untestable without it

#223 measures the marginal value of memory *depth* — 0/1/2/4 pieces
of relevant experience. Two structural ceilings made that read
vacuous:

1. **The pool couldn't hold depth.** Five open warnings is a
   cap on warnings, not on experience. Accumulated experience lives
   in the pools that never rendered — the command ledger (how this
   project builds/tests) and resolved failures (what failed and
   later passed). A depth-4 arm whose memory pool is 5 open rows
   isn't a deeper arm; the curve flattens at the cap and the flat
   slope gets misread as "memory doesn't help."
2. **Knowledge ≠ warnings.** Resolved rows and ledger rows are the
   pools where reuse should show up first — a bound `go test` entry
   point or a once-failed-now-passing command is directly actionable.
   Excluding them biased the ladder toward the pool least likely to
   show benefit (stale warnings), not the pools most likely to show
   it.

## What landed (PR #263, `4cc83d3d` + review fixes `04fdb491`/`a3087e46`)

**Selection.** One prompt analysis binds all three pools — the
invariant being that a row cannot admit under an interpretation its
sibling rejected. `memoryPools{open, resolved, command}` feed shared
scope/kind/mention/path/staleness checks; every candidate records a
pool-tagged `FailureDecision` (`pool ∈ open|resolved|command`).
Command rows carry no headline/files, so `path_gone`/`stale_suspect`
structurally cannot fire on them; their mention vocabulary is the
command text itself (`go test -run TestAdd .` binds `TestAdd`).

**Shadow dedupe.** A ledger row whose normalized `(cmd, cwd)` key
matches an open failure is suppressed as `shadowed_by_open`,
`settled_by: state` — the warning is authoritative, the twin stays
auditable. This is load-bearing for the mask corpus: without it a
rejected decoy open row (`go test ./decoy`, `negated_scope`) would
re-anchor through its ledger twin in `<command_memory>` and the
reject cells' contract would silently die.

**Render.** Per-pool caps 5/3/3 — open warnings dominate under
pressure; worst-case tail grows 5→11 bounded. New envelopes:
`<resolved_failures>` ("resolved, last failed N ago" — `last_seen`
is the last *failing* observation, honestly labeled) and
`<command_memory>` (kind + ok/fail counts). Retry replays use the
turn cache verbatim — all three pools, per #251's invariant.

**Evidence.** `tail.decisions.pool.<pool>[.admitted]` coverage
vocabulary; `knowledge_fetch_error` distinguishes "couldn't fetch"
from "nothing existed" on the soft-fail pools (mirroring what
`fetch_error` did for open). Telemetry `candidates`/`decisions`
fold all pools — the count means "what the selector evaluated."

**Eval contract.** Both envelopes starvation-gated on
`failure_memory` (third instance of that pattern, now with a test).
All mask manifests migrated to `pool.open.*` — the pooled
`candidates`/`admitted` keys silently broadened to all pools and
would have starved reject cells on legitimately-bound ledger rows.
Pooled keys remain as the "memory of any kind" predicates.

**Corpus.** `warm-resolved-command-render` — a seed's own
fail→fix→pass produces the resolved row and the ledger row;
`seed_check.sh` gates the premise (resolved row + test-kind ledger
row + unweakened spec) before measurement spends a run, the mask
corpus's lesson applied at build time.

## Smoke result — `failure-memory-render-smoke`

14/14 conclusive (7/arm). Treatment rendered both
`resolved_failures` and `command_memory` on **all 7 runs**; control
(`failure_memory: false`) rendered neither — its null tail is the
counterfactual. Verdict: **PASS**. First live-model evidence the
knowledge pools reach the tail end-to-end: seed → rows → selector
binds "the tests" → envelopes render.

## What it does not prove

The smoke asserts *render*, not *benefit* — that distinction is the
ladders' entire job. #222 removes the ceiling; it doesn't show the
pool contents are worth their tokens. That read needs #223's
depth/distractor design, which itself waits on the interpretation
gate (#151 cost axis, #197 acceptance rule, #152 verdict) so a
bigger tail's cost is priced mechanically rather than judged.

## Unblocks

- **#223 ladders** — the pool can now hold depth; the slope can
  mean learning.
- **#220 provenance** — attaches to pool-tagged decisions; the
  provenance schema gets `pool` for free.
- **#228 params** — learned-param slots gain pool-shaped shape
  (per-pool caps, referent-kind table, stale-suspect window).
- **#229 user tier** — the three-pool pattern is the template for a
  fourth, screened pool — same selector, `pool: user`.

## Review lineage

Two deep reviews on #263, both approving with fixes that landed:
the starvation gate, the pool-scoped mask migration (a decided
semantics change, not drift), `seed_check.sh`, ambiguity-gate
suppression widened to any bound row (documented in ROUTE_SPACE),
telemetry folding, `knowledge_fetch_error`, the resolved-age label,
`sqlite3` in `requires`, and `check.sh` pinning the test's
assertion content — a corpus that exists to catch placebo
verification no longer has a placebo-checkable check.
