// SPDX-License-Identifier: Apache-2.0

// Command abreport reads rtd-ab /metrics from one or more URLs or files and
// prints, per arm: the rates and means, the test-minus-control difference, a 95%
// confidence interval, the z or t statistic, and the sample-ratio check.
//
//	abreport http://pod-a:8080/metrics http://pod-b:8080/metrics
//	abreport scrape.txt
//	abreport -base before.txt after.txt
//
// Several inputs are summed, so pass every pod to get a fleet view. With -base
// (repeatable) the baseline scrapes are summed and subtracted from the inputs,
// which gives an exact window with no increase() extrapolation. It is not built
// into the image.
package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// samples maps `name{labels}` (labels as written) to a value, summed over inputs.
type samples map[string]float64

var lineRE = regexp.MustCompile(`^([a-zA-Z_:][a-zA-Z0-9_:]*)(\{[^}]*\})?\s+(\S+)\s*$`)

func parse(r io.Reader, into samples) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		ln := sc.Text()
		if ln == "" || ln[0] == '#' {
			continue
		}
		m := lineRE.FindStringSubmatch(ln)
		if m == nil {
			continue
		}
		v, err := strconv.ParseFloat(m[3], 64)
		if err != nil {
			continue
		}
		into[m[1]+m[2]] += v
	}
	return sc.Err()
}

func load(src string, into samples) error {
	if strings.HasPrefix(src, "http://") || strings.HasPrefix(src, "https://") {
		c := &http.Client{Timeout: 10 * time.Second}
		resp, err := c.Get(src)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("%s: HTTP %d", src, resp.StatusCode)
		}
		return parse(resp.Body, into)
	}
	f, err := os.Open(src)
	if err != nil {
		return err
	}
	defer f.Close()
	return parse(f, into)
}

func (s samples) get(name string, labels ...string) float64 {
	if len(labels) == 0 {
		return s[name]
	}
	parts := make([]string, 0, len(labels)/2)
	for i := 0; i+1 < len(labels); i += 2 {
		parts = append(parts, fmt.Sprintf("%s=%q", labels[i], labels[i+1]))
	}
	return s[name+"{"+strings.Join(parts, ",")+"}"]
}

func (s samples) moments(base, arm string) moments {
	return moments{N: s.get(base+"_count", "arm", arm), Sum: s.get(base+"_sum", "arm", arm), SumSq: s.get(base+"_sumsq", "arm", arm)}
}

type listFlag []string

func (l *listFlag) String() string     { return strings.Join(*l, ",") }
func (l *listFlag) Set(v string) error { *l = append(*l, v); return nil }

// sumInputs loads every source and returns the summed series and the set of
// rtd_ab_test_pct values seen (one per input).
func sumInputs(srcs []string) (samples, map[float64]bool, error) {
	s := samples{}
	pcts := map[float64]bool{}
	for _, src := range srcs {
		one := samples{}
		if err := load(src, one); err != nil {
			return nil, nil, err
		}
		pcts[one["rtd_ab_test_pct"]] = true
		for k, v := range one {
			if k != "rtd_ab_test_pct" {
				s[k] += v
			}
		}
	}
	return s, pcts, nil
}

func main() {
	var base listFlag
	flag.Var(&base, "base", "baseline scrape (URL or file) to subtract; repeatable, summed like the inputs")
	flag.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: abreport [-base before ...] <metrics-url-or-file> [more ...]")
	}
	flag.Parse()
	if flag.NArg() == 0 {
		flag.Usage()
		os.Exit(2)
	}
	s, pcts, err := sumInputs(flag.Args())
	if err != nil {
		fmt.Fprintln(os.Stderr, "abreport:", err)
		os.Exit(1)
	}
	if len(base) > 0 {
		b, bp, err := sumInputs(base)
		if err != nil {
			fmt.Fprintln(os.Stderr, "abreport:", err)
			os.Exit(1)
		}
		for p := range bp {
			pcts[p] = true
		}
		subtract(s, b)
	}
	if len(pcts) != 1 {
		fmt.Fprintf(os.Stderr, "abreport: inputs report different rtd_ab_test_pct values %v; the sample-ratio check needs one split\n", pcts)
		os.Exit(1)
	}
	var pct float64
	for p := range pcts {
		pct = p
	}
	if !report(os.Stdout, s, pct) {
		os.Exit(3) // sample-ratio mismatch
	}
}

// subtract removes the baseline from s. A series that went down (a pod restarted
// between the scrapes) cannot be differenced; it is reported on stderr and zeroed.
func subtract(s, base samples) {
	for k, b := range base {
		d := s[k] - b
		if d < 0 {
			fmt.Fprintf(os.Stderr, "abreport: series %s decreased between scrapes (restart?); ignored\n", k)
			d = 0
		}
		s[k] = d
	}
}

