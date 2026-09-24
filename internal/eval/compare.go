package eval

import (
	"encoding/json"
	"fmt"
	"maps"
	"math"
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// compare.go implements `crush eval compare` — the paired
// continuous-metric estimator the gate deliberately isn't. The gate
// answers "did the outcome rate collapse"; compare answers "by how
// much did the metric move", on drift-matched attempt pairs within a
// single invocation.
//
// Pairing: control run i with treatment run i within a trajectory —
// attempt-index (run_index) pairing, not conclusive-ordinal. The
// scheduler alternates lead arms each round, so the two attempts at
// the same run_index are the temporally closest samples the harness
// produced; pairing them is what cancels the slow provider drift that
// dominates pooled variance. run_index is sparse under resampling —
// an attempt whose counterpart never ran or didn't conclude simply
// forms no pair. Conclusive-ordinal pairing was rejected: under
// asymmetric exclusion it pairs samples taken at different wall
// times, injecting the drift the pairing exists to remove.
//
// Estimator: per-pair log-ratio d_i = log(treat/ctrl) → per-trajectory
// mean → unweighted mean across trajectories. The trajectory is the
// unit of analysis, so one arm landing disproportionately on easy
// trajectories can't shift the estimate — the composition bias the
// pooled gate strata are subject to. CI is BCa over a stratified
// bootstrap (pairs resampled within their trajectory); p is a
// sign-flip permutation on pair log-ratios (each pair's d is
// symmetric about 0 under the null), one-sided in the primary's
// declared direction, two-sided otherwise.
//
// Refusals are hard errors, matching the harness's fail-closed
// contract: cross-invocation record sets (pairing across a drift
// boundary is meaningless), a fired structural alarm for the
// invocation (the run's own gate already voided it), and too few
// pairs to support an interval.

// CompareReport is the paired-estimator read-out for one invocation.
type CompareReport struct {
	Experiment string
	Invocation string
	// Null marks an invocation whose arms resolved identically on
	// every trajectory — an accidental or declared A/A. The metrics
	// then measure the harness's own false-effect magnitude, not a
	// treatment effect.
	Null         bool
	Trajectories []string
	Pairs        int
	Unmatched    int
	Metrics      []MetricCompare
	// SkippedMetrics names registry entries with too few measurable
	// pairs to estimate — absent telemetry is visible, not silent.
	SkippedMetrics []string
}

// MetricCompare is one metric's paired estimate.
type MetricCompare struct {
	Name  string
	Pairs int
	// Dropped counts pairs excluded because a side was nonpositive
	// (log-ratio undefined) — e.g. tokens.cache_read on a
	// non-caching provider.
	Dropped  int
	Theta    float64 // mean log-ratio across trajectories.
	DeltaPct float64
	CILoPct  float64
	CIHiPct  float64
	P        float64
	// Verdict is set only on the primary metric: "effect" (CI clears
	// the MDE boundary), "no-mde-effect" (CI clears on the null
	// side), or "inconclusive-underpowered" (CI spans the boundary —
	// Required then holds the powered sample size).
	Verdict  string
	Required int
}

// alarmSnapshot is the structural-alarm subset of a gate Report,
// persisted as results/<exp>/report-<invocation>.json so compare can
// refuse on the run's own verdicts instead of re-inferring them.
type alarmSnapshot struct {
	Invocation string   `json:"invocation"`
	Starved    []string `json:"starved,omitempty"`
	Saturated  []string `json:"saturated,omitempty"`
	Skipped    []string `json:"skipped,omitempty"`
	NoopFlags  []string `json:"noop_flags,omitempty"`
}

// persistAlarms writes the structural-alarm snapshot for this
// invocation. The full Report stays process-local (its Summary is
// the run output); the snapshot is compare's refusal ground truth.
func (r *Runner) persistAlarms(expName, inv string, rep Report) error {
	snap := alarmSnapshot{
		Invocation: inv,
		Starved:    rep.Starved,
		Saturated:  rep.Saturated,
		Skipped:    rep.Skipped,
		NoopFlags:  rep.NoopFlags,
	}
	dir := filepath.Join(r.EvalDir, "results", expName)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "report-"+inv+".json"), data, 0o644)
}

// minComparePairs is the floor below which an interval is
// numerology, not inference.
const minComparePairs = 3

