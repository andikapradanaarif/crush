# Turn-outcome label validation (#402)

The P0 validation set tests whether settled `turn_outcomes` verdicts match
what the owner actually kept. The owner labels from the transcript and git
history; the sampler hides the machine verdict until scoring.

## Frozen sample

The 2026-11-08 sample uses turns settled before `2026-11-08T00:00:00Z`.
It has 50 rows: 20 accepted (9 nexus, 11 vishnu), 10 revised (vishnu),
10 failed and 10 unknown (3 each nexus/vishnu, 2 each valo-backend/kiosk).
All rows have a stored user message for review. Selection ranks rows by
SHA-256 of the fixed seed `p0-turn-outcomes-2026-11-08`, project, session ID,
and turn ID. The draft manifest's hash is recorded in the 11-08 milestone
audit. The blank review sheet is tracked for owner review; it contains
session and turn IDs but no prompt text. The machine-verdict manifest stays
local and ignored so labeling remains blind.

The sampler excludes historical reconcile repair prompts beginning
`Observed failures still open`. A missing repair-prefix entry previously
let those harness-authored messages mint `turn_outcomes` rows. The first
50-row sheet was invalidated before labeling and replaced; use only the
manifest hash recorded in the audit.

## Frozen samples

- **p0 (2026-11-08)** — the wave described below: 50 rows, 20 accepted
  (9 nexus, 11 vishnu), 10 revised (vishnu), 10 failed and 10 unknown
  (3 each nexus/vishnu, 2 each valo-backend/kiosk).
- **r03 (2026-11-11, #438)** — the second blind wave: 32 rows, a census
  of every settled post-#432/#433 real-user row (`workdir`-stamped).
  Strata adjusted to post-fix volume: 14 unknown, 8 revised, 8
  accepted, 2 failed — the 10/10/10 shape is unreachable while the
  failed pool sits at 2. Seed `r03-turn-outcomes-2026-11-11`, cutoff
  `2026-11-11T06:00:00Z`, manifest SHA-256
  `722b355789dc3eedb201503d5c0548fc4eefea230315d4cc99bbfb472235ab0f`
  (pinned in `WAVES["r03"]`).

```sh
python3 eval/labels/turn_outcomes.py sample            # p0 wave
python3 eval/labels/turn_outcomes.py sample --wave r03 # r03 wave
```

`not_applicable` settled rows — turns with no edited paths and no
test-kind command, so nothing for an owner to judge — are skipped by
the sampler, not counted as unexpected verdicts.

This writes the tracked blind review sheet and an ignored local manifest
with the settled verdicts. Do not regenerate the sample after
labeling begins. For each review row, inspect the session with
`crush --data-dir <project-data-dir> session show <session_id> --json` and
the project's git history. `gold_label` is **your judgment**, made without
seeing the machine verdict:

To see the evidence for row 1 without opening a full session, run:

```sh
python3 eval/labels/turn_outcomes.py review --index 1
```

Use `--index 2` for the next row. Omitting `--index` prints all 50; you can
save that private output locally with
`python3 eval/labels/turn_outcomes.py review > eval/labels/turn_outcomes_context.draft.txt`.
The helper shows the selected user prompt, the last assistant text before
the next user-role message, that next message, edited paths, and a command
for the full transcript. It does not show the machine verdict. The next
user-role message may be a harness follow-up, so inspect the full session
and git history when the short view does not settle the question.

| Label | Use it when |
|---|---|
| `accepted` | You kept or used the result without changing it. |
| `revised` | You kept the result but later changed or corrected it yourself. |
| `rejected` | You discarded or reverted the result. |
| `unclear` | The transcript and git history do not show what you did with it. |

For a turn with no lasting artifact, use `unclear` unless you can tell
from the next interaction that the result was used or rejected. The
`note` field is **optional**: leave it as `""` for a clear case. If you
want to explain a judgment, write a brief reason such as `"kept the fix,
then changed its validation check"`. Do not copy private prompts or
source code into notes. Keep the separate manifest closed until all 50
labels are filled.

```sh
python3 eval/labels/turn_outcomes.py score \
  --manifest eval/labels/turn_outcomes_manifest.draft.jsonl \
  --review eval/labels/turn_outcomes_review.draft.jsonl \
  --gold-out eval/labels/turn_outcomes_gold.jsonl
```

The scorer prints a confusion matrix, per-verdict precision and recall, and
the pre-registered accepted-precision decision: ≥80% permits #298 mining,
60–80% requires an evaluator per mined episode, and <60% blocks it. Commit
the gold file only after reviewing every row for private content.
