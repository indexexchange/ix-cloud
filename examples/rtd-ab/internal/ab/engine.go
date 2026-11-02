// SPDX-License-Identifier: Apache-2.0

package ab

import (
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/indexexchange/rtd-ab/internal/router"
	pb "github.com/indexexchange/rtd-ab/pkg/pb/artf"
)

// analyzeSampleN: rtd_ab_analyze_seconds times 1 request in this many, per arm
// and lifecycle. It must be a power of two. Two time.Now() calls cost about
// 50 ns, so timing every request would add more than the 100 ns allowed;
// sampling keeps the average near 3 ns. The HELP line says so.
const analyzeSampleN = 16

var analyzeSpec = newHistSpec(1e9, 1e-6, 2e-6, 5e-6, 1e-5, 2e-5, 5e-5, 1e-4, 2e-4, 5e-4, 1e-3)

// padded keeps hot counters on separate cache lines.
type paddedCounter struct {
	atomic.Uint64
	_ [56]byte
}

// Engine routes requests by auction id and runs the arm's analyses. All state is
// atomic.
type Engine struct {
	rt       *router.Router
	all      []Analysis
	requests [router.NumArms]paddedCounter
	errors   [router.NumArms]paddedCounter
	unrouted [router.NumReasons]paddedCounter
	timing   [router.NumArms]atomicHist
	pool     sync.Pool
	pctText  string
	source   string                 // where the models came from: file, builtin_absent, builtin_invalid
	models   [router.NumArms]string // model directory name per arm
}

// NewEngine builds an Engine for the given router and analyses. pctText is the
// effective percentage shown in rtd_ab_build_info.
func NewEngine(rt *router.Router, pctText string, analyses ...Analysis) *Engine {
	e := &Engine{rt: rt, all: analyses, pctText: pctText}
	e.pool.New = func() any { return new(Observation) }
	for arm := range e.timing {
		e.timing[arm].spec = analyzeSpec
	}
	return e
}

// SetModels records which models the arms run, for rtd_ab_models_info. Call before
// serving. source is "file", "builtin_absent" or "builtin_invalid"; the names are
// validated model directory names or "builtin".
func (e *Engine) SetModels(source, control, test string) {
	e.source, e.models[router.Control], e.models[router.Test] = source, control, test
}

// Observe routes req and runs the chosen arm's analyses. It never panics, never
// blocks on a lock, and never returns an error: the caller's response does not
// depend on it.
func (e *Engine) Observe(req *pb.RTBRequest) {
	if req.GetLifecycle() != LifecyclePublisherBidRequest {
		e.unrouted[router.OtherLifecycle].Add(1)
		return
	}
	arm, why := e.rt.Route(req.GetBidRequest().GetId())
	if why != router.Routed {
		// No auction id to assign by: count it and skip the analysis, so neither
		// arm receives traffic that was not randomized.
		e.unrouted[why].Add(1)
		return
	}
	c := e.requests[arm].Add(1)
	timed := c&(analyzeSampleN-1) == 0
	var t0 time.Time
	if timed {
		t0 = time.Now()
	}
	o := e.pool.Get().(*Observation)
	*o = Observation{}
	for _, a := range e.all {
		e.run(a, arm, req, o)
	}
	e.pool.Put(o)
	if timed {
		e.timing[arm].observe(int64(time.Since(t0)))
	}
}

// run isolates one analysis: a panic is counted and swallowed.
func (e *Engine) run(a Analysis, arm router.Arm, req *pb.RTBRequest, o *Observation) {
	defer func() {
		if r := recover(); r != nil {
			// Count only. Logging here could run once per request under a bad
			// input, so the error rate is exposed as rtd_ab_errors_total.
			e.errors[arm].Add(1)
		}
	}()
	a.Strategy(arm).Analyze(req, o)
	a.Record(arm, o)
}

// ErrorCount returns recovered strategy panics for arm.
func (e *Engine) ErrorCount(arm router.Arm) uint64 { return e.errors[arm].Load() }

// RequestCount returns routed requests for arm.
func (e *Engine) RequestCount(arm router.Arm) uint64 { return e.requests[arm].Load() }

