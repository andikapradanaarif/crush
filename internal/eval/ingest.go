package eval

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/crush/internal/db"
)

// Harbor trial ingestion (#388) — converts completed harbor trial
// dirs into RunRecords so compare, the gate, and session-DB analysis
// run unchanged over externally executed experiments. The runner
// never sees harbor experiments (RunExperiment refuses them); this
// path is their only entry into the record store.
//
// Trial dir layout (harbor trial_dir):
//
//	<trial>/result.json                     TrialResult — task_id,
//	                                        verifier_result.rewards,
//	                                        exception_info, timing
//	<trial>/agent/crush-data/crush.db       session DB copied back by
//	                                        the crush agent's
//	                                        populate_context_post_run
//	<trial>/agent/crush-data/telemetry.json CRUSH_EVAL_TELEMETRY blob —
//	                                        identical schema the
//	                                        in-house driver reads

// IngestReport summarizes one IngestHarborTrials call.
type IngestReport struct {
	Experiment string   `json:"experiment"`
	Invocation string   `json:"invocation"`
	Arm        string   `json:"arm"`
	Records    int      `json:"records"`
	Tasks      []string `json:"tasks"`
	Skipped    []string `json:"skipped"`
}

// harborTrialResult is the subset of harbor's TrialResult the
// ingester reads. task_id arrives as one of harbor's task-id object
// shapes (LocalTaskId{path}, GitTaskId{...,path},
// PackageTaskId{name,...}) — harborTaskID normalizes all three to
// the benchmark task name.
type harborTrialResult struct {
	TaskID         json.RawMessage `json:"task_id"`
	VerifierResult *struct {
		Rewards map[string]float64 `json:"rewards"`
	} `json:"verifier_result"`
	ExceptionInfo *struct {
		Type    string `json:"exception_type"`
		Message string `json:"exception_message"`
	} `json:"exception_info"`
	AgentExecution *struct {
		StartedAt  time.Time `json:"started_at"`
		FinishedAt time.Time `json:"finished_at"`
	} `json:"agent_execution"`
	AgentResult *struct {
		InputTokens  *int64   `json:"n_input_tokens"`
		CacheTokens  *int64   `json:"n_cache_tokens"`
		OutputTokens *int64   `json:"n_output_tokens"`
		CostUSD      *float64 `json:"cost_usd"`
	} `json:"agent_result"`
}

// harborTaskID extracts the task name from any harbor task-id shape:
// PackageTaskId carries a literal name; the path-bearing ids name
// the task after their path's basename.
func harborTaskID(raw json.RawMessage) (string, error) {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		if s != "" {
			return s, nil
		}
		return "", fmt.Errorf("task_id string is empty")
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return "", fmt.Errorf("task_id is neither string nor object")
	}
	if name, ok := obj["name"]; ok {
		if err := json.Unmarshal(name, &s); err == nil && s != "" {
			return s, nil
		}
	}
	if p, ok := obj["path"]; ok {
		if err := json.Unmarshal(p, &s); err == nil && s != "" {
			return filepath.Base(filepath.Clean(strings.ReplaceAll(s, "\\", "/"))), nil
		}
	}
	return "", fmt.Errorf("task_id object carries no name or path")
}

// harborOutcome maps a trial's verifier reward to an Outcome.
// Benchmarks in scope (Terminal-Bench 2.x, SWE-bench) score binary —
// a full-reward task passes, anything less fails; partial credit is
// not a pass for the pass-rate metric. A trial that raised before
// the verifier ran (exception_info) or never produced a reward is
// error — infra derailment, not a task verdict.
func harborOutcome(res *harborTrialResult) (Outcome, map[string]any) {
	detail := map[string]any{}
	if res.ExceptionInfo != nil {
		detail["exception_type"] = res.ExceptionInfo.Type
		detail["exception_message"] = res.ExceptionInfo.Message
		return OutcomeError, detail
	}
	if res.VerifierResult == nil || len(res.VerifierResult.Rewards) == 0 {
		detail["missing"] = "verifier_result"
		return OutcomeError, detail
	}
	reward, ok := res.VerifierResult.Rewards["reward"]
	if !ok {
		switch len(res.VerifierResult.Rewards) {
		case 1:
			for _, v := range res.VerifierResult.Rewards {
				reward = v
			}
		default:
			detail["rewards"] = res.VerifierResult.Rewards
			return OutcomeError, detail
		}
	}
	detail["reward"] = reward
	// Floating-point near-miss reads as fail — binary verifiers
	// emit exact 0/1, so a fractional value is genuine partial
	// credit, and partial credit is not a pass.
	if reward >= 1.0 {
		return OutcomePass, detail
	}
	if reward >= 0 {
		return OutcomeFail, detail
	}
	return OutcomeError, detail
}

