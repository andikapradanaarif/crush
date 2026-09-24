package eval

import (
	"fmt"
	"log/slog"
	"math/rand/v2"
	"sort"
	"strings"
)

// Report is the experiment's gate verdict. Three multiple-comparison
// families: catastrophic (Bonferroni over the full stable band),
// diffuse (single corpus-level test), and the excluded-class
// differential (per-trajectory, its own correction).
type Report struct {
	// Catastrophic lists trajectories whose treatment arm collapsed
	// against their accumulated baseline at corrected significance.
	Catastrophic []string
	// CatastrophicEligible marks which stable-band trajectories had a
	// baseline deep enough that a 0/N could fire — the rest are
	// characterization, not protection.
	CatastrophicEligible map[string]bool
	// DiffuseP is the corpus-level permutation p-value over mid +
	// uncharacterized bands.
	DiffuseP float64
	// DiffusePairs counts the trajectory arm-pairs that fed the
	// diffuse permutation test — zero means the tier never ran and
	// carried no power regardless of the printed p.
	DiffusePairs int
	// ExcludedDifferential lists trajectories whose treatment arm
	// produced significantly more inconclusive/error outcomes than
	// control — mechanism flags aren't orthogonal to exclusion by
	// construction, so a differential is a first-class alarm.
	ExcludedDifferential []string
	// ExpectedExclusionSatisfied lists trajectories where the
	// declared arm produced at least expected_exclusion.min records
	// of the declared error class — the designed exclusion observed.
	// ExpectedExclusionMissed lists trajectories where it fell
	// short — the regime the experiment needs never engaged, so the
	// run was vacuous where it should have been decisive. A miss is
	// a first-class alarm.
	ExpectedExclusionSatisfied []string
	ExpectedExclusionMissed    []string
	// Starved trajectories exhausted their attempts cap on
	// inconclusive; Saturated exhausted on error. Alarm labels on the
	// summary, not persisted trajectory states.
	Starved   []string
	Saturated []string
	// Coincident lists trajectories where treatment AND the current
	// control arm both collapsed against baseline — suspect
	// trajectory rot or model drift, not the change under test.
	Coincident []string
	// Smoke lists stable-band trajectories showing the strict 0/N
	// collapse pattern — used by the smoke tier, which must work
	// where baselines are thin. In a normal experiment report it is
	// still a gate: an ineligible trajectory collapsing to 0/N is the
	// detector working, not a false alarm (p̂≈0.9 → P(0/3|null)≈1e-3).
	Smoke []string
	// Skipped lists trajectories the experiment didn't run —
	// requires pre-flight rejects ("id: tool:go os:linux") or
	// bands absent from runs_per_trajectory. Environment rot and
	// config gaps made explicit instead of error outcomes or
	// silence.
	Skipped []string
	// NoopFlags lists trajectories whose arms resolved to identical
	// flag projections — the flag under test did nothing and the
	// pairing is a guaranteed null. Fails closed.
	NoopFlags []string
	// ArmTokens carries per-arm prompt-side token aggregates over
	// conclusive runs — the benefit measurement (does the flag under
	// test shrink the prompt?), purely informational: it never
	// affects Fired or the verdict. The aa arm appears here too when
	// --aa ran — its delta vs control is the harness's own false
	// effect, visible next to the treatment delta.
	ArmTokens map[string]ArmTokenStats
	// ArmTokensExcluded carries the same aggregates over excluded
	// runs (inconclusive/error — timeouts are conclusive). Resampling to N
	// conclusive silently drops them from the conclusive table —
	// reporting their token mass keeps the selection visible.
	ArmTokensExcluded map[string]ArmTokenStats
	// Primary is the experiment's declared decision-metric read-out:
	// per-arm means over conclusive runs, the mechanism-fired
	// treatment stratum, the aa calibration sample, and the sample
	// size the power gate required. Informational like ArmTokens —
	// verdict logic stays binary until compare lands.
	Primary *PrimaryResult
	// Behavior carries the behavioral metric set per arm over
	// conclusive runs — the mechanism's second-order effects.
	// Informational: it reports, it never gates.
	Behavior map[string]ArmBehavior
	// Guardrails carries the guardrail set per arm — steps, pass
	// count, edit failures. Reported so a treatment that wins the
	// primary by burning steps or edits can't hide it — guardrails
	// are never wins.
	Guardrails map[string]ArmGuardrail
	// NoiseUpdated lists the noise.json CVs an --aa run refreshed.
	NoiseUpdated []string
}

