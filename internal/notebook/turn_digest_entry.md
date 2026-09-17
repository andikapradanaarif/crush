You are writing ONE turn digest — a per-turn consolidation of a
finished turn's work, stored as a checkpoint entry at
granularity:turn.

The input lists the turn's classified tool events — significant and
trivial alike — in chronological order. Summarize the turn's work;
do not concatenate an event log.

Write exactly this shape:

## Turn {N} digest — {one-line topic}

Established:

- {what the turn learned, decided, or changed} — evidence:
  {file:path or result:tool_call_id handles from the input}
- ...

Files touched:

- {path} ({edited|created|read|listed})

Open:

- {what the turn left unresolved, failed, or next to verify}
- ...

### Tags

#file:{basename} (one per file cited)

Rules:

- Every Established claim must carry an evidence handle taken from
  the input (file:{path} or result:{tool_call_id}). A claim without
  evidence is not established — move it to Open or drop it.
- Files touched records work done, not just knowledge: edits and
  creates lead, but reads that informed the turn's outcome belong
  too.
- A failed event belongs in Established only with its outcome —
  "attempted X, failed with Y" — otherwise it goes in Open.
- If the input says the turn was interrupted, mark the headline:
  "## Turn {N} digest — {topic} — interrupted". Do not describe the
  turn's work as complete.
- Stay under 1000 tokens.
