package edge

import (
	"encoding/binary"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"testing"
)

// TestLayoutMatchesDesignDoc pins the bit layout from section 3 of the design
// document. If you change PrefixBytes or HandleBits on purpose (the open
// "edge bit split" question), update the expected masks here in the same commit.
func TestLayoutMatchesDesignDoc(t *testing.T) {
	cases := []struct {
		name      string
		got, want uint64
	}{
		{"frozen", frozenBit, 1 << 63},
		{"value", valueBit, 1 << 62},
		{"type", typeMask, 0x3 << 60},
		{"plen", plenMask, 0x3 << 58},
		{"prefix", prefixMask, 0xFFFFFF << 32}, // 3 bytes; bits 56-57 spare
		{"handle", handleMask, 0xFFFFFFFF},
	}
	var union uint64
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("%s mask = %#x, want %#x", c.name, c.got, c.want)
		}
		if union&c.got != 0 {
			t.Errorf("%s overlaps an earlier field", c.name)
		}
		union |= c.got
	}
	if spare := ^union; spare != 0x3<<56 {
		t.Errorf("spare bits = %#x, want only bits 56-57", spare)
	}
}

func randPrefix(r *rand.Rand) []byte {
	p := make([]byte, r.IntN(PrefixBytes+1))
	for i := range p {
		p[i] = byte(r.Uint32())
	}
	return p
}

func randHandle(r *rand.Rand) Handle {
	return Handle(1 + r.Uint64N(uint64(MaxHandle)))
}

func TestRoundTrip(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 2))
	for i := 0; i < 200_000; i++ {
		prefix := randPrefix(r)
		h := randHandle(r)
		typ := Type(r.IntN(4))

		var e Edge
		switch i % 3 {
		case 0:
			e = Null(prefix)
			if !e.IsNull() || e.Handle() != 0 || e.IsValue() || e.Type() != 0 {
				t.Fatalf("Null(%x) = %#x is not canonical", prefix, uint64(e))
			}
		case 1:
			e = Value(prefix, h)
			if e.IsNull() || !e.IsValue() || e.Handle() != h {
				t.Fatalf("Value(%x, %d) decoded as %#x", prefix, h, uint64(e))
			}
		case 2:
			e = Node(prefix, typ, h)
			if e.IsNull() || e.IsValue() || e.Handle() != h || e.Type() != typ {
				t.Fatalf("Node(%x, %d, %d) decoded as %#x", prefix, typ, h, uint64(e))
			}
		}
		if e.Frozen() {
			t.Fatalf("constructor returned a frozen edge")
		}
		p, n := e.Prefix()
		if n != len(prefix) || e.PrefixLen() != len(prefix) {
			t.Fatalf("prefix length %d, want %d", n, len(prefix))
		}
		for j := 0; j < n; j++ {
			if p[j] != prefix[j] {
				t.Fatalf("prefix byte %d = %#x, want %#x", j, p[j], prefix[j])
			}
		}

		// Fields are independent: freezing and re-prefixing leave the rest alone.
		f := e.Freeze()
		if !f.Frozen() || f.Handle() != e.Handle() || f.IsValue() != e.IsValue() || f.PrefixLen() != e.PrefixLen() {
			t.Fatalf("Freeze disturbed other fields: %#x -> %#x", uint64(e), uint64(f))
		}
		np := randPrefix(r)
		w := e.WithPrefix(np)
		if w.Handle() != e.Handle() || w.IsValue() != e.IsValue() || w.Type() != e.Type() || w.PrefixLen() != len(np) {
			t.Fatalf("WithPrefix disturbed other fields: %#x -> %#x", uint64(e), uint64(w))
		}
		// Cleared keeps the prefix and drops the child, matching Null.
		if c := e.Cleared(); c != Null(prefix) {
			t.Fatalf("Cleared() = %#x, want %#x", uint64(c), uint64(Null(prefix)))
		}
	}
}

