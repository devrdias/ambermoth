package pack_test

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/json"
	"github.com/devrdias/ambermoth/internal/pack"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/devrdias/ambermoth/internal/ledger"
	"github.com/devrdias/ambermoth/internal/ops"
	"github.com/devrdias/ambermoth/internal/trust"
	"github.com/devrdias/ambermoth/internal/tsa/tsatest"
)

func call(s *ledger.Store, ts time.Time, purpose, body string) ledger.Entry {
	salt := ledger.NewSaltID()
	ek := ledger.EntryKey(s.MasterKey, salt)
	m := hmac.New(sha256.New, ek)
	m.Write([]byte(body))
	e := ledger.Entry{"type": ledger.TypeCall, "ts": ledger.FormatTime(ts), "provider": "anthropic",
		"api_style": "anthropic", "endpoint": "messages", "model": "claude-test", "status": 200,
		"tokens_in": 1000, "tokens_out": 200, "cached_in": 0, "key_fp": ledger.KeyFingerprint("sk-1"),
		"purpose": purpose, "salt_id": salt, "prompt_hmac": ledger.FormatHMAC(m.Sum(nil)),
		"output_hmac": ledger.FormatHMAC(m.Sum(nil))}
	if err := s.Append(e); err != nil {
		panic(err)
	}
	return e
}

