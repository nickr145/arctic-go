// Package conform is the shared correctness suite that every index must pass both Arctic-Go variants and the baselines
// It grows with the milestones
// M0 covers sequential model checking and simple concurrent properties
// M2 adds Porcupine linearizability histories and forced-path hooks
package conform

import (
	"encoding/binary"
	"math/rand/v2"
	"sort"
	"sync"
	"sync/atomic"
	"testing"

	arcticgo "github.com/nickr145/arctic-go"
)

// Factory returns a fresh, empty index
type Factory func() arcticgo.Index

func key8(i uint64) []byte {
	k := make([]byte, 8)
	binary.BigEndian.PutUint64(k, i)
	return k
}

// RunMap checks the point-operation semantics of an index
func RunMap(t *testing.T, newIndex Factory) {
	t.Run("Semantics", func(t *testing.T) { semantics(t, newIndex) })
	t.Run("ModelBased", func(t *testing.T) { modelBased(t, newIndex) })
	t.Run("ConcurrentDisjoint", func(t *testing.T) { concurrentDisjoint(t, newIndex) })
	t.Run("ConcurrentContendedInsert", func(t *testing.T) { contendedInsert(t, newIndex) })
}

func semantics(t *testing.T, newIndex Factory) {
	m := newIndex().NewSession()
	k := key8(42)

	if _, ok := m.Get(k); ok {
		t.Fatal("Get on empty index found a key")
	}
	if m.Update(k, 1) {
		t.Fatal("Update of absent key succeeded")
	}
	if m.Delete(k) {
		t.Fatal("Delete of absent key succeeded")
	}
	if !m.Insert(k, 7) {
		t.Fatal("Insert of new key failed")
	}
	if m.Insert(k, 8) {
		t.Fatal("Insert of existing key succeeded")
	}
	if v, ok := m.Get(k); !ok || v != 7 {
		t.Fatalf("Get = %d, %v; want 7, true (failed Insert must not overwrite)", v, ok)
	}
	if !m.Update(k, 9) {
		t.Fatal("Update of existing key failed")
	}
	if v, _ := m.Get(k); v != 9 {
		t.Fatalf("Get after Update = %d, want 9", v)
	}

	// The index must not retain the caller's key buffer.
	k[7] ^= 0xFF
	if _, ok := m.Get(key8(42)); !ok {
		t.Fatal("index aliased the caller's key slice")
	}
	if !m.Delete(key8(42)) {
		t.Fatal("Delete of existing key failed")
	}
	if _, ok := m.Get(key8(42)); ok {
		t.Fatal("Get found a deleted key")
	}
	if !m.Insert(key8(42), 1) {
		t.Fatal("re-Insert after Delete failed")
	}
}

func modelBased(t *testing.T, newIndex Factory) {
	r := rand.New(rand.NewPCG(11, 12))
	m := newIndex().NewSession()
	model := map[uint64]uint64{}
	const keySpace = 2000

	for i := 0; i < 300_000; i++ {
		id := r.Uint64N(keySpace)
		k := key8(id * 0x9E3779B97F4A7C15) // spread keys across the byte range
		val := r.Uint64()
		old, present := model[id]
		switch r.IntN(4) {
		case 0:
			if got := m.Insert(k, val); got != !present {
				t.Fatalf("op %d Insert(%d) = %v, model present = %v", i, id, got, present)
			}
			if !present {
				model[id] = val
			}
		case 1:
			if got := m.Update(k, val); got != present {
				t.Fatalf("op %d Update(%d) = %v, model present = %v", i, id, got, present)
			}
			if present {
				model[id] = val
			}
		case 2:
			if got := m.Delete(k); got != present {
				t.Fatalf("op %d Delete(%d) = %v, model present = %v", i, id, got, present)
			}
			delete(model, id)
		case 3:
			got, ok := m.Get(k)
			if ok != present || (ok && got != old) {
				t.Fatalf("op %d Get(%d) = %d, %v; model %d, %v", i, id, got, ok, old, present)
			}
		}
	}
	for id, want := range model {
		if got, ok := m.Get(key8(id * 0x9E3779B97F4A7C15)); !ok || got != want {
			t.Fatalf("final Get(%d) = %d, %v; want %d", id, got, ok, want)
		}
	}
}

// concurrentDisjoint has each worker own a key range
// The expected final state is exact even though workers interleave freely
func concurrentDisjoint(t *testing.T, newIndex Factory) {
	const workers, perWorker = 8, 5000
	idx := newIndex()
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			m := idx.NewSession()
			base := uint64(w) * perWorker
			for i := uint64(0); i < perWorker; i++ {
				if !m.Insert(key8(base+i), i) {
					t.Errorf("worker %d: Insert(%d) failed", w, base+i)
					return
				}
			}
			for i := uint64(0); i < perWorker; i++ {
				if !m.Update(key8(base+i), i+1) {
					t.Errorf("worker %d: Update(%d) failed", w, base+i)
					return
				}
			}
			for i := uint64(0); i < perWorker; i += 2 {
				if !m.Delete(key8(base + i)) {
					t.Errorf("worker %d: Delete(%d) failed", w, base+i)
					return
				}
			}
		}()
	}
	wg.Wait()
	if t.Failed() {
		return
	}
	m := idx.NewSession()
	for w := uint64(0); w < workers; w++ {
		for i := uint64(0); i < perWorker; i++ {
			v, ok := m.Get(key8(w*perWorker + i))
			if want := i%2 == 1; ok != want || (ok && v != i+1) {
				t.Fatalf("key %d: Get = %d, %v; want present=%v value %d", w*perWorker+i, v, ok, want, i+1)
			}
		}
	}
}

