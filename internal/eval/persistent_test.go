package eval

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// fakeCrushMarkerEnv switches this test binary into the fake-child
// role when a driver spawns it: the child writes canned per-turn
// telemetry files instead of running an agent, so the persistent/
// restart regimes are exercisable without a provider (#117).
const fakeCrushMarkerEnv = "GO_EVAL_FAKE_CRUSH"

func TestMain(m *testing.M) {
	if os.Getenv(fakeCrushMarkerEnv) == "1" {
		os.Exit(fakeEvalChild())
	}
	os.Exit(m.Run())
}

// fakeEvalChild is the test binary's second personality. With
// CRUSH_EVAL_TURNS_FILE set it plays the persistent-process contract:
// one invocation emits <telBase>-<i> per prompt. Without it the
// restart contract applies: one file at the pinned telemetry path.
// GO_EVAL_FAIL_TURN=<i> errors that turn mid-stream; GO_EVAL_EMIT_ONLY
// truncates emission with a clean exit.
func fakeEvalChild() int {
	telBase := os.Getenv(EvalTelemetryEnvVar)
	pid := os.Getpid()
	write := func(path string, turn int, firstOfProcess bool, extra map[string]any) {
		doc := map[string]any{
			"session_id":    "sess-fake",
			"param_version": "pv1",
			"steps":         1,
			"tokens":        map[string]any{"input": 10, "output": 5, "cache_read": 2, "cache_write": 1},
			"request": map[string]any{
				"prompt_requests":    1,
				"prompt_tokens_last": 100 + turn,
				"steps": []map[string]any{{
					"step":             0,
					"pid":              pid,
					"first_of_process": firstOfProcess,
					"input_tokens":     10,
					"output_tokens":    5,
				}},
			},
			"request_vector":   map[string]any{"session_id": "sess-fake", "v": turn},
			"drain":            map[string]any{"attempted": true, "completed": true},
			"resolved_options": map[string]any{},
		}
		for k, v := range extra {
			doc[k] = v
		}
		data, _ := json.Marshal(doc)
		_ = os.WriteFile(path, data, 0o600)
	}

	if tf := os.Getenv(EvalTurnsFileEnvVar); tf != "" {
		data, err := os.ReadFile(tf)
		var turns []string
		if err != nil || json.Unmarshal(data, &turns) != nil {
			fmt.Fprintln(os.Stderr, "turns file unreadable:", err)
			return 2
		}
		emitOnly, _ := strconv.Atoi(os.Getenv("GO_EVAL_EMIT_ONLY"))
		failAt := os.Getenv("GO_EVAL_FAIL_TURN")
		for i := range turns {
			if emitOnly > 0 && i >= emitOnly {
				return 0
			}
			if strconv.Itoa(i) == failAt {
				write(fmt.Sprintf("%s-%d", telBase, i), i, i == 0, map[string]any{
					"model": "mock/m-persistent", "error": "boom", "error_class": "fixture",
				})
				return 1
			}
			write(fmt.Sprintf("%s-%d", telBase, i), i, i == 0, map[string]any{"model": "mock/m-persistent"})
		}
		return 0
	}
	// Restart shape: the driver names the per-turn file itself, and
	// every invocation is a fresh process's first request.
	write(telBase, 0, true, map[string]any{"model": "mock/m-restart"})
	return 0
}

func persistentRunner(t *testing.T, extra ...string) PersistentRunner {
	t.Helper()
	return PersistentRunner{CrushRunner{
		Bin:      os.Args[0],
		Home:     t.TempDir(),
		ExtraEnv: append([]string{fakeCrushMarkerEnv + "=1"}, extra...),
	}}
}

