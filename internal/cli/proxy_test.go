package cli_test

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/devrdias/ambermoth/internal/cli"
	"github.com/devrdias/ambermoth/internal/ledger"
	"github.com/devrdias/ambermoth/internal/proxy"
)

// lockedBuffer is a bytes.Buffer that the proxy goroutine and the test can share.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

//rules:readonly compiled pattern, never written.
var listeningRE = regexp.MustCompile(`listening on (http://\S+)`)

const upstreamBody = `{"model":"gpt-test","usage":{"prompt_tokens":100,"completion_tokens":20}}`

func fakeUpstream(t *testing.T) *httptest.Server {
	t.Helper()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, upstreamBody)
	}))
	t.Cleanup(up.Close)
	return up
}

// pointAt rewrites the ledger config to use one upstream, no TSA and no anchoring.
func pointAt(t *testing.T, dir, upstream string) {
	t.Helper()
	cfgPath := filepath.Join(dir, ledger.ConfigFile)
	cfg, err := proxy.LoadConfig(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Upstreams = map[string]proxy.Upstream{"openai": {BaseURL: upstream, Style: "openai"}}
	cfg.TSAs, cfg.AnchorInterval = nil, ""
	if err := os.WriteFile(cfgPath, proxy.MarshalConfig(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
}

// startProxy runs 'ambermoth proxy' in-process and returns its base URL and a
// function that stops it (as SIGINT would) and returns its exit code.
func startProxy(t *testing.T, dir string) (string, func() int) {
	t.Helper()
	base, stop, _ := startProxyLogs(t, dir)
	return base, stop
}

// startProxyLogs is startProxy that also returns what the proxy has logged so far.
func startProxyLogs(t *testing.T, dir string) (string, func() int, func() string) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	errOut := &lockedBuffer{}
	app := cli.New(cli.Env{
		Stdin: strings.NewReader(""), Stdout: io.Discard, Stderr: errOut,
		Getenv: func(string) string { return "" }, Clock: fixedClock,
		Logger: slog.New(slog.NewTextHandler(errOut, nil)), Version: "test",
	})
	done := make(chan int, 1)
	go func() { done <- app.Run(ctx, []string{"proxy", "--dir", dir, "--listen", "127.0.0.1:0"}) }()
	stop := func() int {
		cancel()
		return <-done
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if m := listeningRE.FindStringSubmatch(errOut.String()); m != nil {
			return m[1], stop, errOut.String
		}
		time.Sleep(5 * time.Millisecond)
	}
	stop()
	t.Fatalf("proxy did not start:\n%s", errOut.String())
	return "", nil, nil
}

func callSeqs(t *testing.T, dir string) []int64 {
	t.Helper()
	s, err := ledger.Open(dir, fixedClock)
	if err != nil {
		t.Fatal(err)
	}
	es, err := s.Entries()
	if err != nil {
		t.Fatal(err)
	}
	var seqs []int64
	for _, e := range es {
		if e.Type() == ledger.TypeCall {
			seqs = append(seqs, e.Seq())
		}
	}
	return seqs
}

func TestProxyRecordsCallsAndPacksThem(t *testing.T) {
	h := newHarness(t)
	h.check(t, runCase{args: []string{"init"}, wantCode: cli.ExitOK})
	pointAt(t, h.dir, fakeUpstream(t).URL)

	base, stop := startProxy(t, h.dir)
	reqBody := `{"model":"gpt-test","messages":[]}`
	req, err := http.NewRequest(http.MethodPost, base+"/openai/v1/chat/completions", strings.NewReader(reqBody))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer sk-proxy-test")
	req.Header.Set("X-Provenance-Purpose", "dev-experiment")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	respBody, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if code := stop(); code != cli.ExitOK {
		t.Fatalf("proxy exit code %d", code)
	}

	seqs := callSeqs(t, h.dir)
	if len(seqs) != 1 {
		t.Fatalf("recorded %d calls, want 1", len(seqs))
	}
	seq := strconv.FormatInt(seqs[0], 10)
	work := filepath.Dir(h.dir)
	packPath := filepath.Join(work, "pack.zip")
	reqPath, respPath, wrongPath := filepath.Join(work, "req"), filepath.Join(work, "resp"), filepath.Join(work, "wrong")
	files := map[string][]byte{reqPath: []byte(reqBody), respPath: respBody, wrongPath: []byte("{}")}
	for path, b := range files {
		if err := os.WriteFile(path, b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	steps := []runCase{
		{name: "reports the call", args: []string{"report"}, wantCode: cli.ExitOK, wantOut: "1 calls · 100 tokens in · 20 tokens out"},
		{name: "amends the call", args: []string{"amend", "--seq", seq, "--purpose", "eval-benchmark", "--reason", "mis-tagged", "--by", "rafa"}, wantCode: cli.ExitOK, wantOut: "amendment recorded"},
		{name: "builds a pack", args: []string{"pack", "--from", "2026-03-01", "--to", "2026-03-31", "--no-tsa", "--disclose", seq, "--out", packPath}, wantCode: cli.ExitOK, wantOut: "1 calls"},
		{name: "verifies the pack", args: []string{"verify-pack", packPath, "--report", filepath.Join(work, "report.html")}, wantCode: cli.ExitOK, wantOut: "summary.html: identical"},
		{name: "verifies the pack as JSON", args: []string{"verify-pack", packPath, "--json"}, wantCode: cli.ExitOK, wantOut: `"ok": true`},
		{name: "matches the disclosed sample", args: []string{"check-sample", "--pack", packPath, "--seq", seq, "--request", reqPath, "--response", respPath}, wantCode: cli.ExitOK, wantOut: "response: MATCHES the ledger"},
		{name: "rejects a different response", args: []string{"check-sample", "--pack", packPath, "--seq", seq, "--response", wrongPath}, wantCode: cli.ExitFail, wantOut: "DOES NOT MATCH"},
	}
	for _, c := range steps {
		t.Run(c.name, func(t *testing.T) { h.check(t, c) })
	}
}
