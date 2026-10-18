-- +goose Up
-- turn_outcomes hunk content (#394): the agent-vs-final substrate
-- the Phase-5 preference tier (#229) and real-failures->tasks (#399)
-- mine. Two half-written columns, one writer each:
--
--   hunk_added — record time: {path: {edits: [{old, new}], spans:
--   [[start,end]], state, truncated}} — the verbatim edit pairs and
--   the line spans the new text occupies NOW, so settle can slice
--   the region even after it is fully rewritten.
--   hunk_after — settle time: {path: {state, windows, truncated,
--   later_turn_touched}} — the post-window content around those
--   spans. NOT automatically "the user's final edit": settlement
--   can run after later same-session turns touched the path, so
--   windows may include later agent work — later_turn_touched marks
--   that. Non-content outcomes are explicit states (missing,
--   binary, unreadable, too_large, sensitive, out_of_workspace,
--   over_cap, region_unknown).
--
-- Both are bounded, redacted, project-local evidence — nothing
-- syncs off-machine.
ALTER TABLE turn_outcomes ADD COLUMN hunk_added TEXT NOT NULL DEFAULT '{}';
ALTER TABLE turn_outcomes ADD COLUMN hunk_after TEXT NOT NULL DEFAULT '{}';

-- +goose Down
ALTER TABLE turn_outcomes DROP COLUMN hunk_after;
ALTER TABLE turn_outcomes DROP COLUMN hunk_added;