// report prints the full comparison and returns false if the sample-ratio check flags.
func report(w io.Writer, s samples, pct float64) bool {
	fmt.Fprintf(w, "rtd-ab report: configured test share %g%%, %d input series\n\n", pct, len(s))

	nC, nT := s.get("rtd_ab_requests_total", "arm", "control"), s.get("rtd_ab_requests_total", "arm", "test")
	r := sampleRatio(nT, nC, pct/100)
	status, ok := "ok", true
	if r.Flag {
		status, ok = "MISMATCH", false
	}
	fmt.Fprintln(w, "Sample-ratio check (flag when the observed share differs from the configured share by more than 3 standard errors)")
	fmt.Fprintf(w, "  test=%.0f control=%.0f observed=%.4f%% expected=%.4f%% deviation=%.2f SE  %s\n", r.Test, r.Control, r.Observed*100, r.Expected*100, r.Sigmas, status)
	var unr float64
	for _, why := range []string{"no_id", "not_hex", "other_lifecycle"} {
		unr += s.get("rtd_ab_unrouted_total", "reason", why)
	}
	fmt.Fprintf(w, "  unrouted (no assignable auction id or another lifecycle; not analyzed): %.0f\n", unr)
	fmt.Fprintln(w, "  note: routing is by the last hex digit of the auction id, so the share varies like a binomial over auctions. A flag points at skewed auction ids, an unrouted spike, a scrape gap or pods with different splits.")

	fmt.Fprintln(w, "\nContent-signal coverage what-if")
	prop(w, "enrichable rate", s.get("rtd_ab_content_enrichable_total", "arm", "control"), nC, s.get("rtd_ab_content_enrichable_total", "arm", "test"), nT)
	mean(w, "signals per request", s.moments("rtd_ab_content_signals", "control"), s.moments("rtd_ab_content_signals", "test"))
	for _, src := range []string{"category", "genre", "keyword", "network", "eid", "none"} {
		c := s.get("rtd_ab_content_signal_total", "arm", "control", "source", src)
		t := s.get("rtd_ab_content_signal_total", "arm", "test", "source", src)
		fmt.Fprintf(w, "    source %-9s per request: control=%.4f test=%.4f\n", src, ratio(c, nC), ratio(t, nT))
	}

	fmt.Fprintln(w, "\nCost")
	for _, arm := range []string{"control", "test"} {
		n := s.get("rtd_ab_analyze_seconds_count", "arm", arm)
		sum := s.get("rtd_ab_analyze_seconds_sum", "arm", arm)
		fmt.Fprintf(w, "  analyze %-8s mean=%.0f ns over %.0f sampled requests (1 in 16)\n", arm, ratio(sum, n)*1e9, n)
	}
	fmt.Fprintf(w, "  recovered strategy panics: control=%.0f test=%.0f\n", s.get("rtd_ab_errors_total", "arm", "control"), s.get("rtd_ab_errors_total", "arm", "test"))
	fmt.Fprintln(w, "\nThese are proxy and counterfactual measures. They show decision coverage and cost. They do not show auction win rate or revenue.")
	return ok
}

func ratio(a, b float64) float64 {
	if b == 0 {
		return 0
	}
	return a / b
}

func prop(w io.Writer, name string, x1, n1, x2, n2 float64) {
	t := twoProportion(x1, n1, x2, n2)
	if !t.Valid {
		fmt.Fprintf(w, "  %s: not enough data (control n=%.0f, test n=%.0f)\n", name, n1, n2)
		return
	}
	fmt.Fprintf(w, "  %s: control=%.4f%% (n=%.0f) test=%.4f%% (n=%.0f) diff=%+.4f pp 95%% CI [%+.4f, %+.4f] z=%.2f p=%.3g\n",
		name, t.P1*100, n1, t.P2*100, n2, t.Diff*100, t.CILo*100, t.CIHi*100, t.Z, t.PValue)
}

func mean(w io.Writer, name string, a, b moments) {
	t := welch(a, b)
	if !t.Valid {
		fmt.Fprintf(w, "  %s: not enough data (control n=%.0f, test n=%.0f)\n", name, a.N, b.N)
		return
	}
	note := ""
	if t.DF < 30 {
		note = "  (df < 30: normal approximation is loose)"
	}
	fmt.Fprintf(w, "  %s: control=%.5f (n=%.0f) test=%.5f (n=%.0f) diff=%+.5f 95%% CI [%+.5f, %+.5f] Welch t=%.2f df=%.0f p=%.3g%s\n",
		name, t.Mean1, a.N, t.Mean2, b.N, t.Diff, t.CILo, t.CIHi, t.T, t.DF, t.PValue, note)
}
