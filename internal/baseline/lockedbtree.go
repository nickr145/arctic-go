package baseline

import (
	"bytes"
	"sync"

	"github.com/google/btree"
	arcticgo "github.com/nickr145/arctic-go"
)

type item struct {
	key []byte
	val uint64
}

func less(a, b item) bool { return bytes.Compare(a.key, b.key) < 0 }

// LockedBTree is a google/btree guarded by a single reader-writer lock
// The classic "ordered map with a mutex" that a lock-free tree must beat
type LockedBTree struct {
	mu sync.RWMutex
	t  *btree.BTreeG[item]
}

// btreeDegree is the google/btree degree; 32 is the library's usual choice
const btreeDegree = 32

// NewLockedBTree returns an empty LockedBTree index
func NewLockedBTree() *LockedBTree {
	return &LockedBTree{t: btree.NewG(btreeDegree, less)}
}

// Name impl arcticgo.Index
func (b *LockedBTree) Name() string { return "btree-rwmutex" }

// NewSession implements arcticgo.Index
// All workers share the one lock
func (b *LockedBTree) NewSession() arcticgo.Map { return b }

// Get impl arcticgo.Map
func (b *LockedBTree) Get(key []byte) (uint64, bool) {
	b.mu.RLock()
	it, ok := b.t.Get(item{key: key})
	b.mu.RUnlock()
	return it.val, ok
}

// Insert impl arcticgo.Map
func (b *LockedBTree) Insert(key []byte, val uint64) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.t.Has(item{key: key}) {
		return false
	}

	// tree owns its keys; callers reuse their buffers
	b.t.ReplaceOrInsert(item{key: bytes.Clone(key), val: val})
	return true
}

// Update impl arcticgo.Map
func (b *LockedBTree) Update(key []byte, val uint64) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	old, ok := b.t.Get(item{key: key})
	if !ok {
		return false
	}
	b.t.ReplaceOrInsert(item{key: old.key, val: val})
	return true
}

// Delete impl arcticgo.Map
func (b *LockedBTree) Delete(key []byte) bool {
	b.mu.Lock()
	_, ok := b.t.Delete(item{key: key})
	b.mu.Unlock()
	return ok
}

// Scan implements arcticgo.Scanner
// The read lock is held while fn runs, so a slow callback blocks writers
// A cost of the single-lock design.
func (b *LockedBTree) Scan(lo, hi []byte, fn func(key []byte, val uint64) bool) {
	visit := func(it item) bool { return fn(it.key, it.val) }
	b.mu.RLock()
	defer b.mu.RUnlock()

	switch {
	case lo == nil && hi == nil:
		b.t.Ascend(visit)
	case hi == nil:
		b.t.AscendGreaterOrEqual(item{key: lo}, visit)
	case lo == nil:
		b.t.AscendLessThan(item{key: hi}, visit)
	default:
		b.t.AscendRange(item{key: lo}, item{key: hi}, visit)
	}
}

var (
	_ arcticgo.Index = (*SyncMap)(nil)
	_ arcticgo.Index = (*LockedBTree)(nil)
	_ arcticgo.Map   = (*SyncMap)(nil)
	_ arcticgo.Tree  = (*LockedBTree)(nil)
)