// HarborTaskSet resolves the experiment's declared task list — the
// corpus-coverage ground truth the alarm snapshot's skipped set
// subtracts recorded ids from.
func HarborTaskSet(evalDir string, spec *HarborSpec) ([]string, error) {
	switch {
	case spec.TB21Split != "":
		m, err := LoadTB21Manifest(evalDir, spec.TB21Split)
		if err != nil {
			return nil, err
		}
		return m.Tasks, nil
	case len(spec.TaskIDs) > 0:
		return slices.Clone(spec.TaskIDs), nil
	case spec.Sample != nil:
		data, err := os.ReadFile(filepath.Join(evalDir, spec.Sample.TasksFile))
		if err != nil {
			return nil, fmt.Errorf("read harbor sample tasks file: %w", err)
		}
		var ids []string
		for line := range strings.Lines(string(data)) {
			if id := strings.TrimSpace(line); id != "" {
				ids = append(ids, id)
			}
		}
		if len(ids) == 0 {
			return nil, fmt.Errorf("harbor sample tasks file %s: no task ids", spec.Sample.TasksFile)
		}
		// Same seeded-draw discipline as the tb21 split: sort,
		// shuffle under the PCG stream, cut at n.
		draw, _ := TB21SplitTasks(ids, spec.Sample.Seed, min(spec.Sample.N, len(ids)))
		return draw, nil
	}
	return nil, fmt.Errorf("harbor spec declares no task set")
}

