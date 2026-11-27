package pack_test

import (
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/devrdias/ambermoth/internal/ledger"
	"github.com/devrdias/ambermoth/internal/ops"
	"github.com/devrdias/ambermoth/internal/pack"
	"github.com/devrdias/ambermoth/internal/tsa/tsatest"
)

// Pack and verify benchmarks on generated ledgers. The ledger of each size is
// written once and shared by every benchmark in the run; see ledgerCache.
//
//	go test ./internal/pack -run '^$' -bench . -benchmem
//	AMBERMOTH_BENCH_1M=1 go test ./internal/pack -run '^$' -bench . -benchmem -timeout 2h

const (
	benchAnchors = 50 // anchor entries per ledger, one TSA each: real tokens come from openssl, one process each
	benchDays    = 20 // the ledger covers this many whole UTC days, ending before today
	sizeHeading  = "%dk"
	million      = 1_000_000
	diskPerEntry = 3000 // bytes: the ledger takes about 850 per entry, the pack and the copy it is built from the rest
)

var (
	benchModels   = [][]string{{"gpt-4o", "gpt-4o-mini", "o3-mini"}, {"claude-sonnet-4", "claude-haiku-4"}}
	benchRoutes   = []benchRoute{{"openai", "chat.completions", "/v1/chat/completions"}, {"anthropic", "messages", "/v1/messages"}}
	benchPurposes = []string{"production-inference", "production-inference", "production-inference", "eval-regression", "eval-benchmark", "rag-query", "batch-job", "dev-experiment"}
	benchKeys     = []string{"sk-bench-1", "sk-bench-2", "sk-bench-3"}
)

type benchRoute struct{ provider, endpoint, path string }

// ledgerCache holds the generated ledgers by size, in one temp directory that
// TestMain removes. Benchmarks in different functions share them, which a
// per-benchmark b.TempDir would not allow.
type ledgerCache struct {
	mu   sync.Mutex
	root string
	by   map[int]*benchLedger
}

var benchCache = ledgerCache{by: map[int]*benchLedger{}}

func TestMain(m *testing.M) {
	code := m.Run()
	if benchCache.root != "" {
		_ = os.RemoveAll(benchCache.root)
	}
	os.Exit(code)
}

// benchLedger is a generated ledger on disk.
type benchLedger struct {
	dir      string
	roots    *x509.CertPool
	entries  int
	start    time.Time // first entry
	step     time.Duration
	bytes    int64
	genTime  time.Duration
	callsEnd int
}

func (bl *benchLedger) at(tick int) time.Time { return bl.start.Add(time.Duration(tick) * bl.step) }

func (bl *benchLedger) ledgerPath() string { return filepath.Join(bl.dir, ledger.LedgerFile) }

// scope is the part of the ledger a pack covers.
type scope struct {
	name           string
	fromDay, toDay int // days from the start of the ledger, the last one excluded
}

var benchScopes = []scope{{"whole", 0, benchDays}, {"middle10", 9, 11}}

// options builds a pack over whole days, as the CLI does: the verifier takes the period from the manifest dates.
func (bl *benchLedger) options(sc scope, out string, roots *x509.CertPool) pack.Options {
	return pack.Options{From: bl.start.AddDate(0, 0, sc.fromDay), To: bl.start.AddDate(0, 0, sc.toDay),
		Roots: roots, Out: out, Generator: "bench"}
}

// benchSizes are the ledger sizes to run: 10k and 100k, or the entry counts in
// AMBERMOTH_BENCH_SIZES (comma separated), and 1M more with AMBERMOTH_BENCH_1M=1.
func benchSizes() []int {
	sizes := []int{10_000, 100_000}
	if list := os.Getenv("AMBERMOTH_BENCH_SIZES"); list != "" {
		sizes = parseSizes(list)
	}
	if os.Getenv("AMBERMOTH_BENCH_1M") == "1" {
		sizes = append(sizes, million)
	}
	return sizes
}

