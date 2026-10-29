package arena

import (
	"sync"
	"testing"
	"unsafe"

	"github.com/nickr145/arctic-go/internal/edge"
)

// small chunks make tests cross chunk boundaries after a handful of allocations.
var small = Options{ChunkBytes: 2048, MaxChunks: 1 << 10}

func TestClassForType(t *testing.T) {
	cases := []struct {
		t edge.Type
		c Class
	}{
		{edge.TypeNode7, ClassNode7},
		{edge.TypeNode13, ClassNode13},
		{edge.TypeNode31, ClassNode31},
		{edge.TypeNode256, ClassNode256},
	}
	for _, c := range cases {
		if got := ClassForType(c.t); got != c.c {
			t.Errorf("ClassForType(%d) = %v, want %v", c.t, got, c.c)
		}
	}
	// Sizes from the design document: 64 B, 128 B, 512 B, 2 KiB.
	for c, bytes := range map[Class]int{ClassNode7: 64, ClassNode13: 128, ClassNode31: 512, ClassNode256: 2048, ClassValue: 8} {
		if got := c.Words() * 8; got != bytes {
			t.Errorf("%v slot is %d bytes, want %d", c, got, bytes)
		}
	}
}

func TestHandlesStartAtOneAndSlotsDoNotOverlap(t *testing.T) {
	a := New(small)
	for c := Class(0); c < NumClasses; c++ {
		const n = 600 // crosses several chunks for every class
		handles := make([]edge.Handle, n)
		for i := range handles {
			h := a.Alloc(c)
			if i == 0 && h != 1 {
				t.Fatalf("%v: first handle = %d, want 1 (0 is null)", c, h)
			}
			w := a.Words(c, h)
			if len(w) != c.Words() || cap(w) != c.Words() {
				t.Fatalf("%v: slot has len %d cap %d, want %d", c, len(w), cap(w), c.Words())
			}
			for j := range w {
				if w[j] != 0 {
					t.Fatalf("%v: fresh slot %d is not zeroed", c, h)
				}
				w[j] = uint64(h)<<8 | uint64(j)
			}
			handles[i] = h
		}
		for _, h := range handles {
			for j, v := range a.Words(c, h) {
				if v != uint64(h)<<8|uint64(j) {
					t.Fatalf("%v: slot %d word %d = %#x: slots overlap", c, h, j, v)
				}
			}
		}
	}
}

func TestNodeSlotsAreCacheLineAligned(t *testing.T) {
	a := New(small)
	for _, c := range []Class{ClassNode7, ClassNode13, ClassNode31, ClassNode256} {
		for i := 0; i < 50; i++ {
			h := a.Alloc(c)
			if addr := uintptr(unsafe.Pointer(&a.Words(c, h)[0])); addr%64 != 0 {
				t.Fatalf("%v handle %d at %#x is not 64-byte aligned", c, h, addr)
			}
		}
	}
}

func TestFreeReusesAndZeroes(t *testing.T) {
	a := New(small)
	h1 := a.Alloc(ClassNode7)
	for i := range a.Words(ClassNode7, h1) {
		a.Words(ClassNode7, h1)[i] = ^uint64(0)
	}
	a.Free(ClassNode7, h1)
	h2 := a.Alloc(ClassNode7)
	if h2 != h1 {
		t.Fatalf("freed handle %d not reused, got %d", h1, h2)
	}
	for i, v := range a.Words(ClassNode7, h2) {
		if v != 0 {
			t.Fatalf("reused slot word %d = %#x, want 0", i, v)
		}
	}
	if s := a.Stats()[ClassNode7]; s.Live() != 1 || s.Free != 0 || s.Slots != 1 {
		t.Fatalf("stats after reuse = %+v", s)
	}
}

func TestValueCells(t *testing.T) {
	a := New(small)
	hs := make([]edge.Handle, 5000)
	for i := range hs {
		hs[i] = a.PutValue(uint64(i) * 7)
	}
	for i, h := range hs {
		if got := a.GetValue(h); got != uint64(i)*7 {
			t.Fatalf("cell %d = %d, want %d", h, got, i*7)
		}
	}
	if s := a.Stats()[ClassValue]; s.Live() != 5000 || s.Chunks < 2 {
		t.Fatalf("value stats = %+v, want 5000 live over several chunks", s)
	}
}

func TestStatsTrackLiveSlots(t *testing.T) {
	a := New(small)
	var hs []edge.Handle
	for i := 0; i < 100; i++ {
		hs = append(hs, a.Alloc(ClassNode13))
	}
	for _, h := range hs[:40] {
		a.Free(ClassNode13, h)
	}
	s := a.Stats()[ClassNode13]
	if s.Slots != 100 || s.Free != 40 || s.Live() != 60 {
		t.Fatalf("stats = %+v, want 100 slots, 40 free, 60 live", s)
	}
}

func TestExhaustionPanics(t *testing.T) {
	a := New(Options{ChunkBytes: 2048, MaxChunks: 2}) // 256 value slots per chunk, slot 0 reserved
	const usable = 2*256 - 1
	for i := 0; i < usable; i++ {
		a.Alloc(ClassValue)
	}
	defer func() {
		if recover() == nil {
			t.Fatal("allocating past the class limit did not panic")
		}
	}()
	a.Alloc(ClassValue)
}

func TestInvalidUsePanics(t *testing.T) {
	a := New(small)
	mustPanic := func(name string, f func()) {
		t.Helper()
		defer func() {
			if recover() == nil {
				t.Errorf("%s did not panic", name)
			}
		}()
		f()
	}
	mustPanic("Words(null)", func() { a.Words(ClassNode7, 0) })
	mustPanic("Free(null)", func() { a.Free(ClassNode7, 0) })
	mustPanic("Free(never allocated)", func() { a.Free(ClassNode7, 99) })
	mustPanic("bad ChunkBytes", func() { New(Options{ChunkBytes: 3000}) })
	mustPanic("tiny ChunkBytes", func() { New(Options{ChunkBytes: 1024}) })
}

// TestConcurrentAllocIsUnique allocates from many goroutines at once. Chunks are
// installed by CAS, so this races goroutines to install the same chunk; each
// handle must still be unique and every written value must survive.
func TestConcurrentAllocIsUnique(t *testing.T) {
	a := New(small)
	const workers, per = 8, 4000
	handles := make([][]edge.Handle, workers)
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < per; i++ {
				handles[w] = append(handles[w], a.PutValue(uint64(w)<<32|uint64(i)))
			}
		}()
	}
	wg.Wait()

	seen := make(map[edge.Handle]bool, workers*per)
	for w, hs := range handles {
		for i, h := range hs {
			if seen[h] {
				t.Fatalf("handle %d handed out twice", h)
			}
			seen[h] = true
			if got := a.GetValue(h); got != uint64(w)<<32|uint64(i) {
				t.Fatalf("handle %d holds %#x, want %#x", h, got, uint64(w)<<32|uint64(i))
			}
		}
	}
}

func TestConcurrentAllocAndFree(t *testing.T) {
	a := New(small)
	var wg sync.WaitGroup
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 3000; i++ {
				h := a.Alloc(ClassNode7)
				words := a.Words(ClassNode7, h)
				words[0] = uint64(h)
				if words[0] != uint64(h) {
					t.Errorf("slot %d was handed to two goroutines", h)
					return
				}
				a.Free(ClassNode7, h)
			}
		}()
	}
	wg.Wait()
	if s := a.Stats()[ClassNode7]; s.Live() != 0 {
		t.Fatalf("%d slots still live after every alloc was freed", s.Live())
	}
}
