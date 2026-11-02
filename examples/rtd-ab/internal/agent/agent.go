// SPDX-License-Identifier: Apache-2.0

// Package agent implements the RTBExtensionPoint for rtd-ab: an analysis-only
// A/B experiment. GetMutations hands the request to the ab.Engine, which routes
// it to the control or test arm and records what that arm would have decided,
// then returns exactly what rtd-simple returns: the echoed id, api_version,
// model_version "none", an empty mutation list and an OK status. The response is
// identical in both arms, so no arm can change an auction.
//
// There is no per-request logging and no request dump.
package agent

import (
	"context"

	"github.com/indexexchange/rtd-ab/internal/ab"
	pb "github.com/indexexchange/rtd-ab/pkg/pb/artf"
)

// Agent implements pb.RTBExtensionPointServer.
type Agent struct {
	pb.UnimplementedRTBExtensionPointServer
	apiVersion string
	eng        *ab.Engine // nil disables the experiment (plain no-op)
}

// New builds an Agent. eng may be nil.
func New(apiVersion string, eng *ab.Engine) *Agent {
	return &Agent{apiVersion: apiVersion, eng: eng}
}

// GetMutations never returns a mutation or an error. Engine.Observe cannot fail
// or panic out of the call (each analysis is wrapped in recover).
func (a *Agent) GetMutations(_ context.Context, req *pb.RTBRequest) (*pb.RTBResponse, error) {
	if a.eng != nil {
		a.eng.Observe(req)
	}
	return &pb.RTBResponse{
		Id:        strPtr(req.GetId()), // echo the request id (required field)
		Mutations: nil,                 // an empty list is a valid response
		Metadata: &pb.Metadata{
			ApiVersion:   strPtr(a.apiVersion),
			ModelVersion: strPtr("none"),
		},
	}, nil
}

func strPtr(s string) *string { return &s }
