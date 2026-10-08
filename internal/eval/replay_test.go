package eval

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"math/rand/v2"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/charmbracelet/crush/internal/db"
	"github.com/stretchr/testify/require"
)

// replayCall is what one RunTurn observed — the probes a fork needs:
// which session it resumed, which config was live, and hashes of the
// workdir (excluding the per-arm config writes) and the session db.
type replayCall struct {
	turnIdx   int
	sessionIn string
	prompt    string
	opts      map[string]any
	workHash  string
	dbHash    string
}

// replayDriver fakes AgentRunner (for agent seeds) and TurnRunner (the
// replay contract). Each turn appends a marker file and a db row so
// snapshots carry accumulating, distinguishable state; fork-time
// probes hash what the turn actually saw.
type replayDriver struct {
	mu      sync.Mutex
	calls   []replayCall
	session string
	// failTurn errors once at that turn index. The recording pass
	// runs before any fork, so the single failure lands on the
	// recording — fork replays of that same turn still succeed.
	failTurn int
	failed   bool
	// boomKey names the arm option that makes every fork erroring
	// when true in the fork's live config.
	boomKey string
	steps   int
}

func newReplayDriver() *replayDriver {
	return &replayDriver{failTurn: -1, session: "rec-session", steps: 2}
}

func (d *replayDriver) Run(_ context.Context, _ string, turns []string, _ Budget) RunResult {
	// Agent seeds run whole turn lists in-process — no snapshot
	// probes needed on the seed path.
	return RunResult{Steps: len(turns) * d.steps, SessionID: "seed-" + d.session}
}

// hashWorkdir hashes sorted relative paths + contents, skipping the
// harness config files — .crush.json/.crushrc legitimately differ per
// fork arm; everything else must be byte-identical across arms.
func hashWorkdir(workdir string) string {
	h := sha256.New()
	_ = filepath.Walk(workdir, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(workdir, p)
		switch filepath.Base(rel) {
		case ".crush.json", ".crushrc":
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		fmt.Fprintf(h, "%s\x00%x\x00", rel, b)
		return nil
	})
	return hex.EncodeToString(h.Sum(nil))
}

// touchDB ensures DataDirFor(workdir)/crush.db exists and carries one
// marker row attributed to this call — real db state the snapshot's
// byte copy must preserve.
func touchDB(ctx context.Context, workdir, id string) error {
	dataDir := DataDirFor(workdir)
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return err
	}
	conn, err := db.Connect(ctx, dataDir)
	if err != nil {
		return err
	}
	if _, err = conn.ExecContext(ctx,
		`INSERT OR IGNORE INTO sessions (id, parent_session_id, title, created_at, updated_at)
		 VALUES ('rec-session', NULL, 't', 1000, 1000)`); err == nil {
		_, err = conn.ExecContext(ctx,
			`INSERT INTO messages (id, session_id, role, parts, created_at, updated_at, is_summary_message)
			 VALUES (?, 'rec-session', 'assistant', ?, 1000, 1000, 0)`, id, `[]`)
	}
	if relErr := db.Release(dataDir); err == nil {
		err = relErr
	}
	return err
}

