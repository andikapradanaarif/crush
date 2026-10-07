package eval

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	_ "modernc.org/sqlite"
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

// Seeds run under WriteSeedConfig — the shared model pin plus harness
// invariants and NO arm options — so both arms start the measured
// session from the same state. WriteArmConfig then merges the arm
// delta over the seed file: the second pass must tolerate its own
// earlier .crushrc write.
func TestWriteSeedConfig_ThenArmConfig(t *testing.T) {
	t.Parallel()
	wd := t.TempDir()
	exp := &Experiment{Model: "hyper/x", Temperature: ptr(0.0)}
	manifest := &FlagsManifest{Defaults: map[string]any{}}
	arm := Arm{Config: ArmConfig{Options: map[string]any{"flag_a": true}}}

	require.NoError(t, WriteSeedConfig(wd, exp, manifest))
	raw, err := os.ReadFile(filepath.Join(wd, ".crush.json"))
	require.NoError(t, err)
	require.Contains(t, string(raw), `"disable_metrics": true`)
	require.Contains(t, string(raw), `"data_directory"`)
	require.NotContains(t, string(raw), "flag_a")

	require.NoError(t, WriteArmConfig(wd, exp, arm, manifest))
	raw, err = os.ReadFile(filepath.Join(wd, ".crush.json"))
	require.NoError(t, err)
	require.Contains(t, string(raw), `"flag_a": true`)
	require.Contains(t, string(raw), `"disable_metrics": true`)

	// Experiment-declared providers ride the generated config: the
	// arm pass must tolerate the seed pass's own write. A fixture
	// .crush.json with DIVERGENT providers still rejects.
	exp.Providers = map[string]any{"tp": map[string]any{"type": "openai-compat", "api_key": "$KEY"}}
	wd3 := t.TempDir()
	require.NoError(t, WriteSeedConfig(wd3, exp, manifest))
	require.NoError(t, WriteArmConfig(wd3, exp, arm, manifest))
	raw, err = os.ReadFile(filepath.Join(wd3, ".crush.json"))
	require.NoError(t, err)
	require.Contains(t, string(raw), `"openai-compat"`)

	wd4 := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(wd4, ".crush.json"),
		[]byte(`{"providers":{"other":{"type":"anthropic"}}}`), 0o644))
	require.Error(t, WriteSeedConfig(wd4, exp, manifest))
	require.Error(t, WriteArmConfig(wd4, exp, arm, manifest))

	// A fixture shipping the experiment's own providers block
	// byte-identically resolves the same way — tolerated. Pinned so
	// the carve-out is a tested decision, not an accident.
	wd5 := t.TempDir()
	provDoc, err := json.Marshal(map[string]any{"providers": exp.Providers})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(wd5, ".crush.json"), provDoc, 0o644))
	require.NoError(t, WriteSeedConfig(wd5, exp, manifest))
	require.NoError(t, WriteArmConfig(wd5, exp, arm, manifest))

	// A fixture .crushrc still collides — only the harness's own
	// identical pin is tolerated.
	wd2 := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(wd2, ".crushrc"), []byte("model large other/y\n"), 0o644))
	require.Error(t, WriteSeedConfig(wd2, exp, manifest))
	require.Error(t, WriteArmConfig(wd2, exp, arm, manifest))
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
		{Arm: ArmControl, Outcome: OutcomePass, Steps: 4, CallMetrics: &CallMetrics{RereadsCrossTurn: 2, Calls: 7, EditFailures: 1}},
		{Arm: ArmControl, Outcome: OutcomePass, Steps: 6, CallMetrics: &CallMetrics{RereadsCrossTurn: 4, Calls: 9}},
		{Arm: ArmControl, Outcome: OutcomeInconclusive, Steps: 99, CallMetrics: &CallMetrics{RereadsCrossTurn: 50, Calls: 50}},
		{Arm: ArmControl, Outcome: OutcomePass, Steps: 5}, // No analysis: guardrails counts it, behavior can't.
		{Arm: ArmTreatment, Outcome: OutcomeFail, Steps: 9, CallMetrics: &CallMetrics{}},
	}
	beh := armBehavior(recs)
	require.Equal(t, 2, beh[ArmControl].Runs) // Analysis-bearing conclusive only.
	require.InDelta(t, 6, beh[ArmControl].RereadsCrossTurn, 1e-9)
	require.InDelta(t, 16, beh[ArmControl].Calls, 1e-9)
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

// seedRecorder drives ExecuteRun's prior_sessions loop — records each
// call's workdir and turns in order and stamps a session id, so a
// test can assert seeds ran before the measured task on one workdir.
// failAt fails the Nth call with Err, timeoutAt times it out (1-based).
// Every call drops a crush.db so the run's preservation step has an
// artifact to snapshot.
type seedRecorder struct {
	calls     [][]string
	workdirs  []string
	failAt    int
	timeoutAt int
	gen       bool // every call reports sidecar generation spend
}

