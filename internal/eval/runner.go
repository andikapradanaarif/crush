package eval

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/charmbracelet/crush/internal/version"
)

// Runner orchestrates quarantine, characterization, and experiments
// over a corpus directory.
type Runner struct {
	// EvalDir is the eval/ root (corpus/, experiments/, results/,
	// bands.json, flags.json live under it).
	EvalDir string
	// Driver executes agent runs; nil uses CrushRunner with the
	// current executable.
	Driver AgentRunner
	// Home is the pinned HOME for agent subprocesses. Created under
	// os.MkdirTemp when empty.
	Home string
	// AttemptsFactor bounds resampling: the runner samples to N
	// conclusive per arm with an attempts cap of AttemptsFactor*N
	// (~2N per the spec) before flagging the trajectory starved or
	// saturated.
	AttemptsFactor float64
	// PermReplicates is the diffuse-tier replicate count.
	PermReplicates int
	// Alpha is the per-family significance level before correction.
	Alpha float64
	// QuarantineRepeats is M — check.sh invocations per state.
	QuarantineRepeats int
	// WorkParent is where materialized workdirs are created. Empty
	// uses os.TempDir.
	WorkParent string
	RNG        *rand.Rand
	// Now is the clock, injectable for tests.
	Now func() time.Time
	// homeCreated marks Home as runner-allocated so Close can
	// remove it; a caller-provided Home is the caller's to clean.
	homeCreated bool
}

// Close removes the pinned-HOME tempdir when the runner created it.
func (r *Runner) Close() {
	if r.homeCreated && r.Home != "" {
		os.RemoveAll(r.Home)
		r.homeCreated = false
	}
}

func (r *Runner) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *Runner) rng() *rand.Rand {
	if r.RNG != nil {
		return r.RNG
	}
	return rand.New(rand.NewPCG(uint64(r.now().UnixNano()), 0))
}

func (r *Runner) driver() AgentRunner {
	if r.Driver != nil {
		return r.Driver
	}
	return CrushRunner{Home: r.home()}
}

func (r *Runner) home() string {
	if r.Home != "" {
		return r.Home
	}
	h, err := os.MkdirTemp("", "crush-eval-home-*")
	if err == nil {
		r.Home = h
		r.homeCreated = true
	}
	return r.Home
}

func (r *Runner) attemptsFactor() float64 {
	if r.AttemptsFactor > 0 {
		return r.AttemptsFactor
	}
	return 2.0
}

func (r *Runner) alpha() float64 {
	if r.Alpha > 0 {
		return r.Alpha
	}
	return 0.05
}

func (r *Runner) permReplicates() int {
	if r.PermReplicates > 0 {
		return r.PermReplicates
	}
	return 10000
}

func (r *Runner) workParent() string {
	if r.WorkParent != "" {
		return r.WorkParent
	}
	return os.TempDir()
}

// Quarantine runs the agent-free validation pass for one trajectory:
// the check must agree with expect_start_state on the start state, pass
// on start+reference (when present), and fail on start+counterexample
// (pass-guards). Returns the quarantine reason, or "" when the
// trajectory is clean.
func (r *Runner) Quarantine(ctx context.Context, traj *Trajectory, trajDir string) (QuarantineReason, error) {
	m := r.QuarantineRepeats
	if m <= 0 {
		m = 5
	}
	// Checks chdir into the workdir; a relative trajectory dir must be
	// resolved to absolute before it reaches the check's environment.
	trajDir, err := filepath.Abs(trajDir)
	if err != nil {
		return "", err
	}

	// check runs the check n times against a materialization and
	// reports whether results were consistent and what they said.
	check := func(patch string) (allPass, allFail bool, err error) {
		wd, err := Materialize(ctx, traj, trajDir, r.workParent())
		if err != nil {
			return false, false, err
		}
		defer os.RemoveAll(wd)
		if patch != "" {
			if err := ApplyPatch(ctx, wd, patch); err != nil {
				return false, false, err
			}
		}
		outs := map[bool]int{}
		for range m {
			res := RunCheck(ctx, traj, trajDir, wd)
			if res.Err != nil {
				return false, false, res.Err
			}
			outs[res.Exit == 0]++
		}
		return outs[true] == m, outs[false] == m, nil
	}

	startPass, startFail, err := check("")
	if err != nil {
		return "", err
	}
	wantPass := traj.Check.ExpectStartState == "pass"

	// Inconsistent results → flaky check.
	if !startPass && !startFail {
		return ReasonFlaky, nil
	}
	// Pass on start where fail expected → vacuous check.
	if !wantPass && startPass {
		return ReasonVacuous, nil
	}
	// Fail on start where pass expected → also vacuous-in-reverse:
	// the check can't even see the good state.
	if wantPass && startFail {
		return ReasonVacuous, nil
	}

	ref := filepath.Join(trajDir, "reference.patch")
	if fileExists(ref) {
		pass, _, err := check(ref)
		if err != nil {
			return "", err
		}
		if !pass {
			// Fails on a known-good state — mis-calibrated,
			// confidently wrong, worse than flaky.
			return ReasonMiscalibrated, nil
		}
	}

	counter := filepath.Join(trajDir, "counterexample.patch")
	if fileExists(counter) {
		_, fail, err := check(counter)
		if err != nil {
			return "", err
		}
		if !fail {
			// Vacuous pass-guard: the check can't see bad.
			return ReasonVacuous, nil
		}
	}
	return "", nil
}