func parseSizes(list string) []int {
	var sizes []int
	for _, f := range strings.Split(list, ",") {
		if n, err := strconv.Atoi(strings.TrimSpace(f)); err == nil && n >= benchAnchors {
			sizes = append(sizes, n)
		}
	}
	return sizes
}

func sizeName(n int) string {
	if n >= million {
		return fmt.Sprintf("%dM", n/million)
	}
	return fmt.Sprintf(sizeHeading, n/1000)
}

// benchFixture returns the ledger of about n entries, writing it on first use.
func benchFixture(b *testing.B, n int) *benchLedger {
	b.Helper()
	benchCache.mu.Lock()
	defer benchCache.mu.Unlock()
	if bl, ok := benchCache.by[n]; ok {
		return bl
	}
	b.StopTimer()
	defer b.StartTimer()
	root := benchCache.root
	if root == "" {
		root = mkdirTemp(b)
		benchCache.root = root
	}
	skipIfDiskShort(b, root, n)
	bl := generateLedger(b, mkdirIn(b, root), n, benchAnchors)
	b.Logf("generated %d entries in %v (%.0f entries/s), ledger %d MB, %d anchors",
		bl.entries, bl.genTime.Round(time.Millisecond), float64(bl.entries)/bl.genTime.Seconds(), bl.bytes>>20, benchAnchors)
	benchCache.by[n] = bl
	return bl
}

func mkdirTemp(tb testing.TB) string {
	dir, err := os.MkdirTemp("", "ambermoth-bench-")
	if err != nil {
		tb.Fatal(err)
	}
	return dir
}

func mkdirIn(tb testing.TB, root string) string {
	dir, err := os.MkdirTemp(root, "ledger-")
	if err != nil {
		tb.Fatal(err)
	}
	return dir
}

func skipIfDiskShort(b *testing.B, dir string, n int) {
	free, err := freeDiskBytes(dir)
	if err != nil {
		b.Fatal(err)
	}
	if need := uint64(n) * diskPerEntry; free < need {
		b.Skipf("%d entries need about %d MB free in %s, have %d MB", n, need>>20, dir, free>>20)
	}
}

// generator writes a ledger in chunks, anchoring after each one.
type generator struct {
	tb      testing.TB
	bl      *benchLedger
	s       *ledger.Store
	now     time.Time // what the store's clock says
	tsaURL  string
	chunk   int
	anchors int
}

// generateLedger writes calls, gaps, amendments and key events through
// Store.Append, one batch per chunk, and anchors the head of each chunk with
// a token from tsatest. Only benchAnchors tokens are made: each is an openssl
// run, which is too slow to do at every K entries of a million.
func generateLedger(tb testing.TB, dir string, n, anchors int) *benchLedger {
	tb.Helper()
	auth := tsatest.New(tb)
	g := &generator{tb: tb, tsaURL: auth.Server(tb).URL, chunk: n / anchors, anchors: anchors}
	g.bl = &benchLedger{dir: dir, roots: auth.Roots, step: benchDays * 24 * time.Hour / time.Duration(n), callsEnd: g.chunk * anchors}
	g.bl.start = time.Now().UTC().AddDate(0, 0, -benchDays-1).Truncate(24 * time.Hour)
	g.now = g.bl.at(0)
	s, err := ledger.Init(dir, "bench", func() time.Time { return g.now })
	if err != nil {
		tb.Fatal(err)
	}
	g.s = s
	began := time.Now()
	g.keyEvents()
	for c := 0; c < anchors; c++ {
		g.writeChunk(c)
	}
	g.bl.genTime = time.Since(began)
	g.finish()
	return g.bl
}

func (g *generator) finish() {
	seq, _, err := g.s.Head()
	if err != nil {
		g.tb.Fatal(err)
	}
	st, err := os.Stat(g.bl.ledgerPath())
	if err != nil {
		g.tb.Fatal(err)
	}
	g.bl.entries, g.bl.bytes = int(seq)+1, st.Size()
}

func (g *generator) keyEvents() {
	for _, k := range benchKeys {
		if _, err := ops.KeyEvent(g.s, "enroll", k, "", "openai", "bench", "initial", ""); err != nil {
			g.tb.Fatal(err)
		}
	}
}

