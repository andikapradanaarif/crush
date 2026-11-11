package eval

import (
	"fmt"
	"math"
	"slices"
	"time"
)

// Harbor base characterization (#404) — the externally executed
// noise floor the selection rules price against: δ is the pass-rate
// spread across repeated runs of the unchanged harness (RRSI E.1:
// the one value that must not be tuned), c_min the relative
// tokens-to-done spread, σ_d the per-task paired-difference SD the
// OOD sample size derives from. Everything computes over ingested
// RunRecords — the measurement is a report, not a runner.

// NoiseHarbor is the "harbor" block of eval/noise.json.
type NoiseHarbor struct {
	Model      string           `json:"model"`
	MeasuredAt time.Time        `json:"measured_at"`
	Passes     *HarborPassNoise `json:"passes,omitempty"`
	OOD        *HarborPairNoise `json:"ood,omitempty"`
}

// HarborPassNoise is the multi-pass characterization: one
// invocation per pass over the same declared task set.
type HarborPassNoise struct {
	Dataset           string    `json:"dataset"`
	Tasks             int       `json:"tasks"`
	TasksSource       string    `json:"tasks_source"`
	TrialsPerPass     int       `json:"trials_per_pass"`
	ConclusivePerPass []int     `json:"conclusive_per_pass"`
	Passes            int       `json:"passes"`
	PassRate          float64   `json:"pass_rate"`
	PassRates         []float64 `json:"pass_rates"`
	SolvedPerPass     []int     `json:"solved_per_pass"`
	Delta             float64   `json:"delta"`
	DeltaTrials       int       `json:"delta_trials"`
	CMin              float64   `json:"c_min"`
	TokensCV          float64   `json:"tokens_cv"`
	TokensPerTrial    float64   `json:"tokens_per_trial"`
	WallSPerTrial     float64   `json:"wall_s_per_trial"`
	WallTrials        int       `json:"wall_trials"`
	CostUSDPerTrial   *float64  `json:"cost_usd_per_trial,omitempty"`
	CostTrials        int       `json:"cost_trials"`
}

// HarborPairNoise is the two-run paired characterization: σ_d is
// the SD of the per-task paired pass difference — the quantity the
// OOD sample-size rule needs.
type HarborPairNoise struct {
	Dataset      string  `json:"dataset"`
	Tasks        int     `json:"tasks"`
	TasksSource  string  `json:"tasks_source"`
	Pairs        int     `json:"pairs"`
	SigmaD       float64 `json:"sigma_d"`
	PassRate     float64 `json:"pass_rate"`
	TokensCV     float64 `json:"tokens_cv"`
	TokensSigmaD float64 `json:"tokens_sigma_d"`
	// TokensLogRatioSD is CV_d — the SD of the per-task paired
	// log-ratio of tokens — the efficiency sizing's noise input
	// (#389a). Log domain makes the measure scale-free; pairs with
	// a zero-token side are skipped (log undefined).
	TokensLogRatioSD float64 `json:"tokens_log_ratio_sd"`
	TokensPerTrial   float64 `json:"tokens_per_trial"`
	WallSPerTrial    float64 `json:"wall_s_per_trial"`
}

// totalTokens is the unpriced tokens-to-done quantity — all billed
// classes summed. weighted_cost needs pinned rates; characterization
// measures the raw count the rates would price.
func totalTokens(r *RunRecord) float64 {
	t := r.Tokens
	return float64(t.Input + t.CacheRead + t.CacheWrite + t.Output)
}

