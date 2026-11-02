// SPDX-License-Identifier: Apache-2.0

package ab

import (
	"testing"
	"time"

	"github.com/indexexchange/rtd-ab/internal/router"
	pb "github.com/indexexchange/rtd-ab/pkg/pb/artf"
)

// A publisher request that exercises every content source.
func benchPub() *pb.RTBRequest {
	return pubReq(pubOpts{
		cat: []string{"IAB1", "IAB12-3"}, pagecat: []string{"632"}, contentCat: []string{"IAB7"},
		genre: "Drama", keywords: "News, sports, cooking, unknownword", network: "ESPN", eids: 3,
	})
}

func TestHotPathAllocsZero(t *testing.T) {
	if raceEnabled {
		t.Skip("allocation counts differ under -race")
	}
	a := NewContentAnalysis(DefaultContentModels())
	req := benchPub()
	for _, arm := range []router.Arm{router.Control, router.Test} {
		var o Observation
		strat := a.Strategy(arm)
		if n := testing.AllocsPerRun(1000, func() { o = Observation{}; strat.Analyze(req, &o) }); n != 0 {
			t.Errorf("%s Analyze allocates %v", arm, n)
		}
		if n := testing.AllocsPerRun(1000, func() { a.Record(arm, &o) }); n != 0 {
			t.Errorf("%s Record allocates %v", arm, n)
		}
	}
	for _, g := range []uint8{0, 2, router.Groups} {
		e := NewEngine(router.New(g), router.BPOf(g).Pct(), NewContentAnalysis(DefaultContentModels()))
		e.Observe(req) // warm the pool
		if n := testing.AllocsPerRun(2000, func() { e.Observe(req) }); n != 0 {
			t.Errorf("Engine.Observe allocates %v at groups=%d", n, g)
		}
	}
}

func BenchmarkStrategy(b *testing.B) {
	a := NewContentAnalysis(DefaultContentModels())
	req := benchPub()
	for _, arm := range []router.Arm{router.Control, router.Test} {
		b.Run(arm.String(), func(b *testing.B) {
			var o Observation
			strat := a.Strategy(arm)
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				o = Observation{}
				strat.Analyze(req, &o)
			}
		})
	}
}

func BenchmarkRecord(b *testing.B) {
	a := NewContentAnalysis(DefaultContentModels())
	req := benchPub()
	for _, arm := range []router.Arm{router.Control, router.Test} {
		b.Run(arm.String(), func(b *testing.B) {
			var o Observation
			a.Strategy(arm).Analyze(req, &o)
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				a.Record(arm, &o)
			}
		})
	}
}

// BenchmarkObserve is the full added per-request cost: routing, pooled
// Observation, recover wrapper, analysis, recording and sampled timing. The
// split is fixed to one arm so each sub-benchmark measures a single arm.
func BenchmarkObserve(b *testing.B) {
	req := benchPub()
	for _, arm := range []router.Arm{router.Control, router.Test} {
		b.Run(arm.String(), func(b *testing.B) {
			e := forceArm(arm, NewContentAnalysis(DefaultContentModels()))
			e.Observe(req)
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				e.Observe(req)
			}
		})
	}
}

// BenchmarkObserveMixed cycles all 16 hex digits at 2 of 16 groups (12.5%), as
// production would see them.
func BenchmarkObserveMixed(b *testing.B) {
	e := NewEngine(router.New(2), "12.5", NewContentAnalysis(DefaultContentModels()))
	var reqs [16]*pb.RTBRequest
	for n := range reqs {
		reqs[n] = withAuction(benchPub(), n)
	}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		e.Observe(reqs[i&15])
	}
}

// BenchmarkTimeNowPair is the cost of timing one request (two clock reads), the
// reason rtd_ab_analyze_seconds is sampled 1 in 16.
func BenchmarkTimeNowPair(b *testing.B) {
	b.ReportAllocs()
	var d time.Duration
	for i := 0; i < b.N; i++ {
		t0 := time.Now()
		d += time.Since(t0)
	}
	_ = d
}
