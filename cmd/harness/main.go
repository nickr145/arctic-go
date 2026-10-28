// Command harness runs the YCSB workloads from the design document against an index and writes one CSV row per (run, operation type)
//
// Example:
//
//	go run ./cmd/harness -index all -workload a -keys rand-u64 -n 1000000 \
//	    -ops 2000000 -threads 1,2,4,8 -out results.csv
package main

import (
	"encoding/csv"
	"errors"
	"flag"
	"fmt"
	"math/rand/v2"
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	arcticgo "github.com/nickr145/arctic-go"
	"github.com/nickr145/arctic-go/internal/baseline"
	"github.com/nickr145/arctic-go/internal/keys"
	"github.com/nickr145/arctic-go/internal/stats"
	"github.com/nickr145/arctic-go/internal/zipf"
)

// indexes is the registry of everything the harness can run
// The Arctic-Go variants register here as they land (M2 onward)
var indexes = map[string]func() arcticgo.Index{
	"syncmap":       func() arcticgo.Index { return baseline.NewSyncMap() },
	"btree-rwmutex": func() arcticgo.Index { return baseline.NewLockedBTree() },
}

// errUnsupported marks an index/workload pair that cannot run, so a sweep over several indexes can skip it instead of aborting
var errUnsupported = errors.New("unsupported combination")

type opKind int

const (
	opRead opKind = iota
	opUpdate
	opInsert
	opScan
	numOps
)

var opNames = [numOps]string{"read", "update", "insert", "scan"}

// maxScanLen is YCSB-E's maximum scan length; lengths are uniform in 1...max
const maxScanLen = 100

type config struct {
	workload      string
	keyset        string
	n             uint64  // keys loaded before run phase
	ops           uint64  // ops in run phase, split across threads
	theta         float64 // Zipfian skew; 0 means uniform
	scramble      bool
	sample        int // record the latency of every sample-th operation
	seed          uint64
	maxProcs      int // 0: GOMAXPROCS = threads, so threads beyond cores oversubscribe
	lockThreads   bool
	dist          *zipf.Dist
	keys          keys.Generator
	insertCounter atomic.Uint64 // next fresh key index for D and E inserts
}

func main() {
	var (
		indexFlag   = flag.String("index", "all", "comma-separated index names, or 'all'")
		workload    = flag.String("workload", "c", "load, a, b, c, d or e")
		keyset      = flag.String("keys", "rand-u64", "key set: seq-u64, rand-u64, uuid16")
		n           = flag.Uint64("n", 1_000_000, "number of keys")
		ops         = flag.Uint64("ops", 2_000_000, "operations in the run phase")
		threadsFlag = flag.String("threads", "1,2,4,8", "comma-separated thread counts")
		theta       = flag.Float64("zipf", 0.99, "Zipfian skew (not 1); 0 for uniform")
		scramble    = flag.Bool("scramble", true, "scramble Zipfian ranks over the key space, as YCSB does")
		sample      = flag.Int("latency-sample", 64, "record the latency of every Nth operation")
		seed        = flag.Uint64("seed", 1, "seed for key and request generation")
		maxProcs    = flag.Int("gomaxprocs", 0, "GOMAXPROCS for each run; 0 means equal to the thread count")
		lockThreads = flag.Bool("lock-os-threads", false, "wire each worker goroutine to an OS thread")
		out         = flag.String("out", "", "append CSV rows to this file (default: stdout)")
		label       = flag.String("label", "", "free-form tag copied to every row, e.g. the machine name")
		listIndexes = flag.Bool("list", false, "list available indexes and exit")
	)
	flag.Parse()

	if *listIndexes {
		for _, name := range sortedIndexNames() {
			fmt.Println(name)
		}
		return
	}
	cfg := &config{
		workload: strings.ToLower(*workload), keyset: *keyset, n: *n, ops: *ops,
		theta: *theta, scramble: *scramble, sample: max(*sample, 1), seed: *seed,
		maxProcs: *maxProcs, lockThreads: *lockThreads,
	}
	if err := cfg.validate(); err != nil {
		fatal(err)
	}
	names, err := selectIndexes(*indexFlag)
	if err != nil {
		fatal(err)
	}
	threads, err := parseInts(*threadsFlag)
	if err != nil {
		fatal(err)
	}

	w, closeOut, err := openOutput(*out)
	if err != nil {
		fatal(err)
	}
	defer closeOut()

	for _, name := range names {
		for _, t := range threads {
			res, err := runOne(cfg, name, t)
			if errors.Is(err, errUnsupported) {
				fmt.Fprintf(os.Stderr, "skipping %s: %v\n", name, err)
				break // the other thread counts would fail the same way
			}
			if err != nil {
				fatal(err)
			}
			fmt.Fprintf(os.Stderr, "%-14s %-5s %-9s threads=%-3d %8.3f Mops/s  %.1f B/key\n",
				name, cfg.workload, cfg.keyset, t, res.opsPerSec/1e6, res.bytesPerKey)
			writeRows(w, cfg, *label, name, t, res)
		}
	}
	w.Flush()
	if err := w.Error(); err != nil {
		fatal(err)
	}
}
func (c *config) validate() error {
	switch c.workload {
	case "load", "a", "b", "c", "d", "e":
	default:
		return fmt.Errorf("unknown workload %q (want load, a, b, c, d or e)", c.workload)
	}
	g, err := keys.New(c.keyset, c.seed)
	if err != nil {
		return err
	}
	c.keys = g
	if c.n < 2 {
		return fmt.Errorf("-n must be at least 2")
	}
	if c.theta != 0 {
		d, err := zipf.New(c.n, c.theta)
		if err != nil {
			return err
		}
		c.dist = d
	}
	return nil
}

