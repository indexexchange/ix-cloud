// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"math"
	"strings"
	"testing"
)

func close(t *testing.T, name string, got, want, tol float64) {
	t.Helper()
	if math.Abs(got-want) > tol {
		t.Errorf("%s=%v want %v (tol %v)", name, got, want, tol)
	}
}

// Expected values computed independently with Python's math module.
func TestTwoProportion(t *testing.T) {
	r := twoProportion(300, 1000, 360, 1000)
	close(t, "diff", r.Diff, 0.06, 1e-12)
	close(t, "ci lo", r.CILo, 0.018868765340270995, 1e-9)
	close(t, "ci hi", r.CIHi, 0.10113123465972901, 1e-9)
	close(t, "z", r.Z, 2.8532635032055063, 1e-9)
	close(t, "p", r.PValue, 0.004327272365904403, 1e-9)
	if twoProportion(1, 0, 1, 5).Valid {
		t.Error("n=0 must be invalid")
	}
}

func TestWelchFromSumAndSumSq(t *testing.T) {
	a := moments{N: 2000, Sum: 301.8400294099202, SumSq: 50.588476100608084}
	b := moments{N: 3000, Sum: 536.9615361516436, SumSq: 114.6805793225036}
	// Python: diff 0.028067164, t 15.3957570, df 4988.79, CI lo 0.0244940607
	w := welch(a, b)
	close(t, "diff", w.Diff, 0.028067164012254425, 1e-9)
	close(t, "t", w.T, 15.395757009753682, 1e-5)
	close(t, "df", w.DF, 4988.793821170206, 0.5)
	close(t, "ci lo", w.CILo, 0.024494060684022725, 1e-6)
	if !w.Valid || w.PValue > 1e-6 {
		t.Errorf("%+v", w)
	}
	if welch(moments{N: 1}, b).Valid {
		t.Error("n<2 must be invalid")
	}
	if got := (moments{N: 5, Sum: 5, SumSq: 5}).variance(); got != 0 {
		t.Errorf("zero variance clamps to 0, got %v", got)
	}
}

func TestSampleRatio(t *testing.T) {
	// Exact 10% split of 100,000: no flag.
	if r := sampleRatio(10000, 90000, 0.10); r.Flag || r.Sigmas > 1e-9 {
		t.Errorf("%+v", r)
	}
	// 10.5% of 100,000 is 5.27 SE away: flag.
	if r := sampleRatio(10500, 89500, 0.10); !r.Flag || math.Abs(r.Sigmas-5.2704627669) > 1e-6 {
		t.Errorf("%+v", r)
	}
	// 2.9 SE: no flag; 3.1 SE: flag. SE at n=10000, p=0.1 is 0.003.
	if sampleRatio(1087, 8913, 0.10).Flag || !sampleRatio(1093, 8907, 0.10).Flag {
		t.Error("3 SE boundary wrong")
	}
	// Degenerate splits.
	if sampleRatio(0, 500, 0).Flag || !sampleRatio(1, 500, 0).Flag || sampleRatio(500, 0, 1).Flag || !sampleRatio(499, 1, 1).Flag {
		t.Error("degenerate split handling wrong")
	}
	if r := sampleRatio(0, 0, 0.1); r.Flag {
		t.Error("no traffic must not flag")
	}
}

const scrape = `# HELP x y
rtd_ab_test_pct 12.5
rtd_ab_requests_total{arm="control"} 8750
rtd_ab_requests_total{arm="test"} 1250
rtd_ab_unrouted_total{reason="not_hex"} 3
rtd_ab_content_enrichable_total{arm="control"} 2700
rtd_ab_content_enrichable_total{arm="test"} 540
rtd_ab_content_signals_count{arm="control"} 8750
rtd_ab_content_signals_sum{arm="control"} 4500
rtd_ab_content_signals_sumsq{arm="control"} 9000
rtd_ab_content_signals_count{arm="test"} 1250
rtd_ab_content_signals_sum{arm="test"} 1500
rtd_ab_content_signals_sumsq{arm="test"} 3500
`

func TestReportPrintsEverySection(t *testing.T) {
	s := samples{}
	if err := parse(strings.NewReader(scrape), s); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if !report(&buf, s, 12.5) {
		t.Fatalf("exact split must not flag:\n%s", buf.String())
	}
	out := buf.String()
	for _, want := range []string{"Sample-ratio check", "observed=12.5000%", "unrouted", "enrichable rate", "Welch t=", "Cost", "do not show auction win rate or revenue"} {
		if !strings.Contains(out, want) {
			t.Errorf("report missing %q:\n%s", want, out)
		}
	}
	// A skewed scrape flags and the report returns false.
	s["rtd_ab_requests_total{arm=\"test\"}"] = 1700
	buf.Reset()
	if report(&buf, s, 12.5) || !strings.Contains(buf.String(), "MISMATCH") {
		t.Errorf("skew not flagged:\n%s", buf.String())
	}
}

func TestParseSumsAcrossInputs(t *testing.T) {
	s := samples{}
	for i := 0; i < 2; i++ {
		if err := parse(strings.NewReader(`rtd_ab_requests_total{arm="test"} 5`+"\n"), s); err != nil {
			t.Fatal(err)
		}
	}
	if got := s.get("rtd_ab_requests_total", "arm", "test"); got != 10 {
		t.Fatalf("got %v", got)
	}
}

func TestSubtractWindow(t *testing.T) {
	after := samples{"a{x=\"1\"}": 150, "b": 20}
	before := samples{"a{x=\"1\"}": 100, "b": 25} // b went down: a restart
	subtract(after, before)
	if after["a{x=\"1\"}"] != 50 || after["b"] != 0 {
		t.Fatalf("%v", after)
	}
}
