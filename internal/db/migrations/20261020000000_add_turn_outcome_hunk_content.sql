-- +goose Up
-- turn_outcomes hunk content (#394): the agent-vs-final substrate
-- the Phase-5 preference tier (#229) and real-failures→tasks (#399)
-- mine. Two half-written columns, one writer each:
--
--   hunk_added — record time: the raw text the turn's edits wrote,
--   per workspace-relative path (the edit's new_string / the write's
--   content, concatenated). Bounded and redacted at write.
--   hunk_after — settle time: the post-window content of the same
--   paths, so (agent output, what the region became) pairs can be
--   reconstructed without re-reading the file. Bounded, redacted,
--   and state-marked (missing/binary/out_of_workspace/over_cap)
--   because a captured file may be gone or unreadable by then.
--
-- Both are project-local evidence — nothing syncs off-machine.
ALTER TABLE turn_outcomes ADD COLUMN hunk_added TEXT NOT NULL DEFAULT '{}';
ALTER TABLE turn_outcomes ADD COLUMN hunk_after TEXT NOT NULL DEFAULT '{}';

-- +goose Down
ALTER TABLE turn_outcomes DROP COLUMN hunk_after;
ALTER TABLE turn_outcomes DROP COLUMN hunk_added;
