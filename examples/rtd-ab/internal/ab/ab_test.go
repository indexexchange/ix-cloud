// SPDX-License-Identifier: Apache-2.0

package ab

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"math"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/indexexchange/rtd-ab/internal/metrics"
	"github.com/indexexchange/rtd-ab/internal/router"
	pb "github.com/indexexchange/rtd-ab/pkg/pb/artf"
)

// ---- Content-signal strategies -----------------------------------------------

func contentOnce(t *testing.T, arm router.Arm, req *pb.RTBRequest) ContentObs {
	t.Helper()
	a := NewContentAnalysis(DefaultContentModels())
	var o Observation
	a.Strategy(arm).Analyze(req, &o)
	return o.Content
}

func TestContentControlUsesCategoriesOnly(t *testing.T) {
	req := pubReq(pubOpts{
		cat: []string{"IAB1"}, sectioncat: []string{"IAB12-3"}, pagecat: []string{"632"}, contentCat: []string{"IAB7"},
		genre: "drama", keywords: "news,sports", network: "ESPN", eids: 2,
	})
	c := contentOnce(t, router.Control, req)
	if c.Signals != 3 || c.Src[srcCategory] != 3 { // capped at 3
		t.Fatalf("%+v", c)
	}
	for _, s := range []int{srcGenre, srcKeyword, srcNetwork, srcEID, srcNone} {
		if c.Src[s] != 0 {
			t.Fatalf("control used source %s: %+v", srcNames[s], c)
		}
	}
}

func TestContentTestAddsSources(t *testing.T) {
	req := pubReq(pubOpts{cat: []string{"IAB1"}, genre: "Drama", keywords: "News, sports;ignored", kwarray: []string{"travel"}, channel: "cnn", eids: 1})
	c := contentOnce(t, router.Test, req)
	want := ContentObs{Signals: 1 + 1 + 3 + 1 + 1}
	want.Src = [numSrc]uint8{1, 1, 3, 1, 1, 0}
	if c != want {
		t.Fatalf("got %+v want %+v", c, want)
	}
	if c.Signals > 10 {
		t.Fatal("signals exceed 10")
	}
}

func TestContentNoSignals(t *testing.T) {
	for _, arm := range []router.Arm{router.Control, router.Test} {
		c := contentOnce(t, arm, pubReq(pubOpts{cat: []string{"bogus", "IAB99"}, genre: "polka", keywords: "zzz,qqq"}))
		if c.Signals != 0 || c.Src[srcNone] != 1 {
			t.Fatalf("%s: %+v", arm, c)
		}
	}
}

func TestContentCategoryValidationAndDedupe(t *testing.T) {
	for in, want := range map[string]bool{"IAB1": true, "iab26": true, "IAB12-3": true, "IAB27": false, "IAB0": false, "IAB1-": false,
		"IAB1-100": false, "1": true, "1500": true, "1501": false, "0": false, "": false, "12a": false, "IAB": false, "-5": false} {
		if got := knownCategory(in); got != want {
			t.Errorf("knownCategory(%q)=%v want %v", in, got, want)
		}
	}
	c := contentOnce(t, router.Control, pubReq(pubOpts{cat: []string{"IAB1", "IAB1"}, pagecat: []string{"IAB1"}}))
	if c.Src[srcCategory] != 1 {
		t.Fatalf("duplicates counted: %+v", c)
	}
}

func TestContentAppAndDooh(t *testing.T) {
	c := contentOnce(t, router.Control, pubReq(pubOpts{app: true, cat: []string{"IAB1"}}))
	if c.Src[srcCategory] != 1 {
		t.Fatalf("app cat: %+v", c)
	}
}

func TestContentKeywordCaps(t *testing.T) {
	long := strings.Repeat("news,", 500) // far more than maxTokens
	c := contentOnce(t, router.Test, pubReq(pubOpts{keywords: long}))
	if c.Src[srcKeyword] != capKeyword {
		t.Fatalf("keyword hits %d want cap %d", c.Src[srcKeyword], capKeyword)
	}
	// Tokens longer than maxTokLen never match and never overrun the stack buffer.
	c = contentOnce(t, router.Test, pubReq(pubOpts{keywords: strings.Repeat("a", 5000)}))
	if c.Signals != 0 {
		t.Fatalf("%+v", c)
	}
}

