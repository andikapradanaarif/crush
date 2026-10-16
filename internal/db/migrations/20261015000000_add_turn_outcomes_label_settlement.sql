-- +goose Up
-- +goose StatementBegin
-- Label settlement (#340): the artifact labels feeding the rate fold
-- and the calibrator re-derived under an unbounded window — commits
-- scanned --since with no --until, whole-file hashes re-checked every
-- pass, and the agent's own commits counted as acceptance. Settlement
-- fixes that: each episode label freezes once at a fixed observation
-- window (next session start or +24h, whichever first), survival is
-- checked per added hunk rather than per whole file, and commits the
-- agent itself ran are excluded.
--
-- label_added_lines: JSON array of the normalized lines the judged
-- edit introduced (the episode's survival fingerprint). Empty for
-- rows recorded before this column existed — those settle without a
-- survival check (honest degradation, not a whole-file fallback).
ALTER TABLE referent_episodes ADD COLUMN label_added_lines TEXT NOT NULL DEFAULT '';
-- settle_at: the outer edge of the observation window in millis —
-- created_at + 24h, stamped at insert and re-stamped at label time
-- (the default 0 only marks pre-existing rows due immediately; a row
-- born with 0 would satisfy the due predicate before its label
-- landed). A newer session provably in the project (sessions carry
-- no project column; membership is a project-keyed ledger row)
-- tightens the effective window at settle time; settle_at is the
-- cap that makes long-idle projects' rows still terminate.
ALTER TABLE referent_episodes ADD COLUMN settle_at INTEGER NOT NULL DEFAULT 0;
-- settled_at: when the label froze. NULL = unsettled; once set the
-- committed/hash_changed/tests fields stop re-deriving — the fold
-- and the calibrator read only settled rows, so "certified on what
-- it gates on" holds across time.
ALTER TABLE referent_episodes ADD COLUMN settled_at INTEGER;

-- turn_outcomes (#340): the acceptance label at the unit procedural
-- memory mines — one row per (session, user-turn). referent_episodes
-- records only vague-prompt single-edit turns, a shape real usage
-- rarely takes; this table labels every turn that produced edits or
-- commands, so the mining census has an input that exists.
--
-- Settlement mirrors episodes: the row opens when the turn's next
-- user message arrives (or a later backfill of stored messages),
-- carries the turn's edited paths, added-line fingerprints per file,
-- and normalized commands, then settles once at its window close.
CREATE TABLE turn_outcomes (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    session_id    TEXT NOT NULL,
    turn_id       TEXT NOT NULL,
    project_key   TEXT NOT NULL,
    edited_paths  TEXT NOT NULL DEFAULT '[]',
    edit_hunks    TEXT NOT NULL DEFAULT '{}',
    commands      TEXT NOT NULL DEFAULT '[]',
    next_verdict  TEXT NOT NULL DEFAULT '',
    verdict       TEXT,
    settle_at     INTEGER NOT NULL,
    settled_at    INTEGER,
    param_version TEXT NOT NULL DEFAULT '',
    created_at    INTEGER NOT NULL,
    UNIQUE(session_id, turn_id)
);
CREATE INDEX turn_outcomes_settle
    ON turn_outcomes (project_key, settled_at);

-- Session-membership probes (#340): the settle triggers and the
-- backfill corpus run correlated EXISTS over these columns once per
-- candidate session — without indexes a mature ledger scans tables
-- per probe.
CREATE INDEX IF NOT EXISTS idx_command_memory_last_session
    ON command_memory (last_session_id);
CREATE INDEX IF NOT EXISTS idx_failure_memory_session
    ON failure_memory (session_id);
CREATE INDEX IF NOT EXISTS idx_referent_episodes_session
    ON referent_episodes (session_id);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS idx_referent_episodes_session;
DROP INDEX IF EXISTS idx_failure_memory_session;
DROP INDEX IF EXISTS idx_command_memory_last_session;
DROP INDEX turn_outcomes_settle;
DROP TABLE turn_outcomes;
ALTER TABLE referent_episodes DROP COLUMN settled_at;
ALTER TABLE referent_episodes DROP COLUMN settle_at;
ALTER TABLE referent_episodes DROP COLUMN label_added_lines;
-- +goose StatementEnd
