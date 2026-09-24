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
type NoiseFile struct {
	CV map[string]float64 `json:"cv"`
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