// ArmBehavior is one arm's behavioral-metric sums over conclusive
// runs; the report divides by Runs for mean/run.
type ArmBehavior struct {
	Runs                int
	RereadsCrossTurn    float64
	RereadsSameTurn     float64
	DiscoveryCalls      float64
	RequestsToFirstEdit float64
}

// ArmGuardrail is one arm's guardrail tallies over conclusive runs.
type ArmGuardrail struct {
	Runs         int
	Passes       int
	StepsSum     int
	EditFailures int
}

// armBehavior aggregates the behavioral metric set — only runs with
// a call-metrics snapshot feed it (absent analysis is unmeasurable,
// not zero).
func armBehavior(records []RunRecord) map[string]ArmBehavior {
	var out map[string]ArmBehavior
	for i := range records {
		r := &records[i]
		if !r.Outcome.Conclusive() || r.CallMetrics == nil {
			continue
		}
		switch r.Arm {
		case ArmControl, ArmTreatment, ArmAA:
		default:
			continue
		}
		if out == nil {
			out = map[string]ArmBehavior{}
		}
		s := out[r.Arm]
		s.Runs++
		s.RereadsCrossTurn += float64(r.CallMetrics.RereadsCrossTurn)
		s.RereadsSameTurn += float64(r.CallMetrics.RereadsSameTurn)
		s.DiscoveryCalls += float64(r.CallMetrics.DiscoveryCallsBeforeWrite)
		s.RequestsToFirstEdit += float64(r.CallMetrics.RequestsToFirstEdit)
		out[r.Arm] = s
	}
	return out
}

// armGuardrails aggregates the guardrail set — steps, pass count,
// and edit failures over conclusive runs.
func armGuardrails(records []RunRecord) map[string]ArmGuardrail {
	var out map[string]ArmGuardrail
	for i := range records {
		r := &records[i]
		if !r.Outcome.Conclusive() {
			continue
		}
		switch r.Arm {
		case ArmControl, ArmTreatment, ArmAA:
		default:
			continue
		}
		if out == nil {
			out = map[string]ArmGuardrail{}
		}
		s := out[r.Arm]
		s.Runs++
		if r.Outcome == OutcomePass {
			s.Passes++
		}
		s.StepsSum += r.Steps
		if r.CallMetrics != nil {
			s.EditFailures += r.CallMetrics.EditFailures
		}
		out[r.Arm] = s
	}
	return out
}

// PrimaryResult is the decision metric's measured read-out.
type PrimaryResult struct {
	Metric         string
	Direction      string
	MDE            float64
	RequiredPerArm int // power-gate minimum
	// Conclusive strata — the powered comparison set.
	Control   metricSample
	Treatment metricSample
	// Mechanism-fired strata split treatment runs by whether the
	// treatment arm's own coverage block (the firing assertion) was
	// met — inconclusive runs count too, so a mechanism that fires
	// then fails still lands in Fired, not silently excluded.
	TreatmentFired   metricSample
	TreatmentUnfired metricSample
	// AA is the calibration arm's sample (control config, third arm).
	AA metricSample
}

// buildPrimaryResult computes the declared metric's strata over the
// invocation's records. requiredN is the power gate's per-arm floor
// (0 when no primary ran the gate — impossible here by construction).
func buildPrimaryResult(e *Experiment, records []RunRecord, requiredN int) *PrimaryResult {
	if e.Primary == nil {
		return nil
	}
	f, err := primaryMetricFunc(e, e.Primary.Metric)
	if err != nil {
		return nil
	}
	res := &PrimaryResult{
		Metric:         e.Primary.Metric,
		Direction:      e.Primary.Direction,
		MDE:            e.Primary.MDE,
		RequiredPerArm: requiredN,
	}
	treatCov := e.Arms[ArmTreatment].Coverage
	for i := range records {
		r := &records[i]
		if !primaryMeasurable(e.Primary.Metric, r) {
			continue
		}
		v := f(r)
		switch r.Arm {
		case ArmControl:
			if r.Outcome.Conclusive() {
				res.Control.add(v)
			}
		case ArmTreatment:
			if r.Outcome.Conclusive() {
				res.Treatment.add(v)
			}
			// The fired stratum only exists when the arm declares a
			// firing assertion — coverage-free treatment would vacuously
			// mark everything fired and duplicate the conclusive mean
			// under a misleading label.
			if len(treatCov) > 0 {
				if fired, err := ArmCoverageMet(treatCov, r); err == nil && fired {
					res.TreatmentFired.add(v)
				} else {
					res.TreatmentUnfired.add(v)
				}
			}
		case ArmAA:
			if r.Outcome.Conclusive() {
				res.AA.add(v)
			}
		}
	}
	return res
}

