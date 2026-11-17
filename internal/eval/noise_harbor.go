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

// rangeOf3 is E[max−min] of three standard normals — the factor
// turning a per-pass SD into the spread G1.d's "range of 3 passes"
// protocol expects.
const rangeOf3 = 1.693

// NoiseHarbor is the "harbor" block of eval/noise.json.
type NoiseHarbor struct {
	Model      string    `json:"model"`
	MeasuredAt time.Time `json:"measured_at"`
	// Status marks the block's standing: "superseded" means the
	// runs it was computed from measured a different system than
	// the protocol now declares (wrong pin, sampling not delivered)
	// and nothing may size against it (#504). The numbers stay
	// recorded for audit; a fresh characterization clears the mark
	// only when it covers every recorded sub-block — a partial
	// rewrite leaves the survivors condemned (MergeHarbor).
	Status string `json:"status,omitempty"`
	// Note explains the status — why the block was superseded.
	Note   string           `json:"note,omitempty"`
	Passes *HarborPassNoise `json:"passes,omitempty"`
	OOD    *HarborPairNoise `json:"ood,omitempty"`
	// Cells is the repeated-cell decomposition (#489) — when
	// recorded it supersedes ood.sigma_d in the sizing path: the
	// components price against the declared k, not whatever the
	// pair read happened to run.
	Cells *HarborCellNoise `json:"cells,omitempty"`
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
	// DeltaSolve is the conclusive-normalized δ — the spread of
	// per-pass solve rates (solved/conclusive), so infra losses
	// stop contaminating solve variance. The G1.d amendment
	// (#489) prices against this quantity; Delta keeps measuring
	// the all-in spread over scheduled trials for the
	// instability read.
	DeltaSolve float64 `json:"delta_solve"`
	// CMin is the legacy read — the relative spread of
	// *arithmetic* per-pass mean tokens, all-in. One runaway
	// trial dominates it; once a cells block exists, the
	// efficiency rule's "real change" floor prices against
	// cells.c_min_expected instead (#507).
	CMin            float64  `json:"c_min"`
	TokensCV        float64  `json:"tokens_cv"`
	TokensPerTrial  float64  `json:"tokens_per_trial"`
	WallSPerTrial   float64  `json:"wall_s_per_trial"`
	WallTrials      int      `json:"wall_trials"`
	CostUSDPerTrial *float64 `json:"cost_usd_per_trial,omitempty"`
	CostTrials      int      `json:"cost_trials"`
}

