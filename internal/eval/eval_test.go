package eval

import (
	"context"
	"encoding/json"
	"math/rand/v2"
	"os"
	"path/filepath"
	"testing"

	"github.com/charmbracelet/crush/internal/agent"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/stretchr/testify/require"
)

// writeTrajectory scaffolds corpus/<id>/ with a spec, check.sh, and
// fixture. Returns the trajectory dir.
func writeTrajectory(t *testing.T, corpusRoot, id string, spec map[string]any) string {
	t.Helper()
	if spec == nil {
		spec = map[string]any{}
	}
	dir := filepath.Join(corpusRoot, id)
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "fixture"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "fixture", "hello.txt"), []byte("hi\n"), 0o644))
	if _, ok := spec["check"]; !ok {
		spec["check"] = map[string]any{"script": "check.sh", "expect_start_state": "fail"}
	}
	if _, ok := spec["check_script_body"]; !ok {
		spec["check_script_body"] = "#!/bin/bash\ntest -f fixed.marker\n"
	}
	body := spec["check_script_body"].(string)
	delete(spec, "check_script_body")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "check.sh"), []byte(body), 0o755))

	if _, ok := spec["origin"]; !ok {
		spec["origin"] = map[string]any{"kind": "synthetic"}
	}
	if _, ok := spec["start_state"]; !ok {
		spec["start_state"] = map[string]any{"kind": "fixture", "fixture_dir": "fixture"}
	}
	if _, ok := spec["task"]; !ok {
		spec["task"] = map[string]any{"turns": []string{"fix it"}}
	}
	spec["id"] = id
	spec["schema_version"] = 1
	data, err := json.Marshal(spec)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "trajectory.json"), data, 0o644))
	return dir
}

func newEvalDir(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "corpus"), 0o755))
	return root
}

// --- trajectory validation ---

func TestValidateTrajectory_PassGuardRequiresCounterexample(t *testing.T) {
	t.Parallel()
	root := newEvalDir(t)
	dir := writeTrajectory(t, filepath.Join(root, "corpus"), "t1", map[string]any{
		"check": map[string]any{"script": "check.sh", "expect_start_state": "pass"},
	})
	tr, err := LoadTrajectory(dir)
	require.Error(t, err)
	require.Nil(t, tr)
	require.Contains(t, err.Error(), "counterexample.patch is required")
}

func TestValidateTrajectory_CounterexampleOnFailIsError(t *testing.T) {
	t.Parallel()
	root := newEvalDir(t)
	dir := writeTrajectory(t, filepath.Join(root, "corpus"), "t1", nil)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "counterexample.patch"), []byte("x"), 0o644))
	_, err := LoadTrajectory(dir)
	require.Error(t, err)
	require.Contains(t, err.Error(), "counterexample.patch on a \"fail\" trajectory")
}

func TestValidateTrajectory_RegressionRequiresReference(t *testing.T) {
	t.Parallel()
	root := newEvalDir(t)
	dir := writeTrajectory(t, filepath.Join(root, "corpus"), "t1", map[string]any{
		"origin": map[string]any{"kind": "regression"},
	})
	_, err := LoadTrajectory(dir)
	require.Error(t, err)
	require.Contains(t, err.Error(), "reference.patch is required")
}

func TestValidateTrajectory_ProductionRequiresScrubbed(t *testing.T) {
	t.Parallel()
	root := newEvalDir(t)
	dir := writeTrajectory(t, filepath.Join(root, "corpus"), "t1", map[string]any{
		"origin": map[string]any{"kind": "production"},
	})
	_, err := LoadTrajectory(dir)
	require.Error(t, err)
	require.Contains(t, err.Error(), "scrubbed")
}

func TestValidateTrajectory_GitNetworkConflict(t *testing.T) {
	t.Parallel()
	root := newEvalDir(t)
	dir := writeTrajectory(t, filepath.Join(root, "corpus"), "t1", map[string]any{
		"start_state": map[string]any{"kind": "git", "repo": "https://example.com/r.git", "ref": "abc"},
		"requires":    map[string]any{"network": false},
	})
	_, err := LoadTrajectory(dir)
	require.Error(t, err)
	require.Contains(t, err.Error(), "network=false")
}

func TestValidateTrajectory_OK(t *testing.T) {
	t.Parallel()
	root := newEvalDir(t)
	dir := writeTrajectory(t, filepath.Join(root, "corpus"), "t1", nil)
	tr, err := LoadTrajectory(dir)
	require.NoError(t, err)
	require.Equal(t, "t1", tr.ID)
}

// --- content hash ---

func TestContentHash_ScopesToRevision(t *testing.T) {
	t.Parallel()
	root := newEvalDir(t)
	dir := writeTrajectory(t, filepath.Join(root, "corpus"), "t1", nil)
	h1, err := ContentHash(dir)
	require.NoError(t, err)
	h2, err := ContentHash(dir)
	require.NoError(t, err)
	require.Equal(t, h1, h2)

	// Editing the check changes the hash — samples scored by the old
	// function are stale.
	require.NoError(t, os.WriteFile(filepath.Join(dir, "check.sh"), []byte("#!/bin/bash\nexit 0\n"), 0o755))
	h3, err := ContentHash(dir)
	require.NoError(t, err)
	require.NotEqual(t, h1, h3)
}

// --- check.sh contract ---

func TestRunCheck_PassFailAndEvalJSON(t *testing.T) {
	t.Parallel()
	root := newEvalDir(t)
	dir := writeTrajectory(t, filepath.Join(root, "corpus"), "t1", map[string]any{
		"check_script_body": "#!/bin/bash\necho 'EVAL_JSON {\"why\":\"marker missing\"}'\nexit 1\n",
	})
	tr, err := LoadTrajectory(dir)
	require.NoError(t, err)
	wd := t.TempDir()
	res := RunCheck(context.Background(), tr, dir, wd, nil)
	require.NoError(t, res.Err)
	require.Equal(t, 1, res.Exit)
	require.Equal(t, "marker missing", res.Detail["why"])

	// Malformed EVAL_JSON never changes the verdict.
	dir2 := writeTrajectory(t, filepath.Join(root, "corpus"), "t2", map[string]any{
		"check_script_body": "#!/bin/bash\necho 'EVAL_JSON {broken'\nexit 0\n",
	})
	tr2, err := LoadTrajectory(dir2)
	require.NoError(t, err)
	res2 := RunCheck(context.Background(), tr2, dir2, wd, nil)
	require.Equal(t, 0, res2.Exit)
	require.Nil(t, res2.Detail)
}

