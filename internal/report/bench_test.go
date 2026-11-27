package report_test

import (
	"fmt"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/devrdias/ambermoth/internal/ledger"
	"github.com/devrdias/ambermoth/internal/report"
)

const benchEntries = 200000

// benchEntry is the i-th entry of a generated ledger: mostly calls over 120
// days, an amendment every 5,000 entries, a key event every 20,000.
func benchEntry(i int) ledger.Entry {
	ts := ledger.FormatTime(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(i) * 50 * time.Second))
	seq := int64(i + 1)
	switch {
	case i%20000 == 19999:
		return ledger.Entry{"seq": seq, "type": "key_event", "ts": ts, "provider": "openai", "key_fp": fmt.Sprint("sha256:k", i%3), "action": "enroll"}
	case i%5000 == 4999:
		return ledger.Entry{"seq": seq, "type": "amendment", "ts": ts, "target_from": seq - 400, "target_to": seq - 1, "new_purpose": "eval", "reason": "r", "by": "b"}
	}
	return ledger.Entry{"seq": seq, "type": "call", "ts": ts, "v": int64(1), "provider": []string{"openai", "anthropic"}[i%2],
		"purpose": []string{"untagged", "prod", "support"}[i%3], "key_fp": fmt.Sprint("sha256:k", i%3),
		"endpoint": []string{"chat", "embed"}[i%2], "model": fmt.Sprint("model-", i%5), "status": int64(200 + 229*int64(i%17/16)),
		"tokens_in": int64(i % 900), "tokens_out": int64(i % 300), "cached_in": int64(i % 40),
		"prompt_hmac": fmt.Sprintf("hmac-sha256:%064x", i), "output_hmac": fmt.Sprintf("hmac-sha256:%064x", i+7),
		"request_id": fmt.Sprintf("req_%024d", i), "prev_hash": fmt.Sprintf("sha256:%064x", i), "entry_hash": fmt.Sprintf("sha256:%064x", i+1)}
}

// peakHeap runs fn and returns the largest live heap seen, sampled every
// millisecond.
func peakHeap(fn func()) uint64 {
	runtime.GC()
	var peak uint64
	var mu sync.Mutex
	stop, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		tk := time.NewTicker(time.Millisecond)
		defer tk.Stop()
		var m runtime.MemStats
		for {
			select {
			case <-stop:
				return
			case <-tk.C:
				runtime.ReadMemStats(&m)
				mu.Lock()
				peak = max(peak, m.HeapAlloc)
				mu.Unlock()
			}
		}
	}()
	fn()
	close(stop)
	<-done
	mu.Lock()
	defer mu.Unlock()
	return peak
}

func benchFilter() report.Filter { return report.Filter{Provider: "openai"} }

// BenchmarkSummarize compares the whole-slice reference with the accumulator
// on 200,000 entries. Both generate the entries inside the timed run; the
// slice form has to hold them all, the accumulator drops each after Add.
func BenchmarkSummarize(b *testing.B) {
	b.Run("slice", func(b *testing.B) {
		b.ReportAllocs()
		var peak uint64
		for i := 0; i < b.N; i++ {
			p := peakHeap(func() {
				es := make([]ledger.Entry, benchEntries)
				for j := range es {
					es[j] = benchEntry(j)
				}
				refSummarize(es, benchFilter())
			})
			peak = max(peak, p)
		}
		b.ReportMetric(float64(peak)/(1<<20), "peak-MB")
	})
	b.Run("slice-wrapper", func(b *testing.B) {
		b.ReportAllocs()
		var peak uint64
		for i := 0; i < b.N; i++ {
			p := peakHeap(func() {
				es := make([]ledger.Entry, benchEntries)
				for j := range es {
					es[j] = benchEntry(j)
				}
				report.Summarize(es, benchFilter())
			})
			peak = max(peak, p)
		}
		b.ReportMetric(float64(peak)/(1<<20), "peak-MB")
	})
	b.Run("stream", func(b *testing.B) {
		b.ReportAllocs()
		var peak uint64
		for i := 0; i < b.N; i++ {
			p := peakHeap(func() {
				z := report.NewSummarizer(benchFilter())
				for j := 0; j < benchEntries; j++ {
					z.Add(benchEntry(j))
				}
				z.Summary()
			})
			peak = max(peak, p)
		}
		b.ReportMetric(float64(peak)/(1<<20), "peak-MB")
	})
}
