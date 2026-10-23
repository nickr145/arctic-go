package stats

import (
	"math/rand/v2"
	"sort"
	"testing"
)

func TestBucketsAreMonotoneAndTight(t *testing.T) {
	prev := -1
	for v := uint64(0); v < 1<<20; v++ {
		b := bucketOf(v)
		if b < prev {
			t.Fatalf("bucketOf(%d) = %d went backwards from %d", v, b, prev)
		}
		prev = b
		low := bucketLow(b)
		if low > v {
			t.Fatalf("bucketLow(%d) = %d exceeds member %d", b, low, v)
		}
		if v >= 8 && float64(v-low)/float64(v) > 0.125 {
			t.Fatalf("value %d is %.1f%% above its bucket floor %d", v, 100*float64(v-low)/float64(v), low)
		}
	}
	if b := bucketOf(^uint64(0)); b >= numBuckets {
		t.Fatalf("max uint64 maps to bucket %d, out of range", b)
	}
}

func TestQuantileAgainstSort(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 1))
	var h Hist
	vals := make([]uint64, 100_000)
	for i := range vals {
		vals[i] = uint64(50 + r.ExpFloat64()*2000)
		h.Record(vals[i])
	}
	sort.Slice(vals, func(i, j int) bool { return vals[i] < vals[j] })
	for _, q := range []float64{0.5, 0.9, 0.99, 0.999} {
		exact := float64(vals[int(q*float64(len(vals)))-1])
		got := float64(h.Quantile(q))
		if got > exact || got < exact*0.85 {
			t.Errorf("q=%v: histogram %v vs exact %v", q, got, exact)
		}
	}
}

func TestMergeAndEmpty(t *testing.T) {
	var a, b, empty Hist
	if empty.Quantile(0.5) != 0 {
		t.Fatal("empty histogram should report 0")
	}
	for i := 0; i < 1000; i++ {
		a.Record(100)
		b.Record(10_000)
	}
	a.Merge(&b)
	if a.Count() != 2000 {
		t.Fatalf("Count = %d, want 2000", a.Count())
	}
	if lo, hi := a.Quantile(0.25), a.Quantile(0.75); lo > 100 || hi < 8000 {
		t.Fatalf("quartiles after merge: %d, %d", lo, hi)
	}
}
