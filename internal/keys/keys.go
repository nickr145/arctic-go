// Package keys generates the benchmark key sets as pure functions of the key
// index, so a 100-million-key run needs no 100-million-entry key array
//
// Every generator is a bijection on the index (or injective on it), so keys
// are unique, and keys of one generator all have the same length, so no key is
// a prefix of another
package keys

import (
	"encoding/binary"
	"fmt"
)

// Kind names a key set
type Kind string

// Key sets implemented so far
// Email-like and URL-like string sets arrive with the string-key work in M1
const (
	SeqU64  Kind = "seq-u64"  // 8-byte big-endian 0,1,2,...
	RandU64 Kind = "rand-u64" // 8-byte pseudo-random integers
	UUID16  Kind = "uuid16"   // 16 pseudo-random bytes (not RFC 4122 version/variant bits, which would cost uniqueness)
)

// Kinds lists the supported key sets
func Kinds() []Kind { return []Kind{SeqU64, RandU64, UUID16} }

// Generator maps key indexes to key bytes
type Generator struct {
	kind Kind
	seed uint64
}

// New returns a generator for the named key set
// The seed changes which random keys are produced; sequential keys ignore it
func New(name string, seed uint64) (Generator, error) {
	for _, k := range Kinds() {
		if string(k) == name {
			return Generator{kind: k, seed: seed}, nil
		}
	}
	return Generator{}, fmt.Errorf("unknown key set %q (have %v)", name, Kinds())
}

// Kind returns the key set
func (g Generator) Kind() Kind { return g.kind }

// Len returns the length of bytes of every key
func (g Generator) Len() int {
	if g.kind == UUID16 {
		return 16
	}
	return 8
}

// Key writes the key for index i into buf which must have room for Len bytes
// It returns the written slice
// It does not allocate
func (g Generator) Key(i uint64, buf []byte) []byte {
	switch g.kind {
	case SeqU64:
		buf = buf[:8]
		binary.BigEndian.PutUint64(buf, i)
	case RandU64:
		buf = buf[:8]
		binary.BigEndian.PutUint64(buf, Mix(i^g.seed))
	case UUID16:
		buf = buf[:16]
		binary.BigEndian.PutUint64(buf[:8], Mix(i^g.seed))
		binary.BigEndian.PutUint64(buf[8:], Mix(^i^g.seed))
	}
	return buf
}

// Mix is the SplitMix64 finalizer -> a bijection on uint64 with good avalanche
func Mix(x uint64) uint64 {
	x += 0x9E3779B97F4A7C15
	x = (x ^ x>>30) * 0xBF58476D1CE4E5B9
	x = (x ^ x>>27) * 0x94D049BB133111EB
	return x ^ x>>31
}
