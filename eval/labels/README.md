# Turn-outcome label validation (#402)

The P0 validation set tests whether settled `turn_outcomes` verdicts match
what the owner actually kept. The owner labels from the transcript and git
history; the sampler hides the machine verdict until scoring.

## Frozen sample

The 2026-11-08 sample uses turns settled before `2026-11-08T00:00:00Z`.
It has 50 rows: 20 accepted (10 nexus, 10 vishnu), 10 revised (vishnu),
10 failed and 10 unknown (3 each nexus/vishnu, 2 each valo-backend/kiosk).
All rows have a stored user message for review. Selection ranks rows by
SHA-256 of the fixed seed `p0-turn-outcomes-2026-11-08`, project, session ID,
and turn ID. The draft manifest's hash is recorded in the 11-08 milestone
audit; keep the draft files local because they identify private
sessions.

```sh
python3 eval/labels/turn_outcomes.py sample
```

This writes ignored `*.draft.jsonl` files: a blind review sheet and a
manifest with the settled verdicts. Do not regenerate the sample after
labeling begins. For each review row, inspect the session with
`crush --data-dir <project-data-dir> session show <session_id> --json` and
the project's git history. Fill `gold_label` with `accepted` (kept as-is),
`revised` (kept but changed by the owner), `rejected` (discarded or
reverted), or `unclear`. Add a note when the gold label disagrees with the
machine verdict; the scorer requires it. The note should describe evidence
without copying private prompts or source code into the eventual gold file.

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
