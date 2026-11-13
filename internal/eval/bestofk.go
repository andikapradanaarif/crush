package eval

import (
	"fmt"
	"sort"
	"strings"
)

// BestOfKSummary is the arm's headline read: pass@1 over the
// selected trials, the arm's total spend across ALL trials (the
// matched budget), and the completeness diagnostic that keeps a
// partial-k arm honest — a task that produced 2 of 3 declared
// trials must not read as a finished 3-trial arm (#405).
type BestOfKSummary struct {
	// Tasks is the count of (trajectory, arm) cells carrying
	// stamped trials — each contributes exactly one selected record.
	Tasks int
	// PassRate is pass@1 over the selected trials.
	PassRate float64
	// Spend is the arm's total across ALL trials under the
	// experiment's spend convention: weighted_cost when the
	// manifest pins cost_weights (sidecar spend included,
	// metrics.go), else raw solver+generator token sum. The gate
	// compares it against control measured the same way.
	Spend float64
	// CompleteTasks counts cells that produced exactly the
	// declared K trials.
	CompleteTasks int
	// IncompleteTasks lists cells whose trial count diverges from
	// the declared K as "traj/arm (n/k)" — informational: the
	// selection still reports what exists, but the budget and
	// pass@1 both read a thinner arm.
	IncompleteTasks []string
}

// SelectBestOfK picks each (trajectory, arm) cell's reported trial
// under its stamped selection rule — verifier-free by contract
// (#405). last_attempt is max run_index: the final sample,
// regardless of its outcome — an inconclusive final trial is the
// arm's honest answer, not a reason to reach back for a better
// one. Records without a BestOfK mark are excluded; records with
// an unrecognized rule are skipped rather than mis-selected.
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
		cell := rec.TrajectoryID + "\x00" + rec.Arm
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

// SummarizeBestOfK computes the arm's summary — pass@1 over
// selected trials plus all-trials spend and the per-cell
// completeness diagnostic. The caller passes the experiment so the
// spend matches whatever convention the gate prices.
func SummarizeBestOfK(exp *Experiment, recs []RunRecord) BestOfKSummary {
	var sum BestOfKSummary
	sel := SelectBestOfK(recs)
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

	// Completeness: count stamped trials per cell against the
	// declared K each record's mark carries.
	counts := map[string]int{}
	declared := map[string]int{}
	for _, rec := range recs {
		if rec.BestOfK == nil {
			continue
		}
		cell := rec.TrajectoryID + "\x00" + rec.Arm
		counts[cell]++
		declared[cell] = rec.BestOfK.K
	}
	cells := make([]string, 0, len(counts))
	for cell := range counts {
		cells = append(cells, cell)
	}
	sort.Strings(cells)
	for _, cell := range cells {
		n, k := counts[cell], declared[cell]
		if n == k {
			sum.CompleteTasks++
		} else {
			sum.IncompleteTasks = append(sum.IncompleteTasks,
				fmt.Sprintf("%s (%d/%d)", strings.ReplaceAll(cell, "\x00", "/"), n, k))
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
	for _, rec := range recs {
		if rec.BestOfK != nil {
			sum.Spend += spend(rec)
		}
	}
	return sum
}