// HarborCellNoise is the repeated-cell characterization (#489): k
// conclusive reps per task resolve each cell's flip rate p̂ᵢ,
// separating within-task Bernoulli noise (reps shrink it) from the
// between-task systematic spread (nothing shrinks it). The pair
// read can't split them either — its σ²_d cancels task difficulty
// by construction (≈2σ²_within + drift), so it measures neither
// component of the sized estimand.
type HarborCellNoise struct {
	Dataset     string `json:"dataset"`
	Tasks       int    `json:"tasks"`
	TasksSource string `json:"tasks_source"`
	// Cells is the per-task outcome matrix — task → [passes,
	// conclusive reps]. The matrix is the raw evidence the
	// decomposition stands on; it stays recorded so the σ's audit.
	// Tasks reports the declared count when the spec resolves
	// (sibling convention) — the recorded cell count is len(Cells).
	Cells       map[string][2]int `json:"cells"`
	RepsPerTask float64           `json:"reps_per_task"`
	PassRate    float64           `json:"pass_rate"`
	// MeasurableCells counts cells with ≥2 reps — the only cells
	// where within noise is estimable. Zero means sigma_within is
	// a structural artifact, not a measurement, and sizing must
	// refuse the block rather than price a within term of 0.
	MeasurableCells int `json:"measurable_cells"`
	// SigmaWithin is the per-trial within-task Bernoulli SD — the
	// mean of p̂ᵢ(1−p̂ᵢ)·kᵢ/(kᵢ−1) over cells with ≥2 reps, the
	// only cells where within noise is measurable. Single-rep
	// cells impute that mean for their Var(p̂) correction; an
	// all-1-rep matrix has no measurable cell and records 0 — an
	// artifact, not a measured noise floor; reps_per_task is the
	// tell. The k/(k−1) factor undoes E[p̂(1−p̂)]'s plug-in bias.
	SigmaWithin float64 `json:"sigma_within"`
	// TokensLogSigmaWithin is the pooled within-task SD of log
	// total tokens — df-weighted over cells with ≥2 token-bearing
	// reps (a single-rep cell contributes no within information).
	// Log domain makes the measure scale-free and stops one
	// runaway trial from dominating the read the way it does the
	// arithmetic spread (#507).
	TokensLogSigmaWithin float64 `json:"tokens_log_sigma_within"`
	// TokensMeasurableCells counts token-bearing cells with ≥2
	// reps — the only cells within token noise is estimable from.
	// Zero means tokens_log_sigma_within = 0 is a structural
	// artifact (no measurable cell), not a measured-flat floor —
	// the same tell measurable_cells gives the pass side.
	TokensMeasurableCells int `json:"token_measurable_cells"`
	// TokensLogSigmaBetween is the between-task systematic SD of
	// log tokens — the same moment decomposition as the pass part:
	// Var(task log-means) minus the expected within contribution,
	// floored at 0.
	TokensLogSigmaBetween float64 `json:"tokens_log_sigma_between"`
	// TokensGeomeanPerTrial is exp(mean(log tokens)) over
	// conclusive trials with tokens > 0 — the multiplicative
	// center the log decomposition prices against.
	TokensGeomeanPerTrial float64 `json:"tokens_geomean_per_trial,omitempty"`
	// DeltaExpected is the model-estimated δ̂ at the manifest's
	// declared schedule (tasks × k reps):
	// 1.693·√(mean_w/(n·k)) — the expected range of 3 normals,
	// i.e. G1.d's "range of 3 passes" priced against the measured
	// within weight instead of one high-variance realization.
	// Absent when no multi-rep cell measured w.
	DeltaExpected float64 `json:"delta_expected,omitempty"`
	// CMinExpected is the model-estimated token c_min at the same
	// schedule: exp(1.693·σ_logw/√(n·k)) − 1 — the expected
	// 3-pass log spread expressed as a relative token change
	// *from the cell minimum* (exp(range)−1 ≈ (max−min)/min).
	// The arithmetic c_min records (max−min)/mean instead, so at
	// high σ the two reads diverge roughly 2× by construction —
	// different denominators, not a bug. Once a cells block
	// exists, "real efficiency" reads price against this, not
	// the arithmetic all-in c_min (#507).
	CMinExpected float64 `json:"c_min_expected,omitempty"`
	// SigmaBetween is the between-task systematic SD — the spread
	// of p̂ᵢ left after subtracting expected within noise.
	// OOD sizing plugs it in for cross-arm effect heterogeneity,
	// which a base-arm characterization can't measure — a
	// modeling choice, not a derivation (see HarborOODSizing).
	SigmaBetween float64 `json:"sigma_between"`

	TokensPerTrial  float64  `json:"tokens_per_trial"`
	WallSPerTrial   float64  `json:"wall_s_per_trial"`
	CostUSDPerTrial *float64 `json:"cost_usd_per_trial,omitempty"`
	CostTrials      int      `json:"cost_trials"`
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
	TokensLogRatioSD float64  `json:"tokens_log_ratio_sd"`
	TokensPerTrial   float64  `json:"tokens_per_trial"`
	WallSPerTrial    float64  `json:"wall_s_per_trial"`
	CostUSDPerTrial  *float64 `json:"cost_usd_per_trial,omitempty"`
	CostTrials       int      `json:"cost_trials"`
}

