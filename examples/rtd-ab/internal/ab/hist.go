// SPDX-License-Identifier: Apache-2.0

package ab

import (
	"fmt"
	"io"
	"math"
	"strconv"
	"sync/atomic"
)

// maxBuckets bounds the finite buckets of a histogram (the bucket array is fixed-size).
const maxBuckets = 12

// histSpec describes a histogram of fixed-point int64 values. A value v (an
// int64 holding real*Scale) lands in the first bucket whose edge is >= v
// (Prometheus "le" semantics); anything above the last edge lands in +Inf.
// Edges and Scale are fixed at construction, so recording is integer-only.
type histSpec struct {
	scale  float64  // fixed-point units per 1.0
	edges  []int64  // ascending, in fixed-point units
	labels []string // le label for each edge
}

func newHistSpec(scale float64, edgesReal ...float64) *histSpec {
	if len(edgesReal) > maxBuckets {
		panic("ab: too many buckets")
	}
	s := &histSpec{scale: scale}
	for _, e := range edgesReal {
		s.edges = append(s.edges, int64(math.Round(e*scale)))
		s.labels = append(s.labels, strconv.FormatFloat(e, 'g', -1, 64))
	}
	return s
}

func (s *histSpec) bucket(v int64) int {
	for i, e := range s.edges {
		if v <= e {
			return i
		}
	}
	return len(s.edges) // +Inf
}

// atomicHist is the shared accumulator. Everything is an atomic integer add:
// no lock and no float compare-and-swap.
type atomicHist struct {
	spec  *histSpec
	b     [maxBuckets + 1]atomic.Uint64
	count atomic.Uint64
	sum   atomic.Int64
	sumsq atomic.Int64
}

// observe records one value.
func (h *atomicHist) observe(v int64) {
	h.b[h.spec.bucket(v)].Add(1)
	h.count.Add(1)
	h.sum.Add(v)
	h.sumsq.Add(v * v)
}

func (h *atomicHist) total() uint64 { return h.count.Load() }

// mean returns sum/count in real units, or 0 with no samples.
func (h *atomicHist) mean() float64 {
	n := h.count.Load()
	if n == 0 {
		return 0
	}
	return float64(h.sum.Load()) / h.spec.scale / float64(n)
}

// writeSeries writes _bucket, _sum and _count for one label set. labels is "" or `a="b",c="d"` without braces. Nothing is
// written until the first observation (lazy series).
func (h *atomicHist) writeSeries(w io.Writer, name, labels string) {
	count := h.count.Load()
	if count == 0 {
		return
	}
	sep := ""
	if labels != "" {
		sep = ","
	}
	var cum uint64
	for i, le := range h.spec.labels {
		cum += h.b[i].Load()
		fmt.Fprintf(w, "%s_bucket{%s%sle=%q} %d\n", name, labels, sep, le, cum)
	}
	fmt.Fprintf(w, "%s_bucket{%s%sle=\"+Inf\"} %d\n", name, labels, sep, count)
	lbl := ""
	if labels != "" {
		lbl = "{" + labels + "}"
	}
	fmt.Fprintf(w, "%s_sum%s %s\n", name, lbl, fmtFloat(float64(h.sum.Load())/h.spec.scale))
	fmt.Fprintf(w, "%s_count%s %d\n", name, lbl, count)
}

func fmtFloat(f float64) string { return strconv.FormatFloat(f, 'g', -1, 64) }

func header(w io.Writer, name, typ, help string) {
	fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s %s\n", name, help, name, typ)
}

// writeSumsq writes a _sumsq counter family for control and test, skipping an
// arm until it has samples.
func writeSumsq(w io.Writer, name string, control, test *atomicHist) {
	for arm, h := range [2]*atomicHist{control, test} {
		if h.count.Load() != 0 {
			label := "control"
			if arm == 1 {
				label = "test"
			}
			fmt.Fprintf(w, "%s{arm=%q} %s\n", name, label, fmtFloat(float64(h.sumsq.Load())/(h.spec.scale*h.spec.scale)))
		}
	}
}