// primaryMeasurable fails closed on the same absent-telemetry
// classes the coverage grammar guards: a metric whose source never
// produced data is unmeasurable, not a zero — counting it would
// de-bias the strata the other way.
func primaryMeasurable(metric string, r *RunRecord) bool {
	switch {
	case strings.HasPrefix(metric, "call_metrics."):
		return r.CallMetrics != nil
	case strings.HasPrefix(metric, "request."):
		return r.Request != nil
	case strings.HasPrefix(metric, "generator_tokens."):
		return r.GeneratorTokens != nil
	}
	return true
}

// ArmTokenStats is one arm's prompt-side token totals over its
// conclusive runs. PromptTotal counts input + cache read + cache
// write — the full prompt-side billed mass, not just uncached input.
type ArmTokenStats struct {
	Runs          int
	PromptTotal   int64
	LastTurnTotal int64 // Σ of each run's last-turn prompt tokens
	LastTurnN     int
}

// PromptMean returns mean prompt-side tokens per conclusive run.
func (s ArmTokenStats) PromptMean() float64 {
	if s.Runs == 0 {
		return 0
	}
	return float64(s.PromptTotal) / float64(s.Runs)
}

// LastTurnMean returns the mean last-turn prompt size — the
// trajectory-final request, where the growth curve terminates.
func (s ArmTokenStats) LastTurnMean() float64 {
	if s.LastTurnN == 0 {
		return 0
	}
	return float64(s.LastTurnTotal) / float64(s.LastTurnN)
}

// Fired reports whether any alarm tripped — including the diffuse
// tier's corpus-level p against alpha.
func (r Report) Fired(alpha float64) bool {
	return r.DiffuseP < alpha ||
		len(r.Catastrophic) > 0 || len(r.ExcludedDifferential) > 0 ||
		len(r.ExpectedExclusionMissed) > 0 ||
		len(r.Starved) > 0 || len(r.Saturated) > 0 || len(r.Smoke) > 0 ||
		len(r.Coincident) > 0 ||
		len(r.Skipped) > 0 || // Corpus shrinkage is an alarm.
		len(r.NoopFlags) > 0
}

// Powered reports whether at least one evidence tier could have
// produced a verdict: a catastrophic-eligible trajectory or a diffuse
// arm-pair. When neither exists the evidence tiers are empty and a
// quiet run means "no evidence", not "no regression" — the verdict
// reads INCONCLUSIVE rather than PASS so a mechanism-never-fired or
// baseline-starved experiment cannot masquerade as a clean bill. The
// collapse-pattern alarms (smoke, noop, excluded-differential, ...)
// still fire on an unpowered report; INCONCLUSIVE only describes the
// quiet case.
func (r Report) Powered() bool {
	for _, ok := range r.CatastrophicEligible {
		if ok {
			return true
		}
	}
	return r.DiffusePairs > 0
}

