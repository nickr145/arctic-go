# Arctic-Go

A Go implementation of [Arctic](https://www.usenix.org/conference/osdi26/presentation/ni)
(OSDI '26), a lock-free adaptive radix tree with ordered range and prefix scans,
adapted to Go's 64-bit atomics and garbage collector. The project also measures
what that adaptation costs. See the design document for the full plan.

**Status:** M0 (setup) is code-complete: interfaces, edge encoding, conformance
suite, baselines and the benchmark harness. Baseline numbers on the cloud VM are
still to do. M1 (sequential tree) is next.

## Layout

```
tree.go                 Map / Scanner / Tree / Index interfaces
internal/edge/          64-bit edge word: encoding, branchless prefix match, freeze/CAS
internal/baseline/      sync.Map and google/btree + RWMutex baselines
internal/conform/       shared correctness suite every index must pass
internal/keys/          seq-u64, rand-u64, uuid16 key sets (pure functions of the index)
internal/zipf/          Zipfian request generator (YCSB's, plus theta > 1)
internal/stats/         log-linear latency histogram
cmd/harness/            YCSB Load/A-E driver, thread sweep, CSV output
```

## Commands

```sh
go test -race ./...                                   # everything, race detector on
go test -run '^$' -fuzz FuzzMatch ./internal/edge     # fuzz the prefix matcher

go run ./cmd/harness -list
mkdir -p results                                      # the harness does not create it
go run ./cmd/harness -index all -workload a -keys rand-u64 \
    -n 5000000 -ops 20000000 -threads 1,2,4,8 -out results/a.csv -label macbook
```

The harness writes one CSV row per (run, operation type): throughput, bytes per
key, and p50/p99/p999 latency from sampled operations (`-latency-sample`).
Re-running appends to the same file. By default `GOMAXPROCS` equals the thread
count, so thread counts above the core count oversubscribe cores, as in the
paper. For pinned runs on the final VM, wrap the binary in `taskset` and pass
`-lock-os-threads`. Workload E skips indexes without ordered scans.

## Development baseline (laptop, single run)

YCSB-A, rand-u64 keys, 5M keys, 20M operations, Zipfian 0.99, MacBook.
These are smoke numbers, **not** the M0 baseline: one run each, no pinning, and
a laptop's core count and thermals add noise.

| Threads | btree-rwmutex (Mops/s) | syncmap (Mops/s) |
|---|---|---|
| 1 | 0.955 | 1.391 |
| 2 | 0.806 | 2.171 |
| 4 | 0.833 | 5.566 |
| 8 | 0.829 | 9.531 |

The single-lock B-tree does not scale: half of workload A is updates, which take
the exclusive lock, and update p99 grows from about 2.6 µs at 1 thread to about
180 µs at 8. `sync.Map` scales but is unordered and uses 135 B/key against 56 B/key
for the B-tree.

## Decisions made in M0

- **`Tree` is split into `Map` + `Scanner`.** `sync.Map` is unordered, so it
  implements `Map` only and shares the point-operation tests and benchmarks.
  `Tree` is `Map` + `Scanner`, which is the design doc's interface.
- **Sessions are explicit in the API.** `Index.NewSession()` returns the
  per-goroutine handle. Indexes without per-goroutine state return themselves.
  This settles the "explicit Session vs pool" open question for now.
- **Scan bounds are `[lo, hi)`**, with `nil` meaning unbounded.
- **The edge bit split is two constants** (`PrefixBytes`, `HandleBits`), with
  compile-time layout checks and a test pinning the design doc's layout.
- **Null edges are canonical**: handle 0 with no type or value bit.
  `Edge.Cleared()` keeps the prefix bytes, which assumes a deleted edge keeps its
  key bytes. Revisit this in M1 when delete and re-insert paths are written.

## Known gaps and calibration items

- **Baseline numbers need the cloud VM.** The table above is a laptop smoke test.
  The M0 exit criterion is `sync.Map` and locked-B-tree results from the many-core
  VM with pinned threads, up to twice the physical core count.
- The Zipfian generator is the Gray et al. generator YCSB uses (exact for ranks
  0 and 1, smoothed beyond). At 100M keys the textbook top-10 shares are 14.2%,
  29.8% and 45.1% for exponents 0.99, 1.1 and 1.2. The paper reports 17%, 33% and
  48%, so its generator is hotter. Before the M4 reclamation experiments,
  calibrate so the skew levels match the paper's.
- Email-like and URL-like key sets arrive with string keys in M1.