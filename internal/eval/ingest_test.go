package eval

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/charmbracelet/crush/internal/db"
)

// writeTrialDir fixtures one harbor trial dir: result.json carrying
// task id + verifier rewards (or an exception), plus the agent's
// copied-back crush-data/telemetry.json.
func writeTrialDir(t *testing.T, root, name, taskID string, reward float64, tel map[string]any) string {
	t.Helper()
	dir := filepath.Join(root, name)
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "agent", "crush-data"), 0o755))
	result := map[string]any{
		"task_id": map[string]any{"name": taskID},
	}
	if reward >= 0 {
		result["verifier_result"] = map[string]any{"rewards": map[string]any{"reward": reward}}
	}
	data, err := json.Marshal(result)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "result.json"), data, 0o644))
	if tel != nil {
		td, err := json.Marshal(tel)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(filepath.Join(dir, "agent", "crush-data", "telemetry.json"), td, 0o644))
	}
	return dir
}

func harborExp(taskIDs ...string) *Experiment {
	temp := 0.2
	return &Experiment{
		Name:        "harbor-exp",
		Model:       "llm/test-model",
		Temperature: &temp,
		Corpus:      []string{"harbor"},
		Arms: map[string]Arm{
			ArmControl: {Config: ArmConfig{}},
			// A tools block needs no flags.json entry and still
			// rotates the arm's BaselineKey — the #409 surface.
			ArmTreatment: {Config: ArmConfig{Tools: map[string]any{
				"grep": map[string]any{"timeout": 1},
			}}},
		},
		RunsPerTrajectory: map[Band]int{BandUncharacterized: 1},
		Harbor: &HarborSpec{
			Dataset:         "terminal-bench@2.0",
			TaskIDs:         taskIDs,
			K:               1,
			HarnessRevision: "abc1234",
		},
	}
}

func TestValidateExperiment_Harbor(t *testing.T) {
	valid := harborExp("t1", "t2")
	require.NoError(t, ValidateExperiment(valid))

	badCorpus := harborExp("t1")
	badCorpus.Corpus = []string{"*"}
	require.ErrorContains(t, ValidateExperiment(badCorpus), `corpus: ["harbor"]`)

	noSource := harborExp()
	noSource.Harbor.TaskIDs = nil
	require.ErrorContains(t, ValidateExperiment(noSource), "exactly one of")

	twoSources := harborExp("t1")
	twoSources.Harbor.TB21Split = "dev"
	require.ErrorContains(t, ValidateExperiment(twoSources), "exactly one of")

	badSplit := harborExp("t1")
	badSplit.Harbor.TaskIDs = nil
	badSplit.Harbor.TB21Split = "prod"
	require.ErrorContains(t, ValidateExperiment(badSplit), "tb21_split")

	zeroK := harborExp("t1")
	zeroK.Harbor.K = 0
	require.ErrorContains(t, ValidateExperiment(zeroK), "harbor.k")

	noRev := harborExp("t1")
	noRev.Harbor.HarnessRevision = ""
	require.ErrorContains(t, ValidateExperiment(noRev), "harness_revision")

	replay := harborExp("t1")
	replay.Replay = &ReplaySpec{}
	require.ErrorContains(t, ValidateExperiment(replay), "replay")

	persistent := harborExp("t1")
	persistent.ProcessModel = ProcessModelPersistent
	require.ErrorContains(t, ValidateExperiment(persistent), "process_model")
}

func TestIngestHarborTrials_RecordsAndSnapshot(t *testing.T) {
	dir := t.TempDir()
	exp := harborExp("task-a", "task-b", "task-c")
	require.NoError(t, ValidateExperiment(exp))
	r := seededRunner(dir)
	ctx := context.Background()

	jobs := t.TempDir()
	tel := func(in int64) map[string]any {
		return map[string]any{
			"session_id": "s", "steps": 4, "model": "llm/test-model",
			"tokens":           map[string]any{"input": in, "output": 40, "cache_read": 10, "cache_write": 5},
			"resolved_options": map[string]any{},
		}
	}
	cDirs := []string{
		writeTrialDir(t, jobs, "ctrl-a", "task-a", 1.0, map[string]any{
			"session_id": "s-ca", "steps": 4, "model": "llm/test-model",
			"tokens":           map[string]any{"input": 100, "output": 40, "cache_read": 10, "cache_write": 5},
			"resolved_options": map[string]any{},
		}),
		writeTrialDir(t, jobs, "ctrl-b", "task-b", 0.0, tel(120)),
		writeTrialDir(t, jobs, "ctrl-c", "task-c", 1.0, tel(90)),
	}
	rep, err := r.IngestHarborTrials(ctx, exp, ArmControl, "inv1", cDirs, nil)
	require.NoError(t, err)
	require.Equal(t, 3, rep.Records)

	tDirs := []string{
		writeTrialDir(t, jobs, "trt-a", "task-a", 0.0, tel(80)),
		writeTrialDir(t, jobs, "trt-b", "task-b", 1.0, tel(110)),
		writeTrialDir(t, jobs, "trt-c", "task-c", 1.0, tel(95)),
	}
	rep, err = r.IngestHarborTrials(ctx, exp, ArmTreatment, "inv1", tDirs, nil)
	require.NoError(t, err)
	require.Equal(t, 3, rep.Records)
	require.Empty(t, rep.Skipped)

	recs, err := r.LoadExperimentRecords(exp.Name)
	require.NoError(t, err)
	require.Len(t, recs, 6)
	var ca *RunRecord
	for i := range recs {
		if recs[i].Arm == ArmControl && recs[i].TrajectoryID == "task-a" {
			ca = &recs[i]
		}
	}
	require.NotNil(t, ca)
	require.Equal(t, OutcomePass, ca.Outcome)
	require.Equal(t, "inv1", ca.Invocation)
	require.Equal(t, 4, ca.Steps)
	require.Equal(t, int64(100), ca.Tokens.Input)
	require.Equal(t, "s-ca", ca.SessionID)
	require.Equal(t, "llm/test-model", ca.Env.ModelPin)
	require.Equal(t, "abc1234", ca.Env.CrushSHA)
	require.Equal(t, "linux", ca.Env.OS)
	// No crush.db fixture — copy-back absence is recorded, not silent.
	require.NotEmpty(t, ca.CallMetricsError)

	// The invocation's alarm snapshot pins the declared blocks.
	snapData, err := os.ReadFile(filepath.Join(dir, "results", exp.Name, "report-inv1.json"))
	require.NoError(t, err)
	var snap alarmSnapshot
	require.NoError(t, json.Unmarshal(snapData, &snap))
	require.Equal(t, "inv1", snap.Invocation)
	require.Empty(t, snap.Skipped)

	// Compare runs over ingested records — harbor experiments are
	// compare-clean end to end.
	rep2, err := r.Compare(exp, "inv1")
	require.NoError(t, err)
	require.NotNil(t, rep2)
}