// Summary renders the report for the CLI.
func (r Report) Summary(alpha float64) string {
	var b strings.Builder
	fire := func(name string, items []string) {
		if len(items) > 0 {
			fmt.Fprintf(&b, "  ALARM %s: %s\n", name, strings.Join(items, ", "))
		}
	}
	fire("catastrophic", r.Catastrophic)
	fire("excluded-differential", r.ExcludedDifferential)
	fire("expected-exclusion-missed", r.ExpectedExclusionMissed)
	fire("coverage-starved", r.Starved)
	fire("error-saturated", r.Saturated)
	fire("smoke", r.Smoke)
	fire("coincident-collapse", r.Coincident)
	fire("noop-flag", r.NoopFlags)
	if len(r.Skipped) > 0 {
		fmt.Fprintf(&b, "  SKIP: %s\n", strings.Join(r.Skipped, ", "))
	}
	if len(r.ExpectedExclusionSatisfied) > 0 {
		fmt.Fprintf(&b, "  expected exclusion met: %s\n", strings.Join(r.ExpectedExclusionSatisfied, ", "))
	}
	eligible := 0
	for _, ok := range r.CatastrophicEligible {
		if ok {
			eligible++
		}
	}
	fmt.Fprintf(&b, "  catastrophic coverage: %d/%d stable trajectories eligible\n", eligible, len(r.CatastrophicEligible))
	fmt.Fprintf(&b, "  diffuse p = %.4g (alpha %.3g)\n", r.DiffuseP, alpha)
	if len(r.ArmTokens) > 0 {
		b.WriteString("  tokens (prompt-side, conclusive runs — informational):\n")
		for _, arm := range []string{ArmControl, ArmTreatment, ArmAA} {
			t, ok := r.ArmTokens[arm]
			if !ok || t.Runs == 0 {
				continue
			}
			fmt.Fprintf(&b, "    %-9s n=%d  mean %s/run", arm, t.Runs, humanTokens(int64(t.PromptMean())))
			if t.LastTurnN > 0 {
				fmt.Fprintf(&b, "  last-turn mean %s", humanTokens(int64(t.LastTurnMean())))
			}
			b.WriteString("\n")
		}
		ctrl, cok := r.ArmTokens[ArmControl]
		treat, tok := r.ArmTokens[ArmTreatment]
		if cok && tok && ctrl.Runs > 0 && treat.Runs > 0 && ctrl.PromptMean() > 0 {
			fmt.Fprintf(&b, "    Δ treatment vs control: %+.1f%%\n",
				100*(treat.PromptMean()-ctrl.PromptMean())/ctrl.PromptMean())
		}
		// The calibration arm's delta is the false-effect magnitude —
		// a control-vs-control comparison should read ~0; when it
		// doesn't, drift or non-exchangeability is priced before the
		// treatment delta is read.
		if aa, ok := r.ArmTokens[ArmAA]; cok && ok && ctrl.Runs > 0 && aa.Runs > 0 && ctrl.PromptMean() > 0 {
			fmt.Fprintf(&b, "    a/a calibration: Δ aa vs control %+.1f%% (n=%d)\n",
				100*(aa.PromptMean()-ctrl.PromptMean())/ctrl.PromptMean(), aa.Runs)
		}
	}
	if len(r.ArmTokensExcluded) > 0 {
		var parts []string
		for _, arm := range []string{ArmControl, ArmTreatment, ArmAA} {
			if t, ok := r.ArmTokensExcluded[arm]; ok && t.Runs > 0 {
				parts = append(parts, fmt.Sprintf("%s n=%d mean %s/run", arm, t.Runs, humanTokens(int64(t.PromptMean()))))
			}
		}
		if len(parts) > 0 {
			fmt.Fprintf(&b, "  tokens, excluded runs (inconclusive/error — de-biased cost view): %s\n",
				strings.Join(parts, "; "))
		}
	}
	if p := r.Primary; p != nil {
		fmt.Fprintf(&b, "  primary %s (%s, mde %.0f%%):", p.Metric, p.Direction, 100*p.MDE)
		fmt.Fprintf(&b, " control %.3g (n=%d), treatment %.3g (n=%d)",
			p.Control.Mean(), p.Control.N, p.Treatment.Mean(), p.Treatment.N)
		if p.Control.Mean() > 0 {
			// Only the relative Δ is undefined on a zero control
			// mean — the absolute comparison still exists.
			fmt.Fprintf(&b, ", Δ %+.1f%%",
				100*(p.Treatment.Mean()-p.Control.Mean())/p.Control.Mean())
		}
		if p.RequiredPerArm > 0 {
			fmt.Fprintf(&b, "  [power: %d/arm required]", p.RequiredPerArm)
		}
		b.WriteString("\n")
		if p.TreatmentFired.N > 0 || p.TreatmentUnfired.N > 0 {
			fmt.Fprintf(&b, "    mechanism-fired stratum: fired n=%d mean %.3g, not-fired n=%d mean %.3g\n",
				p.TreatmentFired.N, p.TreatmentFired.Mean(), p.TreatmentUnfired.N, p.TreatmentUnfired.Mean())
		}
		if p.AA.N > 0 && p.Control.Mean() > 0 {
			fmt.Fprintf(&b, "    a/a: mean %.3g (n=%d), Δ vs control %+.1f%%\n",
				p.AA.Mean(), p.AA.N, 100*(p.AA.Mean()-p.Control.Mean())/p.Control.Mean())
		}
	}
	if len(r.Behavior) > 0 {
		b.WriteString("  behavior (conclusive, mean/run — informational):\n")
		for _, arm := range []string{ArmControl, ArmTreatment, ArmAA} {
			s, ok := r.Behavior[arm]
			if !ok || s.Runs == 0 {
				continue
			}
			n := float64(s.Runs)
			fmt.Fprintf(&b, "    %-9s rereads_xt %.2f  rereads_st %.2f  discovery %.2f  first_edit_req %.2f\n",
				arm, s.RereadsCrossTurn/n, s.RereadsSameTurn/n, s.DiscoveryCalls/n, s.RequestsToFirstEdit/n)
		}
	}
	if len(r.Guardrails) > 0 {
		b.WriteString("  guardrails (conclusive — never wins):\n")
		for _, arm := range []string{ArmControl, ArmTreatment, ArmAA} {
			g, ok := r.Guardrails[arm]
			if !ok || g.Runs == 0 {
				continue
			}
			fmt.Fprintf(&b, "    %-9s pass %d/%d  steps μ%.1f  edit_failures %d\n",
				arm, g.Passes, g.Runs, float64(g.StepsSum)/float64(g.Runs), g.EditFailures)
		}
	}
	if len(r.NoiseUpdated) > 0 {
		fmt.Fprintf(&b, "  noise.json updated: %s\n", strings.Join(r.NoiseUpdated, ", "))
	}
	switch {
	case r.Fired(alpha):
		b.WriteString("  verdict: FAIL\n")
	case !r.Powered():
		fmt.Fprintf(&b, "  verdict: INCONCLUSIVE (no powered tier: 0/%d catastrophic-eligible, %d diffuse pairs)\n",
			len(r.CatastrophicEligible), r.DiffusePairs)
	default:
		b.WriteString("  verdict: PASS\n")
	}
	return b.String()
}