func TestConstructorPanics(t *testing.T) {
	mustPanic := func(name string, f func()) {
		t.Helper()
		defer func() {
			if recover() == nil {
				t.Errorf("%s did not panic", name)
			}
		}()
		f()
	}
	mustPanic("Null with long prefix", func() { Null(make([]byte, PrefixBytes+1)) })
	mustPanic("Node with handle 0", func() { Node(nil, TypeNode7, 0) })
	mustPanic("Value with handle 0", func() { Value(nil, 0) })
	mustPanic("CAS from frozen word", func() {
		var w uint64
		CAS(&w, Null(nil).Freeze(), Null(nil))
	})
	mustPanic("CAS to frozen word", func() {
		var w uint64
		CAS(&w, Null(nil), Null(nil).Freeze())
	})
}

// naiveMatch is the obviously-correct byte loop that Match must agree with.
func naiveMatch(e Edge, key []byte) int {
	p, n := e.Prefix()
	m := 0
	for m < n && m < len(key) && p[m] == key[m] {
		m++
	}
	return m
}

func TestMatchTable(t *testing.T) {
	e := Null([]byte{1, 2, 3})
	cases := []struct {
		key  []byte
		want int
	}{
		{nil, 0},
		{[]byte{}, 0},
		{[]byte{9}, 0},
		{[]byte{1}, 1},
		{[]byte{1, 2}, 2},
		{[]byte{1, 2, 9}, 2},
		{[]byte{1, 2, 3}, 3},
		{[]byte{1, 2, 3, 4, 5, 6, 7, 8, 9}, 3},
		{[]byte{1, 9, 3, 4, 5, 6, 7, 8, 9}, 1},
	}
	for _, c := range cases {
		if got := e.Match(c.key); got != c.want {
			t.Errorf("Match(%v) = %d, want %d", c.key, got, c.want)
		}
	}
	// Zero bytes in the prefix must not be confused with padding.
	z := Null([]byte{0, 0})
	if got := z.Match([]byte{0}); got != 1 {
		t.Errorf("zero prefix vs short key: Match = %d, want 1", got)
	}
	if got := z.Match([]byte{0, 0, 7}); got != 2 {
		t.Errorf("zero prefix vs longer key: Match = %d, want 2", got)
	}
	// An empty prefix matches nothing and everything: zero bytes.
	if got := Null(nil).Match([]byte{1, 2, 3}); got != 0 {
		t.Errorf("empty prefix: Match = %d, want 0", got)
	}
}

// mutate returns a key that shares a random-length prefix with p, then diverges.
func mutatedKey(r *rand.Rand, p []byte) []byte {
	key := append([]byte(nil), p...)
	if len(key) > 0 && r.IntN(3) > 0 {
		key[r.IntN(len(key))] ^= byte(1 + r.IntN(255))
	}
	if r.IntN(4) == 0 && len(key) > 0 {
		key = key[:r.IntN(len(key)+1)]
	}
	for extra := r.IntN(10); extra > 0; extra-- {
		key = append(key, byte(r.Uint32()))
	}
	return key
}

func TestMatchAgainstNaive(t *testing.T) {
	r := rand.New(rand.NewPCG(3, 4))
	for i := 0; i < 500_000; i++ {
		prefix := randPrefix(r)
		e := Null(prefix)
		key := mutatedKey(r, prefix)
		if got, want := e.Match(key), naiveMatch(e, key); got != want {
			t.Fatalf("Match(prefix=%x, key=%x) = %d, want %d", prefix, key, got, want)
		}
	}
}

func FuzzMatch(f *testing.F) {
	f.Add([]byte{1, 2, 3}, []byte{1, 2, 3, 4})
	f.Add([]byte{}, []byte{})
	f.Add([]byte{0, 0, 0}, []byte{0})
	f.Fuzz(func(t *testing.T, prefix, key []byte) {
		if len(prefix) > PrefixBytes {
			prefix = prefix[:PrefixBytes]
		}
		e := Null(prefix)
		if got, want := e.Match(key), naiveMatch(e, key); got != want {
			t.Fatalf("Match(prefix=%x, key=%x) = %d, want %d", prefix, key, got, want)
		}
	})
}

