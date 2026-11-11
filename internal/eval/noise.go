package eval

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
)

// NoiseFile is eval/noise.json: the per-metric coefficients of
// variation the power gate schedules against. Seeded from A/A
// measurements; every --aa run refreshes the metrics it observed.
// Harbor carries the externally executed base characterization —
// δ, c_min, σ_d the OOD gate and cost rules price against (#404).
type NoiseFile struct {
	CV     map[string]float64 `json:"cv"`
	Harbor *NoiseHarbor       `json:"harbor,omitempty"`
}

// LoadNoise reads evalDir/noise.json. A missing file is an empty
// table, not an error — the power gate fails closed on the absent
// CV, so an unseeded noise file can't silently disarm it.
func LoadNoise(evalDir string) (*NoiseFile, error) {
	data, err := os.ReadFile(filepath.Join(evalDir, "noise.json"))
	if err != nil {
		if os.IsNotExist(err) {
			return &NoiseFile{CV: map[string]float64{}}, nil
		}
		return nil, fmt.Errorf("read noise.json: %w", err)
	}
	var n NoiseFile
	if err := json.Unmarshal(data, &n); err != nil {
		return nil, fmt.Errorf("parse noise.json: %w", err)
	}
	if n.CV == nil {
		n.CV = map[string]float64{}
	}
	for metric, cv := range n.CV {
		if cv <= 0 {
			return nil, fmt.Errorf("noise.json: cv[%q] must be > 0, got %g", metric, cv)
		}
	}
	return &n, nil
}

// MergeHarbor folds a freshly computed harbor characterization
// into the recorded block field-wise — a later --pair-only write
// mustn't erase a recorded passes block. A non-empty sub-block
// refuses overwrite without force: measured once, never retuned
// is the discipline, so retuning is an explicit exception.
func (n *NoiseFile) MergeHarbor(h *NoiseHarbor, force bool) error {
	if n.Harbor == nil {
		n.Harbor = h
		return nil
	}
	if h.Passes != nil {
		if n.Harbor.Passes != nil && !force {
			return fmt.Errorf("noise: harbor.passes already recorded — --force to retune the base floor")
		}
		n.Harbor.Passes = h.Passes
	}
	if h.OOD != nil {
		if n.Harbor.OOD != nil && !force {
			return fmt.Errorf("noise: harbor.ood already recorded — --force to retune the base floor")
		}
		n.Harbor.OOD = h.OOD
	}
	if h.Model != "" {
		n.Harbor.Model = h.Model
	}
	// MeasuredAt is the measurement's timestamp — a metadata-only
	// merge (Model, or a no-op write) mustn't redate a sub-block it
	// didn't touch.
	if h.Passes != nil || h.OOD != nil {
		n.Harbor.MeasuredAt = h.MeasuredAt
	}
	return nil
}

