package keys

import (
	"bytes"
	"testing"
)

func TestUniqueAndDeterministic(t *testing.T) {
	for _, kind := range Kinds() {
		g, err := New(string(kind), 99)
		if err != nil {
			t.Fatal(err)
		}
		buf := make([]byte, g.Len())
		seen := make(map[string]uint64, 1_000_000)
		for i := uint64(0); i < 1_000_000; i++ {
			k := g.Key(i, buf)
			if len(k) != g.Len() {
				t.Fatalf("%s: key length %d, want %d", kind, len(k), g.Len())
			}
			if j, dup := seen[string(k)]; dup {
				t.Fatalf("%s: indexes %d and %d produce the same key", kind, j, i)
			}
			seen[string(k)] = i
		}
		g2, _ := New(string(kind), 99)
		b1, b2 := make([]byte, g.Len()), make([]byte, g.Len())
		if !bytes.Equal(g.Key(12345, b1), g2.Key(12345, b2)) {
			t.Fatalf("%s: not deterministic", kind)
		}
	}
}

func TestSeqIsOrdered(t *testing.T) {
	g, _ := New("seq-u64", 0)
	a, b := make([]byte, 8), make([]byte, 8)
	for i := uint64(0); i < 100_000; i += 997 {
		if bytes.Compare(g.Key(i, a), g.Key(i+1, b)) >= 0 {
			t.Fatalf("seq keys %d and %d out of order", i, i+1)
		}
	}
}

func TestUnknownKind(t *testing.T) {
	if _, err := New("nope", 0); err == nil {
		t.Fatal("expected an error for an unknown key set")
	}
}

func TestKeyDoesNotAllocate(t *testing.T) {
	g, _ := New("uuid16", 1)
	buf := make([]byte, 16)
	if n := testing.AllocsPerRun(1000, func() { g.Key(7, buf) }); n != 0 {
		t.Fatalf("Key allocates %v times per call", n)
	}
}