func TestIngestHarborTrials_Outcomes(t *testing.T) {
	dir := t.TempDir()
	exp := harborExp("t-pass", "t-fail", "t-exc", "t-nover", "t-partial")
	r := seededRunner(dir)
	ctx := context.Background()

	jobs := t.TempDir()
	dirs := []string{
		writeTrialDir(t, jobs, "a", "t-pass", 1.0, nil),
		writeTrialDir(t, jobs, "b", "t-fail", 0.0, nil),
		writeTrialDir(t, jobs, "d", "t-nover", -1, nil),
		writeTrialDir(t, jobs, "e", "t-partial", 0.5, nil),
	}
	// Exception trial: no verifier_result, exception_info set.
	excDir := filepath.Join(jobs, "c")
	require.NoError(t, os.MkdirAll(excDir, 0o755))
	data, _ := json.Marshal(map[string]any{
		"task_id":        map[string]any{"path": "/tasks/t-exc"},
		"exception_info": map[string]any{"exception_type": "AgentTimeoutError", "exception_message": "timed out"},
	})
	require.NoError(t, os.WriteFile(filepath.Join(excDir, "result.json"), data, 0o644))
	dirs = append(dirs, excDir)

	_, err := r.IngestHarborTrials(ctx, exp, ArmControl, "inv1", dirs, nil)
	require.NoError(t, err)
	recs, err := r.LoadExperimentRecords(exp.Name)
	require.NoError(t, err)
	got := map[string]Outcome{}
	for _, rec := range recs {
		got[rec.TrajectoryID] = rec.Outcome
	}
	require.Equal(t, OutcomePass, got["t-pass"])
	require.Equal(t, OutcomeFail, got["t-fail"])
	require.Equal(t, OutcomeError, got["t-exc"])
	require.Equal(t, OutcomeError, got["t-nover"])
	require.Equal(t, OutcomeFail, got["t-partial"]) // partial credit is not a pass
}

func TestIngestHarborTrials_DuplicateRefused(t *testing.T) {
	dir := t.TempDir()
	exp := harborExp("task-a")
	r := seededRunner(dir)
	jobs := t.TempDir()
	d := writeTrialDir(t, jobs, "t", "task-a", 1.0, nil)
	_, err := r.IngestHarborTrials(context.Background(), exp, ArmControl, "inv1", []string{d}, nil)
	require.NoError(t, err)
	_, err = r.IngestHarborTrials(context.Background(), exp, ArmControl, "inv1", []string{d}, nil)
	require.ErrorContains(t, err, "already recorded")
}

func TestIngestHarborTrials_UndeclaredTask(t *testing.T) {
	dir := t.TempDir()
	exp := harborExp("task-a")
	r := seededRunner(dir)
	jobs := t.TempDir()
	d := writeTrialDir(t, jobs, "x", "task-rogue", 1.0, nil)
	_, err := r.IngestHarborTrials(context.Background(), exp, ArmControl, "inv1", []string{d}, nil)
	require.ErrorContains(t, err, "not in the experiment's declared task set")
}

func TestIngestHarborTrials_SkippedPartialCorpus(t *testing.T) {
	dir := t.TempDir()
	exp := harborExp("task-a", "task-b", "task-c")
	r := seededRunner(dir)
	jobs := t.TempDir()
	dirs := []string{
		writeTrialDir(t, jobs, "a", "task-a", 1.0, nil),
		writeTrialDir(t, jobs, "b", "task-b", 1.0, nil),
	}
	rep, err := r.IngestHarborTrials(context.Background(), exp, ArmControl, "inv1", dirs, nil)
	require.NoError(t, err)

	// task-c never ran — the snapshot's skipped set marks the
	// corpus uncovered, and compare refuses the partial invocation.
	require.Equal(t, 2, rep.Records)
	require.Equal(t, []string{"task-c"}, rep.Skipped)
	_, err = r.Compare(exp, "inv1")
	require.ErrorContains(t, err, "skipped")
}

