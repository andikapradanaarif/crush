package eval

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"

	"github.com/google/uuid"
)

// Counterfactual replay (#108): a replay experiment records the
// trajectory once under source_arm — snapshotting workdir + crush.db
// at every turn boundary — then replays each turn under each arm from
// byte-identical state. Pairing on (trajectory, fork_turn, run_index)
// removes everything upstream of the fork from the delta: within-turn
// variance instead of whole-trajectory variance.
//
// Honest limits, declared up front:
//   - Forks share history but NOT provider-side cache state — the
//     recorded run's warm prefix cache doesn't transfer. Cache-cost
//     reads remain valid per fork, but "warm continuation" questions
//     still need end-to-end runs.
//   - The prefix is shaped by source_arm: the estimand is "given
//     control-shaped history, what does turn t do under each arm" —
//     not a symmetric cross-product.
//   - Snapshots are invocation-scoped: a recorded prefix is only
//     valid while this invocation's build is.

// replaySnapMeta is the per-boundary ledger stored beside each
// snapshot — what a fork needs to continue the recorded session and
// what the record carries as prefix provenance.
type replaySnapMeta struct {
	Turn       int        `json:"turn"` // the turn index this boundary precedes
	SessionID  string     `json:"session_id,omitempty"`
	StepsUsed  int        `json:"steps_used"`
	TokensUsed TokenUsage `json:"tokens_used"`
}

// replaySnapRoot locates a trajectory's boundary snapshots under the
// invocation's scratch — <workParent>/.replay/<inv>/<traj>-<hash>/.
// Invocation keying is the drift pin: a stale prefix under a new
// build can never cross into this run's pairs.
func (r *Runner) replaySnapRoot(trajID, contentHash, inv string) string {
	return filepath.Join(r.workParent(), ".replay", inv, trajID+"-"+contentHash)
}

// writeReplaySnapshot captures the current workdir + crush.db as the
// boundary-<turn> state. Same discipline as writeSnapshot: db
// checkpointed (raw fallback on the Windows NOTADB quirk), tmp+rename
// publish so a torn write never reads as a valid fork point.
func (r *Runner) writeReplaySnapshot(ctx context.Context, snapRoot string, turn int, workdir string, meta replaySnapMeta) {
	dir := filepath.Join(snapRoot, fmt.Sprintf("turn-%03d", turn))
	tmp := dir + ".tmp-" + uuid.New().String()
	defer os.RemoveAll(tmp)
	if err := os.MkdirAll(filepath.Join(tmp, "workdir"), 0o755); err != nil {
		slog.Warn("Replay snapshot mkdir failed", "error", err)
		return
	}
	dbBytes, ok := captureDB(ctx, DataDirFor(workdir))
	if !ok {
		return
	}
	if err := os.WriteFile(filepath.Join(tmp, "crush.db"), dbBytes, 0o600); err != nil {
		return
	}
	if err := copyTree(workdir, filepath.Join(tmp, "workdir")); err != nil {
		slog.Warn("Replay snapshot workdir capture failed", "error", err)
		return
	}
	if raw, err := json.Marshal(meta); err == nil {
		_ = os.WriteFile(filepath.Join(tmp, "meta.json"), raw, 0o644)
	}
	_ = os.RemoveAll(dir)
	if err := os.Rename(tmp, dir); err != nil {
		slog.Warn("Replay snapshot publish failed", "error", err)
	}
}