func TestRunCheck_TimeoutIsError(t *testing.T) {
	t.Parallel()
	root := newEvalDir(t)
	dir := writeTrajectory(t, filepath.Join(root, "corpus"), "t1", map[string]any{
		"check":             map[string]any{"script": "check.sh", "expect_start_state": "fail", "timeout_seconds": 1},
		"check_script_body": "#!/bin/bash\nsleep 5\n",
	})
	tr, err := LoadTrajectory(dir)
	require.NoError(t, err)
	res := RunCheck(context.Background(), tr, dir, t.TempDir(), nil)
	require.Error(t, res.Err)
	require.Contains(t, res.Err.Error(), "timed out")
}

// --- coverage ---

func TestCoverage_MinMaxAndClosedGrammar(t *testing.T) {
	t.Parallel()
	rec := &RunRecord{Steps: 6}
	rec.StubStats.BoundaryAdvances = 2
	met, err := CoverageMet(Coverage{"min_steps": 3, "min_stub_stats.boundary_advances": 1}, rec)
	require.NoError(t, err)
	require.True(t, met)

	met, err = CoverageMet(Coverage{"min_stub_stats.boundary_advances": 3}, rec)
	require.NoError(t, err)
	require.False(t, met)

	_, _, err = ParseCoverageKey("min_steps")
	require.NoError(t, err)
	_, _, err = ParseCoverageKey("min_bogus")
	require.Error(t, err)
	_, _, err = ParseCoverageKey("gte_steps")
	require.Error(t, err)
}

func TestCoverage_StubKinds(t *testing.T) {
	t.Parallel()
	rec := &RunRecord{}
	rec.StubStats.Kinds = map[string]int{"deleted": 2, "superseded": 1}

	// Every declared kind is a valid coverage field — a new kind that
	// missed registration fails here.
	for _, kind := range message.StubKinds() {
		_, _, err := ParseCoverageKey("min_stub_stats.kinds." + kind.String())
		require.NoError(t, err, "kind %q must be a valid coverage field", kind)
	}

	met, err := CoverageMet(Coverage{"min_stub_stats.kinds.deleted": 1}, rec)
	require.NoError(t, err)
	require.True(t, met)

	// The empty-string superseded mark spells "superseded" in the
	// kinds map — and is a valid predicate.
	met, err = CoverageMet(Coverage{"min_stub_stats.kinds.superseded": 1}, rec)
	require.NoError(t, err)
	require.True(t, met)

	// Kinds absent from the map count as 0.
	met, err = CoverageMet(Coverage{"min_stub_stats.kinds.rerun": 1}, rec)
	require.NoError(t, err)
	require.False(t, met)

	// An unknown kind is still rejected by the closed grammar.
	_, _, err = ParseCoverageKey("min_stub_stats.kinds.bogus")
	require.Error(t, err)
}

func TestCoverage_ArmScoped(t *testing.T) {
	t.Parallel()

	// The arm grammar reaches the flag-dependent call_metrics fields
	// trajectory coverage excludes — asserting "the model called map"
	// is legal only where the arm itself fixes project_index.
	_, _, err := ParseArmCoverageKey("min_call_metrics.map_calls_ok")
	require.NoError(t, err)
	_, _, err = ParseCoverageKey("min_call_metrics.map_calls_ok")
	require.Error(t, err)

	// Shared fields work in both grammars.
	_, _, err = ParseArmCoverageKey("min_stub_stats.results")
	require.NoError(t, err)

	rec := &RunRecord{CallMetrics: &CallMetrics{MapCalls: 3, MapCallsOK: 2}}
	met, err := ArmCoverageMet(Coverage{"min_call_metrics.map_calls": 1}, rec)
	require.NoError(t, err)
	require.True(t, met)

	met, err = ArmCoverageMet(Coverage{"min_call_metrics.map_calls_ok": 3}, rec)
	require.NoError(t, err)
	require.False(t, met)

	// Same fail-closed rule as trajectory coverage: absent analysis
	// starves call_metrics predicates, even max_ ones.
	met, err = ArmCoverageMet(Coverage{"max_call_metrics.map_result_bytes": 1024}, &RunRecord{})
	require.NoError(t, err)
	require.False(t, met)

	// Unknown fields stay rejected.
	_, _, err = ParseArmCoverageKey("min_call_metrics.bogus")
	require.Error(t, err)
}

func TestCoverage_RequestFields(t *testing.T) {
	t.Parallel()

	// request.* decomposes the final rendered request — the direct
	// "did content reach the prompt" predicate.
	for _, f := range []string{
		"prompt_requests", "prompt_tokens_peak", "system_bytes",
		"notebook_bytes", "history_bytes", "tool_call_bytes",
		"tool_result_bytes",
	} {
		_, _, err := ParseCoverageKey("min_request." + f)
		require.NoError(t, err, "request.%s must be a coverage field", f)
	}

	rec := &RunRecord{Request: &RequestStats{NotebookBytes: 4096, SystemBytes: 21037}}
	met, err := CoverageMet(Coverage{"min_request.notebook_bytes": 1}, rec)
	require.NoError(t, err)
	require.True(t, met)
	met, err = CoverageMet(Coverage{"min_request.notebook_bytes": 4097}, rec)
	require.NoError(t, err)
	require.False(t, met)

	// Absent request stats fail closed in BOTH directions — a
	// missing snapshot is not "0 bytes rendered".
	bare := &RunRecord{}
	met, err = CoverageMet(Coverage{"min_request.system_bytes": 1}, bare)
	require.NoError(t, err)
	require.False(t, met)
	met, err = CoverageMet(Coverage{"max_request.system_bytes": 1}, bare)
	require.NoError(t, err)
	require.False(t, met)
	met, err = ArmCoverageMet(Coverage{"min_request.notebook_bytes": 1}, bare)
	require.NoError(t, err)
	require.False(t, met)
}

func TestCoverage_PressureAbsentFailsClosed(t *testing.T) {
	t.Parallel()

	// No Pressure block means the gate never evaluated — notebook or
	// gate flag off, or no window to measure against. Predicates fail
	// closed in BOTH directions: absent silence is not a zero.
	bare := &RunRecord{}
	met, err := ArmCoverageMet(Coverage{"max_pressure.activations": 0}, bare)
	require.NoError(t, err)
	require.False(t, met, "absent gate telemetry must not satisfy max_")
	met, err = ArmCoverageMet(Coverage{"min_pressure.activations": 0}, bare)
	require.NoError(t, err)
	require.False(t, met)

	// Present and silent: the gate ran and never fired — the
	// comfortable-regime assertion.
	rec := &RunRecord{Pressure: &Pressure{Estimate: 42_000}}
	met, err = ArmCoverageMet(Coverage{"max_pressure.activations": 0}, rec)
	require.NoError(t, err)
	require.True(t, met)

	// Present and fired: the latch reads through.
	rec.Pressure.Activations = 1
	rec.Pressure.Engaged = true
	met, err = ArmCoverageMet(Coverage{"min_pressure.engaged": 1}, rec)
	require.NoError(t, err)
	require.True(t, met)
}

