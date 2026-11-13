package eval

import (
	"fmt"
	"sort"
)

// BestOfKSummary is one arm's headline read inside one invocation:
// pass@1 over the selected trials, the arm's total spend across ALL
// trials (the matched budget), and the completeness diagnostic that
// keeps a partial-k arm honest — a task that produced 2 of 3
// declared trials must not read as a finished 3-trial arm (#405).
// Both fields are per-arm, per-invocation by construction: the
// pairing invariant Compare enforces applies here — records from
// different invocations are different conditions, and a headline
// number across arms is meaningless.
type BestOfKSummary struct {
	// Tasks is the count of trajectories carrying stamped trials —
	// each contributes exactly one selected record.
	Tasks int
	// PassRate is pass@1 over the selected trials.
	PassRate float64
	// Spend is the arm's total across ALL trials under the
	// experiment's spend convention: weighted_cost when the
	// manifest pins cost_weights (sidecar spend included,
	// metrics.go), else raw solver+generator token sum. The gate
	// compares it against control measured the same way.
	Spend float64
	// CompleteTasks counts trajectories that produced exactly the
	// declared K trials.
	CompleteTasks int
	// IncompleteTasks lists trajectories whose trial count
	// diverges from the declared K as "traj (n/k)" — informational:
	// the selection still reports what exists, but the budget and
	// pass@1 both read a thinner arm.
	IncompleteTasks []string
}

// bokCell keys a best-of-k selection cell — trajectory, arm, and
// invocation: run_index ordinals reset per invocation, and arms
// declare independent specs, so a trial only competes against
// trials of the same arm on the same task in the same invocation.
func bokCell(rec RunRecord) string {
	return rec.TrajectoryID + "\x00" + rec.Arm + "\x00" + rec.Invocation
}

// SelectBestOfK picks each (trajectory, arm, invocation) cell's
// reported trial under its stamped selection rule — verifier-free
// by contract (#405). last_attempt is max run_index: the final
// sample, regardless of its outcome — an inconclusive final trial
// is the arm's honest answer, not a reason to reach back for a
// better one. Records without a BestOfK mark are excluded; records
// with an unrecognized rule are skipped rather than mis-selected.
func SelectBestOfK(recs []RunRecord) []RunRecord {
	byCell := map[string]RunRecord{}
	for _, rec := range recs {
		mark := rec.BestOfK
		if mark == nil {
			continue
		}
		// Dispatch on the stamped rule — a second policy lands
		// with its own case, not a silent last-attempt read.
		if mark.Selection != SelectionLastAttempt {
			continue
		}
		cell := bokCell(rec)
		if cur, ok := byCell[cell]; !ok || rec.RunIndex > cur.RunIndex {
			byCell[cell] = rec
		}
	}
	out := make([]RunRecord, 0, len(byCell))
	for _, rec := range byCell {
		out = append(out, rec)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].TrajectoryID != out[j].TrajectoryID {
			return out[i].TrajectoryID < out[j].TrajectoryID
		}
		return out[i].Arm < out[j].Arm
	})
	return out
}

// SummarizeBestOfK computes one arm's summary inside one
// invocation — the same record-scope invariant Compare enforces
// ("pairing only holds within one"). recs may be an unfiltered
// record set; only this arm+invocation's stamped records count.
func SummarizeBestOfK(exp *Experiment, arm, invocation string, recs []RunRecord) BestOfKSummary {
	var sum BestOfKSummary
	var scoped []RunRecord
	for _, rec := range recs {
		if rec.Arm == arm && rec.Invocation == invocation && rec.BestOfK != nil {
			scoped = append(scoped, rec)
		}
	}
	sel := SelectBestOfK(scoped)
	if len(sel) == 0 {
		return sum
	}
	sum.Tasks = len(sel)
	passed := 0
	for _, rec := range sel {
		if rec.Outcome == OutcomePass {
			passed++
		}
	}
	sum.PassRate = float64(passed) / float64(len(sel))

	// Completeness: count stamped trials per trajectory against
	// the declared K — max-wins so a spec that changed K between
	// ingests reads the larger obligation, not whichever record
	// iterated last. Any stamped record counts here even when an
	// unrecognized rule excludes it from selection above — a
	// future rule's trials still attest scheduled work.
	counts := map[string]int{}
	declared := map[string]int{}
	for _, rec := range scoped {
		counts[rec.TrajectoryID]++
		if rec.BestOfK.K > declared[rec.TrajectoryID] {
			declared[rec.TrajectoryID] = rec.BestOfK.K
		}
	}
	trajs := make([]string, 0, len(counts))
	for traj := range counts {
		trajs = append(trajs, traj)
	}
	sort.Strings(trajs)
	for _, traj := range trajs {
		n, k := counts[traj], declared[traj]
		if n == k {
			sum.CompleteTasks++
		} else {
			sum.IncompleteTasks = append(sum.IncompleteTasks,
				fmt.Sprintf("%s (%d/%d)", traj, n, k))
		}
	}

	spend := bokSpendFunc(exp)
	for _, rec := range scoped {
		sum.Spend += spend(rec)
	}
	return sum
}

