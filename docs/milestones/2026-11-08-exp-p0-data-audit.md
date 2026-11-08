# 2026-11-08 — P0 data audit and label-sample correction

The 11-07 dogfood read counted six unsettled niceday rows as settled.
A deeper read also found that the reconcile edge's `Observed failures
still open` prompt was missing from `RepairPromptPrefixes`. Its stored
user-role repair messages could therefore mint `turn_outcomes` rows.
The first 50-row sample contained 22 such rows and was invalidated
before anyone labeled it. PR #427 adds the missing prefix; the table
below excludes historical repair-prompt rows without mutating live DBs.

| Project | Rows | Raw settled | Real-user settled | Accepted | Revised | Failed | Unknown | Real-user turns with captured content |
|---|---:|---:|---:|---:|---:|---:|---:|---:|
| nexus | 102 | 100 | 58 | 9 | 0 | 11 | 38 | 0 |
| vishnu | 177 | 177 | 108 | 34 | 13 | 31 | 30 | 37 |
| valo-backend | 48 | 45 | 20 | 0 | 0 | 7 | 13 | 0 |
| kiosk | 24 | 22 | 16 | 0 | 0 | 11 | 5 | 0 |
| niceday-webportal | 6 | 0 | 0 | 0 | 0 | 0 | 0 | 0 |
| **Total** | **357** | **344** | **202** | **43** | **13** | **60** | **86** | **37** |

The raw 344 settled rows include **142 repair-prompt rows**. The
real-user verdict mix reconciles to 202. The 11-07 pooled verdict mix
also omitted 16 failed kiosk rows. The 103 nonempty `hunk_added` maps
counted capture attempts, including `out_of_workspace` cells; after
excluding repair prompts, 37 real-user turns have captured edit content
and positions. The four active project DBs have one `referent_episodes`
row and one `project_rates` row, both in vishnu.

**P0 consequence:** T0.1 still clears its 20-turn content-capture floor
(37), but T0.2 does **not** clear either amended volume floor (202 < 250
settled; 43 < 80 accepted). More real-user usage is needed before P0
can exit. Historical repair rows remain in the DB and must stay out of
future volume and label reports; the code fix prevents new ones.

## T0.3 sample

The [#402](https://github.com/andikapradanaarif/crush/issues/402)
nexus-only plan cannot sample revised turns: nexus has none. The
replacement 50-turn sample excludes reconcile repair prompts and uses
20 accepted (9 nexus, 11 vishnu), 10 revised (vishnu), 10 failed, and
10 unknown. Failed and unknown each draw 3 nexus, 3 vishnu,
2 valo-backend, and 2 kiosk. The original >=80% accepted-precision
screen is unchanged. The sample is a diagnostic while T0.2 rebuilds;
it cannot by itself close P0.

`eval/labels/turn_outcomes.py` froze a blind review sheet and separate
verdict manifest from eligible rows settled before
`2026-11-08T00:00:00Z`, using seed `p0-turn-outcomes-2026-11-08`.
The ignored local manifest's SHA-256 is
`e7686a428c819073cf32c872dac44c3c6919ac474bcc26fce3a750f9f52add90`.
The old manifest hash (`59c9311c...`) is invalid. No owner labels were
entered before replacement.

## T0.4 baseline

`crush stats trend` recorded weekly `trend.jsonl` rows for nexus,
vishnu, valo-backend, and kiosk before this repair-prompt audit. Those
reports include the historical phantom rows, so they are **not** valid
P0/P5 baselines as recorded. Even before filtering, only one week had
measured edit cost (six accepted/revised vishnu turns); nexus,
valo-backend, and kiosk had no captured edit content. The trend report
also assigns whole-session token counters to the session-creation week,
so its tokens-per-turn figure is a weekly session average. A corrected
trend read and new real work in the checkout being edited are needed
before T0.4 can provide a usable P5 comparison.

The corrected `crush stats trend` read was rerun for these four projects.
It excludes stored repair prompts from settled-turn metrics and from the
user-message denominator of tokens per turn. The session token numerator
still includes repair work because tokens are stored per session, not per
message. Each local ledger now contains version 2 rows; copies of the
invalid version 1 ledgers remain alongside them as
`trend.pre-repair-filter.jsonl`.

| Project | Weekly rows | Real settled | Historical repair rows excluded | Weeks with measured edit cost |
|---|---:|---:|---:|---:|
| nexus | 2 | 58 | 42 | 0 |
| vishnu | 3 | 108 | 69 | 1 |
| valo-backend | 7 | 20 | 25 | 0 |
| kiosk | 2 | 16 | 6 | 0 |

Vishnu's sole measured week (2026-10-19) has six comparable turns,
ten edit pairs, and 7.67 changed lines per measured turn. These
corrected rows establish the current read, but T0.4 remains open: it
still lacks two comparable weekly edit-cost baselines before P5.