func TestMatchU64AgainstBytes(t *testing.T) {
	r := rand.New(rand.NewPCG(5, 6))
	var buf [8]byte
	for i := 0; i < 500_000; i++ {
		key := r.Uint64()
		binary.BigEndian.PutUint64(buf[:], key)
		depth := r.IntN(9) // 0..8
		prefix := mutatedKey(r, buf[depth:])
		if len(prefix) > PrefixBytes {
			prefix = prefix[:PrefixBytes]
		}
		e := Null(prefix)
		if got, want := e.MatchU64(key, depth), e.Match(buf[depth:]); got != want {
			t.Fatalf("MatchU64(key=%#x, depth=%d, prefix=%x) = %d, Match on bytes = %d",
				key, depth, prefix, got, want)
		}
	}
}

func TestFreezeIdempotent(t *testing.T) {
	w := uint64(Value([]byte{7, 8}, 42))
	first := Freeze(&w)
	if !first.Frozen() || first.Handle() != 42 || !first.IsValue() {
		t.Fatalf("Freeze returned %#x", uint64(first))
	}
	if again := Freeze(&w); again != first {
		t.Fatalf("second Freeze = %#x, want %#x", uint64(again), uint64(first))
	}
	if Load(&w) != first {
		t.Fatalf("word changed after Freeze")
	}
	// A frozen word is immutable: CAS from its unfrozen form must fail.
	if CAS(&w, first&^Edge(frozenBit), Null(nil)) {
		t.Fatalf("CAS succeeded against a frozen word")
	}
}

// TestConcurrentFreeze races writers against several freezers. Writers bump the
// handle by one per successful CAS; once any freezer returns, no later CAS may
// succeed, so the frozen contents must account for exactly the successes.
func TestConcurrentFreeze(t *testing.T) {
	for round := 0; round < 200; round++ {
		w := uint64(Value(nil, 1))
		var successes atomic.Uint64
		var wg sync.WaitGroup

		for g := 0; g < 6; g++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for {
					old := Load(&w)
					if old.Frozen() {
						return
					}
					next := Value(nil, old.Handle()+1)
					if CAS(&w, old, next) {
						successes.Add(1)
					}
				}
			}()
		}

		results := make([]Edge, 3)
		var fw sync.WaitGroup
		for f := range results {
			fw.Add(1)
			go func() {
				defer fw.Done()
				for i := 0; i < f*50; i++ {
					_ = Load(&w) // stagger the freezers
				}
				results[f] = Freeze(&w)
			}()
		}
		fw.Wait()
		wg.Wait()

		final := Load(&w)
		if !final.Frozen() {
			t.Fatalf("round %d: word not frozen", round)
		}
		for i, r := range results {
			if r != final {
				t.Fatalf("round %d: freezer %d saw %#x, final %#x", round, i, uint64(r), uint64(final))
			}
		}
		if got, want := uint64(final.Handle()), 1+successes.Load(); got != want {
			t.Fatalf("round %d: frozen handle %d, want %d (a CAS succeeded after freezing)", round, got, want)
		}
	}
}

var sink int

func benchInputs() ([]Edge, [][]byte) {
	r := rand.New(rand.NewPCG(7, 8))
	edges := make([]Edge, 1024)
	keys := make([][]byte, 1024)
	for i := range edges {
		p := randPrefix(r)
		edges[i] = Null(p)
		keys[i] = append(mutatedKey(r, p), make([]byte, 8)...)
	}
	return edges, keys
}

func BenchmarkMatch(b *testing.B) {
	edges, keys := benchInputs()
	b.ResetTimer()
	s := 0
	for i := 0; i < b.N; i++ {
		s += edges[i&1023].Match(keys[i&1023])
	}
	sink = s
}

func BenchmarkMatchNaive(b *testing.B) {
	edges, keys := benchInputs()
	b.ResetTimer()
	s := 0
	for i := 0; i < b.N; i++ {
		s += naiveMatch(edges[i&1023], keys[i&1023])
	}
	sink = s
}

func BenchmarkMatchU64(b *testing.B) {
	edges, _ := benchInputs()
	r := rand.New(rand.NewPCG(9, 10))
	ks := make([]uint64, 1024)
	for i := range ks {
		ks[i] = r.Uint64()
	}
	b.ResetTimer()
	s := 0
	for i := 0; i < b.N; i++ {
		s += edges[i&1023].MatchU64(ks[i&1023], i&3)
	}
	sink = s
}
