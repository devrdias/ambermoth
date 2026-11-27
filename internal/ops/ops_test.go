package ops_test

import (
	"context"
	"github.com/devrdias/ambermoth/internal/ops"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/devrdias/ambermoth/internal/ledger"
	"github.com/devrdias/ambermoth/internal/trust"
	"github.com/devrdias/ambermoth/internal/tsa/tsatest"
)

func newStore(t *testing.T) *ledger.Store {
	t.Helper()
	s, err := ledger.Init(t.TempDir(), "test", time.Now)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func verify(t *testing.T, s *ledger.Store, v ledger.AnchorVerifier) *ledger.Report {
	t.Helper()
	es, lines, err := ledger.ReadFile(s.LedgerPath())
	if err != nil {
		t.Fatal(err)
	}
	return ledger.Verify(es, lines, ledger.VerifyOptions{AnchorDir: filepath.Join(s.Dir, ledger.AnchorDir), CheckAnchor: v})
}

func TestAnchorAndPinning(t *testing.T) {
	auth := tsatest.New(t)
	srv := auth.Server(t)
	s := newStore(t)
	s.Append(ledger.Entry{"type": ledger.TypeCall, "ts": ledger.FormatTime(time.Now()), "provider": "openai"})
	es, err := ops.Anchor(context.Background(), s, []string{srv.URL, "http://127.0.0.1:1/down"}, nil)
	if len(es) != 1 || err == nil || !strings.Contains(err.Error(), "some TSAs failed") {
		t.Fatalf("want one anchor and a partial-failure error, got %d, %v", len(es), err)
	}

	// Pinned to the test root: OK and Pinned.
	r := verify(t, s, ops.AnchorVerifier(auth.Roots, false))
	if !r.OK() || len(r.Anchors) != 1 || !r.Anchors[0].Pinned {
		t.Fatalf("pinned verify: %v %+v", r.Errors, r.Anchors)
	}
	// Built-in roots only: the test TSA is not trusted.
	pool, err := trust.Pool(nil)
	if err != nil {
		t.Fatal(err)
	}
	if r := verify(t, s, ops.AnchorVerifier(pool, false)); r.OK() {
		t.Fatal("anchor from an unpinned TSA was accepted")
	}
	// Explicitly allowed: OK but reported as not pinned.
	r = verify(t, s, ops.AnchorVerifier(pool, true))
	if !r.OK() || r.Anchors[0].Pinned {
		t.Fatalf("allow-unpinned: %v %+v", r.Errors, r.Anchors)
	}
	// No roots and not allowed: refused.
	if r := verify(t, s, ops.AnchorVerifier(nil, false)); r.OK() {
		t.Fatal("verification without roots must fail")
	}
	// Extra root file is added to the pool.
	ca := filepath.Join(t.TempDir(), "ca.pem")
	os.WriteFile(ca, auth.CAPEM, 0o600)
	pool2, err := ops.TrustPool([]string{ca})
	if err != nil {
		t.Fatal(err)
	}
	if r := verify(t, s, ops.AnchorVerifier(pool2, false)); !r.OK() {
		t.Fatalf("--tsa-ca root not honoured: %v", r.Errors)
	}
	if _, err := ops.TrustPool([]string{filepath.Join(t.TempDir(), "missing.pem")}); err == nil {
		t.Error("missing root file should be an error")
	}
	if _, err := ops.Anchor(context.Background(), s, nil, nil); err == nil {
		t.Error("no TSAs should be an error")
	}
}

type amendCase struct {
	name              string
	from, to          int64
	purpose, why, who string
}

func TestAmend(t *testing.T) {
	s := newStore(t)
	s.Append(ledger.Entry{"type": ledger.TypeCall, "ts": ledger.FormatTime(time.Now()), "purpose": "untagged"})
	cases := []amendCase{
		{name: "requires a reason", from: 1, to: 1, purpose: "eval", who: "me"},
		{name: "requires an author", from: 1, to: 1, purpose: "eval", why: "x"},
		{name: "requires something to change", from: 1, to: 1, why: "x", who: "me"},
		{name: "refuses the genesis entry", from: 0, to: 1, purpose: "eval", why: "x", who: "me"},
		{name: "refuses seqs past the head", from: 1, to: 9, purpose: "eval", why: "x", who: "me"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := ops.Amend(s, c.from, c.to, c.purpose, "", c.why, c.who); err == nil {
				t.Errorf("Amend(%d..%d): want error", c.from, c.to)
			}
		})
	}
	e, err := ops.Amend(s, 1, 1, "eval-regression", "", "mis-tagged", "rafa")
	if err != nil || e.Str("new_purpose") != "eval-regression" {
		t.Fatalf("amend: %v %v", e, err)
	}
	s.Append(ledger.Entry{"type": ledger.TypeKeyEvent, "action": "enroll", "key_fp": "sha256:x"})
	if _, err := ops.Amend(s, 3, 3, "eval", "", "x", "me"); err == nil {
		t.Error("range without call entries should be refused")
	}
}