func selectIndexes(spec string) ([]string, error) {
	if spec == "all" {
		return sortedIndexNames(), nil
	}
	var names []string
	for _, name := range strings.Split(spec, ",") {
		if _, ok := indexes[name]; !ok {
			return nil, fmt.Errorf("unknown index %q (have %v)", name, sortedIndexNames())
		}
		names = append(names, name)
	}
	return names, nil
}

func sortedIndexNames() []string {
	names := make([]string, 0, len(indexes))
	for name := range indexes {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func parseInts(s string) ([]int, error) {
	var out []int
	for _, f := range strings.Split(s, ",") {
		v, err := strconv.Atoi(strings.TrimSpace(f))
		if err != nil || v < 1 {
			return nil, fmt.Errorf("bad thread count %q", f)
		}
		out = append(out, v)
	}
	return out, nil
}

type result struct {
	seconds     float64
	opsPerSec   float64
	totalOps    uint64
	hists       [numOps]stats.Hist
	bytesPerKey float64
}

// runOne builds a fresh index, loads it (timed only for the load workload), runs the workload on the given number of threads, and reports the result
func runOne(cfg *config, name string, threads int) (*result, error) {
	idx := indexes[name]()
	if cfg.workload == "e" {
		if _, ok := idx.NewSession().(arcticgo.Scanner); !ok {
			return nil, fmt.Errorf("%w: workload e needs scans and %q is unordered", errUnsupported, name)
		}
	}
	procs := cfg.maxProcs
	if procs == 0 {
		procs = threads
	}
	prev := runtime.GOMAXPROCS(procs)
	defer runtime.GOMAXPROCS(prev)

	cfg.insertCounter.Store(cfg.n)
	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)

	res := &result{}
	if cfg.workload == "load" {
		// The load phase is the measurement
		res.totalOps = cfg.n
		res.seconds = parallel(threads, cfg, func(tid int, h *[numOps]stats.Hist) {
			loadRange(cfg, idx.NewSession(), chunk(cfg.n, threads, tid), h)
		}, res)
		res.bytesPerKey = heapPerKey(&before, cfg.n)
	} else {
		// Untimed preload, using every core regardless of the sweep point
		pre := runtime.NumCPU()
		runtime.GOMAXPROCS(max(pre, procs))
		var wg sync.WaitGroup
		for tid := 0; tid < pre; tid++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				loadRange(cfg, idx.NewSession(), chunk(cfg.n, pre, tid), nil)
			}()
		}
		wg.Wait()
		runtime.GOMAXPROCS(procs)
		res.bytesPerKey = heapPerKey(&before, cfg.n)

		perThread := cfg.ops / uint64(threads)
		res.totalOps = perThread * uint64(threads)
		res.seconds = parallel(threads, cfg, func(tid int, h *[numOps]stats.Hist) {
			runOps(cfg, idx.NewSession(), tid, perThread, h)
		}, res)
	}
	res.opsPerSec = float64(res.totalOps) / res.seconds
	runtime.KeepAlive(idx)
	return res, nil
}

