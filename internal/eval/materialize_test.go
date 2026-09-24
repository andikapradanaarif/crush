package eval

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestEnsureRepoMirror exercises the mirror cache offline — a local
// repo stands in for the remote (the mirror path is identical; only
// the caller filters local repos). First call clones, second fetches
// and reuses, and the shared-clone consumer resolves a pinned ref.
func TestEnsureRepoMirror(t *testing.T) {
	src := t.TempDir()
	git := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.CommandContext(t.Context(), "git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, "git %v: %s", args, out)
	}
	git(src, "init", "--quiet")
	require.NoError(t, os.WriteFile(filepath.Join(src, "f.txt"), []byte("v1"), 0o644))
	git(src, "add", ".")
	git(src, "commit", "--quiet", "-m", "init")

	t.Setenv(EvalRepoCacheEnvVar, t.TempDir())

	mirror, err := ensureRepoMirror(t.Context(), src)
	require.NoError(t, err)
	require.DirExists(t, mirror)

	// Second call is the cache-hit path: same mirror, refreshed via
	// fetch — and still answers rev-parse for the pinned commit.
	mirror2, err := ensureRepoMirror(t.Context(), src)
	require.NoError(t, err)
	require.Equal(t, mirror, mirror2)

	headOut, err := exec.CommandContext(t.Context(), "git", "-C", src, "rev-parse", "HEAD").CombinedOutput()
	require.NoError(t, err)
	sha := string(headOut[:len(headOut)-1])
	out, err := exec.CommandContext(t.Context(), "git", "-C", mirror, "rev-parse", sha).CombinedOutput()
	require.NoError(t, err, "%s", out)

	// The consumer half: a --shared clone of the mirror checks out the
	// pinned ref without touching the source again.
	dest := filepath.Join(t.TempDir(), "wt")
	out, err = exec.CommandContext(t.Context(), "git", "clone", "--quiet", "--shared", mirror, dest).CombinedOutput()
	require.NoError(t, err, "%s", out)
	out, err = exec.CommandContext(t.Context(), "git", "-C", dest, "checkout", "--quiet", sha).CombinedOutput()
	require.NoError(t, err, "%s", out)
	require.FileExists(t, filepath.Join(dest, "f.txt"))
}

// TestEnsureRepoMirror_CorruptEntry pins the self-heal: a cache entry
// that exists but is not a valid bare repo is rebuilt rather than
// served to consumer clones forever.
func TestEnsureRepoMirror_CorruptEntry(t *testing.T) {
	src := t.TempDir()
	git := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.CommandContext(t.Context(), "git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, "git %v: %s", args, out)
	}
	git(src, "init", "--quiet")
	require.NoError(t, os.WriteFile(filepath.Join(src, "f.txt"), []byte("v1"), 0o644))
	git(src, "add", ".")
	git(src, "commit", "--quiet", "-m", "init")

	cache := t.TempDir()
	t.Setenv(EvalRepoCacheEnvVar, cache)

	mirror, err := ensureRepoMirror(t.Context(), src)
	require.NoError(t, err)

	// Corrupt the entry: a directory with the right name but no repo.
	require.NoError(t, os.RemoveAll(mirror))
	require.NoError(t, os.MkdirAll(mirror, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(mirror, "garbage"), []byte("x"), 0o644))

	mirror2, err := ensureRepoMirror(t.Context(), src)
	require.NoError(t, err)
	require.Equal(t, mirror, mirror2)
	out, err := exec.CommandContext(t.Context(), "git", "-C", mirror2, "rev-parse", "--is-bare-repository").CombinedOutput()
	require.NoError(t, err, "rebuilt mirror is a real bare repo: %s", out)
}

func TestIsLocalRepo(t *testing.T) {
	t.Parallel()
	require.True(t, isLocalRepo("/abs/path"))
	require.True(t, isLocalRepo("./rel"))
	require.True(t, isLocalRepo("file:///x"))
	require.False(t, isLocalRepo("https://github.com/x/y.git"))
}
