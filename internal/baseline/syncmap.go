// Package baseline holds the comparison indexes from the design document:
// sync.Map (unordered) and google/btree behind a sync.RWMutex (ordered).
package baseline

import (
	"sync"

	arcticgo "github.com/nickr145/arctic-go"
)

// SyncMap wraps the standard library's sync.Map
//
// It is unordered, so it implements Map but not Scanner
// Keys are converted to strings and values are boxed into interfaces, so every operation allocates;
// This baseline represents that cost
type SyncMap struct{ m sync.Map }

// NewSyncMap returns an empty SyncMap index
func NewSyncMap() *SyncMap { return &SyncMap{} }

// Name impl arcticgo.Index
func (s *SyncMap) Name() string { return "syncmap" }

// NewSession impl arcticgo.Index
// A sync.Map is safe for concurrent use, so all workers share it
func (s *SyncMap) NewSession() arcticgo.Map { return s }

// Get impl arcticgo.Map
func (s *SyncMap) Get(key []byte) (uint64, bool) {
	v, ok := s.m.Load(string(key))
	if !ok {
		return 0, false
	}
	return v.(uint64), true
}

// Insert impl arcticgo.Map
func (s *SyncMap) Insert(key []byte, val uint64) bool {
	_, loaded := s.m.LoadOrStore(string(key), val)
	return !loaded
}

// Update impl arcticgo.Map
func (s *SyncMap) Update(key []byte, val uint64) bool {
	k := string(key)
	for {
		old, ok := s.m.Load(k)
		if !ok {
			return false
		}
		if s.m.CompareAndSwap(k, old, val) {
			return true
		}
		// the value changed or the key was deleted btw Load and CAS; retry
	}
}

// Delete impl arcticgo.Map
func (s *SyncMap) Delete(key []byte) bool {
	_, loaded := s.m.LoadAndDelete(string(key))
	return loaded
}
