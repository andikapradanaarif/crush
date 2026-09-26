package eval

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"math/rand/v2"
	"os"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// --- bands ---

func recFor(id, model, baseKey string, outcome Outcome, at time.Time) RunRecord {
	return RunRecord{
		TrajectoryID: id,
		Outcome:      outcome,
		StartedAt:    at,
		BaselineKey:  baseKey,
		Env:          Env{ModelResolved: model, ContentHash: "h"},
	}
}

func TestBands_WindowAndAssign(t *testing.T) {
	t.Parallel()
	b := &Bands{Entries: map[string]BandEntry{}}
	now := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)

	// 25 passes then demotion would need a bad characterization; a
	// clean stable requires two consecutive good characterizations.
	var recs []RunRecord
	for i := range 25 {
		recs = append(recs, recFor("t", "m", "cfg", OutcomePass, now.Add(time.Duration(i)*time.Hour)))
	}
	b.Recompute("t", recs, "h", now, "m", "cfg")
	require.Equal(t, BandMid, b.Band("t")) // First good char → streak 1.
	b.Recompute("t", recs, "h", now.Add(time.Hour), "m", "cfg")
	require.Equal(t, BandStable, b.Band("t")) // Second good → promote.

	// Enough fails to push the windowed p̂ below the stable floor
	// demotes immediately — one bad characterization suffices.
	for i := range 5 {
		recs = append(recs, recFor("t", "m", "cfg", OutcomeFail, now.Add(time.Duration(100+i)*time.Hour)))
	}
	b.Recompute("t", recs, "h", now.Add(110*time.Hour), "m", "cfg")
	require.Equal(t, BandMid, b.Band("t"))
}

func TestBands_NeverPassedTrailing(t *testing.T) {
	t.Parallel()
	b := &Bands{Entries: map[string]BandEntry{}}
	now := time.Now()
	var recs []RunRecord
	for i := range NeverPassedFails {
		recs = append(recs, recFor("t", "m", "c", OutcomeFail, now.Add(time.Duration(i)*time.Minute)))
	}
	b.Recompute("t", recs, "h", now, "m", "c")
	require.Equal(t, BandQuarantined, b.Band("t"))
	require.Equal(t, ReasonNeverPassed, b.Entries["t"].QuarantineReason)

	// With a lifetime pass, the same streak is rot, not never_passed.
	b2 := &Bands{Entries: map[string]BandEntry{}}
	recs2 := append([]RunRecord{recFor("t", "m", "c", OutcomePass, now.Add(-time.Hour))}, recs...)
	b2.Recompute("t", recs2, "h", now, "m", "c")
	require.NotEqual(t, ReasonNeverPassed, b2.Entries["t"].QuarantineReason)
}

func TestBands_ContentHashChangeResets(t *testing.T) {
	t.Parallel()
	b := &Bands{Entries: map[string]BandEntry{}}
	now := time.Now()
	var recs []RunRecord
	for i := range 10 {
		recs = append(recs, recFor("t", "m", "c", OutcomePass, now.Add(time.Duration(i)*time.Minute)))
	}
	b.Recompute("t", recs, "h", now, "m", "c")
	b.Recompute("t", recs, "h", now, "m", "c")
	require.Equal(t, BandStable, b.Band("t"))

	// Corpus revision change → baselines discarded, back to
	// uncharacterized pending re-characterization.
	b.Recompute("t", recs, "h2", now, "m", "c")
	require.Equal(t, BandUncharacterized, b.Band("t"))
	require.Empty(t, b.Entries["t"].Baselines)
}

func TestBands_SuspectCheckAlternation(t *testing.T) {
	t.Parallel()
	b := &Bands{Entries: map[string]BandEntry{}}
	now := time.Now()
	var recs []RunRecord
	for i := range 10 {
		o := OutcomePass
		if i%2 == 0 {
			o = OutcomeFail
		}
		recs = append(recs, recFor("t", "m", "c", o, now.Add(time.Duration(i)*time.Minute)))
	}
	b.Recompute("t", recs, "h", now, "m", "c")
	require.Equal(t, BandQuarantined, b.Band("t"))
	require.Equal(t, ReasonSuspectCheck, b.Entries["t"].QuarantineReason)
}

func TestBands_BaselineKeyedByConfig(t *testing.T) {
	t.Parallel()
	b := &Bands{Entries: map[string]BandEntry{}}
	now := time.Now()
	var recs []RunRecord
	for i := range 6 {
		recs = append(recs, recFor("t", "m", "cfgA", OutcomePass, now.Add(time.Duration(i)*time.Minute)))
	}
	for i := range 6 {
		recs = append(recs, recFor("t", "m", "cfgB", OutcomeFail, now.Add(time.Duration(60+i)*time.Minute)))
	}
	b.Recompute("t", recs, "h", now, "m", "cfgA")
	require.Equal(t, 6, b.Baseline("t", "m", "cfgA").Passes)
	require.Equal(t, 0, b.Baseline("t", "m", "cfgB").Passes)
	require.Equal(t, 6, b.Baseline("t", "m", "cfgB").N)
}

// --- quarantine ---

func quarantineRunner(t *testing.T) (*Runner, string) {
	t.Helper()
	root := newEvalDir(t)
	return &Runner{EvalDir: root, QuarantineRepeats: 3, WorkParent: t.TempDir()}, root
}

func TestQuarantine_CleanFailTrajectory(t *testing.T) {
	t.Parallel()
	r, root := quarantineRunner(t)
	dir := writeTrajectory(t, filepath.Join(root, "corpus"), "t1", nil)
	tr, err := LoadTrajectory(dir)
	require.NoError(t, err)
	reason, err := r.Quarantine(context.Background(), tr, dir)
	require.NoError(t, err)
	require.Empty(t, reason)
}

func TestQuarantine_Vacuous(t *testing.T) {
	t.Parallel()
	r, root := quarantineRunner(t)
	// expect fail, but the check always exits 0.
	dir := writeTrajectory(t, filepath.Join(root, "corpus"), "t1", map[string]any{
		"check_script_body": "#!/bin/bash\nexit 0\n",
	})
	tr, err := LoadTrajectory(dir)
	require.NoError(t, err)
	reason, err := r.Quarantine(context.Background(), tr, dir)
	require.NoError(t, err)
	require.Equal(t, ReasonVacuous, reason)
}

func TestQuarantine_Flaky(t *testing.T) {
	t.Parallel()
	r, root := quarantineRunner(t)
	// Alternating pass/fail on the same state → flaky.
	dir := writeTrajectory(t, filepath.Join(root, "corpus"), "t1", map[string]any{
		// Fresh materialization per rep — the flip marker lives in
		// the shared work parent so it alternates across reps.
		"check_script_body": `#!/bin/bash
f="$(dirname "$EVAL_WORKDIR")/.flip"
if [ -f "$f" ]; then rm "$f"; exit 0; else touch "$f"; exit 1; fi
`,
	})
	tr, err := LoadTrajectory(dir)
	require.NoError(t, err)
	reason, err := r.Quarantine(context.Background(), tr, dir)
	require.NoError(t, err)
	require.Equal(t, ReasonFlaky, reason)
}

func TestQuarantine_CounterexamplePassIsVacuous(t *testing.T) {
	t.Parallel()
	r, root := quarantineRunner(t)
	// Pass-guard whose counterexample doesn't break the check.
	dir := writeTrajectory(t, filepath.Join(root, "corpus"), "t1", map[string]any{
		"check":             map[string]any{"script": "check.sh", "expect_start_state": "pass"},
		"check_script_body": "#!/bin/bash\nexit 0\n",
	})
	patch := `diff --git a/hello.txt b/hello.txt
index ce01362..0000000
--- a/hello.txt
+++ /dev/null
@@ -1 +0,0 @@
-hi
`
	require.NoError(t, os.WriteFile(filepath.Join(dir, "counterexample.patch"), []byte(patch), 0o644))
	tr, err := LoadTrajectory(dir)
	require.NoError(t, err)
	reason, err := r.Quarantine(context.Background(), tr, dir)
	require.NoError(t, err)
	require.Equal(t, ReasonVacuous, reason)
}

