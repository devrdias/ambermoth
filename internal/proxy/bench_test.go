package proxy_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/devrdias/ambermoth/internal/ledger"
	"github.com/devrdias/ambermoth/internal/proxy"
)

// The overhead of the proxy is the latency of BenchmarkProxied minus that of
// BenchmarkDirect for the same body size and mode. Both go through the same
// kind of client and the same upstream; only the proxy and its ledger differ.

const (
	benchPath    = "/v1/chat/completions"
	benchWriterQ = 10000 // the queue size the CLI uses
	streamEvents = 50
)

var benchSizes = []int{1 << 10, 16 << 10, 256 << 10}

func BenchmarkDirect(b *testing.B)  { benchAll(b, scenario{}) }
func BenchmarkProxied(b *testing.B) { benchAll(b, scenario{proxied: true}) }

// The stream benchmarks report the time to the first byte (ttfb-*) next to the total.
func BenchmarkStreamDirect(b *testing.B)  { benchAll(b, scenario{stream: true}) }
func BenchmarkStreamProxied(b *testing.B) { benchAll(b, scenario{proxied: true, stream: true}) }

// The write-ahead benchmarks are the proxied ones with write_ahead on: one fsync
// in the request path, so the figure is the disk's. Compare with Proxied.
func BenchmarkProxiedWriteAhead(b *testing.B) {
	benchAll(b, scenario{proxied: true, writeAhead: true})
}
func BenchmarkStreamProxiedWriteAhead(b *testing.B) {
	benchAll(b, scenario{proxied: true, stream: true, writeAhead: true})
}

// scenario is one benchmark: a body size, with or without the proxy, streaming
// or not, and with write_ahead on or off. benchAll sets the size.
type scenario struct {
	size                        int
	proxied, stream, writeAhead bool
}

func benchAll(b *testing.B, base scenario) {
	sizes, modes := benchSizes, []bool{false, true}
	if base.stream {
		sizes, modes = sizes[:1], modes[:1]
	}
	for _, size := range sizes {
		for _, parallel := range modes {
			sc := base
			sc.size = size
			b.Run(fmt.Sprintf("%dKB/%s", size>>10, modeName(parallel)), func(b *testing.B) { benchRun(b, sc, parallel) })
		}
	}
}

func modeName(parallel bool) string {
	if parallel {
		return "parallel"
	}
	return "sequential"
}

func benchRun(b *testing.B, sc scenario, parallel bool) {
	up := httptest.NewServer(sc.upstream())
	b.Cleanup(up.Close)
	url, rg := up.URL+benchPath, (*benchRig)(nil)
	if sc.proxied {
		rg = newBenchRig(b, up.URL, sc.writeAhead)
		url = rg.url + "/openai" + benchPath
	}
	c, body := benchClient(b), requestBody(sc.size, sc.stream)
	if _, err := timedCall(c, url, body); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	ts, failed := runCalls(b, parallel, func() (timing, error) { return timedCall(c, url, body) })
	b.StopTimer()
	if len(ts) == 0 {
		b.Fatalf("all %d calls failed", failed)
	}
	reportTimings(b, ts, sc.stream)
	b.ReportMetric(float64(failed), "failed")
	if rg != nil {
		b.ReportMetric(float64(rg.dropped(b, len(ts)+int(failed)+1)), "dropped")
	}
}

func (sc scenario) upstream() http.Handler {
	if sc.stream {
		return streamUpstream(nil)
	}
	return chatUpstream()
}

// runCalls makes b.N calls, one after the other or from GOMAXPROCS goroutines.
// A call that fails is counted and left out of the timings.
func runCalls(b *testing.B, parallel bool, call func() (timing, error)) ([]timing, int64) {
	var failed atomic.Int64
	work := func(next func() bool, into []timing) []timing {
		for next() {
			t, err := call()
			if err != nil {
				failed.Add(1)
				continue
			}
			into = append(into, t)
		}
		return into
	}
	if !parallel {
		i := 0
		out := work(func() bool { i++; return i <= b.N }, make([]timing, 0, b.N))
		return out, failed.Load()
	}
	var mu sync.Mutex
	out := make([]timing, 0, b.N)
	b.RunParallel(func(pb *testing.PB) {
		local := work(pb.Next, make([]timing, 0, 1024))
		mu.Lock()
		out = append(out, local...)
		mu.Unlock()
	})
	return out, failed.Load()
}

// timing is how long one call took to the first response byte and to the last.
type timing struct{ first, total time.Duration }

// timedCall posts body to url and reads the whole response.
func timedCall(c *http.Client, url string, body []byte) (timing, error) {
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return timing{}, err
	}
	req.Header.Set("Authorization", "Bearer sk-bench")
	req.Header.Set("Content-Type", "application/json")
	start := time.Now()
	resp, err := c.Do(req)
	if err != nil {
		return timing{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return timing{}, fmt.Errorf("status %d", resp.StatusCode)
	}
	var one [1]byte
	if _, err := io.ReadFull(resp.Body, one[:]); err != nil {
		return timing{}, err
	}
	first := time.Since(start)
	if _, err := io.Copy(io.Discard, resp.Body); err != nil {
		return timing{}, err
	}
	return timing{first: first, total: time.Since(start)}, nil
}