// armTokenStats aggregates the informational benefit metric —
// prompt-side tokens per arm. conclusive selects the sample:
// conclusive runs for the powered comparison, excluded runs for the
// de-biased cost view. Never gates.
func armTokenStats(records []RunRecord, conclusive bool) map[string]ArmTokenStats {
	var out map[string]ArmTokenStats
	for _, r := range records {
		if r.Outcome.Conclusive() != conclusive {
			continue
		}
		switch r.Arm {
		case ArmControl, ArmTreatment, ArmAA:
		default:
			continue
		}
		if out == nil {
			out = map[string]ArmTokenStats{}
		}
		s := out[r.Arm]
		s.Runs++
		s.PromptTotal += r.Tokens.Input + r.Tokens.CacheRead + r.Tokens.CacheWrite
		if n := len(r.PromptTokensPerTurn); n > 0 {
			s.LastTurnTotal += r.PromptTokensPerTurn[n-1]
			s.LastTurnN++
		}
		out[r.Arm] = s
	}
	return out
}

// humanTokens renders a token count compactly for the report.
func humanTokens(n int64) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.2fM", float64(n)/1_000_000)
	case n >= 1_000:
		return fmt.Sprintf("%.1fK", float64(n)/1_000)
	default:
		return fmt.Sprintf("%d", n)
	}
}