func TestContentModelsSelectSourcesAndAllowlists(t *testing.T) {
	cat, _ := SourceBit("category")
	genre, _ := SourceBit("genre")
	kw, _ := SourceBit("keyword")
	m := [router.NumArms]ContentModel{
		router.Control: {Sources: cat | genre, Genres: []string{"polka"}},
		router.Test:    {Sources: kw, Keywords: []string{"zzz", "Qqq"}},
	}
	a := NewContentAnalysis(m)
	req := pubReq(pubOpts{cat: []string{"IAB1"}, genre: "Polka", keywords: "ZZZ qqq news", eids: 2, network: "ESPN"})
	var o Observation
	a.Strategy(router.Control).Analyze(req, &o)
	c := o.Content
	if c.Signals != 2 || c.Src[srcCategory] != 1 || c.Src[srcGenre] != 1 || c.Src[srcKeyword] != 0 || c.Src[srcEID] != 0 {
		t.Fatalf("control: %+v", c)
	}
	o = Observation{}
	a.Strategy(router.Test).Analyze(req, &o)
	c = o.Content
	if c.Signals != 2 || c.Src[srcKeyword] != 2 || c.Src[srcCategory] != 0 { // "news" is not in this model's list
		t.Fatalf("test: %+v", c)
	}
	if a.Strategy(router.Control).Name() != "content_category+genre" || a.Strategy(router.Test).Name() != "content_keyword" {
		t.Fatalf("names: %s %s", a.Strategy(router.Control).Name(), a.Strategy(router.Test).Name())
	}
	// An empty (non-nil) allowlist matches nothing; nil means the built-in list.
	e := NewContentAnalysis([router.NumArms]ContentModel{router.Control: {Sources: genre, Genres: []string{}}, router.Test: {Sources: genre}})
	for arm, want := range map[router.Arm]uint8{router.Control: 0, router.Test: 1} {
		o = Observation{}
		e.Strategy(arm).Analyze(pubReq(pubOpts{genre: "drama"}), &o)
		if o.Content.Src[srcGenre] != want {
			t.Fatalf("%s: genre hits %d want %d", arm, o.Content.Src[srcGenre], want)
		}
	}
}

// ---- Engine -----------------------------------------------------------------

type panicAnalysis struct{ ContentAnalysis }

func (p *panicAnalysis) Strategy(arm router.Arm) Strategy { return panicStrategy{} }

type panicStrategy struct{}

func (panicStrategy) Name() string                         { return "boom" }
func (panicStrategy) Analyze(*pb.RTBRequest, *Observation) { panic("boom") }

func TestPanicIsRecoveredAndCounted(t *testing.T) {
	for _, arm := range []router.Arm{router.Control, router.Test} {
		e := forceArm(arm, &panicAnalysis{*NewContentAnalysis(DefaultContentModels())})
		for i := 0; i < 5; i++ {
			e.Observe(pubReq(pubOpts{}))
		}
		if e.ErrorCount(arm) != 5 || e.ErrorCount(1-arm) != 0 {
			t.Fatalf("%s errors=%d", arm, e.ErrorCount(arm))
		}
	}
}

func TestPanicDoesNotStopOtherAnalyses(t *testing.T) {
	good := NewContentAnalysis(DefaultContentModels())
	e := forceArm(router.Control, &panicAnalysis{*NewContentAnalysis(DefaultContentModels())}, good)
	e.Observe(pubReq(pubOpts{cat: []string{"IAB1"}}))
	if e.ErrorCount(router.Control) != 1 || good.rec[router.Control].signals.total() != 1 {
		t.Fatalf("errors=%d good=%d", e.ErrorCount(router.Control), good.rec[router.Control].signals.total())
	}
}

func TestNilAndEmptyRequestsDoNotPanic(t *testing.T) {
	e := NewEngine(router.New(8), "50", NewContentAnalysis(DefaultContentModels()))
	reqs := []*pb.RTBRequest{{}, {Lifecycle: lcp(LifecyclePublisherBidRequest)}, {Lifecycle: lcp(LifecyclePublisherBidRequest), BidRequest: pubReq(pubOpts{}).BidRequest}}
	for i := 0; i < 20; i++ {
		for _, r := range reqs {
			e.Observe(r)
		}
	}
	if e.ErrorCount(router.Control)+e.ErrorCount(router.Test) != 0 {
		t.Fatal("unexpected recovered panics")
	}
}