func (d *replayDriver) RunTurn(ctx context.Context, workdir, sessionID, prompt string, turnIdx, _ int) (string, RunResult) {
	d.mu.Lock()
	defer d.mu.Unlock()

	call := replayCall{turnIdx: turnIdx, sessionIn: sessionID, prompt: prompt}
	if raw, err := os.ReadFile(filepath.Join(workdir, ".crush.json")); err == nil {
		var doc struct {
			Options map[string]any `json:"options"`
		}
		if json.Unmarshal(raw, &doc) == nil {
			call.opts = maps.Clone(doc.Options)
		}
	}
	// Hash the restored db bytes BEFORE connecting — open mutates the
	// file header even on a read.
	if raw, err := os.ReadFile(filepath.Join(DataDirFor(workdir), "crush.db")); err == nil {
		sum := sha256.Sum256(raw)
		call.dbHash = hex.EncodeToString(sum[:])
	}
	// Unique per call — a fork replaying a recorded turn inserts a
	// fresh row into the restored copy, never colliding with the
	// recording's marker.
	if err := touchDB(ctx, workdir, fmt.Sprintf("t%d-c%d", turnIdx, len(d.calls))); err != nil {
		d.calls = append(d.calls, call)
		return sessionID, RunResult{Err: err}
	}
	// A marker file accumulates across the recorded turns — the
	// workdir half of "identical history".
	_ = os.WriteFile(filepath.Join(workdir, fmt.Sprintf("turn-%02d.marker", turnIdx)), []byte(prompt), 0o644)
	call.workHash = hashWorkdir(workdir)
	d.calls = append(d.calls, call)

	res := RunResult{Steps: d.steps, SessionID: d.session,
		Tokens:          TokenUsage{Input: 10, Output: 5},
		ModelResolved:   "mock/m",
		ResolvedOptions: map[string]any{}}
	if d.boomKey != "" && call.opts[d.boomKey] == true {
		res.Err = fmt.Errorf("boom: %s armed", d.boomKey)
		res.ErrorClass = "fixture_config"
	}
	if turnIdx == d.failTurn && !d.failed {
		d.failed = true
		res.Err = fmt.Errorf("record died at turn %d", turnIdx)
	}
	return d.session, res
}

func replayFixture(t *testing.T, turns ...string) (root, trajDir string, traj *Trajectory) {
	t.Helper()
	requireSeedCheckTooling(t) // Terminal forks exec the check script.
	root = newEvalDir(t)
	trajDir = writeTrajectory(t, filepath.Join(root, "corpus"), "replay-t", map[string]any{
		"task":              map[string]any{"turns": turns},
		"check_script_body": "#!/bin/bash\necho 'EVAL_JSON {\"end_state\":\"checked\"}'\nexit 0\n",
	})
	traj, err := LoadTrajectory(trajDir)
	require.NoError(t, err)
	return root, trajDir, traj
}

func replayExperiment(srcArm string, forkTurns []int) *Experiment {
	return &Experiment{
		Name: "replay-exp", Model: "mock/m", Temperature: ptr(0.0),
		Arms: map[string]Arm{
			ArmControl:   {Config: ArmConfig{Options: map[string]any{"debug": true}}},
			ArmTreatment: {Config: ArmConfig{Options: map[string]any{"debug": false}}},
		},
		Replay: &ReplaySpec{SourceArm: srcArm, ForkTurns: forkTurns},
	}
}

// The core machinery claim: a 3-turn trajectory with n=2 yields
// 3 forks × 2 arms × 2 reps = 12 records, each a single replayed
// turn carrying fork provenance, and every fork ≥1 resumed the
// recorded session while fork 0 started fresh.
func TestRunReplay_RecordsPairPerFork(t *testing.T) {
	t.Parallel()
	root, trajDir, traj := replayFixture(t, "alpha", "bravo", "czech")
	drv := newReplayDriver()
	r := &Runner{EvalDir: root, Driver: drv, WorkParent: t.TempDir(),
		RNG: rand.New(rand.NewPCG(1, 2))}
	exp := replayExperiment("", nil)
	manifest := &FlagsManifest{Defaults: map[string]any{"debug": false}}

	rep := r.runReplay(context.Background(), exp, traj, trajDir, manifest, 2, "inv1", &configErrorTracker{})
	require.Nil(t, rep.Abort)
	require.Empty(t, rep.Skipped)

	recs, err := r.LoadExperimentRecords("replay-exp")
	require.NoError(t, err)
	require.Len(t, recs, 12, "3 forks × 2 arms × 2 reps")
	for _, rec := range recs {
		require.Equal(t, OutcomePass, rec.Outcome, "%+v", rec.CheckDetail)
		require.NotNil(t, rec.Replay)
		require.Contains(t, []int{0, 1, 2}, rec.Replay.ForkTurn)
		require.Equal(t, ArmControl, rec.Replay.SourceArm)
		require.Equal(t, "rec-session", rec.SessionID)
		// Only the terminal fork earns the end-state check —
		// mid-trajectory asserts would judge a prefix.
		if rec.Replay.ForkTurn == 2 {
			require.Equal(t, "checked", rec.CheckDetail["end_state"],
				"terminal fork ran the trajectory check")
		} else {
			require.Empty(t, rec.CheckDetail["end_state"])
		}
		// Prefix provenance lands without charging the fork.
		require.Equal(t, rec.Replay.ForkTurn*2, rec.Replay.PrefixSteps)
		require.Equal(t, 2, rec.Steps, "record measures the forked turn only")
	}
	// The first T calls are the recording pass; the rest are forks.
	require.Len(t, drv.calls, 15)
	recCalls, forkCalls := drv.calls[:3], drv.calls[3:]
	for i, c := range recCalls {
		require.Equal(t, i, c.turnIdx)
	}
	for _, c := range forkCalls {
		if c.turnIdx == 0 {
			require.Empty(t, c.sessionIn, "turn-0 fork has no session to resume")
		} else {
			require.Equal(t, "rec-session", c.sessionIn)
		}
	}
}

