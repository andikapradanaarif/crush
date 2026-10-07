package eval

import (
	"encoding/json"
	"fmt"
	"hash/fnv"
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
// Estimator: per-pair normalized difference d_i = (t_i − c_i)/μ_c —
// the pair's change scaled by its trajectory's mean control value,
// i.e. a per-trajectory ratio-of-means that remains defined when a
// side is 0 (a treatment zero is the effect, not missing data).
// Per-trajectory mean → unweighted mean across trajectories. The
// trajectory is the unit of analysis, so one arm landing
// disproportionately on easy trajectories can't shift the estimate —
// the composition bias the pooled gate strata are subject to. A
// trajectory whose control baseline is nonpositive is dropped for
// that metric (relative change undefined without a baseline). CI is
// BCa over a stratified bootstrap (pairs resampled within their
// trajectory); p is a sign-flip permutation on pair differences
// (each pair's d is symmetric about 0 under the null), one-sided in
// the primary's declared direction, two-sided otherwise. The legacy
// positive-pairs-only log-ratio is reported alongside as a secondary
// view when enough pairs survive it.
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
	// Provenance notes when the primary verdict's pre-registration
	// couldn't be verified against the invocation's snapshot — or
	// why it was suppressed. Empty when clean.
	Provenance string
	// GateVerdict/OutcomeAlarms carry the run's own pass/fail
	// context so a collapsed invocation's token table doesn't read
	// as a win.
	GateVerdict   string
	OutcomeAlarms []string
	// ArmTotals is the survivorship-free spend view — populated only
	// when the experiment pins cost_weights.
	ArmTotals []ArmTotal
	// AcceptanceAlarms carries the compare-level refusals from the
	// #197 cost-acceptance rule (floor-violated, cost-unjustified,
	// cost-unmeasured) — distinct from OutcomeAlarms, which are the
	// run snapshot's own. A clean list means the rule either passed
	// or was never armed.
	AcceptanceAlarms []string
}

// ArmTotal summarizes one arm's spend across every attempted run.
type ArmTotal struct {
	Arm    string
	Runs   int
	Passes int
	// WeightedCost sums all token classes incl. generator_tokens over
	// attempted runs — a failed run's spend is real cost, so the
	// denominator is attempts, not passes.
	WeightedCost   float64
	CostPerAttempt float64
	// CostPerPass is tokens-to-done; 0 when Passes == 0 (read Passes).
	CostPerPass float64
}

// MetricCompare is one metric's paired estimate.
type MetricCompare struct {
	Name  string
	Pairs int
	// Trajectories is how many trajectories contributed ≥1 pair —
	// the estimand averages over this many strata, and partial
	// coverage weakens the composition-bias defense.
	Trajectories int
	// Dropped counts pairs in trajectories with a nonpositive control
	// baseline — the relative estimand is undefined there (e.g.
	// tokens.cache_read on a non-caching provider).
	Dropped int
	// Absent counts pairs skipped because telemetry was absent on a
	// side (nil Request/CallMetrics/GeneratorTokens) — distinct from
	// Dropped: absent means the run never produced the datum.
	Absent int
	// Zeroes counts estimated pairs where an arm's value was 0 —
	// kept by the normalized-difference estimand; a zero treatment
	// mean is often the effect itself (e.g. discovery eliminated).
	Zeroes int
	// NoBaselinePos counts pairs inside no-baseline (μ_c ≤ 0)
	// trajectories whose treatment value was positive — a regression
	// class the relative estimand structurally cannot see (treatment
	// invented calls where control made none) and that would
	// otherwise vanish into Dropped.
	NoBaselinePos int
	// MinBaseline is the smallest trajectory control mean among the
	// estimated trajectories — thin baselines amplify every
	// normalized diff, so the reader needs the floor visible.
	MinBaseline float64
	// CtrlMean/TreatMean are the mean arm values averaged across
	// estimated trajectories — the absolute magnitudes the Δ% is
	// relative to (Δ% itself is a mean of per-trajectory ratios, not
	// the ratio of these means — they can diverge under
	// heterogeneous baselines).
	CtrlMean  float64
	TreatMean float64
	// LogRatioPct/LogRatioPairs carry the legacy estimand — mean of
	// per-pair log-ratios over strictly-positive pairs — as a
	// secondary view when ≥minComparePairs survive.
	LogRatioPct   float64
	LogRatioPairs int
	Theta         float64 // mean normalized difference across trajectories.
	DeltaPct      float64
	CILoPct       float64
	CIHiPct       float64
	P             float64
	// Verdict is set only on the primary metric: "effect" (CI clears
	// the MDE boundary), "no-mde-effect" (CI clears on the null
	// side), or "inconclusive" (CI spans the boundary — Required
	// then holds the powered sample size; inconclusive means the
	// true effect may sit below the bound, not merely too few runs).
	Verdict  string
	Required int
	// Guardrail carries the primary's max_pass_drop outcome:
	// "ok", "violated (...)", or "unevaluable (...)" — empty when
	// undeclared.
	Guardrail string
	// Acceptance carries the #197 cost-justified acceptance read on
	// the primary row — the floor check plus the ΔS-band-routed ΔC
	// bound, e.g. "ok (pass .97 ≥ .90; ΔS beyond δ; ΔC −22% ≤ β₀)".
	// Empty when the rule is unarmed.
	Acceptance string
}

