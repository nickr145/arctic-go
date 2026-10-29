// Package arena is the chunked slab allocator behind the arena variant of
// Arctic-Go.
//
// Nodes and value cells are not Go pointers. A reference is an integer handle:
// the edge's type tag selects a size class, and the handle selects a slot
// within that class. Handle 0 is reserved for null, so the first slot of every
// class is never handed out.
//
// Each class owns a fixed-capacity table of chunks. A chunk is installed with a
// single CAS and never moves or shrinks, so a handle stays valid and readers
// need no lock. Chunks hold only integers ([]uint64), so the garbage collector
// never scans them and GC pauses stay flat as the tree grows.
//
// Reclamation policy lives elsewhere. Free here is the primitive: in M1 the
// tree calls it directly (single goroutine), and M4 replaces direct frees with
// session-local free lists, hazard keys and EBR.
package arena

import (
	"fmt"
	"sync"
	"sync/atomic"
	"unsafe"

	"github.com/nickr145/arctic-go/internal/edge"
)

// Class is a size class. Every slot in a class has the same size.
type Class uint8

// The size classes. The node classes are laid out so that ClassNode7+Class(t)
// is the class for an edge of node type t.
const (
	ClassValue   Class = iota // one word: a user value
	ClassNode7                // 64 B, one cache line
	ClassNode13               // 128 B
	ClassNode31               // 512 B
	ClassNode256              // 2 KiB
	NumClasses
)

var (
	classWords = [NumClasses]int{1, 8, 16, 64, 256}
	classNames = [NumClasses]string{"value", "node7", "node13", "node31", "node256"}
)

// Words returns the slot size of the class in 64-bit words.
func (c Class) Words() int { return classWords[c] }

func (c Class) String() string {
	if c >= NumClasses {
		return fmt.Sprintf("class(%d)", uint8(c))
	}
	return classNames[c]
}

// ClassForType returns the size class of a child node of type t.
func ClassForType(t edge.Type) Class { return ClassNode7 + Class(t) }

// Options configure an Arena. The zero value selects the defaults.
type Options struct {
	// ChunkBytes is the size of one chunk: a power of two, at least 2048 (the
	// largest slot). Default 1 MiB.
	ChunkBytes int
	// MaxChunks is the capacity of each class's chunk table. Default 1<<14,
	// which is 16 GiB per class at the default chunk size.
	MaxChunks int
}

// Defaults.
const (
	DefaultChunkBytes = 1 << 20
	DefaultMaxChunks  = 1 << 14
	minChunkBytes     = 2048
)

// Arena allocates fixed-size slots from per-class chunk tables. Alloc, Free,
// Words and the value helpers are safe for concurrent use.
type Arena struct {
	pools [NumClasses]pool
}

type chunk struct{ words []uint64 }

type pool struct {
	class     Class
	slotWords int
	shift     uint   // log2 of slots per chunk
	mask      uint64 // slots per chunk - 1
	limit     uint64 // highest usable slot index

	table  []atomic.Pointer[chunk]
	next   atomic.Uint64 // slots handed out by the bump allocator
	chunks atomic.Int64

	mu    sync.Mutex
	free  []edge.Handle
	freed map[edge.Handle]struct{} // debug builds only
}

// New returns an empty Arena.
func New(opts Options) *Arena {
	if opts.ChunkBytes == 0 {
		opts.ChunkBytes = DefaultChunkBytes
	}
	if opts.MaxChunks == 0 {
		opts.MaxChunks = DefaultMaxChunks
	}
	if opts.ChunkBytes < minChunkBytes || opts.ChunkBytes&(opts.ChunkBytes-1) != 0 {
		panic("arena: ChunkBytes must be a power of two of at least 2048")
	}
	a := &Arena{}
	for c := Class(0); c < NumClasses; c++ {
		slotWords := c.Words()
		perChunk := uint64(opts.ChunkBytes/8) / uint64(slotWords)
		shift := uint(0)
		for 1<<shift < perChunk {
			shift++
		}
		p := &a.pools[c]
		*p = pool{
			class:     c,
			slotWords: slotWords,
			shift:     shift,
			mask:      perChunk - 1,
			limit:     min(uint64(edge.MaxHandle), uint64(opts.MaxChunks)<<shift-1),
			table:     make([]atomic.Pointer[chunk], opts.MaxChunks),
		}
		if Debug {
			p.freed = make(map[edge.Handle]struct{})
		}
	}
	return a
}

// Alloc returns a zeroed slot of class c. It panics if the class is exhausted.
func (a *Arena) Alloc(c Class) edge.Handle {
	p := &a.pools[c]
	if h, ok := p.pop(); ok {
		clear(p.slot(h)) // reused slots hold old (or poisoned) contents
		return h
	}
	n := p.next.Add(1)
	if n > p.limit {
		panic(fmt.Sprintf("arena: %v class exhausted (%d slots)", c, p.limit))
	}
	p.chunkFor(n >> p.shift) // fresh slots are already zero
	return edge.Handle(n)
}

