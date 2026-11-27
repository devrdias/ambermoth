package proxy_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/devrdias/ambermoth/internal/ledger"
	"github.com/devrdias/ambermoth/internal/proxy"
)

const anthropicSSE = "event: message_start\n" +
	`data: {"type":"message_start","message":{"model":"claude-test","usage":{"input_tokens":25,"cache_read_input_tokens":5,"output_tokens":1}}}` + "\n\n" +
	"event: content_block_delta\n" +
	`data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"Hi"}}` + "\n\n" +
	"event: message_delta\n" +
	`data: {"type":"message_delta","usage":{"output_tokens":42}}` + "\n\n" +
	"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"

const openaiSSE = `data: {"model":"gpt-test","choices":[{"delta":{"content":"Hi"}}]}` + "\n\n" +
	`data: {"model":"gpt-test","choices":[],"usage":{"prompt_tokens":11,"completion_tokens":7,"prompt_tokens_details":{"cached_tokens":3}}}` + "\n\n" +
	"data: [DONE]\n\n"

type upstreamSeen struct {
	path, auth, provHeader string
	body                   string
}

func setup(t *testing.T) (*ledger.Store, *proxy.Writer, *httptest.Server, *upstreamSeen) {
	t.Helper()
	seen := &upstreamSeen{}
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		seen.path, seen.body = r.URL.Path, string(b)
		seen.auth = r.Header.Get("Authorization") + r.Header.Get("X-Api-Key")
		seen.provHeader = r.Header.Get("X-Provenance-Purpose")
		switch {
		case strings.HasSuffix(r.URL.Path, "/messages"):
			w.Header().Set("Content-Type", "text/event-stream")
			for _, chunk := range strings.SplitAfter(anthropicSSE, "\n\n") {
				io.WriteString(w, chunk)
				w.(http.Flusher).Flush()
			}
		case strings.Contains(string(b), `"stream":true`):
			w.Header().Set("Content-Type", "text/event-stream")
			io.WriteString(w, openaiSSE)
		default:
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"model":"gpt-test","usage":{"prompt_tokens":100,"completion_tokens":20}}`)
		}
	}))
	t.Cleanup(up.Close)

	s, err := ledger.Init(t.TempDir(), "test", time.Now)
	if err != nil {
		t.Fatal(err)
	}
	w := proxy.NewWriter(s, 1000, discard)
	w.FlushEvery = 10 * time.Millisecond
	go w.Run(context.Background())
	return s, w, up, seen
}

func calls(t *testing.T, s *ledger.Store) []ledger.Entry {
	es, err := s.Entries()
	if err != nil {
		t.Fatal(err)
	}
	var out []ledger.Entry
	for _, e := range es {
		if e.Type() == ledger.TypeCall {
			out = append(out, e)
		}
	}
	return out
}

func TestEndToEnd(t *testing.T) {
	s, w, up, seen := setup(t)
	cfg := proxy.DefaultConfig()
	cfg.Upstreams = map[string]proxy.Upstream{
		"openai":    {BaseURL: up.URL, Style: "openai"},
		"anthropic": {BaseURL: up.URL, Style: "anthropic"},
	}
	cfg.Routes = []proxy.RouteRule{{Prefix: "/v1/embeddings", Purpose: "rag-indexing"}}
	p, err := proxy.New(cfg, s, w, nil, discard)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(p)
	defer srv.Close()

	do := func(path, body string, hdr map[string]string) string {
		req, _ := http.NewRequest("POST", srv.URL+path, strings.NewReader(body))
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return string(b)
	}

	// 1. Anthropic streaming, purpose via header
	reqBody := `{"model":"claude-test","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	got := do("/anthropic/v1/messages", reqBody, map[string]string{
		"X-Api-Key": "sk-ant-test", "X-Provenance-Purpose": "eval-regression", "X-Provenance-Git-Ref": "a1b2c3d"})
	if got != anthropicSSE {
		t.Fatalf("SSE not passed through byte-for-byte:\n%q", got)
	}
	if seen.provHeader != "" || seen.path != "/v1/messages" || seen.auth != "sk-ant-test" || seen.body != reqBody {
		t.Fatalf("upstream saw %+v", seen)
	}
	// 2. OpenAI streaming with usage chunk, untagged
	do("/openai/v1/chat/completions", `{"model":"gpt-test","stream":true}`, map[string]string{"Authorization": "Bearer sk-oa"})
	// 3. OpenAI non-streaming embeddings, purpose via route rule
	do("/openai/v1/embeddings", `{"model":"emb-test","input":"x"}`, map[string]string{"Authorization": "Bearer sk-oa"})
	// 4. invalid purpose header is recorded, not rejected
	do("/openai/v1/chat/completions", `{"model":"gpt-test"}`, map[string]string{"Authorization": "Bearer sk-oa", "X-Provenance-Purpose": "training"})

	w.Close()
	cs := calls(t, s)
	if len(cs) != 4 {
		t.Fatalf("want 4 calls, got %d", len(cs))
	}
	a := cs[0]
	if a.Str("model") != "claude-test" || a.Int("tokens_in") != 25 || a.Int("tokens_out") != 42 ||
		a.Int("cached_in") != 5 || a.Str("purpose") != "eval-regression" || a.Str("git_ref") != "a1b2c3d" ||
		a.Str("endpoint") != "messages" || !a.Bool("stream") || a.Str("key_fp") != ledger.KeyFingerprint("sk-ant-test") {
		t.Errorf("anthropic entry wrong: %v", a)
	}
	// HMACs are recomputable with the per-entry key (disclosure path).
	ek := ledger.EntryKey(s.MasterKey, a.Str("salt_id"))
	m := hmac.New(sha256.New, ek)
	m.Write([]byte(reqBody))
	if a.Str("prompt_hmac") != ledger.FormatHMAC(m.Sum(nil)) {
		t.Error("prompt_hmac does not match request body")
	}
	m = hmac.New(sha256.New, ek)
	m.Write([]byte(anthropicSSE))
	if a.Str("output_hmac") != ledger.FormatHMAC(m.Sum(nil)) {
		t.Error("output_hmac does not match response stream")
	}
	if o := cs[1]; o.Int("tokens_in") != 11 || o.Int("tokens_out") != 7 || o.Int("cached_in") != 3 || o.Str("purpose") != "untagged" {
		t.Errorf("openai stream entry wrong: %v", o)
	}
	if e := cs[2]; e.Str("purpose") != "rag-indexing" || e.Str("endpoint") != "embeddings" || e.Int("tokens_in") != 100 {
		t.Errorf("embeddings entry wrong: %v", e)
	}
	if e := cs[3]; e.Str("purpose") != "untagged" || !strings.Contains(e.Str("note"), "invalid purpose header: training") {
		t.Errorf("invalid purpose handling wrong: %v", e)
	}
	// Two distinct keys -> two auto-enroll events; ledger verifies.
	es, lines, _ := ledger.ReadFile(s.LedgerPath())
	enrolls := 0
	for _, e := range es {
		if e.Type() == ledger.TypeKeyEvent {
			enrolls++
		}
	}
	if enrolls != 2 {
		t.Errorf("want 2 auto-enroll events, got %d", enrolls)
	}
	if r := ledger.Verify(es, lines, ledger.VerifyOptions{}); !r.OK() {
		t.Fatalf("ledger does not verify: %v", r.Errors)
	}
}