// HarborCostPerTrial returns the measured mean trial cost the
// harbor blocks recorded — the spend basis for schedule
// projections — and whether any block measured one. Prefers the
// cells read (most reps), then pair, then passes. A measured free
// route returns (0, true): $0 measured, not unmeasured, so
// projections should still report (#516 review).
func (n *NoiseFile) HarborCostPerTrial() (float64, bool) {
	if n == nil || n.Harbor == nil {
		return 0, false
	}
	if b := n.Harbor.Cells; b != nil && b.CostUSDPerTrial != nil {
		return *b.CostUSDPerTrial, true
	}
	if b := n.Harbor.OOD; b != nil && b.CostUSDPerTrial != nil {
		return *b.CostUSDPerTrial, true
	}
	if b := n.Harbor.Passes; b != nil && b.CostUSDPerTrial != nil {
		return *b.CostUSDPerTrial, true
	}
	return 0, false
}

// GateArmCount counts the manifest's sized-read arms — every arm
// that isn't a best_of_k selection baseline. The sized schedule's
// trial projection multiplies by gate arms only: candidate baselines
// aren't part of the control/treatment compare being priced (#516
// review).
func GateArmCount(e *Experiment) int {
	n := 0
	for _, a := range e.Arms {
		if a.BestOfK == nil {
			n++
		}
	}
	return n
}

