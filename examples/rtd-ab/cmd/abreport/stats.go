// SPDX-License-Identifier: Apache-2.0

package main

import "math"

const z95 = 1.959963984540054

// propTest compares two proportions: x1/n1 (control) and x2/n2 (test).
type propTest struct {
	P1, P2 float64 // control, test rates
	Diff   float64 // P2 - P1
	CILo   float64 // 95% CI of Diff, unpooled standard error
	CIHi   float64
	Z      float64 // pooled two-proportion z statistic
	PValue float64 // two-sided, normal approximation
	Valid  bool
	N1, N2 float64
}

func twoProportion(x1, n1, x2, n2 float64) propTest {
	t := propTest{N1: n1, N2: n2}
	if n1 <= 0 || n2 <= 0 {
		return t
	}
	t.P1, t.P2 = x1/n1, x2/n2
	t.Diff = t.P2 - t.P1
	se := math.Sqrt(t.P1*(1-t.P1)/n1 + t.P2*(1-t.P2)/n2)
	t.CILo, t.CIHi = t.Diff-z95*se, t.Diff+z95*se
	p := (x1 + x2) / (n1 + n2)
	sePooled := math.Sqrt(p * (1 - p) * (1/n1 + 1/n2))
	if sePooled > 0 {
		t.Z = t.Diff / sePooled
		t.PValue = math.Erfc(math.Abs(t.Z) / math.Sqrt2)
	} else {
		t.PValue = 1
	}
	t.Valid = true
	return t
}

// moments are the three exported series of a continuous measure.
type moments struct{ N, Sum, SumSq float64 }

func (m moments) mean() float64 {
	if m.N == 0 {
		return 0
	}
	return m.Sum / m.N
}

// variance is the unbiased sample variance from _sum, _count and _sumsq.
func (m moments) variance() float64 {
	if m.N < 2 {
		return 0
	}
	v := (m.SumSq - m.Sum*m.Sum/m.N) / (m.N - 1)
	if v < 0 { // rounding
		return 0
	}
	return v
}

// welchTest compares the means of control (a) and test (b).
type welchTest struct {
	Mean1, Mean2 float64
	Diff         float64 // Mean2 - Mean1
	CILo, CIHi   float64 // 95% CI, normal critical value (df is large above 100 samples per arm)
	T, DF        float64
	PValue       float64 // two-sided, normal approximation
	Valid        bool
}

func welch(a, b moments) welchTest {
	w := welchTest{Mean1: a.mean(), Mean2: b.mean()}
	w.Diff = w.Mean2 - w.Mean1
	if a.N < 2 || b.N < 2 {
		return w
	}
	va, vb := a.variance()/a.N, b.variance()/b.N
	se := math.Sqrt(va + vb)
	if se == 0 {
		w.Valid = true
		w.PValue = 1
		return w
	}
	w.T = w.Diff / se
	w.DF = (va + vb) * (va + vb) / (va*va/(a.N-1) + vb*vb/(b.N-1))
	w.CILo, w.CIHi = w.Diff-z95*se, w.Diff+z95*se
	w.PValue = math.Erfc(math.Abs(w.T) / math.Sqrt2)
	w.Valid = true
	return w
}

// srm is the sample-ratio check of one lifecycle.
type srm struct {
	Test, Control float64
	Expected      float64 // configured test share, 0..1
	Observed      float64
	SE            float64
	Sigmas        float64 // |observed - expected| / SE
	Flag          bool    // more than 3 standard errors apart
}

func sampleRatio(test, control, expected float64) srm {
	r := srm{Test: test, Control: control, Expected: expected}
	n := test + control
	if n == 0 {
		return r
	}
	r.Observed = test / n
	r.SE = math.Sqrt(expected * (1 - expected) / n)
	dev := math.Abs(r.Observed - expected)
	switch {
	case r.SE > 0:
		r.Sigmas = dev / r.SE
		r.Flag = r.Sigmas > 3
	default:
		r.Flag = dev > 1e-12
		if r.Flag {
			r.Sigmas = math.Inf(1)
		}
	}
	return r
}