// heapPerKey returns heap growth per loaded key since before
// It forces a collection, so it must run outside any timed region
func heapPerKey(before *runtime.MemStats, keys uint64) float64 {
	runtime.GC()
	var after runtime.MemStats
	runtime.ReadMemStats(&after)
	return float64(after.HeapAlloc-before.HeapAlloc) / float64(keys)
}

func chunk(n uint64, parts, i int) [2]uint64 {
	per := n / uint64(parts)
	lo := per * uint64(i)
	hi := lo + per
	if i == parts-1 {
		hi = n
	}
	return [2]uint64{lo, hi}
}

// parallel runs fn on `threads` goroutines released together and returns the wall-clock seconds until the last one finishes
// Per-thread histograms are merged into res
func parallel(threads int, cfg *config, fn func(tid int, h *[numOps]stats.Hist), res *result) float64 {
	hists := make([][numOps]stats.Hist, threads)
	ready := make(chan struct{}, threads)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for tid := 0; tid < threads; tid++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if cfg.lockThreads {
				runtime.LockOSThread()
				defer runtime.UnlockOSThread()
			}
			ready <- struct{}{}
			<-start
			fn(tid, &hists[tid])
		}()
	}
	for i := 0; i < threads; i++ {
		<-ready
	}
	t0 := time.Now()
	close(start)
	wg.Wait()
	secs := time.Since(t0).Seconds()
	for tid := range hists {
		for op := range res.hists {
			res.hists[op].Merge(&hists[tid][op])
		}
	}
	return secs
}

func loadRange(cfg *config, m arcticgo.Map, r [2]uint64, h *[numOps]stats.Hist) {
	buf := make([]byte, cfg.keys.Len())
	countdown := 1
	for i := r[0]; i < r[1]; i++ {
		k := cfg.keys.Key(i, buf)
		if h != nil {
			if countdown--; countdown == 0 {
				countdown = cfg.sample
				t0 := time.Now()
				m.Insert(k, i)
				h[opInsert].Record(uint64(time.Since(t0)))
				continue
			}
		}
		m.Insert(k, i)
	}
}

type worker struct {
	cfg  *config
	m    arcticgo.Map
	sc   arcticgo.Scanner
	r    *rand.Rand
	gen  *zipf.Gen
	buf  []byte
	sink uint64
}

var sinkAll atomic.Uint64 // keeps results of reads and scans observable

func runOps(cfg *config, m arcticgo.Map, tid int, count uint64, h *[numOps]stats.Hist) {
	w := &worker{
		cfg: cfg, m: m,
		r:   rand.New(rand.NewPCG(cfg.seed, uint64(tid)+1)),
		buf: make([]byte, cfg.keys.Len()),
	}
	w.sc, _ = m.(arcticgo.Scanner)
	if cfg.dist != nil {
		w.gen = cfg.dist.NewGen(w.r)
	}
	countdown := 1
	for i := uint64(0); i < count; i++ {
		op := w.choose()
		if countdown--; countdown == 0 {
			countdown = cfg.sample
			t0 := time.Now()
			w.do(op)
			h[op].Record(uint64(time.Since(t0)))
		} else {
			w.do(op)
		}
	}
	sinkAll.Add(w.sink)
}

