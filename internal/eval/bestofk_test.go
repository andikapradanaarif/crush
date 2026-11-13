package eval

import (
	"testing"

	"github.com/stretchr/testify/require"
)

const armBestOfK = "best_of_k"

func bokExp(mut func(*Arm)) *Experiment {
	temp := 0.2
	t := Arm{BestOfK: &BestOfKSpec{K: 3}}
	if mut != nil {
		mut(&t)
	}
	return &Experiment{
		Name:        "bok",
		Model:       "llm/x",
		Temperature: &temp,
		Corpus:      []string{"harbor"},
		Arms: map[string]Arm{
			ArmControl:   {},
			ArmTreatment: {},
			armBestOfK:   t,
		},
		Harbor: &HarborSpec{Dataset: "d", TaskIDs: []string{"t1"}, K: 1, HarnessRevision: "x"},
	}
}

func TestValidateExperiment_BestOfK(t *testing.T) {
	t.Parallel()
	require.NoError(t, ValidateExperiment(bokExp(nil)))

	bad := bokExp(func(a *Arm) { a.BestOfK.K = 1 })
	require.ErrorContains(t, ValidateExperiment(bad), ">= 2")

	bad = bokExp(func(a *Arm) { a.BestOfK.Selection = "self_judge" })
	require.ErrorContains(t, ValidateExperiment(bad), "selection")

	exp := bokExp(nil)
	exp.Harbor = nil
	require.ErrorContains(t, ValidateExperiment(exp), "harbor")

	// A gate arm can't carry the spec — Evaluate reads its
	// records as replicates, which selected trials are not.
	exp = bokExp(nil)
	exp.Arms[ArmControl] = Arm{BestOfK: &BestOfKSpec{K: 3}}
	require.ErrorContains(t, ValidateExperiment(exp), "may not declare best_of_k")

	// Coverage on a third arm validates but never fires — the
	// coverage gate reads only the treatment arm's block.
	exp = bokExp(nil)
	exp.Arms[armBestOfK] = Arm{BestOfK: &BestOfKSpec{K: 3}, Coverage: Coverage{"steps": 1}}
	require.ErrorContains(t, ValidateExperiment(exp), "coverage has no consumer")

	// A decision-rule endpoint can't name the arm — the reads
	// behind it treat records as replicates.
	exp = bokExp(nil)
	exp.DecisionRule = &DecisionRule{
		Hypothesis: "x",
		Primary:    &RulePrimary{Metric: "tokens.input", Arm: ArmTreatment, Vs: armBestOfK, Direction: PrimaryDecrease, MinImprovement: 0.1},
	}
	require.ErrorContains(t, ValidateExperiment(exp), "can't name a best_of_k arm")

	// An extra arm without the spec has no consumer at all.
	exp = bokExp(nil)
	exp.Arms["mystery"] = Arm{}
	require.ErrorContains(t, ValidateExperiment(exp), "no consumer")
}

func bokRec(traj string, idx int, out Outcome, tokens int64) RunRecord {
	return RunRecord{
		TrajectoryID: traj,
		Arm:          armBestOfK,
		Invocation:   "inv1",
		RunIndex:     idx,
		Outcome:      out,
		Tokens:       TokenUsage{Input: tokens},
		BestOfK:      &BestOfKMark{K: 3, Selection: SelectionLastAttempt},
	}
}

