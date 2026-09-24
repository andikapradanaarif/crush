package eval

import (
	"encoding/json"
	"math"
	"math/rand/v2"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// compareRecord builds a minimal conclusive record for compare
// tests — tokens/steps are the metric payload.
func compareRecord(exp, traj, arm, inv string, idx int, input int, resolved map[string]any) RunRecord {
	return RunRecord{
		Experiment:      exp,
		TrajectoryID:    traj,
		Arm:             arm,
		Invocation:      inv,
		RunIndex:        idx,
		Outcome:         OutcomePass,
		Steps:           10,
		Tokens:          TokenUsage{Input: int64(input), Output: 100},
		ResolvedOptions: resolved,
	}
}

// writeCompareRecords appends records to eval-dir results files the
// same way appendRecord does.
func writeCompareRecords(t *testing.T, evalDir string, recs ...RunRecord) {
	t.Helper()
	for _, rec := range recs {
		dir := filepath.Join(evalDir, "results", rec.Experiment)
		require.NoError(t, os.MkdirAll(dir, 0o755))
		f, err := os.OpenFile(filepath.Join(dir, rec.TrajectoryID+".jsonl"),
			os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		require.NoError(t, err)
		data, err := json.Marshal(rec)
		require.NoError(t, err)
		_, err = f.Write(append(data, '\n'))
		require.NoError(t, err)
		require.NoError(t, f.Close())
	}
}

func seededRunner(dir string) *Runner {
	return &Runner{EvalDir: dir, RNG: rand.New(rand.NewPCG(1, 2)), PermReplicates: 2000}
}

func TestCompare_PairsByRunIndex(t *testing.T) {
	dir := t.TempDir()
	exp := &Experiment{Name: "e", Corpus: []string{"t1"}}
	optsA := map[string]any{"flag": "a"}
	optsB := map[string]any{"flag": "b"}
	var recs []RunRecord
	for i := 1; i <= 6; i++ {
		recs = append(recs, compareRecord("e", "t1", ArmControl, "inv1", i, 100, optsA))
	}
	// Treatment: idx 6 went inconclusive, resampled as idx 7 — the
	// scheduler's sparse-index signature. Conclusive counts stay
	// equal (6=6, no starve) but the pair sets diverge.
	for i := 1; i <= 5; i++ {
		recs = append(recs, compareRecord("e", "t1", ArmTreatment, "inv1", i, 80, optsB))
	}
	inc := compareRecord("e", "t1", ArmTreatment, "inv1", 6, 80, optsB)
	inc.Outcome = OutcomeInconclusive
	recs = append(recs, inc,
		compareRecord("e", "t1", ArmTreatment, "inv1", 7, 80, optsB))
	writeCompareRecords(t, dir, recs...)

	rep, err := seededRunner(dir).Compare(exp, "")
	require.NoError(t, err)
	require.Equal(t, 5, rep.Pairs)
	require.Equal(t, 2, rep.Unmatched) // Control idx 6 + treatment idx 7.
	require.False(t, rep.Null)
}

func TestCompare_CrossInvocationRefuses(t *testing.T) {
	dir := t.TempDir()
	exp := &Experiment{Name: "e"}
	opts := map[string]any{"f": 1}
	var recs []RunRecord
	for _, inv := range []string{"i1", "i2"} {
		for i := 1; i <= 4; i++ {
			recs = append(recs,
				compareRecord("e", "t", ArmControl, inv, i, 100, opts),
				compareRecord("e", "t", ArmTreatment, inv, i, 90, opts))
		}
	}
	writeCompareRecords(t, dir, recs...)

	_, err := seededRunner(dir).Compare(exp, "")
	require.ErrorContains(t, err, "cross-invocation")
	require.ErrorContains(t, err, "--invocation")

	// Explicit selection pairs within the chosen invocation only.
	rep, err := seededRunner(dir).Compare(exp, "i2")
	require.NoError(t, err)
	require.Equal(t, "i2", rep.Invocation)
	require.Equal(t, 4, rep.Pairs)

	_, err = seededRunner(dir).Compare(exp, "nope")
	require.ErrorContains(t, err, "not in record set")
}

func TestCompare_LegacyStarveInference(t *testing.T) {
	dir := t.TempDir()
	exp := &Experiment{Name: "e"}
	opts := map[string]any{"f": 1}
	var recs []RunRecord
	for i := 1; i <= 5; i++ {
		recs = append(recs, compareRecord("e", "t", ArmControl, "i1", i, 100, opts))
	}
	for i := 1; i <= 3; i++ {
		recs = append(recs, compareRecord("e", "t", ArmTreatment, "i1", i, 90, opts))
	}
	writeCompareRecords(t, dir, recs...)

	_, err := seededRunner(dir).Compare(exp, "")
	require.ErrorContains(t, err, "conclusive asymmetry")
}

func TestCompare_SnapshotAlarmsRefuse(t *testing.T) {
	dir := t.TempDir()
	exp := &Experiment{Name: "e"}
	opts := map[string]any{"f": 1}
	var recs []RunRecord
	for i := 1; i <= 4; i++ {
		recs = append(recs,
			compareRecord("e", "t", ArmControl, "i1", i, 100, opts),
			compareRecord("e", "t", ArmTreatment, "i1", i, 90, opts))
	}
	writeCompareRecords(t, dir, recs...)

	r := seededRunner(dir)
	require.NoError(t, r.persistAlarms("e", "i1", Report{Starved: []string{"t"}}))
	_, err := r.Compare(exp, "i1")
	require.ErrorContains(t, err, "starved voided")
}

func TestCompare_NullExperimentLabel(t *testing.T) {
	dir := t.TempDir()
	exp := &Experiment{Name: "e"}
	same := map[string]any{"flag": "same"}
	var recs []RunRecord
	for i := 1; i <= 4; i++ {
		recs = append(recs,
			compareRecord("e", "t", ArmControl, "i1", i, 100, same),
			compareRecord("e", "t", ArmTreatment, "i1", i, 95, same))
	}
	writeCompareRecords(t, dir, recs...)

	rep, err := seededRunner(dir).Compare(exp, "")
	require.NoError(t, err)
	require.True(t, rep.Null)
}

func TestCompare_PartialNoopRefuses(t *testing.T) {
	dir := t.TempDir()
	exp := &Experiment{Name: "e"}
	same := map[string]any{"f": "x"}
	diffC := map[string]any{"f": "a"}
	diffT := map[string]any{"f": "b"}
	var recs []RunRecord
	for i := 1; i <= 4; i++ {
		recs = append(recs,
			compareRecord("e", "noop-traj", ArmControl, "i1", i, 100, same),
			compareRecord("e", "noop-traj", ArmTreatment, "i1", i, 90, same),
			compareRecord("e", "real-traj", ArmControl, "i1", i, 100, diffC),
			compareRecord("e", "real-traj", ArmTreatment, "i1", i, 90, diffT))
	}
	writeCompareRecords(t, dir, recs...)

	_, err := seededRunner(dir).Compare(exp, "")
	require.ErrorContains(t, err, "noop-flag voided")
}

func TestCompare_TooFewPairsRefuses(t *testing.T) {
	dir := t.TempDir()
	exp := &Experiment{Name: "e"}
	writeCompareRecords(t, dir,
		compareRecord("e", "t", ArmControl, "i1", 1, 100, map[string]any{"a": 1}),
		compareRecord("e", "t", ArmTreatment, "i1", 1, 90, map[string]any{"a": 2}))
	_, err := seededRunner(dir).Compare(exp, "")
	require.ErrorContains(t, err, "conclusive pairs")
}

func TestCompare_EstimatorDirection(t *testing.T) {
	dir := t.TempDir()
	exp := &Experiment{Name: "e"}
	c := map[string]any{"f": "c"}
	tr := map[string]any{"f": "t"}
	var recs []RunRecord
	// Treatment is consistently ~20% cheaper on input across two
	// trajectories — the CI must exclude 0 on the negative side.
	for i := 1; i <= 6; i++ {
		for _, traj := range []string{"t1", "t2"} {
			recs = append(recs,
				compareRecord("e", traj, ArmControl, "i1", i, 100+10*i, c),
				compareRecord("e", traj, ArmTreatment, "i1", i, 80+8*i, tr))
		}
	}
	writeCompareRecords(t, dir, recs...)

	rep, err := seededRunner(dir).Compare(exp, "")
	require.NoError(t, err)
	var input *MetricCompare
	for i := range rep.Metrics {
		if rep.Metrics[i].Name == "tokens.input" {
			input = &rep.Metrics[i]
		}
	}
	require.NotNil(t, input)
	require.Equal(t, 12, input.Pairs)
	require.InDelta(t, -20.0, input.DeltaPct, 2.0)
	require.Less(t, input.CIHiPct, 0.0, "a consistent -20%% must clear 0")
	require.Less(t, input.P, 0.05)
}

func TestCompare_PrimaryVerdicts(t *testing.T) {
	dir := t.TempDir()
	exp := &Experiment{
		Name:    "e",
		Primary: &Primary{Metric: "tokens.input", Direction: PrimaryDecrease, MDE: 0.15},
	}
	c := map[string]any{"f": "c"}
	tr := map[string]any{"f": "t"}
	var recs []RunRecord
	// -20% on the primary: clears the -15% MDE boundary → effect.
	for i := 1; i <= 6; i++ {
		recs = append(recs,
			compareRecord("e", "t", ArmControl, "i1", i, 100+10*i, c),
			compareRecord("e", "t", ArmTreatment, "i1", i, 80+8*i, tr))
	}
	writeCompareRecords(t, dir, recs...)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "noise.json"),
		[]byte(`{"cv": {"tokens.input": 0.05}}`), 0o644))

	rep, err := seededRunner(dir).Compare(exp, "")
	require.NoError(t, err)
	require.Equal(t, "tokens.input", rep.Metrics[0].Name, "primary sorts first")
	require.Equal(t, "effect ≥ MDE", rep.Metrics[0].Verdict)
}