func (s *seedRecorder) Run(_ context.Context, workdir string, turns []string, _ Budget) RunResult {
	s.calls = append(s.calls, turns)
	s.workdirs = append(s.workdirs, workdir)
	dataDir := DataDirFor(workdir)
	_ = os.MkdirAll(dataDir, 0o755)
	_ = os.WriteFile(filepath.Join(dataDir, "crush.db"), []byte("seeddb"), 0o644)
	res := RunResult{
		Steps:         2,
		SessionID:     fmt.Sprintf("sess-%d", len(s.calls)),
		ModelResolved: "mock/m",
		Tokens:        TokenUsage{Input: 10, Output: 5},
	}
	if s.gen {
		res.GeneratorTokens = GeneratorTokens{Calls: 1, Input: 100, Output: 20}
	}
	switch len(s.calls) {
	case s.failAt:
		res.Err = fmt.Errorf("seed session blew up")
		res.ErrorClass = "provider_deterministic"
	case s.timeoutAt:
		res.TimedOut = true
	default:
		_ = os.WriteFile(filepath.Join(workdir, "fixed.marker"), []byte("x"), 0o644)
	}
	return res
}

// Warm-start ordering: prior_sessions run first as separate sessions
// on one workdir, then the measured task — the record's Steps/Tokens
// stay the measured session's while WarmStart carries the seeding
// ledger.
func TestExecuteRun_PriorSessionsSeedThenMeasure(t *testing.T) {
	t.Parallel()
	root := newEvalDir(t)
	trajDir := writeTrajectory(t, filepath.Join(root, "corpus"), "warm-t", map[string]any{
		"prior_sessions": []any{
			map[string]any{"turns": []string{"explore — change nothing"}},
			map[string]any{"turns": []string{"explain the bug"}},
		},
		"task": map[string]any{"turns": []string{"fix it"}},
	})
	tr, err := LoadTrajectory(trajDir)
	require.NoError(t, err)
	drv := &seedRecorder{gen: true}
	r := &Runner{
		EvalDir:    root,
		Driver:     drv,
		WorkParent: t.TempDir(),
		RNG:        rand.New(rand.NewPCG(1, 2)),
	}
	exp := &Experiment{Name: "exp1", Model: "mock/m", Temperature: ptr(0.0)}
	manifest := &FlagsManifest{Defaults: map[string]any{}}

	rec, err := r.ExecuteRun(context.Background(), exp, tr, trajDir, ArmControl, Arm{}, manifest, 1, "inv")
	require.NoError(t, err)
	require.Equal(t, OutcomePass, rec.Outcome)

	require.Len(t, drv.calls, 3)
	require.Equal(t, []string{"explore — change nothing"}, drv.calls[0])
	require.Equal(t, []string{"explain the bug"}, drv.calls[1])
	require.Equal(t, []string{"fix it"}, drv.calls[2])
	require.Equal(t, drv.workdirs[0], drv.workdirs[2],
		"seeds and the measured session share one workdir + crush.db")

	require.NotNil(t, rec.WarmStart)
	require.Equal(t, 2, rec.WarmStart.Sessions)
	require.Equal(t, []string{"sess-1", "sess-2"}, rec.WarmStart.SessionIDs)
	require.Equal(t, 4, rec.WarmStart.Steps)
	require.Equal(t, int64(20), rec.WarmStart.Tokens.Input)
	require.Equal(t, 2, rec.WarmStart.GeneratorTokens.Calls)
	require.Equal(t, int64(200), rec.WarmStart.GeneratorTokens.Input,
		"the seeds' sidecar generation spend is priced in the ledger")
	require.Equal(t, 2, rec.Steps, "recorded steps are the measured session's, not the seeds'")
	require.NotNil(t, rec.GeneratorTokens)
	require.Equal(t, 1, rec.GeneratorTokens.Calls,
		"the record's sidecar count is the measured session's alone")
	require.Equal(t, "sess-3", rec.SessionID,
		"the record names the measured session — seeds live in warm_start.session_ids")
}

