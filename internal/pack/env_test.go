package pack_test

import (
	"archive/zip"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"path/filepath"
	"testing"
	"time"

	"github.com/devrdias/ambermoth/internal/ledger"
	"github.com/devrdias/ambermoth/internal/ops"
	"github.com/devrdias/ambermoth/internal/pack"
	"github.com/devrdias/ambermoth/internal/tsa/tsatest"
)

var day0 = time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)

// env is a ledger on a clock the test moves by hand, with two test TSAs.
type env struct {
	t    *testing.T
	s    *ledger.Store
	auth *tsatest.TestAuthority
	urls []string
	now  time.Time
}

func newEnv(t *testing.T) *env {
	t.Helper()
	e := &env{t: t, auth: tsatest.New(t), now: day0}
	e.urls = []string{e.auth.Server(t).URL, e.auth.Server(t).URL}
	s, err := ledger.Init(t.TempDir(), "test", func() time.Time { return e.now })
	if err != nil {
		t.Fatal(err)
	}
	e.s = s
	return e
}

func (e *env) clock() time.Time { return e.now }

// on moves the clock to noon of the given day (0 is 2026-03-01).
func (e *env) on(day int) *env {
	e.now = day0.AddDate(0, 0, day).Add(12 * time.Hour)
	return e
}

func (e *env) append(en ledger.Entry) ledger.Entry {
	e.t.Helper()
	if err := e.s.Append(en); err != nil {
		e.t.Fatal(err)
	}
	return en
}

func (e *env) call(day int, provider, purpose string) ledger.Entry {
	return e.callBody(day, provider, purpose, "body")
}

func (e *env) callBody(day int, provider, purpose, body string) ledger.Entry {
	e.on(day)
	salt := ledger.NewSaltID()
	m := hmac.New(sha256.New, ledger.EntryKey(e.s.MasterKey, salt))
	m.Write([]byte(body))
	h := ledger.FormatHMAC(m.Sum(nil))
	return e.append(ledger.Entry{"type": ledger.TypeCall, "provider": provider, "api_style": provider,
		"endpoint": "messages", "model": "m-" + provider, "status": 200, "tokens_in": 100, "tokens_out": 20,
		"cached_in": 0, "key_fp": ledger.KeyFingerprint("sk-1"), "purpose": purpose, "salt_id": salt,
		"prompt_hmac": h, "output_hmac": h})
}

func (e *env) keyEvent(day int) ledger.Entry {
	e.t.Helper()
	e.on(day)
	en, err := ops.KeyEvent(e.s, "enroll", "sk-1", "", "anthropic", "prod", "initial", "")
	if err != nil {
		e.t.Fatal(err)
	}
	return en
}

func (e *env) link(day int, purpose, repo, commit, firstTs string) ledger.Entry {
	e.on(day)
	return e.append(ledger.Entry{"type": ledger.TypeCodeLink, "purpose": purpose, "repo": repo, "commit": commit,
		"first_commit_ts": firstTs, "commit_count": 3, "path": "", "dirty": false})
}

func (e *env) reconcile(day int) ledger.Entry {
	e.on(day)
	return e.append(ledger.Entry{"type": ledger.TypeReconciliation, "provider": "anthropic", "result": "match"})
}

func (e *env) gap(day int) ledger.Entry {
	e.on(day)
	return e.append(ledger.Entry{"type": ledger.TypeGap, "dropped": 2})
}

func (e *env) amend(day int, from, to int64, purpose string) ledger.Entry {
	e.t.Helper()
	e.on(day)
	// Appended directly: the fixtures use short tags that ops.Amend refuses.
	return e.append(ledger.Entry{"type": ledger.TypeAmendment, "target_from": from, "target_to": to,
		"new_purpose": purpose, "reason": "retagged", "by": "rafa"})
}

// anchor anchors the head with the first n TSAs, one anchor entry each.
func (e *env) anchor(day, n int) []ledger.Entry {
	e.t.Helper()
	e.on(day)
	es, err := ops.Anchor(context.Background(), e.s, e.urls[:n], nil)
	if err != nil {
		e.t.Fatal(err)
	}
	return es
}

// opts covers the whole test ledger, with the output in a new temp directory.
func (e *env) opts() pack.Options {
	return pack.Options{From: day0, To: day0.AddDate(0, 0, 60), Roots: e.auth.Roots,
		Out: filepath.Join(e.t.TempDir(), "pack.zip"), Generator: "test"}
}

// period sets o to the days from..to, inclusive.
func period(o pack.Options, from, to int) pack.Options {
	o.From, o.To = day0.AddDate(0, 0, from), day0.AddDate(0, 0, to+1)
	return o
}

func (e *env) build(o pack.Options) *pack.Manifest {
	e.t.Helper()
	e.on(40)
	m, err := pack.Build(context.Background(), e.s, o)
	if err != nil {
		e.t.Fatalf("Build: %v", err)
	}
	return m
}

func (e *env) verifyOptions() pack.VerifyOptions {
	return pack.VerifyOptions{Clock: e.clock, Roots: e.auth.Roots, Generator: "test"}
}

func (e *env) verify(path string) *pack.PackReport {
	e.t.Helper()
	r, err := pack.VerifyPack(path, e.verifyOptions())
	if err != nil {
		e.t.Fatalf("VerifyPack: %v", err)
	}
	return r
}

// zipNames lists the files of a zip in archive order.
func zipNames(t *testing.T, path string) []string {
	t.Helper()
	zr, err := zip.OpenReader(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = zr.Close() }()
	var names []string
	for _, f := range zr.File {
		names = append(names, f.Name)
	}
	return names
}

func readPack(t *testing.T, path string) map[string][]byte {
	t.Helper()
	files, err := pack.ReadPack(path)
	if err != nil {
		t.Fatal(err)
	}
	return files
}
