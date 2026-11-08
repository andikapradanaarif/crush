-- +goose Up
-- Worktree-aware capture + routed settlement (#360, R8): linked
-- worktrees of one repository share the project partition (the key
-- anchors on the git common dir), but evidence rows stored only
-- workspace-relative targets — a sibling-worktree edit recorded an
-- out-of-scope absolute path, and a shared data_directory let one
-- worktree's settle pass read another's checkout against the same
-- targets.
--
--   workdir   - the recording service's canonical workspace root.
--               Settle resolves each stored target against THIS dir,
--               not the settling service's — a row recorded by
--               worktree B reads B's checkout (or its repo-relative
--               fallback) no matter which pass freezes it. '' means
--               pre-column rows, which read under the settling dir
--               exactly as before.
--   repo_path - the target's path inside its own worktree, relative
--               to that worktree's root (the repo-layout-relative
--               form). It makes the file addressable after the
--               worktree is deleted: settle falls back to `git show
--               <default-branch>:<repo_path>` — merged work survives,
--               dropped work reads diverged. '' means no repo path
--               was provable at record time.
-- +goose StatementBegin
ALTER TABLE turn_outcomes ADD COLUMN workdir TEXT NOT NULL DEFAULT '';
ALTER TABLE referent_episodes ADD COLUMN workdir TEXT NOT NULL DEFAULT '';
ALTER TABLE referent_episodes ADD COLUMN repo_path TEXT NOT NULL DEFAULT '';
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE referent_episodes DROP COLUMN repo_path;
ALTER TABLE referent_episodes DROP COLUMN workdir;
ALTER TABLE turn_outcomes DROP COLUMN workdir;
-- +goose StatementEnd
