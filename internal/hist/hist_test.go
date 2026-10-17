package hist

import (
	"math"
	"testing"
	"time"
)

func TestBucketRoundTrip(t *testing.T) {
	for _, us := range []uint64{0, 1, 15, 16, 17, 31, 32, 33, 100, 1000, 12345, 999_999, 60_000_000} {
		got := valueOf(bucketOf(us))
		if us == 0 {
			if got != 0 {
				t.Fatalf("0 -> %d", got)
			}
			continue
		}
		if err := math.Abs(float64(got)-float64(us)) / float64(us); err > 0.04 {
			t.Errorf("%dus -> bucket value %dus (%.1f%% off)", us, got, err*100)
		}
	}
}

func TestQuantiles(t *testing.T) {
	var h Histogram
	for i := 1; i <= 1000; i++ {
		h.Record(time.Duration(i) * time.Millisecond)
	}
	s := h.Snapshot()
	check := func(q float64, want time.Duration) {
		got := s.Quantile(q)
		if math.Abs(float64(got-want))/float64(want) > 0.04 {
			t.Errorf("q%.2f = %v, want ~%v", q, got, want)
		}
	}
	check(0.5, 500*time.Millisecond)
	check(0.99, 990*time.Millisecond)
	if s.Total != 1000 {
		t.Fatalf("total %d", s.Total)
	}
}

func TestWindow(t *testing.T) {
	var h Histogram
	for i := 0; i < 100; i++ {
		h.Record(time.Millisecond)
	}
	prev := h.Snapshot()
	for i := 0; i < 100; i++ {
		h.Record(100 * time.Millisecond)
	}
	w := h.Snapshot().Sub(prev)
	if w.Total != 100 {
		t.Fatalf("window total %d", w.Total)
	}
	if p := w.Quantile(0.5); p < 95*time.Millisecond {
		t.Fatalf("window p50 %v, want ~100ms", p)
	}
}