// Byte-identical history: the two arms' forks at the same boundary
// must observe identical workdir bytes (modulo config writes) and
// identical crush.db bytes — the paired-comparison premise.
func TestRunReplay_IdenticalHistoryAcrossArms(t *testing.T) {
	t.Parallel()
	root, trajDir, traj := replayFixture(t, "alpha", "bravo", "czech")
	drv := newReplayDriver()
	r := &Runner{EvalDir: root, Driver: drv, WorkParent: t.TempDir(),
		RNG: rand.New(rand.NewPCG(3, 4))}
	exp := replayExperiment("", nil)
	manifest := &FlagsManifest{Defaults: map[string]any{"debug": false}}

	rep := r.runReplay(context.Background(), exp, traj, trajDir, manifest, 2, "inv1", &configErrorTracker{})
	require.Nil(t, rep.Abort)

	byFork := map[int][]replayCall{}
	for _, c := range drv.calls[3:] {
		byFork[c.turnIdx] = append(byFork[c.turnIdx], c)
	}
	for fork, calls := range byFork {
		require.Len(t, calls, 4, "2 arms × 2 reps at fork %d", fork)
		for i := 1; i < len(calls); i++ {
			require.Equal(t, calls[0].workHash, calls[i].workHash,
				"fork %d workdir diverged between arms/reps", fork)
			require.Equal(t, calls[0].dbHash, calls[i].dbHash,
				"fork %d crush.db diverged between arms/reps", fork)
		}
	}
}

// Config isolation: the snapshot's .crush.json carries the source
// arm's merge — forks must rebuild from the neutral base so no
// source-arm option leaks into a treatment fork's resolved config.
func TestRunReplay_ConfigIsolation(t *testing.T) {
	t.Parallel()
	root, trajDir, traj := replayFixture(t, "alpha", "bravo")
	drv := newReplayDriver()
	r := &Runner{EvalDir: root, Driver: drv, WorkParent: t.TempDir(),
		RNG: rand.New(rand.NewPCG(5, 6))}
	exp := &Experiment{
		Name: "replay-exp", Model: "mock/m", Temperature: ptr(0.0),
		Arms: map[string]Arm{
			ArmControl:   {Config: ArmConfig{Options: map[string]any{"src_only": "leak-me", "debug": true}}},
			ArmTreatment: {Config: ArmConfig{Options: map[string]any{"debug": false}}},
		},
		Replay: &ReplaySpec{SourceArm: ArmControl},
	}
	manifest := &FlagsManifest{Defaults: map[string]any{"debug": false, "src_only": ""}}

	rep := r.runReplay(context.Background(), exp, traj, trajDir, manifest, 1, "inv1", &configErrorTracker{})
	require.Nil(t, rep.Abort)

	var sawControl, sawTreatment bool
	for _, c := range drv.calls[2:] { // Fork calls only.
		if c.opts["debug"] == true {
			sawControl = true
			require.Equal(t, "leak-me", c.opts["src_only"])
		} else {
			sawTreatment = true
			_, leaked := c.opts["src_only"]
			require.False(t, leaked, "treatment fork saw source-arm option src_only")
			require.Equal(t, false, c.opts["debug"])
		}
	}
	require.True(t, sawControl && sawTreatment)
}