// The telemetry doc's pressure block decodes into runTelemetry and
// folds across the trajectory's per-turn processes: activations sum
// (each process latches independently), engaged ORs, and estimate
// keeps the latest non-zero — the wire contract and its semantics.
func TestRunTelemetry_PressureDecodeAndFold(t *testing.T) {
	t.Parallel()

	var tel runTelemetry
	require.NoError(t, json.Unmarshal([]byte(
		`{"pressure":{"activations":1,"engaged":true,"estimate":51200}}`), &tel))
	require.Equal(t, 1, tel.Pressure.Activations)
	require.True(t, tel.Pressure.Engaged)
	require.Equal(t, int64(51_200), tel.Pressure.Estimate)

	var res RunResult
	res.addTurnTelemetry(tel, 0)
	res.addTurnTelemetry(tel, 1)
	require.Equal(t, 2, res.Pressure.Activations, "per-process latches sum")
	require.True(t, res.Pressure.Engaged)
	require.Equal(t, int64(51_200), res.Pressure.Estimate)

	// A later turn's larger estimate wins; a turn with none doesn't
	// clobber the audit trail.
	tel.Pressure.Estimate = 61_000
	res.addTurnTelemetry(tel, 2)
	res.addTurnTelemetry(runTelemetry{}, 3)
	require.Equal(t, int64(61_000), res.Pressure.Estimate)
}

func TestWriteOnlyCoverageWarnings(t *testing.T) {
	t.Parallel()

	// A write-side predicate without its render sibling warns —
	// commits can land while the prompt never changes.
	w := writeOnlyCoverageWarnings("treatment", Coverage{"min_checkpoints.written": 1})
	require.Len(t, w, 1)
	require.Contains(t, w[0], "checkpoints.rendered")

	w = writeOnlyCoverageWarnings("treatment", Coverage{"min_hydration.seeds": 1})
	require.Len(t, w, 1)
	require.Contains(t, w[0], "hydration.rendered")

	// The render sibling present → quiet.
	require.Empty(t, writeOnlyCoverageWarnings("treatment", Coverage{
		"min_checkpoints.written": 1, "min_checkpoints.rendered": 1,
	}))

	// Fields outside the write/render pairs stay quiet — and max_ on
	// a write field is a bound, not a firing claim.
	require.Empty(t, writeOnlyCoverageWarnings("treatment", Coverage{"min_steps": 3}))
	require.Empty(t, writeOnlyCoverageWarnings("treatment", Coverage{"max_digests.written": 5}))
}

func TestReport_VerdictPower(t *testing.T) {
	t.Parallel()

	// Quiet + no powered tier → INCONCLUSIVE: a run where neither
	// evidence tier could have spoken is not a pass.
	rep := Report{CatastrophicEligible: map[string]bool{"a": false}, DiffuseP: 1}
	require.Contains(t, rep.Summary(0.05), "verdict: INCONCLUSIVE")

	// An eligible catastrophic trajectory powers the tier → quiet
	// reads PASS again.
	rep = Report{CatastrophicEligible: map[string]bool{"a": true}, DiffuseP: 1}
	require.Contains(t, rep.Summary(0.05), "verdict: PASS")

	// Diffuse pairs power the other tier.
	rep = Report{CatastrophicEligible: map[string]bool{}, DiffusePairs: 2, DiffuseP: 0.9}
	require.Contains(t, rep.Summary(0.05), "verdict: PASS")

	// Alarms override regardless of power.
	rep = Report{
		CatastrophicEligible: map[string]bool{"a": false},
		DiffuseP:             1,
		Smoke:                []string{"a"},
	}
	require.Contains(t, rep.Summary(0.05), "verdict: FAIL")
}

func TestValidateExperiment_ArmCoverage(t *testing.T) {
	t.Parallel()
	temp := 0.0
	exp := &Experiment{
		Name:              "x",
		Model:             "p/m",
		Temperature:       &temp,
		Corpus:            []string{"*"},
		RunsPerTrajectory: map[Band]int{BandUncharacterized: 1},
		Arms: map[string]Arm{
			ArmControl:   {},
			ArmTreatment: {Coverage: Coverage{"min_call_metrics.map_calls": 1}},
		},
	}
	require.NoError(t, ValidateExperiment(exp))

	exp.Arms[ArmTreatment] = Arm{Coverage: Coverage{"min_bogus_field": 1}}
	require.Error(t, ValidateExperiment(exp))
}

func TestValidateExperiment_ArmCoverageStarvation(t *testing.T) {
	t.Parallel()
	temp := 0.0
	exp := &Experiment{
		Name:              "x",
		Model:             "p/m",
		Temperature:       &temp,
		Corpus:            []string{"*"},
		RunsPerTrajectory: map[Band]int{BandUncharacterized: 1},
		Arms:              map[string]Arm{ArmControl: {}, ArmTreatment: {}},
	}

	// min_ on a flag-gated field where the arm sets the flag off —
	// every run starves, so this is a load error.
	exp.Arms[ArmControl] = Arm{
		Config:   ArmConfig{Options: map[string]any{"notebook_stub_superseded": false}},
		Coverage: Coverage{"min_stub_stats.results": 1},
	}
	require.Error(t, ValidateExperiment(exp))

	// Same predicate on the arm that enables the flag is the intended
	// firing assertion — legal.
	exp.Arms[ArmControl] = Arm{}
	exp.Arms[ArmTreatment] = Arm{
		Config:   ArmConfig{Options: map[string]any{"notebook_stub_superseded": true}},
		Coverage: Coverage{"min_stub_stats.results": 1},
	}
	require.NoError(t, ValidateExperiment(exp))

	// The question tool is interactive-only — min_ predicates on
	// question_* starve in headless runs regardless of flags.
	exp.Arms[ArmTreatment] = Arm{Coverage: Coverage{"min_call_metrics.question_calls": 1}}
	require.Error(t, ValidateExperiment(exp))

	// max_ is a bound, not a firing assertion — still legal.
	exp.Arms[ArmTreatment] = Arm{Coverage: Coverage{"max_call_metrics.question_calls": 0}}
	require.NoError(t, ValidateExperiment(exp))

	// map_calls_ok needs the flag; an arm that sets it off starves.
	exp.Arms[ArmTreatment] = Arm{
		Config:   ArmConfig{Options: map[string]any{"project_index": false}},
		Coverage: Coverage{"min_call_metrics.map_calls_ok": 1},
	}
	require.Error(t, ValidateExperiment(exp))

	// map_calls itself is NOT guarded — tool-not-found attempts count,
	// so flag-off arms can measure unprompted map reach.
	exp.Arms[ArmTreatment] = Arm{
		Config:   ArmConfig{Options: map[string]any{"project_index": false}},
		Coverage: Coverage{"min_call_metrics.map_calls": 1},
	}
	require.NoError(t, ValidateExperiment(exp))

	// wrong_pointer_events needs a successful map call — zero on
	// flag-off arms, so min_ starves there.
	exp.Arms[ArmTreatment] = Arm{
		Config:   ArmConfig{Options: map[string]any{"project_index": false}},
		Coverage: Coverage{"min_call_metrics.wrong_pointer_events": 1},
	}
	require.Error(t, ValidateExperiment(exp))

	// request.notebook_bytes is zero by construction when the
	// notebook is off — min_ starves on a notebook-disabled arm.
	exp.Arms[ArmTreatment] = Arm{
		Config:   ArmConfig{Options: map[string]any{"notebook_enabled": false}},
		Coverage: Coverage{"min_request.notebook_bytes": 1},
	}
	require.Error(t, ValidateExperiment(exp))
	exp.Arms[ArmTreatment] = Arm{
		Config:   ArmConfig{Options: map[string]any{"notebook_enabled": true}},
		Coverage: Coverage{"min_request.notebook_bytes": 1},
	}
	require.NoError(t, ValidateExperiment(exp))
}