// Compare runs the paired estimator over one invocation's records.
// invocation "" requires the record set to hold exactly one; an
// explicit value selects among several. Bootstrap and permutation
// draw from the runner's rng/replicate settings, like the gate.
func (r *Runner) Compare(exp *Experiment, invocation string) (*CompareReport, error) {
	replicates := r.permReplicates()
	rng := r.rng()
	records, err := r.LoadExperimentRecords(exp.Name)
	if err != nil {
		return nil, err
	}
	if len(records) == 0 {
		return nil, fmt.Errorf("compare: no records for experiment %q", exp.Name)
	}

	byInv := map[string][]RunRecord{}
	for _, rec := range records {
		byInv[rec.Invocation] = append(byInv[rec.Invocation], rec)
	}
	if invocation == "" {
		if len(byInv) > 1 {
			return nil, fmt.Errorf("compare: cross-invocation record set (%d invocations) — pairing only holds within one; pass --invocation (available: %s)",
				len(byInv), strings.Join(slices.Sorted(maps.Keys(byInv)), ", "))
		}
		for k := range byInv {
			invocation = k
		}
	}
	recs, ok := byInv[invocation]
	if !ok {
		return nil, fmt.Errorf("compare: invocation %q not in record set (available: %s)",
			invocation, strings.Join(slices.Sorted(maps.Keys(byInv)), ", "))
	}

	// Structural-alarm refusal. The persisted snapshot is exact when
	// present; legacy invocations fall back to record inference —
	// noop via resolved options, starve via conclusive asymmetry.
	noop, err := r.invocationAlarms(exp.Name, invocation, recs)
	if err != nil {
		return nil, err
	}
	trajIDs := slices.Sorted(maps.Keys(groupBy(recs, func(r RunRecord) string { return r.TrajectoryID })))
	null := len(noop) > 0 && len(noop) == len(trajIDs)
	switch {
	case len(noop) > 0 && !null:
		return nil, fmt.Errorf("compare: noop-flag voided the invocation — arms resolved identically on %v", noop)
	}

	// Pair conclusive records on (trajectory, run_index).
	type pairKey struct {
		traj string
		idx  int
	}
	ctrl := map[pairKey]RunRecord{}
	treat := map[pairKey]RunRecord{}
	unmatched := 0
	for _, rec := range recs {
		if !rec.Outcome.Conclusive() {
			continue
		}
		k := pairKey{rec.TrajectoryID, rec.RunIndex}
		switch rec.Arm {
		case ArmControl:
			ctrl[k] = rec
		case ArmTreatment:
			treat[k] = rec
		}
	}
	pairs := map[pairKey][2]RunRecord{}
	for k, c := range ctrl {
		if t, ok := treat[k]; ok {
			pairs[k] = [2]RunRecord{c, t}
		}
	}
	unmatched = len(ctrl) + len(treat) - 2*len(pairs)
	if len(pairs) < minComparePairs {
		return nil, fmt.Errorf("compare: %d conclusive pairs — need ≥%d for an interval (unmatched conclusive runs: %d)",
			len(pairs), minComparePairs, unmatched)
	}

	rep := &CompareReport{
		Experiment:   exp.Name,
		Invocation:   invocation,
		Null:         null,
		Trajectories: trajIDs,
		Pairs:        len(pairs),
		Unmatched:    unmatched,
	}

	// Order: declared primary first, then the rest of the registry.
	names := primaryMetricNames()
	if exp.Primary != nil {
		names = append([]string{exp.Primary.Metric}, slices.DeleteFunc(slices.Clone(names),
			func(n string) bool { return n == exp.Primary.Metric })...)
	}
	noise, _ := r.loadNoiseQuiet()
	var skippedMetrics []string

	for _, name := range names {
		fn, err := primaryMetricFunc(exp, name)
		if err != nil {
			continue // weighted_cost without weights, etc.
		}
		mc := MetricCompare{Name: name}
		trajLogR := map[string][]float64{}
		for k, pr := range pairs {
			if !primaryMeasurable(name, &pr[0]) || !primaryMeasurable(name, &pr[1]) {
				continue
			}
			c, t := fn(&pr[0]), fn(&pr[1])
			if c <= 0 || t <= 0 {
				mc.Dropped++
				continue
			}
			trajLogR[k.traj] = append(trajLogR[k.traj], math.Log(t/c))
		}
		trajs := make([][]float64, 0, len(trajLogR))
		for _, tr := range trajIDs {
			if d, ok := trajLogR[tr]; ok {
				trajs = append(trajs, d)
			}
		}
		mc.Pairs = countPairs(trajs)
		if mc.Pairs < minComparePairs {
			skippedMetrics = append(skippedMetrics,
				fmt.Sprintf("%s (%d measurable)", name, mc.Pairs))
			continue // Too few measurable pairs to estimate on.
		}
		mc.Theta = thetaMean(trajs)
		mc.DeltaPct = pctOf(mc.Theta)
		lo, hi := bcaCI(trajs, replicates, rng)
		mc.CILoPct, mc.CIHiPct = pctOf(lo), pctOf(hi)
		mc.P = signFlipP(trajs, mc.Theta, directionOf(exp, name), replicates, rng)
		if exp.Primary != nil && name == exp.Primary.Metric {
			fillPrimaryVerdict(&mc, exp.Primary, noise[name])
		}
		rep.Metrics = append(rep.Metrics, mc)
	}
	if len(rep.Metrics) == 0 {
		return nil, fmt.Errorf("compare: no metric had ≥%d measurable pairs", minComparePairs)
	}
	rep.SkippedMetrics = skippedMetrics
	return rep, nil
}

