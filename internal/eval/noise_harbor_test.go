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

// Conclusive-normalized δ (#489): identical solved counts on
// different conclusive bases must not read as zero solve variance,
// and a rate-limit-thinned pass can't inflate it either.
func TestComputePassNoise_DeltaSolve(t *testing.T) {
	exp := harborNoiseExp()
	var recs []RunRecord
	// p1 solves 3/4 conclusive, p2 solves 3/3 — the solved-count
	// spread is 0 while the solve rates differ by .25.
	pattern := map[string][]Outcome{
		"p1": {OutcomePass, OutcomePass, OutcomeFail, OutcomePass},
		"p2": {OutcomePass, OutcomePass, OutcomePass, OutcomeInconclusive},
	}
	for inv, outs := range pattern {
		for i, task := range []string{"t-a", "t-b"} {
			for k := 0; k < 2; k++ {
				recs = append(recs, noiseRec(inv, ArmControl, task, k, outs[i*2+k], 100))
			}
		}
	}
	n, err := ComputePassNoise(recs, exp, "", ArmControl, "tb21", []string{"p1", "p2"})
	require.NoError(t, err)
	require.Equal(t, []int{4, 3}, n.ConclusivePerPass)
	require.Equal(t, []int{3, 3}, n.SolvedPerPass)
	require.InDelta(t, 0.75, n.PassRates[0], 1e-9)
	require.InDelta(t, 1.0, n.PassRates[1], 1e-9)
	require.Equal(t, 0, n.DeltaTrials)
	require.InDelta(t, 0.0, n.Delta, 1e-9)
	require.InDelta(t, 0.25, n.DeltaSolve, 1e-9)
}

// Excluded tasks shrink the scheduled-trials denominator — their k
// can't inflate the all-in δ.
func TestComputePassNoise_TrialsPerPassExcludes(t *testing.T) {
	exp := harborNoiseExp()
	exp.Harbor.Exclude = map[string]string{"t-b": "image defect"}
	var recs []RunRecord
	pattern := map[string][]Outcome{
		"p1": {OutcomePass, OutcomePass, OutcomeFail, OutcomePass},
		"p2": {OutcomePass, OutcomePass, OutcomePass, OutcomePass},
	}
	for inv, outs := range pattern {
		for i, task := range []string{"t-a", "t-b"} {
			for k := 0; k < 2; k++ {
				recs = append(recs, noiseRec(inv, ArmControl, task, k, outs[i*2+k], 100))
			}
		}
	}
	n, err := ComputePassNoise(recs, exp, "", ArmControl, "tb21", []string{"p1", "p2"})
	require.NoError(t, err)
	require.Equal(t, 2, n.TrialsPerPass) // (2 tasks − 1 excluded) × k=2
	require.Equal(t, 1, n.DeltaTrials)   // solved {3,4}
	require.InDelta(t, 0.5, n.Delta, 1e-9)
}