func TestValidateArmCoverageResolved(t *testing.T) {
	t.Parallel()
	manifest := &FlagsManifest{Defaults: map[string]any{
		"notebook_stub_superseded": false,
		"notebook_enabled":         true,
		"project_index":            false,
	}}
	exp := &Experiment{Arms: map[string]Arm{
		ArmControl:   {},
		ArmTreatment: {},
	}}

	// Firing assertion with NO config: the flag resolves to the
	// manifest's false default — silent starvation, now an error.
	exp.Arms[ArmTreatment] = Arm{Coverage: Coverage{"min_stub_stats.results": 1}}
	require.Error(t, ValidateArmCoverageResolved(exp, manifest))

	// Enabled by the arm — resolves on.
	exp.Arms[ArmTreatment] = Arm{
		Config:   ArmConfig{Options: map[string]any{"notebook_stub_superseded": true}},
		Coverage: Coverage{"min_stub_stats.results": 1},
	}
	require.NoError(t, ValidateArmCoverageResolved(exp, manifest))

	// Enabled by manifest default — resolves on without arm config.
	manifest.Defaults["project_index"] = true
	exp.Arms[ArmTreatment] = Arm{Coverage: Coverage{"min_call_metrics.map_result_bytes": 1}}
	require.NoError(t, ValidateArmCoverageResolved(exp, manifest))

	// Ambiguity-gated edges default off — a fired assertion on an arm
	// that omits the flag starves silently.
	exp.Arms[ArmTreatment] = Arm{Coverage: Coverage{"min_edge_firings.stall.fired": 1}}
	require.Error(t, ValidateArmCoverageResolved(exp, manifest))

	// pressure.* needs the notebook AND the gate: pinning the gate
	// off starves the predicate even though the flag defaults on.
	exp.Arms[ArmTreatment] = Arm{
		Config:   ArmConfig{Options: map[string]any{"notebook_pressure_gate": false}},
		Coverage: Coverage{"min_pressure.activations": 1},
	}
	require.Error(t, ValidateArmCoverageResolved(exp, manifest))

	// Notebook off starves it too, gate pin notwithstanding.
	exp.Arms[ArmTreatment] = Arm{
		Config:   ArmConfig{Options: map[string]any{"notebook_enabled": false}},
		Coverage: Coverage{"min_pressure.activations": 1},
	}
	require.Error(t, ValidateArmCoverageResolved(exp, manifest))

	// Unpinned resolves on via the code default (the gate defaults
	// true) — the firing assertion is legal.
	exp.Arms[ArmTreatment] = Arm{Coverage: Coverage{"min_pressure.activations": 1}}
	require.NoError(t, ValidateArmCoverageResolved(exp, manifest))
}

func TestValidateExperiment_PriorTurnsModeStarvation(t *testing.T) {
	t.Parallel()
	temp := 0.7
	mk := func(opts map[string]any) *Experiment {
		return &Experiment{
			Name:              "x",
			Model:             "p/m",
			Temperature:       &temp,
			Corpus:            []string{"*"},
			RunsPerTrajectory: map[Band]int{BandUncharacterized: 1},
			Arms: map[string]Arm{
				ArmControl: {},
				ArmTreatment: {
					Config:   ArmConfig{Options: opts},
					Coverage: Coverage{"min_prior_turns.turns_collapsed": 1},
				},
			},
		}
	}

	// A collapse mode + notebook on is the intended assertion — legal.
	require.NoError(t, ValidateExperiment(mk(map[string]any{"notebook_enabled": true, "notebook_prior_turns": "stub"})))
	require.NoError(t, ValidateExperiment(mk(map[string]any{"notebook_enabled": true, "notebook_prior_turns": "digest"})))
	require.NoError(t, ValidateExperiment(mk(map[string]any{"notebook_enabled": true, "notebook_prior_turns": "summarize"})))

	// verbatim arms produce no collapse counters — the predicate
	// starves on every run.
	require.Error(t, ValidateExperiment(mk(map[string]any{"notebook_enabled": true, "notebook_prior_turns": "verbatim"})))

	// Notebook off coerces any mode to verbatim — starves.
	require.Error(t, ValidateExperiment(mk(map[string]any{"notebook_enabled": false, "notebook_prior_turns": "stub"})))

	// stub/digest print recall pointers — disabling the tool coerces
	// the mode to verbatim at agent build.
	require.Error(t, ValidateExperiment(mk(map[string]any{
		"notebook_enabled":     true,
		"notebook_prior_turns": "stub",
		"disabled_tools":       []any{"recall"},
	})))

	// summarize renders entries inline — no recall tool needed.
	require.NoError(t, ValidateExperiment(mk(map[string]any{
		"notebook_enabled":     true,
		"notebook_prior_turns": "summarize",
		"disabled_tools":       []any{"recall"},
	})))
}

