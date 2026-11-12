package eval

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
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
			ArmControl:   {Config: ArmConfig{}},
			ArmTreatment: {Config: ArmConfig{Options: map[string]any{"x": 1}}},
		},
		RunsPerTrajectory: map[Band]int{BandUncharacterized: 1},
		Harbor: &HarborSpec{
			Dataset:         "terminal-bench@2.1",
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