// A recording that dies mid-trajectory still yields the boundaries it
// completed — forks before the failure pair honestly, and the report
// says the tail was never reachable.
func TestRunReplay_PartialRecording(t *testing.T) {
	t.Parallel()
	root, trajDir, traj := replayFixture(t, "alpha", "bravo", "czech", "delta")
	drv := newReplayDriver()
	drv.failTurn = 2
	r := &Runner{EvalDir: root, Driver: drv, WorkParent: t.TempDir(),
		RNG: rand.New(rand.NewPCG(7, 8))}
	exp := replayExperiment("", nil)
	manifest := &FlagsManifest{Defaults: map[string]any{"debug": false}}

	rep := r.runReplay(context.Background(), exp, traj, trajDir, manifest, 1, "inv1", &configErrorTracker{})
	require.Nil(t, rep.Abort)
	require.Contains(t, rep.Skipped, "partial")

	recs, err := r.LoadExperimentRecords("replay-exp")
	require.NoError(t, err)
	// Boundaries 0,1,2 recorded — the failed turn's own boundary is
	// still forkable (a different arm may clear what the source arm
	// could not), but boundary 3 never existed.
	require.Len(t, recs, 6, "3 forks × 2 arms × 1 rep")
	forks := map[int]bool{}
	for _, rec := range recs {
		forks[rec.Replay.ForkTurn] = true
	}
	require.Equal(t, map[int]bool{0: true, 1: true, 2: true}, forks)
}

// A fork's own failure is an arm-level data point, not a harness
// collapse: the boom arm dies, the other arm completes its cells.
func TestRunReplay_ForkErrorIsArmData(t *testing.T) {
	t.Parallel()
	root, trajDir, traj := replayFixture(t, "alpha", "bravo")
	drv := newReplayDriver()
	drv.boomKey = "boom"
	r := &Runner{EvalDir: root, Driver: drv, WorkParent: t.TempDir(),
		RNG: rand.New(rand.NewPCG(9, 10))}
	exp := &Experiment{
		Name: "replay-exp", Model: "mock/m", Temperature: ptr(0.0),
		Arms: map[string]Arm{
			ArmControl:   {},
			ArmTreatment: {Config: ArmConfig{Options: map[string]any{"boom": true}}},
		},
		Replay: &ReplaySpec{},
	}
	manifest := &FlagsManifest{Defaults: map[string]any{"boom": false}}

	rep := r.runReplay(context.Background(), exp, traj, trajDir, manifest, 1, "inv1", &configErrorTracker{})
	require.Nil(t, rep.Abort)
	recs, err := r.LoadExperimentRecords("replay-exp")
	require.NoError(t, err)
	var ctrl, treat int
	for _, rec := range recs {
		if rec.Arm == ArmControl {
			require.Equal(t, OutcomePass, rec.Outcome)
			ctrl++
		} else {
			require.Equal(t, OutcomeError, rec.Outcome)
			treat++
		}
	}
	require.Equal(t, 2, ctrl)
	// n=1 with the default 2× attempts factor: each (fork, treatment)
	// cell exhausts 2 attempts → 4 error records before saturating.
	require.Equal(t, 4, treat)
	require.Len(t, rep.Saturated, 2, "both treatment cells saturate")
}