// writeChunk appends the calls of chunk c, then what the chunk ends with: now
// and then an amendment, a key rotation halfway, and always an anchor.
func (g *generator) writeChunk(c int) {
	head, _, err := g.s.Head()
	if err != nil {
		g.tb.Fatal(err)
	}
	batch := make([]ledger.Entry, g.chunk)
	for i := range batch {
		batch[i] = g.entry(c*g.chunk + i)
	}
	if err := g.s.Append(batch...); err != nil {
		g.tb.Fatal(err)
	}
	g.now = g.bl.at((c + 1) * g.chunk)
	g.extras(c, head+1)
	if _, err := ops.Anchor(context.Background(), g.s, []string{g.tsaURL}, nil); err != nil {
		g.tb.Fatal(err)
	}
}

func (g *generator) extras(c int, firstSeq int64) {
	if c%10 == 2 {
		g.append(ledger.Entry{"type": ledger.TypeAmendment, "target_from": firstSeq, "target_to": firstSeq + int64(g.chunk/4),
			"new_purpose": "eval-benchmark", "reason": "retagged", "by": "bench"})
	}
	if c == g.anchors/2 {
		if _, err := ops.KeyEvent(g.s, "rotate", benchKeys[2], "", "openai", "bench", "scheduled", ledger.KeyFingerprint("sk-bench-4")); err != nil {
			g.tb.Fatal(err)
		}
	}
}

func (g *generator) append(e ledger.Entry) {
	e["ts"] = ledger.FormatTime(g.now)
	if err := g.s.Append(e); err != nil {
		g.tb.Fatal(err)
	}
}

// entry is the k-th entry of the call stream: a call, or one of two gaps.
func (g *generator) entry(k int) ledger.Entry {
	ts := ledger.FormatTime(g.bl.at(k))
	if third := g.bl.callsEnd / 3; k == third || k == 2*third {
		return ledger.Entry{"type": ledger.TypeGap, "ts": ts, "dropped": 3, "since": ts, "until": ts, "cause": "write queue full"}
	}
	return benchCall(k, ts)
}

// benchCall is a call entry with the fields of SPEC 4.2, varied by k.
func benchCall(k int, ts string) ledger.Entry {
	r := benchRoutes[k%len(benchRoutes)]
	models := benchModels[k%len(benchRoutes)]
	sum := sha256.Sum256([]byte(fmt.Sprint(k)))
	out := sha256.Sum256(sum[:])
	return ledger.Entry{"type": ledger.TypeCall, "ts": ts, "provider": r.provider, "api_style": r.provider, "endpoint": r.endpoint,
		"method": "POST", "path": r.path, "model": models[k%len(models)], "stream": k%4 == 0, "status": 200,
		"tokens_in": 200 + k*7919%4000, "tokens_out": 50 + k*31%900, "cached_in": k % 3 * 64, "usage_reported": true,
		"latency_ms": 300 + k%2000, "key_fp": ledger.KeyFingerprint(benchKeys[k%len(benchKeys)]),
		"purpose": benchPurposes[k%len(benchPurposes)], "salt_id": hex.EncodeToString(sum[:16]),
		"prompt_hmac": ledger.FormatHMAC(sum[:]), "output_hmac": ledger.FormatHMAC(out[:]),
		"bytes_in": 1000 + k%9000, "bytes_out": 1500 + k%3000, "client_aborted": false}
}

// heapPeak samples the heap while an operation runs.
type heapPeak struct {
	stop, done chan struct{}
	base, peak uint64
}

func startHeapPeak() *heapPeak {
	runtime.GC()
	h := &heapPeak{stop: make(chan struct{}), done: make(chan struct{}), base: heapInuse()}
	h.peak = h.base
	go h.run()
	return h
}

func heapInuse() uint64 {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return m.HeapInuse
}

func (h *heapPeak) run() {
	defer close(h.done)
	t := time.NewTicker(10 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-h.stop:
			return
		case <-t.C:
			h.peak = max(h.peak, heapInuse())
		}
	}
}