// invocationAlarms applies the structural refusals for one
// invocation: persisted snapshot when the run wrote one, else
// records-only inference. Returns the noop trajectory set for the
// null-experiment label; other alarms return refusal errors.
func (r *Runner) invocationAlarms(expName, invocation string, recs []RunRecord) (noop []string, err error) {
	// Noop always derives from records — resolved options are exact
	// on every record, and a partial snapshot (aborted run) can't be
	// trusted to carry it. The snapshot's role is the scheduling
	// alarms records can't show: starved, saturated, skipped.
	noop = noopTrajectories(recs)
	snapPath := filepath.Join(r.EvalDir, "results", expName, "report-"+invocation+".json")
	if data, readErr := os.ReadFile(snapPath); readErr == nil {
		var snap alarmSnapshot
		if json.Unmarshal(data, &snap) == nil {
			switch {
			case len(snap.Starved) > 0:
				return nil, fmt.Errorf("compare: starved voided the invocation — %v exhausted attempts on inconclusive", snap.Starved)
			case len(snap.Saturated) > 0:
				return nil, fmt.Errorf("compare: saturated voided the invocation — %v exhausted attempts on error", snap.Saturated)
			case len(snap.Skipped) > 0:
				return nil, fmt.Errorf("compare: skipped voided the invocation — %v never ran; the corpus wasn't covered", snap.Skipped)
			}
			return noop, nil
		}
	}
	// Legacy records (no snapshot): a starved or saturated arm shows
	// as a conclusive-count asymmetry the scheduler would not
	// produce on its own — both arms target the same n per
	// trajectory.
	for traj, rs := range groupBy(recs, func(r RunRecord) string { return r.TrajectoryID }) {
		var nc, nt int
		for _, rec := range rs {
			if !rec.Outcome.Conclusive() {
				continue
			}
			if rec.Arm == ArmControl {
				nc++
			}
			if rec.Arm == ArmTreatment {
				nt++
			}
		}
		if nc != nt {
			return nil, fmt.Errorf("compare: conclusive asymmetry on %s (control %d, treatment %d) — an arm under-sampled, which is the starve/saturate signature; the invocation is void", traj, nc, nt)
		}
	}
	return noop, nil
}

// noopTrajectories lists trajectories whose control/treatment
// resolved options are identical — the same predicate Evaluate's
// noop-flag uses.
func noopTrajectories(recs []RunRecord) []string {
	byTraj := groupBy(recs, func(r RunRecord) string { return r.TrajectoryID })
	var out []string
	for id, rs := range byTraj {
		var ctrlRes, treatRes map[string]any
		for _, rec := range rs {
			if len(rec.ResolvedOptions) == 0 {
				continue
			}
			switch rec.Arm {
			case ArmControl:
				ctrlRes = rec.ResolvedOptions
			case ArmTreatment:
				treatRes = rec.ResolvedOptions
			}
		}
		if ctrlRes != nil && treatRes != nil && resolvedEqual(ctrlRes, treatRes) {
			out = append(out, id)
		}
	}
	return out
}