// --- experiment end-to-end with a fake driver ---

// fakeRunner inspects the generated .crush.json: the flag under test
// flips whether the marker file gets written. That exercises the whole
// arm-config → materialize → check pipeline.
type fakeRunner struct {
	flag string
}

func (f fakeRunner) Run(_ context.Context, workdir string, _ []string, _ Budget) RunResult {
	data, _ := os.ReadFile(filepath.Join(workdir, ".crush.json"))
	var doc struct {
		Options map[string]any `json:"options"`
	}
	_ = json.Unmarshal(data, &doc)
	// The marker is written when the flag is OFF — so a treatment arm
	// enabling the flag collapses while control (flag off) passes.
	if on, _ := doc.Options[f.flag].(bool); !on {
		_ = os.WriteFile(filepath.Join(workdir, "fixed.marker"), []byte("x"), 0o644)
	}
	return RunResult{Steps: 3, ModelResolved: "mock/m", Tokens: TokenUsage{Input: 10, Output: 5}}
}

func experimentFixture(t *testing.T, r *Runner, root string, flag string) {
	t.Helper()
	// One stable trajectory with a deep baseline + one mid.
	writeTrajectory(t, filepath.Join(root, "corpus"), "stable-t", map[string]any{
		"coverage": map[string]any{"min_steps": 1},
	})
	writeTrajectory(t, filepath.Join(root, "corpus"), "mid-t", nil)

	manifest := &FlagsManifest{Defaults: map[string]any{flag: false}}
	require.NoError(t, os.WriteFile(filepath.Join(root, "flags.json"),
		[]byte(fmt.Sprintf(`{"flag_defaults":{%q:false}}`, flag)), 0o644))

	bands := &Bands{SchemaVersion: 1, Entries: map[string]BandEntry{
		"stable-t": {
			Band:        BandStable,
			Baselines:   map[string]map[string]BaselineCounts{"mock/m": {manifest.keyWith(map[string]any{flag: false}, map[string]any{"$temperature": "0"}): {Passes: 29, N: 30}}},
			ContentHash: mustHash(t, filepath.Join(root, "corpus", "stable-t")),
		},
		"mid-t": {Band: BandMid, ContentHash: mustHash(t, filepath.Join(root, "corpus", "mid-t"))},
	}}
	require.NoError(t, bands.Save(root))
}

func mustHash(t *testing.T, dir string) string {
	t.Helper()
	h, err := ContentHash(dir)
	require.NoError(t, err)
	return h
}

// The arm gate end to end: a run whose check passes but whose arm
// coverage starves lands inconclusive with coverage_scope "arm", and
// a trajectory-coverage miss records "trajectory".
func TestExecuteRun_ArmCoverageGate(t *testing.T) {
	t.Parallel()
	root := newEvalDir(t)
	trajDir := writeTrajectory(t, filepath.Join(root, "corpus"), "gate-t", nil)
	tr, err := LoadTrajectory(trajDir)
	require.NoError(t, err)
	r := &Runner{
		EvalDir:    root,
		Driver:     fakeRunner{flag: "debug"},
		WorkParent: t.TempDir(),
		RNG:        rand.New(rand.NewPCG(1, 2)),
	}
	exp := &Experiment{Name: "exp1", Model: "mock/m", Temperature: ptr(0.0)}
	manifest := &FlagsManifest{Defaults: map[string]any{"debug": false}}
	off := Arm{Config: ArmConfig{Options: map[string]any{"debug": false}}}

	// Flag off → fakeRunner writes the marker → check passes; the arm
	// demands more steps than the driver's fixed 3 → inconclusive.
	starving := off
	starving.Coverage = Coverage{"min_steps": 100}
	rec, err := r.ExecuteRun(context.Background(), exp, tr, trajDir, ArmTreatment, starving, manifest, 1, "inv")
	require.NoError(t, err)
	require.Equal(t, OutcomeInconclusive, rec.Outcome)
	require.Equal(t, "arm", rec.CheckDetail["coverage_scope"])
	require.Equal(t, "min_steps", rec.CheckDetail["coverage_key"])

	// Same run shape under a starving trajectory predicate → the
	// trajectory scope is recorded instead.
	tr.Coverage = Coverage{"min_steps": 100}
	rec, err = r.ExecuteRun(context.Background(), exp, tr, trajDir, ArmControl, off, manifest, 1, "inv")
	require.NoError(t, err)
	require.Equal(t, OutcomeInconclusive, rec.Outcome)
	require.Equal(t, "trajectory", rec.CheckDetail["coverage_scope"])
	require.Equal(t, "min_steps", rec.CheckDetail["coverage_key"])

	// No coverage anywhere → pass, no scope recorded.
	tr.Coverage = nil
	rec, err = r.ExecuteRun(context.Background(), exp, tr, trajDir, ArmControl, off, manifest, 1, "inv")
	require.NoError(t, err)
	require.Equal(t, OutcomePass, rec.Outcome)
	_, ok := rec.CheckDetail["coverage_scope"]
	require.False(t, ok)
}

func TestRunExperiment_CatastrophicFires(t *testing.T) {
	t.Parallel()
	root := newEvalDir(t)
	flag := "debug"
	r := &Runner{
		EvalDir:        root,
		Driver:         fakeRunner{flag: flag},
		WorkParent:     t.TempDir(),
		PermReplicates: 500,
		RNG:            rand.New(rand.NewPCG(7, 8)),
	}
	experimentFixture(t, r, root, flag)

	// Treatment turns the flag ON → fakeRunner writes the marker →
	// check passes; control leaves it off → fails. To make treatment
	// COLLAPSE instead, invert: treatment has flag false? Simpler:
	// flip which arm enables the marker.
	exp := &Experiment{
		Name: "exp1", Model: "mock/m", Temperature: ptr(0.0),
		Corpus:            []string{"*"},
		RunsPerTrajectory: map[Band]int{BandStable: 3, BandMid: 3, BandUncharacterized: 3},
		Arms: map[string]Arm{
			// Control turns the flag on (marker written → pass);
			// treatment leaves it off (fail) — treatment collapses.
			"control":   {Config: ArmConfig{Options: map[string]any{flag: false}}},
			"treatment": {Config: ArmConfig{Options: map[string]any{flag: true}}},
		},
	}

	rep, err := r.RunExperiment(context.Background(), exp)
	require.NoError(t, err)
	require.NotEmpty(t, rep.Catastrophic)
	require.Contains(t, rep.Catastrophic[0], "stable-t")

	// Records were written for both arms.
	recs, err := r.LoadExperimentRecords("exp1")
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(recs), 12) // 2 trajs × 2 arms × 3.
}

