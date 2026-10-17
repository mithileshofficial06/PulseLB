// Package hist is a lock-free latency histogram.
//
// Values are recorded in microseconds into log-linear buckets: every power of
// two is split into 16 sub-buckets, so any reported quantile is within ~3% of
// the true value. Recording is a handful of atomic adds, cheap enough for the
// hot path at tens of thousands of requests per second.
package hist

import (
	"math/bits"
	"sync/atomic"
	"time"
)

const (
	subBuckets = 16
	numBuckets = subBuckets + 40*subBuckets
)

// Histogram records durations. The zero value is ready to use.
type Histogram struct {
	counts [numBuckets]atomic.Uint64
	total  atomic.Uint64
	sum    atomic.Uint64 // microseconds
	max    atomic.Uint64 // microseconds
}

func bucketOf(us uint64) int {
	if us < subBuckets {
		return int(us)
	}
	shift := bits.Len64(us) - 5
	idx := subBuckets + shift*subBuckets + int(us>>uint(shift)) - subBuckets
	if idx >= numBuckets {
		return numBuckets - 1
	}
	return idx
}

// valueOf returns the midpoint of a bucket in microseconds.
func valueOf(idx int) uint64 {
	if idx < subBuckets {
		return uint64(idx)
	}
	shift := (idx - subBuckets) / subBuckets
	m := uint64((idx-subBuckets)%subBuckets + subBuckets)
	return m<<uint(shift) + (uint64(1)<<uint(shift))/2
}

// Record adds one observation.
func (h *Histogram) Record(d time.Duration) {
	us := uint64(0)
	if d > 0 {
		us = uint64(d / time.Microsecond)
	}
	h.counts[bucketOf(us)].Add(1)
	h.total.Add(1)
	h.sum.Add(us)
	for {
		m := h.max.Load()
		if us <= m || h.max.CompareAndSwap(m, us) {
			return
		}
	}
}

// Snapshot is a point-in-time copy of a histogram. Subtracting two snapshots
// gives the observations recorded between them.
type Snapshot struct {
	Counts [numBuckets]uint64
	Total  uint64
	Sum    uint64
	Max    uint64
}

// Snapshot copies the current counts.
func (h *Histogram) Snapshot() Snapshot {
	var s Snapshot
	for i := range h.counts {
		s.Counts[i] = h.counts[i].Load()
	}
	s.Total = h.total.Load()
	s.Sum = h.sum.Load()
	s.Max = h.max.Load()
	return s
}

// Sub returns the observations in s that are not in prev. Max is not
// windowed and stays the all-time max.
func (s Snapshot) Sub(prev Snapshot) Snapshot {
	var d Snapshot
	for i := range s.Counts {
		d.Counts[i] = s.Counts[i] - prev.Counts[i]
	}
	d.Total = s.Total - prev.Total
	d.Sum = s.Sum - prev.Sum
	d.Max = s.Max
	return d
}

// Quantile returns the q-th quantile (0..1), or 0 if empty.
func (s Snapshot) Quantile(q float64) time.Duration {
	if s.Total == 0 {
		return 0
	}
	rank := uint64(q * float64(s.Total))
	if rank >= s.Total {
		rank = s.Total - 1
	}
	var seen uint64
	for i, c := range s.Counts {
		seen += c
		if seen > rank {
			return time.Duration(valueOf(i)) * time.Microsecond
		}
	}
	return time.Duration(s.Max) * time.Microsecond
}

// Mean returns the average observation, or 0 if empty.
func (s Snapshot) Mean() time.Duration {
	if s.Total == 0 {
		return 0
	}
	return time.Duration(s.Sum/s.Total) * time.Microsecond
}
