package proxy_test

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
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

const refusedBody = `{"error":"ambermoth: write-ahead failed, call not sent"}`

var errFsync = errors.New("fsync failed")

// hookStore is the ledger as the writer sees it, with appends that can be
// slowed and failed, and that are counted. Every Store.Append is one fsync.
type hookStore struct {
	*ledger.Store
	delay time.Duration
	fail  func(entries []ledger.Entry) error

	mu      sync.Mutex
	appends [][]string // the entry types of each append that went through
}

func (h *hookStore) Append(entries ...ledger.Entry) error {
	time.Sleep(h.delay)
	if h.fail != nil {
		if err := h.fail(entries); err != nil {
			return err
		}
	}
	types := make([]string, len(entries))
	for i, e := range entries {
		types[i] = e.Type()
	}
	h.mu.Lock()
	h.appends = append(h.appends, types)
	h.mu.Unlock()
	return h.Store.Append(entries...)
}

// appendsWith counts the appends that held an entry of the type.
func (h *hookStore) appendsWith(typ string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for _, types := range h.appends {
		for _, t := range types {
			if t == typ {
				n++
				break
			}
		}
	}
	return n
}

func failOn(typ string) func([]ledger.Entry) error {
	return func(entries []ledger.Entry) error {
		for _, e := range entries {
			if e.Type() == typ {
				return errFsync
			}
		}
		return nil
	}
}

// waRig is a proxy with write_ahead on in front of a counting upstream.
type waRig struct {
	t        *testing.T
	store    *ledger.Store
	hook     *hookStore
	w        *proxy.Writer
	srv      *httptest.Server
	proxy    *proxy.Proxy
	us       *httptest.Server
	upstream atomic.Int64 // requests that reached the upstream
}

type waOpts struct {
	queue      int
	noRun      bool // the writer is never started: nothing is ever written
	ackTimeout time.Duration
	flush      time.Duration // the flush interval, 5 ms if zero
	hook       func(*hookStore)
	writeAhead bool // set by newPlainRig to leave it off
}

func newWARig(t *testing.T, up http.Handler, o waOpts) *waRig {
	t.Helper()
	r := &waRig{t: t}
	us := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.upstream.Add(1)
		up.ServeHTTP(w, req)
	}))
	t.Cleanup(us.Close)
	r.us = us
	s, err := ledger.Init(t.TempDir(), "test", time.Now)
	if err != nil {
		t.Fatal(err)
	}
	r.store, r.hook = s, &hookStore{Store: s}
	if o.hook != nil {
		o.hook(r.hook)
	}
	if o.queue == 0 {
		o.queue = 1000
	}
	r.w = proxy.NewWriter(r.hook, o.queue, discard)
	r.w.FlushEvery = 5 * time.Millisecond
	if o.flush > 0 {
		r.w.FlushEvery = o.flush
	}
	if o.ackTimeout > 0 {
		r.w.AckTimeout = o.ackTimeout
	}
	if !o.noRun {
		go func() { _ = r.w.Run(context.Background()) }()
		t.Cleanup(r.w.Close)
	}
	cfg := proxy.DefaultConfig()
	cfg.WriteAhead = o.writeAhead
	cfg.Upstreams = map[string]proxy.Upstream{
		"openai":    {BaseURL: us.URL, Style: "openai"},
		"anthropic": {BaseURL: us.URL, Style: "anthropic"},
	}
	r.proxy, err = proxy.New(cfg, s, r.w, nil, discard)
	if err != nil {
		t.Fatal(err)
	}
	r.srv = httptest.NewServer(r.proxy)
	t.Cleanup(r.srv.Close)
	return r
}

func newWriteAheadRig(t *testing.T, up http.Handler, o waOpts) *waRig {
	o.writeAhead = true
	return newWARig(t, up, o)
}