// The persistent fold: one subprocess emits per-turn deltas at
// <base>-<i>; the result looks exactly like the restart fold —
// steps/tokens summed, turns stamped, per-turn drains — except every
// step row carries the SAME pid (one process) and first_of_process
// only on turn 0.
func TestPersistentRunner_FoldsPerTurnDeltas(t *testing.T) {
	t.Parallel()
	parent := t.TempDir()
	workdir := filepath.Join(parent, "traj")
	require.NoError(t, os.MkdirAll(workdir, 0o755))

	p := persistentRunner(t)
	res := p.Run(t.Context(), workdir, []string{"a", "b", "c"}, Budget{})

	require.NoError(t, res.Err)
	require.False(t, res.TimedOut)
	require.Equal(t, 3, res.Steps)
	require.Equal(t, "sess-fake", res.SessionID)
	require.Equal(t, "mock/m-persistent", res.ModelResolved)
	require.Equal(t, int64(30), res.Tokens.Input)
	require.Equal(t, "pv1", res.ParamVersion)

	require.Len(t, res.StepRecords, 3)
	pid := res.StepRecords[0].PID
	require.NotZero(t, pid)
	for i, s := range res.StepRecords {
		require.Equal(t, pid, s.PID, "one process drives every turn")
		require.Equal(t, i, s.Turn)
		require.Equal(t, i == 0, s.FirstOfProcess,
			"only the trajectory's first request is first-of-process")
	}
	require.Equal(t, []int64{100, 101, 102}, res.PromptTokensPerTurn)
	require.Len(t, res.Drains, 3)
	require.JSONEq(t, `{"session_id":"sess-fake","v":2}`, string(res.RequestVector))

	// Scratch files clean up: the workdir parent carries neither the
	// turns file nor turn telemetry after the fold.
	for _, pat := range []string{".eval-turns-*", ".eval-telemetry-*"} {
		matches, err := filepath.Glob(filepath.Join(parent, pat))
		require.NoError(t, err)
		require.Empty(t, matches)
	}
}

// A mid-trajectory error folds the turns that reported, classifies
// from the failing turn's telemetry, and stops — same contract as a
// restart turn dying.
func TestPersistentRunner_MidTurnErrorStopsFold(t *testing.T) {
	t.Parallel()
	parent := t.TempDir()
	workdir := filepath.Join(parent, "traj")
	require.NoError(t, os.MkdirAll(workdir, 0o755))

	p := persistentRunner(t, "GO_EVAL_FAIL_TURN=1")
	res := p.Run(t.Context(), workdir, []string{"a", "b", "c"}, Budget{})

	require.Error(t, res.Err)
	require.Contains(t, res.Err.Error(), "boom")
	require.Equal(t, "fixture", res.ErrorClass)
	require.Equal(t, 2, res.Steps, "turns 0-1 folded; turn 2 never emitted")
	require.Len(t, res.StepRecords, 2)
}

// A clean exit that emitted fewer files than the turn list is a
// broken run — silently folding a partial trajectory would read as a
// short but valid measurement.
func TestPersistentRunner_ShortEmissionIsError(t *testing.T) {
	t.Parallel()
	parent := t.TempDir()
	workdir := filepath.Join(parent, "traj")
	require.NoError(t, os.MkdirAll(workdir, 0o755))

	p := persistentRunner(t, "GO_EVAL_EMIT_ONLY=1")
	res := p.Run(t.Context(), workdir, []string{"a", "b", "c"}, Budget{})

	require.Error(t, res.Err)
	require.Contains(t, res.Err.Error(), "emitted 1 of 3")
	require.Equal(t, 1, res.Steps)
}

// Cancellation propagates as the run error, matching runTurnOnce's
// non-deadline ctx branch.
func TestPersistentRunner_CancelledCtx(t *testing.T) {
	t.Parallel()
	parent := t.TempDir()
	workdir := filepath.Join(parent, "traj")
	require.NoError(t, os.MkdirAll(workdir, 0o755))

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	p := persistentRunner(t)
	res := p.Run(ctx, workdir, []string{"a", "b"}, Budget{})
	require.ErrorIs(t, res.Err, context.Canceled)
}