// 2 of 16 hex digits are test: 20 of 160 consecutive auction ids.
func TestRoutingByAuctionID(t *testing.T) {
	e := NewEngine(router.New(2), "12.5", NewContentAnalysis(DefaultContentModels()))
	for i := 0; i < 160; i++ {
		e.Observe(withAuction(pubReq(pubOpts{}), i))
	}
	if e.RequestCount(router.Test) != 20 || e.RequestCount(router.Control) != 140 {
		t.Fatalf("test=%d control=%d", e.RequestCount(router.Test), e.RequestCount(router.Control))
	}
}

func TestOtherLifecycleAndUnroutableAreCountedAndNotAnalyzed(t *testing.T) {
	good := NewContentAnalysis(DefaultContentModels())
	e := NewEngine(router.New(8), "50", good)
	mk := func(id *string) *pb.RTBRequest {
		r := pubReq(pubOpts{cat: []string{"IAB1"}})
		if id == nil {
			r.BidRequest = nil
		} else {
			r.BidRequest.Id = id
		}
		return r
	}
	for _, id := range []*string{nil, sp(""), sp("no-underscore-g"), sp("_x"), sp("uuid-zz_x")} {
		e.Observe(mk(id))
	}
	other := mk(sp(auctionID(1)))
	other.Lifecycle = lcp(pb.Lifecycle(2))
	e.Observe(other)
	e.Observe(&pb.RTBRequest{})
	want := map[router.Reason]uint64{router.NoID: 2, router.NotHex: 3, router.OtherLifecycle: 2}
	for why, n := range want {
		if got := e.UnroutedCount(why); got != n {
			t.Errorf("%s: %d want %d", why, got, n)
		}
	}
	if e.RequestCount(router.Control)+e.RequestCount(router.Test) != 0 || good.rec[router.Control].signals.total()+good.rec[router.Test].signals.total() != 0 {
		t.Fatal("an unassignable request was analyzed")
	}
}

func TestConcurrentExactTotals(t *testing.T) {
	e := NewEngine(router.New(2), "12.5", NewContentAnalysis(DefaultContentModels()))
	var pubs [16]*pb.RTBRequest
	for n := range pubs {
		pubs[n] = withAuction(pubReq(pubOpts{cat: []string{"IAB1"}, genre: "news"}), n)
	}
	const goroutines, per = 64, 1024
	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < per; i++ {
				e.Observe(pubs[i&15])
			}
		}()
	}
	wg.Wait()
	total := uint64(goroutines * per) // 65,536; digits cycle evenly: 2/16 test
	if got := e.RequestCount(router.Test); got != total/8 {
		t.Errorf("test=%d want %d", got, total/8)
	}
	if got := e.RequestCount(router.Control); got != total-total/8 {
		t.Errorf("control=%d", got)
	}
	c := e.all[0].(*ContentAnalysis)
	if c.rec[router.Test].signals.total() != total/8 || c.rec[router.Control].signals.total() != total-total/8 {
		t.Error("recorder totals differ from routed totals")
	}
	if e.ErrorCount(router.Control)+e.ErrorCount(router.Test) != 0 {
		t.Error("errors under concurrency")
	}
}

// ---- Metrics ----------------------------------------------------------------

var declared = []string{
	"rtd_ab_test_pct", "rtd_ab_build_info", "rtd_ab_models_info", "rtd_ab_requests_total", "rtd_ab_unrouted_total",
	"rtd_ab_analyze_seconds", "rtd_ab_errors_total", "rtd_ab_content_enrichable_total",
	"rtd_ab_content_signals", "rtd_ab_content_signals_sumsq", "rtd_ab_content_signal_total",
}

func newStack(g uint8) (*Engine, *metrics.Registry) {
	e := NewEngine(router.New(g), router.BPOf(g).Pct(), NewContentAnalysis(DefaultContentModels()))
	reg := metrics.New("test")
	reg.AddExporter(e.WriteMetrics)
	reg.SetReady(true)
	return e, reg
}

func scrape(reg *metrics.Registry) string {
	rec := httptest.NewRecorder()
	reg.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	return rec.Body.String()
}