func TestRunExperiment_DiffuseAlarmOnUniformShift(t *testing.T) {
	t.Parallel()
	root := newEvalDir(t)
	flag := "debug"
	r := &Runner{
		EvalDir:        root,
		Driver:         fakeRunner{flag: flag},
		WorkParent:     t.TempDir(),
		PermReplicates: 2000,
		RNG:            rand.New(rand.NewPCG(3, 4)),
	}
	// All mid-band trajectories.
	for i := range 6 {
		id := fmt.Sprintf("mid-%d", i)
		writeTrajectory(t, filepath.Join(root, "corpus"), id, nil)
	}
	require.NoError(t, os.WriteFile(filepath.Join(root, "flags.json"),
		[]byte(fmt.Sprintf(`{"flag_defaults":{%q:false}}`, flag)), 0o644))
	bands := &Bands{SchemaVersion: 1, Entries: map[string]BandEntry{}}
	for i := range 6 {
		id := fmt.Sprintf("mid-%d", i)
		bands.Entries[id] = BandEntry{Band: BandMid, ContentHash: mustHash(t, filepath.Join(root, "corpus", id))}
	}
	require.NoError(t, bands.Save(root))

	exp := &Experiment{
		Name: "exp2", Model: "mock/m", Temperature: ptr(0.0),
		Corpus:            []string{"band:mid"},
		RunsPerTrajectory: map[Band]int{BandMid: 5},
		Arms: map[string]Arm{
			"control":   {Config: ArmConfig{Options: map[string]any{flag: false}}},
			"treatment": {Config: ArmConfig{Options: map[string]any{flag: true}}},
		},
	}
	rep, err := r.RunExperiment(context.Background(), exp)
	require.NoError(t, err)
	require.Less(t, rep.DiffuseP, 0.05)
	require.Empty(t, rep.Catastrophic)
}

// A quiet diffuse-only experiment must read powered — the report
// RunExperiment returns has to carry Evaluate's DiffusePairs, else
// every all-uncharacterized corpus reports INCONCLUSIVE while its
// p-value was computed from real pairs.
func TestRunExperiment_QuietDiffusePowered(t *testing.T) {
	t.Parallel()
	root := newEvalDir(t)
	flag := "debug"
	r := &Runner{
		EvalDir:        root,
		Driver:         fakeRunner{flag: flag},
		WorkParent:     t.TempDir(),
		PermReplicates: 500,
		RNG:            rand.New(rand.NewPCG(5, 6)),
	}
	for i := range 4 {
		id := fmt.Sprintf("mid-%d", i)
		writeTrajectory(t, filepath.Join(root, "corpus"), id, nil)
	}
	require.NoError(t, os.WriteFile(filepath.Join(root, "flags.json"),
		[]byte(fmt.Sprintf(`{"flag_defaults":{%q:false}}`, flag)), 0o644))
	bands := &Bands{SchemaVersion: 1, Entries: map[string]BandEntry{}}
	for i := range 4 {
		id := fmt.Sprintf("mid-%d", i)
		bands.Entries[id] = BandEntry{Band: BandMid, ContentHash: mustHash(t, filepath.Join(root, "corpus", id))}
	}
	require.NoError(t, bands.Save(root))

	// Identical arm intents — the marker writes on flag-off so both
	// arms pass, d_t≈0, nothing fires. (fakeRunner reports no
	// ResolvedOptions, so the noop-flag alarm stays silent.)
	exp := &Experiment{
		Name: "expq", Model: "mock/m", Temperature: ptr(0.0),
		Corpus:            []string{"band:mid"},
		RunsPerTrajectory: map[Band]int{BandMid: 3},
		Arms: map[string]Arm{
			"control":   {Config: ArmConfig{Options: map[string]any{flag: false}}},
			"treatment": {Config: ArmConfig{Options: map[string]any{flag: false}}},
		},
	}
	rep, err := r.RunExperiment(context.Background(), exp)
	require.NoError(t, err)
	require.False(t, rep.Fired(0.05))
	require.Equal(t, 4, rep.DiffusePairs)
	require.True(t, rep.Powered())
	require.Contains(t, rep.Summary(0.05), "verdict: PASS")
}

// No eligible catastrophic trajectory AND no diffuse pairs → the
// quiet report is INCONCLUSIVE through the real plumbing too.
func TestRunExperiment_UnpoweredIsInconclusive(t *testing.T) {
	t.Parallel()
	root := newEvalDir(t)
	flag := "debug"
	r := &Runner{
		EvalDir:        root,
		Driver:         fakeRunner{flag: flag},
		WorkParent:     t.TempDir(),
		PermReplicates: 500,
		RNG:            rand.New(rand.NewPCG(9, 10)),
	}
	// Stable-band trajectory with no baseline → never eligible, and
	// stable feeds no diffuse pairs.
	writeTrajectory(t, filepath.Join(root, "corpus"), "stable-t", nil)
	require.NoError(t, os.WriteFile(filepath.Join(root, "flags.json"),
		[]byte(fmt.Sprintf(`{"flag_defaults":{%q:false}}`, flag)), 0o644))
	bands := &Bands{SchemaVersion: 1, Entries: map[string]BandEntry{
		"stable-t": {Band: BandStable, ContentHash: mustHash(t, filepath.Join(root, "corpus", "stable-t"))},
	}}
	require.NoError(t, bands.Save(root))

	// Flag off on both arms → all pass: a collapse would trip the
	// smoke alarm into FAIL, not the quiet-INCONCLUSIVE under test.
	exp := &Experiment{
		Name: "expu", Model: "mock/m", Temperature: ptr(0.0),
		Corpus:            []string{"*"},
		RunsPerTrajectory: map[Band]int{BandStable: 3},
		Arms: map[string]Arm{
			"control":   {Config: ArmConfig{Options: map[string]any{flag: false}}},
			"treatment": {Config: ArmConfig{Options: map[string]any{flag: false}}},
		},
	}
	rep, err := r.RunExperiment(context.Background(), exp)
	require.NoError(t, err)
	require.False(t, rep.Fired(0.05))
	require.False(t, rep.Powered())
	require.Contains(t, rep.Summary(0.05), "verdict: INCONCLUSIVE")
}

func TestWriteArmConfig_CollisionAndContent(t *testing.T) {
	t.Parallel()
	wd := t.TempDir()
	exp := &Experiment{Model: "hyper/x", Temperature: ptr(0.0)}
	arm := Arm{Config: ArmConfig{Options: map[string]any{"flag_a": true}}}
	require.NoError(t, WriteArmConfig(wd, exp, arm, &FlagsManifest{Defaults: map[string]any{}}))

	rc, err := os.ReadFile(filepath.Join(wd, ".crushrc"))
	require.NoError(t, err)
	require.Contains(t, string(rc), "model large hyper/x")
	require.Contains(t, string(rc), "--temperature 0")

	js, err := os.ReadFile(filepath.Join(wd, ".crush.json"))
	require.NoError(t, err)
	require.Contains(t, string(js), `"flag_a": true`)
	require.Contains(t, string(js), `"disable_metrics": true`)

	// A pre-existing config is an error, not a silent override.
	wd2 := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(wd2, "crushrc"), []byte("x"), 0o644))
	require.Error(t, WriteArmConfig(wd2, exp, arm, &FlagsManifest{Defaults: map[string]any{}}))
}

func ptr[T any](v T) *T { return &v }

func TestBands_SuspectCheckIgnoresArmCorrelation(t *testing.T) {
	t.Parallel()
	b := &Bands{Entries: map[string]BandEntry{}}
	now := time.Now()
	// A real treatment effect — control passes, treatment fails every
	// interleaved round — must NOT read as a flaky check: each arm is
	// individually constant.
	var recs []RunRecord
	for i := range 12 {
		c := recFor("t", "m", "c", OutcomePass, now.Add(time.Duration(2*i)*time.Minute))
		c.Arm = "control"
		tr := recFor("t", "m", "c", OutcomeFail, now.Add(time.Duration(2*i+1)*time.Minute))
		tr.Arm = "treatment"
		recs = append(recs, c, tr)
	}
	b.Recompute("t", recs, "h", now, "m", "c")
	require.NotEqual(t, ReasonSuspectCheck, b.Entries["t"].QuarantineReason)
}

