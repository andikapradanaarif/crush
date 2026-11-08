// Worktree-aware capture and routed settlement (#360, R8).
//
// Linked worktrees of one repository share the project partition —
// project_key anchors on the git common dir — but before this file
// the evidence gates scoped every read to the service's own
// workspace root. An edit the agent made inside a sibling worktree
// (the `../<slug>` workflow) recorded an absolute target the scope
// gate refused, so hunk cells stored out_of_workspace forever and
// the survival check abstained; and under a shared data_directory,
// a row recorded by worktree B could settle in worktree A's pass
// against A's checkout of the same relative path.
//
// The fix has three pieces:
//
//   - Capture roots: the in-scope set is every worktree root of the
//     repository (the main checkout plus each linked worktree) —
//     discovered per pass via `git worktree list`, because agents
//     create worktrees mid-session. Anything under no root stays
//     out of scope; the sensitive-path gate is unchanged.
//   - Provenance stamps: rows carry the recording service's
//     workdir, and each in-scope hunk cell carries the target's
//     repo-relative path inside its own worktree. Settlement
//     resolves stored targets against the stamped workdir — never
//     the settling service's — so a shared store routes reads to
//     the checkout the evidence was written under.
//   - Merged-worktree fallback: when the recorded location no
//     longer reads (worktree removed), the repo-relative path is
//     looked up on the default branch — `git show
//     <default>:<repo_path>`. Merged work survives there; dropped
//     work reads diverged, which is the honest verdict: the change
//     exists nowhere durable.
package cmdlog

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/charmbracelet/crush/internal/filepathext"
)

// captureRoots lists the in-scope read roots for one pass: every
// worktree root of this repository, canonicalized. The main
// checkout's root comes from the same listing, so a workspace rooted
// in a repo subdirectory is scoped at worktree granularity like the
// siblings — symmetric roots keep a sibling directory of the
// workspace inside the checkout readable, matching the worktree
// semantics. Outside a repository (or when git fails) the set is
// the workspace alone — pre-worktree behavior.
func (s *service) captureRoots(ctx context.Context) []string {
	if !s.hasRepo {
		return []string{s.workingDir}
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	raw, err := gitOut(ctx, s.workingDir, "worktree", "list", "--porcelain")
	if err != nil || raw == "" {
		return []string{s.workingDir}
	}
	var roots []string
	for line := range strings.SplitSeq(raw, "\n") {
		p, ok := strings.CutPrefix(line, "worktree ")
		if !ok {
			continue
		}
		roots = append(roots, filepathext.Canonical(filepath.Clean(p)))
	}
	if len(roots) == 0 {
		return []string{s.workingDir}
	}
	return roots
}

// resolveTarget maps a stored target to its location: the capture
// root containing it plus the path inside that root. base is the
// recorded workdir for relative targets; absolute targets resolve
// verbatim (relWorkdir stores sibling-worktree paths absolute).
// Containment is canonical-path prefix matching — EvalSymlinks
// where the target exists, textual where it does not (a path under
// a removed worktree still resolves for the repo-path fallback).
// ok=false means the target sits under no capture root.
func resolveTarget(target, base string, roots []string) (root, sub, abs string, ok bool) {
	if filepath.IsAbs(target) {
		abs = filepath.Clean(filepath.FromSlash(target))
	} else {
		if base == "" {
			return "", "", "", false
		}
		abs = filepath.Clean(filepath.Join(base, filepath.FromSlash(target)))
	}
	// Canonical resolves the deepest existing ancestor and rejoins
	// the tail — a not-yet-created file still lands in its true
	// root, and a path under a removed worktree still resolves
	// textually for the repo-path fallback.
	abs = filepathext.Canonical(abs)
	// Longest-prefix containment: nested worktree roots pick the
	// most specific one.
	for _, r := range roots {
		if abs != r && !strings.HasPrefix(abs, r+string(filepath.Separator)) {
			continue
		}
		if len(r) > len(root) {
			root = r
		}
	}
	if root == "" {
		return "", "", abs, false
	}
	rel, err := filepath.Rel(root, abs)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", "", abs, false
	}
	return root, rel, abs, true
}

// repoPath is the target's position inside its repository — the path
// relative to its own worktree root, slash-normalized. Stored at
// record time so a later pass can address the file after the
// worktree is gone; also the commit-evidence keyspace, since `git
// log --name-only` emits repo-relative paths.
func repoPath(sub string) string {
	return filepath.ToSlash(sub)
}

// targetRepoPath computes one target's repo-relative form at record
// time — "" when the target resolves under no capture root. One
// resolution per record keeps the stamp honest: it is provable only
// while the file's worktree still exists.
func (s *service) targetRepoPath(ctx context.Context, target string) string {
	_, sub, _, ok := resolveTarget(target, s.workingDir, s.captureRoots(ctx))
	if !ok {
		return ""
	}
	return repoPath(sub)
}

// readCaptureFile reads sub under the containing worktree root —
// os.Root keeps the read symlink-confined to that root the same way
// readWorkspaceFile confines to the workspace.
func readCaptureFile(root, sub string) ([]byte, bool) {
	r, err := os.OpenRoot(root)
	if err != nil {
		return nil, false
	}
	defer r.Close()
	data, err := r.ReadFile(sub)
	if err != nil {
		return nil, false
	}
	return data, true
}

// defaultBranchRef names the ref settle reads merged worktree
// content from — origin/HEAD when the remote tracks one, else the
// local main/master, else nothing (no fallback evidence).
func (s *service) defaultBranchRef(ctx context.Context) string {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if ref, err := gitOut(ctx, s.workingDir,
		"symbolic-ref", "--short", "refs/remotes/origin/HEAD"); err == nil && ref != "" {
		return ref
	}
	for _, br := range []string{"main", "master"} {
		if _, err := gitOut(ctx, s.workingDir,
			"rev-parse", "--verify", "--quiet", "refs/heads/"+br); err == nil {
			return br
		}
	}
	return ""
}

// mergedFileContent reads repoPath at the repository's default
// branch — the merged-worktree fallback: when the recorded checkout
// no longer reads, the work counts only if it landed somewhere
// durable. A deleted-unmerged worktree produces no content and the
// caller's missing-state decides.
func (s *service) mergedFileContent(ctx context.Context, path string) ([]byte, bool) {
	if !s.hasRepo || path == "" {
		return nil, false
	}
	ref := s.defaultBranchRef(ctx)
	if ref == "" {
		return nil, false
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, err := gitOut(ctx, s.workingDir, "show", ref+":"+path)
	if err != nil {
		return nil, false
	}
	return []byte(out), true
}

// readCaptureData returns file content for the verdict/capture pair:
// the target under its containing worktree root when that reads,
// else the merged default-branch content at its repo path. merged
// marks the second source so cells can flag provenance. root=""
// skips the disk arm — a target under no live root whose only
// remaining address is the repo path.
func (s *service) readCaptureData(ctx context.Context, root, sub, rp string) (data []byte, merged, ok bool) {
	if root != "" {
		if data, ok := readCaptureFile(root, sub); ok {
			return data, false, true
		}
	}
	if rp != "" {
		if data, ok := s.mergedFileContent(ctx, rp); ok {
			return data, true, true
		}
	}
	return nil, false, false
}