func TestBuildAndVerifyPack(t *testing.T) {
	auth := tsatest.New(t)
	srv := auth.Server(t)
	s, err := ledger.Init(t.TempDir(), "test", time.Now)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if _, err := ops.KeyEvent(s, "enroll", "sk-1", "", "anthropic", "prod", "initial", ""); err != nil {
		t.Fatal(err)
	}
	call(s, now.Add(-5*time.Hour), "production-inference", "a")
	if _, err := ops.Anchor(context.Background(), s, []string{srv.URL}, nil); err != nil {
		t.Fatal(err)
	}
	var target ledger.Entry
	for i := 0; i < 10; i++ {
		e := call(s, now.Add(-4*time.Hour+time.Duration(i)*time.Minute), "untagged", `{"q":`+string(rune('0'+i))+`}`)
		if i == 3 {
			target = e
		}
	}
	if _, err := ops.Amend(s, target.Seq()-3, target.Seq()+6, "eval-regression", "", "nightly benchmark was untagged", "rafa"); err != nil {
		t.Fatal(err)
	}
	if _, err := ops.Anchor(context.Background(), s, []string{srv.URL}, nil); err != nil {
		t.Fatal(err)
	}
	call(s, now.Add(-time.Minute), "production-inference", "tail") // unanchored tail

	out := filepath.Join(t.TempDir(), "pack.zip")
	from := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC).AddDate(0, 0, -1)
	m, err := pack.Build(context.Background(), s, pack.Options{From: from, To: from.AddDate(0, 0, 3), Provider: "anthropic", Account: "acme",
		Disclose: []int64{target.Seq()}, TSAs: []string{srv.URL}, Roots: auth.Roots, Out: out, Generator: "test"})
	if err != nil {
		t.Fatal(err)
	}
	if m.Totals.All.Calls != 12 || m.Totals.ByPurpose["eval-regression"].Calls != 10 || m.Totals.ByPurpose["eval-regression"].Retagged != 10 {
		b, _ := json.MarshalIndent(m.Totals.ByPurpose, "", " ")
		t.Fatalf("unexpected totals: %d %s", m.Totals.All.Calls, b)
	}
	vo := pack.VerifyOptions{Clock: time.Now, Roots: auth.Roots, Generator: "test"}
	r, err := pack.VerifyPack(out, vo)
	if err != nil {
		t.Fatal(err)
	}
	if !r.SummaryMatches || !bytes.Contains(r.ReportHTML, []byte("Verified</span></strong> by test")) {
		t.Errorf("expected matching summary and a verified report banner")
	}
	files0, _ := pack.ReadPack(out)
	if !bytes.Contains(files0["summary.html"], []byte("before the segment · self-attested")) {
		t.Errorf("key event before the segment should be labelled self-attested")
	}
	if !r.OK() {
		t.Fatalf("pack should verify: %v %v", r.Errors, r.Ledger.Errors)
	}
	if !strings.Contains(strings.Join(r.Notes, " "), "manifest timestamped") {
		t.Errorf("expected manifest timestamp note, got %v", r.Notes)
	}
	// Disclosed sample checks.
	okReq, _, err := pack.CheckSample(out, target.Seq(), []byte(`{"q":3}`), nil)
	if err != nil || !okReq {
		t.Fatalf("disclosed sample should match: %v", err)
	}
	if okReq, _, _ := pack.CheckSample(out, target.Seq(), []byte(`{"q":4}`), nil); okReq {
		t.Fatal("wrong sample should not match")
	}

	// Tampering: change totals in the manifest and fix up nothing else.
	tamper := func(name string, f func([]byte) []byte) string {
		files, _ := pack.ReadPack(out)
		files[name] = f(files[name])
		p := filepath.Join(t.TempDir(), "t.zip")
		var names []string
		for n := range files {
			names = append(names, n)
		}
		if err := pack.WriteZip(p, names, files, time.Now()); err != nil {
			t.Fatal(err)
		}
		return p
	}
	cases := map[string]string{
		"summary edit": tamper("summary.html", func(b []byte) []byte {
			return bytes.Replace(b, []byte("eval-regression"), []byte("production-inference"), 1)
		}),
		"segment edit": tamper("ledger/segment.jsonl", func(b []byte) []byte {
			return bytes.Replace(b, []byte(`"tokens_in":1000`), []byte(`"tokens_in":10`), 1)
		}),
		"totals edit": tamper("manifest.json", func(b []byte) []byte {
			return bytes.Replace(b, []byte(`"calls": 12`), []byte(`"calls": 2`), 1)
		}),
	}
	for name, p := range cases {
		r, err := pack.VerifyPack(p, vo)
		if err == nil && r.OK() {
			t.Errorf("%s: tampering not detected", name)
		}
	}
	// A pack built and then re-hashed consistently but with fudged totals is still caught.
	files, _ := pack.ReadPack(out)
	var mm pack.Manifest
	if err := json.Unmarshal(files["manifest.json"], &mm); err != nil {
		t.Fatal(err)
	}
	mm.Totals.All.TokensIn = 1
	nb, _ := json.MarshalIndent(mm, "", "  ")
	files["manifest.json"] = nb
	delete(files, "manifest.tsr")
	p := filepath.Join(t.TempDir(), "fudge.zip")
	var names []string
	for n := range files {
		names = append(names, n)
	}
	if err := pack.WriteZip(p, names, files, time.Now()); err != nil {
		t.Fatal(err)
	}
	if r, _ := pack.VerifyPack(p, vo); r.OK() {
		t.Error("fudged totals not detected")
	}

	// Editing summary.html AND updating its hash in the manifest is caught by
	// the re-render check.
	files, _ = pack.ReadPack(out)
	files["summary.html"] = bytes.ReplaceAll(files["summary.html"], []byte("eval-regression"), []byte("production-inference"))
	if err := json.Unmarshal(files["manifest.json"], &mm); err != nil {
		t.Fatal(err)
	}
	for i := range mm.Files {
		if mm.Files[i].Path == "summary.html" {
			mm.Files[i].SHA256 = ledger.SHA256Bytes(files["summary.html"])
		}
	}
	files["manifest.json"], _ = json.MarshalIndent(mm, "", "  ")
	delete(files, "manifest.tsr")
	names = names[:0]
	for n := range files {
		names = append(names, n)
	}
	p2 := filepath.Join(t.TempDir(), "rehashed.zip")
	if err := pack.WriteZip(p2, names, files, time.Now()); err != nil {
		t.Fatal(err)
	}
	if r, _ := pack.VerifyPack(p2, vo); r.OK() || r.SummaryMatches {
		t.Error("summary edited and re-hashed was not detected")
	}

	// A TSA that does not chain to a pinned root fails by default,
	// and passes only with AllowUnpinned, with a warning.
	pinnedOnly, _ := trust.Pool(nil)
	if r, _ := pack.VerifyPack(out, pack.VerifyOptions{Clock: time.Now, Roots: pinnedOnly, Generator: "test"}); r.OK() {
		t.Error("anchors from an unpinned TSA were accepted")
	}
	r2, _ := pack.VerifyPack(out, pack.VerifyOptions{Clock: time.Now, Roots: pinnedOnly, AllowUnpinned: true, Generator: "test"})
	if !r2.OK() || !strings.Contains(strings.Join(r2.Warnings, " "), "NOT pinned") {
		t.Errorf("AllowUnpinned should pass with a warning: %v %v", r2.Errors, r2.Warnings)
	}
	if r, _ := pack.VerifyPack(out, pack.VerifyOptions{Clock: time.Now, Generator: "test"}); r.OK() {
		t.Error("verification without any trust roots must fail")
	}
}