func TestValidateExperiment_BoundaryAdvancesStarvation(t *testing.T) {
	t.Parallel()
	temp := 0.7
	mk := func(opts map[string]any, cov Coverage) *Experiment {
		return &Experiment{
			Name:              "x",
			Model:             "p/m",
			Temperature:       &temp,
			Corpus:            []string{"*"},
			RunsPerTrajectory: map[Band]int{BandUncharacterized: 1},
			Arms: map[string]Arm{
				ArmControl: {},
				ArmTreatment: {
					Config:   ArmConfig{Options: opts},
					Coverage: cov,
				},
			},
		}
	}
	advances := Coverage{"min_stub_stats.boundary_advances": 1}

	// Boundary moves count whenever the notebook prefix renders —
	// verbatim arms churn too, so only notebook_enabled gates it.
	require.NoError(t, ValidateExperiment(mk(map[string]any{"notebook_prior_turns": "verbatim"}, advances)))
	require.NoError(t, ValidateExperiment(mk(map[string]any{"notebook_prior_turns": "summarize"}, advances)))
	require.NoError(t, ValidateExperiment(mk(nil, advances)))
	require.Error(t, ValidateExperiment(mk(map[string]any{"notebook_enabled": false}, advances)))

	// Other stub_stats.* fields stay supersede-only — collapse modes
	// don't feed them.
	require.Error(t, ValidateExperiment(mk(
		map[string]any{"notebook_prior_turns": "summarize", "notebook_stub_superseded": false},
		Coverage{"min_stub_stats.results": 1})))
}

func TestValidateExperiment_EdgeFiringsStarvation(t *testing.T) {
	t.Parallel()
	temp := 0.7
	mk := func(opts map[string]any, cov Coverage) *Experiment {
		return &Experiment{
			Name:              "x",
			Model:             "p/m",
			Temperature:       &temp,
			Corpus:            []string{"*"},
			RunsPerTrajectory: map[Band]int{BandUncharacterized: 1},
			Arms: map[string]Arm{
				ArmControl: {},
				ArmTreatment: {
					Config:   ArmConfig{Options: opts},
					Coverage: cov,
				},
			},
		}
	}

	// Acting outcomes need ambiguity_clarification on — flag-off
	// triggers take the gated short-circuit before resolve.
	require.Error(t, ValidateExperiment(mk(
		map[string]any{"ambiguity_clarification": false},
		Coverage{"min_edge_firings.stall.fired": 1})))
	require.NoError(t, ValidateExperiment(mk(
		map[string]any{"ambiguity_clarification": true},
		Coverage{"min_edge_firings.stall.fired": 1})))
	require.NoError(t, ValidateExperiment(mk(
		map[string]any{"ambiguity_clarification": true},
		Coverage{"min_edge_firings.burn-watch.exhausted": 1})))

	// "gated" rows only exist in the flag-off arm — asserting them on
	// a flag-on arm starves.
	require.Error(t, ValidateExperiment(mk(
		map[string]any{"ambiguity_clarification": true},
		Coverage{"min_edge_firings.stall.gated": 1})))
	require.NoError(t, ValidateExperiment(mk(
		map[string]any{"ambiguity_clarification": false},
		Coverage{"min_edge_firings.stall.gated": 1})))

	// cancelled is flag-independent (mid-scan ctx kills); verification
	// and todos edges are flag-invariant entirely.
	require.NoError(t, ValidateExperiment(mk(
		map[string]any{"ambiguity_clarification": false},
		Coverage{"min_edge_firings.stall.cancelled": 1})))
	require.NoError(t, ValidateExperiment(mk(
		map[string]any{"ambiguity_clarification": false},
		Coverage{"min_edge_firings.verification.fired": 1})))

	// Unreachable (edge, outcome) pairs starve in EVERY config —
	// suppressed is burn-watch-only, stall/burn-watch are step-bound
	// and never clear, and verification/todos never set hints.
	for _, field := range []string{
		"min_edge_firings.stall.suppressed",
		"min_edge_firings.stall.cleared",
		"min_edge_firings.burn-watch.cleared",
		"min_edge_firings.verification.gated",
		"min_edge_firings.verification.suppressed",
		"min_edge_firings.todos.headless-degraded",
		"min_edge_firings.todos.gated",
		"min_edge_firings.no-such-edge.fired",
	} {
		require.Error(t, ValidateExperiment(mk(
			map[string]any{"ambiguity_clarification": true},
			Coverage{field: 1})), field)
		require.Error(t, ValidateExperiment(mk(
			map[string]any{"ambiguity_clarification": false},
			Coverage{field: 1})), field)
	}

	// Reachable flag-invariant pairs pass on either flag state.
	for _, field := range []string{
		"min_edge_firings.verification.fired",
		"min_edge_firings.verification.cleared",
		"min_edge_firings.todos.cleared",
	} {
		require.NoError(t, ValidateExperiment(mk(
			map[string]any{"ambiguity_clarification": false},
			Coverage{field: 1})), field)
	}

	// suppressed is reachable but flag-ON only — the gated
	// short-circuit precedes the burnWatched marker check.
	require.Error(t, ValidateExperiment(mk(
		map[string]any{"ambiguity_clarification": false},
		Coverage{"min_edge_firings.burn-watch.suppressed": 1})))
	require.NoError(t, ValidateExperiment(mk(
		map[string]any{"ambiguity_clarification": true},
		Coverage{"min_edge_firings.burn-watch.suppressed": 1})))
}

func TestValidateExperiment_RecallDisabledStarvation(t *testing.T) {
	t.Parallel()
	temp := 0.7
	mk := func(disabled any) *Experiment {
		return &Experiment{
			Name:              "x",
			Model:             "p/m",
			Temperature:       &temp,
			Corpus:            []string{"*"},
			RunsPerTrajectory: map[Band]int{BandUncharacterized: 1},
			Arms: map[string]Arm{
				ArmControl: {},
				ArmTreatment: {
					Config:   ArmConfig{Options: map[string]any{"disabled_tools": disabled}},
					Coverage: Coverage{"min_recalls.prior_turn_result": 1},
				},
			},
		}
	}

	// recalls.* counters need the recall tool — disabling it starves
	// them. disabled_tools may arrive as []any (JSON) or []string
	// (fixtures) — disablesTool handles both.
	require.Error(t, ValidateExperiment(mk([]any{"recall"})))
	require.Error(t, ValidateExperiment(mk([]string{"recall"})))
	require.NoError(t, ValidateExperiment(mk([]any{"bash"})))
}