// The repeated-cell decomposition (#489): p̂ per cell, Var(p̂)
// minus expected within noise = the systematic spread. Hand-worked
// numbers: cells {4/4, 2/4, 0/4} → Var .25; the unbiased within
// weight .25·4/3 = 1/3 gives within-contribution 1/12 ÷ 3 ≈ .0278
// → σ_between √(.222) ≈ .471, σ_within √(1/9) ≈ .333.
func TestComputeCellNoise_Decomposition(t *testing.T) {
	exp := harborNoiseExp()
	var recs []RunRecord
	add := func(task string, outs ...Outcome) {
		for i, o := range outs {
			recs = append(recs, noiseRec("inv1", ArmControl, task, i, o, 100))
		}
	}
	add("t-a", OutcomePass, OutcomePass, OutcomePass, OutcomePass)
	add("t-b", OutcomePass, OutcomeFail, OutcomePass, OutcomeFail)
	add("t-c", OutcomeFail, OutcomeFail, OutcomeFail, OutcomeFail)
	// Cache classes are billed too — tokens_per_trial prices them.
	recs[len(recs)-1].Tokens.CacheRead = 50
	// A record outside the named invocations mustn't pool.
	recs = append(recs, noiseRec("other", ArmControl, "t-a", 9, OutcomeFail, 100))

	n, err := ComputeCellNoise(recs, exp, "", ArmControl, "tb21", []string{"inv1"})
	require.NoError(t, err)
	// Declared resolves (t-a/t-b) → Tasks reports the declared
	// count; the recorded matrix keeps all three tasks in Cells.
	require.Equal(t, 2, n.Tasks)
	require.Equal(t, "declared", n.TasksSource)
	require.Equal(t, map[string][2]int{"t-a": {4, 4}, "t-b": {2, 4}, "t-c": {0, 4}}, n.Cells)
	require.InDelta(t, 4.0, n.RepsPerTask, 1e-9)
	require.InDelta(t, 0.5, n.PassRate, 1e-9)
	require.InDelta(t, 0.3333, n.SigmaWithin, 1e-3)
	require.InDelta(t, 0.4714, n.SigmaBetween, 1e-3)
	// Per-trial economics aggregate over the same records — a
	// k-rep schedule's price is the point of the read. Cache
	// classes count: (125·11 + 175)/12 ≈ 129.17.
	require.InDelta(t, 129.1667, n.TokensPerTrial, 1e-3)
	require.InDelta(t, 30.0, n.WallSPerTrial, 1e-9)
	require.Equal(t, 12, n.CostTrials)
	require.InDelta(t, 0.02, *n.CostUSDPerTrial, 1e-9)

	// An identical-p̂ corpus carries no systematic spread — the
	// observed variance is all coin flips and floors at 0.
	var flat []RunRecord
	for _, task := range []string{"t-a", "t-b"} {
		for i := 0; i < 4; i++ {
			out := OutcomePass
			if i == 0 {
				out = OutcomeFail
			}
			flat = append(flat, noiseRec("inv1", ArmControl, task, i, out, 100))
		}
	}
	n, err = ComputeCellNoise(flat, exp, "", ArmControl, "tb21", []string{"inv1"})
	require.NoError(t, err)
	require.Equal(t, 0.0, n.SigmaBetween)
	require.InDelta(t, 0.5, n.SigmaWithin, 1e-3) // √(0.75·0.25·4/3)
}

// Unequal cell sizes exercise the per-cell kᵢ weights: t-a has 4
// reps (2/4), t-b has 2 (1/2) — same p̂, different w·k/(k−1).
func TestComputeCellNoise_UnequalReps(t *testing.T) {
	exp := harborNoiseExp()
	recs := []RunRecord{
		noiseRec("inv1", ArmControl, "t-a", 0, OutcomePass, 100),
		noiseRec("inv1", ArmControl, "t-a", 1, OutcomeFail, 100),
		noiseRec("inv1", ArmControl, "t-a", 2, OutcomePass, 100),
		noiseRec("inv1", ArmControl, "t-a", 3, OutcomeFail, 100),
		noiseRec("inv2", ArmControl, "t-b", 0, OutcomePass, 100),
		noiseRec("inv2", ArmControl, "t-b", 1, OutcomeFail, 100),
	}
	n, err := ComputeCellNoise(recs, exp, "", ArmControl, "tb21", []string{"inv1", "inv2"})
	require.NoError(t, err)
	require.Equal(t, map[string][2]int{"t-a": {2, 4}, "t-b": {1, 2}}, n.Cells)
	require.InDelta(t, 3.0, n.RepsPerTask, 1e-9)
	// w_a = .5·.5·4/3 = 1/3; w_b = .5·.5·2/1 = .5 → σ_within
	// √((1/3+.5)/2) = √(.4167) ≈ .6455. Var(p̂)=0 → between 0.
	require.InDelta(t, 0.6455, n.SigmaWithin, 1e-3)
	require.Equal(t, 0.0, n.SigmaBetween)

	// An all-1-rep matrix can't see within noise — no measurable
	// cell exists, so every scrap of spread lands in σ_between.
	// Recorded as an artifact, flagged by reps_per_task < 2.
	var one []RunRecord
	for i, task := range []string{"t-a", "t-b"} {
		out := OutcomePass
		if i == 1 {
			out = OutcomeFail
		}
		one = append(one, noiseRec("inv1", ArmControl, task, 0, out, 100))
	}
	n, err = ComputeCellNoise(one, exp, "", ArmControl, "tb21", []string{"inv1"})
	require.NoError(t, err)
	require.InDelta(t, 1.0, n.RepsPerTask, 1e-9)
	require.Equal(t, 0.0, n.SigmaWithin)
	require.InDelta(t, 0.7071, n.SigmaBetween, 1e-3) // √(Var({1,0})) = √.5
}

