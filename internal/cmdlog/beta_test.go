package cmdlog

import (
	"math"
	"testing"

	"github.com/stretchr/testify/require"
)

// The quantile table uses distributions whose inverse CDF is
// closed-form — the bisection result has to land on the analytic
// answer, not just on a plausible one (#296).
func TestBetaQuantile_KnownValues(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		a, b float64
		q    float64
		want float64
	}{
		// Uniform: the inverse CDF is the identity.
		{"uniform q10", 1, 1, 0.1, 0.1},
		{"uniform q50", 1, 1, 0.5, 0.5},
		{"uniform q90", 1, 1, 0.9, 0.9},
		// Beta(2,1): CDF = x^2, so q(p) = sqrt(p).
		{"beta21 q10", 2, 1, 0.1, math.Sqrt(0.1)},
		{"beta21 q50", 2, 1, 0.5, math.Sqrt(0.5)},
		// Beta(1,2): CDF = 1-(1-x)^2, so q(p) = 1-sqrt(1-p).
		{"beta12 q10", 1, 2, 0.1, 1 - math.Sqrt(0.9)},
		// Beta(21,1): CDF = x^21, so q(p) = p^(1/21) — the shape a
		// 20-acceptance posterior takes.
		{"beta21_1 q10", 21, 1, 0.1, math.Pow(0.1, 1.0/21)},
		// Beta(1,21): q(p) = 1-(1-p)^(1/21).
		{"beta1_21 q10", 1, 21, 0.1, 1 - math.Pow(0.9, 1.0/21)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			require.InDelta(t, c.want, BetaQuantile(c.a, c.b, c.q), 1e-6)
		})
	}
}

func TestBetaQuantile_Symmetry(t *testing.T) {
	t.Parallel()
	// Beta(a,b) mirrored is Beta(b,a): q_a,b(p) = 1 - q_b,a(1-p).
	for _, ab := range [][2]float64{{2, 5}, {1, 1}, {13, 7}, {0.5, 0.5}} {
		got := BetaQuantile(ab[0], ab[1], 0.1)
		want := 1 - BetaQuantile(ab[1], ab[0], 0.9)
		require.InDelta(t, want, got, 1e-6)
	}
}

func TestBetaQuantile_Tails(t *testing.T) {
	t.Parallel()
	require.Zero(t, BetaQuantile(2, 3, 0))
	require.Equal(t, 1.0, BetaQuantile(2, 3, 1))
}

func TestBetaCDF_KnownValues(t *testing.T) {
	t.Parallel()
	require.InDelta(t, 0.3, betaCDF(1, 1, 0.3), 1e-9)
	require.InDelta(t, 0.25, betaCDF(2, 1, 0.5), 1e-9)
	require.InDelta(t, 0.75, betaCDF(1, 2, 0.5), 1e-9)
	require.Zero(t, betaCDF(2, 3, -0.1))
	require.Equal(t, 1.0, betaCDF(2, 3, 1.1))
}
