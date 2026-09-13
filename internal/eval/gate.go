package eval

import (
	"fmt"
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
	// ExcludedDifferential lists trajectories whose treatment arm
	// produced significantly more inconclusive/error outcomes than
	// control — mechanism flags aren't orthogonal to exclusion by
	// construction, so a differential is a first-class alarm.
	ExcludedDifferential []string
	// Starved trajectories exhausted their attempts cap on
	// inconclusive; Saturated exhausted on error. Alarm labels on the
	// summary, not persisted trajectory states.
	Starved   []string
	Saturated []string
	// Smoke lists stable-band trajectories showing the strict 0/N
	// collapse pattern — used by the smoke tier, which must work
	// where baselines are thin.
	Smoke []string
}

// Fired reports whether any alarm tripped.
func (r Report) Fired() bool {
	return len(r.Catastrophic) > 0 || len(r.ExcludedDifferential) > 0 ||
		len(r.Starved) > 0 || len(r.Saturated) > 0 || len(r.Smoke) > 0
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
	fire("coverage-starved", r.Starved)
	fire("error-saturated", r.Saturated)
	fire("smoke", r.Smoke)
	fmt.Fprintf(&b, "  diffuse p = %.4g (alpha %.3g)\n", r.DiffuseP, alpha)
	if !r.Fired() && r.DiffuseP >= alpha {
		b.WriteString("  verdict: PASS\n")
	} else {
		b.WriteString("  verdict: FAIL\n")
	}
	return b.String()
}

// Evaluate runs the gate over one experiment's records. bands is the
// frozen experiment-start snapshot; baselineKey is the experiment's
// baseline condition (control arm's effective-config hash); alpha is
// the per-family significance level before correction.
func Evaluate(exp *Experiment, bands *Bands, baselineKey string, records []RunRecord, alpha float64, replicates int, rng *rand.Rand) Report {
	rep := Report{CatastrophicEligible: map[string]bool{}, DiffuseP: 1}

	byTraj := map[string][]RunRecord{}
	for _, r := range records {
		byTraj[r.TrajectoryID] = append(byTraj[r.TrajectoryID], r)
	}

	stable := stableBandSize(bands)
	if stable == 0 {
		stable = 1
	}
	corrAlpha := alpha / float64(stable) // Bonferroni over the FULL stable band.

	var diffusePairs []ArmPair
	var exclTraj, exclTests []string
	var exclP []float64

	for id, recs := range byTraj {
		ctrl := conclusiveByArm(recs, "control")
		treat := conclusiveByArm(recs, "treatment")

		switch bands.Band(id) {
		case BandStable:
			n := len(treat)
			if n == 0 {
				continue
			}
			base := bands.Baseline(id, exp.Model, baselineKey)
			baseFails := base.N - base.Passes
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
				if p := FisherExactCollapse(fails, n, baseFails, base.N); p < corrAlpha {
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
		// excluded counts between arms.
		et, ec, tt, tc := excludedCounts(recs)
		if tt+tc > 0 {
			exclTraj = append(exclTraj, id)
			exclTests = append(exclTests, id)
			exclP = append(exclP, FisherExactCollapse(et, tt, ec, tc))
		}
	}

	if len(diffusePairs) > 0 {
		rep.DiffuseP = PermutationP(diffusePairs, replicates, rng)
	}

	// Third family: Bonferroni over the trajectories actually tested.
	if len(exclTests) > 0 {
		corr := alpha / float64(len(exclTests))
		for i, p := range exclP {
			if p < corr {
				rep.ExcludedDifferential = append(rep.ExcludedDifferential, exclTraj[i])
			}
		}
	}

	sort.Strings(rep.Catastrophic)
	sort.Strings(rep.ExcludedDifferential)
	sort.Strings(rep.Smoke)
	return rep
}

// stableBandSize counts the full stable band for the Bonferroni
// denominator — correcting across only eligible trajectories would be
// circular, since eligibility is defined by reaching that alpha.
func stableBandSize(bands *Bands) int {
	n := 0
	for _, e := range bands.Entries {
		if e.Band == BandStable {
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
		case "treatment":
			tt++
			if excluded {
				et++
			}
		case "control":
			tc++
			if excluded {
				ec++
			}
		}
	}
	return
}

func allFailed(bits []bool) bool {
	for _, b := range bits {
		if b {
			return false
		}
	}
	return len(bits) > 0
}
