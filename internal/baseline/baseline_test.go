package baseline

import (
	"testing"

	arcticgo "github.com/nickr145/arctic-go"
	"github.com/nickr145/arctic-go/internal/conform"
)

func TestSyncMap(t *testing.T) {
	conform.RunMap(t, func() arcticgo.Index { return NewSyncMap() })
}

func TestLockedBTree(t *testing.T) {
	f := func() arcticgo.Index { return NewLockedBTree() }
	conform.RunMap(t, f)
	t.Run("Scan", func(t *testing.T) { conform.RunScan(t, f) })
}