// bokSpendFunc prices one record under the experiment's convention —
// weighted_cost when cost_weights are pinned (the gate's own metric,
// sidecar spend included, metrics.go), else raw solver+generator
// token sum. Both sides of a matched-budget comparison must read the
// same convention.
func bokSpendFunc(exp *Experiment) func(RunRecord) float64 {
	if exp != nil && exp.CostWeights != nil {
		if weighted, err := primaryMetricFunc(exp, "weighted_cost"); err == nil {
			return func(rec RunRecord) float64 { return weighted(&rec) }
		}
	}
	return func(rec RunRecord) float64 {
		t := rec.Tokens
		v := float64(t.Input + t.Output + t.CacheRead + t.CacheWrite)
		if rec.GeneratorTokens != nil {
			g := rec.GeneratorTokens
			v += float64(g.Input + g.Output + g.CacheRead + g.CacheWrite)
		}
		return v
	}
}

// BestOfKComparison is the task-level matched-budget read (#405): an
// ordinary arm's pass@1 against a best_of_k arm's selected pass@1 on
// the tasks both attempted, with both sides' total spend. The
// comparison the run-level estimator can't express — best_of_k
// spends k trials per task but reports one answer; the ordinary arm
// spends one (or its own replicates) and reports each. The gate's
// pass-rate claim requires EvolvedPass >= BestOfKPass on a complete
// cell set — extra sampling must not beat the harness under test.
type BestOfKComparison struct {
	// EvolvedArm and BestOfKArm name the compared arms.
	EvolvedArm string
	BestOfKArm string
	// SharedTasks counts trajectories both arms have records on —
	// the comparison conditions on the shared set only.
	SharedTasks int
	// EvolvedPass is pass@1 on shared tasks: per-task conclusive
	// pass fraction averaged over tasks (replicates contribute
	// their mean, not extra tasks).
	EvolvedPass float64
	// BestOfKPass is selected-trial pass@1 on shared tasks.
	BestOfKPass float64
	// EvolvedSpend and BestOfKSpend are total arm spend on shared
	// tasks under the experiment's spend convention — all trials
	// for best_of_k, all records for the ordinary arm.
	EvolvedSpend float64
	BestOfKSpend float64
	// SpendRatio is best_of_k spend / evolved spend — the budget
	// match K was chosen for (≈1 is matched; <1 is an easy bar).
	SpendRatio float64
	// Incomplete lists best_of_k cells short of their declared K —
	// a partial arm hasn't spent its full budget, so the gate
	// refuses the claim rather than read a thinner baseline.
	Incomplete []string
	// GateOK is the #405 pass-rate gate: evolved >= best_of_k on
	// shared tasks, all best_of_k cells complete, and at least one
	// shared task. Efficiency claims never consult it.
	GateOK bool
}

