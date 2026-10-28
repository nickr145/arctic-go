// Package zipf generates Zipfian-distributed key ranks for the YCSB workloads
//
// The standard library's Zipf requires an exponent above 1, but YCSB's default is 0.99
// Two samplers are combined:
// - Gray et al. generator that YCSB itself uses for exponents below 1
// - math/rand/v2's Zipf for exponents above 1
// Rank 0 is the hottest
//
// The Gray et al. generator is exact for ranks 0 and 1
// It is a smooth approx beyond that (rank 2 is drawn about 18% too often at 0.99)
// That is the generator YCSB uses, so it is kept
// The cumulative shares that matter for skew stay within a few % of the analytic values
package zipf

import (
	"fmt"
	"math"
	"math/rand/v2"

	"github.com/nickr145/arctic-go/internal/keys"
)

// Dist holds the precomputed constants for a Zipfian distribution over [0, n)
// It is immutable and may be shared; each worker draws from it through its own Gen
type Dist struct {
	n     uint64
	theta float64

	// Constants of the Gray et al. generator (theta < 1)
	zetan, alpha, eta, halfPowTheta float64
}

// exactZeta is how many terms of the zeta sum are added exactly before the tail is approximated by an integral
// It keeps setup for 100 million keys to a few milliseconds instead of seconds
const exactZeta = 1_000_000

// New returns the distribution over [0, n) with the given skew
// theta must be positive and not 1
func New(n uint64, theta float64) (*Dist, error) {
	if n < 2 {
		return nil, fmt.Errorf("zipf: need n >= 2, got %d", n)
	}

	if theta <= 0 || theta == 1 {
		return nil, fmt.Errorf("zipf: theta must be positive and not 1, got %v", theta)
	}

	d := &Dist{n: n, theta: theta}

	if theta < 1 {
		d.zetan = Zeta(n, theta)
		zeta2 := Zeta(2, theta)
		d.alpha = 1 / (1 - theta)
		d.eta = (1 - math.Pow(2/float64(n), 1-theta)) / (1 - zeta2/d.zetan)
		d.halfPowTheta = math.Pow(0.5, theta)

	}

	return d, nil
}

// Zeta returns the generalized harmonic number sum_{i=1..n} i^{-theta}
// Terms beyond the first exactZeta are approximated by the midpoint-rule integral, whose error is far below measurement noise
func Zeta(n uint64, theta float64) float64 {
	head := min(n, exactZeta)
	var sum float64
	for i := uint64(1); i <= head; i++ {
		sum += math.Pow(float64(i), -theta)
	}
	if n > head {
		a, b := float64(head)+0.5, float64(n)+0.5
		sum += (math.Pow(b, 1-theta) - math.Pow(a, 1-theta)) / (1 - theta)
	}
	return sum
}

// N returns the size of the rank range
func (d *Dist) N() uint64 { return d.n }

// Gen draws ranks from a Dist
// It is not safe for concurrent use
type Gen struct {
	d  *Dist
	r  *rand.Rand
	sz *rand.Zipf // theta > 1 only
}

// NewGen returns a generator drawing from d using r
func (d *Dist) NewGen(r *rand.Rand) *Gen {
	g := &Gen{d: d, r: r}
	if d.theta > 1 {
		// rand.Zipf draws k in [0, imax] with P(k) proportional to (v + k)^{-s}
		g.sz = rand.NewZipf(r, d.theta, 1, d.n-1)
	}
	return g
}

// Next returns a rank in [0, n)
// Rank 0 is drawn most often
func (g *Gen) Next() uint64 {
	d := g.d
	if g.sz != nil {
		return g.sz.Uint64()
	}
	u := g.r.Float64()
	uz := u * d.zetan
	if uz < 1 {
		return 0
	}
	if uz < 1+d.halfPowTheta {
		return 1
	}
	v := uint64(float64(d.n) * math.Pow(d.eta*u-d.eta+1, d.alpha))
	return min(v, d.n-1)
}

// Scramble spreads a rank over [0, n) with a hash, as YCSB's scrambled Zipfian does, so hot keys are not clustered at the low end of the key space
// Different ranks can collide - that matches YCSB
func Scramble(rank, n uint64) uint64 { return keys.Mix(rank) % n }
