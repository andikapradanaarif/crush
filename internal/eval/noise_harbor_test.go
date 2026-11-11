package eval

import (
	"os"
	"path/filepath"
	"testing"
	"time"

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
	temp := 0.2
	return &Experiment{
		Name:              "harbor-exp",
		Model:             "llm/test-model",
		Temperature:       &temp,
		Corpus:            []string{"harbor"},
		RunsPerTrajectory: map[Band]int{BandUncharacterized: 1},
		CostWeights:       &CostWeights{CacheRead: 0.1, CacheWrite: 1.25, Output: 4},
		Arms: map[string]Arm{
			ArmControl:   {Config: ArmConfig{}},
			ArmTreatment: {Config: ArmConfig{}},
		},
		Harbor: &HarborSpec{Dataset: "tb21", TaskIDs: []string{"t-a", "t-b"}, K: 2, HarnessRevision: "abc1234"},
	}
}

func TestMergeHarbor_FieldWiseAndRefusal(t *testing.T) {
	n := &NoiseFile{}
	p := &NoiseHarbor{Passes: &HarborPassNoise{Delta: 0.1}}
	require.NoError(t, n.MergeHarbor(p, false))
	require.NotNil(t, n.Harbor.Passes)
	require.Nil(t, n.Harbor.OOD)

	// A --pair-only write fills ood without erasing passes.
	o := &NoiseHarbor{OOD: &HarborPairNoise{SigmaD: 0.3}}
	require.NoError(t, n.MergeHarbor(o, false))
	require.Equal(t, 0.1, n.Harbor.Passes.Delta)
	require.Equal(t, 0.3, n.Harbor.OOD.SigmaD)

	// Overwriting a recorded sub-block refuses without force —
	// retuning the base floor is the explicit exception.
	require.ErrorContains(t, n.MergeHarbor(p, false), "--force")
	require.NoError(t, n.MergeHarbor(p, true))

	// A metadata-only merge (labels, no measured block) keeps the
	// original measurement time — it didn't re-measure anything.
	at := n.Harbor.MeasuredAt
	require.NoError(t, n.MergeHarbor(&NoiseHarbor{Model: "other-model", MeasuredAt: at.Add(time.Hour)}, false))
	require.Equal(t, "other-model", n.Harbor.Model)
	require.Equal(t, at, n.Harbor.MeasuredAt)
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
	require.Equal(t, "declared", n.TasksSource)
	require.Equal(t, 4, n.TrialsPerPass) // 2 tasks × declared k=2
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

// The #389 planning priors must reproduce the issue's arithmetic:
// σ_d ≈ 0.165 (RRSI's SWE-V CI) → N_b ≈ 187 at margin −3pp,
// N_c ≈ 134 at a 4pp superiority claim. The issue's hand-math
// rounds intermediates (σ_d² = 0.0272, z² = 6.175); exact z's
// land within ±1 of its integers.
func TestOODSizing_PriorReproduction(t *testing.T) {
	require.InDelta(t, 187, float64(RequiredNNonInferiority(0.165, -0.03, 0)), 1)
	require.InDelta(t, 134, float64(RequiredNSuperiority(0.165, 0.04)), 1)
	// CV_d 20% at a 10% efficiency claim → (2.8016)²·(0.2/0.1)² ≈ 31.4.
	require.Equal(t, 32, RequiredNEfficiency(0.20, 0.10))
}

func TestHarborOODSizing_Endpoints(t *testing.T) {
	exp := harborNoiseExp()
	exp.DecisionRule = &DecisionRule{
		Hypothesis: "treatment cuts tokens at equal pass",
		Primary:    &RulePrimary{Metric: "tokens_to_done", Arm: ArmTreatment, Vs: ArmControl, Direction: PrimaryDecrease, MinImprovement: 0.10},
		Guardrail:  &RuleGuardrail{Metric: "pass_rate", MinDelta: -0.03},
		OOD:        true,
	}
	// No measured block → unpowerable, fails closed.
	_, err := HarborOODSizing(exp, &NoiseFile{})
	require.ErrorContains(t, err, "harbor")

	noise := &NoiseFile{Harbor: &NoiseHarbor{OOD: &HarborPairNoise{
		SigmaD:           0.165,
		TokensLogRatioSD: 0.20,
	}}}
	s, err := HarborOODSizing(exp, noise)
	require.NoError(t, err)
	require.Equal(t, 32, s.NEfficiency)
	require.InDelta(t, 187, float64(s.NNonInferiority), 1)
	require.Equal(t, s.NNonInferiority, s.Required)

	// Non-OOD rules return nil — nothing to size.
	exp.DecisionRule.OOD = false
	s, err = HarborOODSizing(exp, noise)
	require.NoError(t, err)
	require.Nil(t, s)

	// A superiority-only claim mustn't slip the gate: with no
	// binding endpoint alongside, its own N_c is the floor —
	// Required 0 would make the underpowered check vacuous for
	// exactly the claim being powered.
	exp.DecisionRule = &DecisionRule{
		Hypothesis: "treatment passes more",
		Primary:    &RulePrimary{Metric: "pass_rate", Arm: ArmTreatment, Vs: ArmControl, Direction: PrimaryIncrease, MinImprovement: 0.04},
		OOD:        true,
	}
	s, err = HarborOODSizing(exp, noise)
	require.NoError(t, err)
	require.Zero(t, s.NEfficiency)
	require.Zero(t, s.NNonInferiority)
	require.InDelta(t, 134, float64(s.NSuperiority), 1)
	require.Equal(t, s.NSuperiority, s.Required)
}

func TestValidateExperiment_OODNeedsSizableEndpoint(t *testing.T) {
	exp := harborNoiseExp()
	exp.Resample = ResampleCluster
	exp.DecisionRule = &DecisionRule{
		Hypothesis: "treatment is better somehow",
		Primary:    &RulePrimary{Metric: "tokens_to_done", Arm: ArmTreatment, Vs: ArmControl, Direction: PrimaryDecrease},
		OOD:        true,
	}
	require.ErrorContains(t, ValidateExperiment(exp), "sizable endpoint")
	exp.DecisionRule.Guardrail = &RuleGuardrail{Metric: "pass_rate", MinDelta: -0.03}
	require.NoError(t, ValidateExperiment(exp))
}

// The verdict path: an OOD rule over an underpowered recorded
// corpus reports inconclusive with the required N — never a
// null verdict. 3 tasks can't resolve a ~200-task requirement.
func TestCompare_OODUnderpoweredInconclusive(t *testing.T) {
	dir := t.TempDir()
	exp := harborNoiseExp()
	exp.Resample = ResampleCluster
	exp.DecisionRule = &DecisionRule{
		Hypothesis: "treatment cuts tokens at equal pass",
		Primary:    &RulePrimary{Metric: "tokens_to_done", Arm: ArmTreatment, Vs: ArmControl, Direction: PrimaryDecrease, MinImprovement: 0.10},
		Guardrail:  &RuleGuardrail{Metric: "pass_rate", MinDelta: -0.03},
		OOD:        true,
	}
	var recs []RunRecord
	for i, task := range []string{"t-a", "t-b", "t-c"} {
		recs = append(recs,
			compareRecord(exp.Name, task, ArmControl, "i1", i, 100, map[string]any{"f": "c"}),
			compareRecord(exp.Name, task, ArmTreatment, "i1", i, 80, map[string]any{"f": "t"}))
	}
	writeCompareRecords(t, dir, recs...)
	// Measured floor: σ_d = CV_d = 0.5 → N_b ≈ 6.19·0.25/0.0009
	// ≈ 1717 tasks against 3 recorded.
	require.NoError(t, os.WriteFile(filepath.Join(dir, "noise.json"), []byte(
		`{"cv": {}, "harbor": {"ood": {"sigma_d": 0.5, "tokens_log_ratio_sd": 0.5}}}`), 0o644))
	r := seededRunner(dir)
	require.NoError(t, r.persistAlarms(exp, "i1", Report{}, nil))

	rep, err := r.Compare(exp, "i1")
	require.NoError(t, err)
	require.Equal(t, "inconclusive", rep.RuleResult)
	require.Contains(t, rep.RuleNote, "underpowered")
	require.Contains(t, rep.RuleNote, "3 tasks recorded")
}
