// Package edge implements the 64-bit edge word used by the arena variant of Arctic-Go
//
// Arctic stores compressed prefix bytes and the child's node type in the parent edge, so that edge expansion is a single CAS
// The paper uses 16-byte edges and 128-bit CAS. Go only offers 64-bit atomics, so every edge here is one word:
//
//	bit 63      frozen   set during node replacement; a set bit makes the word immutable
//	bit 62      value    the child is a value cell, not a node
//	bits 60-61  type     child node type (meaningful only when value is clear)
//	bits 58-59  plen     compressed prefix length, 0..PrefixBytes
//	bits 32-57  prefix   up to PrefixBytes bytes, first byte most significant
//	bits 0-31   handle   arena slot of the child; 0 means null
//
// The bit split is derived from PrefixBytes and HandleBits
// The "3 prefix bytes with 32-bit handles, or 4 bytes with smaller handles" question is a two-constant change
// The constant assertions below fail to compile if a split does not fit in 64 bits
package edge

import (
	"encoding/binary"
	"math/bits"
	"sync/atomic"
)

const (
	// PrefixBytes is the max number of compressed prefix bytes per edge
	PrefixBytes = 3
	// HandleBits is the width of the child handle field
	HandleBits = 32
)

const (
	frozenShift = 63
	valueShift  = 62
	typeShift   = 60
	typeBits    = 2
	plenBits    = 2 // must be able to hold 0...PrefixBytes
	plenShift   = typeShift - plenBits
	prefixShift = HandleBits
	prefixBits  = 8 * PrefixBytes

	frozenBit  uint64 = 1 << frozenShift
	valueBit   uint64 = 1 << valueShift
	typeMask   uint64 = (1<<typeBits - 1) << typeShift
	plenMask   uint64 = (1<<plenBits - 1) << plenShift
	prefixMask uint64 = (1<<prefixBits - 1) << prefixShift
	handleMask uint64 = 1<<HandleBits - 1
)

// Compile-time layout checks
// Negative constant cannot convert to uint so each line is a build error when its cond is violated
const (
	_ = uint(plenShift - prefixShift - prefixBits) // prefix field fits below plen
	_ = uint(1<<plenBits - 1 - PrefixBytes)        // plen field can hold PrefixBytes
	_ = uint(32 - HandleBits)                      // Handle (uint32) can hold a full handle
	_ = uint(8 - PrefixBytes)                      // key loads below use at most 8 bytes
)

// Handle is an arena slot index. Handle 0 is reserved for null
type Handle uint32

// MaxHandle is the largest handle that fits in the handle field
const MaxHandle Handle = 1<<HandleBits - 1

// Type is the node type of an edge's child
// Two-bit tag reserved for null
type Type uint8

// Child node types of the arena variant
const (
	TypeNode7 Type = iota
	TypeNode13
	TypeNode31
	TypeNode256
)

// Edge is one edge word. The zero Edge is a null edge with an empty prefix
//
// An Edge is a plain value: the methods below decode or derive new values and never touch shared memory
// Shared words are accessed through Load, CAS and Freeze
type Edge uint64

// Null returns an unfrozen edge with the given prefix and no child
func Null(prefix []byte) Edge {
	return Edge(packPrefix(prefix))
}

// Node returns an unfrozen edge to a child node of type t
// It panics if h is 0 or does not fit in HandleBits
func Node(prefix []byte, t Type, h Handle) Edge {
	checkHandle(h)
	return Edge(packPrefix(prefix) | uint64(t)<<typeShift | uint64(h))
}

// Value returns an unfrozen edge to a value cell
// It panics if h is 0 or does not fit in HandleBits
func Value(prefix []byte, h Handle) Edge {
	checkHandle(h)
	return Edge(packPrefix(prefix) | valueBit | uint64(h))
}

func checkHandle(h Handle) {
	if h == 0 || h > MaxHandle {
		panic("edge: child handle in zero or out of range")
	}
}

// packPrefix returns the plen and prefix fields for p, in place
func packPrefix(p []byte) uint64 {
	if len(p) > PrefixBytes {
		panic("edge: prefix longer than PrefixBytes")
	}

	var field uint64
	for i := 0; i < PrefixBytes; i++ {
		field <<= 8
		if i < len(p) {
			field |= uint64(p[i])
		}
	}

	return uint64(len(p))<<plenShift | field<<prefixShift
}

// Frozen reports whether the frozen bit is set. A frozen edge is immutable
func (e Edge) Frozen() bool { return uint64(e)&frozenBit != 0 }

