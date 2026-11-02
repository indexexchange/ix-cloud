// SPDX-License-Identifier: Apache-2.0

// Package metrics implements the Prometheus /metrics endpoint required by Index
// Cloud. Per "Validating and deploying your container → Configure your metrics
// endpoint", the container MUST expose the histogram metric rpc.server.duration
// (rendered as rpc_server_duration) or it fails Index Cloud health checks and
// receives no traffic.
//
// Required boundaries, in SECONDS: 0.001, 0.002, 0.003, 0.004, 0.005, 0.007,
// 0.010 (per the current "Validating and deploying a container" doc; observations
// are recorded in seconds, matching the doc's example sum/buckets). Permitted
// tags: rpc.system, rpc.grpc.status_code, rpc.method, rpc.service, error_type.
// Total cardinality must stay under 10,000; the tag set here is tiny.
//
// A tiny dependency-free exporter keeps the image small and the hot path light.
package metrics

import (
	"fmt"
	"io"
	"net/http"
	"sort"
	"sync"
	"sync/atomic"

	"github.com/indexexchange/rtd-ab/internal/sysstats"
)

// rpcServerDurationBuckets are the exact boundaries the documentation requires,
// in seconds. rpcServerDurationLE holds the matching le label strings so the
// rendered boundaries read exactly as the doc shows them (e.g. "0.010", which
// "%g" would collapse to "0.01").
var (
	rpcServerDurationBuckets = []float64{0.001, 0.002, 0.003, 0.004, 0.005, 0.007, 0.010}
	rpcServerDurationLE      = []string{"0.001", "0.002", "0.003", "0.004", "0.005", "0.007", "0.010"}
)

const rpcSystem = "grpc"

// series is one rpc_server_duration histogram, keyed by the varying labels.
type series struct {
	statusCode string
	method     string
	service    string
	errorType  string
	buckets    []uint64 // cumulative counts aligned to rpcServerDurationBuckets
	count      uint64
	sumSec     float64
}

// Registry holds the required histogram plus a small amount of container info.
type Registry struct {
	mu           sync.Mutex
	rpc          map[string]*series
	totalCount   uint64  // all RPCs, for the average-response-time gauge
	totalSumSec  float64 // sum of all handler durations in seconds
	appVersion   string
	modelVer     atomic.Value // string
	modelPresent int32
	ready        int32
	extra        []func(io.Writer) // additional exporters (e.g. the traffic profile)
}

// New creates a Registry.
func New(appVersion string) *Registry {
	r := &Registry{rpc: map[string]*series{}, appVersion: appVersion}
	r.modelVer.Store("none")
	return r
}

// SetModelVersion records the loaded model version for the info gauge.
func (r *Registry) SetModelVersion(v string) { r.modelVer.Store(v) }

// SetModel records whether a valid model was found at MODEL_PATH and its version.
func (r *Registry) SetModel(present bool, version string) {
	r.modelVer.Store(version)
	var v int32
	if present {
		v = 1
	}
	atomic.StoreInt32(&r.modelPresent, v)
}

// SetReady toggles the rtd_simple_ready gauge (0/1).
func (r *Registry) SetReady(ready bool) {
	var v int32
	if ready {
		v = 1
	}
	atomic.StoreInt32(&r.ready, v)
}

// ObserveRPC records one RPC: its handler duration in SECONDS, the gRPC status
// code (numeric string, e.g. "0" for OK), the service and method, and error_type
// ("" on success).
func (r *Registry) ObserveRPC(service, method, statusCode, errorType string, durationSec float64) {
	key := statusCode + "|" + service + "|" + method + "|" + errorType
	r.mu.Lock()
	s := r.rpc[key]
	if s == nil {
		s = &series{statusCode: statusCode, method: method, service: service, errorType: errorType, buckets: make([]uint64, len(rpcServerDurationBuckets))}
		r.rpc[key] = s
	}
	s.count++
	s.sumSec += durationSec
	r.totalCount++
	r.totalSumSec += durationSec
	for i, b := range rpcServerDurationBuckets {
		if durationSec <= b {
			s.buckets[i]++
		}
	}
	r.mu.Unlock()
}

// AddExporter registers an extra metrics writer appended to every scrape. Call
// before serving; the list is not guarded for concurrent registration.
func (r *Registry) AddExporter(fn func(io.Writer)) { r.extra = append(r.extra, fn) }

// Handler returns the /metrics HTTP handler.
func (r *Registry) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		r.write(w)
	})
}