// alarmSnapshot is the refusal/provenance record persisted as
// results/<exp>/report-<invocation>.json so compare can refuse on
// the run's own verdicts instead of re-inferring them. Aborted is
// the completeness bit: set whenever RunExperiment returned an
// error — aborts and cancellations included — so a partial record
// set can never masquerade as a finished invocation.
type alarmSnapshot struct {
	Invocation string `json:"invocation"`
	Aborted    string `json:"aborted,omitempty"`
	// Primary pins the declaration in force at run time — compare
	// suppresses the verdict when the loaded experiment's primary
	// has drifted since, closing the post-hoc-MDE hole.
	Primary *Primary `json:"primary,omitempty"`
	// GateVerdict/OutcomeAlarms are context, not refusals: a
	// catastrophic-collapsed invocation's token table shouldn't read
	// as a win without its gate result beside it.
	GateVerdict   string   `json:"gate_verdict,omitempty"`
	OutcomeAlarms []string `json:"outcome_alarms,omitempty"`
	Starved       []string `json:"starved,omitempty"`
	Saturated     []string `json:"saturated,omitempty"`
	Tolerated     []string `json:"tolerated,omitempty"`
	Skipped       []string `json:"skipped,omitempty"`
	NoopFlags     []string `json:"noop_flags,omitempty"`
}

