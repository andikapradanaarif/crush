package eval

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// rec builds a minimal harbor record — conclusive outcome plus the
// token/cost fields the characterization aggregates.
func noiseRec(inv, arm, task string, idx int, out Outcome, tok float64) RunRecord {
	c := 0.02
	return RunRecord{
		Experiment:   "harbor-exp",
		Invocation:   inv,
		Arm:          arm,
		TrajectoryID: task,
		RunIndex:     idx,
		Outcome:      out,
		DurationS:    30,
		CostUSD:      &c,
		Tokens: TokenUsage{
			Input:  int64(tok),
			Output: int64(tok) / 4,
		},
	}
}

func harborNoiseExp() *Experiment {
	return &Experiment{
		Name: "harbor-exp",
		Arms: map[string]Arm{
			ArmControl:   {Config: ArmConfig{}},
			ArmTreatment: {Config: ArmConfig{}},
		},
		Harbor: &HarborSpec{TaskIDs: []string{"t-a", "t-b"}},
	}
}

func TestComputePassNoise_DeltaCMin(t *testing.T) {
	exp := harborNoiseExp()
	var recs []RunRecord
	// Three passes over {t-a,t-b} × k=2 — solved {3,3,4} →
	// delta_trials=1, delta = 1/4 = 0.25.
	pattern := map[string][]Outcome{
		"p1": {OutcomePass, OutcomePass, OutcomeFail, OutcomePass},
		"p2": {OutcomePass, OutcomePass, OutcomePass, OutcomeFail},
		"p3": {OutcomePass, OutcomePass, OutcomePass, OutcomePass},
	}
	toks := map[string]float64{"p1": 1000, "p2": 1100, "p3": 900}
	for inv, outs := range pattern {
		for i, task := range []string{"t-a", "t-b"} {
			for k := 0; k < 2; k++ {
				recs = append(recs, noiseRec(inv, ArmControl, task, k, outs[i*2+k], toks[inv]))
			}
		}
	}
	n, err := ComputePassNoise(recs, exp, "", ArmControl, "tb21", []string{"p1", "p2", "p3"})
	require.NoError(t, err)
	require.Equal(t, 2, n.Tasks)
	require.Equal(t, 4, n.TrialsPerPass)
	require.Equal(t, []int{3, 3, 4}, n.SolvedPerPass)
	require.Equal(t, 1, n.DeltaTrials)
	require.InDelta(t, 0.25, n.Delta, 1e-9)
	// c_min = (1100−900)/1000 ≈ 0.2.
	require.InDelta(t, 0.2, n.CMin, 0.01)
	// Per-task means identical across tasks → across-task CV 0;
	// nonzero dispersion needs distinct task means.
	require.NotNil(t, n.CostUSDPerTrial)
	require.InDelta(t, 0.02, *n.CostUSDPerTrial, 1e-9)
}

func TestComputePairNoise_SigmaD(t *testing.T) {
	exp := harborNoiseExp()
	var recs []RunRecord
	// run 0: both pass; run 1: t-a fails. diffs = {-1, 0} →
	// mean −0.5, sample SD √(0.5²) ≈ 0.707.
	recs = append(recs,
		noiseRec("swe", ArmControl, "t-a", 0, OutcomePass, 100),
		noiseRec("swe", ArmControl, "t-a", 1, OutcomeFail, 120),
		noiseRec("swe", ArmControl, "t-b", 0, OutcomePass, 200),
		noiseRec("swe", ArmControl, "t-b", 1, OutcomePass, 180),
	)
	n, err := ComputePairNoise(recs, exp, "", ArmControl, "swe-pro", []string{"swe"})
	require.NoError(t, err)
	require.Equal(t, 2, n.Pairs)
	require.InDelta(t, 0.7071, n.SigmaD, 0.001)
	require.InDelta(t, 0.75, n.PassRate, 1e-9)
	// Token diffs {−25, +25} (totals are 1.25×tok) → SD 25·√2.
	require.InDelta(t, 35.355, n.TokensSigmaD, 0.001)
}

func TestComputePairNoise_CrossInvocation(t *testing.T) {
	exp := harborNoiseExp()
	var recs []RunRecord
	recs = append(recs,
		noiseRec("r1", ArmControl, "t-a", 0, OutcomePass, 100),
		noiseRec("r2", ArmControl, "t-a", 0, OutcomeFail, 100),
	)
	n, err := ComputePairNoise(recs, exp, "", ArmControl, "swe-pro", []string{"r1", "r2"})
	require.NoError(t, err)
	require.Equal(t, 1, n.Pairs)
	require.Equal(t, 0.0, n.SigmaD) // one pair — SD undefined → 0
}

func TestComputePassNoise_RefusesThin(t *testing.T) {
	exp := harborNoiseExp()
	_, err := ComputePassNoise(nil, exp, "", ArmControl, "tb21", []string{"p1"})
	require.ErrorContains(t, err, "≥2 passes")
	_, err = ComputePassNoise(nil, exp, "", ArmControl, "tb21", []string{"p1", "p2"})
	require.ErrorContains(t, err, "no conclusive")
}
