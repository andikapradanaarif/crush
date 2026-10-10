package cmdlog

import "math"

// Beta-distribution helpers for the empirical-Bayes rate gate
// (#296): the gate decides on a posterior lower-tail quantile, and
// gonum is not a dependency, so the regularized incomplete beta
// function and its bisection inversion live here. The formulas are
// the standard continued-fraction evaluation (Numerical Recipes
// betacf) — accurate to ~1e-7 for the mass ranges the rate layer
// sees (posteriors in the low hundreds at most).

const betaMaxIter = 200

// betacf evaluates the continued fraction for the incomplete beta
// function using Lentz's method.
func betacf(a, b, x float64) float64 {
	const fpmin = 1e-30
	qab := a + b
	qap := a + 1
	qam := a - 1
	c := 1.0
	d := 1.0 - qab*x/qap
	if math.Abs(d) < fpmin {
		d = fpmin
	}
	d = 1 / d
	h := d
	for m := 1; m <= betaMaxIter; m++ {
		m2 := 2 * m
		aa := float64(m) * (b - float64(m)) * x / ((qam + float64(m2)) * (a + float64(m2)))
		d = 1 + aa*d
		if math.Abs(d) < fpmin {
			d = fpmin
		}
		c = 1 + aa/c
		if math.Abs(c) < fpmin {
			c = fpmin
		}
		d = 1 / d
		h *= d * c
		aa = -(a + float64(m)) * (qab + float64(m)) * x / ((a + float64(m2)) * (qap + float64(m2)))
		d = 1 + aa*d
		if math.Abs(d) < fpmin {
			d = fpmin
		}
		c = 1 + aa/c
		if math.Abs(c) < fpmin {
			c = fpmin
		}
		d = 1 / d
		del := d * c
		h *= del
		if math.Abs(del-1) < 3e-14 {
			break
		}
	}
	return h
}

// betaCDF is the regularized incomplete beta function I_x(a, b):
// the probability a Beta(a, b) variate lands at or below x.
func betaCDF(a, b, x float64) float64 {
	if x <= 0 {
		return 0
	}
	if x >= 1 {
		return 1
	}
	lgAB, _ := math.Lgamma(a + b)
	lgA, _ := math.Lgamma(a)
	lgB, _ := math.Lgamma(b)
	// The leading factor is x^a (1-x)^b / B(a,b): the log form adds
	// -ln B(a,b) = lgAB - lgA - lgB, not subtracts — the sign flip
	// shrinks the mass by B(a,b)^2 and only Beta(1,1) is immune.
	front := math.Exp(a*math.Log(x) + b*math.Log(1-x) + lgAB - lgA - lgB)
	// Evaluate the continued fraction in the half that converges —
	// below the mean directly, above it by symmetry.
	if x < (a+1)/(a+b+2) {
		return front * betacf(a, b, x) / a
	}
	return 1 - front*betacf(b, a, 1-x)/b
}

// BetaQuantile returns x such that P(Beta(a,b) ≤ x) ≈ q, found by
// bisection on the monotone CDF. The rate gate asks for a lower-tail
// bound, so precision past ~1e-6 buys nothing — 60 bisection steps
// overshoot that comfortably.
func BetaQuantile(a, b, q float64) float64 {
	if q <= 0 {
		return 0
	}
	if q >= 1 {
		return 1
	}
	lo, hi := 0.0, 1.0
	for range 60 {
		mid := (lo + hi) / 2
		if betaCDF(a, b, mid) < q {
			lo = mid
		} else {
			hi = mid
		}
	}
	return (lo + hi) / 2
}