func TestIngestHarborTrials_IncrementalExtendsK(t *testing.T) {
	dir := t.TempDir()
	exp := harborExp("task-a")
	r := seededRunner(dir)
	ctx := context.Background()
	jobs := t.TempDir()

	first := writeTrialDir(t, jobs, "a1", "task-a", 1.0, nil)
	_, err := r.IngestHarborTrials(ctx, exp, ArmControl, "inv1", []string{first}, nil)
	require.NoError(t, err)

	// A second trial for the same task continues the arm's
	// ordinal sequence instead of colliding on run_index 0 —
	// incremental ingest extends k.
	second := writeTrialDir(t, jobs, "a2", "task-a", 0.0, nil)
	rep, err := r.IngestHarborTrials(ctx, exp, ArmControl, "inv1", []string{second}, nil)
	require.NoError(t, err)
	require.Equal(t, 1, rep.Records)

	recs, err := r.LoadExperimentRecords(exp.Name)
	require.NoError(t, err)
	idx := map[int]bool{}
	for _, rec := range recs {
		require.Equal(t, ArmControl, rec.Arm)
		idx[rec.RunIndex] = true
	}
	require.Equal(t, map[int]bool{0: true, 1: true}, idx)

	// The other arm's ordinals are unaffected — pairing keys on
	// (task, run_index) across arms, so treatment starts at 0.
	trt := writeTrialDir(t, jobs, "t1", "task-a", 1.0, nil)
	_, err = r.IngestHarborTrials(ctx, exp, ArmTreatment, "inv1", []string{trt}, nil)
	require.NoError(t, err)
	recs, err = r.LoadExperimentRecords(exp.Name)
	require.NoError(t, err)
	for _, rec := range recs {
		if rec.Arm == ArmTreatment {
			require.Equal(t, 0, rec.RunIndex)
		}
	}
}

func TestIngestHarborTrials_RunIndexOrdinals(t *testing.T) {
	dir := t.TempDir()
	exp := harborExp("task-a", "task-b")
	r := seededRunner(dir)
	jobs := t.TempDir()
	dirs := []string{
		writeTrialDir(t, jobs, "a1", "task-a", 1.0, nil),
		writeTrialDir(t, jobs, "a2", "task-a", 0.0, nil),
		writeTrialDir(t, jobs, "b1", "task-b", 1.0, nil),
	}
	_, err := r.IngestHarborTrials(context.Background(), exp, ArmControl, "inv1", dirs, nil)
	require.NoError(t, err)
	recs, err := r.LoadExperimentRecords(exp.Name)
	require.NoError(t, err)
	idx := map[string][]int{}
	for _, rec := range recs {
		idx[rec.TrajectoryID] = append(idx[rec.TrajectoryID], rec.RunIndex)
	}
	require.ElementsMatch(t, []int{0, 1}, idx["task-a"])
	require.Equal(t, []int{0}, idx["task-b"])
}

func TestIngestHarborTrials_PendingAndExceptionOnly(t *testing.T) {
	dir := t.TempDir()
	exp := harborExp("task-a", "task-b", "task-c")
	r := seededRunner(dir)
	jobs := t.TempDir()

	done := writeTrialDir(t, jobs, "a1", "task-a", 1.0, nil)

	// Still in flight: config.json but no terminal artifact —
	// skipped into pending, not an error.
	liveDir := filepath.Join(jobs, "task-b__live")
	require.NoError(t, os.MkdirAll(liveDir, 0o755))
	liveCfg, _ := json.Marshal(map[string]any{
		"task":       map[string]any{"path": "task-b"},
		"trial_name": "task-b__live",
	})
	require.NoError(t, os.WriteFile(filepath.Join(liveDir, "config.json"), liveCfg, 0o644))

	// Raised before result.json: exception.txt only — error record,
	// task id recovered from config.json's task.path.
	excDir := filepath.Join(jobs, "task-c__dead")
	require.NoError(t, os.MkdirAll(excDir, 0o755))
	excCfg, _ := json.Marshal(map[string]any{
		"task":       map[string]any{"path": "task-c"},
		"trial_name": "task-c__dead",
	})
	require.NoError(t, os.WriteFile(filepath.Join(excDir, "config.json"), excCfg, 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(excDir, "exception.txt"),
		[]byte("Traceback (most recent call last):\nharbor.trial.errors.EnvironmentStartTimeoutError: Environment start timed out after 2400.0 seconds\n"), 0o644))

	rep, err := r.IngestHarborTrials(context.Background(), exp, ArmControl, "inv1",
		[]string{done, liveDir, excDir}, nil)
	require.NoError(t, err)
	require.Equal(t, 2, rep.Records)
	require.Equal(t, []string{"task-b"}, rep.Pending)

	recs, err := r.LoadExperimentRecords(exp.Name)
	require.NoError(t, err)
	got := map[string]Outcome{}
	var excRec *RunRecord
	for i := range recs {
		got[recs[i].TrajectoryID] = recs[i].Outcome
		if recs[i].TrajectoryID == "task-c" {
			excRec = &recs[i]
		}
	}
	require.Equal(t, OutcomePass, got["task-a"])
	require.Equal(t, OutcomeError, got["task-c"])
	require.NotNil(t, excRec)
	require.Equal(t, "harbor.trial.errors.EnvironmentStartTimeoutError", excRec.CheckDetail["exception_type"])
	require.Equal(t, "exception.txt", excRec.CheckDetail["exception_source"])

	// Re-ingesting after the live trial lands its result fills the
	// cell rather than erroring the whole job on one straggler.
	liveResult, _ := json.Marshal(map[string]any{
		"task_id":         map[string]any{"name": "task-b"},
		"verifier_result": map[string]any{"rewards": map[string]any{"reward": 0.0}},
	})
	require.NoError(t, os.WriteFile(filepath.Join(liveDir, "result.json"), liveResult, 0o644))
	rep, err = r.IngestHarborTrials(context.Background(), exp, ArmControl, "inv1", []string{liveDir}, nil)
	require.NoError(t, err)
	require.Equal(t, 1, rep.Records)
	require.Empty(t, rep.Pending)
	require.Empty(t, rep.Skipped)
}