func TestBands_StaleModelBaselineDoesNotHoldStable(t *testing.T) {
	t.Parallel()
	b := &Bands{Entries: map[string]BandEntry{}}
	now := time.Now()
	// Deep baseline under the OLD model; the current pin has nothing.
	var recs []RunRecord
	for i := range 25 {
		recs = append(recs, recFor("t", "old/model", "cfg", OutcomePass, now.Add(time.Duration(i)*time.Minute)))
	}
	b.Recompute("t", recs, "h", now, "old/model", "cfg")
	b.Recompute("t", recs, "h", now, "old/model", "cfg")
	require.Equal(t, BandStable, b.Band("t"))

	// Re-pin: current condition is a different model — the stale
	// baseline must not hold the band.
	b.Recompute("t", recs, "h", now, "new/model", "cfg")
	require.Equal(t, BandUncharacterized, b.Band("t"))
}

func TestCheckRequires(t *testing.T) {
	t.Parallel()
	missing := CheckRequires(&Trajectory{
		Requires: Requires{Tools: []string{"definitely-not-a-real-binary-xyz"}},
	})
	require.Equal(t, []string{"tool:definitely-not-a-real-binary-xyz"}, missing)

	require.Empty(t, CheckRequires(&Trajectory{
		Requires: Requires{Tools: []string{"go"}, OS: []string{runtime.GOOS}},
	}))
	require.NotEmpty(t, CheckRequires(&Trajectory{
		Requires: Requires{OS: []string{"plan9"}},
	}))
}

func TestWriteArmConfig_MergesJSONConfig(t *testing.T) {
	t.Parallel()
	exp := &Experiment{Model: "hyper/x"}
	arm := Arm{Config: ArmConfig{Options: map[string]any{"flag_a": true}}}

	// A start-state .crush.json merges: its keys survive, arm wins.
	wd := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(wd, ".crush.json"),
		[]byte(`{"options":{"fixture_key":"keep","flag_a":false},"other":"x"}`), 0o644))
	require.NoError(t, WriteArmConfig(wd, exp, arm, &FlagsManifest{Defaults: map[string]any{}}))
	raw, err := os.ReadFile(filepath.Join(wd, ".crush.json"))
	require.NoError(t, err)
	require.Contains(t, string(raw), `"fixture_key": "keep"`)
	require.Contains(t, string(raw), `"flag_a": true`)
	require.Contains(t, string(raw), `"other": "x"`)

	// crush.json is lower precedence than .crush.json — allowed.
	wd2 := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(wd2, "crush.json"), []byte(`{}`), 0o644))
	require.NoError(t, WriteArmConfig(wd2, exp, arm, &FlagsManifest{Defaults: map[string]any{}}))
}

func TestBands_PinSpellingVsResolved(t *testing.T) {
	t.Parallel()
	b := &Bands{Entries: map[string]BandEntry{}}
	now := time.Now()
	// Records stamped with the pin's alias spelling but a canonical
	// resolved model — banding must follow the resolved key.
	var recs []RunRecord
	for i := range 25 {
		r := recFor("t", "canonical/x", "cfg", OutcomePass, now.Add(time.Duration(i)*time.Minute))
		r.Env.ModelPin = "alias/x"
		recs = append(recs, r)
	}
	b.Recompute("t", recs, "h", now, "alias/x", "cfg")
	b.Recompute("t", recs, "h", now, "alias/x", "cfg")
	require.Equal(t, BandStable, b.Band("t"))

	// A different pin's records never count toward this condition.
	var other []RunRecord
	for i := range 25 {
		r := recFor("t", "canonical/x", "cfg", OutcomePass, now.Add(time.Duration(i)*time.Minute))
		r.Env.ModelPin = "other/x"
		other = append(other, r)
	}
	b2 := &Bands{Entries: map[string]BandEntry{}}
	b2.Recompute("t", other, "h", now, "alias/x", "cfg")
	require.Equal(t, BandUncharacterized, b2.Band("t"))
}

func TestRunExperiment_AllSkippedFailsClosed(t *testing.T) {
	t.Parallel()
	root := newEvalDir(t)
	r := &Runner{EvalDir: root, Driver: fakeRunner{flag: "debug"}, WorkParent: t.TempDir()}
	require.NoError(t, os.WriteFile(filepath.Join(root, "flags.json"),
		[]byte(`{"flag_defaults":{"debug":false}}`), 0o644))
	writeTrajectory(t, filepath.Join(root, "corpus"), "needs-missing", map[string]any{
		"requires": map[string]any{"tools": []string{"definitely-not-a-real-binary-xyz"}},
	})

	exp := &Experiment{
		Name: "exp-skip", Model: "mock/m", Corpus: []string{"*"}, Temperature: ptr(0.0),
		RunsPerTrajectory: map[Band]int{BandUncharacterized: 2},
		Arms: map[string]Arm{
			"control":   {Config: ArmConfig{Options: map[string]any{"debug": false}}},
			"treatment": {Config: ArmConfig{Options: map[string]any{"debug": true}}},
		},
	}
	rep, err := r.RunExperiment(context.Background(), exp)
	require.Error(t, err) // Fail closed — no PASS on zero samples.
	require.NotEmpty(t, rep.Skipped)
}

// fakeRunnerFail fails every run regardless of arm — the
// model-drift/rot case the coincidence detector exists for.
type fakeRunnerFail struct{}

func (fakeRunnerFail) Run(_ context.Context, _ string, _ []string, _ Budget) RunResult {
	return RunResult{Steps: 3, ModelResolved: "mock/m"}
}

func TestRunExperiment_CoincidentCollapse(t *testing.T) {
	t.Parallel()
	root := newEvalDir(t)
	flag := "debug"
	r := &Runner{
		EvalDir:        root,
		Driver:         fakeRunnerFail{},
		WorkParent:     t.TempDir(),
		PermReplicates: 500,
		RNG:            rand.New(rand.NewPCG(7, 8)),
	}
	experimentFixture(t, r, root, flag)

	exp := &Experiment{
		Name: "exp-coincident", Model: "mock/m", Temperature: ptr(0.0),
		Corpus:            []string{"*"},
		RunsPerTrajectory: map[Band]int{BandStable: 3, BandMid: 3, BandUncharacterized: 3},
		Arms: map[string]Arm{
			"control":   {Config: ArmConfig{Options: map[string]any{flag: false}}},
			"treatment": {Config: ArmConfig{Options: map[string]any{flag: true}}},
		},
	}
	rep, err := r.RunExperiment(context.Background(), exp)
	require.NoError(t, err)
	// Both arms failed vs the same deep baseline — the gate must
	// attribute rot/drift, not the flag.
	require.NotEmpty(t, rep.Coincident)
	require.Empty(t, rep.Catastrophic)
}

// fakeRunnerResolved reports a fixed resolved projection regardless
// of arm — the flag under test no-ops.
type fakeRunnerResolved struct {
	resolved map[string]any
	pass     bool
}

func (f fakeRunnerResolved) Run(_ context.Context, workdir string, _ []string, _ Budget) RunResult {
	if f.pass {
		_ = os.WriteFile(filepath.Join(workdir, "fixed.marker"), []byte("x"), 0o644)
	}
	return RunResult{Steps: 3, ModelResolved: "mock/m", ResolvedOptions: f.resolved}
}

// Identical resolved projections under differing arm intents →
// noop-flag alarm: the pairing is a guaranteed null.
func TestRunExperiment_NoopFlagAlarm(t *testing.T) {
	t.Parallel()
	root := newEvalDir(t)
	r := &Runner{
		EvalDir:    root,
		Driver:     fakeRunnerResolved{resolved: map[string]any{"debug": false}, pass: true},
		WorkParent: t.TempDir(),
		RNG:        rand.New(rand.NewPCG(1, 2)),
	}
	writeTrajectory(t, filepath.Join(root, "corpus"), "stable-t", nil)
	require.NoError(t, os.WriteFile(filepath.Join(root, "flags.json"),
		[]byte(`{"flag_defaults":{"debug":false}}`), 0o644))
	bands := &Bands{SchemaVersion: 1, Entries: map[string]BandEntry{
		"stable-t": {Band: BandMid, ContentHash: mustHash(t, filepath.Join(root, "corpus", "stable-t"))},
	}}
	require.NoError(t, bands.Save(root))

	exp := &Experiment{
		Name: "noop-exp", Model: "mock/m", Temperature: ptr(0.0),
		Arms: map[string]Arm{
			"control":   {Config: ArmConfig{Options: map[string]any{"debug": false}}},
			"treatment": {Config: ArmConfig{Options: map[string]any{"debug": true}}},
		},
		Corpus:            []string{"*"},
		RunsPerTrajectory: map[Band]int{BandMid: 3},
	}
	rep, err := r.RunExperiment(t.Context(), exp)
	require.NoError(t, err)
	require.Contains(t, rep.NoopFlags, "stable-t")
	require.True(t, rep.Fired(0.05))
}

