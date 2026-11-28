package proxy_test

import (
	"bufio"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/devrdias/ambermoth/internal/ledger"
	"github.com/devrdias/ambermoth/internal/proxy"
)

// lateRead holds back the first Read after the body hit EOF until gate is
// closed, then reports on done. That is the extra read Go's transport makes
// once it has copied Content-Length bytes.
type lateRead struct {
	io.ReadCloser
	gate <-chan struct{}
	done chan<- struct{}
	eof  bool
}

func (l *lateRead) Read(p []byte) (int, error) {
	if l.eof {
		<-l.gate
		defer close(l.done)
	}
	n, err := l.ReadCloser.Read(p)
	l.eof = l.eof || err == io.EOF
	return n, err
}

type lateTransport struct {
	gate <-chan struct{}
	done chan<- struct{}
}

func (lt lateTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r.Body = &lateRead{ReadCloser: r.Body, gate: lt.gate, done: lt.done}
	return http.DefaultTransport.RoundTrip(r)
}

// proxyWith starts a proxy in front of up that uses tr, with a real ledger.
func proxyWith(t *testing.T, up http.Handler, tr http.RoundTripper) (*httptest.Server, *ledger.Store, *proxy.Writer) {
	t.Helper()
	us := httptest.NewServer(up)
	t.Cleanup(us.Close)
	s, err := ledger.Init(t.TempDir(), "test", time.Now)
	if err != nil {
		t.Fatal(err)
	}
	w := proxy.NewWriter(s, 1000, discard)
	go func() { _ = w.Run(context.Background()) }()
	cfg := proxy.DefaultConfig()
	cfg.Upstreams = map[string]proxy.Upstream{"openai": {BaseURL: us.URL, Style: "openai"}}
	p, err := proxy.New(cfg, s, w, tr, discard)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(p)
	t.Cleanup(srv.Close)
	return srv, s, w
}

// A stream must not be cut when the upstream answers before the transport has
// made its last read of the request body. By then the server has closed that
// body, and a read of it fails and tears down the upstream connection.
func TestStreamSurvivesLateRequestBodyRead(t *testing.T) {
	const events = 20
	gate, done := make(chan struct{}), make(chan struct{})
	up := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		for i := 0; i < events; i++ {
			_, _ = fmt.Fprintf(w, "data: %d\n\n", i)
			w.(http.Flusher).Flush()
			if i != 0 {
				continue
			}
			// Let the client see the first event, then the late read run. If it
			// breaks the connection the server notices; if not, stop waiting.
			select {
			case <-done:
			case <-time.After(5 * time.Second):
			}
			select {
			case <-r.Context().Done():
			case <-time.After(300 * time.Millisecond):
			}
		}
	})
	srv, s, w := proxyWith(t, up, lateTransport{gate: gate, done: done})
	resp, err := http.Post(srv.URL+"/openai/v1/chat/completions", "application/json", strings.NewReader(`{"model":"m","stream":true}`))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	br := bufio.NewReader(resp.Body)
	if _, err := br.ReadString('\n'); err != nil {
		t.Fatal(err)
	}
	close(gate)
	rest, err := io.ReadAll(br)
	if err != nil {
		t.Fatalf("stream cut after %d bytes: %v", len(rest), err)
	}
	if got := strings.Count(string(rest), "data: "); got != events-1 {
		t.Errorf("got %d more events, want %d", got, events-1)
	}
	w.Close()
	cs := calls(t, s)
	if len(cs) != 1 || cs[0].Bool("client_aborted") {
		t.Errorf("entry = %v, want one call that was not aborted", cs)
	}
}

// The request is recorded the same whichever way its length is sent.
func TestRequestRecordedByLengthKind(t *testing.T) {
	body := `{"model":"m","messages":[{"role":"user","content":"` + strings.Repeat("x", 5000) + `"}]}`
	kinds := map[string]func() io.Reader{
		"content-length": func() io.Reader { return strings.NewReader(body) },
		"chunked":        func() io.Reader { return io.MultiReader(strings.NewReader(body)) },
	}
	for name, mk := range kinds {
		t.Run(name, func(t *testing.T) {
			seenCh := make(chan string, 1)
			up := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				b, _ := io.ReadAll(r.Body)
				seenCh <- string(b)
				_, _ = io.WriteString(w, `{"model":"m"}`)
			})
			srv, s, w := proxyWith(t, up, nil)
			resp, err := http.Post(srv.URL+"/openai/v1/chat/completions", "application/json", mk())
			if err != nil {
				t.Fatal(err)
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			seen := <-seenCh
			w.Close()
			cs := calls(t, s)
			if len(cs) != 1 || seen != body {
				t.Fatalf("calls = %d, upstream saw %d bytes, want 1 call and %d", len(cs), len(seen), len(body))
			}
			m := hmac.New(sha256.New, ledger.EntryKey(s.MasterKey, cs[0].Str("salt_id")))
			m.Write([]byte(body))
			if cs[0].Int("bytes_in") != int64(len(body)) || cs[0].Str("prompt_hmac") != ledger.FormatHMAC(m.Sum(nil)) {
				t.Errorf("bytes_in = %d, want %d, or prompt_hmac differs", cs[0].Int("bytes_in"), len(body))
			}
		})
	}
}

// An upstream that answers without reading the request still gets the call recorded.
func TestEarlyReplyBeforeBodyRead(t *testing.T) {
	up := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"model":"m"}`)
	})
	srv, s, w := proxyWith(t, up, nil)
	resp, err := http.Post(srv.URL+"/openai/v1/chat/completions", "application/json", strings.NewReader(`{"model":"m"}`))
	if err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil || string(b) != `{"model":"m"}` {
		t.Fatalf("body %q, err %v", b, err)
	}
	w.Close()
	if cs := calls(t, s); len(cs) != 1 || cs[0].Int("status") != 200 {
		t.Errorf("entries = %v, want one with status 200", cs)
	}
}