// recordReplayPrefix executes the trajectory's turns under the source
// arm, capturing a boundary snapshot before each turn. The recording
// is scaffolding, not a sample — its telemetry funds the forks'
// prefix ledger and nothing else. A turn that fails ends the
// recording: boundaries already captured remain forkable (the failed
// turn itself is a valid fork — a different arm may clear it).
//
// Returns the boundary metas keyed by turn index and the neutral
// .crush.json bytes captured after WriteSeedConfig — the pre-arm
// config state forks restore before writing their own arm's delta,
// so the source arm's options can't leak into a treatment fork.
func (r *Runner) recordReplayPrefix(ctx context.Context, exp *Experiment, traj *Trajectory, trajDir string, drv AgentRunner, tr TurnRunner, manifest *FlagsManifest, srcName, seedKey, snapRoot string) (map[int]replaySnapMeta, []byte, *WarmStart, error) {
	workdir, err := Materialize(ctx, traj, trajDir, r.workParent(), r.checkEnv())
	if err != nil {
		return nil, nil, nil, fmt.Errorf("materialize: %w", err)
	}
	defer os.RemoveAll(workdir)
	defer os.RemoveAll(DataDirFor(workdir))

	// Seed exactly as a measured run would — same snapshot reuse,
	// same neutral seed config, same gate. The prefix's warm state
	// is what every fork diverges from.
	contentHash := strings.TrimPrefix(seedKey, "eval-")
	snapKey := traj.ID + "-" + contentHash
	seeded := len(traj.PriorSessions) > 0 || len(traj.SeedCommands) > 0
	warm := &WarmStart{}
	restored := seeded && r.restoreSnapshot(snapKey, workdir, warm)
	if err := WriteSeedConfig(workdir, exp, manifest, seedKey); err != nil {
		return nil, nil, nil, fmt.Errorf("seed config: %w", err)
	}
	// The pre-arm config is the neutral base every fork rebuilds
	// from — capture it before WriteArmConfig merges source options.
	neutralCfg, err := os.ReadFile(filepath.Join(workdir, ".crush.json"))
	if err != nil {
		return nil, nil, nil, fmt.Errorf("neutral config capture: %w", err)
	}
	if !restored && len(traj.SeedCommands) > 0 {
		seedIDs, err := r.runScriptedSeeds(ctx, workdir, traj.SeedCommands, seedKey)
		warm.Sessions += len(seedIDs)
		warm.SessionIDs = append(warm.SessionIDs, seedIDs...)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("seed commands: %w", err)
		}
	}
	if !restored {
		for i, ps := range traj.PriorSessions {
			seed := drv.Run(ctx, workdir, ps.Turns, traj.Budget)
			warm.Sessions++
			warm.Steps += seed.Steps
			warm.Tokens.Input += seed.Tokens.Input
			warm.Tokens.Output += seed.Tokens.Output
			warm.Tokens.CacheRead += seed.Tokens.CacheRead
			warm.Tokens.CacheWrite += seed.Tokens.CacheWrite
			if seed.SessionID != "" {
				warm.SessionIDs = append(warm.SessionIDs, seed.SessionID)
			}
			if seed.Err != nil || seed.TimedOut {
				return nil, nil, nil, fmt.Errorf("prior_session %d of %d failed (timeout=%v): %w",
					i+1, len(traj.PriorSessions), seed.TimedOut, seed.Err)
			}
		}
	}
	if seeded && traj.Check.SeedScript != "" {
		schk := runCheckScript(ctx, traj.Check.SeedScript, trajDir, workdir, r.checkEnv(), checkTimeout(traj))
		if schk.Err != nil || schk.Exit != 0 {
			if restored {
				// Same poison-snapshot discipline as ExecuteRun.
				_ = os.RemoveAll(r.snapshotDir(snapKey))
			}
			return nil, nil, nil, fmt.Errorf("seed check failed (exit %d): %s", schk.Exit,
				strings.TrimSpace(schk.Stderr))
		}
	}
	if seeded && !restored {
		r.writeSnapshot(ctx, snapKey, workdir, warm)
	}
	// The source arm's delta lands only after seeding — the recorded
	// prefix is source-arm-shaped history, declared in ReplayMeta.
	if err := WriteArmConfig(workdir, exp, exp.Arms[srcName], manifest, seedKey); err != nil {
		return nil, nil, nil, fmt.Errorf("source arm config: %w", err)
	}

	snaps := map[int]replaySnapMeta{}
	var sessionID string
	stepsUsed := 0
	var tokensUsed TokenUsage
	record := func(turn int) {
		meta := replaySnapMeta{Turn: turn, SessionID: sessionID, StepsUsed: stepsUsed, TokensUsed: tokensUsed}
		r.writeReplaySnapshot(ctx, snapRoot, turn, workdir, meta)
		// Register only a boundary that verifiably landed — a torn
		// write scheduling forks would burn attempts on restore
		// errors.
		if fileExists(filepath.Join(snapRoot, fmt.Sprintf("turn-%03d", turn), "meta.json")) {
			snaps[turn] = meta
		}
	}
	record(0)
	var recErr error
	for t, prompt := range traj.Task.Turns {
		sid, res := tr.RunTurn(ctx, workdir, sessionID, prompt, t, remainingSteps(traj.Budget, stepsUsed))
		if sid != "" {
			sessionID = sid
		}
		stepsUsed += res.Steps
		tokensUsed.Input += res.Tokens.Input
		tokensUsed.Output += res.Tokens.Output
		tokensUsed.CacheRead += res.Tokens.CacheRead
		tokensUsed.CacheWrite += res.Tokens.CacheWrite
		if res.Err != nil {
			recErr = fmt.Errorf("record turn %d: %w", t+1, res.Err)
			break
		}
		if res.TimedOut || (traj.Budget.MaxSteps > 0 && stepsUsed > traj.Budget.MaxSteps) {
			recErr = fmt.Errorf("record turn %d: timed out", t+1)
			break
		}
		// Boundary t+1 precedes the next turn — nothing to fork
		// after the final turn, so don't pay the copy.
		if t+1 < len(traj.Task.Turns) {
			record(t + 1)
		}
	}
	// The recording manifest is the audit trail: which arm shaped
	// the prefix, which session it recorded, and what the prefix
	// cost — the numbers forks inherit but never pay.
	manifestDoc := map[string]any{
		"trajectory":   traj.ID,
		"source_arm":   srcName,
		"session_id":   sessionID,
		"boundaries":   slices.Sorted(maps.Keys(snaps)),
		"prefix_steps": stepsUsed,
		"prefix_tokens": map[string]int64{
			"input": tokensUsed.Input, "output": tokensUsed.Output,
			"cache_read": tokensUsed.CacheRead, "cache_write": tokensUsed.CacheWrite,
		},
		"record_error": fmt.Sprint(recErr),
	}
	if raw, err := json.Marshal(manifestDoc); err == nil {
		_ = os.WriteFile(filepath.Join(snapRoot, "record.json"), raw, 0o644)
	}
	return snaps, neutralCfg, warm, recErr
}