// post sends a chat request and returns the status and body.
func (r *waRig) post(body string) (int, string) {
	r.t.Helper()
	req, err := http.NewRequest("POST", r.srv.URL+"/openai/v1/chat/completions", strings.NewReader(body))
	if err != nil {
		r.t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer sk-wa")
	req.Header.Set("X-Provenance-Purpose", "eval-regression")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		r.t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	return resp.StatusCode, string(b)
}

func (r *waRig) asRig() *rig { return &rig{t: r.t, store: r.store, w: r.w, srv: r.srv} }

// ledgerEntries drains the writer and returns every entry of the ledger.
func (r *waRig) ledgerEntries() []ledger.Entry {
	r.t.Helper()
	r.w.Close()
	es, err := r.store.Entries()
	if err != nil {
		r.t.Fatal(err)
	}
	return es
}

func ofType(es []ledger.Entry, typ string) []ledger.Entry {
	var out []ledger.Entry
	for _, e := range es {
		if e.Type() == typ {
			out = append(out, e)
		}
	}
	return out
}

func sortedKeys(e ledger.Entry) string {
	keys := make([]string, 0, len(e))
	for k := range e {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return strings.Join(keys, ",")
}

func hasType(es []ledger.Entry, typ string) bool { return len(ofType(es, typ)) > 0 }

func mustVerify(t *testing.T, s *ledger.Store) *ledger.Report {
	t.Helper()
	r := verifyStore(t, s)
	if !r.OK() {
		t.Fatalf("ledger does not verify: %v", r.Errors)
	}
	return r
}

// The intent is on disk before the request reaches the upstream, carries what
// the ADR lists and nothing of the body, and the call entry completes it.
func TestWriteAheadIntentIsOnDiskBeforeTheForward(t *testing.T) {
	var seen []ledger.Entry
	var r *waRig
	r = newWriteAheadRig(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		es, err := r.store.Entries()
		if err != nil {
			t.Error(err)
		}
		seen = ofType(es, ledger.TypeIntent)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"model":"m","usage":{"prompt_tokens":1,"completion_tokens":2}}`)
	}), waOpts{})
	body := `{"model":"m","messages":[]}`

	if status, _ := r.post(body); status != 200 {
		t.Fatalf("status %d", status)
	}

	if len(seen) != 1 {
		t.Fatalf("%d intents on disk when the upstream was reached, want 1", len(seen))
	}
	es := r.ledgerEntries()
	intent, call := ofType(es, ledger.TypeIntent)[0], ofType(es, ledger.TypeCall)[0]
	wantKeys := "call_id,api_style,declared_bytes_in,endpoint,entry_hash,key_fp,method,path,prev_hash,provider,purpose,salt_id,seq,ts,type,v"
	if got := sortedKeys(intent); got != sortedKeysOf(wantKeys) {
		t.Errorf("intent fields\n got %s\nwant %s", got, sortedKeysOf(wantKeys))
	}
	if len(intent.Str("call_id")) != 32 || intent.Int("declared_bytes_in") != int64(len(body)) {
		t.Errorf("call_id %q declared_bytes_in %d", intent.Str("call_id"), intent.Int("declared_bytes_in"))
	}
	shared := []string{"provider", "api_style", "endpoint", "method", "path", "key_fp", "purpose", "salt_id", "ts"}
	for _, k := range shared {
		if intent.Str(k) != call.Str(k) || intent.Str(k) == "" {
			t.Errorf("%s: intent %q, call %q", k, intent.Str(k), call.Str(k))
		}
	}
	if call.Str("call_id") != intent.Str("call_id") || call.Int("intent_seq") != intent.Seq() || intent.Seq() >= call.Seq() {
		t.Errorf("call does not complete the intent: call_id %q intent_seq %d (intent seq %d)", call.Str("call_id"), call.Int("intent_seq"), intent.Seq())
	}
	if rep := mustVerify(t, r.store); rep.OrphanIntents != 0 {
		t.Errorf("%d orphans after a completed call", rep.OrphanIntents)
	}
}

func sortedKeysOf(csv string) string {
	keys := strings.Split(csv, ",")
	sort.Strings(keys)
	return strings.Join(keys, ",")
}

func TestWriteAheadIntentHasNoDeclaredBytesWithoutContentLength(t *testing.T) {
	r := newWriteAheadRig(t, jsonUpstream(), waOpts{})
	req, _ := http.NewRequest("POST", r.srv.URL+"/openai/v1/chat/completions", io.NopCloser(strings.NewReader(`{"model":"m"}`)))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()

	intent := ofType(r.ledgerEntries(), ledger.TypeIntent)[0]
	if _, ok := intent["declared_bytes_in"]; ok {
		t.Errorf("declared_bytes_in set for a chunked request: %v", intent)
	}
}

// With the mode off the call entry is what it was, and there is no intent.
func TestWriteAheadOffRecordsTheCallAsBefore(t *testing.T) {
	r := newWARig(t, jsonUpstream(), waOpts{})
	if status, _ := r.post(`{"model":"m"}`); status != 200 {
		t.Fatalf("status %d", status)
	}
	es := r.ledgerEntries()
	if hasType(es, ledger.TypeIntent) {
		t.Error("an intent entry with write_ahead off")
	}
	want := "api_style,bytes_in,bytes_out,cached_in,client_aborted,endpoint,entry_hash,key_fp,latency_ms,method,model,output_hmac,path," +
		"prev_hash,prompt_hmac,provider,purpose,salt_id,seq,status,stream,tokens_in,tokens_out,ts,type,usage_reported,v"
	if got := sortedKeys(ofType(es, ledger.TypeCall)[0]); got != sortedKeysOf(want) {
		t.Errorf("call fields\n got %s\nwant %s", got, sortedKeysOf(want))
	}
	if r.hook.appendsWith(ledger.TypeIntent) != 0 {
		t.Error("the writer was asked for an intent")
	}
}

// When the intent cannot be written the call is not sent, and nothing about
// it is recorded as lost.
func TestWriteAheadFailsClosed(t *testing.T) {
	cases := []struct {
		name string
		opts waOpts
	}{
		{"the append fails", waOpts{hook: func(h *hookStore) { h.fail = failOn(ledger.TypeIntent) }}},
		{"the queue is full", waOpts{queue: 1, noRun: true}},
		{"no answer in time", waOpts{noRun: true, ackTimeout: 30 * time.Millisecond}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := newWriteAheadRig(t, jsonUpstream(), c.opts)
			if c.opts.queue == 1 {
				if !r.w.Enqueue(goodCall(1)) {
					t.Fatal("setup: queue full already")
				}
			}
			start := time.Now()
			status, body := r.post(`{"model":"m"}`)
			if status != http.StatusServiceUnavailable || strings.TrimSpace(body) != refusedBody {
				t.Fatalf("status %d body %q, want 503 %s", status, body, refusedBody)
			}
			if time.Since(start) > 2*time.Second {
				t.Errorf("took %s to refuse", time.Since(start))
			}
			if n := r.upstream.Load(); n != 0 {
				t.Errorf("upstream reached %d times, want never", n)
			}
		})
	}
}

func TestWriteAheadRefusalUsesTheErrorStyleOfTheProxy(t *testing.T) {
	r := newWriteAheadRig(t, jsonUpstream(), waOpts{noRun: true, ackTimeout: 10 * time.Millisecond})
	resp, err := http.Post(r.srv.URL+"/openai/v1/chat/completions", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	unknown, err := http.Post(r.srv.URL+"/nope/x", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	_ = unknown.Body.Close()
	if got, want := resp.Header.Get("Content-Type"), unknown.Header.Get("Content-Type"); got != want {
		t.Errorf("content type %q, the other proxy errors use %q", got, want)
	}
}

// A refused call leaves no call entry and no gap, and the next call goes through
// once the ledger takes writes again.
func TestWriteAheadRefusedCallLeavesNoCallOrGap(t *testing.T) {
	var failing atomic.Bool
	failing.Store(true)
	r := newWriteAheadRig(t, jsonUpstream(), waOpts{hook: func(h *hookStore) {
		h.fail = func(es []ledger.Entry) error {
			if failing.Load() {
				return failOn(ledger.TypeIntent)(es)
			}
			return nil
		}
	}})
	if status, _ := r.post(`{"model":"m"}`); status != http.StatusServiceUnavailable {
		t.Fatalf("status %d", status)
	}
	failing.Store(false)
	if status, _ := r.post(`{"model":"m"}`); status != 200 {
		t.Fatalf("status after recovery %d", status)
	}

	es := r.ledgerEntries()
	if hasType(es, ledger.TypeGap) {
		t.Errorf("a refused call was counted as dropped: %v", ofType(es, ledger.TypeGap))
	}
	if !hasType(es, ledger.TypeKeyEvent) {
		t.Error("the key was not enrolled by the call that went through: a refused call must not mark it known")
	}
	if len(ofType(es, ledger.TypeCall)) != 1 || len(ofType(es, ledger.TypeIntent)) != 1 {
		t.Errorf("%d calls, %d intents; want 1 and 1", len(ofType(es, ledger.TypeCall)), len(ofType(es, ledger.TypeIntent)))
	}
	if r.upstream.Load() != 1 {
		t.Errorf("upstream reached %d times, want once", r.upstream.Load())
	}
	mustVerify(t, r.store)
}

// Concurrent intents share fsyncs: with a slow disk, N calls take fewer than N appends.
func TestWriteAheadGroupCommit(t *testing.T) {
	const n = 24
	r := newWriteAheadRig(t, jsonUpstream(), waOpts{hook: func(h *hookStore) { h.delay = 40 * time.Millisecond }})
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if status, _ := r.post(`{"model":"m"}`); status != 200 {
				t.Errorf("status %d", status)
			}
		}()
	}
	close(start)
	wg.Wait()

	es := r.ledgerEntries()
	if len(ofType(es, ledger.TypeIntent)) != n || len(ofType(es, ledger.TypeCall)) != n {
		t.Fatalf("%d intents, %d calls, want %d each", len(ofType(es, ledger.TypeIntent)), len(ofType(es, ledger.TypeCall)), n)
	}
	if got := r.hook.appendsWith(ledger.TypeIntent); got >= n {
		t.Errorf("%d fsyncs for %d intents, want fewer", got, n)
	}
	if rep := mustVerify(t, r.store); rep.OrphanIntents != 0 {
		t.Errorf("%d orphans", rep.OrphanIntents)
	}
}

// An intent does not wait for the flush interval: it is written at once.
func TestWriteAheadIntentDoesNotWaitForTheFlushInterval(t *testing.T) {
	r := newWriteAheadRig(t, jsonUpstream(), waOpts{flush: time.Hour})
	start := time.Now()
	if status, _ := r.post(`{"model":"m"}`); status != 200 {
		t.Fatalf("status %d", status)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("took %s with a flush interval of an hour", d)
	}
}

// Whatever completions are queued go out in the same fsync as the intent.
func TestWriterIntentFlushesQueuedCompletionsToo(t *testing.T) {
	s := newStore(t)
	h := &hookStore{Store: s}
	w := proxy.NewWriter(h, 100, discard)
	w.FlushEvery = time.Hour
	if !w.Enqueue(goodCall(1)) || !w.Enqueue(goodCall(2)) {
		t.Fatal("queue full")
	}
	go func() { _ = w.Run(context.Background()) }()
	defer w.Close()

	seq, err := w.Intent(context.Background(), ledger.Entry{"type": ledger.TypeIntent, "call_id": "x"})
	if err != nil || seq != 3 {
		t.Fatalf("seq %d err %v, want 3 and nil", seq, err)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.appends) != 1 || strings.Join(h.appends[0], ",") != "call,call,intent" {
		t.Errorf("appends %v, want one holding the two calls and the intent", h.appends)
	}
}

func TestWriterIntentReturnsTheWriteError(t *testing.T) {
	s := newStore(t)
	h := &hookStore{Store: s, fail: failOn(ledger.TypeIntent)}
	w := proxy.NewWriter(h, 100, discard)
	go func() { _ = w.Run(context.Background()) }()
	defer w.Close()
	if _, err := w.Intent(context.Background(), ledger.Entry{"type": ledger.TypeIntent, "call_id": "x"}); !errors.Is(err, errFsync) {
		t.Fatalf("error %v, want the fsync error", err)
	}
	h.fail = nil
	if _, err := w.Intent(context.Background(), ledger.Entry{"type": ledger.TypeIntent, "call_id": "y"}); err != nil {
		t.Fatal(err)
	}
	w.Close()
	if es, _ := s.Entries(); hasType(es, ledger.TypeGap) {
		t.Errorf("a refused intent was counted as dropped: %v", es)
	}
}

func TestWriterIntentRefusesAnEntryThatCannotBeEncoded(t *testing.T) {
	s := newStore(t)
	w := proxy.NewWriter(s, 100, discard)
	go func() { _ = w.Run(context.Background()) }()
	defer w.Close()
	_, err := w.Intent(context.Background(), ledger.Entry{"type": ledger.TypeIntent, "call_id": "x", "declared_bytes_in": tooBig})
	if !errors.Is(err, ledger.ErrEncode) {
		t.Fatalf("error %v, want ErrEncode", err)
	}
	if seq, err := w.Intent(context.Background(), ledger.Entry{"type": ledger.TypeIntent, "call_id": "y"}); err != nil || seq != 1 {
		t.Errorf("next intent: seq %d err %v, want 1 and nil", seq, err)
	}
	w.Close()
	if es, _ := s.Entries(); hasType(es, ledger.TypeGap) {
		t.Errorf("a refused intent was counted as dropped: %v", es)
	}
}

// After Close an intent is written at once, as a late entry is.
func TestWriterIntentAfterCloseIsWrittenSynchronously(t *testing.T) {
	s := newStore(t)
	w := proxy.NewWriter(s, 10, discard)
	go func() { _ = w.Run(context.Background()) }()
	w.Close()
	seq, err := w.Intent(context.Background(), ledger.Entry{"type": ledger.TypeIntent, "call_id": "z"})
	if err != nil || seq != 1 {
		t.Fatalf("seq %d err %v", seq, err)
	}
}

func TestWriterIntentStopsWaitingWhenTheContextEnds(t *testing.T) {
	s := newStore(t)
	w := proxy.NewWriter(s, 10, discard) // never started
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := w.Intent(ctx, ledger.Entry{"type": ledger.TypeIntent}); !errors.Is(err, context.Canceled) {
		t.Fatalf("error %v, want context.Canceled", err)
	}
}

func TestWriterIntentErrorsAreSentinels(t *testing.T) {
	s := newStore(t)
	w := proxy.NewWriter(s, 1, discard) // never started
	w.AckTimeout = 10 * time.Millisecond
	if _, err := w.Intent(context.Background(), ledger.Entry{"type": ledger.TypeIntent}); !errors.Is(err, proxy.ErrAckTimeout) {
		t.Fatalf("error %v, want ErrAckTimeout", err)
	}
	// The first intent is still queued, so the queue (size 1) is full.
	if _, err := w.Intent(context.Background(), ledger.Entry{"type": ledger.TypeIntent}); !errors.Is(err, proxy.ErrQueueFull) {
		t.Fatalf("error %v, want ErrQueueFull", err)
	}
}

// A failed completion is fail-open: the client is served, the intent stays as
// an orphan and a gap entry says a call was lost.
func TestWriteAheadFailedCompletionStaysFailOpen(t *testing.T) {
	var failCalls atomic.Bool
	var failed atomic.Int64
	r := newWriteAheadRig(t, jsonUpstream(), waOpts{hook: func(h *hookStore) {
		h.fail = func(es []ledger.Entry) error {
			err := failOn(ledger.TypeCall)(es)
			if err != nil && failCalls.Load() {
				failed.Add(1)
				return err
			}
			return nil
		}
	}})
	if status, _ := r.post(`{"model":"m"}`); status != 200 { // enrols the key and is recorded
		t.Fatalf("status %d", status)
	}
	waitFor(t, func() bool { return len(ofType(mustEntries(t, r.store), ledger.TypeCall)) == 1 })
	failCalls.Store(true)
	if status, _ := r.post(`{"model":"m"}`); status != 200 {
		t.Fatalf("status %d, want the client served", status)
	}
	waitFor(t, func() bool { return failed.Load() > 0 })
	failCalls.Store(false)
	if status, _ := r.post(`{"model":"m"}`); status != 200 {
		t.Fatalf("second call: %d", status)
	}

	es := r.ledgerEntries()
	gaps := ofType(es, ledger.TypeGap)
	if len(gaps) != 1 || gaps[0].Int("dropped") != 1 {
		t.Fatalf("gaps %v, want one with dropped 1", gaps)
	}
	rep := mustVerify(t, r.store)
	if rep.OrphanIntents != 1 || rep.Orphans[0].Seq != ofType(es, ledger.TypeIntent)[1].Seq() {
		t.Errorf("orphans %+v, want the intent of the call that was not recorded", rep.Orphans)
	}
}

func mustEntries(t *testing.T, s *ledger.Store) []ledger.Entry {
	t.Helper()
	es, err := s.Entries()
	if err != nil {
		t.Fatal(err)
	}
	return es
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met in 3s")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// A call that fails at the upstream is recorded with status 0 and completes its intent.
func TestWriteAheadUpstreamErrorCompletesTheIntent(t *testing.T) {
	r := newWriteAheadRig(t, jsonUpstream(), waOpts{})
	r.us.Close()
	if status, _ := r.post(`{"model":"m"}`); status != http.StatusBadGateway {
		t.Fatalf("status %d, want 502", status)
	}
	es := r.ledgerEntries()
	call := ofType(es, ledger.TypeCall)[0]
	if call.Int("status") != 0 || call.Int("intent_seq") != ofType(es, ledger.TypeIntent)[0].Seq() {
		t.Errorf("call %v", call)
	}
	if rep := mustVerify(t, r.store); rep.OrphanIntents != 0 {
		t.Errorf("%d orphans", rep.OrphanIntents)
	}
}

// A stream cut by the upstream, and one cut by the client, both end in a call
// entry that completes the intent.
func TestWriteAheadStreamCutCompletesTheIntent(t *testing.T) {
	t.Run("upstream cuts it", func(t *testing.T) {
		r := newWriteAheadRig(t, sseUpstream(true, sseStart), waOpts{})
		_, _, _ = r.asRig().send("POST", "/openai/v1/chat/completions", `{"model":"m","stream":true}`, nil)
		es := r.ledgerEntries()
		call := ofType(es, ledger.TypeCall)[0]
		if !call.Bool("client_aborted") || call.Str("call_id") != ofType(es, ledger.TypeIntent)[0].Str("call_id") {
			t.Errorf("call %v", call)
		}
		if rep := mustVerify(t, r.store); rep.OrphanIntents != 0 {
			t.Errorf("%d orphans", rep.OrphanIntents)
		}
	})
	t.Run("client cuts it", func(t *testing.T) {
		hold := make(chan struct{})
		t.Cleanup(func() { close(hold) })
		r := newWriteAheadRig(t, http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, sseStart)
			w.(http.Flusher).Flush()
			select {
			case <-hold:
			case <-req.Context().Done():
			}
		}), waOpts{})
		ctx, cancel := context.WithCancel(context.Background())
		req, _ := http.NewRequestWithContext(ctx, "POST", r.srv.URL+"/openai/v1/chat/completions", strings.NewReader(`{"stream":true}`))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		buf := make([]byte, 10)
		if _, err := io.ReadFull(resp.Body, buf); err != nil {
			t.Fatal(err)
		}
		cancel()
		_ = resp.Body.Close()
		waitFor(t, func() bool {
			es, _ := r.store.Entries()
			return hasType(es, ledger.TypeCall)
		})
		es := r.ledgerEntries()
		if call := ofType(es, ledger.TypeCall)[0]; !call.Bool("client_aborted") || call.Int("intent_seq") != ofType(es, ledger.TypeIntent)[0].Seq() {
			t.Errorf("call %v", call)
		}
		mustVerify(t, r.store)
	})
}

// The proxy dies after the intent is on disk and before the call is recorded:
// verify reports one orphan and nothing else.
func TestWriteAheadIntentWithoutCallIsAnOrphan(t *testing.T) {
	r := newWriteAheadRig(t, jsonUpstream(), waOpts{})
	if status, _ := r.post(`{"model":"m"}`); status != 200 {
		t.Fatal(status)
	}
	// A second intent that never gets its call: the writer is asked for it directly.
	intent := ledger.Entry{"type": ledger.TypeIntent, "call_id": strings.Repeat("e", 32), "endpoint": "chat.completions"}
	if _, err := r.w.Intent(context.Background(), intent); err != nil {
		t.Fatal(err)
	}
	r.ledgerEntries()
	rep := mustVerify(t, r.store)
	if rep.OrphanIntents != 1 || len(rep.Warnings) != 0 || rep.Orphans[0].Endpoint != "chat.completions" {
		t.Errorf("orphans %+v warnings %v", rep.Orphans, rep.Warnings)
	}
}

// A WebSocket upgrade gets its intent on disk before the upgrade is forwarded.
func TestWriteAheadWebSocketIntentComesFirst(t *testing.T) {
	var r *waRig
	var intents int
	r = newWriteAheadRig(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		es, _ := r.store.Entries()
		intents = len(ofType(es, ledger.TypeIntent))
		conn, brw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		defer func() { _ = conn.Close() }()
		_, _ = brw.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n")
		_ = brw.Flush()
		buf := make([]byte, 5)
		_, _ = io.ReadFull(brw, buf)
	}), waOpts{})

	conn, err := net.Dial("tcp", strings.TrimPrefix(r.srv.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(conn, "GET /openai/v1/realtime?model=m HTTP/1.1\r\nHost: x\r\nConnection: Upgrade\r\nUpgrade: websocket\r\nAuthorization: Bearer sk-ws\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil || resp.StatusCode != 101 {
		t.Fatalf("no upgrade: %v %v", resp, err)
	}
	if _, err := io.WriteString(conn, "hello"); err != nil {
		t.Fatal(err)
	}
	_ = conn.Close()
	waitFor(t, func() bool {
		es, _ := r.store.Entries()
		return hasType(es, ledger.TypeCall)
	})

	if intents != 1 {
		t.Errorf("%d intents on disk when the upstream got the upgrade, want 1", intents)
	}
	es := r.ledgerEntries()
	intent, call := ofType(es, ledger.TypeIntent)[0], ofType(es, ledger.TypeCall)[0]
	if call.Str("transport") != "websocket" || call.Str("call_id") != intent.Str("call_id") || call.Int("intent_seq") != intent.Seq() {
		t.Errorf("call %v does not complete intent %v", call, intent)
	}
	if intent.Str("endpoint") != "realtime" {
		t.Errorf("intent endpoint %q, want realtime", intent.Str("endpoint"))
	}
	mustVerify(t, r.store)
}

func TestWriteAheadRefusesAnUpgradeToo(t *testing.T) {
	r := newWriteAheadRig(t, jsonUpstream(), waOpts{noRun: true, ackTimeout: 10 * time.Millisecond})
	conn, err := net.Dial("tcp", strings.TrimPrefix(r.srv.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	if _, err := io.WriteString(conn, "GET /openai/v1/realtime HTTP/1.1\r\nHost: x\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil || resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("response %v err %v, want 503", resp, err)
	}
	if r.upstream.Load() != 0 {
		t.Error("the upgrade was forwarded")
	}
}

// Requests that are not forwarded get no intent.
func TestWriteAheadNoIntentForUnknownUpstreamOrHealth(t *testing.T) {
	r := newWriteAheadRig(t, jsonUpstream(), waOpts{})
	paths := []string{"/nope/v1/x", "/_ambermoth/health"}
	for _, path := range paths {
		resp, err := http.Get(r.srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
	}
	if es := r.ledgerEntries(); hasType(es, ledger.TypeIntent) {
		t.Errorf("intents written for requests that were not forwarded: %v", ofType(es, ledger.TypeIntent))
	}
}

// The proxy counts, when it starts, the intents that have no call.
func TestProxyCountsOpenIntentsAtStart(t *testing.T) {
	r := newWriteAheadRig(t, jsonUpstream(), waOpts{})
	if got := r.proxy.OpenIntents(); got != 0 {
		t.Fatalf("open intents in a new ledger: %d", got)
	}
	if status, _ := r.post("{}"); status != 200 {
		t.Fatal(status)
	}
	for i := 0; i < 2; i++ {
		if _, err := r.w.Intent(context.Background(), ledger.Entry{"type": ledger.TypeIntent, "call_id": strings.Repeat(string(rune('a'+i)), 32)}); err != nil {
			t.Fatal(err)
		}
	}
	r.ledgerEntries()

	p, err := proxy.New(proxy.DefaultConfig(), r.store, r.w, nil, discard)
	if err != nil {
		t.Fatal(err)
	}
	if got := p.OpenIntents(); got != 2 {
		t.Errorf("open intents %d, want 2 (the completed one is not counted)", got)
	}
}