// A seed that errors leaves a warm state other than the designed
// one — error out instead of measuring a degraded seeding, and never
// launch the measured session. The failure path still preserves the
// db (the artifact the session ids point at), reports the seed's
// ErrorClass for the circuit breaker, and counts real wall clock.
func TestExecuteRun_PriorSessionFailureIsError(t *testing.T) {
	t.Parallel()
	root := newEvalDir(t)
	trajDir := writeTrajectory(t, filepath.Join(root, "corpus"), "warm-t", map[string]any{
		"prior_sessions": []any{
			map[string]any{"turns": []string{"first seed"}},
			map[string]any{"turns": []string{"second seed"}},
		},
		"task": map[string]any{"turns": []string{"fix it"}},
	})
	tr, err := LoadTrajectory(trajDir)
	require.NoError(t, err)
	drv := &seedRecorder{failAt: 1}
	// A stepping clock keeps the wall-clock pin deterministic — a
	// near-instant stub seed can land inside one Windows timer tick
	// and read as exactly 0.
	var clock atomic.Int64
	r := &Runner{
		EvalDir:    root,
		Driver:     drv,
		WorkParent: t.TempDir(),
		RNG:        rand.New(rand.NewPCG(1, 2)),
		Now:        func() time.Time { return time.Unix(0, clock.Add(int64(time.Millisecond))) },
	}
	exp := &Experiment{Name: "exp1", Model: "mock/m", Temperature: ptr(0.0)}
	manifest := &FlagsManifest{Defaults: map[string]any{}}

	rec, err := r.ExecuteRun(context.Background(), exp, tr, trajDir, ArmControl, Arm{}, manifest, 1, "inv")
	require.NoError(t, err)
	require.Equal(t, OutcomeError, rec.Outcome)
	require.Contains(t, rec.CheckDetail["prior_session"], "seed 1 of 2")
	require.Len(t, drv.calls, 1, "no further session launches after a failed seed")
	require.Equal(t, "provider_deterministic", rec.ErrorClass,
		"the breaker reads the seed's typed class, not a bare error")
	require.Positive(t, rec.DurationS, "seeding wall clock was real")
	require.NotEmpty(t, rec.SessionDB,
		"the failed seed's db is preserved — the artifact session_ids promise")
	require.True(t, rec.SessionDBIncomplete,
		"the stub db isn't SQLite — the raw-copy fallback marks it incomplete")
	require.NotNil(t, rec.WarmStart)
	require.Equal(t, 1, rec.WarmStart.Sessions, "sessions counts attempted, not declared")
	require.Equal(t, []string{"sess-1"}, rec.WarmStart.SessionIDs,
		"the failed seed's session id is recorded — the db artifact holds it for forensics")

	// The cold path is untouched: no prior_sessions → no ledger.
	tr.PriorSessions = nil
	rec, err = r.ExecuteRun(context.Background(), exp, tr, trajDir, ArmControl, Arm{}, manifest, 2, "inv")
	require.NoError(t, err)
	require.Nil(t, rec.WarmStart)
}

// A timed-out seed is a failed seeding, not a measured-run timeout —
// the record classifies it error with the same preservation as an
// errored seed.
func TestExecuteRun_PriorSessionTimeoutIsError(t *testing.T) {
	t.Parallel()
	root := newEvalDir(t)
	trajDir := writeTrajectory(t, filepath.Join(root, "corpus"), "warm-t", map[string]any{
		"prior_sessions": []any{
			map[string]any{"turns": []string{"only seed"}},
		},
		"task": map[string]any{"turns": []string{"fix it"}},
	})
	tr, err := LoadTrajectory(trajDir)
	require.NoError(t, err)
	drv := &seedRecorder{timeoutAt: 1}
	r := &Runner{
		EvalDir:    root,
		Driver:     drv,
		WorkParent: t.TempDir(),
		RNG:        rand.New(rand.NewPCG(1, 2)),
	}
	exp := &Experiment{Name: "exp1", Model: "mock/m", Temperature: ptr(0.0)}
	manifest := &FlagsManifest{Defaults: map[string]any{}}

	rec, err := r.ExecuteRun(context.Background(), exp, tr, trajDir, ArmControl, Arm{}, manifest, 1, "inv")
	require.NoError(t, err)
	require.Equal(t, OutcomeError, rec.Outcome)
	require.Contains(t, rec.CheckDetail["prior_session"], "timeout=true")
	require.Len(t, drv.calls, 1)
	require.NotEmpty(t, rec.SessionDB)
}

// seedGateRecorder is seedRecorder plus a per-call marker file —
// call-N.marker — so a seed script can assert it ran after the seeds
// and before the measured session.
type seedGateRecorder struct{ seedRecorder }

func (s *seedGateRecorder) Run(ctx context.Context, workdir string, turns []string, b Budget) RunResult {
	res := s.seedRecorder.Run(ctx, workdir, turns, b)
	_ = os.WriteFile(filepath.Join(workdir, fmt.Sprintf("call-%d.marker", len(s.calls))), []byte("x"), 0o644)
	return res
}

// configSnapshotter records the workdir's .crush.json at each Run —
// the proof that prior_sessions execute under the neutral seed
// config and the measured session under the arm's.
type configSnapshotter struct {
	seedRecorder
	configs []string
}

func (c *configSnapshotter) Run(ctx context.Context, workdir string, turns []string, b Budget) RunResult {
	raw, _ := os.ReadFile(filepath.Join(workdir, ".crush.json"))
	c.configs = append(c.configs, string(raw))
	return c.seedRecorder.Run(ctx, workdir, turns, b)
}

