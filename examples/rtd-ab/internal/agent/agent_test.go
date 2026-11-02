// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"context"
	"testing"

	"github.com/indexexchange/rtd-ab/internal/ab"
	"github.com/indexexchange/rtd-ab/internal/router"
	openrtb "github.com/indexexchange/rtd-ab/pkg/pb/openrtb"
	pb "github.com/indexexchange/rtd-ab/pkg/pb/artf"
)

func s(v string) *string { return &v }

func req(id string, lc pb.Lifecycle) *pb.RTBRequest {
	return &pb.RTBRequest{
		Lifecycle: &lc,
		Id:        s(id),
		BidRequest: &openrtb.BidRequest{
			Id:                       s("00000000-0000-4000-8000-00000000000" + string("0123456789abcdef"[len(id)%16]) + "_ssp"),
			DistributionchannelOneof: &openrtb.BidRequest_Site_{Site: &openrtb.BidRequest_Site{Domain: s("example.com"), Cat: []string{"IAB1"}}},
			Imp:                      []*openrtb.BidRequest_Imp{{Id: s("imp-1")}},
		},
	}
}

func newEngine(g uint8) *ab.Engine {
	return ab.NewEngine(router.New(g), router.BPOf(g).Pct(), ab.NewContentAnalysis(ab.DefaultContentModels()))
}

// TestZeroMutationsEveryArmAndLifecycle proves no arm and no lifecycle ever
// mutates, over 20,000 requests, and that the response is identical in both arms
// (echoed id, api_version, model_version "none").
func TestZeroMutationsEveryArmAndLifecycle(t *testing.T) {
	for _, bp := range []uint8{0, 2, 8, router.Groups} {
		a := New("9.9.9", newEngine(bp))
		for i := 0; i < 5000; i++ {
			for _, r := range []*pb.RTBRequest{
				req("pub", ab.LifecyclePublisherBidRequest),
				req("dsp", pb.Lifecycle_LIFECYCLE_DSP_BID_RESPONSE),
				req("uns", pb.Lifecycle_LIFECYCLE_UNSPECIFIED),
			} {
				resp, err := a.GetMutations(context.Background(), r)
				if err != nil {
					t.Fatalf("bp=%d error: %v", bp, err)
				}
				if len(resp.GetMutations()) != 0 {
					t.Fatalf("bp=%d: %d mutations", bp, len(resp.GetMutations()))
				}
				if resp.GetId() != r.GetId() || resp.GetMetadata().GetApiVersion() != "9.9.9" || resp.GetMetadata().GetModelVersion() != "none" {
					t.Fatalf("bp=%d: response metadata wrong: %v", bp, resp)
				}
			}
		}
	}
}

func TestNilEngineIsPlainNoOp(t *testing.T) {
	resp, err := New("1", nil).GetMutations(context.Background(), req("x", ab.LifecyclePublisherBidRequest))
	if err != nil || len(resp.GetMutations()) != 0 || resp.GetId() != "x" {
		t.Fatalf("%v %v", resp, err)
	}
}

// TestResponsesIdenticalAcrossArms compares the serialized response from an
// all-control and an all-test agent for the same request.
func TestResponsesIdenticalAcrossArms(t *testing.T) {
	c, tst := New("v", newEngine(0)), New("v", newEngine(router.Groups))
	for _, r := range []*pb.RTBRequest{req("a", ab.LifecyclePublisherBidRequest), req("b", ab.LifecyclePublisherBidRequest)} {
		rc, _ := c.GetMutations(context.Background(), r)
		rt, _ := tst.GetMutations(context.Background(), r)
		if rc.String() != rt.String() {
			t.Fatalf("responses differ between arms:\n%s\n%s", rc, rt)
		}
	}
}