func (r *Registry) write(w io.Writer) {
	// Required histogram.
	fmt.Fprintln(w, "# HELP rpc_server_duration Measures the duration of inbound RPC.")
	fmt.Fprintln(w, "# TYPE rpc_server_duration histogram")

	r.mu.Lock()
	keys := make([]string, 0, len(r.rpc))
	for k := range r.rpc {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		s := r.rpc[k]
		lbl := fmt.Sprintf("rpc_system=%q,rpc_grpc_status_code=%q,rpc_method=%q,rpc_service=%q,error_type=%q",
			rpcSystem, s.statusCode, s.method, s.service, s.errorType)
		for i := range rpcServerDurationBuckets {
			fmt.Fprintf(w, "rpc_server_duration_bucket{%s,le=%q} %d\n", lbl, rpcServerDurationLE[i], s.buckets[i])
		}
		fmt.Fprintf(w, "rpc_server_duration_bucket{%s,le=\"+Inf\"} %d\n", lbl, s.count)
		fmt.Fprintf(w, "rpc_server_duration_sum{%s} %s\n", lbl, trimFloat(s.sumSec))
		fmt.Fprintf(w, "rpc_server_duration_count{%s} %d\n", lbl, s.count)
	}
	totalCount, totalSumSec := r.totalCount, r.totalSumSec
	r.mu.Unlock()

	// Average handler response time (convenience; percentiles come from the
	// rpc_server_duration histogram above).
	if totalCount > 0 {
		fmt.Fprintln(w, "# HELP rtd_simple_response_seconds_avg Average GetMutations handler duration in seconds.")
		fmt.Fprintln(w, "# TYPE rtd_simple_response_seconds_avg gauge")
		fmt.Fprintf(w, "rtd_simple_response_seconds_avg %s\n", trimFloat(totalSumSec/float64(totalCount)))
	}

	// Container resource usage, sampled fresh at scrape time from the cgroup files
	// (no background goroutine; read-only-fs and scratch safe). Emitted only when a
	// cgroup source is present.
	if cpu, ok := sysstats.CPUSeconds(); ok {
		fmt.Fprintln(w, "# HELP rtd_simple_cpu_seconds_total Cumulative container CPU time in seconds (from cgroup).")
		fmt.Fprintln(w, "# TYPE rtd_simple_cpu_seconds_total counter")
		fmt.Fprintf(w, "rtd_simple_cpu_seconds_total %s\n", trimFloat(cpu))
	}
	if cores := sysstats.CPULimitCores(); cores > 0 {
		fmt.Fprintln(w, "# HELP rtd_simple_cpu_limit_cores CPU allocation in cores (denominator for CPU %).")
		fmt.Fprintln(w, "# TYPE rtd_simple_cpu_limit_cores gauge")
		fmt.Fprintf(w, "rtd_simple_cpu_limit_cores %s\n", trimFloat(cores))
	}
	if mem, ok := sysstats.MemoryBytes(); ok {
		fmt.Fprintln(w, "# HELP rtd_simple_memory_bytes Current container memory usage in bytes (from cgroup).")
		fmt.Fprintln(w, "# TYPE rtd_simple_memory_bytes gauge")
		fmt.Fprintf(w, "rtd_simple_memory_bytes %.0f\n", mem)
		if limit := sysstats.MemoryLimitBytes(); limit > 0 {
			fmt.Fprintln(w, "# HELP rtd_simple_memory_limit_bytes Memory allocation in bytes (denominator for memory %).")
			fmt.Fprintln(w, "# TYPE rtd_simple_memory_limit_bytes gauge")
			fmt.Fprintf(w, "rtd_simple_memory_limit_bytes %.0f\n", limit)
			fmt.Fprintln(w, "# HELP rtd_simple_memory_percent Container memory usage as a percent of its limit (0-100).")
			fmt.Fprintln(w, "# TYPE rtd_simple_memory_percent gauge")
			fmt.Fprintf(w, "rtd_simple_memory_percent %s\n", trimFloat(mem/limit*100))
		}
	}

	// Supplementary container info (permitted; low cardinality).
	fmt.Fprintln(w, "# HELP rtd_simple_build_info Build/model version info.")
	fmt.Fprintln(w, "# TYPE rtd_simple_build_info gauge")
	fmt.Fprintf(w, "rtd_simple_build_info{app_version=%q,model_version=%q} 1\n", r.appVersion, r.modelVer.Load().(string))
	fmt.Fprintln(w, "# HELP rtd_simple_model_present Whether a valid model was found at MODEL_PATH (1) or not (0).")
	fmt.Fprintln(w, "# TYPE rtd_simple_model_present gauge")
	fmt.Fprintf(w, "rtd_simple_model_present %d\n", atomic.LoadInt32(&r.modelPresent))
	fmt.Fprintln(w, "# HELP rtd_simple_ready Whether the container is ready to serve traffic (1) or not (0).")
	fmt.Fprintln(w, "# TYPE rtd_simple_ready gauge")
	fmt.Fprintf(w, "rtd_simple_ready %d\n", atomic.LoadInt32(&r.ready))

	for _, fn := range r.extra {
		fn(w)
	}
}

func trimFloat(f float64) string { return fmt.Sprintf("%g", f) }