// ExecuteRun performs one trajectory run: materialize, arm config,
// agent turns, check, coverage — and classifies the outcome.
//
// Precedence: error (transport) > timeout (budget) > fail >
// inconclusive (coverage unmet) > pass. A timeout run never reaches
// check.sh — the bound preempts the verdict.
func (r *Runner) ExecuteRun(ctx context.Context, exp *Experiment, traj *Trajectory, trajDir, armName string, arm Arm, manifest *FlagsManifest, attempt int) (RunRecord, error) {
	rec := RunRecord{
		Experiment:   exp.Name,
		TrajectoryID: traj.ID,
		Arm:          armName,
		RunIndex:     attempt,
		StartedAt:    r.now(),
		Env: Env{
			CrushSHA: version.Commit,
			Go:       runtime.Version(),
			OS:       runtime.GOOS,
		},
	}

	contentHash, err := ContentHash(trajDir)
	if err != nil {
		return rec, fmt.Errorf("content hash: %w", err)
	}
	rec.Env.ContentHash = contentHash
	rec.BaselineKey = manifest.BaselineKey(arm.Config.Options)

	workdir, err := Materialize(ctx, traj, trajDir, r.workParent())
	if err != nil {
		rec.Outcome = OutcomeError
		return rec, nil
	}
	defer os.RemoveAll(workdir)

	if err := WriteArmConfig(workdir, exp, arm); err != nil {
		rec.Outcome = OutcomeError
		rec.CheckDetail = map[string]any{"harness": err.Error()}
		return rec, nil
	}

	rec.StartedAt = r.now()
	res := r.driver().Run(ctx, workdir, traj.Task.Turns, traj.Budget)
	rec.DurationS = r.now().Sub(rec.StartedAt).Seconds()
	rec.Steps = res.Steps
	rec.Tokens = res.Tokens
	rec.StubStats = res.StubStats
	rec.Recalls = res.Recalls
	rec.Env.ModelPin = exp.Model
	rec.Env.ModelResolved = res.ModelResolved

	// Preserve the session DB — the failed-run debugging artifact is
	// the full message/tool trace, free.
	if res.SessionID != "" {
		if dst, err := r.preserveSessionDB(exp.Name, traj.ID, armName, attempt, workdir); err == nil {
			rec.SessionDB = dst
		}
	}

	switch {
	case res.Err != nil:
		// The model didn't produce the outcome; the transport did.
		rec.Outcome = OutcomeError
		rec.CheckDetail = map[string]any{"run_error": res.Err.Error()}
		return rec, nil
	case res.TimedOut || (traj.Budget.MaxSteps > 0 && res.Steps > traj.Budget.MaxSteps):
		rec.Outcome = OutcomeTimeout
		return rec, nil
	}

	chk := RunCheck(ctx, traj, trajDir, workdir)
	if chk.Err != nil {
		rec.Outcome = OutcomeError
		rec.CheckDetail = map[string]any{"check_error": chk.Err.Error()}
		return rec, nil
	}
	rec.CheckDetail = chk.Detail
	if chk.Exit != 0 {
		rec.Outcome = OutcomeFail
		return rec, nil
	}

	// Coverage masks passes only: a check-fail is a real outcome
	// whether or not the mechanism fired.
	met, err := CoverageMet(traj.Coverage, &rec)
	if err != nil {
		return rec, fmt.Errorf("coverage eval: %w", err)
	}
	if !met {
		rec.Outcome = OutcomeInconclusive
		return rec, nil
	}
	rec.Outcome = OutcomePass
	return rec, nil
}