func TestValidateArmCoverageResolved_PriorTurnsModeStarvation(t *testing.T) {
	t.Parallel()
	manifest := &FlagsManifest{Defaults: map[string]any{
		"notebook_enabled":     true,
		"notebook_prior_turns": "verbatim",
	}}
	exp := &Experiment{Arms: map[string]Arm{ArmControl: {}, ArmTreatment: {}}}

	// The arm omits the mode: the manifest's verbatim default means
	// no collapse counters exist — silent starvation, now an error.
	exp.Arms[ArmTreatment] = Arm{Coverage: Coverage{"min_prior_turns.turns_collapsed": 1}}
	require.Error(t, ValidateArmCoverageResolved(exp, manifest))

	// digests.* needs digest mode specifically, not any collapse mode.
	exp.Arms[ArmTreatment] = Arm{
		Config:   ArmConfig{Options: map[string]any{"notebook_prior_turns": "stub"}},
		Coverage: Coverage{"min_digests.written": 1},
	}
	require.Error(t, ValidateArmCoverageResolved(exp, manifest))
	exp.Arms[ArmTreatment].Config.Options["notebook_prior_turns"] = "digest"
	require.NoError(t, ValidateArmCoverageResolved(exp, manifest))
}

func TestValidateArmCoverageVsCorpus(t *testing.T) {
	t.Parallel()
	mkTraj := func(id string, turns int) *Trajectory {
		ts := make([]string, turns)
		for i := range ts {
			ts[i] = "turn"
		}
		return &Trajectory{ID: id, Task: Task{Turns: ts}}
	}
	mk := func(cov Coverage) *Experiment {
		return &Experiment{Arms: map[string]Arm{
			ArmControl:   {},
			ArmTreatment: {Coverage: cov},
		}}
	}
	trajs3 := []*Trajectory{mkTraj("a", 3), mkTraj("b", 3)}

	// turns_collapsed ceiling is T−1: the last turn never collapses.
	require.NoError(t, ValidateArmCoverageVsCorpus(mk(Coverage{"min_prior_turns.turns_collapsed": 2}), trajs3))
	require.Error(t, ValidateArmCoverageVsCorpus(mk(Coverage{"min_prior_turns.turns_collapsed": 3}), trajs3))
	require.Error(t, ValidateArmCoverageVsCorpus(mk(Coverage{"min_prior_turns.turns_collapsed": 1}), []*Trajectory{mkTraj("one", 1)}))

	// digests.written ceiling is T — the run-end pass digests the
	// just-finished turn too.
	require.NoError(t, ValidateArmCoverageVsCorpus(mk(Coverage{"min_digests.written": 3}), trajs3))
	require.Error(t, ValidateArmCoverageVsCorpus(mk(Coverage{"min_digests.written": 4}), trajs3))

	// Prior-turn counters are structurally zero below two turns even
	// without a tighter bound.
	require.Error(t, ValidateArmCoverageVsCorpus(mk(Coverage{"min_digests.rendered": 1}), []*Trajectory{mkTraj("one", 1)}))
	require.NoError(t, ValidateArmCoverageVsCorpus(mk(Coverage{"min_digests.rendered": 1}), trajs3))
	// checkpoints.rendered shares the floor: a checkpoint committed
	// at turn k renders at k+1 earliest — no guaranteed render on a
	// 1-turn trajectory.
	require.Error(t, ValidateArmCoverageVsCorpus(mk(Coverage{"min_checkpoints.rendered": 1}), []*Trajectory{mkTraj("one", 1)}))
	require.NoError(t, ValidateArmCoverageVsCorpus(mk(Coverage{"min_checkpoints.rendered": 1}), trajs3))
	require.Error(t, ValidateArmCoverageVsCorpus(mk(Coverage{"min_recalls.prior_turn_result": 1}), []*Trajectory{mkTraj("one", 1)}))
	require.Error(t, ValidateArmCoverageVsCorpus(mk(Coverage{"min_prior_turns.events_collapsed": 1}), []*Trajectory{mkTraj("one", 1)}))

	// max_ bounds and non-turn-bounded fields pass anywhere.
	require.NoError(t, ValidateArmCoverageVsCorpus(mk(Coverage{"max_prior_turns.turns_collapsed": 5}), []*Trajectory{mkTraj("one", 1)}))
	require.NoError(t, ValidateArmCoverageVsCorpus(mk(Coverage{"min_checkpoints.written": 9}), trajs3))

	// A violation on ANY selected trajectory errors and names it.
	err := ValidateArmCoverageVsCorpus(mk(Coverage{"min_prior_turns.turns_collapsed": 1}), []*Trajectory{mkTraj("ok", 3), mkTraj("short", 1)})
	require.Error(t, err)
	require.Contains(t, err.Error(), `"short"`)
}

func TestValidateTrajectory_FlagGatedMinRejected(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "fixture"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "check.sh"), []byte("#!/bin/sh\nexit 1\n"), 0o755))
	mk := func(cov Coverage) *Trajectory {
		return &Trajectory{
			ID: "x", SchemaVersion: 1,
			Origin:     Origin{Kind: "synthetic"},
			StartState: StartState{Kind: "fixture", FixtureDir: "fixture"},
			Task:       Task{Turns: []string{"do it"}},
			Check:      Check{Script: "check.sh", ExpectStartState: "fail"},
			Coverage:   cov,
		}
	}

	// A shared min_ on a flag-gated field starves the flag-off arm —
	// load-time error, not a surprise at run time.
	problems := ValidateTrajectory(mk(Coverage{"min_stub_stats.results": 1}), dir)
	require.NotEmpty(t, problems)
	require.Contains(t, problems[0], "flag-gated")

	problems = ValidateTrajectory(mk(Coverage{"min_recalls.entry": 1}), dir)
	require.NotEmpty(t, problems)

	// request.notebook_bytes is zero by construction on a
	// notebook-off arm — a shared min_ starves it.
	problems = ValidateTrajectory(mk(Coverage{"min_request.notebook_bytes": 1}), dir)
	require.NotEmpty(t, problems)
	require.Contains(t, problems[0], "flag-gated")

	// max_ bounds both arms legitimately — still legal unscoped.
	problems = ValidateTrajectory(mk(Coverage{"max_stub_stats.results": 10}), dir)
	require.Empty(t, problems)

	// Flag-invariant fields unaffected.
	problems = ValidateTrajectory(mk(Coverage{"min_steps": 1}), dir)
	require.Empty(t, problems)
}

func TestValidateTrajectory_StepsBudgetCeiling(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "fixture"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "check.sh"), []byte("#!/bin/sh\nexit 1\n"), 0o755))
	mk := func(cov Coverage) *Trajectory {
		return &Trajectory{
			ID: "x", SchemaVersion: 1,
			Origin:     Origin{Kind: "synthetic"},
			StartState: StartState{Kind: "fixture", FixtureDir: "fixture"},
			Task:       Task{Turns: []string{"do it"}},
			Budget:     Budget{MaxSteps: 10},
			Check:      Check{Script: "check.sh", ExpectStartState: "fail"},
			Coverage:   cov,
		}
	}

	// steps and request.prompt_requests count the same event stream
	// — a min_ above the trajectory-wide cap starves forever.
	problems := ValidateTrajectory(mk(Coverage{"min_steps": 11}), dir)
	require.NotEmpty(t, problems)
	require.Contains(t, problems[0], "max_steps")

	problems = ValidateTrajectory(mk(Coverage{"min_request.prompt_requests": 11}), dir)
	require.NotEmpty(t, problems)
	require.Contains(t, problems[0], "max_steps")

	// At the cap is reachable; max_ bounds stay legal above it.
	require.Empty(t, ValidateTrajectory(mk(Coverage{"min_request.prompt_requests": 10}), dir))
	require.Empty(t, ValidateTrajectory(mk(Coverage{"max_request.prompt_requests": 11}), dir))
}