// ComputePassNoise measures the multi-pass noise floor over harbor
// records — one entry of invs per pass, each a full sweep of the
// experiment's declared task set. Arm is which condition's records
// to read (base characterization runs a single arm; typically the
// declared control).
func ComputePassNoise(records []RunRecord, exp *Experiment, evalDir, arm, dataset string, invs []string) (*HarborPassNoise, error) {
	if len(invs) < 2 {
		return nil, fmt.Errorf("pass characterization needs ≥2 passes, got %d", len(invs))
	}
	tasks, source, err := HarborTaskSetForRecords(exp, evalDir, records, invs, arm)
	if err != nil {
		return nil, err
	}
	out := &HarborPassNoise{Dataset: dataset, Tasks: len(tasks), TasksSource: source}
	var allCost []float64
	var wall, tok metricSample
	taskTok := map[string]*metricSample{}
	var means []float64
	maxConclusive := 0
	for _, inv := range invs {
		var passTok metricSample
		solved, conclusive := 0, 0
		for i := range records {
			r := &records[i]
			if r.Invocation != inv || r.Arm != arm || !r.Outcome.Conclusive() {
				continue
			}
			conclusive++
			if r.Outcome == OutcomePass {
				solved++
			}
			tt := totalTokens(r)
			passTok.add(tt)
			tok.add(tt)
			if r.DurationS > 0 {
				wall.add(r.DurationS)
				out.WallTrials++
			}
			if r.CostUSD != nil {
				allCost = append(allCost, *r.CostUSD)
			}
			ts, ok := taskTok[r.TrajectoryID]
			if !ok {
				ts = &metricSample{}
				taskTok[r.TrajectoryID] = ts
			}
			ts.add(tt)
		}
		if conclusive == 0 {
			return nil, fmt.Errorf("pass %s: no conclusive %s records — is this invocation's arm named %q?", inv, dataset, arm)
		}
		out.PassRates = append(out.PassRates, float64(solved)/float64(conclusive))
		out.SolvedPerPass = append(out.SolvedPerPass, solved)
		out.ConclusivePerPass = append(out.ConclusivePerPass, conclusive)
		maxConclusive = max(maxConclusive, conclusive)
		means = append(means, passTok.Mean())
	}
	// δ's denominator is the scheduled trials per pass — declared
	// tasks × k — not whichever pass happened to stay conclusive
	// longest; infra losses shrink conclusive, not the corpus.
	k := 0
	if exp != nil && exp.Harbor != nil {
		k = exp.Harbor.K
	}
	out.TrialsPerPass = len(tasks) * k
	if out.TrialsPerPass == 0 {
		out.TrialsPerPass = maxConclusive
	}
	// δ is the solved-count spread across passes expressed as a
	// fraction of a pass's trials — RRSI's "spread in passes/N"
	// (N = trials per pass). Both forms are recorded: delta for
	// the ≤3pp gate, delta_trials for the raw read.
	lo, hi := out.SolvedPerPass[0], out.SolvedPerPass[0]
	var rateSum float64
	for i, s := range out.SolvedPerPass {
		lo, hi = min(lo, s), max(hi, s)
		rateSum += out.PassRates[i]
	}
	out.Passes = len(invs)
	out.PassRate = rateSum / float64(len(invs))
	out.DeltaTrials = hi - lo
	out.Delta = float64(out.DeltaTrials) / float64(out.TrialsPerPass)
	// c_min: relative spread of per-pass mean tokens — the
	// efficiency rule's floor for "real" cost change.
	mlo, mhi, msum := means[0], means[0], 0.0
	for _, m := range means {
		mlo, mhi, msum = min(mlo, m), max(mhi, m), msum+m
	}
	if avg := msum / float64(len(means)); avg > 0 {
		out.CMin = (mhi - mlo) / avg
	}
	// tokens_cv is the across-task dispersion of mean trial
	// tokens — the between-task variance CV_d prices.
	var taskMeans metricSample
	for _, ts := range taskTok {
		taskMeans.add(ts.Mean())
	}
	out.TokensCV = taskMeans.CV()
	out.TokensPerTrial = tok.Mean()
	out.WallSPerTrial = wall.Mean()
	if len(allCost) > 0 {
		var s metricSample
		for _, c := range allCost {
			s.add(c)
		}
		m := s.Mean()
		out.CostUSDPerTrial = &m
	}
	out.CostTrials = len(allCost)
	return out, nil
}

