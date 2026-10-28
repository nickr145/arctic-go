package main

import (
	"bytes"
	"encoding/csv"
	"errors"
	"testing"

	"github.com/nickr145/arctic-go/internal/baseline"
	"github.com/nickr145/arctic-go/internal/stats"
)

func testConfig(t *testing.T, workload string) *config {
	t.Helper()
	cfg := &config{
		workload: workload, keyset: "rand-u64", n: 5000, ops: 8000,
		theta: 0.99, scramble: true, sample: 4, seed: 1,
	}
	if err := cfg.validate(); err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestEveryWorkloadRuns(t *testing.T) {
	for _, workload := range []string{"load", "a", "b", "c", "d", "e"} {
		t.Run(workload, func(t *testing.T) {
			cfg := testConfig(t, workload)
			res, err := runOne(cfg, "btree-rwmutex", 2)
			if err != nil {
				t.Fatal(err)
			}
			if res.totalOps == 0 || res.seconds <= 0 || res.opsPerSec <= 0 {
				t.Fatalf("empty result: %+v", res)
			}
			var sampled uint64
			for op := range res.hists {
				sampled += res.hists[op].Count()
			}
			if sampled == 0 {
				t.Fatal("no latency samples recorded")
			}
			if res.bytesPerKey <= 0 {
				t.Fatalf("bytes per key = %v", res.bytesPerKey)
			}

			var buf bytes.Buffer
			w := csv.NewWriter(&buf)
			writeRows(w, cfg, "test", "btree-rwmutex", 2, res)
			rows, err := csv.NewReader(&buf).ReadAll()
			if err != nil || len(rows) == 0 {
				t.Fatalf("CSV output: %v, %d rows", err, len(rows))
			}
			for _, row := range rows {
				if len(row) != len(csvHeader) {
					t.Fatalf("row has %d columns, header has %d", len(row), len(csvHeader))
				}
			}
		})
	}
}

// TestInsertWorkloadsAddKeys checks that D's and E's 5% insert share advances the fresh-key counter -> that the inserted keys are genuinely new
func TestInsertWorkloadsAddKeys(t *testing.T) {
	for _, workload := range []string{"d", "e"} {
		cfg := testConfig(t, workload)
		cfg.insertCounter.Store(cfg.n)
		m := baseline.NewLockedBTree()
		loadRange(cfg, m, [2]uint64{0, cfg.n}, nil)
		var h [numOps]stats.Hist
		runOps(cfg, m, 0, 4000, &h)
		inserted := cfg.insertCounter.Load() - cfg.n
		if inserted < 100 || inserted > 400 {
			t.Fatalf("workload %s: %d inserts in 4000 ops, want about 200", workload, inserted)
		}
		buf := make([]byte, 8)
		if _, ok := m.Get(cfg.keys.Key(cfg.n+inserted-1, buf)); !ok {
			t.Fatalf("workload %s: last inserted key is missing", workload)
		}
	}
}

func TestScanNeedsOrderedIndex(t *testing.T) {
	cfg := testConfig(t, "e")
	if _, err := runOne(cfg, "syncmap", 1); !errors.Is(err, errUnsupported) {
		t.Fatalf("err = %v, want errUnsupported", err)
	}
}