// --- run telemetry ---

// TestRunTelemetry_StubKinds pins the telemetry contract end to end:
// the child's stub_stats.kinds object parses, and per-turn kind deltas
// sum into the trajectory totals like the other counters.
func TestRunTelemetry_StubKinds(t *testing.T) {
	t.Parallel()
	writeTel := func(kinds map[string]int) runTelemetry {
		doc := map[string]any{
			"session_id": "s1",
			"steps":      3,
			"stub_stats": map[string]any{
				"invalidations":     1,
				"results":           2,
				"saved_bytes":       900,
				"boundary_advances": 1,
				"kinds":             kinds,
			},
		}
		path := filepath.Join(t.TempDir(), "tel.json")
		data, err := json.Marshal(doc)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(path, data, 0o644))
		tel, err := readTelemetry(path)
		require.NoError(t, err)
		return tel
	}

	var res RunResult
	res.addTurnTelemetry(writeTel(map[string]int{"superseded": 1, "stale": 1}), 0)
	// A mid-run turn may emit no kinds at all — sparse map or absent.
	res.addTurnTelemetry(writeTel(nil), 1)
	res.addTurnTelemetry(writeTel(map[string]int{"deleted": 2}), 2)

	require.Equal(t, 9, res.Steps)
	require.Equal(t, 6, res.StubStats.Results)
	require.Equal(t, map[string]int{"superseded": 1, "stale": 1, "deleted": 2}, res.StubStats.Kinds)
}

// TestRunTelemetry_EdgeFirings pins the edge_firings contract: the
// child's per-edge outcome map parses and per-turn deltas sum into
// trajectory totals — the counters reset with each `crush run`
// process, so a repair turn's replan shows up in the same turn's
// file and the next turn's rows add on top.
func TestRunTelemetry_EdgeFirings(t *testing.T) {
	t.Parallel()
	writeTel := func(firings map[string]map[string]int) runTelemetry {
		doc := map[string]any{
			"session_id":   "s1",
			"steps":        3,
			"edge_firings": firings,
		}
		path := filepath.Join(t.TempDir(), "tel.json")
		data, err := json.Marshal(doc)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(path, data, 0o644))
		tel, err := readTelemetry(path)
		require.NoError(t, err)
		return tel
	}

	var res RunResult
	res.addTurnTelemetry(writeTel(map[string]map[string]int{
		"stall":        {"fired": 1},
		"todos":        {"fired": 1},
		"verification": {"cleared": 1},
	}), 0)
	res.addTurnTelemetry(writeTel(nil), 1)
	res.addTurnTelemetry(writeTel(map[string]map[string]int{
		"stall":      {"fired": 1, "exhausted": 1},
		"burn-watch": {"gated": 1},
	}), 2)

	require.Equal(t, map[string]map[string]int{
		"stall":        {"fired": 2, "exhausted": 1},
		"todos":        {"fired": 1},
		"verification": {"cleared": 1},
		"burn-watch":   {"gated": 1},
	}, res.EdgeFirings)
}

// TestRunTelemetry_RequestCurve pins the request-telemetry contract:
// the child's per-turn prompt_tokens_last appends to the growth
// curve (never sums — a snapshot, not a counter), peak takes the
// max, and the composition keeps the latest turn's bytes.
func TestRunTelemetry_RequestCurve(t *testing.T) {
	t.Parallel()
	writeTel := func(last, peak, toolResultBytes int64) runTelemetry {
		doc := map[string]any{
			"session_id": "s1",
			"steps":      2,
			"request": map[string]any{
				"prompt_requests":    2,
				"prompt_tokens_last": last,
				"prompt_tokens_peak": peak,
				"system_bytes":       1000,
				"notebook_bytes":     2000,
				"history_bytes":      3000,
				"tool_call_bytes":    400,
				"tool_result_bytes":  toolResultBytes,
			},
		}
		path := filepath.Join(t.TempDir(), "tel.json")
		data, err := json.Marshal(doc)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(path, data, 0o644))
		tel, err := readTelemetry(path)
		require.NoError(t, err)
		return tel
	}

	var res RunResult
	res.addTurnTelemetry(writeTel(12000, 12000, 8000), 0)
	res.addTurnTelemetry(writeTel(0, 0, 0), 1) // turn with no requests — no curve point
	res.addTurnTelemetry(writeTel(25000, 30000, 9000), 2)

	require.Equal(t, []int64{12000, 25000}, res.PromptTokensPerTurn)
	require.Equal(t, int64(6), res.Request.PromptRequests)
	require.Equal(t, int64(30000), res.Request.PromptTokensPeak)
	// Composition keeps the last turn's rendered request.
	require.Equal(t, int64(9000), res.Request.ToolResultBytes)
	require.Equal(t, int64(3000), res.Request.HistoryBytes)
}

