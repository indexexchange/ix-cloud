// SPDX-License-Identifier: Apache-2.0

// Package health provides the health signalling Index Cloud requires:
//
//   - gRPC health (grpc.health.v1.Health) on GRPC_PORT — "Building your container
//     → Required endpoints" mandates the gRPC healthcheck protocol.
//   - HTTP /heartbeat (liveness) and /healthcheck (readiness) on SERVER_PORT —
//     listed alongside /metrics in the SERVER_PORT env-var description.
//
// Readiness flips to SERVING only after the model is loaded, so Index does not
// route traffic before the container is warm.
package health

import (
	"encoding/json"
	"net/http"
	"sync/atomic"

	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
)

// serviceName "" is the conventional overall-server entry kubelet's gRPC probe uses.
const serviceName = ""

// Checker tracks readiness and drives the gRPC health server.
type Checker struct {
	ready   int32
	grpc    *health.Server
	version string
}

// New builds a Checker wrapping a gRPC health.Server (initially NOT_SERVING).
func New(version string) *Checker {
	h := health.NewServer()
	h.SetServingStatus(serviceName, healthpb.HealthCheckResponse_NOT_SERVING)
	return &Checker{grpc: h, version: version}
}

// GRPCServer exposes the gRPC health server for healthpb.RegisterHealthServer.
func (c *Checker) GRPCServer() *health.Server { return c.grpc }

// SetReady flips readiness and mirrors it into the gRPC health status.
func (c *Checker) SetReady(ready bool) {
	if ready {
		atomic.StoreInt32(&c.ready, 1)
		c.grpc.SetServingStatus(serviceName, healthpb.HealthCheckResponse_SERVING)
	} else {
		atomic.StoreInt32(&c.ready, 0)
		c.grpc.SetServingStatus(serviceName, healthpb.HealthCheckResponse_NOT_SERVING)
	}
}

// IsReady reports current readiness.
func (c *Checker) IsReady() bool { return atomic.LoadInt32(&c.ready) == 1 }

type payload struct {
	Status  string `json:"status"`
	Version string `json:"version,omitempty"`
}

// LivenessHandler serves /heartbeat: 200 while the process runs.
func (c *Checker) LivenessHandler(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, payload{Status: "SERVING", Version: c.version})
}

// ReadinessHandler serves /healthcheck: 200 when ready, 503 otherwise.
func (c *Checker) ReadinessHandler(w http.ResponseWriter, _ *http.Request) {
	if c.IsReady() {
		writeJSON(w, http.StatusOK, payload{Status: "SERVING", Version: c.version})
		return
	}
	writeJSON(w, http.StatusServiceUnavailable, payload{Status: "NOT_SERVING", Version: c.version})
}

func writeJSON(w http.ResponseWriter, code int, p payload) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(p)
}