func TestIdleMetricsHaveAllHeadersAndRoutingSeries(t *testing.T) {
	_, reg := newStack(2)
	body := scrape(reg)
	for _, n := range declared {
		if !strings.Contains(body, "# HELP "+n+" ") || !strings.Contains(body, "# TYPE "+n+" ") {
			t.Errorf("missing HELP/TYPE for %s", n)
		}
	}
	if v, ok := sample(body, "rtd_ab_test_pct "); !ok || v != "12.5" {
		t.Errorf("rtd_ab_test_pct=%q", v)
	}
	if got := strings.Count(body, "rtd_ab_requests_total{"); got != 2 {
		t.Errorf("routing series=%d want 2 (one per arm)", got)
	}
	for _, lazy := range []string{"rtd_ab_content_signal_total{", "rtd_ab_analyze_seconds_bucket", "rtd_ab_errors_total{", "rtd_ab_content_signals_bucket", "rtd_ab_unrouted_total{"} {
		if strings.Contains(body, lazy) {
			t.Errorf("series %s emitted before any observation", lazy)
		}
	}
	if !strings.Contains(body, `rtd_ab_build_info{test_pct="12.5",strategy_control="content_category",strategy_test="content_category+genre+keyword+network+eid"} 1`) {
		t.Errorf("build_info wrong:\n%s", body)
	}
	if len(body) > 6*1024 {
		t.Errorf("idle payload %d bytes, budget 6144", len(body))
	}
	t.Logf("idle payload: %d bytes, %d series", len(body), series(body))
}

func TestLoadedPayloadAndSeriesBounds(t *testing.T) {
	e, reg := newStack(8)
	for _, r := range mixedTraffic(40000, "tok") {
		e.Observe(r)
	}
	e.errors[router.Control].Add(1)
	e.errors[router.Test].Add(1)
	body := scrape(reg)
	n := series(body)
	if n > 300 {
		t.Errorf("series=%d, bound 300", n)
	}
	if len(body) > 25*1024 {
		t.Errorf("loaded payload %d bytes, budget 25600", len(body))
	}
	t.Logf("loaded payload: %d bytes, %d series", len(body), n)
	for _, want := range []string{
		`rtd_ab_content_signal_total{arm="test",source="none"}`, `rtd_ab_content_signal_total{arm="control",source="category"}`,
		`rtd_ab_content_signal_total{arm="test",source="eid"}`, `rtd_ab_analyze_seconds_count{arm="test"}`, `rtd_ab_errors_total{arm="control"}`,
		`rtd_ab_content_signals_sumsq{arm="control"}`, `rtd_ab_unrouted_total{reason="not_hex"}`, `rtd_ab_unrouted_total{reason="other_lifecycle"}`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing series %s", want)
		}
	}
}

func TestMetricsMoveWithTraffic(t *testing.T) {
	e, reg := newStack(router.Groups) // everything to test
	for i := 0; i < 100; i++ {
		e.Observe(pubReq(pubOpts{cat: []string{"IAB1"}, keywords: "news"}))
	}
	body := scrape(reg)
	for prefix, want := range map[string]string{
		`rtd_ab_requests_total{arm="test"} `:                        "100",
		`rtd_ab_requests_total{arm="control"} `:                     "0",
		`rtd_ab_content_enrichable_total{arm="test"} `:              "100",
		`rtd_ab_content_signal_total{arm="test",source="keyword"} `: "100",
		`rtd_ab_content_signals_count{arm="test"} `:                 "100",
	} {
		if v, ok := sample(body, prefix); !ok || v != want {
			t.Errorf("%s got %q want %s", prefix, v, want)
		}
	}
}

// _sum, _count and _sumsq must be consistent: mean and variance recover the data.
func TestSumSqSupportsVariance(t *testing.T) {
	e, reg := newStack(router.Groups)
	reqs := []*pb.RTBRequest{
		pubReq(pubOpts{}), pubReq(pubOpts{cat: []string{"IAB1"}}), pubReq(pubOpts{cat: []string{"IAB1", "IAB2"}, genre: "news"}),
		pubReq(pubOpts{cat: []string{"IAB1", "IAB2", "IAB3"}, genre: "news", keywords: "news,sports", eids: 1}),
	}
	for _, r := range reqs {
		e.Observe(r)
	}
	body := scrape(reg)
	get := func(p string) float64 {
		v, ok := sample(body, p)
		if !ok {
			t.Fatalf("missing %s", p)
		}
		var f float64
		fmt.Sscanf(v, "%g", &f)
		return f
	}
	vals := []float64{0, 1, 3, 3 + 1 + 2 + 1}
	var ws, wq float64
	for _, v := range vals {
		ws += v
		wq += v * v
	}
	if sum, cnt, sq := get(`rtd_ab_content_signals_sum{arm="test"}`), get(`rtd_ab_content_signals_count{arm="test"}`), get(`rtd_ab_content_signals_sumsq{arm="test"}`); cnt != 4 || math.Abs(sum-ws) > 1e-9 || math.Abs(sq-wq) > 1e-9 {
		t.Fatalf("sum=%v want %v, sumsq=%v want %v, count=%v", sum, ws, sq, wq, cnt)
	}
}