// IsValue reports whether the child is a value cell
func (e Edge) IsValue() bool { return uint64(e)&valueBit != 0 }

// Handle returns the child's arena slot, or 0 if the child is null
func (e Edge) Handle() Handle { return Handle(uint64(e) & handleMask) }

// IsNull reports whether the edge has no child
func (e Edge) IsNull() bool { return uint64(e)&handleMask == 0 }

// Type returns the child's node type. It is meaningless for value and null edges
func (e Edge) Type() Type { return Type(uint64(e) & typeMask >> typeShift) }

// PrefixLen returns the number of compressed prefix bytes, 0...PrefixBytes
func (e Edge) PrefixLen() int { return int(uint64(e) & plenMask >> plenShift) }

// Prefix returns the compressed prefix bytes. Only the first n bytes of p are meaningful
func (e Edge) Prefix() (p [PrefixBytes]byte, n int) {
	field := uint64(e) & prefixMask >> prefixShift

	for i := range p {
		p[i] = byte(field >> (8 * (PrefixBytes - 1 - i)))
	}

	return p, e.PrefixLen()
}

// Freeze returns e with the frozen bit set
func (e Edge) Freeze() Edge { return Edge(uint64(e) | frozenBit) }

// WithPrefix returns e with its prefix replaced
// The child and flags are kept
// It panics if prefix is longer than PrefixBytes
func (e Edge) WithPrefix(prefix []byte) Edge {
	return Edge(uint64(e)&^(plenMask|prefixMask) | packPrefix(prefix))
}

// Cleared returns an unfrozen edge with e's prefix and no child
// It is the result a delete CASes in
func (e Edge) Cleared() Edge { return Edge(uint64(e) & (plenMask | prefixMask)) }

// Match returns how many leading bytes of the edge's prefix agree with the start of key, capped by the prefix length and by len(key)
// The edge matches key completely when Match(key) == PrefixLen()
//
// It is branch-free apart from the load of key: XOR the stored prefix with the key bytes, then count leading zero bits
func (e Edge) Match(key []byte) int {
	stored := uint64(e) & prefixMask >> prefixShift
	diff := stored ^ loadKey(key)
	m := bits.LeadingZeros64(diff<<(64-prefixBits)) >> 3
	return min(m, e.PrefixLen(), len(key))
}

// MatchU64 is Match for a native integer key
// The key's bytes are taken from most to least significant, starting depth bytes in (0 <= depth <= 8), so no byte swap or slice is needed
func (e Edge) MatchU64(key uint64, depth int) int {
	stored := uint64(e) & prefixMask >> prefixShift
	k := key << (8 * depth) >> (64 - prefixBits)
	diff := stored ^ k
	m := bits.LeadingZeros64(diff<<(64-prefixBits)) >> 3
	return min(m, e.PrefixLen(), 8-depth)
}

// loadKey returns the first PrefixBytes bytes of key, big-endian, zero padded on the right when key is shorter
func loadKey(key []byte) uint64 {
	if len(key) >= 8 {
		return binary.BigEndian.Uint64(key) >> (64 - prefixBits)
	}
	var k uint64
	for i := 0; i < PrefixBytes; i++ {
		k <<= 8
		if i < len(key) {
			k |= uint64(key[i])
		}
	}
	return k
}

// Load atomically reads the edge word at p
func Load(p *uint64) Edge { return Edge(atomic.LoadUint64(p)) }

// CAS atomically replaces old with new at p
// It returns false if the word no longer holds old
//
// Callers promise to CAS only unfrozen words and to install unfrozen words, so a frozen word never changes
// CAS panics if either argument is frozen, because that is a protocol bug rather than contention
func CAS(p *uint64, old, new Edge) bool {
	if (old|new)&Edge(frozenBit) != 0 {
		panic("edge: CAS with a frozen word")
	}
	return atomic.CompareAndSwapUint64(p, uint64(old), uint64(new))
}

// Freeze sets the frozen bit of the word at p and returns the frozen value
//
// Freezing cannot fail: it retries until the bit is set, whether by this call or by another thread freezing concurrently
// It is idempotent, and every caller gets back the same frozen contents
func Freeze(p *uint64) Edge {
	for {
		old := Load(p)
		if old.Frozen() {
			return old
		}
		if atomic.CompareAndSwapUint64(p, uint64(old), uint64(old.Freeze())) {
			return old.Freeze()
		}
	}
}