// contendedInsert races every worker over the same keys
// Linearizability demands that exactly one Insert per key succeeds
func contendedInsert(t *testing.T, newIndex Factory) {
	const workers, keys = 8, 4000
	idx := newIndex()
	var wins atomic.Int64
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			m := idx.NewSession()
			for i := uint64(0); i < keys; i++ {
				if m.Insert(key8(i), uint64(w)) {
					wins.Add(1)
				}
			}
		}()
	}
	wg.Wait()
	if got := wins.Load(); got != keys {
		t.Fatalf("%d Inserts succeeded for %d keys; want exactly one per key", got, keys)
	}
	m := idx.NewSession()
	for i := uint64(0); i < keys; i++ {
		if v, ok := m.Get(key8(i)); !ok || v >= workers {
			t.Fatalf("Get(%d) = %d, %v", i, v, ok)
		}
	}
}

// RunScan checks range and prefix scans against a sorted model
// It applies to ordered indexes only, and is sequential
// The concurrent scan properties (at-most-once, ordered, no lost keys) arrive with M3
func RunScan(t *testing.T, newIndex Factory) {
	r := rand.New(rand.NewPCG(21, 22))
	m := newIndex().NewSession()
	sc, ok := m.(arcticgo.Scanner)
	if !ok {
		t.Fatal("index does not implement Scanner")
	}

	present := map[uint64]uint64{}
	for len(present) < 3000 {
		id := r.Uint64()
		if m.Insert(key8(id), id>>3) {
			present[id] = id >> 3
		}
	}
	// A cluster of keys with a long common prefix exercises prefix scans.
	for i := uint64(0); i < 200; i++ {
		id := 0xAABBCCDD00000000 | i
		if m.Insert(key8(id), i) {
			present[id] = i
		}
	}
	for id := range present {
		if r.IntN(5) == 0 {
			m.Delete(key8(id))
			delete(present, id)
		}
	}
	sorted := make([]uint64, 0, len(present))
	for id := range present {
		sorted = append(sorted, id)
	}
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })

	collect := func(lo, hi []byte, limit int) []uint64 {
		var got []uint64
		sc.Scan(lo, hi, func(k []byte, v uint64) bool {
			id := binary.BigEndian.Uint64(k)
			if present[id] != v {
				t.Fatalf("Scan yielded key %#x with value %d, want %d", id, v, present[id])
			}
			got = append(got, id)
			return limit <= 0 || len(got) < limit
		})
		return got
	}
	expect := func(lo, hi uint64, loNil, hiNil bool) []uint64 {
		var want []uint64
		for _, id := range sorted {
			if (loNil || id >= lo) && (hiNil || id < hi) {
				want = append(want, id)
			}
		}
		return want
	}
	equal := func(name string, got, want []uint64) {
		t.Helper()
		if len(got) != len(want) {
			t.Fatalf("%s: got %d keys, want %d", name, len(got), len(want))
		}
		for i := range got {
			if got[i] != want[i] {
				t.Fatalf("%s: key %d = %#x, want %#x", name, i, got[i], want[i])
			}
		}
	}

	equal("full scan", collect(nil, nil, 0), expect(0, 0, true, true))

	for i := 0; i < 200; i++ {
		a, b := sorted[r.IntN(len(sorted))], sorted[r.IntN(len(sorted))]
		if a > b {
			a, b = b, a
		}
		equal("range", collect(key8(a), key8(b), 0), expect(a, b, false, false))
	}
	lo := sorted[len(sorted)/3]
	equal("open-ended hi", collect(key8(lo), nil, 0), expect(lo, 0, false, true))
	equal("open-ended lo", collect(nil, key8(lo), 0), expect(0, lo, true, false))

	// Prefix scan: [prefix, prefix+1) over the clustered keys.
	equal("prefix", collect(key8(0xAABBCCDD00000000), key8(0xAABBCCDE00000000), 0),
		expect(0xAABBCCDD00000000, 0xAABBCCDE00000000, false, false))

	// Early termination.
	got := collect(nil, nil, 10)
	equal("limit 10", got, expect(0, 0, true, true)[:10])

	// Empty range.
	if got := collect(key8(sorted[5]), key8(sorted[5]), 0); len(got) != 0 {
		t.Fatalf("empty range yielded %d keys", len(got))
	}
}
