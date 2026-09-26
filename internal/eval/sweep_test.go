package eval

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// unsetEnv removes key for the test duration, restoring any previous
// value — t.Setenv cannot express unset.
func unsetEnv(t *testing.T, key string) {
	t.Helper()
	if v, ok := os.LookupEnv(key); ok {
		require.NoError(t, os.Unsetenv(key))
		t.Cleanup(func() { os.Setenv(key, v) })
	}
}

// envValue extracts key's effective value from a k=v env slice —
// exec.Cmd.Env resolves last-wins, so the last match is what the
// child sees.
func envValue(env []string, key string) (string, bool) {
	val, found := "", false
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, key+"="); ok {
			val, found = v, true
		}
	}
	return val, found
}

func TestSweepStaleTemps(t *testing.T) {
	t.Parallel()
	parent := t.TempDir()
	mk := func(name string, age time.Duration) string {
		d := filepath.Join(parent, name)
		require.NoError(t, os.MkdirAll(d, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(d, "f"), []byte("x"), 0o644))
		mtime := time.Now().Add(-age)
		require.NoError(t, os.Chtimes(d, mtime, mtime))
		return d
	}
	staleHome := mk("crush-eval-home-1", 48*time.Hour)
	staleWorkdir := mk("eval-run-42", 48*time.Hour)
	staleData := mk("eval-run-42.crush-data", 48*time.Hour)
	fresh := mk("crush-eval-home-2", time.Minute)
	other := mk("unrelated", 100*24*time.Hour)
	goBuild := mk("go-build999", 48*time.Hour)

	removed := sweepStaleTemps(parent, time.Now())
	require.Equal(t, 3, removed)
	for _, d := range []string{staleHome, staleWorkdir, staleData} {
		require.NoDirExists(t, d)
	}
	for _, d := range []string{fresh, other, goBuild} {
		require.DirExists(t, d)
	}
}

func TestSweepStaleTemps_ReadOnlyTree(t *testing.T) {
	t.Parallel()
	parent := t.TempDir()
	d := filepath.Join(parent, "crush-eval-home-ro")
	sub := filepath.Join(d, "go", "pkg", "mod")
	require.NoError(t, os.MkdirAll(sub, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(sub, "m.go"), []byte("x"), 0o444))
	old := time.Now().Add(-48 * time.Hour)
	require.NoError(t, os.Chtimes(d, old, old))
	// Module-cache trees land read-only — a plain RemoveAll fails on
	// them; the sweep must chmod through.
	require.NoError(t, os.Chmod(sub, 0o555))
	require.NoError(t, os.Chmod(filepath.Join(d, "go"), 0o555))
	require.NoError(t, os.Chmod(filepath.Join(d, "go", "pkg"), 0o555))

	require.Equal(t, 1, sweepStaleTemps(parent, time.Now()))
	require.NoDirExists(t, d)
}

func TestSweepStaleTemps_Bounded(t *testing.T) {
	t.Parallel()
	parent := t.TempDir()
	old := time.Now().Add(-48 * time.Hour)
	for i := range staleTempMaxPerSweep + 5 {
		d := filepath.Join(parent, fmt.Sprintf("crush-eval-home-%d", i))
		require.NoError(t, os.Mkdir(d, 0o755))
		require.NoError(t, os.Chtimes(d, old, old))
	}
	require.Equal(t, staleTempMaxPerSweep, sweepStaleTemps(parent, time.Now()))
	entries, err := os.ReadDir(parent)
	require.NoError(t, err)
	require.Len(t, entries, 5)
}

func TestGoCachePins(t *testing.T) {
	unsetEnv(t, "GOMODCACHE")
	unsetEnv(t, "GOCACHE")
	t.Setenv(EvalGoCacheEnvVar, "/x/eval-go")
	pins := goCachePins(nil)
	require.Equal(t, filepath.Join("/x/eval-go", "mod"), pins["GOMODCACHE"])
	require.Equal(t, filepath.Join("/x/eval-go", "build"), pins["GOCACHE"])
}

func TestGoCachePins_HonorsAmbient(t *testing.T) {
	t.Setenv("GOMODCACHE", "/operator/mod")
	t.Setenv("GOCACHE", "/operator/build")
	t.Setenv(EvalGoCacheEnvVar, "/x/eval-go")
	require.Empty(t, goCachePins(nil))
}

func TestGoCachePins_HonorsReserved(t *testing.T) {
	unsetEnv(t, "GOMODCACHE")
	unsetEnv(t, "GOCACHE")
	pins := goCachePins(map[string]bool{"GOMODCACHE": true})
	require.NotContains(t, pins, "GOMODCACHE")
	require.Contains(t, pins, "GOCACHE")
}

func TestCheckEnv_PinsGoCaches(t *testing.T) {
	unsetEnv(t, "GOMODCACHE")
	unsetEnv(t, "GOCACHE")
	t.Setenv(EvalGoCacheEnvVar, "/x/eval-go")
	r := &Runner{Home: t.TempDir()}
	env := r.checkEnv()
	mod, ok := envValue(env, "GOMODCACHE")
	require.True(t, ok)
	require.Equal(t, filepath.Join("/x/eval-go", "mod"), mod)
	build, ok := envValue(env, "GOCACHE")
	require.True(t, ok)
	require.Equal(t, filepath.Join("/x/eval-go", "build"), build)
}

func TestSubprocessEnv_PinsGoCaches(t *testing.T) {
	unsetEnv(t, "GOMODCACHE")
	unsetEnv(t, "GOCACHE")
	t.Setenv(EvalGoCacheEnvVar, "/x/eval-go")
	c := CrushRunner{Home: t.TempDir()}
	env := c.subprocessEnv("tel", 0)
	mod, ok := envValue(env, "GOMODCACHE")
	require.True(t, ok)
	require.Equal(t, filepath.Join("/x/eval-go", "mod"), mod)
}

func TestSubprocessEnv_GoCacheYieldsToExtraEnv(t *testing.T) {
	unsetEnv(t, "GOMODCACHE")
	unsetEnv(t, "GOCACHE")
	c := CrushRunner{Home: t.TempDir(), ExtraEnv: []string{"GOMODCACHE=/caller/mod"}}
	env := c.subprocessEnv("tel", 0)
	mod, ok := envValue(env, "GOMODCACHE")
	require.True(t, ok)
	require.Equal(t, "/caller/mod", mod)
}
