package proxy_test

import (
	"bufio"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// The proxy must pass stream chunks on as they arrive: the first one has to
// reach the client while the upstream is still holding back the rest.
func TestStreamIsNotBuffered(t *testing.T) {
	release := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	up := httptest.NewServer(streamUpstream(func() { <-release }))
	t.Cleanup(up.Close)
	rg := newBenchRig(t, up.URL)
	t.Cleanup(unblock) // runs first, so the upstream handler can finish

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, rg.url+"/openai"+benchPath, strings.NewReader(string(requestBody(1<<10, true))))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := benchClient(t).Do(req)
	if err != nil {
		t.Fatalf("no response while the upstream holds back the stream: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	r := bufio.NewReader(resp.Body)
	first, err := r.ReadString('\n')
	if err != nil || !strings.HasPrefix(first, "data: ") {
		t.Fatalf("first chunk = %q, %v; want it before the upstream sent the last", first, err)
	}
	unblock()
	rest, err := io.ReadAll(r)
	if err != nil || !strings.Contains(string(rest), "[DONE]") {
		t.Fatalf("rest of the stream: %v, %d bytes, no [DONE]", err, len(rest))
	}
}

// Overhead is far below its 20 ms target on any machine (about 0.3 ms here),
// so the median of 200 calls against that bound only fails on a gross
// regression. A median is steady on a shared runner, and the test takes about
// 0.1 s. It is skipped under -race, which slows the proxy several times.
func TestOverheadStaysUnderTarget(t *testing.T) {
	if raceEnabled || testing.Short() {
		t.Skip("timing test: skipped with -race and -short")
	}
	const calls, target = 200, 20 * time.Millisecond
	up := httptest.NewServer(chatUpstream())
	t.Cleanup(up.Close)
	rg := newBenchRig(t, up.URL)
	c, body := benchClient(t), requestBody(1<<10, false)
	direct := medianLatency(t, c, up.URL+benchPath, body, calls)
	proxied := medianLatency(t, c, rg.url+"/openai"+benchPath, body, calls)
	if over := proxied - direct; over > target {
		t.Fatalf("median overhead %v over %d calls (direct %v, proxied %v); target is under %v", over, calls, direct, proxied, target)
	}
}

// medianLatency makes n sequential calls, after one to open the connection.
func medianLatency(t *testing.T, c *http.Client, url string, body []byte, n int) time.Duration {
	t.Helper()
	if _, err := timedCall(c, url, body); err != nil {
		t.Fatal(err)
	}
	d := make([]time.Duration, n)
	for i := range d {
		ti, err := timedCall(c, url, body)
		if err != nil {
			t.Fatal(err)
		}
		d[i] = ti.total
	}
	sort.Slice(d, func(i, j int) bool { return d[i] < d[j] })
	return d[n/2]
}