func TestIngestHarborTrials_RefusesNonHarbor(t *testing.T) {
	dir := t.TempDir()
	exp := harborExp("task-a")
	exp.Harbor = nil
	r := seededRunner(dir)
	_, err := r.IngestHarborTrials(context.Background(), exp, ArmControl, "inv1", []string{t.TempDir()}, nil)
	require.ErrorContains(t, err, "no harbor block")
}

func TestRunExperiment_RefusesHarbor(t *testing.T) {
	dir := t.TempDir()
	exp := harborExp("task-a")
	r := seededRunner(dir)
	_, err := r.RunExperiment(context.Background(), exp)
	require.ErrorContains(t, err, "records arrive via `crush eval ingest`")
}

// harbor.exclude strikes declared tasks from the coverage
// obligation — their absence stops voiding the invocation while
// the reduced corpus stays visible in the snapshot, and their
// trials still ingest as ordinary records.
func TestIngestHarborTrials_ExcludedTask(t *testing.T) {
	dir := t.TempDir()
	exp := harborExp("task-a", "task-b")
	exp.Harbor.Exclude = map[string]string{"task-b": "image fails env build at 4x timeout — dataset defect"}
	require.NoError(t, ValidateExperiment(exp))
	r := seededRunner(dir)
	ctx := context.Background()

	jobs := t.TempDir()
	dirs := []string{writeTrialDir(t, jobs, "ctrl-a", "task-a", 1.0, map[string]any{
		"session_id": "s", "steps": 2, "model": "llm/test-model",
		"tokens": map[string]any{"input": 100}, "resolved_options": map[string]any{},
	})}
	rep, err := r.IngestHarborTrials(ctx, exp, ArmControl, "inv1", dirs, nil)
	require.NoError(t, err)
	require.Empty(t, rep.Skipped)
	require.Equal(t, []string{"task-b"}, rep.Excluded)

	data, err := os.ReadFile(filepath.Join(dir, "results", exp.Name, "report-inv1.json"))
	require.NoError(t, err)
	var snap alarmSnapshot
	require.NoError(t, json.Unmarshal(data, &snap))
	require.Empty(t, snap.Skipped)
	require.Equal(t, []string{"task-b"}, snap.ExcludedTasks)

	// The excluded task's trials still ingest — they ran; the
	// record is the exclusion's own evidence.
	dirs = []string{writeTrialDir(t, jobs, "ctrl-b", "task-b", 0.0, map[string]any{
		"session_id": "s2", "steps": 1, "model": "llm/test-model",
		"tokens": map[string]any{"input": 10}, "resolved_options": map[string]any{},
	})}
	rep, err = r.IngestHarborTrials(ctx, exp, ArmControl, "inv1", dirs, nil)
	require.NoError(t, err)
	require.Equal(t, 1, rep.Records)

	// An exclusion naming an undeclared task refuses at ingest —
	// the declared set only resolves here.
	bad := harborExp("task-a")
	bad.Harbor.Exclude = map[string]string{"nope": "typo"}
	_, err = r.IngestHarborTrials(ctx, bad, ArmControl, "inv1", dirs, nil)
	require.ErrorContains(t, err, "undeclared task")
}

func TestValidateExperiment_HarborExcludeReason(t *testing.T) {
	exp := harborExp("t1")
	exp.Harbor.Exclude = map[string]string{"t1": ""}
	require.ErrorContains(t, ValidateExperiment(exp), "reason is required")

	exp.Harbor.Exclude = map[string]string{"t1": "   "}
	require.ErrorContains(t, ValidateExperiment(exp), "reason is required")

	exp.Harbor.Exclude = map[string]string{"t1": "image fails env build"}
	require.NoError(t, ValidateExperiment(exp))
}

// fixtureRates builds the pinned rate block the cost tests share —
// USD per 1M tokens: input, output, cache_read, cache_write.
func fixtureRates(input, output, cacheRead, cacheWrite float64) *TokenRates {
	return &TokenRates{Input: &input, Output: &output, CacheRead: &cacheRead, CacheWrite: &cacheWrite}
}