// fillPrimaryVerdict applies the MDE decision boundary to the
// primary metric's CI.
func fillPrimaryVerdict(mc *MetricCompare, p *Primary, cv float64) {
	var boundary float64
	if p.Direction == PrimaryDecrease {
		boundary = math.Log(1 - p.MDE)
	} else {
		boundary = math.Log(1 + p.MDE)
	}
	// Back-translate the pct-space CI to theta space for the
	// comparison — cleaner than converting the boundary.
	lo := math.Log(1 + mc.CILoPct/100)
	hi := math.Log(1 + mc.CIHiPct/100)
	effectSide := mc.Theta < boundary
	if p.Direction == PrimaryIncrease {
		effectSide = mc.Theta > boundary
	}
	switch {
	case effectSide && (p.Direction == PrimaryDecrease && hi < boundary || p.Direction == PrimaryIncrease && lo > boundary):
		mc.Verdict = "effect ≥ MDE"
	case !effectSide && (p.Direction == PrimaryDecrease && lo > boundary || p.Direction == PrimaryIncrease && hi < boundary):
		mc.Verdict = "no MDE effect"
	default:
		mc.Verdict = "inconclusive-underpowered"
		if cv > 0 {
			mc.Required = RequiredSamples(cv, p.MDE)
		}
	}
}

// directionOf returns the metric's declared direction — the
// primary's own for the primary metric, "" (two-sided) otherwise.
func directionOf(e *Experiment, name string) string {
	if e.Primary != nil && e.Primary.Metric == name {
		return e.Primary.Direction
	}
	return ""
}

// thetaMean is the estimand: unweighted mean across trajectories of
// each trajectory's mean pair log-ratio.
func thetaMean(trajs [][]float64) float64 {
	var sum float64
	for _, d := range trajs {
		sum += mean(d)
	}
	return sum / float64(len(trajs))
}

func mean(d []float64) float64 {
	var s float64
	for _, v := range d {
		s += v
	}
	return s / float64(len(d))
}

func countPairs(trajs [][]float64) int {
	var n int
	for _, d := range trajs {
		n += len(d)
	}
	return n
}

func pctOf(theta float64) float64 { return (math.Exp(theta) - 1) * 100 }

// bcaCI is a BCa 95% interval over a stratified bootstrap: pairs
// resample within their own trajectory, preserving trajectory
// weights — the same stratification the estimand uses. Jackknife
// over pairs supplies the acceleration term.
func bcaCI(trajs [][]float64, replicates int, rng *rand.Rand) (lo, hi float64) {
	thetaHat := thetaMean(trajs)
	boot := make([]float64, replicates)
	var below int
	for b := range replicates {
		rs := make([][]float64, len(trajs))
		for i, d := range trajs {
			s := make([]float64, len(d))
			for j := range s {
				s[j] = d[rng.IntN(len(d))]
			}
			rs[i] = s
		}
		boot[b] = thetaMean(rs)
		if boot[b] < thetaHat {
			below++
		}
	}
	slices.Sort(boot)

	// Jackknife: leave one pair out at a time, from its trajectory.
	var jk []float64
	for i, d := range trajs {
		for j := range d {
			l := make([][]float64, len(trajs))
			for k := range trajs {
				l[k] = trajs[k]
			}
			l[i] = slices.Delete(slices.Clone(d), j, j+1)
			if len(l[i]) > 0 {
				jk = append(jk, thetaMean(l))
			}
		}
	}
	jkMean := mean(jk)
	var num, den float64
	for _, v := range jk {
		d := jkMean - v
		num += d * d * d
		den += d * d
	}
	var a float64
	if den > 0 {
		a = num / (6 * math.Pow(den, 1.5))
	}

	z0 := normInv(float64(below) / float64(replicates))
	if math.IsInf(z0, 0) || math.IsNaN(z0) {
		z0 = 0 // Degenerate resample (all-equal pairs) — fall back to percentile.
	}
	adj := func(q float64) float64 {
		z := normInv(q)
		return normCDF(z0 + (z0+z)/(1-a*(z0+z)))
	}
	return boot[clampIdx(adj(0.025)*float64(replicates), replicates)],
		boot[clampIdx(adj(0.975)*float64(replicates), replicates)]
}

func clampIdx(x float64, n int) int {
	i := int(x)
	return min(max(i, 0), n-1)
}