// runReplay schedules a replay experiment's trajectory: record the
// prefix once, then fill each (fork, arm) cell to n conclusive
// replicates under the same round-robin alternation the normal
// scheduler uses — provider drift must not systematically hand one
// arm the later samples within a cell.
func (r *Runner) runReplay(ctx context.Context, exp *Experiment, traj *Trajectory, trajDir string, manifest *FlagsManifest, n int, inv string, configErrs *configErrorTracker) trajReport {
	rep := trajReport{}
	drv := r.driver()
	tr, ok := drv.(TurnRunner)
	if !ok {
		rep.Skipped = "driver cannot replay single turns (no TurnRunner)"
		return rep
	}
	if len(traj.Task.Turns) == 0 {
		rep.Skipped = "replay: no turns"
		return rep
	}
	if cr, ok := drv.(CrushRunner); ok {
		cr.FlagKeys = flagNames(manifest)
		drv = cr
		tr = cr
	}

	srcName := exp.Replay.SourceArm
	if srcName == "" {
		srcName = ArmControl
	}

	contentHash, err := ContentHash(trajDir, trajContentRefs(trajDir, traj)...)
	if err != nil {
		rep.Abort = fmt.Errorf("content hash: %w", err)
		return rep
	}
	seedKey := "eval-" + contentHash
	snapRoot := r.replaySnapRoot(traj.ID, contentHash, inv)
	// Snapshots outlive the trajectory's schedule deliberately:
	// they're the forensic artifact a surprising fork record points
	// back to, and workParent's temp lifetime bounds the disk.

	snaps, neutralCfg, warm, recErr := r.recordReplayPrefix(ctx, exp, traj, trajDir, drv, tr, manifest, srcName, seedKey, snapRoot)
	// Only declared boundaries count — fork_turns is the authored
	// subset, and an unrecorded boundary can't fork at all.
	forks := make([]int, 0, len(snaps))
	for t := range snaps {
		forks = append(forks, t)
	}
	if sel := exp.Replay.ForkTurns; len(sel) > 0 {
		want := map[int]bool{}
		for _, t := range sel {
			want[t] = true
		}
		forks = slices.DeleteFunc(forks, func(t int) bool { return !want[t] })
	}
	slices.Sort(forks)
	if len(forks) == 0 {
		// No boundary means nothing pairs — surface the recording
		// failure as the reason rather than an empty schedule.
		if recErr != nil {
			rep.Abort = fmt.Errorf("replay record: %w", recErr)
		} else {
			rep.Skipped = "replay: no fork boundaries recorded"
		}
		return rep
	}
	if recErr != nil {
		// Partial prefix — the recorded turn died mid-trajectory.
		// Forks before it still pair honestly; the alarm says the
		// tail of the trajectory was never reachable.
		rep.Skipped = fmt.Sprintf("replay record partial: %v", recErr)
	}

	type cell struct {
		fork int
		arm  string
	}
	armNames := []string{ArmControl, ArmTreatment}
	cells := make([]cell, 0, len(forks)*len(armNames))
	for _, f := range forks {
		for _, a := range armNames {
			cells = append(cells, cell{f, a})
		}
	}
	maxAttempts := int(float64(n) * r.attemptsFactor())
	conclusive := map[cell]int{}
	attempts := map[cell]int{}
	excluded := map[cell]map[Outcome]int{}
	excludedClass := map[cell]map[string]int{}
	label := func(c cell) string { return fmt.Sprintf("%s/f%d/%s", traj.ID, c.fork, c.arm) }
	done := func() bool {
		for _, c := range cells {
			if conclusive[c] < n && attempts[c] < maxAttempts {
				return false
			}
		}
		return true
	}
	for round := 0; !done(); round++ {
		if ctx.Err() != nil {
			return rep
		}
		// Rotate the lead cell per round — same drift-neutrality as
		// the normal scheduler's arm alternation.
		order := append(slices.Clone(cells[round%len(cells):]), cells[:round%len(cells)]...)
		progressed := false
		for _, c := range order {
			if conclusive[c] >= n || attempts[c] >= maxAttempts {
				continue
			}
			progressed = true
			attempts[c]++
			rec, err := r.executeReplayRun(ctx, exp, traj, trajDir, c.arm, exp.Arms[c.arm],
				manifest, inv, snapRoot, neutralCfg, snaps[c.fork], c.fork, attempts[c], tr, srcName, seedKey, warm)
			if err != nil {
				slog.Warn("Replay run harness failed", "trajectory", traj.ID, "fork", c.fork, "arm", c.arm, "error", err)
				rec = RunRecord{
					Experiment: exp.Name, TrajectoryID: traj.ID, Arm: c.arm, Invocation: inv,
					RunIndex: attempts[c], Outcome: OutcomeError,
					Replay: &ReplayMeta{ForkTurn: c.fork, SourceArm: srcName},
				}
			}
			if err := r.appendRecord(rec); err != nil {
				slog.Warn("Failed to append run record", "error", err)
				if excluded[c] == nil {
					excluded[c] = map[Outcome]int{}
				}
				excluded[c][OutcomeError]++
				continue
			}
			if rec.Outcome.Conclusive() {
				conclusive[c]++
			} else {
				if excluded[c] == nil {
					excluded[c] = map[Outcome]int{}
				}
				excluded[c][rec.Outcome]++
				if rec.Outcome == OutcomeError && rec.ErrorClass != "" {
					if excludedClass[c] == nil {
						excludedClass[c] = map[string]int{}
					}
					excludedClass[c][rec.ErrorClass]++
				}
			}
			if isConfigClassError(rec) {
				configErrs.n++
				if configErrs.first == "" {
					if s, ok := rec.CheckDetail["run_error"].(string); ok && s != "" {
						configErrs.first = s
					} else {
						configErrs.first = rec.ErrorClass
					}
				}
				if configErrs.n >= 2 {
					rep.Abort = fmt.Errorf("config-class failure after %d runs: %s", configErrs.n, configErrs.first)
					return rep
				}
			}
			slog.Info("Eval replay run", "trajectory", traj.ID, "fork", c.fork, "arm", c.arm,
				"outcome", rec.Outcome, "conclusive", conclusive[c], "attempts", attempts[c])
		}
		if !progressed {
			break
		}
	}
	for _, c := range cells {
		if conclusive[c] < n {
			if excluded[c][OutcomeError] >= excluded[c][OutcomeInconclusive] {
				rep.Saturated = append(rep.Saturated, label(c))
			} else {
				rep.Starved = append(rep.Starved, label(c))
			}
		}
	}
	return rep
}