func reportTimings(b *testing.B, ts []timing, stream bool) {
	totals := make([]time.Duration, len(ts))
	firsts := make([]time.Duration, len(ts))
	for i, t := range ts {
		totals[i], firsts[i] = t.total, t.first
	}
	reportPercentiles(b, "", totals)
	if stream {
		reportPercentiles(b, "ttfb-", firsts)
	}
}

func reportPercentiles(b *testing.B, prefix string, d []time.Duration) {
	sort.Slice(d, func(i, j int) bool { return d[i] < d[j] })
	ranks := []int{50, 95, 99}
	for _, p := range ranks {
		b.ReportMetric(float64(percentile(d, p))/float64(time.Millisecond), fmt.Sprintf("%sp%d-ms", prefix, p))
	}
}

// percentile returns the p-th percentile of sorted d, by the nearest-rank method.
func percentile(d []time.Duration, p int) time.Duration {
	if len(d) == 0 {
		return 0
	}
	rank := int(math.Ceil(float64(p) / 100 * float64(len(d))))
	return d[max(rank, 1)-1]
}

// benchClient is a keep-alive client with room for the parallel runs.
func benchClient(tb testing.TB) *http.Client {
	tr := &http.Transport{MaxIdleConns: 256, MaxIdleConnsPerHost: 256, DisableCompression: true}
	tb.Cleanup(tr.CloseIdleConnections)
	return &http.Client{Transport: tr}
}

// benchRig is a proxy with a real ledger on disk and the writer set up as the CLI does.
type benchRig struct {
	url   string
	store *ledger.Store
	w     *proxy.Writer
}

func newBenchRig(tb testing.TB, upstream string, writeAhead bool) *benchRig {
	tb.Helper()
	s, err := ledger.Init(tb.TempDir(), "bench", time.Now)
	if err != nil {
		tb.Fatal(err)
	}
	w := proxy.NewWriter(s, benchWriterQ, discard)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = w.Run(context.Background())
	}()
	cfg := proxy.DefaultConfig()
	cfg.WriteAhead = writeAhead
	cfg.Upstreams = map[string]proxy.Upstream{"openai": {BaseURL: upstream, Style: "openai"}}
	p, err := proxy.New(cfg, s, w, nil, discard)
	if err != nil {
		tb.Fatal(err)
	}
	srv := httptest.NewServer(p)
	tb.Cleanup(func() {
		srv.Close()
		w.Close()
		<-done
	})
	return &benchRig{url: srv.URL, store: s, w: w}
}

// dropped drains the writer and returns how many of the n calls made (the
// warm-up included) did not reach the ledger.
func (r *benchRig) dropped(tb testing.TB, n int) int {
	tb.Helper()
	r.w.Close()
	es, err := r.store.Entries()
	if err != nil {
		tb.Fatal(err)
	}
	recorded := 0
	for _, e := range es {
		if e.Type() == ledger.TypeCall {
			recorded++
		}
	}
	return n - recorded
}

// requestBody is a chat request of exactly size bytes.
func requestBody(size int, stream bool) []byte {
	head := `{"model":"gpt-bench","messages":[{"role":"user","content":"`
	if stream {
		head = `{"model":"gpt-bench","stream":true,"messages":[{"role":"user","content":"`
	}
	const tail = `"}]}`
	return []byte(head + strings.Repeat("x", size-len(head)-len(tail)) + tail)
}

// chatUpstream answers every request at once with a chat completion of about 1.5 KB.
func chatUpstream() http.Handler {
	text := strings.Repeat("The ledger records what was sent and what came back. ", 24)
	reply := `{"id":"chatcmpl-bench","object":"chat.completion","created":1760000000,"model":"gpt-bench",` +
		`"choices":[{"index":0,"message":{"role":"assistant","content":"` + text + `"},"finish_reason":"stop"}],` +
		`"usage":{"prompt_tokens":120,"completion_tokens":310,"total_tokens":430,"prompt_tokens_details":{"cached_tokens":0}}}`
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, reply)
	})
}

// streamUpstream sends an SSE response of about 50 chunks, flushing after each.
// afterFirst, if set, runs once the first chunk is out.
func streamUpstream(afterFirst func()) http.Handler {
	events := make([]string, 0, streamEvents+2)
	for i := 0; i < streamEvents; i++ {
		events = append(events, fmt.Sprintf(`data: {"id":"c","model":"gpt-bench","choices":[{"index":%d,"delta":{"content":"token "}}]}`+"\n\n", i))
	}
	events = append(events, `data: {"choices":[],"usage":{"prompt_tokens":120,"completion_tokens":50}}`+"\n\n", "data: [DONE]\n\n")
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		for i, ev := range events {
			_, _ = io.WriteString(w, ev)
			w.(http.Flusher).Flush()
			if i == 0 && afterFirst != nil {
				afterFirst()
			}
		}
	})
}
