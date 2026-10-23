// Package stats holds the small measurement primitives used by the harness
package stats

import "math/bits"

// Hist is a log-linear latency histogram over nanoseconds
// Values below 8 get exact buckets
// Every power-of-two range above that is split into 8 linear sub-buckets, bounding the relative error of a percentile at 12.5%
// It never allocates after construction and is not safe for concurrent use; give each worker its own and Merge them afterwards
type Hist struct {
	counts [numBuckets]uint64
	n      uint64
}

const numBuckets = 64 * 8

func bucketOf(v uint64) int {
	if v < 8 {
		return int(v)
	}
	l := bits.Len64(v)
	return (l-3)*8 + int(v>>(l-4)&7)
}

// bucketLow is the smallest value that falls in bucket b
func bucketLow(b int) uint64 {
	if b < 8 {
		return uint64(b)
	}
	e, top := b/8, uint64(b%8)
	return (8 | top) << (e - 1)
}

// Record adds one observation, in nanoseconds
func (h *Hist) Record(ns uint64) {
	h.counts[bucketOf(ns)]++
	h.n++
}

// Merge adds the observations of o into h
func (h *Hist) Merge(o *Hist) {
	for i, c := range o.counts {
		h.counts[i] += c
	}
	h.n += o.n
}

// Count returns the number of observations
func (h *Hist) Count() uint64 { return h.n }

// Quantile returns the lower bound of the bucket holding the q-quantile (0 <= q <= 1), or 0 if the histogram is empty
func (h *Hist) Quantile(q float64) uint64 {
	if h.n == 0 {
		return 0
	}
	target := uint64(q*float64(h.n) + 0.5)
	if target < 1 {
		target = 1
	}
	var seen uint64
	for b, c := range h.counts {
		seen += c
		if seen >= target {
			return bucketLow(b)
		}
	}
	return bucketLow(numBuckets - 1)
}
