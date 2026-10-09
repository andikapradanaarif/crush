-- +goose Up
-- +goose StatementBegin
-- Session digests (#164): cross-session retrieval — "continue", "what
-- did we do yesterday", "the login thing". One row per session holds
-- the resumable summary (title + latest checkpoint + touched files);
-- the FTS5 shadow indexes the searchable text so paraphrased prompts
-- match. Pointers only by contract — the digest carries paths, titles,
-- and dates, never unverifiable narrative.
--
-- Same provenance pack as the other memory rows (#220): project_key
-- partitions, param_version stamps the parameter set in force.

CREATE TABLE session_digests (
    session_id  TEXT NOT NULL PRIMARY KEY,
    title       TEXT NOT NULL DEFAULT '',
    -- The latest consolidated checkpoint text where one exists, else
    -- empty — the digest falls back to title + files alone.
    checkpoint  TEXT NOT NULL DEFAULT '',
    -- Newline-joined workspace-relative paths (filetracker read/write
    -- union). Plain text so FTS5 tokenizes path segments.
    files       TEXT NOT NULL DEFAULT '',
    ended_at    INTEGER NOT NULL,
    project_key TEXT NOT NULL DEFAULT '',
    param_version TEXT NOT NULL DEFAULT ''
);
CREATE INDEX session_digests_project ON session_digests (project_key, ended_at DESC);

-- FTS5 shadow over the digest's searchable text. session_id is
-- UNINDEXED (join key only); body carries title + checkpoint + files
-- as one tokenized blob. A regular FTS5 table — not external-content —
-- so digest refresh is a plain DELETE+INSERT and no 'delete' command
-- or rowid mirroring is involved. The small text duplication is the
-- price of that simplicity.
CREATE VIRTUAL TABLE session_digests_fts USING fts5(
    session_id UNINDEXED, body
);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE session_digests_fts;
DROP TABLE session_digests;
-- +goose StatementEnd
