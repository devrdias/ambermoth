package cli_test

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/devrdias/ambermoth/internal/cli"
	"github.com/devrdias/ambermoth/internal/ledger"
	"github.com/devrdias/ambermoth/internal/proxy"
	"github.com/devrdias/ambermoth/internal/tsa/tsatest"
)

// anchorFixture is an initialised ledger with one key event and a test TSA
// whose root is saved as ca.pem next to the ledger. Needs openssl.
type anchorFixture struct {
	*harness
	tsaURL string
	caPEM  string
}

func newAnchorFixture(t *testing.T) *anchorFixture {
	t.Helper()
	auth := tsatest.New(t)
	h := newHarness(t)
	h.env["TEST_KEY"] = "sk-anchor"
	h.check(t, runCase{args: []string{"init"}, wantCode: cli.ExitOK})
	h.check(t, runCase{args: []string{"key", "enroll", "--provider", "openai", "--key-env", "TEST_KEY"}, wantCode: cli.ExitOK})
	useTSAs(t, h.dir)
	caPEM := filepath.Join(filepath.Dir(h.dir), "ca.pem")
	writeFile(t, caPEM, string(auth.CAPEM))
	return &anchorFixture{harness: h, tsaURL: auth.Server(t).URL, caPEM: caPEM}
}

func TestAnchorCommand(t *testing.T) {
	t.Parallel()
	t.Run("fails when no TSA is configured", func(t *testing.T) {
		t.Parallel()
		f := newAnchorFixture(t)
		f.check(t, runCase{args: []string{"anchor"}, wantCode: cli.ExitFail, wantErr: "no TSA configured"})
		if n := len(entriesOfType(t, f.dir, ledger.TypeAnchor)); n != 0 {
			t.Errorf("%d anchor entries, want 0", n)
		}
	})
	t.Run("anchors the head through --tsa", func(t *testing.T) {
		t.Parallel()
		f := newAnchorFixture(t)
		f.check(t, runCase{args: []string{"anchor", "--tsa", f.tsaURL}, wantCode: cli.ExitOK, wantOut: "anchored seq 1 (sha256:"})
		anchors := entriesOfType(t, f.dir, ledger.TypeAnchor)
		if len(anchors) != 1 {
			t.Fatalf("%d anchor entries, want 1", len(anchors))
		}
		if got := anchors[0].Int("target_seq"); got != 1 {
			t.Errorf("target_seq = %d, want 1 (the key event)", got)
		}
		if got := anchors[0].Str("tsa"); got != f.tsaURL {
			t.Errorf("tsa = %q, want %q", got, f.tsaURL)
		}
	})
	t.Run("uses the TSAs from the config", func(t *testing.T) {
		t.Parallel()
		f := newAnchorFixture(t)
		useTSAs(t, f.dir, f.tsaURL)
		f.check(t, runCase{args: []string{"anchor"}, wantCode: cli.ExitOK, wantOut: "via " + f.tsaURL})
		if n := len(entriesOfType(t, f.dir, ledger.TypeAnchor)); n != 1 {
			t.Errorf("%d anchor entries, want 1", n)
		}
	})
	t.Run("anchors with every working TSA and fails when one is down", func(t *testing.T) {
		t.Parallel()
		f := newAnchorFixture(t)
		bad := brokenTSA(t).URL
		f.check(t, runCase{args: []string{"anchor", "--tsa", f.tsaURL, "--tsa", bad}, wantCode: cli.ExitFail,
			wantOut: "via " + f.tsaURL, wantErr: "some TSAs failed"})
		if n := len(entriesOfType(t, f.dir, ledger.TypeAnchor)); n != 1 {
			t.Errorf("%d anchor entries, want 1 from the working TSA", n)
		}
	})
	t.Run("records nothing when every TSA is down", func(t *testing.T) {
		t.Parallel()
		f := newAnchorFixture(t)
		f.check(t, runCase{args: []string{"anchor", "--tsa", brokenTSA(t).URL}, wantCode: cli.ExitFail, wantErr: "all TSAs failed"})
		if n := len(entriesOfType(t, f.dir, ledger.TypeAnchor)); n != 0 {
			t.Errorf("%d anchor entries, want 0", n)
		}
	})
}