// Arm options must not reach the seeds: an option active during
// seeding (the reconcile edge, the tail) makes the arms' starting
// states differ before measurement. Only the measured call sees them.
func TestExecuteRun_SeedsRunNeutralConfig(t *testing.T) {
	t.Parallel()
	root := newEvalDir(t)
	trajDir := writeTrajectory(t, filepath.Join(root, "corpus"), "warm-t", map[string]any{
		"prior_sessions": []any{
			map[string]any{"turns": []string{"seed one"}},
			map[string]any{"turns": []string{"seed two"}},
		},
		"task": map[string]any{"turns": []string{"fix it"}},
	})
	tr, err := LoadTrajectory(trajDir)
	require.NoError(t, err)
	drv := &configSnapshotter{}
	r := &Runner{
		EvalDir:    root,
		Driver:     drv,
		WorkParent: t.TempDir(),
		RNG:        rand.New(rand.NewPCG(1, 2)),
	}
	exp := &Experiment{Name: "exp1", Model: "mock/m", Temperature: ptr(0.0)}
	manifest := &FlagsManifest{Defaults: map[string]any{}}
	arm := Arm{Config: ArmConfig{Options: map[string]any{"failure_memory": true}}}

	rec, err := r.ExecuteRun(context.Background(), exp, tr, trajDir, ArmTreatment, arm, manifest, 1, "inv")
	require.NoError(t, err)
	require.Equal(t, OutcomePass, rec.Outcome)
	require.Len(t, drv.configs, 3, "two seeds plus the measured run")
	for i, cfg := range drv.configs[:2] {
		require.NotContains(t, cfg, "failure_memory",
			"seed %d ran under the arm config — arms' seed states diverge", i+1)
		require.Contains(t, cfg, "disable_metrics")
	}
	require.Contains(t, drv.configs[2], `"failure_memory": true`,
		"the measured session must carry the arm delta")
}

// requireSeedCheckTooling skips when the shell toolchain seed-check
// scripts exec isn't on PATH — corpus scripts are POSIX-authored, so
// this is a tooling gate, not a verdict. bash is always required;
// db-aware scripts pass their extra tools (e.g. sqlite3) explicitly.
func requireSeedCheckTooling(t *testing.T, extra ...string) {
	t.Helper()
	for _, tool := range append([]string{"bash"}, extra...) {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("seed-check script needs %s on PATH", tool)
		}
	}
}

// The seed-state gate runs after every prior session and before the
// measured one: a valid designed state lets the run proceed and the
// script's EVAL_JSON lands on the record as SeedState.
func TestExecuteRun_SeedCheckPass(t *testing.T) {
	t.Parallel()
	requireSeedCheckTooling(t)
	root := newEvalDir(t)
	trajDir := writeTrajectory(t, filepath.Join(root, "corpus"), "warm-t", map[string]any{
		"prior_sessions": []any{map[string]any{"turns": []string{"seed it"}}},
		"task":           map[string]any{"turns": []string{"fix it"}},
		"check": map[string]any{
			"script":             "check.sh",
			"expect_start_state": "fail",
			"seed_script":        "seed_check.sh",
		},
	})
	require.NoError(t, os.WriteFile(filepath.Join(trajDir, "seed_check.sh"), []byte(
		"#!/bin/bash\n"+
			"test -f call-1.marker && ! test -f call-2.marker\n"+
			"echo 'EVAL_JSON {\"seeded\":\"yes\"}'\n"), 0o755))
	tr, err := LoadTrajectory(trajDir)
	require.NoError(t, err)
	drv := &seedGateRecorder{}
	r := &Runner{EvalDir: root, Driver: drv, WorkParent: t.TempDir(), RNG: rand.New(rand.NewPCG(1, 2))}
	exp := &Experiment{Name: "exp1", Model: "mock/m", Temperature: ptr(0.0)}
	manifest := &FlagsManifest{Defaults: map[string]any{}}

	rec, err := r.ExecuteRun(context.Background(), exp, tr, trajDir, ArmControl, Arm{}, manifest, 1, "inv")
	require.NoError(t, err)
	require.Equal(t, OutcomePass, rec.Outcome)
	require.Len(t, drv.calls, 2, "seed then measured — the gate does not consume a session")
	require.Equal(t, map[string]any{"seeded": "yes"}, rec.SeedState,
		"the gate's EVAL_JSON is what the workdir verifiably looked like")
}

