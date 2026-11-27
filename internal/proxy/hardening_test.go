package proxy_test

import (
	"bufio"
	"context"
	"github.com/devrdias/ambermoth/internal/proxy"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/devrdias/ambermoth/internal/ledger"
)

// A WebSocket (HTTP 101) session through the proxy produces one call entry
// with both directions counted.
func TestWebSocketUpgradeIsRecorded(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, brw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.Close()
		brw.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n")
		brw.Flush()
		buf := make([]byte, 5)
		if _, err := io.ReadFull(brw, buf); err != nil {
			return
		}
		conn.Write([]byte("echo:" + string(buf)))
	}))
	defer up.Close()

	s, _ := ledger.Init(t.TempDir(), "test", time.Now)
	w := proxy.NewWriter(s, 100, discard)
	w.FlushEvery = 5 * time.Millisecond
	go w.Run(context.Background())
	cfg := proxy.DefaultConfig()
	cfg.Upstreams = map[string]proxy.Upstream{"openai": {BaseURL: up.URL, Style: "openai"}}
	p, _ := proxy.New(cfg, s, w, nil, discard)
	srv := httptest.NewServer(p)
	defer srv.Close()

	conn, err := net.Dial("tcp", strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	io.WriteString(conn, "GET /openai/v1/realtime?model=m HTTP/1.1\r\nHost: x\r\nConnection: Upgrade\r\nUpgrade: websocket\r\nAuthorization: Bearer sk-ws\r\n\r\n")
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil || resp.StatusCode != 101 {
		t.Fatalf("no upgrade: %v %v", resp, err)
	}
	io.WriteString(conn, "hello")
	got := make([]byte, 10)
	if _, err := io.ReadFull(br, got); err != nil || string(got) != "echo:hello" {
		t.Fatalf("echo: %q %v", got, err)
	}
	conn.Close()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if len(calls(t, s)) > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	w.Close()
	cs := calls(t, s)
	if len(cs) != 1 {
		t.Fatalf("want 1 call entry for the WebSocket session, got %d", len(cs))
	}
	e := cs[0]
	if e.Str("transport") != "websocket" || e.Str("endpoint") != "realtime" || e.Int("status") != 101 {
		t.Errorf("bad entry: %v", e)
	}
	if e.Int("bytes_in") != 5 || e.Int("bytes_out") != 10 {
		t.Errorf("bytes in/out = %d/%d, want 5/10", e.Int("bytes_in"), e.Int("bytes_out"))
	}
	if e.Str("key_fp") != ledger.KeyFingerprint("sk-ws") {
		t.Errorf("key fingerprint not recorded")
	}
}

// Enqueue racing Close must not panic, and late entries are still written.
func TestEnqueueAfterCloseIsSafe(t *testing.T) {
	s, _ := ledger.Init(t.TempDir(), "test", time.Now)
	w := proxy.NewWriter(s, 4, discard)
	w.FlushEvery = time.Millisecond
	go w.Run(context.Background())
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				w.Enqueue(ledger.Entry{"type": ledger.TypeCall})
			}
		}()
	}
	time.Sleep(time.Millisecond)
	w.Close()
	wg.Wait()
	w.Close() // idempotent
	if !w.Enqueue(ledger.Entry{"type": ledger.TypeCall, "late": true}) {
		t.Fatal("late entry was not written")
	}
	es, lines, err := ledger.ReadFile(s.LedgerPath())
	if err != nil {
		t.Fatal(err)
	}
	if r := ledger.Verify(es, lines, ledger.VerifyOptions{}); !r.OK() {
		t.Fatalf("ledger broken after racing close: %v", r.Errors)
	}
	if !es[len(es)-1].Bool("late") {
		t.Error("late entry missing")
	}
}

func TestNoteIsCapped(t *testing.T) {
	long := strings.Repeat("é", 1000) // 2000 bytes
	got := proxy.TruncateUTF8(long, proxy.MaxNoteBytes)
	if len(got) > proxy.MaxNoteBytes+len("…[truncated]") || !strings.HasSuffix(got, "…[truncated]") {
		t.Fatalf("not capped: %d bytes", len(got))
	}
	if !strings.HasPrefix(got, "é") || strings.ContainsRune(got, '�') {
		t.Error("split a UTF-8 sequence")
	}
	if proxy.TruncateUTF8("short", proxy.MaxNoteBytes) != "short" {
		t.Error("short note changed")
	}
}