func TestCompare_UnderpoweredVerdict(t *testing.T) {
	dir := t.TempDir()
	exp := &Experiment{
		Name:    "e",
		Primary: &Primary{Metric: "tokens.input", Direction: PrimaryDecrease, MDE: 0.15},
	}
	c := map[string]any{"f": "c"}
	tr := map[string]any{"f": "t"}
	var recs []RunRecord
	// Wildly alternating pairs — θ≈0 but the CI is wide enough to
	// span the -15% boundary, which is what "underpowered" means:
	// the data can't separate MDE from no-effect.
	for i := 1; i <= 8; i++ {
		treat := 50
		if i%2 == 0 {
			treat = 200
		}
		recs = append(recs,
			compareRecord("e", "t", ArmControl, "i1", i, 100, c),
			compareRecord("e", "t", ArmTreatment, "i1", i, treat, tr))
	}
	writeCompareRecords(t, dir, recs...)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "noise.json"),
		[]byte(`{"cv": {"tokens.input": 0.10}}`), 0o644))

	rep, err := seededRunner(dir).Compare(exp, "")
	require.NoError(t, err)
	require.Equal(t, "inconclusive-underpowered", rep.Metrics[0].Verdict)
	require.Equal(t, RequiredSamples(0.10, 0.15), rep.Metrics[0].Required)
}

