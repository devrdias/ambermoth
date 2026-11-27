package ledger_test

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/devrdias/ambermoth/internal/canon"
	"github.com/devrdias/ambermoth/internal/ledger"
)

// chainReader yields a valid ledger of n entries one line at a time, so the
// input itself takes no memory. Each call entry carries pad bytes of text.
type chainReader struct {
	n, pad int
	next   int
	prev   string
	buf    []byte
}

func newChainReader(n, pad int) *chainReader { return &chainReader{n: n, pad: pad} }

func (c *chainReader) Read(p []byte) (int, error) {
	for len(c.buf) == 0 {
		if c.next >= c.n {
			return 0, io.EOF
		}
		c.buf = c.line()
	}
	k := copy(p, c.buf)
	c.buf = c.buf[k:]
	return k, nil
}

func (c *chainReader) line() []byte {
	i := c.next
	e := ledger.Entry{"v": ledger.FormatVersion, "seq": i, "ts": ledger.FormatTime(epoch.Add(time.Duration(i) * time.Second)),
		"type": ledger.TypeCall, "prev_hash": ledger.ZeroHash}
	if i == 0 {
		e["type"], e["ledger_id"] = ledger.TypeGenesis, "00000000-0000-4000-8000-000000000001"
	} else {
		e["prev_hash"] = c.prev
		e["provider"], e["model"] = "openai", "gpt-test"
		e["note"] = strings.Repeat("x", c.pad)
	}
	h, err := ledger.ComputeHash(e)
	if err != nil {
		panic(err)
	}
	e["entry_hash"] = h
	b, err := canon.Marshal(map[string]any(e))
	if err != nil {
		panic(err)
	}
	c.next, c.prev = i+1, h
	return append(b, '\n')
}

func TestVerifyReaderNamesTheLineThatIsTooLong(t *testing.T) {
	t.Parallel()
	in := append([]byte("{\"a\":1}\n\n"), bytes.Repeat([]byte("a"), ledger.MaxLineBytes+1)...)
	r, err := ledger.VerifyReader(bytes.NewReader(in), ledger.VerifyOptions{})
	if err == nil || err.Error() != "line 3 is longer than 16 MiB" {
		t.Fatalf("error = %v, want line 3 is longer than 16 MiB", err)
	}
	if r != nil {
		t.Errorf("got a report with the error: %+v", r)
	}
}

var errDisk = errors.New("disk failed")

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errDisk }

func TestVerifyReaderReturnsReadErrors(t *testing.T) {
	t.Parallel()
	if _, err := ledger.VerifyReader(failingReader{}, ledger.VerifyOptions{}); !errors.Is(err, errDisk) {
		t.Errorf("error = %v, want %v", err, errDisk)
	}
}

func TestVerifyReaderMatchesVerifyOnGeneratedLedger(t *testing.T) {
	t.Parallel()
	data, err := io.ReadAll(newChainReader(500, 100))
	if err != nil {
		t.Fatal(err)
	}
	es, lines, err := ledger.Read(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	got, err := ledger.VerifyReader(bytes.NewReader(data), ledger.VerifyOptions{})
	if err != nil {
		t.Fatal(err)
	}
	want := ledger.Verify(es, lines, ledger.VerifyOptions{})
	if !want.OK() || got.Entries != 500 {
		t.Fatalf("generated ledger: ok=%v entries=%d", want.OK(), got.Entries)
	}
	if g, w := projectReport(t, got), projectReport(t, want); !bytes.Equal(g, w) {
		t.Errorf("reports differ\n%s", firstDifference(g, w))
	}
}

// peakHeap runs fn and returns the highest heap in use seen while it ran,
// sampled every millisecond, above the heap before it.
func peakHeap(fn func()) uint64 {
	runtime.GC()
	var base runtime.MemStats
	runtime.ReadMemStats(&base)
	var peak atomic.Uint64
	stop, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		tick := time.NewTicker(time.Millisecond)
		defer tick.Stop()
		var m runtime.MemStats
		for {
			select {
			case <-stop:
				return
			case <-tick.C:
				runtime.ReadMemStats(&m)
				if m.HeapInuse > peak.Load() {
					peak.Store(m.HeapInuse)
				}
			}
		}
	}()
	fn()
	close(stop)
	<-done
	if p := peak.Load(); p > base.HeapInuse {
		return p - base.HeapInuse
	}
	return 0
}

// Memory is bound by the largest entry, not by the ledger: 16 entries of
// 1 MB each are 16 MB of input. Reading them all takes over 50 MB.
func TestVerifyReaderMemoryDoesNotGrowWithEntrySize(t *testing.T) {
	const limit = 40 << 20
	var rep *ledger.Report
	peak := peakHeap(func() {
		var err error
		rep, err = ledger.VerifyReader(newChainReader(16, 1<<20), ledger.VerifyOptions{})
		if err != nil {
			t.Error(err)
		}
	})
	if rep == nil || !rep.OK() || rep.Entries != 16 {
		t.Fatalf("report = %+v", rep)
	}
	t.Logf("peak %d MB", peak>>20)
	if peak > limit {
		t.Errorf("peak heap %d MB for 16 MB of input, want under %d MB", peak>>20, limit>>20)
	}
}

// benchFile writes a generated ledger of n entries to a temp file, so the
// benchmarks read it as the verify command does and the heap holds none of it.
func benchFile(b *testing.B, n int) string {
	b.Helper()
	path := filepath.Join(b.TempDir(), "ledger.jsonl")
	f, err := os.Create(path)
	if err != nil {
		b.Fatal(err)
	}
	if _, err := io.Copy(f, newChainReader(n, 500)); err != nil {
		b.Fatal(err)
	}
	if err := f.Close(); err != nil {
		b.Fatal(err)
	}
	return path
}

var benchSizes = []int{50_000, 100_000, 200_000}

func benchName(n int) string { return fmt.Sprintf("%dk", n/1000) }

// runBenchmark times verify over a generated ledger file of each size and
// reports the highest peak heap over the runs.
func runBenchmark(b *testing.B, verify func(f *os.File) (*ledger.Report, error)) {
	for _, n := range benchSizes {
		b.Run(benchName(n), func(b *testing.B) {
			path := benchFile(b, n)
			var peak uint64
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				f, err := os.Open(path)
				if err != nil {
					b.Fatal(err)
				}
				var r *ledger.Report
				p := peakHeap(func() { r, err = verify(f) })
				_ = f.Close()
				if err != nil || !r.OK() || r.Entries != n {
					b.Fatalf("verify: %v, ok=%v entries=%d", err, r.OK(), r.Entries)
				}
				peak = max(peak, p)
			}
			b.ReportMetric(float64(peak)/(1<<20), "peak-heap-MB")
		})
	}
}

// BenchmarkVerify is the old path: read every entry, then verify.
func BenchmarkVerify(b *testing.B) {
	runBenchmark(b, func(f *os.File) (*ledger.Report, error) {
		es, lines, err := ledger.Read(f)
		if err != nil {
			return nil, err
		}
		return ledger.Verify(es, lines, ledger.VerifyOptions{}), nil
	})
}

func BenchmarkVerifyReader(b *testing.B) {
	runBenchmark(b, func(f *os.File) (*ledger.Report, error) {
		return ledger.VerifyReader(f, ledger.VerifyOptions{})
	})
}