// A dropped auto_enroll is retried on the next call with that key.
func TestAutoEnrollRetriedAfterDrop(t *testing.T) {
	s, _ := ledger.Init(t.TempDir(), "test", time.Now)
	w := proxy.NewWriter(s, 1, discard)
	cfg := proxy.DefaultConfig()
	cfg.AutoEnroll = true
	p, _ := proxy.New(cfg, s, w, nil, discard)
	w.Enqueue(ledger.Entry{"type": ledger.TypeCall}) // fill the queue
	p.MaybeEnroll("sha256:k", "openai")              // dropped
	go w.Run(context.Background())
	time.Sleep(20 * time.Millisecond)
	p.MaybeEnroll("sha256:k", "openai") // retried
	p.MaybeEnroll("sha256:k", "openai") // now known: no third entry
	w.Close()
	es, _ := s.Entries()
	n := 0
	for _, e := range es {
		if e.Type() == ledger.TypeKeyEvent && e.Str("key_fp") == "sha256:k" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("want exactly 1 auto_enroll entry, got %d", n)
	}
}

// holdConn is an upstream connection whose Write has delivered its bytes but
// has not returned yet, the state a real conn is in when the peer answers
// before the writer goroutine is scheduled again.
type holdConn struct {
	entered  chan struct{} // closed once Write has taken the bytes
	release  chan struct{} // closed by Close; lets Write return
	returned chan struct{} // closed just before Write returns
	once     sync.Once
}

func (h *holdConn) Read([]byte) (int, error) {
	<-h.release
	return 0, io.EOF
}

func (h *holdConn) Write(b []byte) (int, error) {
	close(h.entered)
	<-h.release
	close(h.returned)
	time.Sleep(20 * time.Millisecond) // the writer goroutine is descheduled here
	return len(b), nil
}

// Close frees Write and waits until it is on its way out, so Close always
// finishes while Write is still short of returning.
func (h *holdConn) Close() error {
	h.once.Do(func() { close(h.release) })
	<-h.returned
	return nil
}

type upgradeTransport struct{ body io.ReadCloser }

func (u upgradeTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	h := http.Header{}
	h.Set("Connection", "Upgrade")
	h.Set("Upgrade", "websocket")
	return &http.Response{StatusCode: http.StatusSwitchingProtocols, Status: "101 Switching Protocols",
		Proto: "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1, Header: h, Body: u.body, Request: r}, nil
}

// A session closed while a client write is still returning from the upstream
// conn records that write.
func TestWebSocketCloseCountsWriteInFlight(t *testing.T) {
	hc := &holdConn{entered: make(chan struct{}), release: make(chan struct{}), returned: make(chan struct{})}
	s, _ := ledger.Init(t.TempDir(), "test", time.Now)
	w := proxy.NewWriter(s, 100, discard)
	w.FlushEvery = 5 * time.Millisecond
	go func() { _ = w.Run(context.Background()) }()
	cfg := proxy.DefaultConfig()
	cfg.Upstreams = map[string]proxy.Upstream{"openai": {BaseURL: "http://upstream.invalid", Style: "openai"}}
	p, _ := proxy.New(cfg, s, w, upgradeTransport{body: hc}, discard)
	srv := httptest.NewServer(p)
	defer srv.Close()

	conn, err := net.Dial("tcp", strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	io.WriteString(conn, "GET /openai/v1/realtime HTTP/1.1\r\nHost: x\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n\r\n")
	br := bufio.NewReader(conn)
	if resp, err := http.ReadResponse(br, nil); err != nil || resp.StatusCode != 101 {
		t.Fatalf("no upgrade: %v %v", resp, err)
	}
	io.WriteString(conn, "hello")
	select {
	case <-hc.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("upstream never saw the write")
	}
	p.CloseUpgrades()
	w.Close()

	cs := calls(t, s)
	if len(cs) != 1 {
		t.Fatalf("want 1 call entry, got %d", len(cs))
	}
	if got := cs[0].Int("bytes_in"); got != 5 {
		t.Errorf("bytes_in = %d, want 5", got)
	}
}