// Mixed cells: the k=1 cell's within contribution is p(1−p)/1 —
// the largest of any cell — so it imputes the multi-rep mean rather
// than contributing 0 and dumping the mass into σ_between. σ_within
// is the mean over cells where it's measurable, not diluted by
// singletons.
func TestComputeCellNoise_SingletonImputation(t *testing.T) {
	exp := harborNoiseExp()
	recs := []RunRecord{
		noiseRec("inv1", ArmControl, "t-a", 0, OutcomePass, 100),
		noiseRec("inv1", ArmControl, "t-a", 1, OutcomeFail, 100),
		noiseRec("inv1", ArmControl, "t-a", 2, OutcomePass, 100),
		noiseRec("inv1", ArmControl, "t-a", 3, OutcomeFail, 100),
		noiseRec("inv1", ArmControl, "t-b", 0, OutcomePass, 100),
	}
	n, err := ComputeCellNoise(recs, exp, "", ArmControl, "tb21", []string{"inv1"})
	require.NoError(t, err)
	require.Equal(t, map[string][2]int{"t-a": {2, 4}, "t-b": {1, 1}}, n.Cells)
	// σ_within = √(w_a) = √(.25·4/3) = √(1/3) ≈ .5774 — the
	// measurable cell alone, not diluted by t-b's singleton.
	require.InDelta(t, 0.5774, n.SigmaWithin, 1e-3)
	// withinSum = w_a/4 + w̄·1 = 1/12 + 1/3 = 5/12; Var({.5,1})=.125
	// → .125 − .208 < 0 → floors at 0.
	require.Equal(t, 0.0, n.SigmaBetween)
}

// noise.json's cells matrix is hand-editable — malformed entries
// (0 reps, passes>reps, padded short arrays, NaN σ's) must fail
// closed at load, not produce NaN sizing that reads "powered".
func TestLoadNoise_CellValidation(t *testing.T) {
	dir := t.TempDir()
	write := func(t *testing.T, cells string) {
		t.Helper()
		require.NoError(t, os.WriteFile(filepath.Join(dir, "noise.json"),
			[]byte(`{"cv":{},"harbor":{"cells":{"cells":`+cells+`,"sigma_within":0.4,"sigma_between":0.3}}}`), 0o644))
	}
	write(t, `{"t-a":[0,0]}`)
	_, err := LoadNoise(dir)
	require.ErrorContains(t, err, "harbor.cells")
	write(t, `{"t-a":[5,4]}`)
	_, err = LoadNoise(dir)
	require.ErrorContains(t, err, "harbor.cells")
	// A short array unmarshals to [4,0] — 0 reps trips the same guard.
	write(t, `{"t-a":[4]}`)
	_, err = LoadNoise(dir)
	require.ErrorContains(t, err, "harbor.cells")
	write(t, `{"t-a":[2,4],"t-b":[1,4]}`)
	_, err = LoadNoise(dir)
	require.NoError(t, err)
}