// A gate that exits non-zero rejects the warm start before the
// measured session launches — clean execution, wrong state is
// inconclusive, not a model failure.
func TestExecuteRun_SeedCheckRejectsBeforeMeasure(t *testing.T) {
	t.Parallel()
	requireSeedCheckTooling(t)
	root := newEvalDir(t)
	trajDir := writeTrajectory(t, filepath.Join(root, "corpus"), "warm-t", map[string]any{
		"prior_sessions": []any{map[string]any{"turns": []string{"seed it"}}},
		"task":           map[string]any{"turns": []string{"fix it"}},
		"check": map[string]any{
			"script":             "check.sh",
			"expect_start_state": "fail",
			"seed_script":        "seed_check.sh",
		},
	})
	require.NoError(t, os.WriteFile(filepath.Join(trajDir, "seed_check.sh"), []byte(
		"#!/bin/bash\n"+
			"echo 'EVAL_JSON {\"decoy\":\"fail\"}'\n"+
			"echo 'decoy still broken' >&2\n"+
			"exit 1\n"), 0o755))
	tr, err := LoadTrajectory(trajDir)
	require.NoError(t, err)
	drv := &seedGateRecorder{}
	r := &Runner{EvalDir: root, Driver: drv, WorkParent: t.TempDir(), RNG: rand.New(rand.NewPCG(1, 2))}
	exp := &Experiment{Name: "exp1", Model: "mock/m", Temperature: ptr(0.0)}
	manifest := &FlagsManifest{Defaults: map[string]any{}}

	rec, err := r.ExecuteRun(context.Background(), exp, tr, trajDir, ArmControl, Arm{}, manifest, 1, "inv")
	require.NoError(t, err)
	require.Equal(t, OutcomeInconclusive, rec.Outcome)
	require.Len(t, drv.calls, 1, "the measured session never launches on a rejected seeding")
	require.Contains(t, rec.CheckDetail["seed_check"], "assertion failed")
	require.Contains(t, rec.CheckDetail["seed_check_stderr"], "decoy still broken")
	require.Equal(t, map[string]any{"decoy": "fail"}, rec.SeedState,
		"the failing detail is what forensics inspects")
	require.NotEmpty(t, rec.SessionDB,
		"the rejected seeding's db is preserved like a failed seed's")
	require.NotNil(t, rec.WarmStart)
	require.Equal(t, 1, rec.WarmStart.Sessions)
}

// A gate that cannot complete — here, timeout — is a harness error,
// not an invalid seeding.
func TestExecuteRun_SeedCheckHarnessError(t *testing.T) {
	t.Parallel()
	requireSeedCheckTooling(t)
	root := newEvalDir(t)
	trajDir := writeTrajectory(t, filepath.Join(root, "corpus"), "warm-t", map[string]any{
		"prior_sessions": []any{map[string]any{"turns": []string{"seed it"}}},
		"task":           map[string]any{"turns": []string{"fix it"}},
		"check": map[string]any{
			"script":             "check.sh",
			"expect_start_state": "fail",
			"timeout_seconds":    1,
			"seed_script":        "seed_check.sh",
		},
	})
	require.NoError(t, os.WriteFile(filepath.Join(trajDir, "seed_check.sh"),
		[]byte("#!/bin/bash\nsleep 5\n"), 0o755))
	tr, err := LoadTrajectory(trajDir)
	require.NoError(t, err)
	drv := &seedGateRecorder{}
	r := &Runner{EvalDir: root, Driver: drv, WorkParent: t.TempDir(), RNG: rand.New(rand.NewPCG(1, 2))}
	exp := &Experiment{Name: "exp1", Model: "mock/m", Temperature: ptr(0.0)}
	manifest := &FlagsManifest{Defaults: map[string]any{}}

	rec, err := r.ExecuteRun(context.Background(), exp, tr, trajDir, ArmControl, Arm{}, manifest, 1, "inv")
	require.NoError(t, err)
	require.Equal(t, OutcomeError, rec.Outcome)
	require.Contains(t, rec.CheckDetail["seed_check_error"], "timed out")
	require.Contains(t, rec.CheckDetail, "seed_check_stderr",
		"a half-run gate's partial output stays on the error record")
	require.Len(t, drv.calls, 1)
}