// expected_exclusion declares the designed death: a met expectation
// reports satisfied and consumes the differential; a miss is its own
// alarm — the regime never engaged, so the run was vacuous.
func TestEvaluate_ExpectedExclusion(t *testing.T) {
	t.Parallel()
	bands := &Bands{Entries: map[string]BandEntry{"t1": {Band: BandMid}}}
	corpus := map[string]bool{"t1": true}
	mkRecs := func(arm string, n int, out Outcome, class string) []RunRecord {
		recs := make([]RunRecord, 0, n)
		for range n {
			recs = append(recs, RunRecord{TrajectoryID: "t1", Arm: arm, Outcome: out, ErrorClass: class})
		}
		return recs
	}
	exp := func(min int) *Experiment {
		return &Experiment{
			Name: "ee", Model: "mock/m",
			Arms:              map[string]Arm{ArmControl: {}, ArmTreatment: {}},
			ExpectedExclusion: &ExpectedExclusion{Arm: ArmControl, ErrorClass: "window_cap_enforced", Min: min},
		}
	}
	deaths := append(mkRecs(ArmControl, 5, OutcomeError, "window_cap_enforced"),
		mkRecs(ArmTreatment, 5, OutcomePass, "")...)

	// Declared death met — satisfied; the Fisher is one-sided
	// treatment-heavy, so a control-heavy split alarms nothing either
	// way, but the satisfaction bookkeeping is the contract.
	rep := Evaluate(exp(3), bands, "", deaths, corpus, 0.05, 500, rand.New(rand.NewPCG(1, 2)))
	require.Equal(t, []string{"t1"}, rep.ExpectedExclusionSatisfied)
	require.Empty(t, rep.ExpectedExclusionMissed)
	require.Empty(t, rep.ExcludedDifferential)
	require.False(t, rep.Fired(0.05))

	// Below the declared minimum — the regime under-engaged; the
	// miss is the alarm.
	rep = Evaluate(exp(6), bands, "", deaths, corpus, 0.05, 500, rand.New(rand.NewPCG(1, 2)))
	require.Empty(t, rep.ExpectedExclusionSatisfied)
	require.Equal(t, []string{"t1"}, rep.ExpectedExclusionMissed)
	require.True(t, rep.Fired(0.05))

	// Treatment-side collapse stays alarming: declared on control
	// but treatment is the arm that died — the expectation missed
	// and the directional Fisher still evaluates.
	reverse := append(mkRecs(ArmControl, 5, OutcomePass, ""),
		mkRecs(ArmTreatment, 5, OutcomeError, "window_cap_enforced")...)
	rep = Evaluate(exp(3), bands, "", reverse, corpus, 0.05, 500, rand.New(rand.NewPCG(1, 2)))
	require.Empty(t, rep.ExpectedExclusionSatisfied)
	require.Equal(t, []string{"t1"}, rep.ExpectedExclusionMissed)
	require.Equal(t, []string{"t1"}, rep.ExcludedDifferential)

	// No declaration — satisfied/missed machinery is inert.
	exp2 := &Experiment{Name: "ee2", Model: "mock/m", Arms: map[string]Arm{ArmControl: {}, ArmTreatment: {}}}
	rep = Evaluate(exp2, bands, "", deaths, corpus, 0.05, 500, rand.New(rand.NewPCG(1, 2)))
	require.Empty(t, rep.ExpectedExclusionSatisfied)
	require.Empty(t, rep.ExpectedExclusionMissed)
}

// A manifest key that isn't a config.Options field must be rejected —
// otherwise the flag silently no-ops and both arms resolve identically.
func TestLoadFlagsManifest_RejectsUnknownKeys(t *testing.T) {
	t.Parallel()
	root := newEvalDir(t)
	require.NoError(t, os.WriteFile(filepath.Join(root, "flags.json"),
		[]byte(`{"flag_defaults":{"notebook_stub_superseeded":false}}`), 0o644))
	_, err := LoadFlagsManifest(root)
	require.ErrorContains(t, err, "not a config.Options key")
}

// Resolved-vs-intent key divergence: the record's resolved
// baseline_key must be what banding joins on — manifest intent keys
// would orphan every record under a different namespace.
func TestRecomputeAll_ResolvedKeyJoins(t *testing.T) {
	t.Parallel()
	root := newEvalDir(t)
	writeTrajectory(t, filepath.Join(root, "corpus"), "t1", nil)
	require.NoError(t, os.WriteFile(filepath.Join(root, "flags.json"),
		[]byte(`{"flag_defaults":{"debug":false}}`), 0o644))
	manifest, err := LoadFlagsManifest(root)
	require.NoError(t, err)

	// Resolved says true where intent says false — stands in for the
	// nil-vs-default divergence on non-materialized options.
	resolved := map[string]any{"debug": true}
	resolvedKey := manifest.BaselineKey(resolved)
	intentKey := manifest.BaselineKey(nil)
	require.NotEqual(t, intentKey, resolvedKey)

	r := &Runner{EvalDir: root}
	hash := mustHash(t, filepath.Join(root, "corpus", "t1"))
	base := time.Now()
	for i := range 30 {
		require.NoError(t, r.appendRecord(RunRecord{
			Experiment: "char", TrajectoryID: "t1", Arm: "baseline",
			BaselineKey: resolvedKey, ResolvedOptions: resolved,
			Outcome: OutcomePass, StartedAt: base.Add(time.Duration(i) * time.Second),
			Env: Env{ModelPin: "mock/m", ModelResolved: "mock/m", ContentHash: hash},
		}))
	}

	bands := &Bands{Entries: map[string]BandEntry{}}
	corpus, err := LoadCorpus(root)
	require.NoError(t, err)
	require.NoError(t, r.RecomputeAll(bands, corpus, manifest, "mock/m", "0"))

	entry := bands.Entry("t1")
	// The baseline accumulated under the RESOLVED key — intent key
	// must stay empty, and 30/30 passes move the band off
	// uncharacterized (stable itself needs a promotion streak).
	require.Equal(t, 30, entry.Baselines["mock/m"][resolvedKey].N)
	require.Zero(t, entry.Baselines["mock/m"][intentKey].N)
	require.Equal(t, BandMid, entry.Band)
}

// Harness invariants can't be arm-overridden — data_directory
// relocation would break telemetry/session-DB paths.
func TestWriteArmConfig_RejectsHarnessInvariants(t *testing.T) {
	t.Parallel()
	wd := t.TempDir()
	exp := &Experiment{Model: "hyper/x", Temperature: ptr(0.0)}
	manifest := &FlagsManifest{Defaults: map[string]any{"data_directory": "/x"}}
	err := WriteArmConfig(wd, exp, Arm{Config: ArmConfig{Options: map[string]any{"data_directory": "evil"}}}, manifest)
	require.ErrorContains(t, err, "harness manages")
	err = WriteArmConfig(t.TempDir(), exp, Arm{Config: ArmConfig{Options: map[string]any{"disable_metrics": false}}}, manifest)
	require.ErrorContains(t, err, "harness manages")
}

