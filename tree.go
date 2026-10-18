// Package arcticgo is a Go implementation of Arctic (OSDI '26), a lock-free adaptive radix tree with ordered range and prefix scans
// This file defines the API shared by every index in the repository
// The two Arctic-Go variants (arena-backed and garbage-collected) and the baselines they are measured against
package arcticgo

// Map is the point-operation API
//
// Keys are byte strings. Tree implementations require that no key is a prefix of another key
// Byte-string callers must terminate keys with a sentinel byte + fixed-width integers satisfy the rule on their own
//
// A Map value is NOT necessarily safe for use by several goroutines at once.
// Obtain one per worker goroutine from Index.NewSession
type Map interface {
	// Get returns value for key
	Get(key []byte) (uint64, bool)
	// Insert adds key. Returns false, and changes nothing, if key exists
	Insert(key []byte, val uint64) bool
	// Update replaces value of key. Returns false if key is absent
	Update(key []byte, val uint64) bool
	// Delete removes key. Returns false if key is absent
	Delete(key []byte) bool
}

// Scanner is implemented by ordered indexes
//
// Scan calls fn for every key k with low <= k < hi in lexicographic order until fn -> false
// A nil lo = from the first key
// A nil hi = to the last key
// The key slice passed to fn is only valid for the duration of the call
//
// Scans are not snapshot-isolated
// They guarantee that each key is observed at most once, in order, + that every in-range key present for the whole scan is observed
type Scanner interface {
	Scan(lo, hi []byte, fn func(key []byte, val uint64) bool)
}

// Tree is an ordered map: point operations + scans
//
// It's the design document's Tree interface split into Map and Scanner
// So, unordered baselines like sync.Map can share the point operation test suite and benchmark harness
type Tree interface {
	Map
	Scanner
}

// Index is a factory for per-worker handles onto one shared structure
//
// The arena-backed variant needs per-goroutine state (hazard slot, local free lists, retired list), so workers never use an Index directly
// Indexes with no such state return a shared, goroutine-safe Map from NewSession
type Index interface {
	// Name identifies the index in benchmark output
	Name() string
	// NewSession returns a handle for one worker goroutine
	// If the result also implements Scanner, the index supports scans
	NewSession() Map
}
