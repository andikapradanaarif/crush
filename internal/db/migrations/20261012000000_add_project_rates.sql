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

-- +goose Down
-- +goose StatementBegin
DROP TABLE project_rates;
-- +goose StatementEnd
