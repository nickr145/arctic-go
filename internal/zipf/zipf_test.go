package zipf

import (
	"math"
	"math/rand/v2"
	"testing"
)

func TestZetaApproximationMatchesExactSum(t *testing.T) {
	const n = 4 * exactZeta
	for _, theta := range []float64{0.5, 0.99, 1.2} {
		var exact float64
		for i := uint64(1); i <= n; i++ {
			exact += math.Pow(float64(i), -theta)
		}
		if got := Zeta(n, theta); math.Abs(got-exact)/exact > 1e-9 {
			t.Errorf("theta %v: Zeta = %.12f, exact %.12f", theta, got, exact)
		}
	}
}

// TestRankFrequencies compares sampled frequencies with the analytic
// distribution P(rank k) = (k+1)^-theta / zeta(n, theta).
//
// For theta > 1 the standard library's sampler is exact, so individual ranks
// are checked. The Gray et al. generator used for theta < 1 (and by YCSB) is
// exact only for ranks 0 and 1 and smooths the rest, so for it the remaining
// ranks are checked through cumulative shares instead.
func TestRankFrequencies(t *testing.T) {
	const n, samples = 10_000, 3_000_000
	for _, theta := range []float64{0.99, 1.1, 1.2} {
		d, err := New(n, theta)
		if err != nil {
			t.Fatal(err)
		}
		g := d.NewGen(rand.New(rand.NewPCG(1, uint64(theta*100))))
		z := Zeta(n, theta)
		counts := make([]int, n)
		for i := 0; i < samples; i++ {
			k := g.Next()
			if k >= n {
				t.Fatalf("theta %v: rank %d out of range", theta, k)
			}
			counts[k]++
		}
		perRank := []int{0, 1}
		if theta > 1 {
			perRank = []int{0, 1, 2, 9, 99}
		}
		for _, k := range perRank {
			want := math.Pow(float64(k+1), -theta) / z
			got := float64(counts[k]) / samples
			if math.Abs(got-want)/want > 0.05 {
				t.Errorf("theta %v rank %d: frequency %.5f, want %.5f", theta, k, got, want)
			}
		}
		for _, top := range []int{10, 100, 1000} {
			var got, want float64
			for k := 0; k < top; k++ {
				got += float64(counts[k]) / samples
				want += math.Pow(float64(k+1), -theta) / z
			}
			if math.Abs(got-want)/want > 0.05 {
				t.Errorf("theta %v: top-%d share %.4f, want %.4f", theta, top, got, want)
			}
		}
	}
}

func TestNewRejectsBadParameters(t *testing.T) {
	for _, theta := range []float64{0, -1, 1} {
		if _, err := New(100, theta); err == nil {
			t.Errorf("theta %v accepted", theta)
		}
	}
	if _, err := New(1, 0.99); err == nil {
		t.Error("n=1 accepted")
	}
}

func TestScrambleStaysInRange(t *testing.T) {
	for r := uint64(0); r < 100_000; r++ {
		if Scramble(r, 1000) >= 1000 {
			t.Fatalf("Scramble(%d, 1000) out of range", r)
		}
	}
}