// IngestHarborTrials converts each harbor trial dir in trialDirs
// into one RunRecord under the experiment's results and rewrites
// the invocation's alarm snapshot. arm and invocation are explicit
// inputs — the trial artifacts carry no notion of which arm's config
// ran or which paired invocation they belong to, and guessing either
// would corrupt the pairing the records exist to serve.
//
// run_index derivation: trials are grouped by task and ordered by
// dir name, so the i-th sorted trial of a task in each arm's job
// gets index i — the same ordinal pairing the runner's interleaved
// scheduling produces. runIndexOverride applies only to single-trial
// calls (per-trial ingestion with explicit indexing).
func (r *Runner) IngestHarborTrials(ctx context.Context, exp *Experiment, arm, invocation string, trialDirs []string, runIndexOverride *int) (*IngestReport, error) {
	if exp.Harbor == nil {
		return nil, fmt.Errorf("ingest: experiment %q declares no harbor block — only externally executed experiments ingest", exp.Name)
	}
	if _, ok := exp.Arms[arm]; !ok {
		return nil, fmt.Errorf("ingest: arm %q is not declared (%v) — an undeclared arm's options can't be audited", arm, sortedKeys(exp.Arms))
	}
	if invocation == "" {
		return nil, fmt.Errorf("ingest: --invocation is required — records pair within an invocation")
	}
	if len(trialDirs) == 0 {
		return nil, fmt.Errorf("ingest: no trial dirs")
	}
	if runIndexOverride != nil && len(trialDirs) != 1 {
		return nil, fmt.Errorf("ingest: --run-index applies to a single trial dir, got %d", len(trialDirs))
	}
	declared, err := HarborTaskSet(r.EvalDir, exp.Harbor)
	if err != nil {
		return nil, err
	}
	declaredSet := map[string]bool{}
	for _, id := range declared {
		declaredSet[id] = true
	}
	manifest, err := LoadFlagsManifest(r.EvalDir)
	if err != nil {
		return nil, err
	}
	existing, err := r.LoadExperimentRecords(exp.Name)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	dirSeen := map[string]bool{}
	var invRecords []RunRecord
	nextIndex := map[string]int{}
	for _, rec := range existing {
		if rec.Invocation != invocation {
			continue
		}
		invRecords = append(invRecords, rec)
		seen[rec.TrajectoryID+"\x00"+rec.Arm+"\x00"+strconv.Itoa(rec.RunIndex)] = true
		if d, _ := rec.CheckDetail["trial_dir"].(string); d != "" {
			dirSeen[rec.TrajectoryID+"\x00"+rec.Arm+"\x00"+d] = true
		}
		// Ordinals continue past this arm's recorded cells so an
		// incremental ingest extends k instead of colliding on
		// index 0. Other arms' cells don't consume ordinals —
		// pairing keys on (task, run_index) across arms.
		if rec.Arm == arm && rec.RunIndex+1 > nextIndex[rec.TrajectoryID] {
			nextIndex[rec.TrajectoryID] = rec.RunIndex + 1
		}
	}
	// Group trials by task so same-task dirs take consecutive
	// run_index ordinals; dirs sort lexically, so attempt ordering
	// matches harbor's own trial naming. Task ids come from
	// result.json — the dir name is harbor-version-sensitive.
	dirs := slices.Clone(trialDirs)
	sort.Strings(dirs)
	taskOf := make([]string, len(dirs))
	for i, dir := range dirs {
		id, err := peekTrialTask(dir)
		if err != nil {
			return nil, fmt.Errorf("ingest %s: %w", dir, err)
		}
		taskOf[i] = id
	}
	tempKey := temperatureKey(exp.Temperature)
	rep := &IngestReport{Experiment: exp.Name, Invocation: invocation, Arm: arm}
	for i, dir := range dirs {
		// The index keys the artifact name — assign before ingest
		// so k>1 trials don't overwrite each other's preserved DB.
		runIndex := nextIndex[taskOf[i]]
		nextIndex[taskOf[i]]++
		if runIndexOverride != nil {
			runIndex = *runIndexOverride
		}
		rec, err := r.ingestTrial(ctx, exp, arm, invocation, dir, runIndex, tempKey, manifest)
		if err != nil {
			return rep, fmt.Errorf("ingest %s: %w", dir, err)
		}
		rec.RunIndex = runIndex
		key := rec.TrajectoryID + "\x00" + rec.Arm + "\x00" + strconv.Itoa(rec.RunIndex)
		if seen[key] {
			return rep, fmt.Errorf("ingest %s: %s arm %s run %d already recorded in invocation %s — refuse to duplicate a paired cell",
				dir, rec.TrajectoryID, rec.Arm, rec.RunIndex, invocation)
		}
		dirKey := rec.TrajectoryID + "\x00" + rec.Arm + "\x00" + dir
		if dirSeen[dirKey] {
			return rep, fmt.Errorf("ingest %s: this trial is already recorded for %s arm %s in invocation %s — the same run's data can't fill two paired cells",
				dir, rec.TrajectoryID, rec.Arm, invocation)
		}
		if !declaredSet[rec.TrajectoryID] {
			return rep, fmt.Errorf("ingest %s: task %q is not in the experiment's declared task set — an undeclared task's records would corrupt corpus coverage", dir, rec.TrajectoryID)
		}
		seen[key] = true
		dirSeen[dirKey] = true
		if err := r.appendRecord(*rec); err != nil {
			return rep, err
		}
		invRecords = append(invRecords, *rec)
		rep.Records++
		rep.Tasks = append(rep.Tasks, rec.TrajectoryID)
	}
	if err := r.persistIngestAlarms(exp, invocation, declared, invRecords); err != nil {
		return rep, err
	}
	// Mirror the snapshot's skipped set so the operator's ingest
	// output shows what the declared corpus still owes — a
	// non-empty list means the invocation's verdict is void.
	recorded := map[string]bool{}
	for _, rec := range invRecords {
		recorded[rec.TrajectoryID] = true
	}
	for _, id := range declared {
		if !recorded[id] {
			rep.Skipped = append(rep.Skipped, id)
		}
	}
	rep.Tasks = slices.Compact(slices.Clone(rep.Tasks))
	return rep, nil
}

// ingestTrial builds one record from a single trial dir: verifier
// outcome from result.json, run metrics from the telemetry blob the
// in-container child wrote, session DB preserved + analyzed through
// the same path the runner's artifacts take.
// peekTrialTask reads just the task id out of a trial's result.json
// — the grouping key for run_index ordinals.
func peekTrialTask(dir string) (string, error) {
	data, err := os.ReadFile(filepath.Join(dir, "result.json"))
	if err != nil {
		return "", err
	}
	var res harborTrialResult
	if err := json.Unmarshal(data, &res); err != nil {
		return "", err
	}
	return harborTaskID(res.TaskID)
}

