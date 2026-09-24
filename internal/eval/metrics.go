package eval

import (
	"fmt"
	"maps"
	"math"
	"slices"
	"strings"
)

// primaryMetricFunc resolves a primary-metric name to its per-run
// accessor. The registry is closed — the union of every coverage
// field plus the derived metrics below — so a primary can never be a
// ratio. That is deliberate: any X/steps metric moves when a
// regression merely inflates step count, and "per-request prompt" is
// prompt_total/steps — a phantom that hides behind step inflation
// instead of measuring the prompt.
func primaryMetricFunc(e *Experiment, name string) (func(*RunRecord) float64, error) {
	switch name {
	case "weighted_cost":
		if e.CostWeights == nil {
			return nil, fmt.Errorf("primary metric %q requires cost_weights (h, o) pinned for the model", name)
		}
		w := *e.CostWeights
		return func(r *RunRecord) float64 {
			return float64(r.Tokens.Input) +
				w.CacheRead*float64(r.Tokens.CacheRead) +
				w.Output*float64(r.Tokens.Output)
		}, nil
	case "generator_tokens.input":
		return func(r *RunRecord) float64 {
			if r.GeneratorTokens == nil {
				return 0
			}
			return float64(r.GeneratorTokens.Input)
		}, nil
	case "generator_tokens.output":
		return func(r *RunRecord) float64 {
			if r.GeneratorTokens == nil {
				return 0
			}
			return float64(r.GeneratorTokens.Output)
		}, nil
	}
	if f, ok := coverageFields[name]; ok {
		return f, nil
	}
	if f, ok := armOnlyCoverageFields[name]; ok {
		return f, nil
	}
	return nil, fmt.Errorf("unknown primary metric %q (available: %s)",
		name, strings.Join(primaryMetricNames(), ", "))
}

// primaryMetricNames lists the closed registry for errors and docs.
func primaryMetricNames() []string {
	names := []string{"weighted_cost", "generator_tokens.input", "generator_tokens.output"}
	names = append(names, slices.Sorted(maps.Keys(coverageFields))...)
	names = append(names, slices.Sorted(maps.Keys(armOnlyCoverageFields))...)
	return names
}

// metricSample summarizes one stratum of a continuous metric.
type metricSample struct {
	N    int
	Sum  float64
	Sum2 float64
}

func (s *metricSample) add(v float64) {
	s.N++
	s.Sum += v
	s.Sum2 += v * v
}

// Mean is the stratum's sample mean; 0 when empty.
func (s metricSample) Mean() float64 {
	if s.N == 0 {
		return 0
	}
	return s.Sum / float64(s.N)
}

// CV is the sample coefficient of variation — the noise.json
// quantity. Returns 0 when the sample can't support one.
func (s metricSample) CV() float64 {
	if s.N < 2 {
		return 0
	}
	mean := s.Mean()
	if mean == 0 {
		return 0
	}
	v := (s.Sum2 - float64(s.N)*mean*mean) / float64(s.N-1)
	if v <= 0 {
		return 0
	}
	return math.Sqrt(v) / mean
}