// finish stops sampling and returns the highest heap in use seen, above where it started.
func (h *heapPeak) finish() uint64 {
	close(h.stop)
	<-h.done
	return max(h.peak, heapInuse()) - h.base
}

// reportRate reports entries per second over b.N runs of entries each, and the peak heap.
func reportRate(b *testing.B, entries int, h *heapPeak) {
	b.ReportMetric(float64(entries)*float64(b.N)/b.Elapsed().Seconds(), "entries/s")
	b.ReportMetric(float64(h.finish())/(1<<20), "peak-heap-MB")
}

func openBenchLedger(b *testing.B, bl *benchLedger) *ledger.Store {
	b.Helper()
	s, err := ledger.Open(bl.dir, time.Now)
	if err != nil {
		b.Fatal(err)
	}
	return s
}

func BenchmarkBuild(b *testing.B) {
	for _, n := range benchSizes() {
		for _, sc := range benchScopes {
			b.Run(sizeName(n)+"/"+sc.name, func(b *testing.B) { runBuild(b, n, sc) })
		}
	}
}

func runBuild(b *testing.B, n int, sc scope) {
	bl := benchFixture(b, n)
	s := openBenchLedger(b, bl)
	o := bl.options(sc, filepath.Join(b.TempDir(), "pack.zip"), bl.roots)
	b.ReportAllocs()
	b.ResetTimer()
	h := startHeapPeak()
	for i := 0; i < b.N; i++ {
		if _, err := pack.Build(context.Background(), s, o); err != nil {
			b.Fatal(err)
		}
	}
	b.StopTimer()
	reportRate(b, bl.entries, h)
}

func BenchmarkVerifyPack(b *testing.B) {
	for _, n := range benchSizes() {
		for _, sc := range benchScopes {
			b.Run(sizeName(n)+"/"+sc.name, func(b *testing.B) { runVerifyPack(b, n, sc) })
		}
	}
}

func runVerifyPack(b *testing.B, n int, sc scope) {
	bl := benchFixture(b, n)
	path := filepath.Join(b.TempDir(), "pack.zip")
	m, err := pack.Build(context.Background(), openBenchLedger(b, bl), bl.options(sc, path, bl.roots))
	if err != nil {
		b.Fatal(err)
	}
	vo := pack.VerifyOptions{Roots: bl.roots, Generator: "bench", Clock: time.Now}
	b.ReportAllocs()
	b.ResetTimer()
	h := startHeapPeak()
	for i := 0; i < b.N; i++ {
		r, err := pack.VerifyPack(path, vo)
		if err != nil || !r.OK() {
			b.Fatalf("VerifyPack: %v, %v", err, r.Errors)
		}
	}
	b.StopTimer()
	reportRate(b, int(m.Segment.LastSeq-m.Segment.FirstSeq)+1, h)
}

func BenchmarkVerify(b *testing.B) {
	for _, n := range benchSizes() {
		b.Run(sizeName(n), func(b *testing.B) { runVerify(b, n) })
	}
}

// runVerify does what `ambermoth verify` does: read the file, then verify it.
func runVerify(b *testing.B, n int) {
	bl := benchFixture(b, n)
	opt := ledger.VerifyOptions{AnchorDir: filepath.Join(bl.dir, ledger.AnchorDir), CheckAnchor: ops.AnchorVerifier(bl.roots, false)}
	var read time.Duration
	b.ReportAllocs()
	b.ResetTimer()
	h := startHeapPeak()
	for i := 0; i < b.N; i++ {
		t := time.Now()
		es, lines, err := ledger.ReadFile(bl.ledgerPath())
		read += time.Since(t)
		if err != nil {
			b.Fatal(err)
		}
		if r := ledger.Verify(es, lines, opt); !r.OK() {
			b.Fatalf("Verify: %v", r.Errors)
		}
	}
	b.StopTimer()
	b.ReportMetric(read.Seconds()/float64(b.N), "read-s/op")
	reportRate(b, bl.entries, h)
}
