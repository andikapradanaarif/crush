# 2026-11-08 — P0 data audit and label-sample freeze

This corrects the 11-07 volume read using the live `crush.db` files
registered for the five projects in that read. It does not change the P0
thresholds. Counts use `settled_at IS NOT NULL`; verdicts are counted only
among settled rows. Capture health comes from `crush stats trend`.

| Project | Rows | Settled | Accepted | Revised | Failed | Unknown | Edit turns with content | Comparable settled edit turns |
|---|---:|---:|---:|---:|---:|---:|---:|---:|
| nexus | 102 | 100 | 30 | 0 | 24 | 46 | 0/19 | 0/18 |
| vishnu | 177 | 177 | 56 | 15 | 52 | 54 | 40/67 | 9/67 |
| valo-backend | 48 | 45 | 0 | 0 | 16 | 29 | 0/9 | 0/9 |
| kiosk | 24 | 22 | 0 | 0 | 16 | 6 | 0/15 | 0/14 |
| niceday-webportal | 6 | 0 | 0 | 0 | 0 | 0 | — | — |
| **Total** | **357** | **344** | **86** | **15** | **108** | **135** | **40/110** | **9/108** |

The 11-07 read counted niceday's six **unsettled** rows as settled,
giving 350 instead of 344. Its pooled verdict mix omitted 16 failed kiosk
turns. The 103 turns with nonempty `hunk_added` maps counted capture
attempts, including `out_of_workspace` cells; only 40 turns have captured
edit content and positions. T0.1 and T0.2 still clear their thresholds:
40 content-captured turns exceed 20, and 344 settled with 86 accepted
exceed 250 and 80. The recorded mix now reconciles to 344.

The four active project DBs have one `referent_episodes` row and one
`project_rates` row, both in vishnu — still too little for learned
selector calibration.

## T0.3 sample

The [#402](https://github.com/andikapradanaarif/crush/issues/402)
nexus-only plan cannot sample a revised turn: nexus has none. The
50-turn sample is therefore stratified across the four projects with
settled rows: 20 accepted (10 nexus, 10 vishnu), 10 revised (vishnu),
10 failed, and 10 unknown. Failed and unknown each draw 3 nexus,
3 vishnu, 2 valo-backend, and 2 kiosk. This keeps the original
20-accepted precision screen and adds cross-project coverage.

`eval/labels/turn_outcomes.py` froze a blind review sheet and separate
verdict manifest from rows settled before `2026-11-08T00:00:00Z`, with
seed `p0-turn-outcomes-2026-11-08`. The ignored local manifest's SHA-256 is
`59c9311cf6561174ee379b6f64ee1a3ab293d7c90ebe6ffeaf0de7af29f94f1a`.
The owner has not labeled the 50 rows; T0.3 remains open. The original
≥80% accepted-precision decision rule is unchanged.

## T0.4 baseline

`crush stats trend` ran and recorded weekly rows in local `trend.jsonl`
ledgers for nexus, vishnu, valo-backend, and kiosk. Nexus and
valo-backend meet the report's run requirement. The edit-cost baseline
is still sparse: nexus, valo-backend, and kiosk have no captured edit
content; vishnu has nine comparable settled edit turns across all
verdicts, of which six accepted/revised turns yield an edit-cost value
in one week. Thus two recorded weekly rows are **not** two informative
edit-cost baseline weeks for P5's T5.2 comparison. The report's
tokens-per-turn field also divides session-level token counters by
user-message count and assigns the result to the session's creation
week; it is a weekly session average, not a turn-level cost.

The capture warning points to sessions started outside the checkout
being edited. Fresh real work in each checkout is needed to grow an
edit-cost baseline; rerunning the trend report cannot fill missing
record-time hunk content in the old rows.