// Selection is a pure function over the stamped set — max run_index
// per (trajectory, arm) cell, inconclusive final trials included.
func TestSelectBestOfK(t *testing.T) {
	t.Parallel()
	recs := []RunRecord{
		bokRec("a", 0, OutcomePass, 10),
		bokRec("a", 1, OutcomeFail, 10),
		bokRec("a", 2, OutcomePass, 10),
		bokRec("b", 0, OutcomeFail, 20),
		bokRec("b", 1, OutcomeInconclusive, 20), // Last attempt stands.
		{TrajectoryID: "a", Arm: ArmControl, RunIndex: 0, Outcome: OutcomePass},
	}
	sel := SelectBestOfK(recs)
	require.Len(t, sel, 2)
	require.Equal(t, 2, sel[0].RunIndex) // Task a: last trial.
	require.Equal(t, "a", sel[0].TrajectoryID)
	require.Equal(t, 1, sel[1].RunIndex) // Task b: inconclusive wins nothing back.
	require.Equal(t, OutcomeInconclusive, sel[1].Outcome)

	// Trials on the same task but different arms never compete for
	// one slot — selection partitions by (trajectory, arm).
	both := bokRec("a", 0, OutcomePass, 1)
	both.Arm = ArmControl
	recs = append(recs, both)
	sel = SelectBestOfK(recs)
	require.Len(t, sel, 3)

	// And run_index ordinals reset per invocation — a stale
	// invocation's late trial never outbids the current one.
	stale := bokRec("a", 9, OutcomeFail, 1)
	stale.Invocation = "inv0"
	sel = SelectBestOfK(append(recs, stale))
	for _, rec := range sel {
		if rec.TrajectoryID == "a" && rec.Arm == armBestOfK && rec.Invocation == "inv1" {
			require.Equal(t, 2, rec.RunIndex)
		}
	}
}

func TestSummarizeBestOfK(t *testing.T) {
	t.Parallel()
	recs := []RunRecord{
		bokRec("a", 0, OutcomePass, 10),
		bokRec("a", 1, OutcomePass, 10),
		bokRec("a", 2, OutcomeFail, 10),
		bokRec("b", 0, OutcomePass, 20),
		bokRec("b", 1, OutcomePass, 20),
		bokRec("b", 2, OutcomePass, 20),
		bokRec("c", 0, OutcomePass, 5), // 1 of 3 trials — partial arm.
	}
	sum := SummarizeBestOfK(&Experiment{}, armBestOfK, "inv1", recs)
	// Selected: a=fail(idx2), b=pass(idx2), c=pass(idx0) → 2/3.
	// Spend: all seven trials' tokens.
	require.Equal(t, 3, sum.Tasks)
	require.Equal(t, 2, sum.CompleteTasks)
	require.Equal(t, []string{"c (1/3)"}, sum.IncompleteTasks)
	require.InDelta(t, 2.0/3.0, sum.PassRate, 1e-9)
	require.Equal(t, 95.0, sum.Spend)

	// Scope: another arm or invocation contributes nothing.
	other := bokRec("a", 0, OutcomePass, 999)
	other.Arm, other.Invocation = ArmControl, "inv2"
	sum = SummarizeBestOfK(&Experiment{}, armBestOfK, "inv1", append(recs, other))
	require.Equal(t, 95.0, sum.Spend)
}