func TestThetaMean_EqualTrajectoryWeights(t *testing.T) {
	// Trajectory composition: t1 has 3 pairs, t2 has 1 — the
	// estimand weights trajectories equally, not pairs.
	trajs := [][]float64{{0.1, 0.1, 0.1}, {0.5}}
	require.InDelta(t, (0.1+0.5)/2, thetaMean(trajs), 1e-9)
}

func TestSignFlipP_ExtremeArrangements(t *testing.T) {
	// All-same-sign pairs: the observed arrangement and its full
	// mirror are the most extreme — two-sided p is the minimum the
	// enumeration can report.
	trajs := [][]float64{{0.2, 0.3, 0.1}}
	obs := thetaMean(trajs)
	p := signFlipP(trajs, obs, "", 1000, rand.New(rand.NewPCG(1, 2)))
	require.InDelta(t, 3.0/9.0, p, 1e-9) // (2 extreme + 1)/(8 + 1).

	// One-sided increase: only the observed arrangement is ≥ obs.
	// (decrease on all-positive d correctly reports p≈1 — the
	// observed θ is the least extreme arrangement for that claim.)
	p = signFlipP(trajs, obs, PrimaryIncrease, 1000, rand.New(rand.NewPCG(1, 2)))
	require.InDelta(t, 2.0/9.0, p, 1e-9)
}

func TestBcaCI_SymmetricData(t *testing.T) {
	rng := rand.New(rand.NewPCG(3, 4))
	trajs := [][]float64{{-0.1, 0.05, -0.02, 0.08, -0.05, 0.01}}
	lo, hi := bcaCI(trajs, 5000, rng)
	theta := thetaMean(trajs)
	require.Less(t, lo, theta)
	require.Greater(t, hi, theta)
	require.True(t, math.Abs(lo) < 0.5 && math.Abs(hi) < 0.5)
}
