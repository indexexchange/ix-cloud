// SPDX-License-Identifier: Apache-2.0

// Package runtimetune applies Go runtime settings derived from the container's
// cgroup limits at startup, to cut the allocation-driven GC overhead and the
// GOMAXPROCS oversubscription that profiling identified as the dominant cost of a
// gRPC RTD container:
//
//   - GOMAXPROCS is matched to the CPU limit (avoids oversubscribing a fractional
//     CPU allocation, which causes CFS throttling and wasted scheduler/GC work).
//   - GOMEMLIMIT is set from the memory limit as a soft ceiling.
//   - GOGC is raised from the default 100 to run GC far less often (the live heap
//     is small; the memory limit is the real ceiling).
//
// Explicit GOMAXPROCS / GOGC / GOMEMLIMIT environment variables always win, so an
// operator (or Index) can override any of these.
package runtimetune

import (
	"fmt"
	"math"
	"os"
	"runtime"
	"runtime/debug"
	"strings"

	"github.com/indexexchange/rtd-ab/internal/sysstats"
)

// DefaultGCPercent raises GOGC from the default 100. A no-op gRPC service keeps a
// small live heap but allocates heavily per request (protobuf unmarshal), so GC
// fires very often at GOGC=100 (~18% of CPU). These containers run with a large
// memory allocation and a tiny live heap, so we spend the spare memory to buy CPU:
// 600 cuts GC to ~3% of CPU with the heap still only tens of MB, bounded by
// GOMEMLIMIT as the hard ceiling. Raise further only if a profile shows GC still
// material and memory headroom remains.
const DefaultGCPercent = 600

// memLimitHeadroom leaves margin below the cgroup memory limit for non-heap memory.
const memLimitHeadroom = 0.90

// Apply sets GOMAXPROCS, GOGC, and GOMEMLIMIT from the cgroup limits unless the
// matching environment variable is already set (explicit config wins). It returns
// a one-line summary suitable for logging.
func Apply() string {
	var parts []string

	// GOMAXPROCS: match the CPU allocation (round up, minimum 1).
	if os.Getenv("GOMAXPROCS") != "" {
		parts = append(parts, fmt.Sprintf("GOMAXPROCS=%d(env)", runtime.GOMAXPROCS(0)))
	} else {
		n := int(math.Ceil(sysstats.CPULimitCores()))
		if n < 1 {
			n = 1
		}
		runtime.GOMAXPROCS(n)
		parts = append(parts, fmt.Sprintf("GOMAXPROCS=%d(cpu-limit)", n))
	}

	// GOGC: raise to cut GC frequency.
	if os.Getenv("GOGC") != "" {
		parts = append(parts, "GOGC=env")
	} else {
		debug.SetGCPercent(DefaultGCPercent)
		parts = append(parts, fmt.Sprintf("GOGC=%d", DefaultGCPercent))
	}

	// GOMEMLIMIT: soft ceiling from the memory allocation.
	switch {
	case os.Getenv("GOMEMLIMIT") != "":
		parts = append(parts, "GOMEMLIMIT=env")
	default:
		if lim := sysstats.MemoryLimitBytes(); lim > 0 {
			soft := int64(lim * memLimitHeadroom)
			debug.SetMemoryLimit(soft)
			parts = append(parts, fmt.Sprintf("GOMEMLIMIT=%dMiB", soft/(1<<20)))
		} else {
			parts = append(parts, "GOMEMLIMIT=unset")
		}
	}

	return strings.Join(parts, " ")
}
