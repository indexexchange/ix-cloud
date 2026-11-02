// SPDX-License-Identifier: Apache-2.0

// Package ab runs the analysis-only A/B experiment. Each PUBLISHER_BID_REQUEST is
// routed to an arm by its auction id (internal/router), the arm's Strategy reads
// the request through generated getters and fills a fixed-size Observation, and
// the Analysis merges that Observation into lock-free atomic recorders that
// /metrics exports.
//
// No strategy ever produces a mutation. A strategy panic is recovered, counted in
// rtd_ab_errors_total, and the RPC still returns its normal empty OK response.
package ab

import (
	"io"

	"github.com/indexexchange/rtd-ab/internal/router"
	pb "github.com/indexexchange/rtd-ab/pkg/pb/artf"
)

// LifecyclePublisherBidRequest is the only lifecycle this container analyzes.
const LifecyclePublisherBidRequest = pb.Lifecycle_LIFECYCLE_PUBLISHER_BID_REQUEST

// Strategy is one arm's implementation of an analysis. Analyze reads req through
// generated getters only (no protoreflect) and writes into o. It must not
// allocate, block, or log, and must not retain req or o.
type Strategy interface {
	// Name is a bounded identifier used in rtd_ab_build_info.
	Name() string
	Analyze(req *pb.RTBRequest, o *Observation)
}

// Analysis bundles a control/test Strategy pair with the recorders and metric
// families for one what-if. To add an analysis, implement this interface and pass
// it to NewEngine (see README, "Adding a strategy").
type Analysis interface {
	Name() string
	Strategy(arm router.Arm) Strategy
	// Record merges one request's Observation into the atomic recorders. It runs
	// only when Analyze returned without panicking.
	Record(arm router.Arm, o *Observation)
	// WriteMetrics writes this analysis's families: HELP/TYPE always, series only
	// after the first observation.
	WriteMetrics(w io.Writer)
	// SummaryLine renders cumulative per-pod totals for one arm for the
	// once-a-minute log line. It contains numbers only.
	SummaryLine(arm router.Arm) string
}

// Observation is the fixed-size scratch a Strategy fills for one request. Each
// analysis owns one region; adding an analysis adds a region. The struct is
// pooled by the Engine and zeroed before each request.
type Observation struct {
	Content ContentObs
}