// The gate must resolve its script under a relative trajDir — the
// default --eval-dir is "eval", and bash would otherwise resolve the
// relative script path against the workdir and exit 127, masquerading
// as an invalid seeding.
func TestExecuteRun_SeedCheckRelativeTrajDir(t *testing.T) {
	requireSeedCheckTooling(t)
	parent := t.TempDir()
	root := filepath.Join(parent, "ev")
	require.NoError(t, os.MkdirAll(filepath.Join(root, "corpus"), 0o755))
	trajDir := writeTrajectory(t, filepath.Join(root, "corpus"), "warm-t", map[string]any{
		"prior_sessions": []any{map[string]any{"turns": []string{"seed it"}}},
		"task":           map[string]any{"turns": []string{"fix it"}},
		"check": map[string]any{
			"script":             "check.sh",
			"expect_start_state": "fail",
			"seed_script":        "seed_check.sh",
		},
	})
	// EVAL_TRAJECTORY_DIR must resolve for gate scripts too.
	require.NoError(t, os.WriteFile(filepath.Join(trajDir, "seed_check.sh"), []byte(
		"#!/bin/bash\n"+
			"test -f \"$EVAL_TRAJECTORY_DIR/check.sh\"\n"+
			"echo 'EVAL_JSON {\"seeded\":\"yes\"}'\n"), 0o755))
	// Not parallel — the chdir is process-global.
	t.Chdir(parent)
	relTraj := filepath.Join("ev", "corpus", "warm-t")
	tr, err := LoadTrajectory(relTraj)
	require.NoError(t, err)
	drv := &seedGateRecorder{}
	r := &Runner{EvalDir: "ev", Driver: drv, WorkParent: t.TempDir(), RNG: rand.New(rand.NewPCG(1, 2))}
	exp := &Experiment{Name: "exp1", Model: "mock/m", Temperature: ptr(0.0)}
	manifest := &FlagsManifest{Defaults: map[string]any{}}

	rec, err := r.ExecuteRun(context.Background(), exp, tr, relTraj, ArmControl, Arm{}, manifest, 1, "inv")
	require.NoError(t, err)
	require.Equal(t, OutcomePass, rec.Outcome)
	require.Equal(t, map[string]any{"seeded": "yes"}, rec.SeedState)
}

// With multiple seeds the gate asserts only the final state — and its
// db analysis pairs with the last seed's session.
func TestExecuteRun_SeedCheckMultipleSeeds(t *testing.T) {
	t.Parallel()
	requireSeedCheckTooling(t)
	root := newEvalDir(t)
	trajDir := writeTrajectory(t, filepath.Join(root, "corpus"), "warm-t", map[string]any{
		"prior_sessions": []any{
			map[string]any{"turns": []string{"first seed"}},
			map[string]any{"turns": []string{"second seed"}},
		},
		"task": map[string]any{"turns": []string{"fix it"}},
		"check": map[string]any{
			"script":             "check.sh",
			"expect_start_state": "fail",
			"seed_script":        "seed_check.sh",
		},
	})
	require.NoError(t, os.WriteFile(filepath.Join(trajDir, "seed_check.sh"), []byte(
		"#!/bin/bash\n"+
			"test -f call-2.marker && ! test -f call-3.marker\n"+
			"echo 'EVAL_JSON {\"seeded\":\"after-both\"}'\n"), 0o755))
	tr, err := LoadTrajectory(trajDir)
	require.NoError(t, err)
	drv := &seedGateRecorder{}
	r := &Runner{EvalDir: root, Driver: drv, WorkParent: t.TempDir(), RNG: rand.New(rand.NewPCG(1, 2))}
	exp := &Experiment{Name: "exp1", Model: "mock/m", Temperature: ptr(0.0)}
	manifest := &FlagsManifest{Defaults: map[string]any{}}

	rec, err := r.ExecuteRun(context.Background(), exp, tr, trajDir, ArmControl, Arm{}, manifest, 1, "inv")
	require.NoError(t, err)
	require.Equal(t, OutcomePass, rec.Outcome)
	require.Len(t, drv.calls, 3, "two seeds, gate, then measured")
	require.Equal(t, map[string]any{"seeded": "after-both"}, rec.SeedState)
	require.Equal(t, []string{"sess-1", "sess-2"}, rec.WarmStart.SessionIDs)
}

// A rejecting gate that prints no EVAL_JSON leaves SeedState nil —
// absent detail is distinguishable from a reported wrong state.
func TestExecuteRun_SeedCheckRejectNoDetail(t *testing.T) {
	t.Parallel()
	requireSeedCheckTooling(t)
	root := newEvalDir(t)
	trajDir := writeTrajectory(t, filepath.Join(root, "corpus"), "warm-t", map[string]any{
		"prior_sessions": []any{map[string]any{"turns": []string{"seed it"}}},
		"check": map[string]any{
			"script":             "check.sh",
			"expect_start_state": "fail",
			"seed_script":        "seed_check.sh",
		},
	})
	require.NoError(t, os.WriteFile(filepath.Join(trajDir, "seed_check.sh"),
		[]byte("#!/bin/bash\nexit 1\n"), 0o755))
	tr, err := LoadTrajectory(trajDir)
	require.NoError(t, err)
	drv := &seedGateRecorder{}
	r := &Runner{EvalDir: root, Driver: drv, WorkParent: t.TempDir(), RNG: rand.New(rand.NewPCG(1, 2))}
	exp := &Experiment{Name: "exp1", Model: "mock/m", Temperature: ptr(0.0)}
	manifest := &FlagsManifest{Defaults: map[string]any{}}

	rec, err := r.ExecuteRun(context.Background(), exp, tr, trajDir, ArmControl, Arm{}, manifest, 1, "inv")
	require.NoError(t, err)
	require.Equal(t, OutcomeInconclusive, rec.Outcome)
	require.Nil(t, rec.SeedState)
	require.Len(t, drv.calls, 1)
}