// Evaluate runs the gate over one experiment's records. bands is the
// frozen experiment-start snapshot; baselineKey is the experiment's
// baseline condition (control arm's effective-config hash); alpha is
// the per-family significance level before correction.
func Evaluate(exp *Experiment, bands *Bands, baselineKey string, records []RunRecord, corpusIDs map[string]bool, alpha float64, replicates int, rng *rand.Rand) Report {
	rep := Report{CatastrophicEligible: map[string]bool{}, DiffuseP: 1}

	byTraj := map[string][]RunRecord{}
	for _, r := range records {
		byTraj[r.TrajectoryID] = append(byTraj[r.TrajectoryID], r)
	}
	rep.ArmTokens = armTokenStats(records, true)
	rep.ArmTokensExcluded = armTokenStats(records, false)
	rep.Behavior = armBehavior(records)
	rep.Guardrails = armGuardrails(records)

	stable := stableBandSize(bands, corpusIDs)
	if stable == 0 {
		stable = 1
	}
	corrAlpha := alpha / float64(stable) // Bonferroni over the FULL stable band.

	var diffusePairs []ArmPair
	var exclTraj []string
	var exclP []float64

	for id, recs := range byTraj {
		ctrl := conclusiveByArm(recs, ArmControl)
		treat := conclusiveByArm(recs, ArmTreatment)

		// No-op detection: if both arms' latest resolved projections
		// are identical, the flag under test did nothing — the null
		// is guaranteed and the verdict is meaningless. Arms whose
		// runs never reported telemetry (crashed/fake drivers) carry
		// no projection and are skipped — the alarm is telemetry-
		// dependent by design.
		var ctrlRes, treatRes map[string]any
		for _, r := range recs {
			// Records append chronologically — last non-empty wins.
			if len(r.ResolvedOptions) > 0 {
				if r.Arm == ArmControl {
					ctrlRes = r.ResolvedOptions
				}
				if r.Arm == ArmTreatment {
					treatRes = r.ResolvedOptions
				}
			}
		}
		if ctrlRes != nil && treatRes != nil && resolvedEqual(ctrlRes, treatRes) {
			rep.NoopFlags = append(rep.NoopFlags, id)
		}

		switch bands.Band(id) {
		case BandStable:
			// Baselines are keyed on the resolved model observed in
			// the runs, not the experiment's spelling — an alias or
			// normalization difference must not silently darken the
			// catastrophic tier. Warn on divergence.
			model := exp.Model
			for _, rec := range recs {
				if rec.Env.ModelResolved != "" {
					model = rec.Env.ModelResolved
					break
				}
			}
			if model != exp.Model {
				slog.Warn("Resolved model differs from experiment pin; baselines keyed on resolved",
					"trajectory", id, "resolved", model, "pin", exp.Model)
			}
			base := bands.Baseline(id, model, baselineKey)
			baseFails := base.N - base.Passes
			// Coincidence detector: a control arm that collapses
			// against the same baseline signals trajectory rot or
			// model drift — alarming regardless of the treatment
			// outcome, and it disqualifies the catastrophic verdict
			// (the collapse isn't attributable to the flag). Checked
			// before the treatment-emptiness bail: an all-excluded
			// treatment arm doesn't excuse a rotted control.
			ctrlFails := 0
			for _, ok := range ctrl {
				if !ok {
					ctrlFails++
				}
			}
			ctrlCollapsed := len(ctrl) > 0 && base.N > 0 &&
				FisherExactCollapse(ctrlFails, len(ctrl), baseFails, base.N) < corrAlpha
			if ctrlCollapsed {
				rep.Coincident = append(rep.Coincident, fmt.Sprintf("%s (control %d/%d vs baseline %d/%d)", id, len(ctrl)-ctrlFails, len(ctrl), base.Passes, base.N))
			}
			n := len(treat)
			if n == 0 {
				continue
			}
			// Eligibility is computed, not a fixed n: the trajectory
			// qualifies when a 0/N result would reach corrected
			// significance against its baseline.
			pMin := FisherExactCollapse(n, n, baseFails, base.N)
			eligible := pMin < corrAlpha
			rep.CatastrophicEligible[id] = eligible
			if eligible {
				fails := 0
				for _, ok := range treat {
					if !ok {
						fails++
					}
				}
				if p := FisherExactCollapse(fails, n, baseFails, base.N); p < corrAlpha && !ctrlCollapsed {
					rep.Catastrophic = append(rep.Catastrophic, fmt.Sprintf("%s (%d/%d vs baseline %d/%d, p=%.2g)", id, n-fails, n, base.Passes, base.N, p))
				}
			}
			// Smoke pattern: strict 0/N, no baseline required.
			if n > 0 && allFailed(treat) {
				rep.Smoke = append(rep.Smoke, id)
			}

		case BandMid, BandUncharacterized:
			if len(ctrl) > 0 && len(treat) > 0 {
				diffusePairs = append(diffusePairs, ArmPair{Control: ctrl, Treatment: treat})
			}
		}

		// Excluded-class differential, per trajectory: Fisher on
		// excluded counts between arms — unless an expected_exclusion
		// declaration owns the asymmetry.
		et, ec, tt, tc := excludedCounts(recs)
		if tt+tc > 0 {
			consume := false
			if d := exp.ExpectedExclusion; d != nil {
				if excludedClassCount(recs, d.Arm, d.ErrorClass) >= d.Min {
					rep.ExpectedExclusionSatisfied = append(rep.ExpectedExclusionSatisfied, id)
					// The declaration consumes the differential only
					// when the asymmetry ran the declared direction —
					// an equal-or-reverse split is still suspect and
					// keeps its Fisher.
					declared, other := ec, et
					if d.Arm == ArmTreatment {
						declared, other = et, ec
					}
					consume = declared > other
				} else {
					rep.ExpectedExclusionMissed = append(rep.ExpectedExclusionMissed, id)
				}
			}
			if !consume {
				exclTraj = append(exclTraj, id)
				exclP = append(exclP, FisherExactCollapse(et, tt, ec, tc))
			}
		}
	}

	rep.DiffusePairs = len(diffusePairs)
	if len(diffusePairs) > 0 {
		rep.DiffuseP = PermutationP(diffusePairs, replicates, rng)
	}

	// Third family: Bonferroni over the trajectories actually tested.
	if len(exclTraj) > 0 {
		corr := alpha / float64(len(exclTraj))
		for i, p := range exclP {
			if p < corr {
				rep.ExcludedDifferential = append(rep.ExcludedDifferential, exclTraj[i])
			}
		}
	}

	sort.Strings(rep.Catastrophic)
	sort.Strings(rep.ExcludedDifferential)
	sort.Strings(rep.ExpectedExclusionSatisfied)
	sort.Strings(rep.ExpectedExclusionMissed)
	sort.Strings(rep.Smoke)
	return rep
}