// setTrialID rewrites a fixture's result.json to carry harbor's
// per-trial uuid — the dir-independent dedup key (#504).
func setTrialID(t *testing.T, dir, id string) {
	t.Helper()
	p := filepath.Join(dir, "result.json")
	var m map[string]any
	data, err := os.ReadFile(p)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(data, &m))
	m["id"] = id
	out, err := json.Marshal(m)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(p, out, 0o644))
}

// Harbor never reports a per-trial cost — ingest must price the
// billed classes at the manifest's pinned route rates so a paid
// pin's spend is measured (#505). The noise block's cost fields
// populate off those records — cost_trials > 0 is the proof.
func TestIngestHarborTrials_PinnedRateCost(t *testing.T) {
	dir := t.TempDir()
	exp := harborExp("task-a", "task-b")
	exp.CostRates = fixtureRates(0.15, 0.60, 0.015, 0.30)
	r := seededRunner(dir)
	ctx := context.Background()
	jobs := t.TempDir()
	tel := map[string]any{
		"session_id": "s1", "model": "llm/test-model",
		"resolved_options": map[string]any{},
		"tokens": map[string]any{
			"input": 1000000, "output": 100000,
			"cache_read": 10000000, "cache_write": 100000,
		},
	}
	// 1M·0.15 + 0.1M·0.60 + 10M·0.015 + 0.1M·0.30 = $0.39.
	d1 := writeTrialDir(t, jobs, "a", "task-a", 1.0, tel)
	d2 := writeTrialDir(t, jobs, "b", "task-b", 1.0, tel)
	rep, err := r.IngestHarborTrials(ctx, exp, ArmControl, "inv1", []string{d1, d2}, nil)
	require.NoError(t, err)
	require.Equal(t, 2, rep.Records)

	recs, err := r.LoadExperimentRecords(exp.Name)
	require.NoError(t, err)
	for _, rec := range recs {
		require.NotNil(t, rec.CostUSD)
		require.InDelta(t, 0.39, *rec.CostUSD, 1e-9)
		require.Equal(t, "cost_rates", rec.CheckDetail["cost_basis"])
	}
	cells, err := ComputeCellNoise(recs, exp, "", ArmControl, "tb21", []string{"inv1"})
	require.NoError(t, err)
	require.Equal(t, 2, cells.CostTrials)
	require.NotNil(t, cells.CostUSDPerTrial)
	require.InDelta(t, 0.39, *cells.CostUSDPerTrial, 1e-9)

	// A harness-reported cost wins over the pinned computation —
	// the rates are a fallback, not an override.
	exp2 := harborExp("task-c")
	exp2.CostRates = fixtureRates(0.15, 0.60, 0.015, 0.30)
	d3 := writeTrialDir(t, jobs, "c", "task-c", 1.0, tel)
	p := filepath.Join(d3, "result.json")
	var m map[string]any
	data, err := os.ReadFile(p)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(data, &m))
	m["agent_result"] = map[string]any{"cost_usd": 9.99}
	out, err := json.Marshal(m)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(p, out, 0o644))
	r2 := seededRunner(t.TempDir())
	_, err = r2.IngestHarborTrials(ctx, exp2, ArmControl, "inv1", []string{d3}, nil)
	require.NoError(t, err)
	recs2, err := r2.LoadExperimentRecords(exp2.Name)
	require.NoError(t, err)
	require.Len(t, recs2, 1)
	require.InDelta(t, 9.99, *recs2[0].CostUSD, 1e-9)
	require.Nil(t, recs2[0].CheckDetail["cost_basis"])
}

// The declared-rate block refuses partial or negative pins — a
// missing rate would price its class free by default (#505).
func TestValidateExperiment_CostRates(t *testing.T) {
	valid := harborExp("t1")
	valid.CostRates = fixtureRates(0.15, 0.6, 0.015, 0.3)
	require.NoError(t, ValidateExperiment(valid))

	free := harborExp("t1")
	free.CostRates = fixtureRates(0, 0, 0, 0)
	require.NoError(t, ValidateExperiment(free))

	neg := harborExp("t1")
	neg.CostRates = fixtureRates(-0.15, 0.6, 0.015, 0.3)
	require.ErrorContains(t, ValidateExperiment(neg), "non-negative")

	missing := harborExp("t1")
	missing.CostRates = &TokenRates{}
	require.ErrorContains(t, ValidateExperiment(missing), "every class")

	partial := harborExp("t1")
	partial.CostRates = &TokenRates{Input: new(float64)}
	require.ErrorContains(t, ValidateExperiment(partial), "every class")
}

