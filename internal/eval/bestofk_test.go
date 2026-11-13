package eval

import (
	"testing"

	"github.com/stretchr/testify/require"
)

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
			ArmTreatment: t,
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
}

func bokRec(traj string, idx int, out Outcome, tokens int64) RunRecord {
	return RunRecord{
		TrajectoryID: traj,
		Arm:          ArmTreatment,
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
		if rec.TrajectoryID == "a" && rec.Arm == ArmTreatment && rec.Invocation == "inv1" {
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
	sum := SummarizeBestOfK(&Experiment{}, ArmTreatment, "inv1", recs)
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
	sum = SummarizeBestOfK(&Experiment{}, ArmTreatment, "inv1", append(recs, other))
	require.Equal(t, 95.0, sum.Spend)
}