// Free returns a slot to its class for reuse. The caller must guarantee that
// nothing can still read the slot.
func (a *Arena) Free(c Class, h edge.Handle) {
	p := &a.pools[c]
	if h == 0 || uint64(h) > p.next.Load() {
		panic(fmt.Sprintf("arena: free of invalid %v handle %d", c, h))
	}
	p.mu.Lock()
	if Debug {
		if _, dup := p.freed[h]; dup {
			p.mu.Unlock()
			panic(fmt.Sprintf("arena: double free of %v handle %d", c, h))
		}
		p.freed[h] = struct{}{}
		// Poison before the handle can be popped again, or a concurrent Alloc
		// would race with these writes.
		poison(p.slot(h))
	}
	p.free = append(p.free, h)
	p.mu.Unlock()
}

// Words returns the slot's words. The slice has the class's slot size and, for
// node classes, a 64-byte-aligned base. Words panics on the null handle, and in
// debug builds also on a freed slot, so a read of a reclaimed node fails loudly.
func (a *Arena) Words(c Class, h edge.Handle) []uint64 {
	if h == 0 {
		panic(fmt.Sprintf("arena: null %v handle", c))
	}
	p := &a.pools[c]
	if Debug {
		p.mu.Lock()
		_, isFree := p.freed[h]
		p.mu.Unlock()
		if isFree {
			panic(fmt.Sprintf("arena: read of freed %v handle %d", c, h))
		}
	}
	return p.slot(h)
}

// PutValue stores v in a fresh value cell and returns its handle.
func (a *Arena) PutValue(v uint64) edge.Handle {
	h := a.Alloc(ClassValue)
	atomic.StoreUint64(&a.pools[ClassValue].slot(h)[0], v)
	return h
}

// GetValue reads the value cell h.
func (a *Arena) GetValue(h edge.Handle) uint64 {
	return atomic.LoadUint64(&a.Words(ClassValue, h)[0])
}

// ClassStats describes one size class.
type ClassStats struct {
	Slots  uint64 // slots ever handed out by the bump allocator
	Free   uint64 // slots currently on the free list
	Chunks int    // chunks installed
}

// Live is the number of slots currently allocated.
func (s ClassStats) Live() uint64 { return s.Slots - s.Free }

// Stats is a snapshot of every class, indexed by Class.
type Stats [NumClasses]ClassStats

// Stats returns a snapshot. It is exact only when no other goroutine is
// allocating or freeing.
func (a *Arena) Stats() Stats {
	var s Stats
	for c := range a.pools {
		p := &a.pools[c]
		p.mu.Lock()
		s[c] = ClassStats{
			Slots:  min(p.next.Load(), p.limit),
			Free:   uint64(len(p.free)),
			Chunks: int(p.chunks.Load()),
		}
		p.mu.Unlock()
	}
	return s
}

func (p *pool) pop() (edge.Handle, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := len(p.free)
	if n == 0 {
		return 0, false
	}
	h := p.free[n-1]
	p.free = p.free[:n-1]
	if Debug {
		delete(p.freed, h)
	}
	return h, true
}

// slot returns the words of slot h without any liveness check.
func (p *pool) slot(h edge.Handle) []uint64 {
	ch := p.table[uint64(h)>>p.shift].Load()
	if ch == nil {
		panic(fmt.Sprintf("arena: %v handle %d was never allocated", p.class, h))
	}
	off := (uint64(h) & p.mask) * uint64(p.slotWords)
	end := off + uint64(p.slotWords)
	return ch.words[off:end:end]
}

// chunkFor returns chunk idx, installing it with one CAS if it is missing. A
// thread that loses the race discards its chunk and uses the winner's.
func (p *pool) chunkFor(idx uint64) *chunk {
	if ch := p.table[idx].Load(); ch != nil {
		return ch
	}
	nc := newChunk(int(p.mask+1) * p.slotWords)
	if p.table[idx].CompareAndSwap(nil, nc) {
		p.chunks.Add(1)
		return nc
	}
	return p.table[idx].Load()
}

// newChunk allocates a zeroed chunk whose first word is 64-byte aligned, so
// node offsets line up with cache lines.
func newChunk(words int) *chunk {
	buf := make([]uint64, words+8)
	off := 0
	if r := uintptr(unsafe.Pointer(&buf[0])) % 64; r != 0 {
		off = int((64 - r) / 8)
	}
	return &chunk{words: buf[off : off+words : off+words]}
}