// Save writes the noise file atomically-ish (rename after write).
func (n *NoiseFile) Save(evalDir string) error {
	data, err := json.MarshalIndent(n, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(evalDir, "noise.json")
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o644); err != nil {
		return fmt.Errorf("write noise.json: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("rename noise.json: %w", err)
	}
	return nil
}

// Z-values for the power calculation: one-sided α = 0.05 (the
// primary declares a direction) and power 0.80.
const (
	zAlphaOneSided = 1.6449
	zAlphaTwoSided = 1.96
	zBeta80        = 0.8416
)

// RequiredSamples is the per-arm sample count needed to detect a
// relative effect mde against per-arm coefficient of variation cv,
// one-sided α = 0.05, power 0.80:
//
//	n = ⌈2·(z_α + z_β)²·(cv/mde)²⌉ ≈ ⌈12.38·(cv/mde)²⌉
//
// The factor 2 prices both arms' sampling error; cv/mde is the
// noise-to-signal ratio. At cv 11% a 5% effect needs ~60/arm — the
// seed A/A measurements put steps there, which is exactly why the
// gate exists. Results clamp at MaxInt32 — an absurd input must
// still refuse, not wrap negative and open the gate.
func RequiredSamples(cv, mde float64) int {
	if cv <= 0 || mde <= 0 {
		return 0
	}
	r := cv / mde
	n := 2 * (zAlphaOneSided + zBeta80) * (zAlphaOneSided + zBeta80) * r * r
	if n >= math.MaxInt32 {
		return math.MaxInt32
	}
	return int(math.Ceil(n))
}

// OOD endpoint sizings (#389) — the task-count requirements for the
// held-out claim. Three endpoints, priced independently:
//
//	(a) efficiency  N_a = (z₁₋α/2 + z₁₋β)² · CV_d² / Δ_c²
//	    CV_d = SD of the per-task paired log-ratio of tokens
//	    (noise.harbor.ood.tokens_log_ratio_sd), Δ_c = the primary's
//	    declared min_improvement.
//	(b) pass non-inferiority
//	    N_b = (z₁₋α + z₁₋β)² · σ_d² / (Δ_true − m)²
//	    σ_d = SD of per-task paired pass diffs (noise.harbor.ood.
//	    sigma_d), m = the guardrail's min_delta (< 0), Δ_true = 0
//	    (conservative — the claim is "no worse than m").
//	(c) pass superiority — reported, not required:
//	    N_c = (z₁₋α/2 + z₁₋β)² · σ_d² / Δ², Δ = declared improvement.
//
// Non-inferiority is the one-sided test (z₁₋α); superiority and
// efficiency price both tails.
type OODSizing struct {
	// NEfficiency is N_a — tasks needed for the continuous
	// primary's declared improvement at CV_d.
	NEfficiency int `json:"n_efficiency,omitempty"`
	// NNonInferiority is N_b — tasks needed to bound pass
	// regression inside the declared margin.
	NNonInferiority int `json:"n_noninferiority,omitempty"`
	// NSuperiority is N_c — tasks for the declared pass_rate
	// improvement; reported alongside the binding endpoints, binding
	// only when it's the sole declared claim.
	NSuperiority int `json:"n_superiority,omitempty"`
	// Required is N_OOD = max of the required endpoints — and the
	// superiority count when that's the only endpoint declared, so a
	// superiority-only read can't sail through unpowered.
	Required int `json:"required"`
}

// RequiredNEfficiency prices the paired continuous endpoint:
// (z₁₋α/2 + z₁₋β)² · (CV_d/Δ_c)².
func RequiredNEfficiency(cvD, deltaC float64) int {
	if cvD <= 0 || deltaC <= 0 {
		return 0
	}
	r := cvD / deltaC
	n := (zAlphaTwoSided + zBeta80) * (zAlphaTwoSided + zBeta80) * r * r
	if n >= math.MaxInt32 {
		return math.MaxInt32
	}
	return int(math.Ceil(n))
}

// RequiredNNonInferiority prices the paired binary non-inferiority
// endpoint: (z₁₋α + z₁₋β)² · σ_d²/(Δ_true−m)². Δ_true is the
// planning true-difference (0 conservative); m < 0 is the margin.
func RequiredNNonInferiority(sigmaD, margin, deltaTrue float64) int {
	gap := deltaTrue - margin
	if sigmaD <= 0 || gap <= 0 {
		return 0
	}
	n := (zAlphaOneSided + zBeta80) * (zAlphaOneSided + zBeta80) * sigmaD * sigmaD / (gap * gap)
	if n >= math.MaxInt32 {
		return math.MaxInt32
	}
	return int(math.Ceil(n))
}

// RequiredNSuperiority prices the paired binary superiority
// endpoint: (z₁₋α/2 + z₁₋β)² · σ_d²/Δ².
func RequiredNSuperiority(sigmaD, delta float64) int {
	if sigmaD <= 0 || delta <= 0 {
		return 0
	}
	n := (zAlphaTwoSided + zBeta80) * (zAlphaTwoSided + zBeta80) * sigmaD * sigmaD / (delta * delta)
	if n >= math.MaxInt32 {
		return math.MaxInt32
	}
	return int(math.Ceil(n))
}

// HarborOODSizing computes the task-count requirements for an OOD
// decision rule against the measured harbor noise floor. nil rule
// or non-OOD claims return nil — nothing to size. Missing measured
// inputs error: an OOD claim that can't be sized is underpowered
// by construction (inconclusive, never null). A recorded SD of 0
// errors identically — a genuinely zero-variance pair read would
// need infinitely many tasks either way, so fail-closed is the
// only honest answer.
func HarborOODSizing(exp *Experiment, noise *NoiseFile) (*OODSizing, error) {
	rule := exp.DecisionRule
	if rule == nil || !rule.OOD {
		return nil, nil
	}
	if noise == nil || noise.Harbor == nil || noise.Harbor.OOD == nil {
		return nil, fmt.Errorf("OOD sizing needs measured harbor noise — run #404 characterization first (noise.json has no harbor.ood block)")
	}
	ood := noise.Harbor.OOD
	out := &OODSizing{}
	sized := false
	if p := rule.Primary; p != nil && p.Metric != "pass_rate" {
		// The noise input is a SD of per-task log token ratios, so
		// any continuous primary prices against the token floor as a
		// proxy — wall_s and cost share its shape closely enough for
		// a planning number, and the token read is the only paired
		// continuous variance the base characterization measures.
		// Δ_c is used raw rather than |log(1−Δ)|: for small declared
		// effects the approximation is tight, and the residual bias
		// is conservative for improvements (overstates N slightly).
		if ood.TokensLogRatioSD <= 0 {
			return nil, fmt.Errorf("OOD sizing: primary %q needs tokens_log_ratio_sd — the #404 pair read didn't measure it", p.Metric)
		}
		if p.MinImprovement <= 0 {
			return nil, fmt.Errorf("OOD sizing: primary %q needs min_improvement > 0 — the declared efficiency effect is the Δ_c being powered", p.Metric)
		}
		out.NEfficiency = RequiredNEfficiency(ood.TokensLogRatioSD, p.MinImprovement)
		sized = true
	}
	if p := rule.Primary; p != nil && p.Metric == "pass_rate" && p.MinImprovement > 0 {
		if ood.SigmaD <= 0 {
			return nil, fmt.Errorf("OOD sizing: pass_rate primary needs sigma_d — the #404 pair read didn't measure it")
		}
		out.NSuperiority = RequiredNSuperiority(ood.SigmaD, p.MinImprovement)
		sized = true
	}
	if g := rule.Guardrail; g != nil && g.Metric == "pass_rate" {
		if ood.SigmaD <= 0 {
			return nil, fmt.Errorf("OOD sizing: pass_rate guardrail needs sigma_d — the #404 pair read didn't measure it")
		}
		if g.MinDelta < 0 {
			out.NNonInferiority = RequiredNNonInferiority(ood.SigmaD, g.MinDelta, 0)
			sized = true
		} else if g.MinDelta > 0 {
			out.NSuperiority = max(out.NSuperiority, RequiredNSuperiority(ood.SigmaD, g.MinDelta))
			sized = true
		}
	}
	if !sized {
		return nil, fmt.Errorf("OOD sizing: the rule declares no sizable endpoint — a continuous primary (min_improvement) or a pass_rate primary/guardrail")
	}
	out.Required = max(out.NEfficiency, out.NNonInferiority)
	if out.Required == 0 {
		// A superiority-only claim has no binding endpoint to hide
		// behind — its own count is the floor, or the power gate is
		// vacuous for exactly the claim being powered.
		out.Required = out.NSuperiority
	}
	return out, nil
}

// checkPower is the scheduling refusal: an experiment with a
// declared primary only runs when its total per-arm sample can
// resolve the MDE at the recorded noise level. Checked after corpus
// selection (available n needs band resolution), before any run —
// an underpowered experiment must not burn a paid invocation.
func checkPower(e *Experiment, trajs []*Trajectory, bands *Bands, noise *NoiseFile) (int, int, error) {
	if e.Primary == nil {
		return 0, 0, nil
	}
	cv, ok := noise.CV[e.Primary.Metric]
	if !ok {
		return 0, 0, fmt.Errorf(
			"power gate: no recorded CV for primary metric %q — seed it in eval/noise.json "+
				"(an --aa run refreshes recorded metrics, but can't bootstrap an absent primary CV on this same experiment)",
			e.Primary.Metric)
	}
	required := RequiredSamples(cv, e.Primary.MDE)
	available := 0
	for _, t := range trajs {
		available += e.RunsPerTrajectory[bands.Band(t.ID)]
	}
	if available < required {
		return required, available, fmt.Errorf(
			"power gate: %d conclusive/arm planned across %d trajectories, need %d/arm "+
				"(primary %s, cv %.3g, mde %.3g, α=0.05 one-sided, power 0.80)",
			available, len(trajs), required, e.Primary.Metric, cv, e.Primary.MDE)
	}
	return required, available, nil
}

// updateNoiseFromAA refreshes recorded CVs from an --aa run's
// control + aa records — both are the control condition, so the
// pool measures pure harness noise. Only metrics with a computable
// CV and at least 4 pooled samples update; the file records a
// running estimate (80% old / 20% new) so one thin run can't
// rewrite the noise floor.
func (n *NoiseFile) updateNoiseFromAA(records []RunRecord, metrics map[string]func(*RunRecord) float64) []string {
	var pool []RunRecord
	for _, r := range records {
		if (r.Arm == ArmControl || r.Arm == ArmAA) && r.Outcome.Conclusive() {
			pool = append(pool, r)
		}
	}
	if len(pool) < 4 {
		return nil
	}
	var updated []string
	for metric, f := range metrics {
		var s metricSample
		for i := range pool {
			if !primaryMeasurable(metric, &pool[i]) {
				continue
			}
			s.add(f(&pool[i]))
		}
		// The pooled-size gate isn't enough — a metric measurable on
		// a handful of runs mustn't blend a 2-sample CV into the
		// noise floor at 20% weight.
		if s.N < 4 {
			continue
		}
		cv := s.CV()
		if cv == 0 {
			continue
		}
		if old, ok := n.CV[metric]; ok {
			cv = 0.8*old + 0.2*cv
		}
		n.CV[metric] = cv
		updated = append(updated, fmt.Sprintf("%s=%.3g", metric, n.CV[metric]))
	}
	slices.Sort(updated)
	return updated
}

// aaMetrics are the continuous metrics an A/A run feeds back into
// noise.json — the tracked set, not the whole registry, so a noisy
// one-off can't pollute the table.
var aaMetrics = []string{
	"steps",
	"tokens.input",
	"tokens.output",
	"tokens.cache_read",
	"request.prompt_tokens_peak",
	"request.history_bytes",
	"weighted_cost",
}

// noiseUpdateMetrics resolves the A/A refresh set, skipping metrics
// the experiment can't evaluate (e.g. weighted_cost without
// cost_weights).
func noiseUpdateMetrics(e *Experiment) map[string]func(*RunRecord) float64 {
	out := map[string]func(*RunRecord) float64{}
	for _, m := range aaMetrics {
		if f, err := primaryMetricFunc(e, m); err == nil {
			out[m] = f
		}
	}
	if e.Primary != nil {
		if f, err := primaryMetricFunc(e, e.Primary.Metric); err == nil {
			out[e.Primary.Metric] = f
		}
	}
	return out
}