// signFlipP is the paired permutation test: under the null, each
// pair's log-ratio is symmetric about 0, so replicates flip every
// pair's sign independently and recompute θ. Exact enumeration when
// 2^pairs is tractable. One-sided in the declared direction when
// given; two-sided (|θ*| ≥ |θ̂|) otherwise.
func signFlipP(trajs [][]float64, observed float64, direction string, replicates int, rng *rand.Rand) float64 {
	var flat []float64
	var sizes []int
	for _, d := range trajs {
		flat = append(flat, d...)
		sizes = append(sizes, len(d))
	}
	stat := func(signs []bool) float64 {
		var rebuilt [][]float64
		var off int
		for _, n := range sizes {
			d := make([]float64, n)
			for j := range d {
				v := flat[off+j]
				if signs[off+j] {
					v = -v
				}
				d[j] = v
			}
			rebuilt = append(rebuilt, d)
			off += n
		}
		return thetaMean(rebuilt)
	}
	extreme := func(t float64) bool {
		switch direction {
		case PrimaryDecrease:
			return t <= observed
		case PrimaryIncrease:
			return t >= observed
		default:
			return math.Abs(t) >= math.Abs(observed)
		}
	}
	n := len(flat)
	var hits, total float64
	if n <= 16 { // 2^16 = 65536 — cheap exact.
		for mask := range 1 << n {
			signs := make([]bool, n)
			for i := range n {
				signs[i] = mask&(1<<i) != 0
			}
			if extreme(stat(signs)) {
				hits++
			}
			total++
		}
	} else {
		for range replicates {
			signs := make([]bool, n)
			for i := range signs {
				signs[i] = rng.IntN(2) == 0
			}
			if extreme(stat(signs)) {
				hits++
			}
			total++
		}
	}
	// Add-one: the unflipped arrangement is itself a legal replicate.
	return (hits + 1) / (total + 1)
}

// normCDF / normInv — standard normal, via math.Erf and a bisection
// inverse (percentile-index precision, not statistical precision).
func normCDF(x float64) float64 { return 0.5 * (1 + math.Erf(x/math.Sqrt2)) }

func normInv(p float64) float64 {
	if p <= 0 {
		return math.Inf(-1)
	}
	if p >= 1 {
		return math.Inf(1)
	}
	lo, hi := -10.0, 10.0
	for range 100 {
		mid := (lo + hi) / 2
		if normCDF(mid) < p {
			lo = mid
		} else {
			hi = mid
		}
	}
	return (lo + hi) / 2
}

// loadNoiseQuiet reads noise.json for the underpowered verdict's
// required-n line. Missing or malformed → nil; compare reports the
// estimate regardless — noise only enriches the verdict.
func (r *Runner) loadNoiseQuiet() (map[string]float64, error) {
	nf, err := LoadNoise(r.EvalDir)
	if err != nil {
		return nil, err
	}
	return nf.CV, nil
}

func groupBy[S ~[]E, E any, K comparable](s S, key func(E) K) map[K]S {
	out := map[K]S{}
	for _, e := range s {
		k := key(e)
		out[k] = append(out[k], e)
	}
	return out
}

// Summary renders the compare report.
func (rep *CompareReport) Summary() string {
	var b strings.Builder
	fmt.Fprintf(&b, "compare: %s @ %s\n", rep.Experiment, rep.Invocation)
	if rep.Null {
		fmt.Fprintln(&b, "  NULL EXPERIMENT — arms resolved identically on every trajectory;")
		fmt.Fprintln(&b, "  these CIs measure the harness's own false-effect magnitude.")
	}
	fmt.Fprintf(&b, "  pairs: %d conclusive (unmatched: %d) across %d trajectories\n",
		rep.Pairs, rep.Unmatched, len(rep.Trajectories))
	fmt.Fprintf(&b, "  %-32s %6s %9s %22s %8s  %s\n", "metric", "pairs", "Δ%", "95% CI (BCa)", "p", "verdict")
	for _, m := range rep.Metrics {
		p := fmt.Sprintf("%.3f", m.P)
		if m.P < 0.001 {
			p = "<0.001"
		}
		fmt.Fprintf(&b, "  %-32s %6d %+8.1f%%  [%+6.1f%%, %+6.1f%%] %8s  %s\n",
			m.Name, m.Pairs, m.DeltaPct, m.CILoPct, m.CIHiPct, p, m.Verdict)
		if m.Dropped > 0 {
			fmt.Fprintf(&b, "  %-32s %6s\n", "", fmt.Sprintf("(%d pairs dropped — nonpositive side)", m.Dropped))
		}
		if m.Verdict == "inconclusive-underpowered" && m.Required > 0 {
			fmt.Fprintf(&b, "  INCONCLUSIVE — underpowered: %s needs n≈%d pairs\n", m.Name, m.Required)
		}
	}
	if len(rep.SkippedMetrics) > 0 {
		fmt.Fprintf(&b, "  skipped (<%d measurable pairs): %s\n",
			minComparePairs, strings.Join(rep.SkippedMetrics, ", "))
	}
	return b.String()
}
