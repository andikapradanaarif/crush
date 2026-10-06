-- +goose Up
-- +goose StatementBegin
-- Per-observation provenance (#220): every memory row records who
-- observed it (session + tool call), the repo state it was true of,
-- whether the observed action was itself memory-suggested (the
-- contamination-screen flag — memory-recommended actions must not
-- feed back as independent evidence), and which partition the row
-- belongs to. project_key is the stable repo identity — canonical
-- git common-dir plus normalized remote URL when one exists — so a
-- candidate whose project_key differs from the current partition is
-- inadmissible as a join condition, never a ranking signal.
-- param_version reserves the learned-params snapshot field (#228);
-- rows carry it empty until the params substrate exists.

ALTER TABLE failure_memory ADD COLUMN session_id TEXT NOT NULL DEFAULT '';
ALTER TABLE failure_memory ADD COLUMN tool_call_id TEXT NOT NULL DEFAULT '';
ALTER TABLE failure_memory ADD COLUMN repo_state TEXT NOT NULL DEFAULT '';
ALTER TABLE failure_memory ADD COLUMN suggested INTEGER NOT NULL DEFAULT 0;
ALTER TABLE failure_memory ADD COLUMN project_key TEXT NOT NULL DEFAULT '';
ALTER TABLE failure_memory ADD COLUMN param_version TEXT NOT NULL DEFAULT '';
-- resolved_in already names the resolving session; resolved_call
-- names the call that carried the resolving verdict, where
-- detectable.
ALTER TABLE failure_memory ADD COLUMN resolved_call TEXT NOT NULL DEFAULT '';

-- command_memory's partition is key material: a store shared across
-- projects (an absolute data_directory) must not merge two projects'
-- tallies into one row, so project_key joins the primary key.
-- SQLite cannot alter a primary key — recreate and copy.
CREATE TABLE command_memory_new (
    cmd_norm          TEXT NOT NULL,
    cwd               TEXT NOT NULL DEFAULT '',
    kind              TEXT NOT NULL DEFAULT 'other',
    last_exit         INTEGER NOT NULL,
    last_at           INTEGER NOT NULL,
    ok_count          INTEGER NOT NULL DEFAULT 0,
    fail_count        INTEGER NOT NULL DEFAULT 0,
    last_session_id   TEXT NOT NULL DEFAULT '',
    last_tool_call_id TEXT NOT NULL DEFAULT '',
    repo_state        TEXT NOT NULL DEFAULT '',
    suggested         INTEGER NOT NULL DEFAULT 0,
    project_key       TEXT NOT NULL DEFAULT '',
    param_version     TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (cmd_norm, cwd, project_key)
);
INSERT INTO command_memory_new (
    cmd_norm, cwd, kind, last_exit, last_at,
    ok_count, fail_count, last_session_id
)
SELECT cmd_norm, cwd, kind, last_exit, last_at,
    ok_count, fail_count, last_session_id
FROM command_memory;
DROP TABLE command_memory;
ALTER TABLE command_memory_new RENAME TO command_memory;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE failure_memory DROP COLUMN session_id;
ALTER TABLE failure_memory DROP COLUMN tool_call_id;
ALTER TABLE failure_memory DROP COLUMN repo_state;
ALTER TABLE failure_memory DROP COLUMN suggested;
ALTER TABLE failure_memory DROP COLUMN project_key;
ALTER TABLE failure_memory DROP COLUMN param_version;
ALTER TABLE failure_memory DROP COLUMN resolved_call;

CREATE TABLE command_memory_new (
    cmd_norm        TEXT NOT NULL,
    cwd             TEXT NOT NULL DEFAULT '',
    kind            TEXT NOT NULL DEFAULT 'other',
    last_exit       INTEGER NOT NULL,
    last_at         INTEGER NOT NULL,
    ok_count        INTEGER NOT NULL DEFAULT 0,
    fail_count      INTEGER NOT NULL DEFAULT 0,
    last_session_id TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (cmd_norm, cwd)
);
INSERT INTO command_memory_new (
    cmd_norm, cwd, kind, last_exit, last_at,
    ok_count, fail_count, last_session_id
)
SELECT cmd_norm, cwd, kind, last_exit, last_at,
    ok_count, fail_count, last_session_id
FROM command_memory
GROUP BY cmd_norm, cwd;
DROP TABLE command_memory;
ALTER TABLE command_memory_new RENAME TO command_memory;
-- +goose StatementEnd