// Pairing granularity: control fork-1 rep-1 must not pair with
// treatment fork-0 rep-1 — per-fork deltas of +50% everywhere produce
// theta exactly +50% only when the fork coordinate partitions pairs.
func TestCompare_PairsWithinFork(t *testing.T) {
	t.Parallel()
	root := newEvalDir(t)
	r := &Runner{EvalDir: root}
	exp := &Experiment{Name: "pair-exp", Model: "mock/m", Temperature: ptr(0.0),
		Arms: map[string]Arm{ArmControl: {}, ArmTreatment: {}}}
	for _, fork := range []int{0, 1, 2} {
		csteps := (fork + 1) * 10
		tsteps := csteps * 3 / 2 // Uniform +50% — only correct pairing yields theta 0.5.
		for _, kv := range []struct {
			arm   string
			steps int
		}{{ArmControl, csteps}, {ArmTreatment, tsteps}} {
			rec := RunRecord{
				Experiment: "pair-exp", TrajectoryID: "tr", Arm: kv.arm,
				Invocation: "inv", RunIndex: 1, Outcome: OutcomePass,
				Steps:  kv.steps,
				Replay: &ReplayMeta{ForkTurn: fork, SourceArm: ArmControl},
			}
			require.NoError(t, r.appendRecord(rec))
		}
	}
	rep, err := r.Compare(exp, "inv")
	require.NoError(t, err)
	require.Equal(t, 3, rep.Pairs)
	var stepsMC *MetricCompare
	for i := range rep.Metrics {
		if rep.Metrics[i].Name == "steps" {
			stepsMC = &rep.Metrics[i]
		}
	}
	require.NotNil(t, stepsMC)
	require.InDelta(t, 50.0, stepsMC.DeltaPct, 0.01,
		"mispaired forks would smear the uniform +50% delta")
}

// fork_turns restricts which boundaries fork — the recorded prefix
// still covers the full trajectory, but only listed turns schedule.
func TestRunReplay_ForkTurnsSubset(t *testing.T) {
	t.Parallel()
	root, trajDir, traj := replayFixture(t, "alpha", "bravo", "czech")
	drv := newReplayDriver()
	r := &Runner{EvalDir: root, Driver: drv, WorkParent: t.TempDir(),
		RNG: rand.New(rand.NewPCG(11, 12))}
	exp := replayExperiment("", []int{2})
	manifest := &FlagsManifest{Defaults: map[string]any{"debug": false}}

	rep := r.runReplay(context.Background(), exp, traj, trajDir, manifest, 1, "inv1", &configErrorTracker{})
	require.Nil(t, rep.Abort)
	recs, err := r.LoadExperimentRecords("replay-exp")
	require.NoError(t, err)
	require.Len(t, recs, 2)
	for _, rec := range recs {
		require.Equal(t, 2, rec.Replay.ForkTurn)
	}
}

// A driver without TurnRunner skips cleanly rather than erroring the
// experiment.
func TestRunReplay_DriverWithoutTurnRunner(t *testing.T) {
	t.Parallel()
	root, trajDir, traj := replayFixture(t, "a", "b")
	r := &Runner{EvalDir: root, Driver: fakeRunner{}, WorkParent: t.TempDir(),
		RNG: rand.New(rand.NewPCG(13, 14))}
	exp := replayExperiment("", nil)
	manifest := &FlagsManifest{Defaults: map[string]any{}}
	rep := r.runReplay(context.Background(), exp, traj, trajDir, manifest, 1, "inv1", &configErrorTracker{})
	require.Contains(t, rep.Skipped, "TurnRunner")
}

// ValidateExperiment: replay source_arm must name a declared arm;
// fork_turns rejects negatives and dupes.
func TestValidateExperiment_Replay(t *testing.T) {
	t.Parallel()
	base := func() *Experiment {
		return &Experiment{
			Name: "e", Model: "mock/m", Temperature: ptr(0.0),
			Corpus:            []string{"*"},
			RunsPerTrajectory: map[Band]int{BandStable: 1},
			Arms:              map[string]Arm{ArmControl: {}, ArmTreatment: {}},
			Replay:            &ReplaySpec{},
		}
	}
	require.NoError(t, ValidateExperiment(base()))

	e := base()
	e.Replay.SourceArm = "nope"
	require.ErrorContains(t, ValidateExperiment(e), "source_arm")

	e = base()
	e.Replay.ForkTurns = []int{-1}
	require.ErrorContains(t, ValidateExperiment(e), "fork_turns")

	e = base()
	e.Replay.ForkTurns = []int{2, 2}
	require.ErrorContains(t, ValidateExperiment(e), "twice")
}