// UnroutedCount returns requests that could not be assigned, by reason.
func (e *Engine) UnroutedCount(why router.Reason) uint64 {
	if why >= router.NumReasons {
		return 0
	}
	return e.unrouted[why].Load()
}

// StrategyNames joins the strategy names of every analysis for one arm.
func (e *Engine) StrategyNames(arm router.Arm) string {
	names := make([]string, 0, len(e.all))
	for _, a := range e.all {
		names = append(names, a.Strategy(arm).Name())
	}
	return strings.Join(names, ",")
}

// WriteMetrics is the exporter registered with the metrics registry. HELP and
// TYPE lines are always written; series appear after their first observation,
// except rtd_ab_test_pct, rtd_ab_models_info and the routed-request counters,
// which are always present.
func (e *Engine) WriteMetrics(w io.Writer) {
	header(w, "rtd_ab_test_pct", "gauge", "Effective share of auctions routed to the test arm, in percent (a multiple of 6.25).")
	fmt.Fprintf(w, "rtd_ab_test_pct %s\n", fmtFloat(float64(e.rt.BP())/100))
	header(w, "rtd_ab_build_info", "gauge", "Effective split and strategy names; value is always 1.")
	fmt.Fprintf(w, "rtd_ab_build_info{test_pct=%q,strategy_control=%q,strategy_test=%q} 1\n",
		e.pctText, e.StrategyNames(router.Control), e.StrategyNames(router.Test))
	header(w, "rtd_ab_models_info", "gauge", "Model directory per arm and where the models came from (file, builtin_absent, builtin_invalid); value is always 1.")
	if e.source != "" {
		for arm := router.Arm(0); arm < router.NumArms; arm++ {
			fmt.Fprintf(w, "rtd_ab_models_info{arm=%q,model=%q,source=%q} 1\n", arm.String(), e.models[arm], e.source)
		}
	}

	header(w, "rtd_ab_requests_total", "counter", "Auctions routed, by arm.")
	for arm := router.Arm(0); arm < router.NumArms; arm++ {
		fmt.Fprintf(w, "rtd_ab_requests_total{arm=%q} %d\n", arm.String(), e.requests[arm].Load())
	}

	header(w, "rtd_ab_unrouted_total", "counter", "Requests not analyzed, by reason (no_id, not_hex, other_lifecycle).")
	for why := router.NoID; why < router.NumReasons; why++ {
		if v := e.unrouted[why].Load(); v != 0 {
			fmt.Fprintf(w, "rtd_ab_unrouted_total{reason=%q} %d\n", why.String(), v)
		}
	}

	header(w, "rtd_ab_analyze_seconds", "histogram", "Analysis time per request in seconds, sampled 1 in 16 (timing every request costs about 50 ns).")
	for arm := router.Arm(0); arm < router.NumArms; arm++ {
		e.timing[arm].writeSeries(w, "rtd_ab_analyze_seconds", fmt.Sprintf("arm=%q", arm.String()))
	}

	header(w, "rtd_ab_errors_total", "counter", "Recovered strategy panics, by arm.")
	for arm := router.Arm(0); arm < router.NumArms; arm++ {
		if v := e.errors[arm].Load(); v != 0 {
			fmt.Fprintf(w, "rtd_ab_errors_total{arm=%q} %d\n", arm.String(), v)
		}
	}

	for _, a := range e.all {
		a.WriteMetrics(w)
	}
}

// RunSummary logs at most one line per arm per interval, only for arms that saw
// requests in the interval, until ctx is cancelled. The lines carry counts and
// rates only.
func (e *Engine) RunSummary(ctx context.Context, every time.Duration, logf func(string, ...any)) {
	if every <= 0 {
		return
	}
	t := time.NewTicker(every)
	defer t.Stop()
	var prev [router.NumArms]uint64
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			for arm := router.Arm(0); arm < router.NumArms; arm++ {
				total := e.requests[arm].Load()
				if total == prev[arm] {
					continue
				}
				prev[arm] = total
				var b strings.Builder
				fmt.Fprintf(&b, "rtd-ab summary arm=%s requests=%d errors=%d", arm, total, e.errors[arm].Load())
				for _, a := range e.all {
					b.WriteByte(' ')
					b.WriteString(a.SummaryLine(arm))
				}
				logf("%s", b.String())
			}
		}
	}
}