// The restart regime's evidence: the same fake emits one file per
// subprocess, so distinct pids across turns prove the process
// boundary — and every turn's first step is first_of_process.
func TestCrushRunner_RestartSpawnsPerTurn(t *testing.T) {
	t.Parallel()
	parent := t.TempDir()
	workdir := filepath.Join(parent, "traj")
	require.NoError(t, os.MkdirAll(workdir, 0o755))

	c := CrushRunner{
		Bin:      os.Args[0],
		Home:     t.TempDir(),
		ExtraEnv: []string{fakeCrushMarkerEnv + "=1"},
	}
	res := c.Run(t.Context(), workdir, []string{"a", "b", "c"}, Budget{})

	require.NoError(t, res.Err)
	require.Equal(t, "mock/m-restart", res.ModelResolved,
		"the per-turn subprocess path ran, not the turns-file loop")
	require.Len(t, res.StepRecords, 3)
	pids := map[int]bool{}
	for i, s := range res.StepRecords {
		require.Equal(t, i, s.Turn)
		require.True(t, s.FirstOfProcess, "every turn's first step opens a process")
		pids[s.PID] = true
	}
	require.Greater(t, len(pids), 1, "turns ran under separate pids")
}

// ExecuteRun wraps a CrushRunner into PersistentRunner when the
// experiment declares the persistent regime — the child sees the
// turns file (mock/m-persistent proves the path) and the record
// carries the regime as provenance.
func TestExecuteRun_ProcessModelPersistent(t *testing.T) {
	requireSeedCheckTooling(t) // The trajectory check execs bash.
	root := newEvalDir(t)
	trajDir := writeTrajectory(t, filepath.Join(root, "corpus"), "pm-t", map[string]any{
		"task":              map[string]any{"turns": []string{"a", "b"}},
		"check_script_body": "#!/bin/bash\nexit 0\n",
	})
	traj, err := LoadTrajectory(trajDir)
	require.NoError(t, err)

	r := &Runner{
		EvalDir: root,
		Driver: CrushRunner{
			Bin:      os.Args[0],
			Home:     t.TempDir(),
			ExtraEnv: []string{fakeCrushMarkerEnv + "=1"},
		},
		WorkParent: t.TempDir(),
		RNG:        rand.New(rand.NewPCG(1, 2)),
		Now:        func() time.Time { return time.Unix(0, 0) },
	}
	exp := &Experiment{
		Name: "pm-exp", Model: "mock/m", Temperature: ptr(0.0),
		ProcessModel: ProcessModelPersistent,
		Arms:         map[string]Arm{ArmControl: {}, ArmTreatment: {}},
	}
	manifest := &FlagsManifest{Defaults: map[string]any{}}

	rec, err := r.ExecuteRun(context.Background(), exp, traj, trajDir,
		ArmControl, Arm{}, manifest, 1, "inv")
	require.NoError(t, err)
	require.Equal(t, ProcessModelPersistent, rec.ProcessModel)
	require.Equal(t, OutcomePass, rec.Outcome)
	require.Equal(t, 2, rec.Steps)
	require.Equal(t, "sess-fake", rec.SessionID)
	require.Equal(t, "mock/m-persistent", rec.Env.ModelResolved,
		"the persistent turns-file path ran")

	// The default regime and a custom Driver are untouched: custom
	// drivers own their semantics.
	exp.ProcessModel = ""
	rec, err = r.ExecuteRun(context.Background(), exp, traj, trajDir,
		ArmControl, Arm{}, manifest, 2, "inv")
	require.NoError(t, err)
	require.Equal(t, ProcessModelRestart, rec.ProcessModel)
	require.Equal(t, "mock/m-restart", rec.Env.ModelResolved)
}