// persistAlarms writes the structural-alarm snapshot for this
// invocation. The full Report stays process-local (its Summary is
// the run output); the snapshot is compare's refusal ground truth.
func (r *Runner) persistAlarms(exp *Experiment, inv string, rep Report, retErr error) error {
	snap := alarmSnapshot{
		Invocation: inv,
		Primary:    exp.Primary,
		Starved:    rep.Starved,
		Saturated:  rep.Saturated,
		Tolerated:  rep.Tolerated,
		Skipped:    rep.Skipped,
		NoopFlags:  rep.NoopFlags,
	}
	if retErr != nil {
		snap.Aborted = retErr.Error()
	}
	if rep.Fired(r.alpha()) {
		snap.GateVerdict = "fail"
	} else if !rep.Powered() {
		snap.GateVerdict = "inconclusive"
	} else {
		snap.GateVerdict = "pass"
	}
	if len(rep.Catastrophic) > 0 {
		snap.OutcomeAlarms = append(snap.OutcomeAlarms, "catastrophic: "+strings.Join(rep.Catastrophic, ", "))
	}
	if len(rep.ExcludedDifferential) > 0 {
		snap.OutcomeAlarms = append(snap.OutcomeAlarms, "excluded-differential: "+strings.Join(rep.ExcludedDifferential, ", "))
	}
	if len(rep.ExpectedExclusionMissed) > 0 {
		snap.OutcomeAlarms = append(snap.OutcomeAlarms, "expected-exclusion-missed: "+strings.Join(rep.ExpectedExclusionMissed, ", "))
	}
	if len(rep.ExpectedExclusionSatisfied) > 0 {
		snap.OutcomeAlarms = append(snap.OutcomeAlarms, "expected-exclusion met: "+strings.Join(rep.ExpectedExclusionSatisfied, ", "))
	}
	if len(rep.Tolerated) > 0 {
		snap.OutcomeAlarms = append(snap.OutcomeAlarms, "tolerated transient noise: "+strings.Join(rep.Tolerated, ", "))
	}
	if len(rep.Coincident) > 0 {
		snap.OutcomeAlarms = append(snap.OutcomeAlarms, "coincident-collapse: "+strings.Join(rep.Coincident, ", "))
	}
	if rep.DiffuseP < r.alpha() {
		snap.OutcomeAlarms = append(snap.OutcomeAlarms, fmt.Sprintf("diffuse p=%.3g", rep.DiffuseP))
	}
	dir := filepath.Join(r.EvalDir, "results", exp.Name)
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
// draw replicates from the runner's settings; when the runner has no
// injected RNG the seed derives from the invocation ID, so identical
// data yields identical intervals.
func (r *Runner) Compare(exp *Experiment, invocation string) (*CompareReport, error) {
	// The eval lock also covers readers — an in-flight run appends
	// records as it goes, and a torn read could see a balanced
	// prefix of an invocation whose snapshot doesn't exist yet.
	unlock, err := r.acquireLock()
	if err != nil {
		return nil, err
	}
	defer unlock()

	replicates := r.permReplicates()
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
	// present; legacy invocations fall back to record inference.
	snap, noop, err := r.invocationAlarms(exp.Name, invocation, recs, exp.ExpectedExclusion)
	if err != nil {
		return nil, err
	}
	trajIDs := slices.Sorted(maps.Keys(groupBy(recs, func(r RunRecord) string { return r.TrajectoryID })))
	null := len(noop) > 0 && len(noop) == len(trajIDs)
	switch {
	case len(noop) > 0 && !null:
		return nil, fmt.Errorf("compare: noop-flag voided the invocation — arms resolved identically on %v", noop)
	}

	// Primary provenance: the verdict label is the pre-committed
	// part — it only prints when the declaration in force now
	// matches what the invocation ran under.
	primaryTrusted := true
	var provNote string
	if snap == nil {
		// Legacy invocation — no snapshot to verify against.
		if exp.Primary != nil {
			provNote = "no alarm snapshot — primary provenance unverifiable"
		}
	} else {
		switch {
		case snap.Primary == nil && exp.Primary != nil:
			primaryTrusted = false
			provNote = "primary declared after the invocation ran — verdict suppressed (post-hoc)"
		case snap.Primary != nil && exp.Primary != nil && !samePrimary(snap.Primary, exp.Primary):
			primaryTrusted = false
			provNote = fmt.Sprintf("primary drifted since the invocation (ran with %s %s %.3g) — verdict suppressed",
				snap.Primary.Metric, snap.Primary.Direction, snap.Primary.MDE)
		case snap.Primary != nil && exp.Primary == nil:
			provNote = fmt.Sprintf("primary was declared at run time (%s %s %.3g) and has since been removed",
				snap.Primary.Metric, snap.Primary.Direction, snap.Primary.MDE)
		}
	}

	rng := r.RNG
	if rng == nil {
		// Deterministic per invocation: identical data → identical
		// intervals, no nanotime jitter between compare calls.
		h := fnv.New64a()
		h.Write([]byte(invocation))
		rng = rand.New(rand.NewPCG(h.Sum64(), 0))
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
	// Deterministic pair order — the FNV-seeded bootstrap reuses
	// diff positions, so identical data must build identical diffs.
	pairKeys := slices.SortedFunc(maps.Keys(pairs), func(a, b pairKey) int {
		if c := strings.Compare(a.traj, b.traj); c != 0 {
			return c
		}
		return a.idx - b.idx
	})

	rep := &CompareReport{
		Experiment:   exp.Name,
		Invocation:   invocation,
		Null:         null,
		Trajectories: trajIDs,
		Pairs:        len(pairs),
		Unmatched:    unmatched,
		Provenance:   provNote,
	}
	if snap != nil {
		rep.GateVerdict = snap.GateVerdict
		rep.OutcomeAlarms = snap.OutcomeAlarms
	}
	if costFn, err := primaryMetricFunc(exp, "weighted_cost"); err == nil {
		rep.ArmTotals = armTotals(recs, costFn)
	}

	// Order: declared primary first, then the rest of the registry.
	names := primaryMetricNames()
	if exp.Primary != nil {
		names = append([]string{exp.Primary.Metric}, slices.DeleteFunc(slices.Clone(names),
			func(n string) bool { return n == exp.Primary.Metric })...)
	}
	noise, noiseErr := LoadNoise(r.EvalDir)
	var skippedMetrics []string

	for _, name := range names {
		fn, err := primaryMetricFunc(exp, name)
		if err != nil {
			// Unresolvable metrics (weighted_cost without weights)
			// surface like unmeasurable ones — the primary included:
			// its row is absent but the reason isn't.
			skippedMetrics = append(skippedMetrics,
				fmt.Sprintf("%s (unresolvable: %v)", name, err))
			continue
		}
		mc := MetricCompare{Name: name}
		// Accumulate per trajectory: aligned raw (control, treatment)
		// pair values — the bootstrap resamples these and
		// re-normalizes per replicate, so baseline estimation noise
		// enters the CI — plus the strictly-positive pairs for the
		// legacy log-ratio view.
		type acc struct {
			c, t   []float64
			posLog []float64
			cSum   float64
			tSum   float64
			zeroes int
			tPos   int
		}
		perTraj := map[string]*acc{}
		for _, k := range pairKeys {
			pr := pairs[k]
			if !primaryMeasurable(name, &pr[0]) || !primaryMeasurable(name, &pr[1]) {
				mc.Absent++
				continue
			}
			c, t := fn(&pr[0]), fn(&pr[1])
			a := perTraj[k.traj]
			if a == nil {
				a = &acc{}
				perTraj[k.traj] = a
			}
			// (0,0) pairs are real observations — both arms did
			// nothing — contributing d=0, not absence.
			a.c = append(a.c, c)
			a.t = append(a.t, t)
			a.cSum += c
			a.tSum += t
			if t > 0 {
				a.tPos++
			}
			if c <= 0 || t <= 0 {
				a.zeroes++
			} else {
				a.posLog = append(a.posLog, math.Log(t/c))
			}
		}
		trajs := make([][]float64, 0, len(perTraj))
		raws := make([]trajPairs, 0, len(perTraj))
		posTrajs := make([][]float64, 0, len(perTraj))
		var nTraj int
		mc.MinBaseline = math.Inf(1)
		for _, tr := range trajIDs {
			a := perTraj[tr]
			if a == nil {
				continue
			}
			mu := a.cSum / float64(len(a.c))
			if mu <= 0 {
				// No positive control baseline — relative change
				// undefined for this trajectory. A positive
				// treatment sum here is an invisible regression:
				// count it separately rather than letting it
				// vanish into Dropped.
				mc.Dropped += len(a.c)
				mc.NoBaselinePos += a.tPos
				continue
			}
			d := make([]float64, len(a.c))
			for i := range d {
				d[i] = (a.t[i] - a.c[i]) / mu
			}
			trajs = append(trajs, d)
			raws = append(raws, trajPairs{c: a.c, t: a.t, cSum: a.cSum, tSum: a.tSum})
			if len(a.posLog) > 0 {
				posTrajs = append(posTrajs, a.posLog)
			}
			mc.Zeroes += a.zeroes
			mc.CtrlMean += mu
			mc.TreatMean += a.tSum / float64(len(a.c))
			mc.MinBaseline = min(mc.MinBaseline, mu)
			nTraj++
		}
		if nTraj > 0 {
			mc.CtrlMean /= float64(nTraj)
			mc.TreatMean /= float64(nTraj)
		}
		if n := countPairs(posTrajs); n >= minComparePairs {
			mc.LogRatioPct = pctOf(thetaMean(posTrajs))
			mc.LogRatioPairs = n
		}
		mc.Pairs = countPairs(trajs)
		mc.Trajectories = len(trajs)
		if mc.Pairs < minComparePairs {
			skippedMetrics = append(skippedMetrics,
				fmt.Sprintf("%s (%d measurable, %d no-baseline, %d absent)", name, mc.Pairs, mc.Dropped, mc.Absent))
			continue // Too few measurable pairs to estimate on.
		}
		mc.Theta = thetaMeanRaw(raws)
		mc.DeltaPct = mc.Theta * 100
		lo, hi := bcaCI(raws, replicates, rng)
		mc.CILoPct, mc.CIHiPct = lo*100, hi*100
		// The permutation test deliberately normalizes on the
		// observed baseline: conditioning on μ̂ is defensible under
		// permutation (positive scaling preserves sign symmetry),
		// unlike the interval, which must propagate baseline noise.
		mc.P = signFlipP(trajs, mc.Theta, directionOf(exp, name), replicates, rng)
		if exp.Primary != nil && name == exp.Primary.Metric {
			switch {
			case primaryTrusted:
				fillPrimaryVerdict(&mc, exp.Primary, trajs, noiseCV(noise, noiseErr, name))
				if exp.Primary.MaxPassDrop > 0 {
					mc.Guardrail = passGuardrail(recs, exp.Primary.MaxPassDrop)
					// A violated guardrail un-stands the verdict —
					// "cheaper but failing more" is not the declared
					// decision.
					if strings.HasPrefix(mc.Guardrail, "violated") && mc.Verdict != "" {
						mc.Verdict += " — GUARDRAIL VIOLATED"
					}
				}
			case exp.Primary.MaxPassDrop > 0:
				// The declared bound may have drifted with the
				// suppressed verdict — show the check's presence,
				// not a number the run wasn't committed to.
				mc.Guardrail = "unevaluable (provenance untrusted)"
			}
		}
		rep.Metrics = append(rep.Metrics, mc)
	}
	if len(rep.Metrics) == 0 {
		return nil, fmt.Errorf("compare: no metric had ≥%d measurable pairs", minComparePairs)
	}
	rep.SkippedMetrics = skippedMetrics
	rep.applyAcceptance(exp, recs, primaryTrusted)
	return rep, nil
}

// invocationAlarms applies the structural refusals for one
// invocation. The persisted snapshot is authoritative for what
// records can't show (aborted, starved, saturated, skipped, and
// primary provenance); record inference always runs alongside it —
// conclusive asymmetry is ground truth a clean-looking snapshot can
// still be missing (a torn write, a pre-snapshot defect). Returns
// the parsed snapshot (nil for legacy invocations) and the noop
// trajectory set for the null-experiment label.
func (r *Runner) invocationAlarms(expName, invocation string, recs []RunRecord, expectedExclusion *ExpectedExclusion) (snap *alarmSnapshot, noop []string, err error) {
	// Noop always derives from records — resolved options are exact
	// on every record, and a partial snapshot (aborted run) can't be
	// trusted to carry it.
	noop = noopTrajectories(recs)
	snapPath := filepath.Join(r.EvalDir, "results", expName, "report-"+invocation+".json")
	if data, readErr := os.ReadFile(snapPath); readErr == nil {
		var s alarmSnapshot
		if json.Unmarshal(data, &s) == nil {
			snap = &s
		}
	}
	if snap != nil {
		switch {
		case snap.Aborted != "":
			return nil, nil, fmt.Errorf("compare: invocation did not complete (%s) — a partial corpus can't carry a verdict", snap.Aborted)
		case len(snap.Starved) > 0:
			return nil, nil, fmt.Errorf("compare: starved voided the invocation — %v exhausted attempts on inconclusive", snap.Starved)
		case len(snap.Saturated) > 0:
			return nil, nil, fmt.Errorf("compare: saturated voided the invocation — %v exhausted attempts on error", snap.Saturated)
		case len(snap.Skipped) > 0:
			return nil, nil, fmt.Errorf("compare: skipped voided the invocation — %v never ran; the corpus wasn't covered", snap.Skipped)
		}
	}
	// A starved or saturated arm shows as a conclusive-count
	// asymmetry the scheduler would not produce on its own — both
	// arms target the same n per trajectory. Runs even when a
	// snapshot parsed: the check is ground truth the snapshot can
	// only redundantly confirm.
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
			// A declared expected exclusion explains the shortfall:
			// the under-sampled arm dying entirely the declared way
			// at-or-beyond min is the designed condition. Shortfalls
			// the declaration doesn't cover keep the refusal.
			under := ArmControl
			if nt < nc {
				under = ArmTreatment
			}
			if d := expectedExclusion; d == nil || d.Arm != under ||
				!exclusionShortfallExplained(rs, under, d) {
				return nil, nil, fmt.Errorf("compare: conclusive asymmetry on %s (control %d, treatment %d) — an arm under-sampled, which is the starve/saturate signature; the invocation is void", traj, nc, nt)
			}
		}
	}
	return snap, noop, nil
}