// CompareBestOfK is the matched-budget read: the ordinary arm's
// pass@1 against the selected pass@1 on shared tasks, both sides'
// spend counted under one convention.
func TestCompareBestOfK(t *testing.T) {
	t.Parallel()
	exp := bokExp(nil)
	// Evolved (treatment): a passes 1/2 replicates, b passes, c fails.
	evolved := []RunRecord{
		{TrajectoryID: "a", Arm: ArmTreatment, Invocation: "inv1", RunIndex: 0, Outcome: OutcomePass, Tokens: TokenUsage{Input: 10}},
		{TrajectoryID: "a", Arm: ArmTreatment, Invocation: "inv1", RunIndex: 1, Outcome: OutcomeFail, Tokens: TokenUsage{Input: 10}},
		{TrajectoryID: "b", Arm: ArmTreatment, Invocation: "inv1", RunIndex: 0, Outcome: OutcomePass, Tokens: TokenUsage{Input: 20}},
		{TrajectoryID: "c", Arm: ArmTreatment, Invocation: "inv1", RunIndex: 0, Outcome: OutcomeFail, Tokens: TokenUsage{Input: 5}},
	}
	// best_of_k k=2 cells: a selected fail, b selected pass, c selected pass.
	trials := []RunRecord{
		bokRec("a", 0, OutcomePass, 10), bokRec("a", 1, OutcomeFail, 10),
		bokRec("b", 0, OutcomePass, 20), bokRec("b", 1, OutcomePass, 20),
		bokRec("c", 0, OutcomePass, 5), bokRec("c", 1, OutcomePass, 5),
	}
	for i := range trials {
		trials[i].BestOfK.K = 2
	}
	// A task only the bok arm ran — off the shared set, ignored.
	lonely := bokRec("d", 0, OutcomePass, 99)
	lonely.BestOfK.K = 1
	recs := append(append(evolved, trials...), lonely)

	cmp, err := CompareBestOfK(exp, ArmTreatment, armBestOfK, "inv1", recs)
	require.NoError(t, err)
	require.Equal(t, 3, cmp.SharedTasks)
	// Evolved: a=1/2, b=1, c=0 → (0.5+1+0)/3 = 0.5.
	require.InDelta(t, 0.5, cmp.EvolvedPass, 1e-9)
	// Selected: a=fail, b=pass, c=pass → 2/3.
	require.InDelta(t, 2.0/3.0, cmp.BestOfKPass, 1e-9)
	require.Equal(t, 45.0, cmp.EvolvedSpend)
	require.Equal(t, 70.0, cmp.BestOfKSpend) // d's lone trial is off the shared set.
	require.InDelta(t, 70.0/45.0, cmp.SpendRatio, 1e-9)
	// Evolved 0.5 < bok 0.67 — the pass-rate claim fails: extra
	// sampling beat the harness under test.
	require.False(t, cmp.GateOK)
	require.Empty(t, cmp.Incomplete)

	// c's last attempt failing makes bok 2/3 — evolved still loses.
	recs2 := append(append([]RunRecord{}, evolved...),
		bokRec("a", 0, OutcomePass, 10), bokRec("a", 1, OutcomePass, 10),
		bokRec("b", 0, OutcomePass, 20), bokRec("b", 1, OutcomePass, 20),
		bokRec("c", 0, OutcomePass, 5), bokRec("c", 1, OutcomeFail, 5),
	)
	for _, i := range []int{4, 5, 6, 7, 8, 9} {
		recs2[i].BestOfK.K = 2
	}
	cmp, err = CompareBestOfK(exp, ArmTreatment, armBestOfK, "inv1", recs2)
	require.NoError(t, err)
	require.InDelta(t, 2.0/3.0, cmp.BestOfKPass, 1e-9) // a pass, b pass, c fail.
	require.InDelta(t, 0.5, cmp.EvolvedPass, 1e-9)
	require.False(t, cmp.GateOK) // 0.5 < 0.67 still loses.

	// An incomplete shared cell blocks the claim even when winning.
	recs3 := append(append([]RunRecord{}, evolved...),
		bokRec("a", 0, OutcomeFail, 10), bokRec("a", 1, OutcomeFail, 10),
		bokRec("b", 0, OutcomeFail, 20), bokRec("b", 1, OutcomeFail, 20),
		bokRec("c", 0, OutcomeFail, 5), // 1/2 — partial cell.
	)
	for _, i := range []int{4, 5, 6, 7, 8} {
		recs3[i].BestOfK.K = 2
	}
	cmp, err = CompareBestOfK(exp, ArmTreatment, armBestOfK, "inv1", recs3)
	require.NoError(t, err)
	require.Equal(t, []string{"c (1/2)"}, cmp.Incomplete)
	require.False(t, cmp.GateOK) // Incomplete cells refuse the claim.

	// Same records with the cell complete: bok 0/3, evolved 0.5 —
	// the harness clears the extra-sampling bar.
	recs4 := append(append([]RunRecord{}, evolved...),
		bokRec("a", 0, OutcomeFail, 10), bokRec("a", 1, OutcomeFail, 10),
		bokRec("b", 0, OutcomeFail, 20), bokRec("b", 1, OutcomeFail, 20),
		bokRec("c", 0, OutcomeFail, 5), bokRec("c", 1, OutcomeFail, 5),
	)
	for _, i := range []int{4, 5, 6, 7, 8, 9} {
		recs4[i].BestOfK.K = 2
	}
	cmp, err = CompareBestOfK(exp, ArmTreatment, armBestOfK, "inv1", recs4)
	require.NoError(t, err)
	require.True(t, cmp.GateOK)

	// Evolved out-passing an underspending baseline is a false win —
	// the matched-spend half of the claim refuses it.
	cheap := []RunRecord{
		{TrajectoryID: "a", Arm: ArmTreatment, Invocation: "inv1", RunIndex: 0, Outcome: OutcomePass, Tokens: TokenUsage{Input: 100}},
	}
	cheapBok := []RunRecord{
		bokRec("a", 0, OutcomeFail, 1), bokRec("a", 1, OutcomeFail, 1),
	}
	cheapBok[0].BestOfK.K, cheapBok[1].BestOfK.K = 2, 2
	cmp, err = CompareBestOfK(exp, ArmTreatment, armBestOfK, "inv1", append(cheap, cheapBok...))
	require.NoError(t, err)
	require.InDelta(t, 1.0, cmp.EvolvedPass, 1e-9)
	require.Zero(t, cmp.BestOfKPass)
	require.Less(t, cmp.SpendRatio, 1.0)
	require.False(t, cmp.GateOK)

	// Wrong arm roles fail fast.
	_, err = CompareBestOfK(exp, armBestOfK, ArmTreatment, "inv1", recs)
	require.ErrorContains(t, err, "no best_of_k spec")
	_, err = CompareBestOfK(exp, ArmControl, "ghost", "inv1", recs)
	require.ErrorContains(t, err, "not declared")
}