// A seed script that vanishes between load and run is infra, not an
// invalid seeding. Through ExecuteRun the content hash catches it
// first (seed_script is a hashed ref); the stat belt inside
// runCheckScript covers the race window directly — a missing script
// is Err, never a judged exit.
func TestRunCheckScript_MissingFileIsError(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	res := runCheckScript(context.Background(), "gone.sh", dir, dir, nil, time.Second)
	require.Error(t, res.Err)
	require.Contains(t, res.Err.Error(), "does not exist")
	require.Equal(t, -1, res.Exit)
}

// dbSeedRecorder seeds a REAL crush.db with a failure_memory row —
// resolved or open per the flag — so a db-aware gate can assert the
// memory state a disobedient seed would corrupt. The worktree still
// gets fixed.marker, so only the row state varies.
type dbSeedRecorder struct {
	seedRecorder
	resolved bool
	// strayRow also inserts a DIFFERENT open -count=1 row — the
	// disobedient-seed signature of resolving the designed row while
	// leaving a substitute (e.g. re-running the still-red task).
	strayRow bool
}

func (s *dbSeedRecorder) Run(ctx context.Context, workdir string, turns []string, b Budget) RunResult {
	res := s.seedRecorder.Run(ctx, workdir, turns, b)
	if len(s.calls) != 1 {
		return res
	}
	dbPath := filepath.Join(DataDirFor(workdir), "crush.db")
	_ = os.Remove(dbPath)
	db, err := sql.Open("sqlite", "file:"+dbPath)
	if err != nil {
		return res
	}
	defer db.Close()
	_, _ = db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS failure_memory (
		signature TEXT NOT NULL PRIMARY KEY, cmd TEXT NOT NULL,
		cwd TEXT NOT NULL DEFAULT '', headline TEXT NOT NULL,
		files TEXT NOT NULL DEFAULT '[]', first_seen INTEGER NOT NULL,
		last_seen INTEGER NOT NULL, resolved_in TEXT NOT NULL DEFAULT '')`)
	resolvedIn := ""
	if s.resolved {
		resolvedIn = "sess-1"
	}
	_, _ = db.ExecContext(ctx, `INSERT INTO failure_memory
		(signature, cmd, cwd, headline, first_seen, last_seen, resolved_in)
		VALUES ('sig1', 'go test -count=1 ./decoy', '.', 'FAIL', 1000, 1000, ?)`, resolvedIn)
	if s.strayRow {
		_, _ = db.ExecContext(ctx, `INSERT INTO failure_memory
			(signature, cmd, cwd, headline, first_seen, last_seen, resolved_in)
			VALUES ('sig2', 'go test -count=1 .', '.', 'FAIL', 2000, 2000, '')`)
	}
	return res
}

// A seed that re-ran the verbatim recorded command resolves the row —
// the worktree can look exactly right while the premise (a stale open
// failure) is gone. A db-aware gate must catch it.
func TestExecuteRun_SeedCheckResolvedRowRejects(t *testing.T) {
	t.Parallel()
	requireSeedCheckTooling(t, "sqlite3")
	root := newEvalDir(t)
	trajDir := writeTrajectory(t, filepath.Join(root, "corpus"), "warm-t", map[string]any{
		"prior_sessions": []any{map[string]any{"turns": []string{"seed it"}}},
		"check": map[string]any{
			"script":             "check.sh",
			"expect_start_state": "fail",
			"seed_script":        "seed_check.sh",
		},
	})
	require.NoError(t, os.WriteFile(filepath.Join(trajDir, "seed_check.sh"), []byte(
		"#!/bin/bash\n"+
			`db="$(dirname "$EVAL_WORKDIR")/$(basename "$EVAL_WORKDIR").crush-data/crush.db"`+"\n"+
			`open=$(sqlite3 "$db" "SELECT COUNT(*) FROM failure_memory WHERE resolved_in='' AND cmd='go test -count=1 ./decoy' AND cwd IN ('.','');")`+"\n"+
			`echo "EVAL_JSON {\"open_stale_rows\":$open}"`+"\n"+
			`[ "$open" -ge 1 ]`+"\n"), 0o755))
	tr, err := LoadTrajectory(trajDir)
	require.NoError(t, err)
	drv := &dbSeedRecorder{resolved: true}
	r := &Runner{EvalDir: root, Driver: drv, WorkParent: t.TempDir(), RNG: rand.New(rand.NewPCG(1, 2))}
	exp := &Experiment{Name: "exp1", Model: "mock/m", Temperature: ptr(0.0)}
	manifest := &FlagsManifest{Defaults: map[string]any{}}

	rec, err := r.ExecuteRun(context.Background(), exp, tr, trajDir, ArmControl, Arm{}, manifest, 1, "inv")
	require.NoError(t, err)
	require.Equal(t, OutcomeInconclusive, rec.Outcome,
		"a resolved row means 'no open failure' — not the cell's stale premise")
	require.Len(t, drv.calls, 1, "measured session never launches")
	require.Equal(t, map[string]any{"open_stale_rows": float64(0)}, rec.SeedState)
}

// The same gate passes when the open row survives — the db assertion
// is a premise check, not a universal veto.
func TestExecuteRun_SeedCheckOpenRowPasses(t *testing.T) {
	t.Parallel()
	requireSeedCheckTooling(t, "sqlite3")
	root := newEvalDir(t)
	trajDir := writeTrajectory(t, filepath.Join(root, "corpus"), "warm-t", map[string]any{
		"prior_sessions": []any{map[string]any{"turns": []string{"seed it"}}},
		"check": map[string]any{
			"script":             "check.sh",
			"expect_start_state": "fail",
			"seed_script":        "seed_check.sh",
		},
	})
	require.NoError(t, os.WriteFile(filepath.Join(trajDir, "seed_check.sh"), []byte(
		"#!/bin/bash\n"+
			`db="$(dirname "$EVAL_WORKDIR")/$(basename "$EVAL_WORKDIR").crush-data/crush.db"`+"\n"+
			`open=$(sqlite3 "$db" "SELECT COUNT(*) FROM failure_memory WHERE resolved_in='' AND cmd='go test -count=1 ./decoy' AND cwd IN ('.','');")`+"\n"+
			`echo "EVAL_JSON {\"open_stale_rows\":$open}"`+"\n"+
			`[ "$open" -ge 1 ]`+"\n"), 0o755))
	tr, err := LoadTrajectory(trajDir)
	require.NoError(t, err)
	drv := &dbSeedRecorder{resolved: false}
	r := &Runner{EvalDir: root, Driver: drv, WorkParent: t.TempDir(), RNG: rand.New(rand.NewPCG(1, 2))}
	exp := &Experiment{Name: "exp1", Model: "mock/m", Temperature: ptr(0.0)}
	manifest := &FlagsManifest{Defaults: map[string]any{}}

	rec, err := r.ExecuteRun(context.Background(), exp, tr, trajDir, ArmControl, Arm{}, manifest, 1, "inv")
	require.NoError(t, err)
	require.Equal(t, OutcomePass, rec.Outcome)
	require.Equal(t, map[string]any{"open_stale_rows": float64(1)}, rec.SeedState)
}

// The reviewer's false-pass: a seed that resolved the designed row but
// left a DIFFERENT open -count=1 row (re-running the still-red task).
// A LIKE-scoped gate would count the stray row and pass; the
// designed-row predicate must still reject.
func TestExecuteRun_SeedCheckStrayRowStillRejects(t *testing.T) {
	t.Parallel()
	requireSeedCheckTooling(t, "sqlite3")
	root := newEvalDir(t)
	trajDir := writeTrajectory(t, filepath.Join(root, "corpus"), "warm-t", map[string]any{
		"prior_sessions": []any{map[string]any{"turns": []string{"seed it"}}},
		"check": map[string]any{
			"script":             "check.sh",
			"expect_start_state": "fail",
			"seed_script":        "seed_check.sh",
		},
	})
	require.NoError(t, os.WriteFile(filepath.Join(trajDir, "seed_check.sh"), []byte(
		"#!/bin/bash\n"+
			`db="$(dirname "$EVAL_WORKDIR")/$(basename "$EVAL_WORKDIR").crush-data/crush.db"`+"\n"+
			`open=$(sqlite3 "$db" "SELECT COUNT(*) FROM failure_memory WHERE resolved_in='' AND cmd='go test -count=1 ./decoy' AND cwd IN ('.','');")`+"\n"+
			`echo "EVAL_JSON {\"open_stale_rows\":$open}"`+"\n"+
			`[ "$open" -ge 1 ]`+"\n"), 0o755))
	tr, err := LoadTrajectory(trajDir)
	require.NoError(t, err)
	drv := &dbSeedRecorder{resolved: true, strayRow: true}
	r := &Runner{EvalDir: root, Driver: drv, WorkParent: t.TempDir(), RNG: rand.New(rand.NewPCG(1, 2))}
	exp := &Experiment{Name: "exp1", Model: "mock/m", Temperature: ptr(0.0)}
	manifest := &FlagsManifest{Defaults: map[string]any{}}

	rec, err := r.ExecuteRun(context.Background(), exp, tr, trajDir, ArmControl, Arm{}, manifest, 1, "inv")
	require.NoError(t, err)
	require.Equal(t, OutcomeInconclusive, rec.Outcome,
		"a stray -count=1 row is live memory, not the designed stale row")
	require.Len(t, drv.calls, 1, "measured session never launches")
	require.Equal(t, map[string]any{"open_stale_rows": float64(0)}, rec.SeedState)
}