// A fixture pinning a manifest flag — in EITHER config file — keys
// the trajectory under a foreign condition forever.
func TestWriteArmConfig_RejectsFixtureManifestFlags(t *testing.T) {
	t.Parallel()
	manifest := &FlagsManifest{Defaults: map[string]any{"auto_lsp": true}}
	exp := &Experiment{Model: "hyper/x", Temperature: ptr(0.0)}
	arm := Arm{Config: ArmConfig{Options: map[string]any{"auto_lsp": false}}}

	wd := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(wd, ".crush.json"),
		[]byte(`{"options":{"auto_lsp":false}}`), 0o644))
	require.ErrorContains(t, WriteArmConfig(wd, exp, arm, manifest), "manifest flag")

	// crush.json too — lower precedence, same trap.
	wd2 := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(wd2, "crush.json"),
		[]byte(`{"options":{"auto_lsp":false}}`), 0o644))
	require.ErrorContains(t, WriteArmConfig(wd2, exp, arm, manifest), "manifest flag")
}

// Experiments must pin temperature — unpinned arms land in the
// "default" temp cell no characterized baseline joins.
func TestValidateExperiment_RequiresTemperature(t *testing.T) {
	t.Parallel()
	exp := &Experiment{
		Name: "x", Model: "mock/m", Corpus: []string{"*"},
		RunsPerTrajectory: map[Band]int{BandMid: 1},
		Arms: map[string]Arm{
			ArmControl:   {},
			ArmTreatment: {},
		},
	}
	require.ErrorContains(t, ValidateExperiment(exp), "temperature")
}

// --- primary endpoint, power gate, a/a arm, de-biased metrics (#105) ---

func TestValidateExperiment_Primary(t *testing.T) {
	t.Parallel()
	base := func() *Experiment {
		return &Experiment{
			Name: "x", Model: "mock/m", Temperature: ptr(0.0), Corpus: []string{"*"},
			RunsPerTrajectory: map[Band]int{BandMid: 1},
			Arms:              map[string]Arm{ArmControl: {}, ArmTreatment: {}},
		}
	}

	exp := base()
	exp.Primary = &Primary{Metric: "request.prompt_tokens_peak", Direction: PrimaryDecrease, MDE: 0.15}
	require.NoError(t, ValidateExperiment(exp))

	exp = base()
	exp.Primary = &Primary{Metric: "steps", Direction: "sideways", MDE: 0.1}
	require.ErrorContains(t, ValidateExperiment(exp), "direction")

	exp = base()
	exp.Primary = &Primary{Metric: "steps", Direction: PrimaryDecrease, MDE: 0}
	require.ErrorContains(t, ValidateExperiment(exp), "mde")

	exp = base()
	exp.Primary = &Primary{Metric: "steps", Direction: PrimaryDecrease, MDE: 1.5}
	require.ErrorContains(t, ValidateExperiment(exp), "mde")

	exp = base()
	exp.Primary = &Primary{Metric: "prompt_per_request", Direction: PrimaryDecrease, MDE: 0.1}
	require.ErrorContains(t, ValidateExperiment(exp), "unknown primary metric")

	// weighted_cost needs its pinned prices.
	exp = base()
	exp.Primary = &Primary{Metric: "weighted_cost", Direction: PrimaryDecrease, MDE: 0.1}
	require.ErrorContains(t, ValidateExperiment(exp), "cost_weights")
	exp.CostWeights = &CostWeights{CacheRead: 0.1, Output: 4}
	require.NoError(t, ValidateExperiment(exp))

	// max_pass_drop bounds the guardrail.
	exp.Primary.MaxPassDrop = 0.1
	require.NoError(t, ValidateExperiment(exp))
	exp.Primary.MaxPassDrop = -0.05
	require.ErrorContains(t, ValidateExperiment(exp), "max_pass_drop")
	exp.Primary.MaxPassDrop = 1
	require.ErrorContains(t, ValidateExperiment(exp), "max_pass_drop")

	exp = base()
	exp.CostWeights = &CostWeights{CacheRead: -0.1}
	require.ErrorContains(t, ValidateExperiment(exp), "cost_weights")
}

func TestValidateExperiment_ExpectedExclusion(t *testing.T) {
	t.Parallel()
	base := func() *Experiment {
		return &Experiment{
			Name: "x", Model: "mock/m", Temperature: ptr(0.0), Corpus: []string{"*"},
			RunsPerTrajectory: map[Band]int{BandMid: 1},
			Arms: map[string]Arm{
				ArmControl:   {Config: ArmConfig{Options: map[string]any{"enforce_context_window": true}}},
				ArmTreatment: {},
			},
		}
	}

	exp := base()
	exp.ExpectedExclusion = &ExpectedExclusion{Arm: ArmControl, ErrorClass: "window_cap_enforced", Min: 5}
	require.NoError(t, ValidateExperiment(exp))

	// The differential only compares control and treatment.
	exp = base()
	exp.ExpectedExclusion = &ExpectedExclusion{Arm: "observer", ErrorClass: "window_cap_enforced", Min: 1}
	require.ErrorContains(t, ValidateExperiment(exp), "expected_exclusion.arm")

	exp = base()
	exp.ExpectedExclusion = &ExpectedExclusion{Arm: ArmControl, Min: 1}
	require.ErrorContains(t, ValidateExperiment(exp), "error_class is required")

	exp = base()
	exp.ExpectedExclusion = &ExpectedExclusion{Arm: ArmControl, ErrorClass: "window_cap_enforced", Min: 0}
	require.ErrorContains(t, ValidateExperiment(exp), "min must be >= 1")

	// The manufactured class can't occur on an unenforced arm —
	// unmeetable expectations fail at load, not at runtime.
	exp = base()
	exp.ExpectedExclusion = &ExpectedExclusion{Arm: ArmTreatment, ErrorClass: "window_cap_enforced", Min: 1}
	exp.Arms[ArmTreatment] = Arm{Config: ArmConfig{Options: map[string]any{"enforce_context_window": false}}}
	require.ErrorContains(t, ValidateExperiment(exp), "enforce_context_window")

	// A non-manufactured class needs no flag.
	exp = base()
	exp.ExpectedExclusion = &ExpectedExclusion{Arm: ArmControl, ErrorClass: "provider_server", Min: 1}
	require.NoError(t, ValidateExperiment(exp))
}

func TestPrimaryMetricFunc_WeightedCost(t *testing.T) {
	t.Parallel()
	exp := &Experiment{CostWeights: &CostWeights{CacheRead: 0.1, Output: 4}}
	f, err := primaryMetricFunc(exp, "weighted_cost")
	require.NoError(t, err)
	rec := &RunRecord{Tokens: TokenUsage{Input: 1000, CacheRead: 10000, Output: 500}}
	// 1000 + 0.1*10000 + 4*500 = 4000.
	require.InDelta(t, 4000, f(rec), 1e-9)

	// Sidecar spend prices at the same class rates.
	rec.GeneratorTokens = &GeneratorTokens{Input: 100, Output: 50, CacheRead: 200}
	// 4000 + 100 + 0.1*200 + 4*50 = 4320.
	require.InDelta(t, 4320, f(rec), 1e-9)

	// The closed registry rejects ratios by construction — there is
	// no grammar for "X/steps".
	_, err = primaryMetricFunc(exp, "tokens.input_per_step")
	require.ErrorContains(t, err, "unknown primary metric")

	f, err = primaryMetricFunc(exp, "request.prompt_tokens_peak")
	require.NoError(t, err)
	rec.Request = &RequestStats{PromptTokensPeak: 12345}
	require.InDelta(t, 12345, f(rec), 1e-9)
}

