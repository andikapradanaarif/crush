-- +goose Up
-- +goose StatementBegin
-- Referent memory (#165): the "smarter every time" substrate — learn
-- that this user's "the config" means internal/config/config.go.
-- Two tables split observation from promotion: referent_episodes is
-- the raw event log (a vague prompt whose turn the agent resolved to
-- a file edit, judged by the next user turn), and referent_memory
-- holds the promoted candidates that earn injection. Episodes carry
-- the same per-observation provenance as the other memory rows
-- (#220): the session and mutating tool call that produced the
-- observation, the repo state it was true of, whether the edit was
-- itself memory-suggested (echo — must not count as independent
-- evidence), the project partition, and the params snapshot in force.

CREATE TABLE referent_episodes (
    id               INTEGER PRIMARY KEY AUTOINCREMENT,
    phrase           TEXT NOT NULL,
    target           TEXT NOT NULL,
    session_id       TEXT NOT NULL,
    -- The vague user message the episode stems from — also the
    -- idempotency key: repair retries re-run the turn-context pass,
    -- and INSERT OR IGNORE on this unique pair makes a re-emit a
    -- no-op instead of a duplicate observation.
    source_message_id TEXT NOT NULL,
    tool_call_id     TEXT NOT NULL DEFAULT '',
    repo_state       TEXT NOT NULL DEFAULT '',
    -- The contamination flag: 1 when the rendered tail itself pointed
    -- the agent at this target, so the episode is echo, not evidence.
    memory_suggested INTEGER NOT NULL DEFAULT 0,
    -- How the next user turn judged the edit: accepted (no revision
    -- signal) or revised (correction cue). Episodes only insert once
    -- a verdict exists — a session's final turn is never judged and
    -- never recorded, which is honest: it could not promote anyway.
    verdict          TEXT NOT NULL,
    project_key      TEXT NOT NULL DEFAULT '',
    param_version    TEXT NOT NULL DEFAULT '',
    created_at       INTEGER NOT NULL,
    UNIQUE (session_id, source_message_id, target)
);
CREATE INDEX referent_episodes_promotion
    ON referent_episodes (project_key, phrase, target, verdict, memory_suggested);

CREATE TABLE referent_memory (
    phrase        TEXT NOT NULL,
    target        TEXT NOT NULL,
    -- Clean accepted evidence count — distinct sessions only: the
    -- same session re-deriving the same mapping is one observation
    -- repeated, not two.
    hits          INTEGER NOT NULL DEFAULT 0,
    last_at       INTEGER NOT NULL,
    project_key   TEXT NOT NULL DEFAULT '',
    param_version TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (phrase, target, project_key)
);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE referent_memory;
DROP TABLE referent_episodes;
-- +goose StatementEnd