func TestFailOpenWhenUpstreamDown(t *testing.T) {
	s, err := ledger.Init(t.TempDir(), "test", time.Now)
	if err != nil {
		t.Fatal(err)
	}
	cfg := proxy.DefaultConfig()
	cfg.Upstreams = map[string]proxy.Upstream{"openai": {BaseURL: "http://127.0.0.1:1", Style: "openai"}}
	w := proxy.NewWriter(s, 10, discard)
	w.FlushEvery = 5 * time.Millisecond
	go w.Run(context.Background())
	p, _ := proxy.New(cfg, s, w, nil, discard)
	srv := httptest.NewServer(p)
	defer srv.Close()
	resp, err := http.Post(srv.URL+"/openai/v1/chat/completions", "application/json", strings.NewReader(`{"model":"m"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 502 {
		t.Fatalf("want 502, got %d", resp.StatusCode)
	}
	w.Close()
	cs := calls(t, s)
	if len(cs) != 1 || cs[0].Int("status") != 0 {
		t.Fatalf("expected one status-0 call entry, got %v", cs)
	}
}

func TestGapEntryAfterDrops(t *testing.T) {
	s, _ := ledger.Init(t.TempDir(), "test", time.Now)
	w := proxy.NewWriter(s, 1, discard)
	for i := 0; i < 5; i++ { // queue of 1, writer not running: 4 dropped
		w.Enqueue(ledger.Entry{"type": ledger.TypeCall})
	}
	go w.Run(context.Background())
	w.Close()
	es, _ := s.Entries()
	var gap ledger.Entry
	for _, e := range es {
		if e.Type() == ledger.TypeGap {
			gap = e
		}
	}
	if gap == nil || gap.Int("dropped") != 4 {
		t.Fatalf("expected gap entry with 4 dropped, got %v", gap)
	}
}

// discard is a logger that drops everything, for tests.
var discard = slog.New(slog.NewTextHandler(io.Discard, nil))