// exclusionShortfallExplained reports whether an arm's conclusive
// shortfall is fully accounted for by the declared exclusion —
// every non-conclusive record on the arm carries the declared error
// class or a transient-infrastructure class (weather, not an
// unexplained death) and the declared-class count meets the declared
// minimum.
func exclusionShortfallExplained(rs []RunRecord, arm string, d *ExpectedExclusion) bool {
	declared := 0
	for _, rec := range rs {
		if rec.Arm != arm || rec.Outcome.Conclusive() {
			continue
		}
		if rec.Outcome != OutcomeError ||
			(rec.ErrorClass != d.ErrorClass && !infraTransientClasses[rec.ErrorClass]) {
			return false
		}
		if rec.ErrorClass == d.ErrorClass {
			declared++
		}
	}
	return declared >= d.Min
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

// noiseCV pulls a metric's recorded CV, or 0 when the noise file is
// missing/malformed/entryless — the underpowered verdict degrades
// to a printed "unknown" rather than a silently absent number.
func noiseCV(n *NoiseFile, err error, metric string) float64 {
	if err != nil || n == nil {
		return 0
	}
	return n.CV[metric]
}

// fillPrimaryVerdict applies the MDE decision boundary to the
// primary metric's CI. The normalized-difference estimand is already
// in relative units, so the boundary is ±MDE directly and a CI
// spanning it is "inconclusive" — the true effect may simply be
// below the bound; underpowered is only the case where a powered n
// would tighten it.
func fillPrimaryVerdict(mc *MetricCompare, p *Primary, trajs [][]float64, cv float64) {
	var boundary float64
	if p.Direction == PrimaryDecrease {
		boundary = -p.MDE
	} else {
		boundary = p.MDE
	}
	// The pct-space CI translates to fraction space by /100.
	lo := mc.CILoPct / 100
	hi := mc.CIHiPct / 100
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
		mc.Verdict = "inconclusive"
		mc.Required = requiredPairs(trajs, p.MDE, cv)
	}
}