func TestHistogramBucketsAreCumulativeAndEndAtCount(t *testing.T) {
	e, reg := newStack(router.Groups)
	for i := 0; i < 50; i++ {
		e.Observe(pubReq(pubOpts{cat: []string{"IAB1"}, genre: "news", keywords: "news,sports", eids: 1}))
	}
	body := scrape(reg)
	var prev float64 = -1
	for _, l := range strings.Split(body, "\n") {
		if strings.HasPrefix(l, `rtd_ab_content_signals_bucket{arm="test"`) {
			var v float64
			fmt.Sscanf(l[strings.LastIndex(l, " "):], "%g", &v)
			if v < prev {
				t.Fatalf("bucket decreased: %s", l)
			}
			prev = v
		}
	}
	if prev != 50 {
		t.Fatalf("+Inf bucket=%v want 50", prev)
	}
}

func TestModelsInfoMetric(t *testing.T) {
	e, reg := newStack(4)
	if body := scrape(reg); strings.Contains(body, "rtd_ab_models_info{") {
		t.Fatal("models_info series before SetModels")
	}
	e.SetModels("file", "model_a", "model_b")
	body := scrape(reg)
	for _, want := range []string{`rtd_ab_models_info{arm="control",model="model_a",source="file"} 1`, `rtd_ab_models_info{arm="test",model="model_b",source="file"} 1`, "# TYPE rtd_ab_models_info gauge"} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %s", want)
		}
	}
}

// ---- Logging and sentinel ---------------------------------------------------

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met within 5s")
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func TestSentinelNeverReachesLogsOrMetrics(t *testing.T) {
	const token = "SENTINELx9f3a7c1e"
	e, reg := newStack(5)
	var mu sync.Mutex
	var logs bytes.Buffer
	logf := func(f string, a ...any) { mu.Lock(); fmt.Fprintf(&logs, f+"\n", a...); mu.Unlock() }

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { e.RunSummary(ctx, 5*time.Millisecond, logf); close(done) }()
	for _, r := range mixedTraffic(5000, token) {
		r.Id = sp(token)
		e.Observe(r)
	}
	waitFor(t, func() bool { mu.Lock(); defer mu.Unlock(); return logs.Len() > 0 })
	cancel()
	<-done

	mu.Lock()
	defer mu.Unlock()
	body := scrape(reg)
	for _, s := range []string{token, "pub.example", "example", "imp-1", "00000000-0000-4000"} {
		if strings.Contains(logs.String(), s) || strings.Contains(body, s) {
			t.Fatalf("request-derived string %q leaked into logs or /metrics", s)
		}
	}
	if logs.Len() == 0 {
		t.Fatal("summary produced no log lines")
	}
}

func TestSummaryAtMostOneLinePerArmPerIntervalAndSilentWhenIdle(t *testing.T) {
	e, _ := newStack(8)
	const interval = 50 * time.Millisecond
	var mu sync.Mutex
	lines := 0
	logf := func(string, ...any) { mu.Lock(); lines++; mu.Unlock() }
	count := func() int { mu.Lock(); defer mu.Unlock(); return lines }

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { e.RunSummary(ctx, interval, logf); close(done) }()
	time.Sleep(3 * interval) // idle: no lines
	if n := count(); n != 0 {
		t.Fatalf("%d lines while idle", n)
	}
	start := time.Now()
	for i := 0; i < 100; i++ {
		e.Observe(withAuction(pubReq(pubOpts{}), i))
	}
	waitFor(t, func() bool { return count() > 0 })
	time.Sleep(2 * interval) // traffic has stopped: later ticks must stay silent
	cancel()
	<-done
	ticks := int(time.Since(start)/interval) + 1
	if n := count(); n < 1 || n > 2*ticks {
		t.Fatalf("lines=%d ticks<=%d", n, ticks)
	}
}

func TestNoLoggingOnRequestPath(t *testing.T) {
	var buf bytes.Buffer
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)
	e, _ := newStack(8)
	e2 := forceArm(router.Test, &panicAnalysis{*NewContentAnalysis(DefaultContentModels())}) // panics every request
	for i := 0; i < 1000; i++ {
		e.Observe(withAuction(pubReq(pubOpts{}), i))
		e2.Observe(pubReq(pubOpts{}))
	}
	if buf.Len() != 0 {
		t.Fatalf("request path logged %d bytes: %q", buf.Len(), buf.String())
	}
}
