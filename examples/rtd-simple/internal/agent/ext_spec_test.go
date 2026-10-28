// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"testing"

	pb "github.com/indexexchange/rtd-simple/pkg/pb/artf"
	openrtb "github.com/indexexchange/rtd-simple/pkg/pb/openrtb"
	"google.golang.org/protobuf/proto"
)

// The spec-generated bindings model "Ext ext = 99" on every OpenRTB object, so
// imp.ext round-trips as typed data (not unknown bytes), and RTBRequest carries
// the originator and applicable_intents fields.
func TestSpecBindingsModelExt(t *testing.T) {
	gpid := "gpid-1"
	in := &pb.RTBRequest{
		BidRequest:        &openrtb.BidRequest{Imp: []*openrtb.BidRequest_Imp{{Ext: &openrtb.BidRequest_Imp_Ext{Gpid: &gpid}}}},
		Originator:        &pb.Originator{},
		ApplicableIntents: []pb.Intent{1},
	}
	b, err := proto.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var out pb.RTBRequest
	if err := proto.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	imp := out.GetBidRequest().GetImp()[0]
	if imp.GetExt().GetGpid() != gpid {
		t.Fatalf("imp.ext.gpid not decoded: %v", imp.GetExt())
	}
	if len(imp.ProtoReflect().GetUnknown()) != 0 || len(out.ProtoReflect().GetUnknown()) != 0 {
		t.Fatal("ext/originator/intents should not be unknown fields")
	}
	if out.Originator == nil || len(out.ApplicableIntents) != 1 {
		t.Fatal("originator/applicable_intents lost")
	}
}
