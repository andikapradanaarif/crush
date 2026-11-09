# 2026-11-09 — R0 post-fix turn-integrity audit

The real-data portion of
[#437](https://github.com/andikapradanaarif/crush/issues/437) now has
limited positive evidence: seven stored repair messages created after
#432 have no corresponding repair-authored `turn_outcomes` rows.
Only two outcomes were created in that window, both pending. This is
an observation of clean attribution, not enough evidence to declare
the R0 signal-integrity exit or verdict precision established.

## Scope and method

The read at **2026-11-09 14:47 UTC** used the registered project data
directories, deduplicated by resolved database path. Thirteen existing
databases were read through SQLite `mode=ro`; eleven had
`turn_outcomes`, and two predated that table. Twelve additional unique
registered database paths were absent, including old temporary test
and review checkouts. They were not counted as clean databases.
No database rows were changed.

Outcomes were left-joined to their source message using both
`turn_id = messages.id` and matching `session_id`. The audit checked
pending and settled outcomes, all project partitions in each database,
and classified the joined messages using the current predicate's
conditions: user role, no shell or subagent-report part, no summary
flag, and no repair fingerprint.

Repair detection concatenated every text part with a newline, matching
`JoinedText`, then checked the eight literals in `RepairPromptPrefixes`
and the merged retry's `repairRetryOutro`. Those literals were read
from the checked-out `run_edges.go`, rather than using only the old
reconcile prefix. Stored repair user messages were also counted
independently of outcomes, to distinguish exercised repair paths from
an empty audit window.

Windows use merge timestamps as lower bounds, not proof that a running
binary contained that merge. `turn_outcomes.created_at` is in
milliseconds; `messages.created_at` is in seconds. Each database was
read independently, so this is not an atomic fleet snapshot.

| Window begins (UTC, inclusive) | Outcome rows | Settled | Repair-authored outcomes | Other non-prompt outcomes | Outcomes missing source messages | Stored repair user messages |
|---|---:|---:|---:|---:|---:|---:|
| #432: 2026-11-08 09:08:11 | 2 | 0 | 0 | 0 | 0 | 7 |
| #433: 2026-11-08 15:24:30 | 1 | 0 | 0 | 0 | 0 | 2 |
| #457: 2026-11-09 07:08:59 | 1 | 0 | 0 | 0 | 0 | 2 |

The nonempty project reads are:

| Project | Outcomes after #432 | Repair messages after #432 | Outcomes after #457 | Repair messages after #457 |
|---|---:|---:|---:|---:|
| aurum | 1 | 2 | 1 | 2 |
| nexus | 1 | 3 | 0 | 0 |
| valo-backend | 0 | 2 | 0 | 0 |

All other readable databases had zero outcomes and zero repair messages
in these windows. Aurum now has a registered database, superseding the
11-09 roadmap's earlier absence observation. Borneo remains absent
from the registry examined here.

## Capture-health read (R0.1 / H0)

The existing fleet command completed successfully at
**2026-11-09 14:48:44 UTC**:

```sh
CGO_ENABLED=0 GOEXPERIMENT=greenteagc go run . stats fleet --all --json
```

It reported 13 project partitions: **one pass, zero failures, ten
pending, two pre-schema**. Aurum's one post-fix turn recorded two
edited path instances, both with `ok` content cells: **2/2 = 100%**,
above the H0 threshold of 80%. Its outcome is still pending, so this
demonstrates record-time capture rather than settled-label quality.
Every other partition has no post-fix edited path instances.

The fleet command identifies the post-#433 era by a nonempty `workdir`
stamp; the turn-integrity audit above uses merge-time windows. These
are different filters. Neither identifies the actual binary version
that wrote a row. Missing registered database files are outside the
fleet command's resolved targets; the separate inventory above makes
that absence explicit.

This supersedes the earlier "zero post-#433 turns" observation for
aurum only. Fleet-wide H0 is still pending: one project's two captured
paths do not establish health on the other active projects or linked
worktree workflows.

## Regression checks and disposition

At main commit `4250bc6fa`, both existing regression tests passed:

```sh
CGO_ENABLED=0 GOEXPERIMENT=greenteagc go test ./internal/agent \
  -run 'Test(RepairPromptPrefixes_Stable|IsRealUserMsg)' -count=1
```

`TestRepairPromptPrefixes_Stable` exercises the rendered verification,
todo, stall, replan, burn-watch, verify-loop, and reconcile sections,
plus the merged retry outro. `TestIsRealUserMsg` checks repair,
shell, report, summary, and real prompt classification.

This supplies an initial real-row read for #437, alongside the guard
and boundary-scan fixes in #457 and #459. Further clean usage and
settlement are still needed for R0.3's blind-label pass (#438), R0.4's
post-fix volume targets, and a stronger read of the attribution
invariant. Historical phantom rows remain excluded rather than
deleted. The roadmap thresholds are unchanged.