// executeReplayRun replays trajectory turn fork from the recorded
// boundary under the fork arm — one turn, one record.
func (r *Runner) executeReplayRun(ctx context.Context, exp *Experiment, traj *Trajectory, trajDir, armName string, arm Arm, manifest *FlagsManifest, inv, snapRoot string, neutralCfg []byte, meta replaySnapMeta, fork, attempt int, tr TurnRunner, srcName, seedKey string, warm *WarmStart) (RunRecord, error) {
	rec := RunRecord{
		Experiment:   exp.Name,
		Invocation:   inv,
		TrajectoryID: traj.ID,
		Arm:          armName,
		RunIndex:     attempt,
		StartedAt:    r.now(),
		Env: Env{
			CrushSHA:    crushSHA(),
			Go:          goToolchain(),
			OS:          runtime.GOOS,
			ContentHash: strings.TrimPrefix(seedKey, "eval-"),
			Temperature: temperatureKey(exp.Temperature),
			ModelPin:    exp.Model,
		},
		Replay: &ReplayMeta{
			ForkTurn:     fork,
			SourceArm:    srcName,
			PrefixSteps:  meta.StepsUsed,
			PrefixTokens: meta.TokensUsed,
		},
	}
	rec.BaselineKey = manifest.keyWith(arm.Config.Options,
		map[string]any{"$temperature": rec.Env.Temperature})
	// Warm provenance rides the fork — the restored db really does
	// contain those seed sessions. Spend fields stay zero: the
	// recording pass paid them once ("seeded once, measured N").
	if warm != nil && warm.Sessions > 0 {
		rec.WarmStart = &WarmStart{Sessions: warm.Sessions, SessionIDs: warm.SessionIDs}
	}

	workdir, err := Materialize(ctx, traj, trajDir, r.workParent(), r.checkEnv())
	if err != nil {
		rec.Outcome = OutcomeError
		rec.CheckDetail = map[string]any{"harness": "materialize: " + err.Error()}
		return rec, nil
	}
	defer os.RemoveAll(workdir)
	defer os.RemoveAll(DataDirFor(workdir))

	snapDir := filepath.Join(snapRoot, fmt.Sprintf("turn-%03d", fork))
	if err := overlaySnapshot(snapDir, workdir); err != nil {
		rec.Outcome = OutcomeError
		rec.CheckDetail = map[string]any{"harness": "snapshot restore: " + err.Error()}
		return rec, nil
	}
	// Config isolation: the restored .crush.json carries the source
	// arm's merge — restore the pre-arm neutral file first so no
	// source option survives into this fork's config. WriteArmConfig
	// then merges the fork's delta plus a fresh data_directory.
	if len(neutralCfg) > 0 {
		if err := os.WriteFile(filepath.Join(workdir, ".crush.json"), neutralCfg, 0o644); err != nil {
			rec.Outcome = OutcomeError
			rec.CheckDetail = map[string]any{"harness": "neutral config restore: " + err.Error()}
			return rec, nil
		}
	}
	if err := WriteArmConfig(workdir, exp, arm, manifest, seedKey); err != nil {
		rec.Outcome = OutcomeError
		rec.CheckDetail = map[string]any{"harness": err.Error()}
		return rec, nil
	}

	sid, res := tr.RunTurn(ctx, workdir, meta.SessionID, traj.Task.Turns[fork], fork, remainingSteps(traj.Budget, meta.StepsUsed))
	rec.DurationS = r.now().Sub(rec.StartedAt).Seconds()
	rec.SessionID = sid
	rec.Steps = res.Steps
	rec.Tokens = res.Tokens
	rec.StubStats = res.StubStats
	rec.PriorTurns = res.PriorTurns
	rec.Recalls = res.Recalls
	rec.Checkpoints = res.Checkpoints
	rec.Digests = res.Digests
	rec.Hydration = res.Hydration
	rec.EdgeFirings = res.EdgeFirings
	rec.PromptTokensPerTurn = res.PromptTokensPerTurn
	rec.StepRecords = res.StepRecords
	rec.Tail = res.Tail
	rec.TailRuns = res.TailRuns
	if res.Pressure.Estimate > 0 || res.Pressure.Engaged || res.Pressure.Activations > 0 {
		rec.Pressure = &res.Pressure
	}
	rec.ErrorClass = res.ErrorClass
	rec.ParamVersion = res.ParamVersion
	if res.GeneratorTokens.Calls > 0 {
		rec.GeneratorTokens = &res.GeneratorTokens
	}
	if res.Request.PromptRequests > 0 {
		rec.Request = &res.Request
	}
	rec.Env.ModelResolved = res.ModelResolved
	if len(res.ResolvedOptions) > 0 {
		rec.ResolvedOptions = res.ResolvedOptions
		rec.BaselineKey = manifest.keyWith(res.ResolvedOptions,
			map[string]any{"$temperature": rec.Env.Temperature})
	}
	rec.Env.ModelSmall = res.ModelSmall
	rec.Env.ModelSummary = res.ModelSummary

	// The preserved db contains recorded turns 0..fork — the prefix
	// plus this fork's replay — so turn attribution spans that slice.
	r.preserveArtifacts(ctx, &rec, exp.Name, traj.ID, armName, inv, attempt, workdir, sid, traj.Task.Turns[:fork+1])

	switch {
	case res.Err != nil:
		rec.Outcome = OutcomeError
		rec.CheckDetail = map[string]any{"run_error": res.Err.Error()}
		return rec, nil
	case res.TimedOut || (traj.Budget.MaxSteps > 0 && meta.StepsUsed+res.Steps > traj.Budget.MaxSteps):
		rec.Outcome = OutcomeTimeout
		return rec, nil
	}

	// Only the terminal fork produces an end state worth checking —
	// mid-trajectory checks assert final conditions a prefix
	// can't satisfy, so earlier forks gate on coverage alone.
	if fork == len(traj.Task.Turns)-1 {
		chk := RunCheck(ctx, traj, trajDir, workdir, r.checkEnv())
		rec.CheckStdout = string(tail([]byte(chk.Stdout), 4096))
		rec.CheckStderr = string(tail([]byte(chk.Stderr), 4096))
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
	}
	met, failedKey, err := coverageMet(traj.Coverage, &rec, coverageFields)
	if err != nil {
		return rec, fmt.Errorf("coverage eval: %w", err)
	}
	scope := ""
	if !met {
		scope = "trajectory"
	} else if armMet, akey, aerr := coverageMet(arm.Coverage, &rec, armFields); aerr != nil {
		return rec, fmt.Errorf("arm coverage eval: %w", aerr)
	} else if !armMet {
		scope, failedKey = "arm", akey
		met = false
	}
	if !met {
		if rec.CheckDetail == nil {
			rec.CheckDetail = map[string]any{}
		}
		rec.CheckDetail["coverage_scope"] = scope
		rec.CheckDetail["coverage_key"] = failedKey
		rec.Outcome = OutcomeInconclusive
		return rec, nil
	}
	rec.Outcome = OutcomePass
	return rec, nil
}