// stableBandSize counts the live corpus's stable band for the
// Bonferroni denominator — correcting across only eligible
// trajectories would be
// circular, since eligibility is defined by reaching that alpha.
// stableBandSize counts stable-band members of the live corpus —
// entries for trajectories removed from the corpus must not inflate
// the Bonferroni denominator.
func stableBandSize(bands *Bands, corpusIDs map[string]bool) int {
	n := 0
	for id, e := range bands.Entries {
		if e.Band == BandStable && corpusIDs[id] {
			n++
		}
	}
	return n
}

// conclusiveByArm returns pass/fail bits (timeout counts as fail — a
// real failure mode) for one arm's conclusive runs.
func conclusiveByArm(recs []RunRecord, arm string) []bool {
	var out []bool
	for _, r := range recs {
		if r.Arm != arm || !r.Outcome.Conclusive() {
			continue
		}
		out = append(out, r.Outcome == OutcomePass)
	}
	return out
}

// excludedCounts returns (treatExcluded, ctrlExcluded, treatAttempts,
// ctrlAttempts) over inconclusive+error vs all attempts.
func excludedCounts(recs []RunRecord) (et, ec, tt, tc int) {
	for _, r := range recs {
		excluded := r.Outcome == OutcomeInconclusive || r.Outcome == OutcomeError
		switch r.Arm {
		case ArmTreatment:
			tt++
			if excluded {
				et++
			}
		case ArmControl:
			tc++
			if excluded {
				ec++
			}
		}
	}
	return
}

// excludedClassCount returns the arm's error records carrying the
// given class — the satisfaction signal for expected_exclusion.
// Inconclusive records carry no error class and never count.
func excludedClassCount(recs []RunRecord, arm, class string) int {
	n := 0
	for _, r := range recs {
		if r.Arm == arm && r.Outcome == OutcomeError && r.ErrorClass == class {
			n++
		}
	}
	return n
}

func allFailed(bits []bool) bool {
	for _, b := range bits {
		if b {
			return false
		}
	}
	return len(bits) > 0
}