// The session's own catwalk-priced cost lands in check_detail as the
// independent cross-check — read from the preserved artifact's
// sessions.cost, never the primary value (#505).
func TestIngestHarborTrials_DBCostCrossCheck(t *testing.T) {
	dir := t.TempDir()
	exp := harborExp("task-a")
	r := seededRunner(dir)
	ctx := context.Background()
	jobs := t.TempDir()
	tel := map[string]any{
		"session_id": "s1", "model": "llm/test-model",
		"resolved_options": map[string]any{},
	}
	d := writeTrialDir(t, jobs, "a", "task-a", 1.0, tel)
	dataDir := filepath.Join(d, "agent", "crush-data")
	conn, err := db.Connect(ctx, dataDir)
	require.NoError(t, err)
	insertSession(t, conn, "s1", "")
	_, err = conn.ExecContext(ctx, `UPDATE sessions SET cost = 0.42 WHERE id = 's1'`)
	require.NoError(t, err)
	require.NoError(t, db.Release(dataDir))

	_, err = r.IngestHarborTrials(ctx, exp, ArmControl, "inv1", []string{d}, nil)
	require.NoError(t, err)
	recs, err := r.LoadExperimentRecords(exp.Name)
	require.NoError(t, err)
	require.Len(t, recs, 1)
	require.InDelta(t, 0.42, recs[0].CheckDetail["db_cost_usd"], 1e-9)
}

// One trial's data must land in exactly one cell of the experiment —
// a second arm, a second invocation, or a differently-spelled path to
// the same dir are all refusals, or a duplicated job produces a fake
// perfect A/A (the pilot-b2 defect).
func TestIngestHarborTrials_CrossArmAndInvocationDedup(t *testing.T) {
	dir := t.TempDir()
	exp := harborExp("task-a")
	r := seededRunner(dir)
	ctx := context.Background()
	jobs := t.TempDir()
	d := writeTrialDir(t, jobs, "t", "task-a", 1.0, nil)

	_, err := r.IngestHarborTrials(ctx, exp, ArmControl, "inv1", []string{d}, nil)
	require.NoError(t, err)

	_, err = r.IngestHarborTrials(ctx, exp, ArmTreatment, "inv1", []string{d}, nil)
	require.ErrorContains(t, err, "can't fill two cells")

	_, err = r.IngestHarborTrials(ctx, exp, ArmControl, "inv2", []string{d}, nil)
	require.ErrorContains(t, err, "can't fill two cells")

	_, err = r.IngestHarborTrials(ctx, exp, ArmControl, "inv2",
		[]string{d + string(filepath.Separator) + "."}, nil)
	require.ErrorContains(t, err, "can't fill two cells")

	recs, err := r.LoadExperimentRecords(exp.Name)
	require.NoError(t, err)
	require.Len(t, recs, 1)
}

// The dir-path key can't see a job re-ingested under a differently-
// spelled dir — harbor's trial id in result.json is the second key.
func TestIngestHarborTrials_TrialIDDedup(t *testing.T) {
	dir := t.TempDir()
	exp := harborExp("task-a")
	r := seededRunner(dir)
	ctx := context.Background()
	jobs := t.TempDir()

	d1 := writeTrialDir(t, jobs, "a", "task-a", 1.0, nil)
	setTrialID(t, d1, "uuid-1")
	_, err := r.IngestHarborTrials(ctx, exp, ArmControl, "inv1", []string{d1}, nil)
	require.NoError(t, err)

	d2 := writeTrialDir(t, jobs, "b", "task-a", 1.0, nil)
	setTrialID(t, d2, "uuid-1")
	_, err = r.IngestHarborTrials(ctx, exp, ArmControl, "inv1", []string{d2}, nil)
	require.ErrorContains(t, err, "trial id uuid-1")

	recs, err := r.LoadExperimentRecords(exp.Name)
	require.NoError(t, err)
	require.Len(t, recs, 1)
	require.Equal(t, "uuid-1", recs[0].CheckDetail["trial_id"])
}

// The pin gate: what ran must be what the manifest declared —
// resolved large/small models equal their pins when non-empty, and
// empty telemetry fields (a trial that died early) record rather
// than refuse: they're already inconclusive.
func TestIngestHarborTrials_PinGate(t *testing.T) {
	dir := t.TempDir()
	exp := harborExp("task-a", "task-b", "task-c")
	exp.SmallModel = "llm/small-pin"
	r := seededRunner(dir)
	ctx := context.Background()
	jobs := t.TempDir()
	tel := func(model, small string) map[string]any {
		return map[string]any{
			"session_id": "s", "model": model, "model_small": small,
			"resolved_options": map[string]any{},
		}
	}

	d := writeTrialDir(t, jobs, "a", "task-a", 1.0, tel("llm/other-model", "llm/small-pin"))
	_, err := r.IngestHarborTrials(ctx, exp, ArmControl, "inv1", []string{d}, nil)
	require.ErrorContains(t, err, `resolved model "llm/other-model" ≠ manifest pin`)

	d = writeTrialDir(t, jobs, "b", "task-b", 1.0, tel("llm/test-model", "llm/other-small"))
	_, err = r.IngestHarborTrials(ctx, exp, ArmControl, "inv1", []string{d}, nil)
	require.ErrorContains(t, err, `resolved small model "llm/other-small" ≠ manifest small_model`)

	// Matching both pins records.
	d = writeTrialDir(t, jobs, "c", "task-c", 1.0, tel("llm/test-model", "llm/small-pin"))
	rep, err := r.IngestHarborTrials(ctx, exp, ArmControl, "inv1", []string{d}, nil)
	require.NoError(t, err)
	require.Equal(t, 1, rep.Records)

	// Empty telemetry model — the trial died before its first call —
	// is already inconclusive, not a pin violation.
	d2 := writeTrialDir(t, jobs, "c2", "task-c", 0.0, tel("", ""))
	rep, err = r.IngestHarborTrials(ctx, exp, ArmControl, "inv1", []string{d2}, nil)
	require.NoError(t, err)
	require.Equal(t, 1, rep.Records)
}

