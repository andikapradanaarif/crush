package eval

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// The allowlist is the #345 fix: an ambient var the manifest never
// referenced cannot reach the child — the leak class that let a dead
// ANTHROPIC_API_KEY silently select the small model in #333.
func TestSubprocessEnv_ScrubsUnreferencedAmbient(t *testing.T) {
	t.Setenv("EVALTEST_AMBIENT_LEAK", "secret")
	t.Setenv("EVALTEST_MANIFEST_KEY", "abc123")
	c := CrushRunner{
		Home:       t.TempDir(),
		AllowedEnv: []string{"EVALTEST_MANIFEST_KEY"},
	}
	env := c.subprocessEnv("tel", 0, nil)
	_, ok := envValue(env, "EVALTEST_AMBIENT_LEAK")
	require.False(t, ok, "unreferenced ambient var must be scrubbed")
	got, ok := envValue(env, "EVALTEST_MANIFEST_KEY")
	require.True(t, ok)
	require.Equal(t, "abc123", got)
	// The fixed minimal set always passes.
	_, ok = envValue(env, "PATH")
	require.True(t, ok, "PATH is in the fixed set")
	got, ok = envValue(env, "HOME")
	require.True(t, ok)
	require.Equal(t, c.Home, got, "HOME is pinned, not inherited")
}

// Empty AllowedEnv is fail-closed: a path that forgets to populate it
// scrubs everything non-fixed rather than inheriting everything.
func TestSubprocessEnv_NilAllowlistScrubsAll(t *testing.T) {
	t.Setenv("EVALTEST_SHOULD_NOT_PASS", "x")
	c := CrushRunner{Home: t.TempDir()}
	env := c.subprocessEnv("tel", 0, nil)
	_, ok := envValue(env, "EVALTEST_SHOULD_NOT_PASS")
	require.False(t, ok)
	_, ok = envValue(env, "PATH")
	require.True(t, ok)
}

// Harness-prefixed vars stay stripped even when the allowlist names
// them — a manifest can't un-pin EVAL_/CRUSH_ wiring.
func TestSubprocessEnv_AllowlistCannotAdmitHarnessPrefix(t *testing.T) {
	t.Setenv("CRUSH_EVALTEST_LEAK", "x")
	c := CrushRunner{Home: t.TempDir(), AllowedEnv: []string{"CRUSH_EVALTEST_LEAK"}}
	env := c.subprocessEnv("tel", 0, nil)
	_, ok := envValue(env, "CRUSH_EVALTEST_LEAK")
	require.False(t, ok)
}

// Ambient GOMODCACHE/GOCACHE are scrubbed by the allowlist, so the
// shared-cache pin must win over them — otherwise every child falls
// back to per-home caches (the 1-3GB-per-run leak the pin prevents).
func TestSubprocessEnv_GoCachePinBeatsAmbient(t *testing.T) {
	cacheDir := filepath.Join(os.TempDir(), "eval-go")
	t.Setenv(EvalGoCacheEnvVar, cacheDir)
	t.Setenv("GOMODCACHE", "/ambient/mod")
	t.Setenv("GOCACHE", "/ambient/build")
	c := CrushRunner{Home: t.TempDir()}
	env := c.subprocessEnv("tel", 0, nil)
	got, ok := envValue(env, "GOMODCACHE")
	require.True(t, ok)
	require.Equal(t, filepath.Join(cacheDir, "mod"), got,
		"ambient GOMODCACHE is scrubbed — the pin must apply")
	got, ok = envValue(env, "GOCACHE")
	require.True(t, ok)
	require.Equal(t, filepath.Join(cacheDir, "build"), got)
}

// ExtraEnv is caller intent — it still overrides pins and needs no
// allowlist entry.
func TestSubprocessEnv_ExtraEnvBypassesAllowlist(t *testing.T) {
	c := CrushRunner{Home: t.TempDir(), ExtraEnv: []string{"EVALTEST_CALLER_KEY=v"}}
	env := c.subprocessEnv("tel", 0, nil)
	got, ok := envValue(env, "EVALTEST_CALLER_KEY")
	require.True(t, ok)
	require.Equal(t, "v", got)
}

func TestEnvAllowlist_ManifestRefs(t *testing.T) {
	exp := &Experiment{
		Name:  "allow",
		Model: "testp/m",
		Providers: map[string]any{
			"testp": map[string]any{
				"type":     "openai-compat",
				"api_key":  "$EVALTEST_LLM_KEY",
				"base_url": "${EVALTEST_LLM_URL:-http://localhost:1}",
				"default_headers": map[string]any{
					"X-Auth": "${EVALTEST_HEADER_TOKEN}",
				},
			},
		},
		Arms: map[string]Arm{
			ArmControl: {Config: ArmConfig{Options: map[string]any{
				"proxy_url": "$EVALTEST_OPT_REF",
			}}},
		},
	}
	got := envAllowlist(exp)
	require.Contains(t, got, "EVALTEST_LLM_KEY")
	require.Contains(t, got, "EVALTEST_LLM_URL")
	require.Contains(t, got, "EVALTEST_HEADER_TOKEN")
	require.Contains(t, got, "EVALTEST_OPT_REF")
	require.Contains(t, got, "PATH")
	require.Contains(t, got, "HOME")
	require.Contains(t, got, "TMPDIR")
	// Sorted output — the record field diffs cleanly run-to-run.
	require.IsIncreasing(t, got)
}

// A builtin pin with no providers block borrows the catalog's
// credential templates — anthropic/... admits $ANTHROPIC_API_KEY and
// the endpoint override, nothing else.
func TestEnvAllowlist_BuiltinPinBorrowsCatalogTemplates(t *testing.T) {
	exp := &Experiment{
		Name:  "allow-builtin",
		Model: "anthropic/claude-sonnet-4-5",
		Arms:  map[string]Arm{ArmControl: {}},
	}
	got := envAllowlist(exp)
	require.Contains(t, got, "ANTHROPIC_API_KEY")
	require.NotContains(t, got, "OPENAI_API_KEY",
		"unrelated catalog providers contribute nothing")
}

// An ambient-but-unreferenced credential can no longer mask a missing
// manifest key in preflight: the resolver sees the child env exactly.
func TestPreflightExperiment_AmbientCannotMaskMissing(t *testing.T) {
	// The manifest references a key that is absent even in the parent
	// env — same abort shape as before, now provably over the
	// allowlisted view.
	unsetEnv(t, "EVALTEST_NEVER_SET")
	r := &Runner{Driver: CrushRunner{Home: t.TempDir()}}
	err := r.preflightExperiment(t.Context(), pfExperiment(testProvider("$EVALTEST_NEVER_SET")))
	require.Error(t, err)
	require.Contains(t, err.Error(), "resolves empty")
}

func TestEnvRecord_AllowlistProvenance(t *testing.T) {
	// The record carries names, never values.
	exp := &Experiment{
		Name:  "rec",
		Model: "testp/m",
		Providers: map[string]any{
			"testp": map[string]any{"type": "openai-compat", "api_key": "$EVALTEST_REC_KEY"},
		},
		Arms: map[string]Arm{ArmControl: {}},
	}
	got := envAllowlist(exp)
	require.Contains(t, got, "EVALTEST_REC_KEY")
	// Field rides Env — marshals under env_allowlist.
	e := Env{EnvAllowlist: got}
	data, err := json.Marshal(e)
	require.NoError(t, err)
	require.Contains(t, string(data), `"env_allowlist"`)
	require.NotContains(t, string(data), "abc123",
		"allowlist records names only — no value ever lands in it")
}