func TestRequiredSamples(t *testing.T) {
	t.Parallel()
	// n = ceil(12.372 * (cv/mde)^2): the seed steps CV (0.11) at a
	// 5% MDE needs ~60/arm — far past the old n=3 default.
	require.Equal(t, 60, RequiredSamples(0.11, 0.05))
	// prompt_tokens_peak's tight CV (0.047) resolves 15% at n=2.
	require.Equal(t, 2, RequiredSamples(0.047, 0.15))
	require.Zero(t, RequiredSamples(0, 0.1))
	require.Zero(t, RequiredSamples(0.1, 0))
	// Schema-valid extremes must refuse, not wrap negative and open
	// the gate — the clamp keeps "required" beyond any possible plan.
	require.Equal(t, math.MaxInt32, RequiredSamples(1e200, 1e-160))
}

func TestLoadNoise(t *testing.T) {
	t.Parallel()
	root := newEvalDir(t)
	n, err := LoadNoise(root) // Missing file → empty, not an error.
	require.NoError(t, err)
	require.Empty(t, n.CV)

	require.NoError(t, os.WriteFile(filepath.Join(root, "noise.json"),
		[]byte(`{"cv":{"steps":0.11,"request.prompt_tokens_peak":0.047}}`), 0o644))
	n, err = LoadNoise(root)
	require.NoError(t, err)
	require.InDelta(t, 0.11, n.CV["steps"], 1e-9)

	require.NoError(t, os.WriteFile(filepath.Join(root, "noise.json"),
		[]byte(`{"cv":{"steps":0}}`), 0o644))
	_, err = LoadNoise(root)
	require.ErrorContains(t, err, "cv")
}

func TestCheckPower(t *testing.T) {
	t.Parallel()
	noise := &NoiseFile{CV: map[string]float64{"steps": 0.11, "request.prompt_tokens_peak": 0.047}}
	trajs := []*Trajectory{{ID: "a"}, {ID: "b"}, {ID: "c"}, {ID: "d"}}
	bands := &Bands{Entries: map[string]BandEntry{}}
	for _, tr := range trajs {
		bands.Entries[tr.ID] = BandEntry{Band: BandMid}
	}
	exp := &Experiment{
		RunsPerTrajectory: map[Band]int{BandMid: 3},
		Primary:           &Primary{Metric: "steps", Direction: PrimaryDecrease, MDE: 0.05},
	}
	// 4 trajs × 3 = 12/arm < 60 required → refuse with the count.
	_, _, err := checkPower(exp, trajs, bands, noise)
	require.ErrorContains(t, err, "power gate")
	require.ErrorContains(t, err, "60")

	// The tight-CV metric resolves at n=2 → the same plan passes.
	exp.Primary = &Primary{Metric: "request.prompt_tokens_peak", Direction: PrimaryDecrease, MDE: 0.15}
	req, avail, err := checkPower(exp, trajs, bands, noise)
	require.NoError(t, err)
	require.Equal(t, 2, req)
	require.Equal(t, 12, avail)

	// An unrecorded metric fails closed — the gate can't run blind.
	exp.Primary = &Primary{Metric: "tokens.cache_read", Direction: PrimaryDecrease, MDE: 0.1}
	_, _, err = checkPower(exp, trajs, bands, noise)
	require.ErrorContains(t, err, "no recorded CV")

	// No primary → no gate.
	exp.Primary = nil
	_, _, err = checkPower(exp, trajs, bands, noise)
	require.NoError(t, err)
}

func TestRunExperiment_PowerGateRefuses(t *testing.T) {
	t.Parallel()
	root := newEvalDir(t)
	flag := "debug"
	r := &Runner{EvalDir: root, Driver: fakeRunner{flag: flag}, WorkParent: t.TempDir()}
	for i := range 4 {
		id := fmt.Sprintf("mid-%d", i)
		writeTrajectory(t, filepath.Join(root, "corpus"), id, nil)
	}
	require.NoError(t, os.WriteFile(filepath.Join(root, "flags.json"),
		[]byte(fmt.Sprintf(`{"flag_defaults":{%q:false}}`, flag)), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(root, "noise.json"),
		[]byte(`{"cv":{"steps":0.11}}`), 0o644))
	bands := &Bands{SchemaVersion: 1, Entries: map[string]BandEntry{}}
	for i := range 4 {
		id := fmt.Sprintf("mid-%d", i)
		bands.Entries[id] = BandEntry{Band: BandMid, ContentHash: mustHash(t, filepath.Join(root, "corpus", id))}
	}
	require.NoError(t, bands.Save(root))

	exp := &Experiment{
		Name: "underpowered", Model: "mock/m", Temperature: ptr(0.0),
		Corpus:            []string{"band:mid"},
		RunsPerTrajectory: map[Band]int{BandMid: 3},
		Primary:           &Primary{Metric: "steps", Direction: PrimaryDecrease, MDE: 0.05},
		Arms: map[string]Arm{
			ArmControl:   {Config: ArmConfig{Options: map[string]any{flag: false}}},
			ArmTreatment: {Config: ArmConfig{Options: map[string]any{flag: true}}},
		},
	}
	_, err := r.RunExperiment(context.Background(), exp)
	require.ErrorContains(t, err, "power gate")
	require.ErrorContains(t, err, "60")

	// Refusal precedes scheduling — no records were burned.
	recs, lerr := r.LoadExperimentRecords("underpowered")
	require.NoError(t, lerr)
	require.Empty(t, recs)
}

// varyRunner is fakeRunner plus per-call step variance — the A/A
// noise refresh needs a real CV, which identical samples can't
// produce.
type varyRunner struct {
	flag string
	n    atomic.Int64
}

func (f *varyRunner) Run(ctx context.Context, workdir string, turns []string, b Budget) RunResult {
	res := fakeRunner{flag: f.flag}.Run(ctx, workdir, turns, b)
	res.Steps = 3 + int(f.n.Add(1)%5)
	return res
}

func TestRunExperiment_AAArm(t *testing.T) {
	t.Parallel()
	root := newEvalDir(t)
	flag := "debug"
	r := &Runner{
		EvalDir:        root,
		Driver:         &varyRunner{flag: flag},
		WorkParent:     t.TempDir(),
		PermReplicates: 500,
		RNG:            rand.New(rand.NewPCG(7, 8)),
		AA:             true,
	}
	experimentFixture(t, r, root, flag)

	exp := &Experiment{
		Name: "exp-aa", Model: "mock/m", Temperature: ptr(0.0),
		Corpus:            []string{"*"},
		RunsPerTrajectory: map[Band]int{BandStable: 3, BandMid: 3, BandUncharacterized: 3},
		Arms: map[string]Arm{
			ArmControl:   {Config: ArmConfig{Options: map[string]any{flag: false}}},
			ArmTreatment: {Config: ArmConfig{Options: map[string]any{flag: true}}},
		},
	}
	rep, err := r.RunExperiment(context.Background(), exp)
	require.NoError(t, err)

	// The aa arm sampled like the others but stayed out of every
	// gate tier — the catastrophic alarm is control-vs-treatment.
	recs, err := r.LoadExperimentRecords("exp-aa")
	require.NoError(t, err)
	aaN := 0
	for _, rec := range recs {
		if rec.Arm == ArmAA {
			aaN++
		}
	}
	require.Equal(t, 6, aaN) // 2 trajs × 3.
	require.NotEmpty(t, rep.Catastrophic)
	require.Equal(t, 6, rep.ArmTokens[ArmAA].Runs)

	// The calibration line and the refreshed CVs are in the report.
	s := rep.Summary(0.05)
	require.Contains(t, s, "a/a calibration")
	require.NotEmpty(t, rep.NoiseUpdated)
	noise, err := LoadNoise(root)
	require.NoError(t, err)
	require.Contains(t, noise.CV, "steps")

	// Every Evaluate-produced field reaches the caller — the
	// wholesale-adoption fix for the dropped-field bug class.
	require.NotEmpty(t, rep.Guardrails)
	require.Equal(t, 6, rep.Guardrails[ArmControl].Runs)
	require.Equal(t, 6, rep.Guardrails[ArmControl].Passes)
	require.NotEmpty(t, rep.ArmTokens)

	// The aa arm is removed before return — a reused *Experiment
	// doesn't leak it into the next call.
	require.NotContains(t, exp.Arms, ArmAA)

	// AA starvation never alarms — it shares control's coverage.
	require.NotContains(t, s, "/aa")
}

