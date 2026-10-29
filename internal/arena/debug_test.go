//go:build arenadebug

package arena

import "testing"

func TestDebugReadOfFreedSlotPanics(t *testing.T) {
	a := New(small)
	h := a.Alloc(ClassNode31)
	a.Free(ClassNode31, h)
	defer func() {
		if recover() == nil {
			t.Fatal("Words on a freed slot did not panic")
		}
	}()
	a.Words(ClassNode31, h)
}

func TestDebugDoubleFreePanics(t *testing.T) {
	a := New(small)
	h := a.Alloc(ClassNode7)
	a.Free(ClassNode7, h)
	defer func() {
		if recover() == nil {
			t.Fatal("double free did not panic")
		}
	}()
	a.Free(ClassNode7, h)
}

func TestDebugFreedSlotIsPoisoned(t *testing.T) {
	a := New(small)
	h := a.Alloc(ClassNode7)
	a.Words(ClassNode7, h)[3] = 42
	raw := a.pools[ClassNode7].slot(h) // bypasses the liveness check
	a.Free(ClassNode7, h)
	for i, v := range raw {
		if v != poisonWord {
			t.Fatalf("word %d = %#x after free, want poison %#x", i, v, uint64(poisonWord))
		}
	}
}

func TestDebugReallocClearsPoison(t *testing.T) {
	a := New(small)
	h := a.Alloc(ClassNode7)
	a.Free(ClassNode7, h)
	h2 := a.Alloc(ClassNode7)
	if h2 != h {
		t.Fatalf("expected reuse of %d, got %d", h, h2)
	}
	for i, v := range a.Words(ClassNode7, h2) {
		if v != 0 {
			t.Fatalf("word %d = %#x after realloc", i, v)
		}
	}
}
