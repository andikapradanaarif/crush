-- +goose Up
-- +goose StatementBegin
-- Artifact-based acceptance labels (#294): the verdict column is the
-- next user turn's cue-word judgment, which English regexes read
-- only partially. The label_* columns instead record what the
-- artifacts show — whether the edit survived, committed, shipped
-- green tests, and what the turn cost — so acceptance can be judged
-- from evidence rather than guessed from the follow-up's lexicon.
--
-- Tri-state convention: a NULL signal means "never observed" (no
-- repo, no test run, no baseline), which is honest absence — it must
-- never be read as a passing signal, matching the fail-closed rule
-- the learned-label pipeline applies elsewhere.
--
-- label_target_hash anchors the survival check: the SHA-256 of the
-- target file as the labeler first saw it — the post-edit state,
-- taken at episode-record time, not at edit time (anything that
-- happened between is part of the observation). An empty hash means
-- no baseline exists (file already gone, or the row predates this
-- migration) and survival can never be checked for that episode.
ALTER TABLE referent_episodes ADD COLUMN label_target_hash TEXT NOT NULL DEFAULT '';
-- label_hash_changed: 1 once the file's current hash differs from
-- the baseline — the edit (or anything later) no longer survives
-- intact. 0 while it still matches. NULL before the first check.
ALTER TABLE referent_episodes ADD COLUMN label_hash_changed INTEGER;
-- label_committed: 1 once git shows a commit newer than created_at
-- touching the target — the strongest survival signal, monotone once
-- observed. 0 while unobserved. NULL outside a repository.
ALTER TABLE referent_episodes ADD COLUMN label_committed INTEGER;
-- label_tests_green: the judged session's last-run kind='test'
-- command verdicts at label time — 1 when every observed test run
-- ended clean, 0 when any failed, NULL when the session ran no test
-- the ledger recognized (unobserved, not green).
ALTER TABLE referent_episodes ADD COLUMN label_tests_green INTEGER;
-- label_wrong_target: mutating edits in the judged turn that fell
-- outside the episode's admitted referent. Always 0 today — the
-- episode writer abstains on multi-target turns — recorded so the
-- signal exists the day that rule relaxes.
ALTER TABLE referent_episodes ADD COLUMN label_wrong_target INTEGER NOT NULL DEFAULT 0;
-- label_steps / label_tokens: what the judged turn cost — tool calls
-- issued and session request tokens spent through the label pass.
-- Cost is part of acceptance: a correct edit that took thirty turns
-- is not the outcome memory exists to produce.
ALTER TABLE referent_episodes ADD COLUMN label_steps INTEGER NOT NULL DEFAULT 0;
ALTER TABLE referent_episodes ADD COLUMN label_tokens INTEGER NOT NULL DEFAULT 0;
-- labeled_at: when the label row was last stamped — the freshness
-- anchor for the maturity pass's re-check cadence.
ALTER TABLE referent_episodes ADD COLUMN labeled_at INTEGER NOT NULL DEFAULT 0;

CREATE INDEX referent_episodes_label_pending
    ON referent_episodes (project_key, labeled_at);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX referent_episodes_label_pending;
ALTER TABLE referent_episodes DROP COLUMN labeled_at;
ALTER TABLE referent_episodes DROP COLUMN label_tokens;
ALTER TABLE referent_episodes DROP COLUMN label_steps;
ALTER TABLE referent_episodes DROP COLUMN label_wrong_target;
ALTER TABLE referent_episodes DROP COLUMN label_tests_green;
ALTER TABLE referent_episodes DROP COLUMN label_committed;
ALTER TABLE referent_episodes DROP COLUMN label_hash_changed;
ALTER TABLE referent_episodes DROP COLUMN label_target_hash;
-- +goose StatementEnd
