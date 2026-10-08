package eval

import (
	"maps"
	"math"
	"slices"
)

// ProcessInteraction is the 2×2 read the process-model fidelity
// experiment exists to produce (#117):
//
//	              notebook off   notebook on
//	restart          A1             C1        Δ_restart = C1−A1
//	persistent       A2             C2        Δ_persist  = C2−A2
//
//	I = Δ_restart − Δ_persist — whether the fresh-process-per-turn
//	model changes the measured notebook effect. When |I| exceeds the
//	materiality bound (10% of baseline task cost — the mean control
//	metric under restart, the eval's historical regime), the honest
//	output is per-regime estimates, never a corrected point estimate:
//	the interaction is evidence about a measurement artifact, not a
//	multiplier to apply.
type ProcessInteraction struct {
	Metric string `json:"metric"`
	// Cells carries the per-trajectory regime×arm means — the
	// per-regime estimates the materiality rule publishes.
	Cells []InteractionTrajectory `json:"trajectories"`
	// Incomplete lists trajectories missing a cell — a trajectory
	// with no conclusive run in one regime contributes no
	// interaction estimate, and dropping it silently would hide
	// regime-specific failure (e.g. persistent runs erroring where
	// restart ones passed).
	Incomplete []string `json:"incomplete,omitempty"`
	// MeanInteraction averages I over complete trajectories.
	MeanInteraction float64 `json:"mean_interaction"`
	// MaterialThreshold is 10% of the mean control metric under
	// restart — the issue's materiality bound on baseline task cost.
	// Zero when no restart-control baseline exists, in which case
	// Materiality is "unevaluable".
	MaterialThreshold float64 `json:"material_threshold"`
	// Materiality is "material" (|mean I| ≥ threshold), "immaterial"
	// (below — restart estimates read as persistent-equivalent for
	// this metric), or "unevaluable" (no complete trajectory or no
	// baseline to price the bound against).
	Materiality string `json:"materiality"`
}

// InteractionTrajectory is one trajectory's four cells plus the two
// deltas and their difference.
type InteractionTrajectory struct {
	Trajectory string `json:"trajectory"`
	// RestartControl/RestartTreatment/PersistControl/PersistTreatment
	// are the mean metric per regime×arm cell over conclusive runs.
	RestartControl   float64 `json:"restart_control"`
	RestartTreatment float64 `json:"restart_treatment"`
	PersistControl   float64 `json:"persist_control"`
	PersistTreatment float64 `json:"persist_treatment"`
	RestartDelta     float64 `json:"restart_delta"`
	PersistDelta     float64 `json:"persist_delta"`
	Interaction      float64 `json:"interaction"`
	// Runs counts the conclusive records per cell, keyed
	// "regime:arm" — sparse cells make the deltas fragile, so the
	// sample sizes travel with the means.
	Runs map[string]int `json:"runs"`
}

// ProcessModelInteraction computes the restart-vs-persistent
// interaction over a mixed record pool — records self-identify via
// process_model (empty reads as restart, the historical regime). Only
// conclusive outcomes count: error/timeout/inconclusive runs are not
// task-cost samples. exp supplies the metric namespace (cost weights
// for weighted_cost, flag projection context for request metrics).
func ProcessModelInteraction(records []RunRecord, exp *Experiment, metric string) (ProcessInteraction, error) {
	metricFn, err := primaryMetricFunc(exp, metric)
	if err != nil {
		return ProcessInteraction{}, err
	}
	rep := ProcessInteraction{Metric: metric}

	// traj → regime → arm → samples. process_model "" is restart:
	// pre-#117 records predate the field but ran under restart.
	type key struct{ regime, arm string }
	cells := map[string]map[key][]float64{}
	for _, rec := range records {
		if rec.Replay != nil {
			continue // Fork records are boundary samples, not task runs.
		}
		if !rec.Outcome.Conclusive() {
			continue
		}
		regime := rec.ProcessModel
		if regime == "" {
			regime = ProcessModelRestart
		}
		m := cells[rec.TrajectoryID]
		if m == nil {
			m = map[key][]float64{}
			cells[rec.TrajectoryID] = m
		}
		k := key{regime, rec.Arm}
		m[k] = append(m[k], metricFn(&rec))
	}

	mean := func(xs []float64) (float64, bool) {
		if len(xs) == 0 {
			return 0, false
		}
		var s float64
		for _, x := range xs {
			s += x
		}
		return s / float64(len(xs)), true
	}

	var baselineSum float64
	baselineN := 0
	for _, trajID := range slices.Sorted(maps.Keys(cells)) {
		m := cells[trajID]
		a1, okA1 := mean(m[key{ProcessModelRestart, ArmControl}])
		c1, okC1 := mean(m[key{ProcessModelRestart, ArmTreatment}])
		a2, okA2 := mean(m[key{ProcessModelPersistent, ArmControl}])
		c2, okC2 := mean(m[key{ProcessModelPersistent, ArmTreatment}])
		if !(okA1 && okC1 && okA2 && okC2) {
			rep.Incomplete = append(rep.Incomplete, trajID)
			continue
		}
		dR := c1 - a1
		dP := c2 - a2
		rep.Cells = append(rep.Cells, InteractionTrajectory{
			Trajectory:       trajID,
			RestartControl:   a1,
			RestartTreatment: c1,
			PersistControl:   a2,
			PersistTreatment: c2,
			RestartDelta:     dR,
			PersistDelta:     dP,
			Interaction:      dR - dP,
			Runs: map[string]int{
				"restart:control":      len(m[key{ProcessModelRestart, ArmControl}]),
				"restart:treatment":    len(m[key{ProcessModelRestart, ArmTreatment}]),
				"persistent:control":   len(m[key{ProcessModelPersistent, ArmControl}]),
				"persistent:treatment": len(m[key{ProcessModelPersistent, ArmTreatment}]),
			},
		})
		rep.MeanInteraction += dR - dP
		baselineSum += a1
		baselineN++
	}
	switch {
	case len(rep.Cells) == 0:
		rep.Materiality = "unevaluable"
	default:
		rep.MeanInteraction /= float64(len(rep.Cells))
		if baselineN == 0 || baselineSum <= 0 {
			// No restart-control baseline prices the bound — the
			// interaction is reported but materiality can't be.
			rep.Materiality = "unevaluable"
			break
		}
		rep.MaterialThreshold = 0.10 * (baselineSum / float64(baselineN))
		if math.Abs(rep.MeanInteraction) >= rep.MaterialThreshold {
			rep.Materiality = "material"
		} else {
			rep.Materiality = "immaterial"
		}
	}
	return rep, nil
}