func TestVerifyReportsAnchors(t *testing.T) {
	t.Parallel()
	f := newAnchorFixture(t)
	f.check(t, runCase{args: []string{"verify"}, wantCode: cli.ExitOK, wantOut: "nothing is anchored yet; run 'ambermoth anchor'"})
	f.check(t, runCase{args: []string{"anchor", "--tsa", f.tsaURL}, wantCode: cli.ExitOK})
	f.check(t, runCase{args: []string{"key", "enroll", "--provider", "openai", "--key-env", "TEST_KEY"}, wantCode: cli.ExitOK})

	cases := []runCase{
		{name: "pinned root", args: []string{"verify", "--tsa-ca", f.caPEM}, wantCode: cli.ExitOK,
			wantOut: "anchored through seq 1, 1 entries after the last anchor"},
		{name: "pinned root marks the anchor valid", args: []string{"verify", "--tsa-ca", f.caPEM}, wantCode: cli.ExitOK,
			wantOut: "✓ seq 1"},
		{name: "unpinned TSA is flagged", args: []string{"verify", "--allow-unpinned-tsa"}, wantCode: cli.ExitOK,
			wantOut: "(NOT PINNED)"},
		{name: "unknown root fails the anchor", args: []string{"verify"}, wantCode: cli.ExitFail,
			wantOut: "✗ seq 1"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) { f.check(t, c) })
	}
	_, out, _ := f.run("verify", "--tsa-ca", f.caPEM)
	if !strings.Contains(out, "(pinned)") {
		t.Errorf("verify with the TSA root does not say pinned:\n%s", out)
	}
}

// anchorsNow returns the anchor entries in the ledger file, or nil while it
// cannot be read (the proxy may be mid-write).
func anchorsNow(dir string) []ledger.Entry {
	es, _, err := ledger.ReadFile(filepath.Join(dir, ledger.LedgerFile))
	if err != nil {
		return nil
	}
	var anchors []ledger.Entry
	for _, e := range es {
		if e.Type() == ledger.TypeAnchor {
			anchors = append(anchors, e)
		}
	}
	return anchors
}

// waitForAnchors polls the ledger until it holds n anchor entries.
func waitForAnchors(t *testing.T, dir string, n int) []ledger.Entry {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if anchors := anchorsNow(dir); len(anchors) >= n {
			return anchors
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("ledger did not reach %d anchors", n)
	return nil
}

func TestProxyAnchorLoopAnchorsRepeatedly(t *testing.T) {
	t.Parallel()
	f := newAnchorFixture(t)
	pointAt(t, f.dir, fakeUpstream(t).URL)
	setConfig(t, f.dir, func(c *proxy.Config) { c.TSAs, c.AnchorInterval = []string{f.tsaURL}, "20ms" })

	_, stop := startProxy(t, f.dir)
	anchors := waitForAnchors(t, f.dir, 2)
	if code := stop(); code != cli.ExitOK {
		t.Fatalf("proxy exit code %d", code)
	}
	for _, e := range anchors {
		if e.Str("tsa") != f.tsaURL {
			t.Errorf("anchor %d used tsa %q, want %q", e.Seq(), e.Str("tsa"), f.tsaURL)
		}
	}
	f.check(t, runCase{args: []string{"verify", "--tsa-ca", f.caPEM}, wantCode: cli.ExitOK, wantOut: "Ledger verification: OK"})
}

func TestProxyAnchorLoopLogsFailuresAndKeepsGoing(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.check(t, runCase{args: []string{"init"}, wantCode: cli.ExitOK})
	pointAt(t, h.dir, fakeUpstream(t).URL)
	setConfig(t, h.dir, func(c *proxy.Config) { c.TSAs, c.AnchorInterval = []string{brokenTSA(t).URL}, "20ms" })

	_, stop, logs := startProxyLogs(t, h.dir)
	deadline := time.Now().Add(20 * time.Second)
	for strings.Count(logs(), "anchoring failed") < 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	got := logs()
	if code := stop(); code != cli.ExitOK {
		t.Fatalf("proxy exit code %d", code)
	}
	if n := strings.Count(got, "anchoring failed"); n < 2 {
		t.Fatalf("logged %d anchoring failures, want the loop to retry after the first:\n%s", n, got)
	}
	if n := len(entriesOfType(t, h.dir, ledger.TypeAnchor)); n != 0 {
		t.Errorf("%d anchor entries, want 0", n)
	}
}