func (r *Runner) ingestTrial(ctx context.Context, exp *Experiment, arm, invocation, dir string, runIndex int, tempKey string, manifest *FlagsManifest) (*RunRecord, error) {
	data, err := os.ReadFile(filepath.Join(dir, "result.json"))
	if err != nil {
		return nil, fmt.Errorf("read result.json: %w", err)
	}
	var res harborTrialResult
	if err := json.Unmarshal(data, &res); err != nil {
		return nil, fmt.Errorf("parse result.json: %w", err)
	}
	taskID, err := harborTaskID(res.TaskID)
	if err != nil {
		return nil, err
	}
	outcome, detail := harborOutcome(&res)
	// The source dir is provenance — and the dedupe key that keeps
	// one trial's data from filling two paired cells.
	detail["trial_dir"] = dir
	dataDir := filepath.Join(dir, "agent", "crush-data")
	rec := &RunRecord{
		Experiment:   exp.Name,
		TrajectoryID: taskID,
		Arm:          arm,
		Invocation:   invocation,
		Outcome:      outcome,
		CheckDetail:  detail,
	}
	if res.AgentExecution != nil && !res.AgentExecution.StartedAt.IsZero() {
		rec.StartedAt = res.AgentExecution.StartedAt
		if !res.AgentExecution.FinishedAt.IsZero() {
			rec.DurationS = res.AgentExecution.FinishedAt.Sub(res.AgentExecution.StartedAt).Seconds()
		}
	}
	// Telemetry is the same CRUSH_EVAL_TELEMETRY blob the in-house
	// driver decodes — the copy-back makes the container child's
	// report readable host-side. A trial that died before writing
	// telemetry still earns its (error) record.
	if tel, err := readRunTelemetry(filepath.Join(dataDir, "telemetry.json")); err == nil {
		rec.SessionID = tel.SessionID
		rec.ParamVersion = tel.ParamVersion
		rec.Steps = tel.Steps
		rec.Tokens.Input = tel.Tokens.Input
		rec.Tokens.Output = tel.Tokens.Output
		rec.Tokens.CacheRead = tel.Tokens.CacheRead
		rec.Tokens.CacheWrite = tel.Tokens.CacheWrite
		rec.StubStats.Invalidations = tel.StubStats.Invalidations
		rec.StubStats.Results = tel.StubStats.Results
		rec.StubStats.SavedBytes = tel.StubStats.SavedBytes
		rec.StubStats.BoundaryAdvances = tel.StubStats.BoundaryAdvances
		rec.StubStats.Kinds = tel.StubStats.Kinds
		rec.PriorTurns.TurnsCollapsed = tel.PriorTurns.TurnsCollapsed
		rec.PriorTurns.EventsCollapsed = tel.PriorTurns.EventsCollapsed
		rec.Recalls.Result = tel.Recalls.Result
		rec.Recalls.Entry = tel.Recalls.Entry
		rec.Recalls.Empty = tel.Recalls.Empty
		rec.Recalls.Cross = tel.Recalls.Cross
		rec.Recalls.PriorTurnResult = tel.Recalls.PriorTurnResult
		rec.Checkpoints.Written = tel.Checkpoints.Written
		rec.Checkpoints.Rendered = tel.Checkpoints.Rendered
		rec.Digests.Written = tel.Digests.Written
		rec.Digests.Rendered = tel.Digests.Rendered
		rec.Hydration.Seeds = tel.Hydration.Seeds
		rec.Hydration.PlanSeeds = tel.Hydration.PlanSeeds
		rec.Hydration.Rendered = tel.Hydration.Rendered
		rec.EdgeFirings = tel.EdgeFirings
		rec.Env.ModelResolved = tel.Model
		rec.Env.ModelSmall = tel.ModelSmall
		rec.Env.ModelSummary = tel.ModelSummary
		rec.ErrorClass = tel.ErrorClass
		if tel.Error != "" && outcome != OutcomeError {
			detail["run_error"] = tel.Error
		}
		if tel.GeneratorTokens.Calls > 0 {
			rec.GeneratorTokens = &GeneratorTokens{
				Calls:      tel.GeneratorTokens.Calls,
				Input:      tel.GeneratorTokens.Input,
				Output:     tel.GeneratorTokens.Output,
				CacheRead:  tel.GeneratorTokens.CacheRead,
				CacheWrite: tel.GeneratorTokens.CacheWrite,
			}
		}
		if tel.Request.PromptRequests > 0 {
			rec.Request = &RequestStats{
				PromptRequests:   tel.Request.PromptRequests,
				PromptTokensPeak: tel.Request.PromptTokensPeak,
				SystemBytes:      tel.Request.SystemBytes,
				NotebookBytes:    tel.Request.NotebookBytes,
				HistoryBytes:     tel.Request.HistoryBytes,
				ToolCallBytes:    tel.Request.ToolCallBytes,
				ToolResultBytes:  tel.Request.ToolResultBytes,
			}
		}
		if tel.Pressure.Estimate > 0 || tel.Pressure.Engaged || tel.Pressure.Activations > 0 {
			rec.Pressure = &Pressure{
				Activations: tel.Pressure.Activations,
				Engaged:     tel.Pressure.Engaged,
				Estimate:    tel.Pressure.Estimate,
			}
		}
		if len(tel.ResolvedOptions) > 0 {
			rec.ResolvedOptions = tel.ResolvedOptions
			rec.BaselineKey = manifest.keyWith(tel.ResolvedOptions, map[string]any{"$temperature": tempKey})
		}
		if tel.Tail != nil {
			rec.Tail = []TurnTail{*tel.Tail}
		}
		rec.TailRuns = tel.TailRuns
		if tel.Drain != nil {
			rec.Drains = []DrainReport{*tel.Drain}
		}
		rec.StepRecords = tel.Request.Steps
	}
	// The session DB is the failed-run forensic artifact — same
	// preserve-then-analyze path as preserveArtifacts, against the
	// copied-back file. Its absence is the copy-back breaking —
	// record it like preserveArtifacts records a failed preserve.
	dbPath := filepath.Join(dataDir, "crush.db")
	if !fileExists(dbPath) {
		rec.CallMetricsError = fmt.Sprintf("no session db at %s — agent copy-back did not land", dbPath)
	} else {
		dstDir := filepath.Join(r.EvalDir, "results", exp.Name, "artifacts")
		if err := os.MkdirAll(dstDir, 0o755); err != nil {
			return nil, err
		}
		dst := filepath.Join(dstDir, fmt.Sprintf("%s-%s-%s-%d.db", taskID, arm, invocation, runIndex))
		walSafe, err := vacuumDBInto(ctx, dbPath, dst)
		if err != nil {
			rec.CallMetricsError = fmt.Sprintf("session db not preserved: %v", err)
		} else {
			if rel, rerr := filepath.Rel(r.EvalDir, dst); rerr == nil {
				rec.SessionDB = rel
			}
			rec.SessionDBIncomplete = !walSafe
			metrics, aerr := AnalyzeSessionDB(ctx, dst, AnalyzeOptions{
				SessionID: rec.SessionID,
				GOOS:      "linux", // harbor task containers are linux
			})
			if aerr != nil {
				rec.CallMetricsError = aerr.Error()
			} else {
				rec.CallMetrics = metrics
				if rec.SessionID == "" {
					rec.SessionID = metrics.SessionID
				}
			}
		}
	}
	rec.Env.ModelPin = exp.Model
	rec.Env.Temperature = tempKey
	rec.Env.CrushSHA = exp.Harbor.HarnessRevision
	rec.Env.OS = "linux"
	return rec, nil
}

