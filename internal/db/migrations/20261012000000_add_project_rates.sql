-- +goose Up
-- +goose StatementBegin
-- project_rates: per-(project, signal) Beta-Binomial posterior mass
-- (#296, learning layer B). alpha/beta are the accumulated counts —
-- DECAYED, not raw: a session boundary multiplies both by γ before
-- folding that session's outcomes, so the stored pair is an
-- effective sample size that fades ~5% per evidence-bearing session.
-- last_event_id is the fold cursor — rows the outcome stream
-- produced after it are uncounted evidence.
CREATE TABLE project_rates (
    project_key   TEXT NOT NULL,
    signal        TEXT NOT NULL,
    alpha         REAL NOT NULL DEFAULT 0,
    beta          REAL NOT NULL DEFAULT 0,
    last_event_id INTEGER NOT NULL DEFAULT 0,
    -- The session the last fold ended in — a new batch that opens
    -- in a different session must still discount once, or
    -- cross-batch boundaries silently skip decay.
    last_session_id TEXT NOT NULL DEFAULT '',
    updated_at    INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (project_key, signal)
);
-- +goose StatementEnd

-- rate_folded on referent_episodes is the fold's membership mark,
-- NOT an id watermark: the settled predicate can become true only
-- after labels mature, so a row that couldn't settle when higher
-- ids folded must still count when it does. 0 means never folded.
ALTER TABLE referent_episodes ADD COLUMN rate_folded INTEGER NOT NULL DEFAULT 0;
CREATE INDEX referent_episodes_rate_fold
    ON referent_episodes (project_key, rate_folded, id);

-- +goose Down
-- +goose StatementBegin
DROP INDEX referent_episodes_rate_fold;
ALTER TABLE referent_episodes DROP COLUMN rate_folded;
DROP TABLE project_rates;
-- +goose StatementEnd
