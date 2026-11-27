package pack_test

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/devrdias/ambermoth/internal/ledger"
	"github.com/devrdias/ambermoth/internal/ops"
	"github.com/devrdias/ambermoth/internal/pack"
	"github.com/devrdias/ambermoth/internal/tsa/tsatest"
)

// packFixture is a pack whose segment starts at an anchor after a key event,
// so it carries a context entry.
type packFixture struct {
	path string
	vo   pack.VerifyOptions
}

func newPackFixture(t *testing.T) packFixture {
	t.Helper()
	auth := tsatest.New(t)
	srv := auth.Server(t)
	s, err := ledger.Init(t.TempDir(), "test", time.Now)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if _, err := ops.KeyEvent(s, "enroll", "sk-1", "", "openai", "prod", "initial", ""); err != nil {
		t.Fatal(err)
	}
	call(s, now.Add(-72*time.Hour), "production-inference", "old")
	if _, err := ops.Anchor(context.Background(), s, []string{srv.URL}, nil); err != nil {
		t.Fatal(err)
	}
	call(s, now.Add(-time.Hour), "production-inference", "new")
	if _, err := ops.Anchor(context.Background(), s, []string{srv.URL}, nil); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "pack.zip")
	from := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC).AddDate(0, 0, -1)
	if _, err := pack.Build(context.Background(), s, pack.Options{From: from, To: from.AddDate(0, 0, 3),
		Roots: auth.Roots, Out: path, Generator: "test"}); err != nil {
		t.Fatal(err)
	}
	return packFixture{path: path, vo: pack.VerifyOptions{Clock: time.Now, Roots: auth.Roots, Generator: "test"}}
}

// rewrite applies edit to a copy of the pack, then re-hashes every listed file
// in the manifest, so only the check under test can notice the edit.
func (f packFixture) rewrite(t *testing.T, edit func(files map[string][]byte, m *pack.Manifest)) string {
	t.Helper()
	files, err := pack.ReadPack(f.path)
	if err != nil {
		t.Fatal(err)
	}
	var m pack.Manifest
	if err := json.Unmarshal(files["manifest.json"], &m); err != nil {
		t.Fatal(err)
	}
	edit(files, &m)
	for i := range m.Files {
		if b, ok := files[m.Files[i].Path]; ok {
			m.Files[i].SHA256 = ledger.SHA256Bytes(b)
		}
	}
	mb, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	files["manifest.json"] = mb
	delete(files, "manifest.tsr")
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	out := filepath.Join(t.TempDir(), "edited.zip")
	if err := pack.WriteZip(out, names, files, time.Now()); err != nil {
		t.Fatal(err)
	}
	return out
}

type tamperCase struct {
	name    string
	edit    func(files map[string][]byte, m *pack.Manifest)
	wantErr string
}

func TestVerifyPackRejects(t *testing.T) {
	f := newPackFixture(t)
	r, err := pack.VerifyPack(f.path, f.vo)
	if err != nil || !r.OK() {
		t.Fatalf("fixture pack does not verify: %v %+v", err, r)
	}
	cases := []tamperCase{
		{
			name:    "an unsupported format",
			edit:    func(_ map[string][]byte, m *pack.Manifest) { m.Format = "ambermoth-pack/99" },
			wantErr: "unsupported pack format",
		},
		{
			name:    "a listed file that is missing",
			edit:    func(files map[string][]byte, _ *pack.Manifest) { delete(files, "LIMITS.md") },
			wantErr: "LIMITS.md: listed in manifest but missing",
		},
		{
			name:    "a file the manifest does not list",
			edit:    func(files map[string][]byte, _ *pack.Manifest) { files["extra.txt"] = []byte("x") },
			wantErr: "extra.txt: present in zip but not listed",
		},
		{
			name:    "a genesis from another ledger",
			edit:    func(_ map[string][]byte, m *pack.Manifest) { m.LedgerID = "someone-else" },
			wantErr: "does not match manifest someone-else",
		},
		{
			name:    "a malformed period",
			edit:    func(_ map[string][]byte, m *pack.Manifest) { m.Period.From = "last week" },
			wantErr: "is not two YYYY-MM-DD dates",
		},
		{
			name: "an altered context entry",
			edit: func(files map[string][]byte, _ *pack.Manifest) {
				files["ledger/context.jsonl"] = bytes.Replace(files["ledger/context.jsonl"], []byte(`"prod"`), []byte(`"test"`), 1)
			},
			wantErr: "hash mismatch",
		},
		{
			name: "an unparseable context line",
			edit: func(files map[string][]byte, _ *pack.Manifest) {
				files["ledger/context.jsonl"] = append(files["ledger/context.jsonl"], []byte("not json\n")...)
			},
			wantErr: "context.jsonl",
		},
		{
			name:    "a segment start that differs from the manifest",
			edit:    func(_ map[string][]byte, m *pack.Manifest) { m.Segment.FirstSeq++ },
			wantErr: "segment start does not match manifest",
		},
	}
	for _, c := range cases {
		t.Run("rejects "+c.name, func(t *testing.T) {
			r, err := pack.VerifyPack(f.rewrite(t, c.edit), f.vo)
			if err != nil {
				t.Fatalf("VerifyPack: %v", err)
			}
			all := strings.Join(r.Errors, "\n")
			if r.OK() || !strings.Contains(all, c.wantErr) {
				t.Fatalf("want an error containing %q, got OK=%v errors:\n%s", c.wantErr, r.OK(), all)
			}
		})
	}
}

func TestVerifyPackWarnsAboutAnotherVerifier(t *testing.T) {
	f := newPackFixture(t)
	vo := f.vo
	vo.Generator = "ambermoth/other"
	r, err := pack.VerifyPack(f.path, vo)
	if err != nil {
		t.Fatal(err)
	}
	if r.SummaryMatches || !strings.Contains(strings.Join(r.Warnings, " "), "cannot re-render it byte for byte") {
		t.Fatalf("want a re-render warning, got %v", r.Warnings)
	}
}