// requiredPairs prices the paired design against its own noise:
// n = ⌈(zα+zβ)²·Var(dᵢ)/δ²⌉ with δ = mde — the normalized-diff
// estimand is already in relative units. The estimand weights
// trajectories equally, so Var(dᵢ) averages the within-trajectory
// variances — pooling the flat array would fold between-trajectory
// baseline spread into the noise and overstate n. A degenerate or
// unmeasurable variance falls back to the pooled-CV n so the verdict
// never prints an absent n.
func requiredPairs(trajs [][]float64, mde float64, cv float64) int {
	delta := mde
	if delta > 0 {
		var sum, cnt float64
		for _, d := range trajs {
			if len(d) < 2 {
				continue
			}
			m := mean(d)
			var ss float64
			for _, v := range d {
				x := v - m
				ss += x * x
			}
			sum += ss / float64(len(d)-1)
			cnt++
		}
		if cnt > 0 && sum > 0 {
			return int(math.Ceil(6.186 * (sum / cnt) / (delta * delta)))
		}
	}
	if cv > 0 {
		return RequiredSamples(cv, mde)
	}
	return 0
}

// directionOf returns the metric's declared direction — the
// primary's own for the primary metric, "" (two-sided) otherwise.
func directionOf(e *Experiment, name string) string {
	if e.Primary != nil && e.Primary.Metric == name {
		return e.Primary.Direction
	}
	return ""
}

// trajPairs carries one trajectory's raw (control, treatment) pair
// values and their sums — the bootstrap resamples these and
// re-normalizes each replicate by its own resampled baseline, so
// denominator noise enters the interval rather than being frozen at
// the observed μ̂.
type trajPairs struct {
	c, t       []float64
	cSum, tSum float64
}

// thetaMeanRaw is the estimand computed from raw pair sums:
// per-trajectory (Σt−Σc)/Σc — identical to the mean of normalized
// diffs on observed data — averaged unweighted across trajectories.
// Callers must exclude non-positive-baseline trajectories upstream.
func thetaMeanRaw(trajs []trajPairs) float64 {
	var sum float64
	for _, tr := range trajs {
		sum += (tr.tSum - tr.cSum) / tr.cSum
	}
	return sum / float64(len(trajs))
}

// thetaMean is the estimand: unweighted mean across trajectories of
// each trajectory's mean normalized pair difference.
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

