package cli_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/devrdias/ambermoth/internal/cli"
	"github.com/devrdias/ambermoth/internal/ledger"
	"github.com/devrdias/ambermoth/internal/proxy"
)

// setConfig rewrites the ledger's config.json after applying mutate.
func setConfig(t *testing.T, dir string, mutate func(*proxy.Config)) {
	t.Helper()
	path := filepath.Join(dir, ledger.ConfigFile)
	cfg, err := proxy.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	mutate(&cfg)
	if err := os.WriteFile(path, proxy.MarshalConfig(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
}

// useTSAs makes the ledger's config point at urls and nothing else.
func useTSAs(t *testing.T, dir string, urls ...string) {
	t.Helper()
	setConfig(t, dir, func(c *proxy.Config) { c.TSAs = urls })
}

// entriesOfType reads the ledger file and returns its entries of one type.
func entriesOfType(t *testing.T, dir, typ string) []ledger.Entry {
	t.Helper()
	es, _, err := ledger.ReadFile(filepath.Join(dir, ledger.LedgerFile))
	if err != nil {
		t.Fatal(err)
	}
	var out []ledger.Entry
	for _, e := range es {
		if e.Type() == typ {
			out = append(out, e)
		}
	}
	return out
}

// brokenTSA is a TSA endpoint that always answers 500.
func brokenTSA(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		http.Error(w, "down", http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// recordCall initialises the harness ledger and records one call through the
// proxy (openai, 100 tokens in, 20 out, on 2026-03-04).
func (h *harness) recordCall(t *testing.T) {
	t.Helper()
	h.check(t, runCase{args: []string{"init"}, wantCode: cli.ExitOK})
	pointAt(t, h.dir, fakeUpstream(t).URL)
	base, stop := startProxy(t, h.dir)
	req, err := http.NewRequest(http.MethodPost, base+"/openai/v1/chat/completions", strings.NewReader(`{"model":"gpt-test"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer sk-fixture")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if code := stop(); code != cli.ExitOK {
		t.Fatalf("proxy exit code %d", code)
	}
}