func TestBuildPrimaryResult_Strata(t *testing.T) {
	t.Parallel()
	exp := &Experiment{
		Primary: &Primary{Metric: "request.prompt_tokens_peak", Direction: PrimaryDecrease, MDE: 0.15},
		Arms: map[string]Arm{
			ArmControl: {},
			ArmTreatment: {Coverage: Coverage{
				"min_checkpoints.rendered": 1,
			}},
		},
	}
	recs := []RunRecord{
		{Arm: ArmControl, Outcome: OutcomePass, Request: &RequestStats{PromptTokensPeak: 100}},
		{Arm: ArmControl, Outcome: OutcomePass, Request: &RequestStats{PromptTokensPeak: 120}},
		{Arm: ArmControl, Outcome: OutcomeInconclusive, Request: &RequestStats{PromptTokensPeak: 999}}, // Excluded.
		{Arm: ArmTreatment, Outcome: OutcomePass, Request: &RequestStats{PromptTokensPeak: 80}, Checkpoints: Checkpoints{Rendered: 3}},
		{Arm: ArmTreatment, Outcome: OutcomeInconclusive, Request: &RequestStats{PromptTokensPeak: 90}, Checkpoints: Checkpoints{Rendered: 2}},
		{Arm: ArmTreatment, Outcome: OutcomePass, Request: &RequestStats{PromptTokensPeak: 95}}, // Mechanism never fired.
		{Arm: ArmTreatment, Outcome: OutcomeError},                                              // No request snapshot → unmeasurable, skipped.
		{Arm: ArmAA, Outcome: OutcomePass, Request: &RequestStats{PromptTokensPeak: 110}},
	}
	res := buildPrimaryResult(exp, recs, 20)
	require.Equal(t, 20, res.RequiredPerArm)
	require.Equal(t, 2, res.Control.N)
	require.InDelta(t, 110, res.Control.Mean(), 1e-9)
	// Conclusive treatment: the pass at 80 and the unfired pass at 95.
	require.Equal(t, 2, res.Treatment.N)
	require.InDelta(t, 87.5, res.Treatment.Mean(), 1e-9)
	// Fired stratum counts the inconclusive-but-fired run too.
	require.Equal(t, 2, res.TreatmentFired.N)
	require.InDelta(t, 85, res.TreatmentFired.Mean(), 1e-9)
	require.Equal(t, 1, res.TreatmentUnfired.N)
	require.InDelta(t, 95, res.TreatmentUnfired.Mean(), 1e-9)
	require.Equal(t, 1, res.AA.N)
	require.InDelta(t, 110, res.AA.Mean(), 1e-9)

	// A coverage-free treatment arm has no firing assertion — the
	// strata must not vacuously report every run as "fired".
	exp.Arms[ArmTreatment] = Arm{}
	res = buildPrimaryResult(exp, recs, 0)
	require.Zero(t, res.TreatmentFired.N)
	require.Zero(t, res.TreatmentUnfired.N)
}

func TestUpdateNoiseFromAA(t *testing.T) {
	t.Parallel()
	exp := &Experiment{Primary: &Primary{Metric: "steps", Direction: PrimaryDecrease, MDE: 0.1}}
	metrics := noiseUpdateMetrics(exp)
	require.Contains(t, metrics, "steps")
	require.Contains(t, metrics, "request.prompt_tokens_peak")
	require.NotContains(t, metrics, "weighted_cost") // No cost_weights.

	// Identical values → CV 0 → nothing updates.
	n := &NoiseFile{CV: map[string]float64{}}
	var recs []RunRecord
	for i := range 6 {
		arm := ArmControl
		if i%2 == 0 {
			arm = ArmAA
		}
		recs = append(recs, RunRecord{Arm: arm, Outcome: OutcomePass, Steps: 10})
	}
	require.Empty(t, n.updateNoiseFromAA(recs, metrics))

	// Variance in the control+aa pool produces a CV; an existing
	// entry blends 80/20 with the new measurement.
	recs[1].Steps, recs[3].Steps, recs[5].Steps = 12, 8, 11
	recs[0].Steps, recs[2].Steps, recs[4].Steps = 9, 11, 10
	n.CV["steps"] = 0.10
	updated := n.updateNoiseFromAA(recs, metrics)
	require.NotEmpty(t, updated)
	require.Contains(t, updated[0], "steps=")
	require.Greater(t, n.CV["steps"], 0.0)
	// Excluded runs don't enter the noise pool.
	recs = append(recs, RunRecord{Arm: ArmAA, Outcome: OutcomeError, Steps: 9999})
	cv0 := n.CV["steps"]
	n.updateNoiseFromAA(recs, metrics)
	require.InDelta(t, cv0, n.CV["steps"], cv0*0.25) // 80/20 blend bounds drift.
}

// The behavioral and guardrail aggregators feed report blocks that
// Summary prints — pin their filtering the same way the token table
// is pinned.
func TestArmMetricAggregators(t *testing.T) {
	t.Parallel()
	recs := []RunRecord{
		{Arm: ArmControl, Outcome: OutcomePass, Steps: 4, CallMetrics: &CallMetrics{RereadsCrossTurn: 2, EditFailures: 1}},
		{Arm: ArmControl, Outcome: OutcomePass, Steps: 6, CallMetrics: &CallMetrics{RereadsCrossTurn: 4}},
		{Arm: ArmControl, Outcome: OutcomeInconclusive, Steps: 99, CallMetrics: &CallMetrics{RereadsCrossTurn: 50}},
		{Arm: ArmControl, Outcome: OutcomePass, Steps: 5}, // No analysis: guardrails counts it, behavior can't.
		{Arm: ArmTreatment, Outcome: OutcomeFail, Steps: 9, CallMetrics: &CallMetrics{}},
	}
	beh := armBehavior(recs)
	require.Equal(t, 2, beh[ArmControl].Runs) // Analysis-bearing conclusive only.
	require.InDelta(t, 6, beh[ArmControl].RereadsCrossTurn, 1e-9)
	require.Equal(t, 1, beh[ArmTreatment].Runs)

	g := armGuardrails(recs)
	require.Equal(t, 3, g[ArmControl].Runs)
	require.Equal(t, 3, g[ArmControl].Passes)
	require.Equal(t, 15, g[ArmControl].StepsSum)
	require.Equal(t, 1, g[ArmControl].EditFailures)
	require.Zero(t, g[ArmTreatment].Passes)
}

// A flag-gated primary on an arm where the gate resolves off
// compares mechanism-presence to structural zero — validation
// rejects it like a starved min_ predicate.
func TestValidateArmCoverageResolved_FlagGatedPrimary(t *testing.T) {
	t.Parallel()
	manifest := &FlagsManifest{Defaults: map[string]any{}}
	exp := &Experiment{
		Primary: &Primary{Metric: "checkpoints.rendered", Direction: PrimaryIncrease, MDE: 0.5},
		Arms: map[string]Arm{
			ArmControl:   {Config: ArmConfig{Options: map[string]any{"notebook_checkpoint": false}}},
			ArmTreatment: {Config: ArmConfig{Options: map[string]any{"notebook_checkpoint": true}}},
		},
	}
	require.ErrorContains(t, ValidateArmCoverageResolved(exp, manifest), "structural zeros")

	// The mechanism enabled on both arms reopens the metric.
	exp.Arms[ArmControl] = exp.Arms[ArmTreatment]
	require.NoError(t, ValidateArmCoverageResolved(exp, manifest))
}
