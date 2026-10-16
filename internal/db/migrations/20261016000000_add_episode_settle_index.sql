-- +goose Up
-- +goose StatementBegin
-- Backing index for ListUnsettledReferentLabels (#355): the settle
-- scan filters (project_key, settled_at IS NULL) on every detached
-- per-turn pass, and without an index a mature episodes table is
-- scanned per call. Mirrors turn_outcomes_settle (#340), which the
-- same pass's turn-side query already uses.
CREATE INDEX IF NOT EXISTS referent_episodes_settle
    ON referent_episodes (project_key, settled_at);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS referent_episodes_settle;
-- +goose StatementEnd