// The declared temperature is provenance, not a promise — the
// child's sampling export reports what actually went on the request,
// and a mismatch (or none sent) refuses the record. A child old
// enough to not emit sampling can't be checked and records.
func TestIngestHarborTrials_TemperatureProvenance(t *testing.T) {
	dir := t.TempDir()
	exp := harborExp("task-a", "task-b", "task-c", "task-d")
	r := seededRunner(dir)
	ctx := context.Background()
	jobs := t.TempDir()
	tel := func(large map[string]any) map[string]any {
		m := map[string]any{
			"session_id": "s", "model": "llm/test-model",
			"resolved_options": map[string]any{},
		}
		if large != nil {
			m["sampling"] = map[string]any{"large": large}
		}
		return m
	}

	// Declared 0.2, sent 0.2 — records.
	d := writeTrialDir(t, jobs, "a", "task-a", 1.0, tel(map[string]any{"temperature": 0.2}))
	rep, err := r.IngestHarborTrials(ctx, exp, ArmControl, "inv1", []string{d}, nil)
	require.NoError(t, err)
	require.Equal(t, 1, rep.Records)

	// Declared 0.2, sent 0.7 — refused.
	d = writeTrialDir(t, jobs, "b", "task-b", 1.0, tel(map[string]any{"temperature": 0.7}))
	_, err = r.IngestHarborTrials(ctx, exp, ArmControl, "inv1", []string{d}, nil)
	require.ErrorContains(t, err, "resolved temperature 0.7 ≠ manifest 0.2")

	// Declared 0.2, sent none — refused: the pilot's own defect.
	d = writeTrialDir(t, jobs, "c", "task-c", 1.0, tel(map[string]any{}))
	_, err = r.IngestHarborTrials(ctx, exp, ArmControl, "inv1", []string{d}, nil)
	require.ErrorContains(t, err, "the run sent none")

	// No sampling block at all — a child predating the export —
	// can't be verified and records.
	d = writeTrialDir(t, jobs, "d", "task-d", 1.0, tel(nil))
	rep, err = r.IngestHarborTrials(ctx, exp, ArmControl, "inv1", []string{d}, nil)
	require.NoError(t, err)
	require.Equal(t, 1, rep.Records)

	recs, err := r.LoadExperimentRecords(exp.Name)
	require.NoError(t, err)
	require.Len(t, recs, 2)
}

// A partially-pinned rates block must record, not panic — a
// manifest built outside LoadExperiment bypasses the pinned check
// (#516 review).
func TestIngestHarborTrials_PartialRatesNoPanic(t *testing.T) {
	in := 0.15
	dir := t.TempDir()
	exp := harborExp("task-a")
	exp.CostRates = &TokenRates{Input: &in}
	r := seededRunner(dir)
	ctx := context.Background()
	jobs := t.TempDir()
	tel := map[string]any{
		"session_id": "s1", "model": "llm/test-model",
		"resolved_options": map[string]any{},
		"tokens":           map[string]any{"input": 100, "output": 10},
	}
	d1 := writeTrialDir(t, jobs, "a", "task-a", 1.0, tel)
	_, err := r.IngestHarborTrials(ctx, exp, ArmControl, "inv1", []string{d1}, nil)
	require.NoError(t, err)
	recs, err := r.LoadExperimentRecords(exp.Name)
	require.NoError(t, err)
	require.Len(t, recs, 1)
	require.Nil(t, recs[0].CostUSD)
	require.Contains(t, recs[0].CheckDetail["cost_error"], "cost_rates")
}

// writeForeignTrialDir fixtures a non-Crush harbor trial dir (#510):
// config.json carries the job's agent block (name + model_name),
// result.json the verifier outcome and harbor's agent_result
// accounting — and there is no agent/crush-data by design.
func writeForeignTrialDir(t *testing.T, root, dirName, taskID, agentName, agentModel string, reward float64) string {
	t.Helper()
	dir := filepath.Join(root, dirName)
	require.NoError(t, os.MkdirAll(dir, 0o755))
	cfg := map[string]any{
		"task":  map[string]any{"path": "/tasks/" + taskID},
		"agent": map[string]any{"name": agentName, "model_name": agentModel},
	}
	data, err := json.Marshal(cfg)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "config.json"), data, 0o644))
	result := map[string]any{
		"task_id": map[string]any{"name": taskID},
		"agent_info": map[string]any{
			"name":       agentName,
			"model_info": agentModel,
		},
		"agent_execution": map[string]any{
			"started_at":  "2026-11-18T00:00:00Z",
			"finished_at": "2026-11-18T00:10:00Z",
		},
		"agent_result": map[string]any{
			"n_input_tokens":  50000,
			"n_cache_tokens":  700000,
			"n_output_tokens": 30000,
		},
	}
	if reward >= 0 {
		result["verifier_result"] = map[string]any{"rewards": map[string]any{"reward": reward}}
	}
	data, err = json.Marshal(result)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "result.json"), data, 0o644))
	return dir
}