// CompareBestOfK compares one ordinary arm against one best_of_k arm
// inside one invocation — the comparison the issue calls "best_of_k
// vs control at matched tokens". recs may be an unfiltered record
// set; only this invocation's records count. evolvedArm must not
// declare best_of_k (a selection arm's answer isn't a per-task mean)
// and bokArm must declare it.
func CompareBestOfK(exp *Experiment, evolvedArm, bokArm, invocation string, recs []RunRecord) (*BestOfKComparison, error) {
	if exp == nil {
		return nil, fmt.Errorf("compare best_of_k: nil experiment")
	}
	arm, ok := exp.Arms[bokArm]
	if !ok {
		return nil, fmt.Errorf("compare best_of_k: arm %q not declared", bokArm)
	}
	if arm.BestOfK == nil {
		return nil, fmt.Errorf("compare best_of_k: arm %q declares no best_of_k spec", bokArm)
	}
	if _, ok := exp.Arms[evolvedArm]; !ok {
		return nil, fmt.Errorf("compare best_of_k: arm %q not declared", evolvedArm)
	}
	if exp.Arms[evolvedArm].BestOfK != nil {
		return nil, fmt.Errorf("compare best_of_k: arm %q is itself best_of_k — compare two ordinary arms via Compare", evolvedArm)
	}

	cmp := &BestOfKComparison{EvolvedArm: evolvedArm, BestOfKArm: bokArm}
	sel := map[string]RunRecord{}
	for _, rec := range SelectBestOfK(recs) {
		if rec.Arm == bokArm && rec.Invocation == invocation {
			sel[rec.TrajectoryID] = rec
		}
	}
	evolved := map[string][]RunRecord{}
	for _, rec := range recs {
		if rec.Arm == evolvedArm && rec.Invocation == invocation && rec.BestOfK == nil {
			evolved[rec.TrajectoryID] = append(evolved[rec.TrajectoryID], rec)
		}
	}

	spend := bokSpendFunc(exp)
	for _, rec := range recs {
		if rec.Arm == bokArm && rec.Invocation == invocation && rec.BestOfK != nil {
			if _, shared := evolved[rec.TrajectoryID]; shared {
				cmp.BestOfKSpend += spend(rec)
			}
		}
	}
	passSum := 0.0
	for traj, erecs := range evolved {
		brec, shared := sel[traj]
		if !shared {
			continue
		}
		cmp.SharedTasks++
		if brec.Outcome == OutcomePass {
			cmp.BestOfKPass++
		}
		conclusive, passes := 0, 0
		for _, rec := range erecs {
			cmp.EvolvedSpend += spend(rec)
			if rec.Outcome.Conclusive() {
				conclusive++
				if rec.Outcome == OutcomePass {
					passes++
				}
			}
		}
		if conclusive > 0 {
			passSum += float64(passes) / float64(conclusive)
		}
	}
	if cmp.SharedTasks > 0 {
		cmp.EvolvedPass = passSum / float64(cmp.SharedTasks)
		cmp.BestOfKPass /= float64(cmp.SharedTasks)
	}
	if cmp.EvolvedSpend > 0 {
		cmp.SpendRatio = cmp.BestOfKSpend / cmp.EvolvedSpend
	}

	// Completeness against the declared K — shared tasks only, so
	// an off-comparison partial cell doesn't block the read.
	counts, declared := map[string]int{}, map[string]int{}
	for _, rec := range recs {
		if rec.Arm != bokArm || rec.Invocation != invocation || rec.BestOfK == nil {
			continue
		}
		if _, shared := evolved[rec.TrajectoryID]; !shared {
			continue
		}
		counts[rec.TrajectoryID]++
		if rec.BestOfK.K > declared[rec.TrajectoryID] {
			declared[rec.TrajectoryID] = rec.BestOfK.K
		}
	}
	trajs := make([]string, 0, len(counts))
	for traj := range counts {
		trajs = append(trajs, traj)
	}
	sort.Strings(trajs)
	for _, traj := range trajs {
		if counts[traj] != declared[traj] {
			cmp.Incomplete = append(cmp.Incomplete,
				fmt.Sprintf("%s (%d/%d)", traj, counts[traj], declared[traj]))
		}
	}
	cmp.GateOK = cmp.SharedTasks > 0 && len(cmp.Incomplete) == 0 &&
		cmp.EvolvedPass >= cmp.BestOfKPass
	return cmp, nil
}