// preserveSessionDB copies the run's SQLite DB into
// results/<experiment>/artifacts/<trajectory>-<arm>-<run_index>.db.
func (r *Runner) preserveSessionDB(expName, trajID, arm string, runIndex int, workdir string) (string, error) {
	src := filepath.Join(workdir, ".crush", "crush.db")
	if !fileExists(src) {
		return "", fmt.Errorf("no session db at %s", src)
	}
	dstDir := filepath.Join(r.EvalDir, "results", expName, "artifacts")
	if err := os.MkdirAll(dstDir, 0o755); err != nil {
		return "", err
	}
	dst := filepath.Join(dstDir, fmt.Sprintf("%s-%s-%d.db", trajID, arm, runIndex))
	data, err := os.ReadFile(src)
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(dst, data, 0o644); err != nil {
		return "", err
	}
	return filepath.Rel(r.EvalDir, dst)
}

// appendRecord writes one line to results/<experiment>/<traj>.jsonl.
func (r *Runner) appendRecord(rec RunRecord) error {
	dir := filepath.Join(r.EvalDir, "results", rec.Experiment)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(dir, rec.TrajectoryID+".jsonl"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	data, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	_, err = f.Write(append(data, '\n'))
	return err
}

// LoadRecords reads every run record for a trajectory across all
// experiments — the revision input for bands.json.
func (r *Runner) LoadRecords(trajectoryID string) ([]RunRecord, error) {
	return r.loadRecordsFiltered(func(rec RunRecord) bool { return rec.TrajectoryID == trajectoryID })
}

// LoadExperimentRecords reads one experiment's records.
func (r *Runner) LoadExperimentRecords(expName string) ([]RunRecord, error) {
	return r.loadRecordsFiltered(func(rec RunRecord) bool { return rec.Experiment == expName })
}

func (r *Runner) loadRecordsFiltered(match func(RunRecord) bool) ([]RunRecord, error) {
	root := filepath.Join(r.EvalDir, "results")
	var out []RunRecord
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || filepath.Ext(path) != ".jsonl" {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for line := range strings.Lines(string(data)) {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			var rec RunRecord
			if err := json.Unmarshal([]byte(line), &rec); err != nil {
				continue // Tolerate a torn final line.
			}
			if match(rec) {
				out = append(out, rec)
			}
		}
		return nil
	})
	if os.IsNotExist(err) {
		return nil, nil
	}
	return out, err
}