func TestKeyEvent(t *testing.T) {
	s := newStore(t)
	e, err := ops.KeyEvent(s, "enroll", "sk-secret", "", "openai", "prod", "", "")
	if err != nil || e.Str("key_fp") != ledger.KeyFingerprint("sk-secret") {
		t.Fatalf("enroll: %v %v", e, err)
	}
	b, _ := os.ReadFile(s.LedgerPath())
	if strings.Contains(string(b), "sk-secret") {
		t.Fatal("raw API key written to the ledger")
	}
	if _, err := ops.KeyEvent(s, "rotate", "", "sha256:abc", "openai", "", "", "sha256:def"); err != nil {
		t.Fatal(err)
	}
	if _, err := ops.KeyEvent(s, "delete", "k", "", "", "", "", ""); err == nil {
		t.Error("unknown action accepted")
	}
	if _, err := ops.KeyEvent(s, "revoke", "", "nope", "", "", "", ""); err == nil {
		t.Error("bad fingerprint accepted")
	}
}

func TestLink(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := t.TempDir()
	g := func(args ...string) {
		c := exec.Command("git", args...)
		c.Dir = repo
		c.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	g("init", "-q")
	os.MkdirAll(filepath.Join(repo, "evals"), 0o755)
	os.WriteFile(filepath.Join(repo, "evals", "run.py"), []byte("print(1)\n"), 0o644)
	g("add", ".")
	g("commit", "-qm", "evals")
	g("remote", "add", "origin", "https://user:token@example.com/acme/app.git")

	s := newStore(t)
	e, err := ops.Link(s, "eval-regression", repo, "evals")
	if err != nil {
		t.Fatal(err)
	}
	if e.Str("repo") != "https://example.com/acme/app.git" {
		t.Errorf("credentials not redacted from remote: %s", e.Str("repo"))
	}
	if e.Int("commit_count") != 1 || len(e.Str("commit")) != 40 || e.Bool("dirty") {
		t.Errorf("bad code_link: %v", e)
	}
	os.WriteFile(filepath.Join(repo, "x"), []byte("x"), 0o644)
	if e, _ := ops.Link(s, "eval-regression", repo, ""); !e.Bool("dirty") {
		t.Error("dirty tree not flagged")
	}
	if _, err := ops.Link(s, "x", t.TempDir(), ""); err == nil {
		t.Error("non-repo accepted")
	}
}

func TestReconcile(t *testing.T) {
	s := newStore(t)
	day := time.Date(2026, 3, 2, 10, 0, 0, 0, time.UTC)
	s.Append(
		ledger.Entry{"type": ledger.TypeCall, "ts": ledger.FormatTime(day), "provider": "openai", "tokens_in": int64(100), "tokens_out": int64(10)},
		ledger.Entry{"type": ledger.TypeCall, "ts": ledger.FormatTime(day.Add(time.Hour)), "provider": "openai", "tokens_in": int64(50), "tokens_out": int64(5)},
		ledger.Entry{"type": ledger.TypeCall, "ts": ledger.FormatTime(day.Add(24 * time.Hour)), "provider": "openai", "tokens_in": int64(7), "tokens_out": int64(1)},
		// A malformed ts must not panic.
		ledger.Entry{"type": ledger.TypeCall, "ts": "bad", "provider": "openai", "tokens_in": int64(1)},
	)
	csv := filepath.Join(t.TempDir(), "usage.csv")
	os.WriteFile(csv, []byte("date,model,input_tokens,output_tokens\n2026-03-02,m,150,15\n2026-03-03T00:00:00Z,m,7,1\n"), 0o600)
	e, lines, err := ops.Reconcile(s, "openai", csv, 0.01)
	if err != nil || e.Str("result") != "match" || len(lines) != 2 {
		t.Fatalf("want match: %v %v %v", e, lines, err)
	}
	os.WriteFile(csv, []byte("date,model,input_tokens,output_tokens\n2026-03-02,m,300,15\n"), 0o600)
	e, _, _ = ops.Reconcile(s, "openai", csv, 0.01)
	if e.Str("result") != "mismatch" || e.Int("days_mismatched") != 1 {
		t.Fatalf("want mismatch: %v", e)
	}
	os.WriteFile(csv, []byte("day,tokens\n"), 0o600)
	if _, _, err := ops.Reconcile(s, "openai", csv, 0.01); err == nil {
		t.Error("CSV without required columns accepted")
	}
	if r := verify(t, s, nil); r.OK() || !strings.Contains(strings.Join(r.Errors, " "), `unparseable ts "bad"`) {
		t.Fatalf("verify should flag the malformed ts: %v", r.Errors)
	}
}
