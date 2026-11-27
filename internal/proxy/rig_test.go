package proxy_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/devrdias/ambermoth/internal/ledger"
	"github.com/devrdias/ambermoth/internal/proxy"
)

// rig is a proxy in front of one test upstream, both on httptest servers.
type rig struct {
	t     *testing.T
	store *ledger.Store
	w     *proxy.Writer
	srv   *httptest.Server
}

// newRig serves up behind the "openai" and "anthropic" upstreams. tune may change the config.
func newRig(t *testing.T, up http.Handler, tune func(*proxy.Config)) *rig {
	t.Helper()
	us := httptest.NewServer(up)
	t.Cleanup(us.Close)
	s, err := ledger.Init(t.TempDir(), "test", time.Now)
	if err != nil {
		t.Fatal(err)
	}
	w := proxy.NewWriter(s, 1000, discard)
	w.FlushEvery = 5 * time.Millisecond
	go func() { _ = w.Run(context.Background()) }()
	cfg := proxy.DefaultConfig()
	cfg.Upstreams = map[string]proxy.Upstream{
		"openai":    {BaseURL: us.URL, Style: "openai"},
		"anthropic": {BaseURL: us.URL, Style: "anthropic"},
	}
	if tune != nil {
		tune(&cfg)
	}
	p, err := proxy.New(cfg, s, w, nil, discard)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(p)
	t.Cleanup(srv.Close)
	return &rig{t: t, store: s, w: w, srv: srv}
}

// send makes a request through the proxy and returns the status, the bytes
// the client received and the read error, if the body was cut.
func (r *rig) send(method, path, body string, hdr map[string]string) (int, string, error) {
	r.t.Helper()
	req, err := http.NewRequest(method, r.srv.URL+path, strings.NewReader(body))
	if err != nil {
		r.t.Fatal(err)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		r.t.Fatal(err)
	}
	b, rerr := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	return resp.StatusCode, string(b), rerr
}

// entries drains the writer and returns the call entries in order.
func (r *rig) entries() []ledger.Entry {
	r.t.Helper()
	r.w.Close()
	return calls(r.t, r.store)
}

// jsonUpstream answers every request with a small JSON body.
func jsonUpstream() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"model":"m","usage":{"prompt_tokens":1,"completion_tokens":2}}`)
	})
}
