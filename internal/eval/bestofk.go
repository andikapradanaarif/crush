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

	// Spend under the experiment's convention — weighted_cost
	// when cost_weights are pinned (the gate's own metric),
	// solver+generator raw tokens otherwise.
	spend := func(rec RunRecord) float64 {
		t := rec.Tokens
		v := float64(t.Input + t.Output + t.CacheRead + t.CacheWrite)
		if rec.GeneratorTokens != nil {
			g := rec.GeneratorTokens
			v += float64(g.Input + g.Output + g.CacheRead + g.CacheWrite)
		}
		return v
	}
	if exp != nil && exp.CostWeights != nil {
		if weighted, err := primaryMetricFunc(exp, "weighted_cost"); err == nil {
			spend = func(rec RunRecord) float64 { return weighted(&rec) }
		}
	}
	for _, rec := range scoped {
		sum.Spend += spend(rec)
	}
	return sum
}