// ComputePairNoise measures σ_d over two paired runs of the same
// tasks. Two call shapes: one invocation string pairs its own
// run_index 0 vs 1 cells per task; two strings pair each's
// run_index 0 cell (independent jobs).
func ComputePairNoise(records []RunRecord, exp *Experiment, evalDir, arm, dataset string, invs []string) (*HarborPairNoise, error) {
	var aSel, bSel func(*RunRecord) bool
	switch len(invs) {
	case 1:
		aSel = func(r *RunRecord) bool { return r.Invocation == invs[0] && r.RunIndex == 0 }
		bSel = func(r *RunRecord) bool { return r.Invocation == invs[0] && r.RunIndex == 1 }
	case 2:
		aSel = func(r *RunRecord) bool { return r.Invocation == invs[0] && r.RunIndex == 0 }
		bSel = func(r *RunRecord) bool { return r.Invocation == invs[1] && r.RunIndex == 0 }
	default:
		return nil, fmt.Errorf("pair characterization takes 1 invocation (paired by run_index) or 2 (paired across invocations), got %d", len(invs))
	}
	type taskRec struct {
		pass bool
		tok  float64
	}
	a, b := map[string]taskRec{}, map[string]taskRec{}
	var wall, tok metricSample
	taskTok := map[string]*metricSample{}
	add := func(m map[string]taskRec, r *RunRecord) {
		m[r.TrajectoryID] = taskRec{pass: r.Outcome == OutcomePass, tok: totalTokens(r)}
		tok.add(totalTokens(r))
		wall.add(r.DurationS)
		ts, ok := taskTok[r.TrajectoryID]
		if !ok {
			ts = &metricSample{}
			taskTok[r.TrajectoryID] = ts
		}
		ts.add(totalTokens(r))
	}
	for i := range records {
		r := &records[i]
		if r.Arm != arm || !r.Outcome.Conclusive() {
			continue
		}
		if aSel(r) {
			add(a, r)
		} else if bSel(r) {
			add(b, r)
		}
	}
	var diffs, tokDiffs, lrDiffs metricSample
	pairs, passSum := 0, 0
	for task, ar := range a {
		br, ok := b[task]
		if !ok {
			continue
		}
		pairs++
		d := 0.0
		if ar.pass {
			d++
			passSum++
		}
		if br.pass {
			d--
			passSum++
		}
		diffs.add(d)
		tokDiffs.add(ar.tok - br.tok)
		if ar.tok > 0 && br.tok > 0 {
			lrDiffs.add(math.Log(br.tok / ar.tok))
		}
	}
	if pairs == 0 {
		return nil, fmt.Errorf("pair characterization: no (task, run_index) pairs across %v — check invocations and arm %q", invs, arm)
	}
	mean := diffs.Mean()
	sd := 0.0
	if diffs.N >= 2 {
		sd = math.Sqrt((diffs.Sum2 - float64(diffs.N)*mean*mean) / float64(diffs.N-1))
	}
	tmean := tokDiffs.Mean()
	tsd := 0.0
	if tokDiffs.N >= 2 {
		tsd = math.Sqrt((tokDiffs.Sum2 - float64(tokDiffs.N)*tmean*tmean) / float64(tokDiffs.N-1))
	}
	lrMean := lrDiffs.Mean()
	lrsd := 0.0
	if lrDiffs.N >= 2 {
		lrsd = math.Sqrt((lrDiffs.Sum2 - float64(lrDiffs.N)*lrMean*lrMean) / float64(lrDiffs.N-1))
	}
	var taskMeans metricSample
	for _, ts := range taskTok {
		taskMeans.add(ts.Mean())
	}
	out := &HarborPairNoise{
		Dataset:          dataset,
		Pairs:            pairs,
		SigmaD:           sd,
		PassRate:         float64(passSum) / float64(2*pairs),
		TokensCV:         taskMeans.CV(),
		TokensSigmaD:     tsd,
		TokensLogRatioSD: lrsd,
		TokensPerTrial:   tok.Mean(),
		WallSPerTrial:    wall.Mean(),
	}
	out.TasksSource = "recorded"
	if exp != nil && exp.Harbor != nil {
		if ids, err := HarborTaskSet(evalDir, exp.Harbor); err == nil {
			out.Tasks = len(ids)
			out.TasksSource = "declared"
		}
	}
	if out.Tasks == 0 {
		out.Tasks = pairs
	}
	return out, nil
}

// HarborTaskSetForRecords resolves the declared task set, falling
// back to the recorded task union when the spec's source isn't
// readable (a sample's tasks file lives wherever the operator ran).
// The returned source names which won — a "recorded" task set on a
// partial corpus reports tasks=what ran, not what was declared.
func HarborTaskSetForRecords(exp *Experiment, evalDir string, records []RunRecord, invs []string, arm string) (ids []string, source string, err error) {
	if exp != nil && exp.Harbor != nil {
		if ids, err := HarborTaskSet(evalDir, exp.Harbor); err == nil {
			return ids, "declared", nil
		}
	}
	invSet := map[string]bool{}
	for _, i := range invs {
		invSet[i] = true
	}
	seen := map[string]bool{}
	for _, r := range records {
		if r.Arm == arm && invSet[r.Invocation] && !seen[r.TrajectoryID] {
			seen[r.TrajectoryID] = true
			ids = append(ids, r.TrajectoryID)
		}
	}
	slices.Sort(ids)
	if len(ids) == 0 {
		return nil, "", fmt.Errorf("no %s records found for invocations %v", arm, invs)
	}
	return ids, "recorded", nil
}