// bcaCI is a BCa 95% interval over a stratified bootstrap on raw
// pairs: (c,t) values resample jointly within their own trajectory,
// and each replicate re-normalizes by its own resampled control sum —
// preserving trajectory weights and propagating baseline estimation
// noise into the interval (θ̂ is a ratio-of-means; treating μ̂_c as a
// constant would capture numerator noise only). A replicate whose
// resampled control sum is non-positive has no defined baseline and
// is skipped rather than floored on a fabricated denominator.
// Jackknife at the estimand's own unit supplies the acceleration term.
func bcaCI(trajs []trajPairs, replicates int, rng *rand.Rand) (lo, hi float64) {
	thetaHat := thetaMeanRaw(trajs)
	boot := make([]float64, 0, replicates)
	var below int
	for range replicates {
		var sum float64
		degenerate := false
		for _, tr := range trajs {
			var cs, ts float64
			for range len(tr.c) {
				j := rng.IntN(len(tr.c))
				cs += tr.c[j]
				ts += tr.t[j]
			}
			if cs <= 0 {
				degenerate = true
				break
			}
			sum += (ts - cs) / cs
		}
		if degenerate {
			continue
		}
		theta := sum / float64(len(trajs))
		boot = append(boot, theta)
		if theta < thetaHat {
			below++
		}
	}
	if len(boot) == 0 {
		// Every replicate resampled an all-zero baseline — no
		// resample variance is expressible; collapse to the point.
		return thetaHat, thetaHat
	}
	slices.Sort(boot)

	// Whole-trajectory deletion when ≥2 trajectories exist — the only
	// unit that sees single-pair-trajectory leverage. A lone
	// trajectory falls back to pair-level deletion, skipping
	// deletions that zero the baseline.
	var jk []float64
	if len(trajs) >= 2 {
		for i := range trajs {
			jk = append(jk, thetaMeanRaw(slices.Delete(slices.Clone(trajs), i, i+1)))
		}
	} else {
		tr := trajs[0]
		for j := range tr.c {
			cs := tr.cSum - tr.c[j]
			if cs <= 0 {
				continue
			}
			jk = append(jk, (tr.tSum-tr.t[j]-cs)/cs)
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

	z0 := normInv(float64(below) / float64(len(boot)))
	if math.IsInf(z0, 0) || math.IsNaN(z0) {
		z0 = 0 // Degenerate resample (all-equal pairs) — fall back to percentile.
	}
	adj := func(q float64) float64 {
		z := normInv(q)
		return normCDF(z0 + (z0+z)/(1-a*(z0+z)))
	}
	return boot[clampIdx(adj(0.025)*float64(len(boot)), len(boot))],
		boot[clampIdx(adj(0.975)*float64(len(boot)), len(boot))]
}

func clampIdx(x float64, n int) int {
	i := int(x)
	return min(max(i, 0), n-1)
}

// signFlipP is the paired permutation test: under the null, each
// pair's difference is symmetric about 0, so replicates flip every
// pair's sign independently and recompute θ. Operates on diffs
// normalized by the observed baseline — conditioning on μ̂ is
// defensible under permutation since positive scaling preserves
// sign symmetry. Exact enumeration when 2^pairs is tractable.
// One-sided in the declared direction when given; two-sided
// (|θ*| ≥ |θ̂|) otherwise.
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
	if n <= 16 { // 2^16 = 65536 — cheap exact.
		var hits float64
		for mask := range 1 << n {
			signs := make([]bool, n)
			for i := range n {
				signs[i] = mask&(1<<i) != 0
			}
			if extreme(stat(signs)) {
				hits++
			}
		}
		// Exact enumeration: the observed arrangement is already
		// enumerated — no add-one correction (that convention is for
		// Monte-Carlo draws).
		return hits / float64(int64(1)<<n)
	}
	var hits float64
	for range replicates {
		signs := make([]bool, n)
		for i := range signs {
			signs[i] = rng.IntN(2) == 0
		}
		if extreme(stat(signs)) {
			hits++
		}
	}
	// Add-one: the observed arrangement is itself a legal replicate
	// the draw may have missed — the estimate never reports p=0.
	return (hits + 1) / (float64(replicates) + 1)
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

// armTotals aggregates weighted spend per arm over every attempted
// run — including error/inconclusive attempts, whose tokens are real
// cost even though they form no pair. Sorted by arm name; any arm
// beyond control/treatment (e.g. a mask-only comparator arm) gets a
// row here even though it never pairs.
func armTotals(recs []RunRecord, costFn func(*RunRecord) float64) []ArmTotal {
	byArm := groupBy(recs, func(r RunRecord) string { return r.Arm })
	out := make([]ArmTotal, 0, len(byArm))
	for _, arm := range slices.Sorted(maps.Keys(byArm)) {
		at := ArmTotal{Arm: arm}
		for _, rec := range byArm[arm] {
			at.Runs++
			if rec.Outcome == OutcomePass {
				at.Passes++
			}
			at.WeightedCost += costFn(&rec)
		}
		if at.Runs > 0 {
			at.CostPerAttempt = at.WeightedCost / float64(at.Runs)
		}
		if at.Passes > 0 {
			at.CostPerPass = at.WeightedCost / float64(at.Passes)
		}
		out = append(out, at)
	}
	return out
}

// conclusiveRates is the shared pass-rate read: pass/conclusive per
// arm (errors and inconclusives are not outcomes).
func conclusiveRates(recs []RunRecord) (cRate, tRate float64, cConc, tConc int) {
	var cPass, tPass int
	for _, rec := range recs {
		if !rec.Outcome.Conclusive() {
			continue
		}
		switch rec.Arm {
		case ArmControl:
			cConc++
			if rec.Outcome == OutcomePass {
				cPass++
			}
		case ArmTreatment:
			tConc++
			if rec.Outcome == OutcomePass {
				tPass++
			}
		}
	}
	if cConc > 0 {
		cRate = float64(cPass) / float64(cConc)
	}
	if tConc > 0 {
		tRate = float64(tPass) / float64(tConc)
	}
	return cRate, tRate, cConc, tConc
}

// passGuardrail evaluates the primary's max_pass_drop: treatment's
// conclusive pass rate may trail control's by at most maxDrop.
// Errors and inconclusives don't count as outcomes — the rate is
// pass / conclusive on each side.
func passGuardrail(recs []RunRecord, maxDrop float64) string {
	cRate, tRate, cConc, tConc := conclusiveRates(recs)
	if cConc == 0 || tConc == 0 {
		return fmt.Sprintf("unevaluable (control %d, treatment %d conclusive)", cConc, tConc)
	}
	if drop := cRate - tRate; drop > maxDrop {
		return fmt.Sprintf("violated (pass %.2f vs %.2f, drop %.2f > %.2f)", tRate, cRate, drop, maxDrop)
	}
	return fmt.Sprintf("ok (pass %.2f vs %.2f)", tRate, cRate)
}

// applyAcceptance runs the #197 cost-justified acceptance rule on the
// primary row, after the metric loop has priced every registry entry.
// Armed when primary pins any of floor/noise_band/base_cost_allowance
// (floor then required, δ defaults to MDE, β₀ to 0.10 — enforced at
// load). The rule, in order:
//
//  1. Floor — each arm's conclusive pass rate must clear it; a breach
//     fails acceptance regardless of cost (token savings on a
//     regressing arm are not savings, and a control-side breach means
//     the corpus never produced the regime).
//  2. ΔS beyond δ in the declared direction — a real effect may carry
//     flat cost growth up to β₀ (not proportional: proportional
//     allowance is the circular RRSI sketch).
//  3. |ΔS| within δ — the effect is indistinguishable from noise, so
//     the feature may only ship at ΔC ≤ 0; a cheaper treatment is the
//     efficiency tie-break.
//
// ΔC is the paired weighted_cost estimand, which already prices every
// token class incl. generator_tokens — a mechanism that outsources
// spend to the sidecar can't hide it. A regression beyond δ needs no
// cost read: the primary verdict already fails it.
func (rep *CompareReport) applyAcceptance(exp *Experiment, recs []RunRecord, primaryTrusted bool) {
	p := exp.Primary
	if p == nil || (p.Floor == nil && p.NoiseBand == nil && p.BaseCostAllowance == nil) {
		return
	}
	var pm, cm *MetricCompare
	for i := range rep.Metrics {
		switch rep.Metrics[i].Name {
		case p.Metric:
			pm = &rep.Metrics[i]
		case "weighted_cost":
			cm = &rep.Metrics[i]
		}
	}
	if !primaryTrusted {
		// The declared bounds may have drifted with the suppressed
		// verdict — show the check's presence, not numbers the run
		// wasn't committed to (the guardrail's drift handling).
		if pm != nil {
			pm.Acceptance = "unevaluable (provenance untrusted)"
		}
		return
	}
	floor := 0.0
	if p.Floor != nil {
		floor = *p.Floor
	}
	delta := p.MDE
	if p.NoiseBand != nil {
		delta = *p.NoiseBand
	}
	allow := 0.10
	if p.BaseCostAllowance != nil {
		allow = *p.BaseCostAllowance
	}
	stamp := func(m *MetricCompare, label, detail string) {
		if m != nil {
			m.Acceptance = detail
			if label != "" && m.Verdict != "" {
				m.Verdict += " — " + label
			}
		}
		if label != "" {
			rep.AcceptanceAlarms = append(rep.AcceptanceAlarms, detail)
		}
	}

	// 1. Floor — the hard quality bound, cost-independent.
	cRate, tRate, cConc, tConc := conclusiveRates(recs)
	if tConc == 0 || cConc == 0 {
		stamp(pm, "FLOOR VIOLATED", fmt.Sprintf("floor-violated (unevaluable pass rate — control %d, treatment %d conclusive)", cConc, tConc))
		return
	}
	if tRate < floor {
		stamp(pm, "FLOOR VIOLATED", fmt.Sprintf("floor-violated (treatment pass %.2f < floor %.2f)", tRate, floor))
		return
	}
	if cRate < floor {
		stamp(pm, "FLOOR VIOLATED", fmt.Sprintf("floor-violated (control pass %.2f < floor %.2f — baseline below the quality floor)", cRate, floor))
		return
	}
	floorNote := fmt.Sprintf("pass %.2f ≥ floor %.2f", tRate, floor)

	// 2/3. Cost leg — routed by where ΔS sits against δ.
	if pm == nil {
		rep.AcceptanceAlarms = append(rep.AcceptanceAlarms,
			"cost rule unevaluable — primary metric had no measurable pairs")
		return
	}
	if p.Metric == "weighted_cost" {
		stamp(pm, "", floorNote+"; ΔC is ΔS — cost rule n/a when the primary is the cost metric")
		return
	}
	if cm == nil {
		rep.AcceptanceAlarms = append(rep.AcceptanceAlarms,
			"cost-unmeasured (weighted_cost produced no row — unpriced spend can't be bounded)")
		stamp(pm, "", floorNote+"; ΔC unmeasured — acceptance can't be granted on an unpriced cost leg")
		return
	}
	dS := pm.Theta
	if p.Direction == PrimaryDecrease {
		dS = -dS // Positive dS is improvement in the declared direction.
	}
	dC := cm.Theta // Positive dC is cost growth.
	switch {
	case dS > delta:
		if dC > allow {
			stamp(pm, "COST-UNJUSTIFIED", fmt.Sprintf("cost-unjustified (ΔS %+.0f%% beyond δ %.0f%%; ΔC %+.0f%% > β₀ %.0f%%)", dS*100, delta*100, dC*100, allow*100))
		} else {
			stamp(pm, "", fmt.Sprintf("ok (%s; ΔS %+.0f%% beyond δ %.0f%%; ΔC %+.0f%% ≤ β₀ %.0f%%)", floorNote, dS*100, delta*100, dC*100, allow*100))
		}
	case dS < -delta:
		stamp(pm, "", fmt.Sprintf("%s; ΔS %+.0f%% regresses beyond δ — no cost read rescues a quality loss", floorNote, dS*100))
	default:
		if dC > 0 {
			stamp(pm, "COST-UNJUSTIFIED", fmt.Sprintf("cost-unjustified (ΔS %+.0f%% within δ ±%.0f%%; ΔC %+.0f%% > 0 — noise-level effects may only ship cost-neutral)", dS*100, delta*100, dC*100))
		} else {
			stamp(pm, "", fmt.Sprintf("ok (%s; ΔS %+.0f%% within δ ±%.0f%%; ΔC %+.0f%% ≤ 0 — efficiency tie-break favors treatment)", floorNote, dS*100, delta*100, dC*100))
		}
	}
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
	if rep.GateVerdict != "" {
		line := fmt.Sprintf("  gate verdict: %s", rep.GateVerdict)
		if len(rep.OutcomeAlarms) > 0 {
			line += " (" + strings.Join(rep.OutcomeAlarms, "; ") + ")"
		}
		fmt.Fprintln(&b, line)
	}
	if rep.Null {
		fmt.Fprintln(&b, "  NULL EXPERIMENT — arms resolved identically on every trajectory;")
		fmt.Fprintln(&b, "  these CIs measure the harness's own false-effect magnitude.")
	}
	if rep.Provenance != "" {
		fmt.Fprintf(&b, "  provenance: %s\n", rep.Provenance)
	}
	fmt.Fprintf(&b, "  pairs: %d conclusive (unmatched: %d) across %d trajectories\n",
		rep.Pairs, rep.Unmatched, len(rep.Trajectories))
	fmt.Fprintln(&b, "  Δ% is the mean of per-trajectory normalized diffs — the ctrl≈/treat≈")
	fmt.Fprintln(&b, "  means below are magnitude context, not the ratio's numerator/denominator.")
	fmt.Fprintf(&b, "  %-32s %6s %6s %9s %22s %8s  %s\n", "metric", "pairs", "trajs", "Δ%", "95% CI (BCa)", "p", "verdict")
	for _, m := range rep.Metrics {
		p := fmt.Sprintf("%.3f", m.P)
		if m.P < 0.001 {
			p = "<0.001"
		}
		fmt.Fprintf(&b, "  %-32s %6d %6d %+8.1f%%  [%+6.1f%%, %+6.1f%%] %8s  %s\n",
			m.Name, m.Pairs, m.Trajectories, m.DeltaPct, m.CILoPct, m.CIHiPct, p, m.Verdict)
		fmt.Fprintf(&b, "  %-32s %6s\n", "",
			fmt.Sprintf("(ctrl≈%.1f → treat≈%.1f, min-baseline %.2g; %d zero-side, %d no-baseline%s, %d absent)",
				m.CtrlMean, m.TreatMean, m.MinBaseline, m.Zeroes, m.Dropped,
				noBaselinePosNote(m.NoBaselinePos), m.Absent))
		if m.LogRatioPairs >= minComparePairs {
			fmt.Fprintf(&b, "  %-32s %6s\n", "",
				fmt.Sprintf("(legacy log-ratio %+.1f%% on %d positive pairs)", m.LogRatioPct, m.LogRatioPairs))
		}
		if m.Guardrail != "" {
			fmt.Fprintf(&b, "  %-32s guardrail: %s\n", "", m.Guardrail)
		}
		if m.Acceptance != "" {
			fmt.Fprintf(&b, "  %-32s acceptance: %s\n", "", m.Acceptance)
		}
		if m.Verdict == "inconclusive" {
			if m.Required > 0 {
				fmt.Fprintf(&b, "  INCONCLUSIVE — CI spans the MDE boundary: %s needs n≈%d pairs to resolve (paired-variance estimate)\n", m.Name, m.Required)
			} else {
				fmt.Fprintf(&b, "  INCONCLUSIVE — CI spans the MDE boundary: %s; required n unknown (no recorded CV)\n", m.Name)
			}
		}
	}
	for _, a := range rep.AcceptanceAlarms {
		fmt.Fprintf(&b, "  ACCEPTANCE ALARM: %s\n", a)
	}
	if len(rep.ArmTotals) > 0 {
		fmt.Fprintf(&b, "  %-32s %6s %6s %12s %14s %14s\n", "arm", "runs", "passes", "wtd cost", "per attempt", "per pass")
		for _, a := range rep.ArmTotals {
			fmt.Fprintf(&b, "  %-32s %6d %6d %12.0f %14.0f %14.0f\n",
				a.Arm, a.Runs, a.Passes, a.WeightedCost, a.CostPerAttempt, a.CostPerPass)
		}
	}
	if len(rep.SkippedMetrics) > 0 {
		fmt.Fprintf(&b, "  skipped (<%d measurable pairs): %s\n",
			minComparePairs, strings.Join(rep.SkippedMetrics, ", "))
	}
	return b.String()
}

// noBaselinePosNote flags no-baseline pairs whose treatment side was
// positive — treatment invented activity where control had none, a
// regression direction the relative estimand cannot express.
func noBaselinePosNote(n int) string {
	if n == 0 {
		return ""
	}
	return fmt.Sprintf(" (%d with treat>0 — invisible regression)", n)
}