// choose picks the next operation according to the workload's mix
func (w *worker) choose() opKind {
	p := w.r.IntN(100)
	switch w.cfg.workload {
	case "a":
		if p < 50 {
			return opRead
		}
		return opUpdate
	case "b":
		if p < 95 {
			return opRead
		}
		return opUpdate
	case "d":
		if p < 95 {
			return opRead
		}
		return opInsert
	case "e":
		if p < 95 {
			return opScan
		}
		return opInsert
	}
	return opRead // workload c
}

// existing picks the index of an already-loaded key: Zipfian (scrambled) or uniform over the first n keys
func (w *worker) existing() uint64 {
	c := w.cfg
	if w.gen == nil {
		return w.r.Uint64N(c.n)
	}
	rank := w.gen.Next()
	if c.scramble {
		return zipf.Scramble(rank, c.n)
	}
	return rank
}

// latest picks a key skewed toward the most recent insertions (workload D)
func (w *worker) latest() uint64 {
	newest := w.cfg.insertCounter.Load() - 1
	var back uint64
	if w.gen == nil {
		back = w.r.Uint64N(w.cfg.n)
	} else {
		back = w.gen.Next()
	}
	return newest - min(back, newest)
}

func (w *worker) do(op opKind) {
	c := w.cfg
	switch op {
	case opRead:
		var i uint64
		if c.workload == "d" {
			i = w.latest()
		} else {
			i = w.existing()
		}
		if v, ok := w.m.Get(c.keys.Key(i, w.buf)); ok {
			w.sink += v
		}
	case opUpdate:
		i := w.existing()
		w.m.Update(c.keys.Key(i, w.buf), w.r.Uint64())
	case opInsert:
		i := c.insertCounter.Add(1) - 1
		w.m.Insert(c.keys.Key(i, w.buf), i)
	case opScan:
		want := 1 + w.r.IntN(maxScanLen)
		got := 0
		w.sc.Scan(c.keys.Key(w.existing(), w.buf), nil, func(_ []byte, v uint64) bool {
			w.sink += v
			got++
			return got < want
		})
	}
}

var csvHeader = []string{
	"label", "index", "workload", "keys", "n", "ops", "threads", "gomaxprocs", "zipf", "scramble",
	"seconds", "ops_per_sec", "bytes_per_key", "op", "op_samples", "p50_ns", "p99_ns", "p999_ns",
}

func writeRows(w *csv.Writer, cfg *config, label, name string, threads int, res *result) {
	procs := cfg.maxProcs
	if procs == 0 {
		procs = threads
	}
	for op := opKind(0); op < numOps; op++ {
		h := &res.hists[op]
		if h.Count() == 0 {
			continue
		}
		w.Write([]string{
			label, name, cfg.workload, cfg.keyset,
			strconv.FormatUint(cfg.n, 10), strconv.FormatUint(res.totalOps, 10),
			strconv.Itoa(threads), strconv.Itoa(procs),
			strconv.FormatFloat(cfg.theta, 'g', -1, 64), strconv.FormatBool(cfg.scramble),
			strconv.FormatFloat(res.seconds, 'f', 6, 64),
			strconv.FormatFloat(res.opsPerSec, 'f', 0, 64),
			strconv.FormatFloat(res.bytesPerKey, 'f', 1, 64),
			opNames[op], strconv.FormatUint(h.Count(), 10),
			strconv.FormatUint(h.Quantile(0.50), 10),
			strconv.FormatUint(h.Quantile(0.99), 10),
			strconv.FormatUint(h.Quantile(0.999), 10),
		})
	}
	w.Flush()
}

// openOutput returns a CSV writer on stdout or an appended file, emitting the header only when the destination is empty
func openOutput(path string) (*csv.Writer, func(), error) {
	if path == "" {
		w := csv.NewWriter(os.Stdout)
		w.Write(csvHeader)
		return w, func() {}, nil
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, nil, err
	}
	w := csv.NewWriter(f)
	if st, err := f.Stat(); err == nil && st.Size() == 0 {
		w.Write(csvHeader)
	}
	return w, func() { w.Flush(); f.Close() }, nil
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "harness:", err)
	os.Exit(1)
}