// readRunTelemetry decodes one CRUSH_EVAL_TELEMETRY blob — the same
// schema the in-house driver's per-turn files carry.
func readRunTelemetry(path string) (*runTelemetry, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var tel runTelemetry
	if err := json.Unmarshal(data, &tel); err != nil {
		return nil, fmt.Errorf("parse telemetry: %w", err)
	}
	return &tel, nil
}

// vacuumDBInto snapshots a WAL-mode sqlite db to dst via VACUUM INTO
// (consistent, includes un-checkpointed commits), falling back to a
// raw copy. walSafe=false marks the fallback — the WAL tail may be
// truncated.
func vacuumDBInto(ctx context.Context, src, dst string) (walSafe bool, err error) {
	conn, err := db.ConnectReadOnly(ctx, src)
	if err == nil {
		// VACUUM INTO takes a string literal, not a bound parameter.
		_, err = conn.ExecContext(ctx, `VACUUM INTO '`+strings.ReplaceAll(dst, "'", "''")+`'`)
		conn.Close()
	}
	if err != nil {
		data, rerr := os.ReadFile(src)
		if rerr != nil {
			return false, rerr
		}
		if werr := os.WriteFile(dst, data, 0o644); werr != nil {
			return false, werr
		}
		return false, nil
	}
	return true, nil
}

// persistIngestAlarms rewrites the invocation's alarm snapshot for
// an ingested run: same refusal ground truth persistAlarms writes —
// the declared pins (primary, decision rule, resample unit) that
// provenance checks compare against, and the declared-minus-recorded
// skipped set that voids a partial corpus. Called after each arm's
// ingest so the snapshot converges over the union of both arms'
// records.
func (r *Runner) persistIngestAlarms(exp *Experiment, invocation string, declared []string, invRecords []RunRecord) error {
	recorded := map[string]bool{}
	for _, rec := range invRecords {
		recorded[rec.TrajectoryID] = true
	}
	snap := alarmSnapshot{
		Invocation:   invocation,
		Primary:      exp.Primary,
		DecisionRule: exp.DecisionRule,
		Resample:     exp.Resample,
	}
	for _, id := range declared {
		if !recorded[id] {
			snap.Skipped = append(snap.Skipped, id)
		}
	}
	dir := filepath.Join(r.EvalDir, "results", exp.Name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "report-"+invocation+".json"), data, 0o644)
}