// The interaction arithmetic: per-trajectory regime deltas differ
// (C1−A1) − (C2−A2), incomplete trajectories are listed not dropped,
// and materiality is 10% of the restart-control baseline mean.
func TestProcessModelInteraction(t *testing.T) {
	t.Parallel()
	exp := &Experiment{}
	rec := func(traj, regime, arm string, steps int, o Outcome) RunRecord {
		return RunRecord{
			TrajectoryID: traj, ProcessModel: regime,
			Arm: arm, Steps: steps, Outcome: o,
		}
	}
	recs := []RunRecord{
		// t1: restart Δ = 8−10 = −2; persistent Δ = 9−10 = −1; I = −1.
		rec("t1", "restart", "control", 10, OutcomePass),
		rec("t1", "restart", "treatment", 8, OutcomePass),
		rec("t1", "persistent", "control", 10, OutcomePass),
		rec("t1", "persistent", "treatment", 9, OutcomePass),
		// t2: restart Δ = 0; persistent Δ = −2; I = +2.
		rec("t2", "restart", "control", 20, OutcomePass),
		rec("t2", "restart", "treatment", 20, OutcomePass),
		rec("t2", "persistent", "control", 20, OutcomePass),
		rec("t2", "persistent", "treatment", 18, OutcomePass),
		// t3 has no persistent cells — incomplete, listed.
		rec("t3", "restart", "control", 5, OutcomePass),
		rec("t3", "restart", "treatment", 5, OutcomePass),
		// A runaway record never samples.
		rec("t1", "restart", "control", 999, OutcomeError),
	}
	rep, err := ProcessModelInteraction(recs, exp, "steps")
	require.NoError(t, err)
	require.Len(t, rep.Cells, 2)
	require.Equal(t, []string{"t3"}, rep.Incomplete)
	require.InDelta(t, 0.5, rep.MeanInteraction, 1e-9) // (−1 + 2)/2
	require.InDelta(t, -1, rep.Cells[0].Interaction, 1e-9)
	require.InDelta(t, 2, rep.Cells[1].Interaction, 1e-9)
	// Baseline = mean restart-control = (10+20)/2 = 15 → bound 1.5;
	// |0.5| is under it.
	require.InDelta(t, 1.5, rep.MaterialThreshold, 1e-9)
	require.Equal(t, "immaterial", rep.Materiality)
}

// A regime-dependent notebook effect clears the bound → "material",
// meaning per-regime publication; the pooled estimate must not be
// read as the answer.
func TestProcessModelInteraction_Material(t *testing.T) {
	t.Parallel()
	exp := &Experiment{}
	rec := func(traj, regime, arm string, steps int) RunRecord {
		return RunRecord{
			TrajectoryID: traj, ProcessModel: regime,
			Arm: arm, Steps: steps, Outcome: OutcomePass,
		}
	}
	recs := []RunRecord{
		// Restart measures a −4 effect; persistent measures −1 —
		// restart overstated churn, exactly the suspect direction.
		rec("t1", "restart", "control", 10),
		rec("t1", "restart", "treatment", 6),
		rec("t1", "persistent", "control", 10),
		rec("t1", "persistent", "treatment", 9),
	}
	rep, err := ProcessModelInteraction(recs, exp, "steps")
	require.NoError(t, err)
	require.InDelta(t, -3, rep.MeanInteraction, 1e-9) // (−4) − (−1)
	require.InDelta(t, 1.0, rep.MaterialThreshold, 1e-9)
	require.Equal(t, "material", rep.Materiality)

	// No shared trajectory → nothing to estimate.
	rep, err = ProcessModelInteraction(
		[]RunRecord{rec("only", "restart", "control", 10)}, exp, "steps")
	require.NoError(t, err)
	require.Equal(t, "unevaluable", rep.Materiality)
	require.Empty(t, rep.Cells)
}

// process_model accepts restart/persistent/empty and rejects unknown
// values; persistent + replay has no semantics (forked boundaries are
// restart-shaped by construction) and fails at load.
func TestValidateExperiment_ProcessModel(t *testing.T) {
	t.Parallel()
	base := func() *Experiment {
		return &Experiment{
			Name: "e", Model: "m/x", Temperature: ptr(0.0),
			Corpus:            []string{"t"},
			RunsPerTrajectory: map[Band]int{BandStable: 1},
			Arms:              map[string]Arm{ArmControl: {}, ArmTreatment: {}},
		}
	}
	require.NoError(t, ValidateExperiment(base()))
	e := base()
	e.ProcessModel = ProcessModelPersistent
	require.NoError(t, ValidateExperiment(e))
	e = base()
	e.ProcessModel = "sideways"
	require.ErrorContains(t, ValidateExperiment(e), "process_model")
	e = base()
	e.ProcessModel = ProcessModelPersistent
	e.Replay = &ReplaySpec{}
	require.ErrorContains(t, ValidateExperiment(e), "replay")
}