// RunExperiment executes a paired comparison: frozen bands, per-
// trajectory resampling to N conclusive per arm, arms interleaved in
// time so provider drift lands on both and cancels in the pairing.
// Returns the gate report.
func (r *Runner) RunExperiment(ctx context.Context, exp *Experiment) (Report, error) {
	corpus, err := LoadCorpus(r.EvalDir)
	if err != nil {
		return Report{}, err
	}
	manifest, err := LoadFlagsManifest(r.EvalDir)
	if err != nil {
		return Report{}, err
	}
	if err := manifest.ValidateArmFlags(exp); err != nil {
		return Report{}, err
	}
	bands, err := LoadBands(r.EvalDir)
	if err != nil {
		return Report{}, err
	}
	frozen := deepCopyBands(bands) // Band assignments freeze at experiment start.

	trajs, err := SelectCorpus(corpus, frozen, exp.Corpus)
	if err != nil {
		return Report{}, err
	}

	baselineKey := manifest.BaselineKey(exp.Arms["control"].Config.Options)
	rep := Report{CatastrophicEligible: map[string]bool{}, DiffuseP: 1}

	runnable := 0
	for _, traj := range trajs {
		band := frozen.Band(traj.ID)
		n := exp.RunsPerTrajectory[band]
		if n == 0 {
			continue
		}
		runnable++
		trajDir := filepath.Join(r.EvalDir, "corpus", traj.ID)
		trep := r.runTrajectory(ctx, exp, traj, trajDir, manifest, n)
		rep.Starved = append(rep.Starved, trep.Starved...)
		rep.Saturated = append(rep.Saturated, trep.Saturated...)
		if trep.Skipped != "" {
			rep.Skipped = append(rep.Skipped, fmt.Sprintf("%s: %s", traj.ID, trep.Skipped))
		}
	}
	// Fail closed: a corpus whose every runnable trajectory skipped
	// would otherwise report PASS on zero samples.
	if runnable > 0 && len(rep.Skipped) == runnable {
		return rep, fmt.Errorf("all %d runnable trajectories skipped requires pre-flight: %s",
			runnable, strings.Join(rep.Skipped, "; "))
	}
	// A selection whose bands are all absent from runs_per_trajectory
	// schedules nothing — a config error, not a clean gate.
	if len(trajs) > 0 && runnable == 0 {
		return rep, fmt.Errorf("no selected trajectory's band is covered by runs_per_trajectory")
	}
	// Cancellation aborts cleanly — don't evaluate the gate on a
	// partial record set and print a misleading verdict.
	if err := ctx.Err(); err != nil {
		return rep, err
	}

	// Gate on the frozen snapshot — the current experiment's own
	// control arm is excluded from baselines automatically because
	// bands were frozen before it ran.
	records, err := r.LoadExperimentRecords(exp.Name)
	if err != nil {
		return rep, err
	}
	gate := Evaluate(exp, frozen, baselineKey, records, r.alpha(), r.permReplicates(), r.rng())
	rep.Catastrophic = gate.Catastrophic
	rep.CatastrophicEligible = gate.CatastrophicEligible
	rep.DiffuseP = gate.DiffuseP
	rep.ExcludedDifferential = gate.ExcludedDifferential
	rep.Smoke = gate.Smoke

	// Every paired comparison adds baseline samples as a byproduct —
	// recompute characterization state after gating so the frozen
	// snapshot stays clean for the coincidence-detector role.
	if err := r.RecomputeAll(bands, corpus, manifest, exp.Model); err != nil {
		slog.Warn("Failed to recompute bands", "error", err)
	}
	if err := bands.Save(r.EvalDir); err != nil {
		return rep, fmt.Errorf("save bands.json: %w", err)
	}
	return rep, nil
}

// trajReport carries per-trajectory alarm labels back to the
// experiment summary.
type trajReport struct {
	Starved   []string
	Saturated []string
	Skipped   string
}

// runTrajectory samples one trajectory to N conclusive runs per arm,
// alternating arms each round — per-run interleaving, the finest
// granularity, is what makes within-trajectory runs approximately
// exchangeable for the permutation test.
func (r *Runner) runTrajectory(ctx context.Context, exp *Experiment, traj *Trajectory, trajDir string, manifest *FlagsManifest, n int) trajReport {
	rep := trajReport{}
	if missing := CheckRequires(traj); len(missing) > 0 {
		// Environment pre-flight: a missing tool is a skip-report,
		// not an error outcome masquerading as flakiness.
		rep.Skipped = strings.Join(missing, ",")
		return rep
	}
	armNames := []string{"control", "treatment"}
	maxAttempts := int(float64(n) * r.attemptsFactor())
	conclusive := map[string]int{"control": 0, "treatment": 0}
	attempts := map[string]int{"control": 0, "treatment": 0}
	excluded := map[string]map[Outcome]int{
		"control": {}, "treatment": {},
	}

	for conclusive["control"] < n || conclusive["treatment"] < n {
		if ctx.Err() != nil {
			// Stop cleanly on cancellation — don't materialize and
			// error-append up to ~2N records per remaining trajectory.
			return rep
		}
		progressed := false
		for _, armName := range armNames {
			if conclusive[armName] >= n || attempts[armName] >= maxAttempts {
				continue
			}
			progressed = true
			attempts[armName]++

			rec, err := r.ExecuteRun(ctx, exp, traj, trajDir, armName, exp.Arms[armName], manifest, attempts[armName])
			if err != nil {
				slog.Warn("Run harness failed", "trajectory", traj.ID, "arm", armName, "error", err)
				rec = RunRecord{Experiment: exp.Name, TrajectoryID: traj.ID, Arm: armName, Outcome: OutcomeError}
				rec.RunIndex = attempts[armName]
			}
			if rec.Outcome.Conclusive() {
				conclusive[armName]++
			} else {
				excluded[armName][rec.Outcome]++
			}
			if err := r.appendRecord(rec); err != nil {
				slog.Warn("Failed to append run record", "error", err)
			}
			slog.Info("Eval run", "trajectory", traj.ID, "arm", armName, "outcome", rec.Outcome,
				"conclusive", conclusive[armName], "attempts", attempts[armName])
		}
		if !progressed {
			break
		}
	}

	// Attempts-cap exhaustion is a distinct alarm per excluded class:
	// coverage-starved (inconclusive) vs error-saturated (infra).
	for _, armName := range armNames {
		if conclusive[armName] < n {
			if excluded[armName][OutcomeError] >= excluded[armName][OutcomeInconclusive] {
				rep.Saturated = append(rep.Saturated, fmt.Sprintf("%s/%s", traj.ID, armName))
			} else {
				rep.Starved = append(rep.Starved, fmt.Sprintf("%s/%s", traj.ID, armName))
			}
		}
	}
	return rep
}