// A foreign_agent arm ingests a non-Crush agent's harbor trials:
// outcome from the verifier reward, wall time from agent_execution,
// tokens from harbor's agent_result — crush-data's absence is the
// design, so no CallMetricsError lands. Identity is checked both
// ways: the trial must name the arm's declared agent, and its
// reported model the declared parity spelling.
func TestIngestHarborTrials_ForeignAgent(t *testing.T) {
	dir := t.TempDir()
	exp := harborExp("task-a", "task-b", "task-c", "task-d", "task-e")
	exp.Arms[ArmControl] = Arm{
		Config: ArmConfig{},
		ForeignAgent: &ForeignAgentSpec{
			Name:  "terminus-2",
			Model: "openai/deepseek-v4.1-flash",
		},
	}
	require.NoError(t, ValidateExperiment(exp))
	r := seededRunner(dir)
	ctx := context.Background()
	jobs := t.TempDir()

	// The clean case: terminus-2's trial under the foreign arm.
	d := writeForeignTrialDir(t, jobs, "a", "task-a", "terminus-2", "openai/deepseek-v4.1-flash", 1.0)
	rep, err := r.IngestHarborTrials(ctx, exp, ArmControl, "inv1", []string{d}, nil)
	require.NoError(t, err)
	require.Equal(t, 1, rep.Records)
	recs, err := r.LoadExperimentRecords(exp.Name)
	require.NoError(t, err)
	require.Len(t, recs, 1)
	rec := recs[0]
	require.Equal(t, OutcomePass, rec.Outcome)
	require.Equal(t, "terminus-2", rec.Env.Agent)
	require.Equal(t, "openai/deepseek-v4.1-flash", rec.Env.ModelResolved)
	require.Equal(t, int64(50000), rec.Tokens.Input)
	require.Equal(t, int64(30000), rec.Tokens.Output)
	require.Equal(t, int64(700000), rec.Tokens.CacheRead)
	require.Equal(t, 600.0, rec.DurationS)
	require.Empty(t, rec.CallMetricsError)
	// The declared equivalence rides in the record — pin spelling
	// vs harbor spelling isn't drift evidence on a foreign arm.
	require.Contains(t, rec.CheckDetail["model_parity"], "declared ≡")

	// A different agent's job under the foreign arm refuses.
	d = writeForeignTrialDir(t, jobs, "b", "task-b", "terminus-3", "openai/deepseek-v4.1-flash", 1.0)
	_, err = r.IngestHarborTrials(ctx, exp, ArmControl, "inv1", []string{d}, nil)
	require.ErrorContains(t, err, "a different agent's records can't fill")

	// A wrong-model job refuses — the parity check.
	d = writeForeignTrialDir(t, jobs, "c", "task-c", "terminus-2", "anthropic/claude-opus-4.5", 1.0)
	_, err = r.IngestHarborTrials(ctx, exp, ArmControl, "inv1", []string{d}, nil)
	require.ErrorContains(t, err, "≠ declared foreign model")

	// No model reported at all refuses — parity can't be proven.
	d = writeForeignTrialDir(t, jobs, "d", "task-d", "terminus-2", "", 1.0)
	_, err = r.IngestHarborTrials(ctx, exp, ArmControl, "inv1", []string{d}, nil)
	require.ErrorContains(t, err, "reported no model")

	// The same foreign trial under the crush arm refuses — a
	// foreign agent's records can't fill a crush arm's cells.
	d = writeForeignTrialDir(t, jobs, "e", "task-e", "terminus-2", "openai/deepseek-v4.1-flash", 1.0)
	_, err = r.IngestHarborTrials(ctx, exp, ArmTreatment, "inv1", []string{d}, nil)
	require.ErrorContains(t, err, "can't fill a crush arm's cells")

	// A merely crush-sounding name isn't the wrapper — the check
	// is exact, not a substring match.
	exp2 := harborExp("task-a")
	d = writeForeignTrialDir(t, jobs, "f", "task-a", "my-crush-agent", "openai/deepseek-v4.1-flash", 1.0)
	_, err = r.IngestHarborTrials(ctx, exp2, ArmControl, "inv1", []string{d}, nil)
	require.ErrorContains(t, err, "can't fill a crush arm's cells")
}

// foreign_agent is a harbor-only declaration requiring both name
// (the arm-agent identity check) and model (the parity spelling).
func TestValidateExperiment_ForeignAgent(t *testing.T) {
	noHarbor := harborExp("task-a")
	noHarbor.Harbor = nil
	noHarbor.Arms[ArmControl] = Arm{
		Config:       ArmConfig{},
		ForeignAgent: &ForeignAgentSpec{Name: "terminus-2", Model: "openai/x"},
	}
	require.ErrorContains(t, ValidateExperiment(noHarbor), "requires a harbor block")

	missing := harborExp("task-a")
	missing.Arms[ArmControl] = Arm{
		Config:       ArmConfig{},
		ForeignAgent: &ForeignAgentSpec{Name: "terminus-2"},
	}
	require.ErrorContains(t, ValidateExperiment(missing), "both name and model")

	combo := harborExp("task-a")
	combo.Arms[ArmControl] = Arm{
		Config:       ArmConfig{},
		ForeignAgent: &ForeignAgentSpec{Name: "terminus-2", Model: "openai/x"},
		BestOfK:      &BestOfKSpec{K: 4},
	}
	require.ErrorContains(t, ValidateExperiment(combo), "foreign_agent + best_of_k")
}
