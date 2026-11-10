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
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/crush/internal/filepathext"
)

// captureView is one pass's capture surface: the repo's worktree
// roots and the main checkout's branch — both from the same
// `git worktree list` probe — plus a memo slot for the default
// branch ref. One view per record or settle pass means a batch of
// merged-fallback reads resolves the ref once instead of paying
// the symbolic-ref probe per missing path per row.
type captureView struct {
	roots      []string
	mainBranch string
	ref        string
	refSet     bool
	// foreignRepo memoizes each out-of-scope ancestor dir's own git
	// common-dir ("" = under no repository) — the cross_repo
	// sub-state's probe (#466). One entry per edited directory, not
	// per path; the memo mutates, which is safe only because the
	// record and settle passes run their path loops sequentially.
	foreignRepo map[string]string
}

// captureView lists the in-scope read roots for one pass: every
// worktree root of this repository, canonicalized. The main
// checkout's root comes from the same listing, so a workspace rooted
// in a repo subdirectory is scoped at worktree granularity like the
// siblings — symmetric roots keep a sibling directory of the
// workspace inside the checkout readable, matching the worktree
// semantics. Outside a repository (or when git fails) the set is
// the workspace alone — pre-worktree behavior. The listing's first
// block is the main checkout; its branch rides along as the
// default-ref fallback for repos whose default is neither
// origin/HEAD nor main/master (trunk, develop, ...).
func (s *service) captureView(ctx context.Context) *captureView {
	v := &captureView{roots: []string{s.workingDir}}
	if !s.hasRepo {
		return v
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	raw, err := gitOut(ctx, s.workingDir, "worktree", "list", "--porcelain")
	if err != nil || raw == "" {
		return v
	}
	var roots []string
	block := 0
	for line := range strings.SplitSeq(raw, "\n") {
		if p, ok := strings.CutPrefix(line, "worktree "); ok {
			block++
			roots = append(roots, filepathext.Canonical(filepath.Clean(p)))
			continue
		}
		if block == 1 && v.mainBranch == "" {
			if b, ok := strings.CutPrefix(line, "branch refs/heads/"); ok {
				v.mainBranch = b
			}
		}
	}
	if len(roots) > 0 {
		v.roots = roots
	}
	return v
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

// outOfScopeState names why a target resolved under no capture root
// (#466): cross_repo when the path's own repository is provable and
// differs from the session's — a designed abstention, since that
// repo's partition owns its evidence — while out_of_workspace keeps
// meaning "under no repository we can prove". The same-repo case
// (a path inside the session's own repo that missed the roots
// listing — a worktree born between view and record) keeps
// out_of_workspace: it is the same abstention, not a foreign claim.
// The probe runs on the deepest existing ancestor — a planned write
// or a removed worktree leaves no dir to ask — and memoizes per dir,
// so a batch of paths under one foreign tree probes once.
func (s *service) outOfScopeState(ctx context.Context, v *captureView, abs string) string {
	if abs == "" {
		// An unresolvable target — a relative path on a row with
		// no stamped workdir — is under no provable repository.
		// Without this guard the walk lands on "." and probes the
		// process CWD, which knows nothing about the row.
		return hunkOutOfWorkspace
	}
	dir := abs
	for {
		if st, err := os.Stat(dir); err == nil {
			if !st.IsDir() {
				dir = filepath.Dir(dir)
			}
			break
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return hunkOutOfWorkspace
		}
		dir = parent
	}
	if v.foreignRepo == nil {
		v.foreignRepo = map[string]string{}
	}
	common, probed := v.foreignRepo[dir]
	if !probed {
		pctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		out, err := gitOut(pctx, dir, "rev-parse", "--git-common-dir")
		cancel()
		if err == nil && out != "" {
			if !filepath.IsAbs(out) {
				out = filepath.Join(dir, out)
			}
			common = filepathext.Canonical(out)
		}
		v.foreignRepo[dir] = common
	}
	if common != "" && common != s.commonDir {
		return hunkCrossRepo
	}
	return hunkOutOfWorkspace
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
// while the file's worktree still exists. Sensitive targets store
// no address, the same asymmetry hunk cells keep: git is not a
// back door around the sensitive gate.
func (s *service) targetRepoPath(ctx context.Context, v *captureView, target string) string {
	if sensitivePath(target) {
		return ""
	}
	_, sub, _, ok := resolveTarget(target, s.workingDir, v.roots)
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

// defaultRef resolves the ref merged-fallback reads come from,
// memoized on the view: origin/HEAD when the remote tracks one,
// else local main/master, else the main checkout's branch — the
// trunk/develop case. A repo with none of those gets no fallback
// (the honest absent arm), and the probe runs at most once per
// pass no matter how many paths miss.
func (s *service) defaultRef(ctx context.Context, v *captureView) string {
	if v.refSet {
		return v.ref
	}
	v.refSet = true
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if ref, err := gitOut(ctx, s.workingDir,
		"symbolic-ref", "--short", "refs/remotes/origin/HEAD"); err == nil && ref != "" {
		v.ref = ref
		return v.ref
	}
	for _, br := range []string{"main", "master"} {
		if _, err := gitOut(ctx, s.workingDir,
			"rev-parse", "--verify", "--quiet", "refs/heads/"+br); err == nil {
			v.ref = br
			return v.ref
		}
	}
	v.ref = v.mainBranch
	return v.ref
}

// mergedRead fetches path's content at the repository's default
// branch — the merged-worktree fallback (R8): when the recorded
// checkout no longer reads, the work counts only if it landed
// somewhere durable. bounded applies the MaxHunkReadBytes stat
// gate via cat-file -s before the content read — the capture arm
// passes it so a giant blob pays no scan, matching the fs arm's
// stat-before-read; the verdict arm skips it, matching the fs
// arm's read-anything-readable parity. The returned state names
// the failure (missing, too_large, sensitive) so the capture cell
// records why rather than guessing missing. Raw cat-file, never
// the trimmed gitOut: hashing and line numbering run on these
// bytes, and `cat-file blob` refuses non-blob specs where
// `git show` would emit a tree listing as if it were content.
func (s *service) mergedRead(ctx context.Context, v *captureView, path string, bounded bool) ([]byte, string, bool) {
	switch {
	case path == "":
		return nil, hunkMissing, false
	case sensitivePath(path):
		// No repo-relative address is stamped for sensitive
		// targets either — this path only arrives pre-migration
		// or via the computed-sub arm, and refuses the same way.
		return nil, hunkSensitive, false
	case !s.hasRepo:
		return nil, hunkMissing, false
	}
	ref := s.defaultRef(ctx, v)
	if ref == "" {
		return nil, hunkMissing, false
	}
	spec := ref + ":" + path
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if bounded {
		sz, err := gitOut(ctx, s.workingDir, "cat-file", "-s", spec)
		if err != nil {
			return nil, hunkMissing, false
		}
		if n, perr := strconv.ParseInt(sz, 10, 64); perr != nil || n > MaxHunkReadBytes {
			return nil, hunkTooLarge, false
		}
	}
	out, err := gitOutRaw(ctx, s.workingDir, "cat-file", "blob", spec)
	if err != nil {
		return nil, hunkMissing, false
	}
	return out, hunkOK, true
}

// readCaptureData returns file content for the verdict pair: the
// target under its containing worktree root when that reads, else
// the merged default-branch content at its repo path. merged marks
// the second source so cells can flag provenance. root="" skips
// the disk arm — a target under no live root whose only remaining
// address is the repo path. The merged arm reads unbounded, parity
// with readCaptureFile: the verdict checks any readable file; the
// capture path applies the size gate itself via mergedRead.
func (s *service) readCaptureData(ctx context.Context, v *captureView, root, sub, rp string) (data []byte, merged, ok bool) {
	if root != "" {
		if data, ok := readCaptureFile(root, sub); ok {
			return data, false, true
		}
	}
	if rp != "" {
		if data, _, ok := s.mergedRead(ctx, v, rp, false); ok {
			return data, true, true
		}
	}
	return nil, false, false
}