// TestRunTelemetry_StepRecords pins the per-request table contract:
// request.steps rows fold across turns with the trajectory turn
// stamped on each, generator_tokens sums the sidecar spend, and a
// notebook-off child leaving the block empty contributes nothing.
func TestRunTelemetry_StepRecords(t *testing.T) {
	t.Parallel()
	writeTel := func(steps []map[string]any, gen map[string]any) runTelemetry {
		doc := map[string]any{
			"session_id": "s1",
			"steps":      len(steps),
			"request": map[string]any{
				"prompt_requests": len(steps),
				"steps":           steps,
			},
		}
		if gen != nil {
			doc["generator_tokens"] = gen
		}
		path := filepath.Join(t.TempDir(), "tel.json")
		data, err := json.Marshal(doc)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(path, data, 0o644))
		tel, err := readTelemetry(path)
		require.NoError(t, err)
		return tel
	}

	var res RunResult
	res.addTurnTelemetry(writeTel([]map[string]any{
		{"step": 0, "input_tokens": 100, "output_tokens": 10, "cache_read_tokens": 5, "cache_write_tokens": 3, "prefix_hash": "aa", "first_changed_index": 0, "first_changed_cause": "cold"},
		{"step": 1, "input_tokens": 120, "output_tokens": 8, "prefix_hash": "aa", "first_changed_index": 2, "first_changed_cause": "append"},
	}, map[string]any{"calls": 2, "input": 300, "output": 30, "cache_write": 12}), 0)
	res.addTurnTelemetry(writeTel([]map[string]any{
		{"step": 0, "input_tokens": 200, "output_tokens": 20, "cache_read_tokens": 150, "prefix_hash": "bb", "first_changed_index": 1, "first_changed_cause": "notebook-prefix"},
	}, nil), 1)

	require.Len(t, res.StepRecords, 3)
	require.Equal(t, 0, res.StepRecords[0].Turn)
	require.Equal(t, 0, res.StepRecords[0].Step)
	require.Equal(t, int64(100), res.StepRecords[0].InputTokens)
	require.Equal(t, int64(5), res.StepRecords[0].CacheReadTokens)
	require.Equal(t, "cold", res.StepRecords[0].FirstChangedCause)
	require.Equal(t, 1, res.StepRecords[2].Turn)
	require.Equal(t, "notebook-prefix", res.StepRecords[2].FirstChangedCause)
	require.Equal(t, "bb", res.StepRecords[2].PrefixHash)

	require.Equal(t, 2, res.GeneratorTokens.Calls)
	require.Equal(t, int64(300), res.GeneratorTokens.Input)
	require.Equal(t, int64(30), res.GeneratorTokens.Output)
	require.Equal(t, int64(12), res.GeneratorTokens.CacheWrite)
}

// TestStepRecord_WireContract pins the agent→eval wire shape: the
// child marshals agent.StepRecord into the telemetry doc and
// addTurnTelemetry decodes into eval.StepRecord. A renamed JSON tag
// on either side compiles fine and silently drops the field —
// round-tripping a fully populated row makes drift visible.
func TestStepRecord_WireContract(t *testing.T) {
	t.Parallel()
	src := agent.StepRecord{
		Step:              3,
		InputTokens:       11,
		OutputTokens:      22,
		CacheReadTokens:   33,
		CacheWriteTokens:  44,
		Estimated:         true,
		Failed:            true,
		PrefixHash:        "deadbeef",
		FirstChanged:      7,
		FirstChangedCause: "notebook-prefix",
	}
	b, err := json.Marshal(src)
	require.NoError(t, err)
	var dst StepRecord
	require.NoError(t, json.Unmarshal(b, &dst))
	require.Equal(t, src.Step, dst.Step)
	require.Equal(t, src.InputTokens, dst.InputTokens)
	require.Equal(t, src.OutputTokens, dst.OutputTokens)
	require.Equal(t, src.CacheReadTokens, dst.CacheReadTokens)
	require.Equal(t, src.CacheWriteTokens, dst.CacheWriteTokens)
	require.Equal(t, src.Estimated, dst.Estimated)
	require.Equal(t, src.Failed, dst.Failed)
	require.Equal(t, src.PrefixHash, dst.PrefixHash)
	require.Equal(t, src.FirstChanged, dst.FirstChanged)
	require.Equal(t, src.FirstChangedCause, dst.FirstChangedCause)
	// Turn is driver-assigned (addTurnTelemetry stamps it), not wire.
	require.Zero(t, dst.Turn)
}

// TestArmTokenStats pins the informational benefit metric: conclusive
// runs only, prompt-side = input + cache read + cache write, and the
// last-turn mean reads each run's curve tail.
func TestArmTokenStats(t *testing.T) {
	t.Parallel()
	recs := []RunRecord{
		{Arm: ArmControl, Outcome: OutcomePass, Tokens: TokenUsage{Input: 1000, CacheRead: 9000}, PromptTokensPerTurn: []int64{10, 40}},
		{Arm: ArmControl, Outcome: OutcomeInconclusive, Tokens: TokenUsage{Input: 99999}}, // excluded
		{Arm: ArmControl, Outcome: OutcomeFail, Tokens: TokenUsage{Input: 2000}, PromptTokensPerTurn: []int64{30}},
		{Arm: ArmTreatment, Outcome: OutcomePass, Tokens: TokenUsage{Input: 500, CacheRead: 5500, CacheWrite: 1000}, PromptTokensPerTurn: []int64{10, 20}},
	}
	stats := armTokenStats(recs, true)
	require.Equal(t, 2, stats[ArmControl].Runs)
	require.Equal(t, int64(12000), stats[ArmControl].PromptTotal)
	require.InDelta(t, 35, stats[ArmControl].LastTurnMean(), 1e-9) // (40+30)/2
	require.Equal(t, int64(7000), stats[ArmTreatment].PromptTotal)
	require.InDelta(t, 20, stats[ArmTreatment].LastTurnMean(), 1e-9)
	require.Nil(t, armTokenStats(nil, true))
	// The excluded stratum reports the runs resampling drops.
	excl := armTokenStats(recs, false)
	require.Equal(t, 1, excl[ArmControl].Runs)
	require.Equal(t, int64(99999), excl[ArmControl].PromptTotal)
}

// --- stats ---

func TestFisherExactCollapse_KnownValues(t *testing.T) {
	t.Parallel()
	// 0/3 vs 3/3 → one-sided p = 0.05.
	require.InDelta(t, 0.05, FisherExactCollapse(3, 3, 0, 3), 1e-9)
	// 0/3 vs 29/30 → ≈ 7.33e-4.
	require.InDelta(t, 0.000733, FisherExactCollapse(3, 3, 1, 30), 1e-5)
	// 0/3 vs 19/20 → ≈ 0.00226.
	require.InDelta(t, 0.002259, FisherExactCollapse(3, 3, 1, 20), 1e-5)
	// No failures in current arm → p = 1.
	require.InDelta(t, 1.0, FisherExactCollapse(0, 3, 5, 30), 1e-9)
}

func TestPermutationP_DetectsShift(t *testing.T) {
	t.Parallel()
	rng := rand.New(rand.NewPCG(1, 2))

	// Uniform degradation: treatment fails everywhere, control passes.
	var pairs []ArmPair
	for range 10 {
		pairs = append(pairs, ArmPair{
			Control:   []bool{true, true, true, true, true},
			Treatment: []bool{false, false, false, false, false},
		})
	}
	p := PermutationP(pairs, 2000, rng)
	require.Less(t, p, 0.001)

	// Null-ish: arms identical → p should be well above 0.05.
	var nullPairs []ArmPair
	for range 10 {
		nullPairs = append(nullPairs, ArmPair{
			Control:   []bool{true, false, true, true, false},
			Treatment: []bool{true, false, true, true, false},
		})
	}
	p = PermutationP(nullPairs, 2000, rng)
	require.Greater(t, p, 0.05)
}
