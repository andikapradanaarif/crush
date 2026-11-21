package eval

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
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

// harborCrushAgent is the registered agent identity the crush
// wrapper's jobs carry in config.json's agent block — the exact
// string a crush arm's trial must report. agent_info's self-report
// spells it short ("crush"); both are admitted, anything else is a
// foreign trial (#510).
const harborCrushAgent = "eval.harbor.crush_agent:Crush"

// recordLoc names where one ingested trial already lives — the
// (invocation, arm, run_index) a dedup refusal points back to.
type recordLoc struct {
	task       string
	arm        string
	runIndex   int
	invocation string
}

// checkHarborPin is the provenance gate (#504): what ran must be
// what the manifest declared. The resolved large model must equal
// exp.Model and — when the manifest pins `small_model` — the resolved
// small must equal it too; the declared temperature must equal the
// large slot's resolved request-side temperature once the child's
// telemetry carries it. Empty telemetry fields (a trial that died
// before its first call, or a child predating the sampling export)
// can't prove a mismatch and are already inconclusive — they pass.
func checkHarborPin(exp *Experiment, rec *RunRecord) error {
	// Foreign arms (#510): the trial's only model evidence is the
	// harbor-reported spelling — compare it to the arm's declared
	// foreign_agent.model, and refuse a silent gap outright: a
	// foreign record whose model can't be proven is contamination,
	// not a vacuous pass like crush arms' empty wire stamps (which
	// are inconclusive on other axes anyway). Small-model and
	// temperature checks don't apply — foreign wire stamps don't
	// exist for ingest to verify.
	if fa := exp.Arms[rec.Arm].ForeignAgent; fa != nil {
		switch {
		case rec.Env.ModelResolved == "":
			return fmt.Errorf("arm %s declares foreign agent %q but the trial reported no model — parity with the declared %q can't be proven", rec.Arm, fa.Name, fa.Model)
		case rec.Env.ModelResolved != fa.Model:
			return fmt.Errorf("arm %s: harbor-reported model %q ≠ declared foreign model %q — the job that ran is not the declared condition", rec.Arm, rec.Env.ModelResolved, fa.Model)
		}
		return nil
	}
	if exp.Model != "" && rec.Env.ModelResolved != "" && rec.Env.ModelResolved != exp.Model {
		return fmt.Errorf("resolved model %q ≠ manifest pin %q — the arm config that ran is not the declared condition", rec.Env.ModelResolved, exp.Model)
	}
	if exp.SmallModel != "" && rec.Env.ModelSmall != "" && rec.Env.ModelSmall != exp.SmallModel {
		return fmt.Errorf("resolved small model %q ≠ manifest small_model %q — auxiliary calls ran on a different model than the arm declared", rec.Env.ModelSmall, exp.SmallModel)
	}
	if exp.Temperature != nil {
		if s, ok := rec.Env.Sampling["large"]; ok {
			switch {
			case s.Temperature == nil:
				return fmt.Errorf("declared temperature %v but the run sent none — the arm config that ran is not the declared condition", *exp.Temperature)
			case *s.Temperature != *exp.Temperature:
				return fmt.Errorf("resolved temperature %v ≠ manifest %v — the run's condition differs from the declared one", *s.Temperature, *exp.Temperature)
			}
		}
	}
	return nil
}

// IngestReport summarizes one IngestHarborTrials call.
type IngestReport struct {
	Experiment string   `json:"experiment"`
	Invocation string   `json:"invocation"`
	Arm        string   `json:"arm"`
	Records    int      `json:"records"`
	Tasks      []string `json:"tasks"`
	Skipped    []string `json:"skipped"`
	// Excluded names declared tasks struck by harbor.exclude — the
	// coverage obligation the manifest forgave, shown beside
	// Skipped so a clean read still shows its reduced corpus.
	Excluded []string `json:"excluded,omitempty"`
	// Pending names trial dirs skipped because they carry no terminal
	// artifact — the trial is still in flight, not failed; ingest it
	// again once harbor writes result.json or exception.txt.
	Pending []string `json:"pending"`
}