// RecomputeAll rebuilds characterization state from accumulated run
// records — bands are revised, never trusted from genesis. model is
// the current pin; the default-condition baseline key comes from the
// manifest. Band assignment and the never_passed/suspect_check scans
// read only that (model, key) pair — a stale model's deep baseline
// must not hold a trajectory stable across a re-pin.
func (r *Runner) RecomputeAll(bands *Bands, corpus map[string]*Trajectory, manifest *FlagsManifest, model string) error {
	curKey := manifest.BaselineKey(nil)
	for id := range corpus {
		trajDir := filepath.Join(r.EvalDir, "corpus", id)
		hash, err := ContentHash(trajDir)
		if err != nil {
			return fmt.Errorf("content hash %s: %w", id, err)
		}
		records, err := r.LoadRecords(id)
		if err != nil {
			return err
		}
		bands.Recompute(id, records, hash, r.now(), model, curKey)
	}
	return nil
}

// Characterize runs genesis/re-characterization: n runs per trajectory
// under the current default condition (empty arm options → the
// manifest's baseline key), then recompute.
func (r *Runner) Characterize(ctx context.Context, model string, temperature *float64, n int, selectors []string) error {
	corpus, err := LoadCorpus(r.EvalDir)
	if err != nil {
		return err
	}
	manifest, err := LoadFlagsManifest(r.EvalDir)
	if err != nil {
		return err
	}
	bands, err := LoadBands(r.EvalDir)
	if err != nil {
		return err
	}
	trajs, err := SelectCorpus(corpus, bands, selectors)
	if err != nil {
		return err
	}
	if n <= 0 {
		n = GenesisRuns
	}
	exp := &Experiment{Name: CharacterizeExperiment, Model: model, Temperature: temperature}
	for _, traj := range trajs {
		if missing := CheckRequires(traj); len(missing) > 0 {
			slog.Warn("Skipping trajectory — unmet requires", "trajectory", traj.ID, "missing", missing)
			continue
		}
		trajDir := filepath.Join(r.EvalDir, "corpus", traj.ID)
		for i := range n {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			rec, err := r.ExecuteRun(ctx, exp, traj, trajDir, "baseline", Arm{}, manifest, i+1)
			if err != nil {
				return fmt.Errorf("characterize %s: %w", traj.ID, err)
			}
			if err := r.appendRecord(rec); err != nil {
				return err
			}
			slog.Info("Characterize run", "trajectory", traj.ID, "outcome", rec.Outcome)
		}
	}
	if err := r.RecomputeAll(bands, corpus, manifest, model); err != nil {
		return err
	}
	return bands.Save(r.EvalDir)
}

