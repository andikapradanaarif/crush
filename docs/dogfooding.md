# Dogfooding and edit capture

Run each Crush session in the checkout where it will edit files, using
that checkout's own `.crush` directory. The session's workspace stays
fixed when a shell command changes directory or creates a sibling Git
worktree.

For example, create the worktree first, then start Crush inside it:

```sh
git worktree add -b feat/task ../repo-task
cd ../repo-task
crush --cwd "$PWD" --data-dir "$PWD/.crush"
```

When an existing session starts using another checkout, open a new session
from that checkout. Keep data directories separate across worktrees.

## Check capture after an edit

After a real edit, continue the conversation so the completed turn is
recorded. Turn outcomes are recorded at the following user message or
backfilled during a later session. They are not written at each tool call.

Check the current project's last seven days:

```sh
crush stats capture
```

Inspect a registered project, an explicit checkout/data directory, or all
registered databases:

```sh
crush stats capture --project nexus --since 7d
crush stats capture --cwd /path/to/repo-task --data-dir /path/to/repo-task/.crush
crush stats capture --all --since all --json
```

The report reads all recorded edit turns, including failed, unknown, and
pending turns. It shows:

- **Content:** turns with a non-empty captured edit, including deletions.
- **Positions:** turns with content and recorded line spans.
- **Pending/settled:** whether the observation window has closed.
- **Comparable settled turns:** turns with at least one pair that passes
  the existing edit-cost checks. These are capture-health counts across
  all verdicts, not an acceptance rate or admission to memory mining.
- **Exclusions:** why content or a settled pair is unavailable.

Recorded content can still be truncated, and stored positions can later
drift. Comparable pairs additionally require untruncated content, valid
settled regions, and no later-turn, stale, or unverified-position flags.

`crush stats trend` also prints capture health, including pending turns,
alongside its weekly settled-turn metrics. The capture summary uses the
same `--since` window as the trend report. Weekly JSONL metrics retain
their existing format; `stats capture --json` provides the capture report.

## Zero capture or unavailable evidence

If edited turns exist but no content was captured, the report warns. An
`out_of_workspace` exclusion means a path is outside the session root,
including a sibling worktree or an escaping symlink. Start the next
session in the checkout being edited, with its own data directory.

Sensitive paths remain excluded. Missing tables or capture columns are
reported as unavailable. A window with no recorded edit turns is distinct
from failed capture; continue the session or inspect a wider time window.

The report is read-only: it does not migrate databases, force settlement,
rewrite historical labels, or recover previously discarded edit text.
Keep the checkout available while its outcomes settle so after-edit
capture can read the files. The observation window is capped at 24 hours.

Capturing edits verifies that the data pipeline works. P0's real-usage
volume and owner-label requirements still apply; a fixture or a successful
diagnostic is not a substitute for dogfooding evidence.