// harborTrialResult is the subset of harbor's TrialResult the
// ingester reads. task_id arrives as one of harbor's task-id object
// shapes (LocalTaskId{path}, GitTaskId{...,path},
// PackageTaskId{name,...}) — harborTaskID normalizes all three to
// the benchmark task name.
type harborTrialResult struct {
	TaskID json.RawMessage `json:"task_id"`
	// ID is harbor's per-trial UUID — a dir-independent dedup key:
	// a job moved or re-ingested under a differently-spelled path
	// still matches (#504).
	ID             string `json:"id"`
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
	// AgentInfo is the agent's self-reported identity (#510) —
	// name and a loosely-shaped model_info the config block
	// supplements; either may carry the model a foreign agent ran.
	AgentInfo *struct {
		Name      string          `json:"name"`
		ModelInfo json.RawMessage `json:"model_info"`
	} `json:"agent_info"`
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

// harborOutcome normalizes a trial's terminal facts. Reward,
// exception, and execution are retained as SEPARATE fields — #503
// had 56 trials carrying both an exception_info and a verifier
// reward (three of them reward 1), and the early exception return
// that dropped the reward is exactly the evidence loss this fixes:
// whether a timed-out-but-verified trial scores is a protocol
// decision the record must let the owner revisit without re-reading
// raw trial dirs.
//
// Eligibility is explicit, not flow-shaped:
//   - exception anywhere → the verdict is contaminated: OutcomeError,
//     not score-eligible, requeue-eligible (a fresh run can replace
//     the observation; its retained reward is evidence, not a verdict);
//   - no usable reward → no verdict was produced: OutcomeError,
//     not score-eligible, requeue-eligible;
//   - reward ≥ 1 → pass; 0 ≤ reward < 1 → fail (binary verifiers
//     emit exact 0/1 — a fractional value is genuine partial credit,
//     and partial credit is not a pass); both score-eligible;
//   - negative reward → malformed artifact: error, requeue-eligible.
func harborOutcome(res *harborTrialResult) (Outcome, map[string]any) {
	detail := map[string]any{}
	detail["executed"] = res.AgentExecution != nil
	if res.ExceptionInfo != nil {
		detail["exception_type"] = res.ExceptionInfo.Type
		detail["exception_message"] = res.ExceptionInfo.Message
	}
	reward, hasReward := 0.0, false
	if res.VerifierResult == nil || len(res.VerifierResult.Rewards) == 0 {
		detail["missing"] = "verifier_result"
	} else if r, ok := res.VerifierResult.Rewards["reward"]; ok {
		reward, hasReward = r, true
	} else if len(res.VerifierResult.Rewards) == 1 {
		for _, v := range res.VerifierResult.Rewards {
			reward, hasReward = v, true
		}
	} else {
		detail["rewards"] = res.VerifierResult.Rewards
	}
	if hasReward {
		detail["reward"] = reward
	}
	// The explicit eligibility rule — kept beside the derived fields
	// so the record states its own protocol handling.
	eligible := func(score, requeue bool) {
		detail["score_eligible"] = score
		detail["requeue_eligible"] = requeue
	}
	switch {
	case res.ExceptionInfo != nil, !hasReward, reward < 0:
		eligible(false, true)
		return OutcomeError, detail
	case reward >= 1.0:
		eligible(true, false)
		return OutcomePass, detail
	default:
		eligible(true, false)
		return OutcomeFail, detail
	}
}

// HarborTaskSet resolves the experiment's declared task list — the
// corpus-coverage ground truth the alarm snapshot's skipped set
// subtracts recorded ids from.
func HarborTaskSet(evalDir string, spec *HarborSpec) ([]string, error) {
	if spec == nil {
		return nil, fmt.Errorf("harbor spec declares no task set")
	}
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
	for _, id := range slices.Sorted(maps.Keys(exp.Harbor.Exclude)) {
		if !declaredSet[id] {
			return nil, fmt.Errorf("ingest: harbor.exclude names undeclared task %q — exclusions can only strike the declared set", id)
		}
	}
	manifest, err := LoadFlagsManifest(r.EvalDir)
	if err != nil {
		return nil, err
	}
	// Harbor trials key off the arm's config blocks — the same
	// validation RunExperiment runs, or a malformed/unvalidated
	// pre_tool_use spelling would hash a BaselineKey no local run
	// of the identical arm produces (canonicalization is in place
	// and idempotent).
	if err := manifest.ValidateArmFlags(exp); err != nil {
		return nil, err
	}
	existing, err := r.LoadExperimentRecords(exp.Name)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	// One trial's data must land in exactly one cell of the whole
	// experiment — same arm re-ingest, a second arm, and a later
	// invocation are all refusals, or a duplicated job produces a
	// fake perfect A/A (#504, the pilot-b2 defect). The index spans
	// every recorded invocation and arm, keyed on the cleaned trial
	// path AND harbor's trial id — either match means the trial is
	// already recorded.
	seenDir := map[string]recordLoc{}
	seenTrialID := map[string]recordLoc{}
	var invRecords []RunRecord
	nextIndex := map[string]int{}
	for _, rec := range existing {
		loc := recordLoc{task: rec.TrajectoryID, arm: rec.Arm, runIndex: rec.RunIndex, invocation: rec.Invocation}
		if d, _ := rec.CheckDetail["trial_dir"].(string); d != "" {
			seenDir[filepath.Clean(d)] = loc
		}
		if id, _ := rec.CheckDetail["trial_id"].(string); id != "" {
			seenTrialID[id] = loc
		}
		if rec.Invocation != invocation {
			continue
		}
		invRecords = append(invRecords, rec)
		seen[rec.TrajectoryID+"\x00"+rec.Arm+"\x00"+strconv.Itoa(rec.RunIndex)] = true
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
	pending := make([]bool, len(dirs))
	for i, dir := range dirs {
		id, err := peekTrialTask(dir)
		if errors.Is(err, errTrialPending) {
			pending[i] = true
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("ingest %s: %w", dir, err)
		}
		taskOf[i] = id
	}
	tempKey := temperatureKey(exp.Temperature)
	rep := &IngestReport{Experiment: exp.Name, Invocation: invocation, Arm: arm}
	for i, dir := range dirs {
		if pending[i] {
			// A still-running trial's task is best-effort — its
			// config.json is written at trial start, the dir
			// basename is the last resort.
			if id, err := trialConfigTask(dir); err == nil && id != "" {
				rep.Pending = append(rep.Pending, id)
			} else {
				rep.Pending = append(rep.Pending, filepath.Base(filepath.Clean(dir)))
			}
			continue
		}
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
		if err := checkHarborPin(exp, rec); err != nil {
			return rep, fmt.Errorf("ingest %s: %w", dir, err)
		}
		key := rec.TrajectoryID + "\x00" + rec.Arm + "\x00" + strconv.Itoa(rec.RunIndex)
		if seen[key] {
			return rep, fmt.Errorf("ingest %s: %s arm %s run %d already recorded in invocation %s — refuse to duplicate a paired cell",
				dir, rec.TrajectoryID, rec.Arm, rec.RunIndex, invocation)
		}
		newLoc := recordLoc{task: rec.TrajectoryID, arm: arm, runIndex: rec.RunIndex, invocation: invocation}
		if loc, dup := seenDir[filepath.Clean(dir)]; dup {
			return rep, fmt.Errorf("ingest %s: this trial is already recorded for %s arm %s run %d in invocation %s — the same run's data can't fill two cells",
				dir, loc.task, loc.arm, loc.runIndex, loc.invocation)
		}
		if id, _ := rec.CheckDetail["trial_id"].(string); id != "" {
			if loc, dup := seenTrialID[id]; dup {
				return rep, fmt.Errorf("ingest %s: harbor trial id %s is already recorded for %s arm %s run %d in invocation %s — the same run's data can't fill two cells",
					dir, id, loc.task, loc.arm, loc.runIndex, loc.invocation)
			}
			seenTrialID[id] = newLoc
		}
		if !declaredSet[rec.TrajectoryID] {
			return rep, fmt.Errorf("ingest %s: task %q is not in the experiment's declared task set — an undeclared task's records would corrupt corpus coverage", dir, rec.TrajectoryID)
		}
		seen[key] = true
		seenDir[filepath.Clean(dir)] = newLoc
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
		if exp.Harbor.Exclude[id] != "" {
			rep.Excluded = append(rep.Excluded, id)
			continue
		}
		if !recorded[id] {
			rep.Skipped = append(rep.Skipped, id)
		}
	}
	slices.Sort(rep.Excluded)
	rep.Tasks = slices.Compact(slices.Clone(rep.Tasks))
	return rep, nil
}

// ingestTrial builds one record from a single trial dir: verifier
// outcome from result.json, run metrics from the telemetry blob the
// in-container child wrote, session DB preserved + analyzed through
// the same path the runner's artifacts take.
// errTrialPending marks a trial dir with no terminal artifact — the
// trial is still in flight. Ingest skips it rather than erroring a
// whole job's worth of finished trials on one live straggler.
var errTrialPending = errors.New("trial has no terminal artifact")

// peekTrialTask reads just the task id out of a trial's result.json
// — the grouping key for run_index ordinals. A trial that raised
// before harbor wrote result.json (older harbor layouts leave
// exception.txt alone) derives its id from config.json; one still in
// flight returns errTrialPending.
func peekTrialTask(dir string) (string, error) {
	data, err := os.ReadFile(filepath.Join(dir, "result.json"))
	if err == nil {
		var res harborTrialResult
		if err := json.Unmarshal(data, &res); err != nil {
			return "", err
		}
		return harborTaskID(res.TaskID)
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return "", err
	}
	if !fileExists(filepath.Join(dir, "exception.txt")) {
		return "", errTrialPending
	}
	return trialConfigTask(dir)
}

// trialConfigTask recovers the task id for a trial that never wrote
// result.json — config.json's task.path basename (local/git ids), its
// task.name (package ids), or the trial_name's "__"-prefix.
func trialConfigTask(dir string) (string, error) {
	data, err := os.ReadFile(filepath.Join(dir, "config.json"))
	if err != nil {
		return "", err
	}
	var cfg struct {
		Task struct {
			Path string `json:"path"`
			Name string `json:"name"`
		} `json:"task"`
		TrialName string `json:"trial_name"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return "", err
	}
	switch {
	case cfg.Task.Path != "":
		return filepath.Base(filepath.Clean(cfg.Task.Path)), nil
	case cfg.Task.Name != "":
		return cfg.Task.Name, nil
	case cfg.TrialName != "":
		if i := strings.LastIndex(cfg.TrialName, "__"); i > 0 {
			return cfg.TrialName[:i], nil
		}
		return cfg.TrialName, nil
	}
	return "", fmt.Errorf("config.json carries no task id")
}

// trialAgent reports which agent ran a trial (#510): config.json's
// agent block is the scheduled identity (name = registered agent,
// model_name = the --model LiteLLM spelling); result.json's
// agent_info is the agent's self-report filling either gap.
func trialAgent(dir string, res *harborTrialResult) (name, model string) {
	if data, err := os.ReadFile(filepath.Join(dir, "config.json")); err == nil {
		var cfg struct {
			Agent struct {
				Name      string `json:"name"`
				ModelName string `json:"model_name"`
			} `json:"agent"`
		}
		if json.Unmarshal(data, &cfg) == nil {
			name, model = cfg.Agent.Name, cfg.Agent.ModelName
		}
	}
	if res != nil && res.AgentInfo != nil {
		if name == "" {
			name = res.AgentInfo.Name
		}
		if model == "" {
			model = agentInfoModel(res.AgentInfo.ModelInfo)
		}
	}
	return name, model
}

// agentInfoModel pulls a model string out of agent_info.model_info —
// harbor agents emit it as a plain string or an object carrying a
// name/model field.
func agentInfoModel(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var obj struct {
		Name  string `json:"name"`
		Model string `json:"model"`
	}
	if json.Unmarshal(raw, &obj) == nil {
		if obj.Name != "" {
			return obj.Name
		}
		return obj.Model
	}
	return ""
}

// exceptionDetail distills a trial's exception.txt into check_detail
// fields — the last non-empty line of a harbor traceback carries
// "some.ErrorType: message".
func exceptionDetail(dir string) map[string]any {
	detail := map[string]any{"exception_source": "exception.txt"}
	data, err := os.ReadFile(filepath.Join(dir, "exception.txt"))
	if err != nil {
		detail["exception_type"] = "unknown"
		return detail
	}
	last := ""
	for line := range strings.Lines(string(data)) {
		if t := strings.TrimSpace(line); t != "" {
			last = t
		}
	}
	if i := strings.Index(last, ": "); i > 0 {
		detail["exception_type"] = last[:i]
		detail["exception_message"] = last[i+2:]
	} else if last != "" {
		detail["exception_type"] = last
	} else {
		detail["exception_type"] = "unknown"
	}
	return detail
}

func (r *Runner) ingestTrial(ctx context.Context, exp *Experiment, arm, invocation, dir string, runIndex int, tempKey string, manifest *FlagsManifest) (*RunRecord, error) {
	var res harborTrialResult
	var taskID string
	var outcome Outcome
	var detail map[string]any
	data, err := os.ReadFile(filepath.Join(dir, "result.json"))
	switch {
	case err == nil:
		if err := json.Unmarshal(data, &res); err != nil {
			return nil, fmt.Errorf("parse result.json: %w", err)
		}
		taskID, err = harborTaskID(res.TaskID)
		if err != nil {
			return nil, err
		}
		outcome, detail = harborOutcome(&res)
	case errors.Is(err, fs.ErrNotExist) && fileExists(filepath.Join(dir, "exception.txt")):
		// Trial raised before harbor wrote result.json — the
		// exception file is the terminal artifact; the record is
		// error by definition, and whatever telemetry/DB landed
		// before the raise still fills its metrics below.
		taskID, err = trialConfigTask(dir)
		if err != nil {
			return nil, fmt.Errorf("derive task from config.json: %w", err)
		}
		outcome, detail = OutcomeError, exceptionDetail(dir)
		// Same explicit rule as result.json exceptions: contaminated
		// verdict — not score-eligible, requeue-eligible.
		detail["score_eligible"] = false
		detail["requeue_eligible"] = true
	default:
		return nil, fmt.Errorf("read result.json: %w", err)
	}
	// The source dir is provenance — and the dedupe key that keeps
	// one trial's data from filling two paired cells. The harbor
	// trial id is the dir-independent second key (#504).
	detail["trial_dir"] = dir
	if res.ID != "" {
		detail["trial_id"] = res.ID
	}
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
	if res.AgentResult != nil {
		rec.CostUSD = res.AgentResult.CostUSD
	}
	// Agent identity is provenance and a refusal (#510): the arm
	// declares who ran — a foreign arm wants exactly its declared
	// agent, a crush arm wants the crush wrapper; a job run by
	// anything else under either arm is the wrong experiment's data.
	agentName, agentModel := trialAgent(dir, &res)
	rec.Env.Agent = agentName
	fa := exp.Arms[arm].ForeignAgent
	switch {
	case fa != nil:
		if agentName != fa.Name {
			return nil, fmt.Errorf("arm %s declares foreign agent %q but the trial ran %q — a different agent's records can't fill this arm's cells", arm, fa.Name, agentName)
		}
		// Foreign trials carry no crush-data — their metrics are
		// harbor's own accounting: verifier outcome (above), wall
		// time from agent_execution (above), tokens and cost from
		// agent_result. crush-data absence is by design, not a
		// copy-back defect.
		rec.Env.ModelResolved = agentModel
		// env.model_resolved will carry the harbor-side spelling
		// while env.model_pin keeps exp.Model's — a reader seeing
		// two different strings could take it for the drift the pin
		// check exists to catch. The parity claim is declared, so
		// record it: same route and weights by construction (#510).
		detail["model_parity"] = fmt.Sprintf("harbor spelling %q declared ≡ manifest pin %q", agentModel, exp.Model)
		if res.AgentResult != nil {
			if res.AgentResult.InputTokens != nil {
				rec.Tokens.Input = *res.AgentResult.InputTokens
			}
			if res.AgentResult.OutputTokens != nil {
				rec.Tokens.Output = *res.AgentResult.OutputTokens
			}
			if res.AgentResult.CacheTokens != nil {
				rec.Tokens.CacheRead = *res.AgentResult.CacheTokens
			}
		}
	case agentName != "" && agentName != harborCrushAgent && agentName != "crush":
		return nil, fmt.Errorf("arm %s runs the crush wrapper but the trial ran %q — a foreign agent's records can't fill a crush arm's cells", arm, agentName)
	}
	// Telemetry is the same CRUSH_EVAL_TELEMETRY blob the in-house
	// driver decodes — the copy-back makes the container child's
	// report readable host-side. A trial that died before writing
	// telemetry still earns its (error) record.
	if fa == nil {
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
			rec.PriorTurns.SegmentsCollapsed = tel.PriorTurns.SegmentsCollapsed
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
			rec.Env.Sampling = tel.Sampling
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
				// The arm's hooks/tools blocks key the record the same
				// way a local run would — the container's config was
				// rendered from this arm, and pooled baselines must not
				// mix hook/tool conditions (#388 + #409).
				keyExtra := exp.Arms[arm].Config.KeyExtra()
				keyExtra["$temperature"] = tempKey
				rec.BaselineKey = manifest.keyWith(tel.ResolvedOptions, keyExtra)
			}
			if spec := exp.Arms[arm].BestOfK; spec != nil {
				sel := spec.Selection
				if sel == "" {
					sel = SelectionLastAttempt
				}
				rec.BestOfK = &BestOfKMark{K: spec.K, Selection: sel}
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
	}
	// Harbor reports no per-trial cost — price the billed classes at
	// the manifest's pinned route rates so a paid pin's spend is
	// measured, not assumed. A harness-reported cost wins when
	// present; the main-model classes price the spend (the sidecar's
	// generator tokens run a different model's rates).
	if rec.CostUSD == nil && exp.CostRates != nil {
		if c, err := exp.CostRates.price(rec.Tokens); err != nil {
			// An unpinned block reached ingest — a manifest built
			// outside LoadExperiment can skip the pinned check.
			// Record it rather than panic (#516 review).
			detail["cost_error"] = err.Error()
		} else {
			rec.CostUSD = &c
			detail["cost_basis"] = "cost_rates"
		}
	}
	// The session DB is the failed-run forensic artifact — same
	// preserve-then-analyze path as preserveArtifacts, against the
	// copied-back file. Its absence is the copy-back breaking —
	// record it like preserveArtifacts records a failed preserve.
	// Foreign arms have no crush.db by design (#510) — nothing to
	// preserve or flag.
	dbPath := filepath.Join(dataDir, "crush.db")
	switch {
	case fa != nil:
	case !fileExists(dbPath):
		rec.CallMetricsError = fmt.Sprintf("no session db at %s — agent copy-back did not land", dbPath)
	default:
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
			// The session's own catwalk-priced cost is the independent
			// cross-check for the pinned-rate number — provenance in
			// check_detail, never the primary value.
			if rec.SessionID != "" {
				if c, cerr := sessionDBCost(ctx, dst, rec.SessionID); cerr == nil {
					detail["db_cost_usd"] = c
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
		if exp.Harbor != nil && exp.Harbor.Exclude[id] != "" {
			snap.ExcludedTasks = append(snap.ExcludedTasks, id)
			continue
		}
		if !recorded[id] {
			snap.Skipped = append(snap.Skipped, id)
		}
	}
	slices.Sort(snap.ExcludedTasks)
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
