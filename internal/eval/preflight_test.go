package eval

import (
	"context"
	"errors"
	"math/rand/v2"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func pfExperiment(providers map[string]any) *Experiment {
	return &Experiment{
		Name:      "pf",
		Model:     "testp/m",
		Providers: providers,
		Arms:      map[string]Arm{ArmControl: {}, ArmTreatment: {}},
	}
}

func testProvider(apiKey string) map[string]any {
	return map[string]any{
		"testp": map[string]any{
			"name":     "TestP",
			"type":     "openai-compat",
			"api_key":  apiKey,
			"base_url": "https://example.invalid/v1",
			"models":   []any{map[string]any{"id": "m", "name": "m"}},
		},
	}
}

func TestPreflightExperiment_UnsetEnvRef(t *testing.T) {
	t.Parallel()
	r := &Runner{Driver: CrushRunner{Home: t.TempDir()}}
	err := r.preflightExperiment(t.Context(), pfExperiment(testProvider("$EVALTEST_MISSING_KEY")))
	require.Error(t, err)
	require.Contains(t, err.Error(), "testp")
	require.Contains(t, err.Error(), "resolves empty")
}

func TestPreflightExperiment_SetButEmpty(t *testing.T) {
	// Set-but-empty resolves empty — the provider survives prep but
	// every request would 401; the declared-credential check flags it.
	t.Setenv("EVALTEST_EMPTY_KEY", "")
	r := &Runner{Driver: CrushRunner{Home: t.TempDir()}}
	err := r.preflightExperiment(t.Context(), pfExperiment(testProvider("$EVALTEST_EMPTY_KEY")))
	require.Error(t, err)
	require.Contains(t, err.Error(), "testp")
	require.Contains(t, err.Error(), "resolves empty")
}

func TestPreflightExperiment_LoopbackKeylessOK(t *testing.T) {
	t.Parallel()
	// A keyless local provider is the legitimate empty-credential case.
	p := map[string]any{
		"testp": map[string]any{
			"name":     "Local",
			"type":     "openai-compat",
			"base_url": "http://localhost:11434/v1",
			"models":   []any{map[string]any{"id": "m", "name": "m"}},
		},
	}
	r := &Runner{Driver: CrushRunner{Home: t.TempDir()}}
	require.NoError(t, r.preflightExperiment(t.Context(), pfExperiment(p)))
}

func TestPreflightExperiment_ExtraEnvOverlay(t *testing.T) {
	t.Parallel()
	// Credentials injected via ExtraEnv resolve identically — a
	// LookupEnv-only check would false-abort here.
	r := &Runner{Driver: CrushRunner{Home: t.TempDir(), ExtraEnv: []string{"EVALTEST_SET_KEY=abc123"}}}
	require.NoError(t, r.preflightExperiment(t.Context(), pfExperiment(testProvider("$EVALTEST_SET_KEY"))))
}

func TestPreflightExperiment_CommandRef(t *testing.T) {
	t.Parallel()
	r := &Runner{Driver: CrushRunner{Home: t.TempDir()}}
	require.NoError(t, r.preflightExperiment(t.Context(), pfExperiment(testProvider("$(echo evaltest_cmd_key)"))))
}

func TestPreflightExperiment_ModelMissingDiscoveryOff(t *testing.T) {
	p := testProvider("$EVALTEST_SET_KEY")
	p["testp"].(map[string]any)["models"] = []any{map[string]any{"id": "other", "name": "other"}}
	p["testp"].(map[string]any)["discover_models"] = false
	t.Setenv("EVALTEST_SET_KEY", "abc123")
	r := &Runner{Driver: CrushRunner{Home: t.TempDir()}}
	err := r.preflightExperiment(t.Context(), pfExperiment(p))
	require.Error(t, err)
	require.Contains(t, err.Error(), `model "m" not found in provider "testp"`)
}

func TestPreflightExperiment_ModelMissingDiscoveryUnset(t *testing.T) {
	// discover_models unset + a non-empty declared list → the child
	// never discovers (autoTrigger needs models empty) → a pin outside
	// the list fails at runtime. Preflight flags it post-prep.
	p := testProvider("$EVALTEST_SET_KEY")
	p["testp"].(map[string]any)["models"] = []any{map[string]any{"id": "other", "name": "other"}}
	t.Setenv("EVALTEST_SET_KEY", "abc123")
	r := &Runner{Driver: CrushRunner{Home: t.TempDir()}}
	err := r.preflightExperiment(t.Context(), pfExperiment(p))
	require.Error(t, err)
	require.Contains(t, err.Error(), `model "m" not found in provider "testp"`)
}

func TestPreflightExperiment_BuiltinProviderMissingKey(t *testing.T) {
	t.Parallel()
	// No providers block — the child resolves via the catwalk catalog,
	// which wants ANTHROPIC_API_KEY. Deliberately absent in test env.
	if os.Getenv("ANTHROPIC_API_KEY") != "" {
		t.Skip("ANTHROPIC_API_KEY set — cannot assert the abort")
	}
	exp := pfExperiment(nil)
	exp.Model = "anthropic/claude-sonnet-4-5"
	r := &Runner{Driver: CrushRunner{Home: t.TempDir()}}
	err := r.preflightExperiment(t.Context(), exp)
	require.Error(t, err)
	require.Contains(t, err.Error(), "anthropic")
}

// errRunner fails every run with a fixed error — the breaker's fixture.
type errRunner struct{ err string }

func (e errRunner) Run(_ context.Context, _ string, _ []string, _ Budget) RunResult {
	return RunResult{Err: errors.New(e.err)}
}

// dbErrRunner writes a crush.db into the run's data dir before failing —
// mirroring the real config-failure shape (the child creates the DB in
// setupLocalWorkspace before IsConfigured errors, and preserveSessionDB
// snapshots it). This is the record shape the SessionDB guard got wrong.
type dbErrRunner struct{ err string }

func (e dbErrRunner) Run(_ context.Context, workdir string, _ []string, _ Budget) RunResult {
	_ = os.MkdirAll(DataDirFor(workdir), 0o755)
	_ = os.WriteFile(filepath.Join(DataDirFor(workdir), "crush.db"), []byte("x"), 0o644)
	return RunResult{Err: errors.New(e.err)}
}

func breakerFixture(t *testing.T) (*Runner, *Experiment, *Trajectory, string) {
	root := newEvalDir(t)
	trajDir := writeTrajectory(t, filepath.Join(root, "corpus"), "brk-t", nil)
	tr, err := LoadTrajectory(trajDir)
	require.NoError(t, err)
	exp := &Experiment{
		Name: "brk", Model: "mock/m",
		Arms: map[string]Arm{ArmControl: {}, ArmTreatment: {}},
	}
	return &Runner{
		EvalDir:    root,
		WorkParent: t.TempDir(),
		RNG:        rand.New(rand.NewPCG(1, 2)),
	}, exp, tr, trajDir
}

func TestRunTrajectory_ConfigClassBreaker(t *testing.T) {
	t.Parallel()
	r, exp, tr, trajDir := breakerFixture(t)
	// Production shape: the child creates crush.db before IsConfigured
	// fails, so the record carries session_db + a capitalized message —
	// the exact axes the earlier fixture diverged on.
	r.Driver = dbErrRunner{err: "crush run failed: exit status 1: \n   ERROR  \n\n  No providers configured - please run 'crush' to set up a provider interactively."}
	rep := r.runTrajectory(t.Context(), exp, tr, trajDir, &FlagsManifest{Defaults: map[string]any{}}, 3, "inv", &configErrorTracker{})
	require.Error(t, rep.Abort)
	require.Contains(t, rep.Abort.Error(), "config-class")
}

func TestIsConfigClassError_RealShapes(t *testing.T) {
	t.Parallel()
	// The actual record bytes from invocation 071231Z.
	realRecord := RunRecord{
		Outcome:   OutcomeError,
		Steps:     0,
		DurationS: 0.2,
		SessionDB: "results/x/artifacts/t-control-1.db",
		CheckDetail: map[string]any{
			"run_error": "crush run failed: exit status 1:           \n   ERROR  \n          \n  No providers configured - please run 'crush' to set up a provider interactively.",
		},
	}
	require.True(t, isConfigClassError(realRecord))

	// Reached-the-model records never classify — request stats exist.
	withReq := realRecord
	withReq.Request = &RequestStats{PromptRequests: 3}
	require.False(t, isConfigClassError(withReq))

	// Rate limits keep sampling even when byte-identical.
	rateLimit := RunRecord{
		Outcome:     OutcomeError,
		CheckDetail: map[string]any{"run_error": "crush run failed: exit status 1: Error: rate limit exceeded"},
	}
	require.False(t, isConfigClassError(rateLimit))

	// Auth-class: telemetry-written mid-run credential failure.
	auth := RunRecord{
		Outcome:     OutcomeError,
		Steps:       5,
		SessionDB:   "x.db",
		Request:     &RequestStats{PromptRequests: 5},
		CheckDetail: map[string]any{"run_error": "agent run failed: unauthorized: No API-key provided."},
	}
	require.True(t, isConfigClassError(auth))

	// Auth-adjacent rate limit doesn't classify.
	authRL := auth
	authRL.CheckDetail = map[string]any{"run_error": "agent run failed: api key rate limit exceeded"}
	require.False(t, isConfigClassError(authRL))
}

func TestIsConfigClassError_ErrorClass(t *testing.T) {
	t.Parallel()
	// Typed classification wins over the string fallback — the child's
	// ProviderError status decides, not message text.
	mk := func(class string) RunRecord {
		return RunRecord{
			Outcome:     OutcomeError,
			ErrorClass:  class,
			Steps:       5,
			Request:     &RequestStats{PromptRequests: 5},
			CheckDetail: map[string]any{"run_error": "agent run failed: opaque provider text"},
		}
	}
	for _, class := range []string{"auth", "provider_unreachable"} {
		require.True(t, isConfigClassError(mk(class)), class)
	}
	for _, class := range []string{"rate_limit", "provider_transient", "provider_other", "context_too_large", "window_cap_enforced", "cancelled", "timeout"} {
		require.False(t, isConfigClassError(mk(class)), class)
	}
	// provider_deterministic and provider_server share the scope
	// split: pre-model (no steps, no request) they're config-shaped
	// and trip the experiment breaker; mid-run they're
	// trajectory-shaped — fixture-class, so two strikes skip the
	// trajectory instead of aborting the experiment. A dead
	// endpoint can't produce a completed request, so a real outage
	// still reads step-0.
	for _, class := range []string{"provider_deterministic", "provider_server"} {
		midRun := mk(class)
		require.False(t, isConfigClassError(midRun), class)
		require.True(t, isFixtureConfigError(midRun), class)
		step0 := midRun
		step0.Steps = 0
		step0.Request = nil
		require.True(t, isConfigClassError(step0), class)
		require.False(t, isFixtureConfigError(step0), class)
	}
	// context_too_large is trajectory-scoped — fixture-class, so two
	// strikes skip the trajectory rather than abort the experiment.
	require.True(t, isFixtureConfigError(mk("context_too_large")))
	// window_cap_enforced is neither breaker: a manufactured cap
	// death is the experiment's designed condition, so it keeps
	// sampling as an ordinary excluded-class error — fixture-classing
	// it would void the invocation exactly when the cap works.
	require.False(t, isConfigClassError(mk("window_cap_enforced")))
	require.False(t, isFixtureConfigError(mk("window_cap_enforced")))
}

// classRunner fails every run with a fixed typed error class — the
// breaker's fixture for error_class records.
type classRunner struct{ class string }

func (e classRunner) Run(_ context.Context, _ string, _ []string, _ Budget) RunResult {
	return RunResult{Err: errors.New("agent run failed: provider exploded"), ErrorClass: e.class}
}

func TestRunTrajectory_ProviderServerBreaker(t *testing.T) {
	t.Parallel()
	r, exp, tr, trajDir := breakerFixture(t)
	// A provider 5xx that survived the child's internal retries trips
	// the config breaker at two strikes — the strike count is the
	// persistence test.
	r.Driver = classRunner{class: "provider_server"}
	rep := r.runTrajectory(t.Context(), exp, tr, trajDir, &FlagsManifest{Defaults: map[string]any{}}, 3, "inv", &configErrorTracker{})
	require.Error(t, rep.Abort)
	require.Contains(t, rep.Abort.Error(), "config-class")
}

func TestRunTrajectory_TransientClassKeepsSampling(t *testing.T) {
	t.Parallel()
	r, exp, tr, trajDir := breakerFixture(t)
	// Transient-class errors burn attempts but never trip the config
	// breaker — the attempts cap is the mechanism.
	r.Driver = classRunner{class: "provider_transient"}
	rep := r.runTrajectory(t.Context(), exp, tr, trajDir, &FlagsManifest{Defaults: map[string]any{}}, 1, "inv", &configErrorTracker{})
	require.NoError(t, rep.Abort)
}

func TestRunTrajectory_AuthClassBreaker(t *testing.T) {
	t.Parallel()
	r, exp, tr, trajDir := breakerFixture(t)
	// A custom provider that survived load with a dead credential fails
	// every request — telemetry present, session written, steps > 0.
	r.Driver = errRunner{err: "agent run failed: unauthorized: No API-key provided."}
	rep := r.runTrajectory(t.Context(), exp, tr, trajDir, &FlagsManifest{Defaults: map[string]any{}}, 3, "inv", &configErrorTracker{})
	require.Error(t, rep.Abort)
	require.Contains(t, rep.Abort.Error(), "config-class")
}

func TestRunTrajectory_FixtureBreakerSkips(t *testing.T) {
	t.Parallel()
	r, exp, tr, trajDir := breakerFixture(t)
	// A corrupt fixture .crush.json — WriteArmConfig rejects it, the
	// record carries harness detail, two failures skip the trajectory
	// instead of burning to the cap.
	require.NoError(t, os.WriteFile(filepath.Join(trajDir, "fixture", ".crush.json"), []byte("{invalid"), 0o644))
	r.Driver = errRunner{err: "agent run failed: unreachable"}
	rep := r.runTrajectory(t.Context(), exp, tr, trajDir, &FlagsManifest{Defaults: map[string]any{}}, 3, "inv", &configErrorTracker{})
	require.NoError(t, rep.Abort)
	require.Contains(t, rep.Skipped, "fixture config")
}

func TestIsFixtureConfigError_Shapes(t *testing.T) {
	t.Parallel()
	// Bare records are NOT fixture-class — ExecuteRun-internal errors
	// (spawn/disk/timeout) are transient, not deterministic.
	require.False(t, isFixtureConfigError(RunRecord{Outcome: OutcomeError}))
	require.True(t, isFixtureConfigError(RunRecord{
		Outcome:     OutcomeError,
		CheckDetail: map[string]any{"harness": "materialize: .crush.json does not parse"},
	}))
	require.True(t, isFixtureConfigError(RunRecord{
		Outcome:     OutcomeError,
		CheckDetail: map[string]any{"check_error": "exec check.sh: no such file"},
	}))
	// Subprocess errors are not fixture-class — they run through the
	// config-class classifier instead.
	require.False(t, isFixtureConfigError(RunRecord{
		Outcome:     OutcomeError,
		CheckDetail: map[string]any{"run_error": "crush run failed: exit status 1"},
	}))
	require.False(t, isFixtureConfigError(RunRecord{Outcome: OutcomePass}))
}

func TestRunTrajectory_NonConfigErrorsKeepSampling(t *testing.T) {
	t.Parallel()
	r, exp, tr, trajDir := breakerFixture(t)
	// Identical string every time — a rate limit looks the same. The
	// breaker must not fire; attempts run to the cap instead.
	r.Driver = errRunner{err: "crush run failed: exit status 1: Error: rate limit exceeded"}
	rep := r.runTrajectory(t.Context(), exp, tr, trajDir, &FlagsManifest{Defaults: map[string]any{}}, 3, "inv", &configErrorTracker{})
	require.NoError(t, rep.Abort)
	require.NotEmpty(t, rep.Saturated)
}
