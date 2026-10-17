-- +goose Up
-- Per-(session, command) last-verdict history (#357).
-- command_memory's last_session_id is last-writer: a session whose
-- test commands were all re-run elsewhere loses its kind='test'
-- evidence entirely and settles unknown instead of failed. This
-- table keeps the session's own last verdict per command — the same
-- CASE-guarded last-exit semantics as the ledger, partitioned by
-- session. project_key stays key material so a shared data store
-- never merges two projects' histories into one row, and so the
-- sibling-partition reclaim can re-key it like the other tables.
CREATE TABLE IF NOT EXISTS session_command_runs (
    session_id    TEXT NOT NULL,
    cmd_norm      TEXT NOT NULL,
    cwd           TEXT NOT NULL DEFAULT '',
    kind          TEXT NOT NULL DEFAULT 'other',
    last_exit     INTEGER NOT NULL,
    last_at       INTEGER NOT NULL,
    project_key   TEXT NOT NULL DEFAULT '',
    param_version TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (session_id, cmd_norm, cwd, project_key)
) WITHOUT ROWID;
CREATE INDEX IF NOT EXISTS idx_session_command_runs_key
    ON session_command_runs(project_key, session_id);

-- +goose Down
DROP TABLE IF EXISTS session_command_runs;