// When cost_weights pin relative prices, both sides' spend reads the
// weighted convention — including generator-token sidecar spend at
// the pinned tier — not the raw-token fallback.
func TestCompareBestOfK_WeightedSpend(t *testing.T) {
	t.Parallel()
	exp := bokExp(nil)
	exp.CostWeights = &CostWeights{
		CacheRead: 0.1, CacheWrite: 0.5, Output: 2.0,
		Generator: &CostWeights{CacheRead: 0.05, CacheWrite: 0.25, Output: 1.0},
	}
	gen := &GeneratorTokens{Input: 10, Output: 4}
	evolved := []RunRecord{
		{
			TrajectoryID: "a", Arm: ArmTreatment, Invocation: "inv1", RunIndex: 0, Outcome: OutcomePass,
			Tokens: TokenUsage{Input: 10, CacheRead: 100, Output: 5}, GeneratorTokens: gen,
		},
	}
	trials := []RunRecord{
		bokRec("a", 0, OutcomeFail, 0), bokRec("a", 1, OutcomeFail, 0),
	}
	for i := range trials {
		trials[i].BestOfK.K = 2
		trials[i].Tokens = TokenUsage{Input: 10, CacheRead: 100, Output: 5}
		trials[i].GeneratorTokens = gen
	}
	cmp, err := CompareBestOfK(exp, ArmTreatment, armBestOfK, "inv1", append(evolved, trials...))
	require.NoError(t, err)
	// Per record: 10 + 0.1·100 + 2.0·5 = 30 solver, plus
	// 10 + 1.0·4 = 14 generator → 44 weighted. Raw would be 129.
	require.InDelta(t, 44.0, cmp.EvolvedSpend, 1e-9)
	require.InDelta(t, 88.0, cmp.BestOfKSpend, 1e-9)
	require.InDelta(t, 2.0, cmp.SpendRatio, 1e-9)
	require.True(t, cmp.GateOK) // Complete cell, pass 1>0, ratio 2.
}