// ProjectedGateTrials prices the sized harbor schedule's trial
// count — required tasks × declared reps/arm × gate arms. A
// non-harbor manifest projects 0: it has no harbor schedule to price
// (a non-harbor OOD rule still sizes against a shared noise.json).
func ProjectedGateTrials(e *Experiment, required int) int {
	if e == nil || e.Harbor == nil {
		return 0
	}
	return required * max(e.Harbor.K, 1) * GateArmCount(e)
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
	// A best-of-k arm's trials aren't replicates — TrialsPerPass
	// denominates by HarborSpec.K and "solved" is per-trial, while
	// the arm's real read is per selected record (#405). Refuse
	// rather than emit noise numbers for the wrong unit.
	if exp != nil && exp.Arms[arm].BestOfK != nil {
		return nil, fmt.Errorf("pass noise: arm %q declares best_of_k — trials are selection candidates, not replicates; use the task-level best_of_k read", arm)
	}
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
			// A conclusive trial that emitted no token telemetry
			// prices $0 — pooling it would bias the mean toward
			// free and misreport cost_trials. Zero-token records
			// are unmeasured, not free (#516 review).
			if r.CostUSD != nil && tt > 0 {
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
	// Exclusions shrink the scheduled obligation too — an excluded
	// task's k trials would only inflate the all-in δ denominator,
	// biasing the instability read optimistic.
	excluded := 0
	if exp != nil && exp.Harbor != nil {
		for id := range exp.Harbor.Exclude {
			if slices.Contains(tasks, id) {
				excluded++
			}
		}
	}
	out.TrialsPerPass = (len(tasks) - excluded) * k
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
	// Conclusive-normalized δ: passes normalize by their own
	// conclusive count, so a rate-limit-thinned pass can't inflate
	// the solve-variance spread.
	rlo, rhi := out.PassRates[0], out.PassRates[0]
	for _, r := range out.PassRates {
		rlo, rhi = min(rlo, r), max(rhi, r)
	}
	out.DeltaSolve = rhi - rlo
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
	var allCost []float64
	taskTok := map[string]*metricSample{}
	add := func(m map[string]taskRec, r *RunRecord) {
		m[r.TrajectoryID] = taskRec{pass: r.Outcome == OutcomePass, tok: totalTokens(r)}
		tok.add(totalTokens(r))
		wall.add(r.DurationS)
		// Zero-token records price $0 without measuring — excluded
		// from the cost pool, same as the pass and cell blocks.
		if r.CostUSD != nil && totalTokens(r) > 0 {
			allCost = append(allCost, *r.CostUSD)
		}
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

// ComputeCellNoise measures the repeated-cell decomposition over
// harbor records — every conclusive record of the named arm across
// the named invocations is one cell rep, grouped by task. Per task
// p̂ᵢ = passes/conclusive; the decomposition then reads
//
//	Var(p̂ᵢ) = σ²_between + meanᵢ[p̂ᵢ(1−p̂ᵢ)/kᵢ]
//
// and solves for σ²_between (floored at 0 — a corpus thinner than
// its noise floor reports no systematic spread). Requires ≥2 tasks
// with conclusive cells. The within weight uses the unbiased
// p̂ᵢ(1−p̂ᵢ)·kᵢ/(kᵢ−1) form — the raw plug-in reads low by ~1/kᵢ.
func ComputeCellNoise(records []RunRecord, exp *Experiment, evalDir, arm, dataset string, invs []string) (*HarborCellNoise, error) {
	if exp != nil && exp.Arms[arm].BestOfK != nil {
		return nil, fmt.Errorf("cell noise: arm %q declares best_of_k — trials are selection candidates, not replicates; use the task-level best_of_k read", arm)
	}
	if len(invs) < 1 {
		return nil, fmt.Errorf("cell characterization needs ≥1 invocation, got %d", len(invs))
	}
	invSet := map[string]bool{}
	for _, i := range invs {
		invSet[i] = true
	}
	cells := map[string][2]int{}
	logTok := map[string][]float64{}
	var wall, tok metricSample
	var allCost []float64
	var logSum float64
	var logN int
	for i := range records {
		r := &records[i]
		if r.Arm != arm || !invSet[r.Invocation] || !r.Outcome.Conclusive() {
			continue
		}
		c := cells[r.TrajectoryID]
		c[1]++
		if r.Outcome == OutcomePass {
			c[0]++
		}
		cells[r.TrajectoryID] = c
		wall.add(r.DurationS)
		tt := totalTokens(r)
		tok.add(tt)
		if tt > 0 {
			l := math.Log(tt)
			logTok[r.TrajectoryID] = append(logTok[r.TrajectoryID], l)
			logSum += l
			logN++
		}
		// Zero-token records price $0 without measuring — excluded
		// from the cost pool, same as the pass and pair blocks.
		if r.CostUSD != nil && tt > 0 {
			allCost = append(allCost, *r.CostUSD)
		}
	}
	if len(cells) < 2 {
		return nil, fmt.Errorf("cell characterization: %d tasks with conclusive %s cells — the decomposition needs ≥2", len(cells), arm)
	}
	// p̂ᵢ per cell; wᵢ = p̂ᵢ(1−p̂ᵢ)·kᵢ/(kᵢ−1) is the unbiased
	// Bernoulli weight over cells where it's measurable (kᵢ ≥ 2),
	// and wᵢ/kᵢ the within contribution that Var(p̂ᵢ) is expected
	// to carry. Single-rep cells have the largest true within
	// contribution of any cell — p(1−p)/1 — so excluding them
	// would under-correct Var(p̂) and dump the missing mass into
	// σ²_between; they impute at the multi-rep mean instead.
	var phats []float64
	var wSum, withinSum float64
	var measurable, singletons int
	for _, c := range cells {
		k := float64(c[1])
		p := float64(c[0]) / k
		phats = append(phats, p)
		if k >= 2 {
			w := p * (1 - p) * k / (k - 1)
			wSum += w
			withinSum += w / k
			measurable++
		} else {
			singletons++
		}
	}
	n := float64(len(cells))
	wMean := 0.0
	if measurable > 0 {
		wMean = wSum / float64(measurable)
	}
	withinSum += wMean * float64(singletons)
	var phat metricSample
	for _, p := range phats {
		phat.add(p)
	}
	mean := phat.Mean()
	varSum := 0.0
	for _, p := range phats {
		varSum += (p - mean) * (p - mean)
	}
	varSum /= n - 1
	betweenVar := varSum - withinSum/n
	if betweenVar < 0 {
		betweenVar = 0
	}
	out := &HarborCellNoise{
		Dataset:         dataset,
		Tasks:           len(cells),
		TasksSource:     "recorded",
		Cells:           cells,
		MeasurableCells: measurable,
		PassRate:        mean,
		SigmaWithin:     math.Sqrt(wMean),
		SigmaBetween:    math.Sqrt(betweenVar),
		TokensPerTrial:  tok.Mean(),
		WallSPerTrial:   wall.Mean(),
	}
	if len(allCost) > 0 {
		var s metricSample
		for _, c := range allCost {
			s.add(c)
		}
		m := s.Mean()
		out.CostUSDPerTrial = &m
	}
	out.CostTrials = len(allCost)
	for _, c := range cells {
		out.RepsPerTask += float64(c[1])
	}
	out.RepsPerTask /= n
	// nSched is the task count the expected reads divide by — the
	// tasks that can actually produce reps. Recorded cells are the
	// fallback; a resolved declaration shrinks by exclusions the
	// same way TrialsPerPass shrinks its denominator — including
	// excluded tasks inflates n and prices δ̂/c_min_expected
	// optimistic, the anti-conservative direction on the efficiency
	// floor (#517 review).
	nSched := n
	if exp != nil && exp.Harbor != nil {
		if ids, err := HarborTaskSet(evalDir, exp.Harbor); err == nil && len(ids) > 0 {
			// Sibling blocks report the declared count — the
			// recorded matrix itself already lives in Cells.
			out.Tasks = len(ids)
			out.TasksSource = "declared"
			excluded := 0
			for id := range exp.Harbor.Exclude {
				if slices.Contains(ids, id) {
					excluded++
				}
			}
			nSched = max(float64(len(ids)-excluded), 1)
		}
	}
	// The log-token decomposition mirrors the pass part: pooled
	// within-cell variance (df-weighted) is the within σ², and the
	// between σ² is Var(task log-means) minus each cell's expected
	// within contribution σw²/kᵢ — homoscedastic pooled σw² stands
	// in for the unmeasurable per-cell within variance.
	if logN > 0 {
		out.TokensGeomeanPerTrial = math.Exp(logSum / float64(logN))
	}
	var withinSS, withinDF, invK float64
	var logMeans []float64
	for _, xs := range logTok {
		ki := float64(len(xs))
		invK += 1 / ki
		m := 0.0
		for _, x := range xs {
			m += x
		}
		m /= ki
		logMeans = append(logMeans, m)
		for _, x := range xs {
			withinSS += (x - m) * (x - m)
		}
		withinDF += ki - 1
		if ki >= 2 {
			out.TokensMeasurableCells++
		}
	}
	if withinDF > 0 && len(logMeans) >= 2 {
		sigmaW2 := withinSS / withinDF
		out.TokensLogSigmaWithin = math.Sqrt(sigmaW2)
		lMean := 0.0
		for _, m := range logMeans {
			lMean += m
		}
		lMean /= float64(len(logMeans))
		lVar := 0.0
		for _, m := range logMeans {
			lVar += (m - lMean) * (m - lMean)
		}
		lVar /= float64(len(logMeans) - 1)
		lbVar := lVar - sigmaW2*invK/float64(len(logMeans))
		if lbVar < 0 {
			lbVar = 0
		}
		out.TokensLogSigmaBetween = math.Sqrt(lbVar)
	}
	// Expected-read estimates at the declared schedule: the range-
	// of-3 factor turns the measured within weights into the
	// spread a 3-pass protocol expects, replacing the realized
	// range a single characterization happened to produce.
	kPlan := 1.0
	if exp != nil && exp.Harbor != nil && exp.Harbor.K > 0 {
		kPlan = float64(exp.Harbor.K)
	}
	if wMean > 0 {
		out.DeltaExpected = rangeOf3 * math.Sqrt(wMean/(nSched*kPlan))
	}
	if out.TokensLogSigmaWithin > 0 {
		out.CMinExpected = math.Exp(rangeOf3*out.TokensLogSigmaWithin/math.Sqrt(nSched*kPlan)) - 1
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