// Smoke runs the smoke tier: every stable-band trajectory gets n runs
// under the current default condition, and any strict 0/N result is a
// collapse alarm. Smoke is not a sample for p̂ — its records flow
// through the _characterize pipeline anyway since they're
// baseline-eligible under current defaults.
func (r *Runner) Smoke(ctx context.Context, model string, temperature *float64, n int) ([]string, error) {
	corpus, err := LoadCorpus(r.EvalDir)
	if err != nil {
		return nil, err
	}
	manifest, err := LoadFlagsManifest(r.EvalDir)
	if err != nil {
		return nil, err
	}
	bands, err := LoadBands(r.EvalDir)
	if err != nil {
		return nil, err
	}
	trajs, err := SelectCorpus(corpus, bands, []string{"band:stable"})
	if err != nil {
		return nil, err
	}
	if n <= 0 {
		n = 5
	}
	exp := &Experiment{Name: CharacterizeExperiment, Model: model, Temperature: temperature}
	var alarms []string
	for _, traj := range trajs {
		if missing := CheckRequires(traj); len(missing) > 0 {
			slog.Warn("Skipping trajectory — unmet requires", "trajectory", traj.ID, "missing", missing)
			continue
		}
		trajDir := filepath.Join(r.EvalDir, "corpus", traj.ID)
		passes := 0
		for i := range n {
			if ctx.Err() != nil {
				return alarms, ctx.Err()
			}
			rec, err := r.ExecuteRun(ctx, exp, traj, trajDir, "baseline", Arm{}, manifest, i+1)
			if err != nil {
				return alarms, fmt.Errorf("smoke %s: %w", traj.ID, err)
			}
			if rec.Outcome == OutcomePass {
				passes++
			}
			if err := r.appendRecord(rec); err != nil {
				return alarms, err
			}
		}
		// Strict 0/N — a single failure at p=0.95, N=5 is a 23% false
		// alarm; smoke catches collapses, not drift.
		if passes == 0 {
			alarms = append(alarms, traj.ID)
		}
	}
	if err := r.RecomputeAll(bands, corpus, manifest, model); err != nil {
		return alarms, err
	}
	return alarms, bands.Save(r.EvalDir)
}

// QuarantineCorpus runs the quarantine pass over every corpus
// trajectory and records verdicts into bands.
func (r *Runner) QuarantineCorpus(ctx context.Context, selectors []string) (map[string]QuarantineReason, error) {
	corpus, err := LoadCorpus(r.EvalDir)
	if err != nil {
		return nil, err
	}
	bands, err := LoadBands(r.EvalDir)
	if err != nil {
		return nil, err
	}
	// includeQuarantined: this pass is how they get re-validated.
	trajs, err := selectCorpus(corpus, bands, selectors, true)
	if err != nil {
		return nil, err
	}
	verdicts := map[string]QuarantineReason{}
	for _, traj := range trajs {
		if missing := CheckRequires(traj); len(missing) > 0 {
			slog.Warn("Skipping quarantine — unmet requires", "trajectory", traj.ID, "missing", missing)
			continue
		}
		trajDir := filepath.Join(r.EvalDir, "corpus", traj.ID)
		reason, err := r.Quarantine(ctx, traj, trajDir)
		if err != nil {
			return verdicts, fmt.Errorf("quarantine %s: %w", traj.ID, err)
		}
		verdicts[traj.ID] = reason
		e := bands.Entry(traj.ID)
		if h, err := ContentHash(trajDir); err == nil {
			e.ContentHash = h
		}
		if reason != "" {
			e.Band = BandQuarantined
			e.QuarantineReason = reason
		} else if e.Band == BandQuarantined {
			// Re-validated: back to uncharacterized pending
			// characterization runs.
			e.Band = BandUncharacterized
			e.QuarantineReason = ""
		}
		bands.Put(traj.ID, *e)
	}
	return verdicts, bands.Save(r.EvalDir)
}

// deepCopyBands snapshots band state for the experiment-start freeze.
func deepCopyBands(b *Bands) *Bands {
	out := &Bands{SchemaVersion: b.SchemaVersion, Entries: map[string]BandEntry{}}
	for k, v := range b.Entries {
		cp := v
		if v.Baselines != nil {
			cp.Baselines = map[string]map[string]BaselineCounts{}
			for m, byCfg := range v.Baselines {
				inner := map[string]BaselineCounts{}
				for c, counts := range byCfg {
					inner[c] = counts
				}
				cp.Baselines[m] = inner
			}
		}
		out.Entries[k] = cp
	}
	return out
}
