-- +goose Up
-- turn_outcomes hunk content (#394): the agent-vs-final substrate
-- the Phase-5 preference tier (#229) and real-failures->tasks (#399)
-- mine. Two half-written columns, one writer each:
--
--   hunk_added - record time: {path: {v, edits: [{old,new}], spans:
--   [{start,end,before,after}], state, truncated}}. The verbatim edit
--   pairs plus execution-time line spans derived from each tool
--   result's own before/after content (edit/multiedit metadata) or
--   diff (write metadata) -- never re-read from a workspace that may
--   already have moved on. before/after are the nearest non-blank
--   boundary lines, trimmed and scrubbed, used to re-anchor the span
--   after later changes.
--   hunk_after - settle time: {path: {v, state, windows:
--   [{start,text}], truncated, later_turn_touched, relocated,
--   drifted, stale}} - the post-window content around those spans,
--   redacted line-wise before slicing so a secret block cut in half
--   still leaks nothing. NOT automatically "the user's final edit":
--   settlement can run after later turns in the same project touched
--   the path, so windows may include later agent work --
--   later_turn_touched marks that. Non-content outcomes are explicit
--   states (missing, binary, unreadable, too_large, sensitive,
--   out_of_workspace, over_cap, region_unknown).
--
-- Both are bounded, redacted, project-local evidence -- nothing
-- syncs off-machine.
ALTER TABLE turn_outcomes ADD COLUMN hunk_added TEXT NOT NULL DEFAULT '{}';
ALTER TABLE turn_outcomes ADD COLUMN hunk_after TEXT NOT NULL DEFAULT '{}';

-- +goose Down
ALTER TABLE turn_outcomes DROP COLUMN hunk_after;
ALTER TABLE turn_outcomes DROP COLUMN hunk_added;