// A recorded cells block supersedes raw σ_d in sizing: the
// decomposed σ_d recombines σ_between with both paired cells'
// within noise at the manifest's declared k — sizing on raw σ_d
// oversizes massively (#489).
func TestHarborOODSizing_CellsPreferred(t *testing.T) {
	exp := harborNoiseExp()
	exp.Harbor.K = 8 // The planned repeated-cell schedule.
	exp.DecisionRule = &DecisionRule{
		Hypothesis: "h",
		OOD:        true,
		Guardrail:  &RuleGuardrail{Metric: "pass_rate", MinDelta: -0.05},
	}
	noise := &NoiseFile{Harbor: &NoiseHarbor{
		OOD:   &HarborPairNoise{SigmaD: 0.56, TokensLogRatioSD: 0.5},
		Cells: &HarborCellNoise{SigmaBetween: 0.30, SigmaWithin: 0.50},
	}}
	sizing, err := HarborOODSizing(exp, noise)
	require.NoError(t, err)
	// σ_eff² = .30² + 2·.50²/8 = .1525 → σ_eff ≈ .3905.
	// N = (1.6449+0.8416)²·.1525/.0025 ≈ 377.3 → 378.
	require.Equal(t, 378, sizing.NNonInferiority)
	require.Equal(t, 378, sizing.Required)
	require.Equal(t, "cells", sizing.SigmaSource)

	// Same manifest, no cells: the raw pair σ_d prices it — the
	// measured-inflation the amendment exists to remove.
	sizing, err = HarborOODSizing(exp, &NoiseFile{Harbor: &NoiseHarbor{
		OOD: &HarborPairNoise{SigmaD: 0.56, TokensLogRatioSD: 0.5},
	}})
	require.NoError(t, err)
	require.Equal(t, 776, sizing.NNonInferiority)
	require.Equal(t, "pair", sizing.SigmaSource)
}

// The decomposition's refusal paths: best_of_k arms (selection
// candidates aren't replicates), zero invocations, and fewer than
// two tasks with conclusive cells.
func TestComputeCellNoise_Errors(t *testing.T) {
	exp := harborNoiseExp()
	exp.Arms["bok"] = Arm{BestOfK: &BestOfKSpec{K: 3}}
	recs := []RunRecord{
		noiseRec("inv1", ArmControl, "t-a", 0, OutcomePass, 100),
	}
	_, err := ComputeCellNoise(recs, exp, "", "bok", "tb21", []string{"inv1"})
	require.ErrorContains(t, err, "best_of_k")
	_, err = ComputeCellNoise(recs, exp, "", ArmControl, "tb21", nil)
	require.ErrorContains(t, err, "≥1 invocation")
	_, err = ComputeCellNoise(recs, exp, "", ArmControl, "tb21", []string{"inv1"})
	require.ErrorContains(t, err, "≥2")
}

// A cells-only noise block still sizes a pass_rate rule — the
// efficiency leg (tokens_log_ratio_sd) is the only one needing the
// pair read, so --cells --size can't fail closed on pass claims.
func TestHarborOODSizing_CellsOnly(t *testing.T) {
	exp := harborNoiseExp()
	exp.Harbor.K = 8
	exp.DecisionRule = &DecisionRule{
		Hypothesis: "h",
		OOD:        true,
		Guardrail:  &RuleGuardrail{Metric: "pass_rate", MinDelta: -0.05},
	}
	sizing, err := HarborOODSizing(exp, &NoiseFile{Harbor: &NoiseHarbor{
		Cells: &HarborCellNoise{SigmaBetween: 0.30, SigmaWithin: 0.50},
	}})
	require.NoError(t, err)
	require.Equal(t, 378, sizing.NNonInferiority)

	// The same cells can't power a continuous primary — the token
	// pair read is genuinely missing.
	exp.DecisionRule = &DecisionRule{
		Hypothesis: "h",
		OOD:        true,
		Primary:    &RulePrimary{Metric: "weighted_cost", MinImprovement: 0.1},
	}
	_, err = HarborOODSizing(exp, &NoiseFile{Harbor: &NoiseHarbor{
		Cells: &HarborCellNoise{SigmaBetween: 0.30, SigmaWithin: 0.50},
	}})
	require.ErrorContains(t, err, "tokens_log_ratio_sd")
}

// The cells block merges field-wise and refuses silent retuning,
// same discipline as passes/ood.
func TestMergeHarbor_CellsBlock(t *testing.T) {
	n := &NoiseFile{Harbor: &NoiseHarbor{Model: "m"}}
	c := &HarborCellNoise{SigmaBetween: 0.3}
	require.NoError(t, n.MergeHarbor(&NoiseHarbor{Cells: c, MeasuredAt: time.Now()}, false))
	require.ErrorContains(t, n.MergeHarbor(&NoiseHarbor{Cells: c}, false), "already recorded")
	require.NoError(t, n.MergeHarbor(&NoiseHarbor{Cells: c}, true))
}
