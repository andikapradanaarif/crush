package eval

import "sort"

// SelectBestOfK picks each task's reported trial under the arm's
// selection rule — verifier-free by contract (#405). last_attempt is
// max run_index: the final sample, regardless of its outcome — an
// inconclusive final trial is the arm's honest answer, not a reason
// to reach back for a better one. Records without a BestOfK mark are
// excluded; the caller passes the arm's record set.
func SelectBestOfK(recs []RunRecord) []RunRecord {
	byTask := map[string]RunRecord{}
	for _, rec := range recs {
		if rec.BestOfK == nil {
			continue
		}
		if cur, ok := byTask[rec.TrajectoryID]; !ok || rec.RunIndex > cur.RunIndex {
			byTask[rec.TrajectoryID] = rec
		}
	}
	out := make([]RunRecord, 0, len(byTask))
	for _, rec := range byTask {
		out = append(out, rec)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].TrajectoryID < out[j].TrajectoryID })
	return out
}

// BestOfKSummary reports the arm's headline numbers: pass@1 over the
// selected trials and the arm's total token spend across ALL trials —
// the matched budget the comparison prices against (#405's gate:
// pass(evolved) >= pass(best_of_k) at matched tokens).
func BestOfKSummary(recs []RunRecord) (passRate float64, tokens int64) {
	sel := SelectBestOfK(recs)
	if len(sel) == 0 {
		return 0, 0
	}
	passed := 0
	for _, rec := range sel {
		if rec.Outcome == OutcomePass {
			passed++
		}
	}
	for _, rec := range recs {
		if rec.BestOfK == nil {
			continue
		}
		tokens += rec.Tokens.Input + rec.Tokens.Output + rec.Tokens.CacheRead + rec.Tokens.CacheWrite
	}
	return float64(passed) / float64(len(sel)), tokens
}
