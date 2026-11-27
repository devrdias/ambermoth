package cli_test

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/devrdias/ambermoth/internal/cli"
	"github.com/devrdias/ambermoth/internal/ledger"
	"github.com/devrdias/ambermoth/internal/pack"
	"github.com/devrdias/ambermoth/internal/tsa/tsatest"
)

// rewritePack copies the pack at src to dst after edit changed its files or
// manifest. The manifest's file hashes are left as they are.
func rewritePack(t *testing.T, src, dst string, edit func(files map[string][]byte, m *pack.Manifest)) {
	t.Helper()
	files, err := pack.ReadPack(src)
	if err != nil {
		t.Fatal(err)
	}
	var m pack.Manifest
	if err := json.Unmarshal(files["manifest.json"], &m); err != nil {
		t.Fatal(err)
	}
	edit(files, &m)
	mb, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	files["manifest.json"] = append(mb, '\n')
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for n, b := range files {
		w, err := zw.Create(n)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(b); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	writeFile(t, dst, buf.String())
}

type badPackRun struct {
	name     string
	edit     func(files map[string][]byte, m *pack.Manifest)
	wantLine string // an exact line of the report
}

func dropToken(files map[string][]byte, m *pack.Manifest) {
	var kept []pack.File
	for _, f := range m.Files {
		if strings.HasPrefix(f.Path, "anchors/") {
			delete(files, f.Path)
			continue
		}
		kept = append(kept, f)
	}
	m.Files = kept
}

func TestVerifyPackExitsWithFailureAndNamesTheError(t *testing.T) {
	t.Parallel()
	auth := tsatest.New(t)
	h := recordedHarness(t)
	useTSAs(t, h.dir, auth.Server(t).URL)
	ca := h.workFile("ca.pem")
	writeFile(t, ca, string(auth.CAPEM))
	h.check(t, runCase{args: []string{"anchor"}, wantCode: cli.ExitOK})
	good := h.workFile("good.zip")
	h.check(t, runCase{args: []string{"pack", "--from", "2026-03-01", "--to", "2026-03-31", "--no-tsa", "--tsa-ca", ca, "--out", good}, wantCode: cli.ExitOK})
	h.check(t, runCase{args: []string{"verify-pack", good, "--tsa-ca", ca}, wantCode: cli.ExitOK, wantOut: "Evidence pack verification: OK"})

	cases := []badPackRun{
		{"a first entry that does not link to what the manifest says",
			func(_ map[string][]byte, m *pack.Manifest) { m.Segment.FirstPrevHash = ledger.ZeroHash },
			"  ERROR: segment start does not match manifest"},
		{"a last entry that is not the manifest's head",
			func(_ map[string][]byte, m *pack.Manifest) { m.Segment.LastHash = ledger.ZeroHash },
			"  ERROR: segment end does not match manifest"},
		{"a start the manifest does not claim anchored",
			func(_ map[string][]byte, m *pack.Manifest) { m.Segment.StartAnchored = false },
			"  ERROR: segment neither starts at genesis nor at an anchored entry"},
		{"a changed entry",
			func(files map[string][]byte, _ *pack.Manifest) {
				files["ledger/segment.jsonl"] = bytes.Replace(files["ledger/segment.jsonl"], []byte(`"tokens_in":100`), []byte(`"tokens_in":1`), 1)
			},
			"  ERROR: seq 2: entry_hash mismatch (entry altered after writing)"},
		{"a changed entry the manifest still lists with its old hash",
			func(files map[string][]byte, _ *pack.Manifest) {
				files["ledger/segment.jsonl"] = bytes.Replace(files["ledger/segment.jsonl"], []byte(`"tokens_in":100`), []byte(`"tokens_in":1`), 1)
			},
			"  ERROR: ledger/segment.jsonl: SHA-256 does not match manifest (file altered)"},
		{"a token file missing from the pack", dropToken,
			"  ERROR: manifest claims the segment start is anchored, but no valid anchor covers seq 2"},
		{"a token file missing, as the ledger reports it", dropToken,
			"  ERROR: anchor seq 3: token file missing: 2-0-"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			bad := filepath.Join(t.TempDir(), "bad.zip")
			rewritePack(t, good, bad, c.edit)
			code, out, errOut := h.run("verify-pack", bad, "--tsa-ca", ca)
			if code != cli.ExitFail || !strings.Contains(out, "Evidence pack verification: FAILED") || !strings.Contains(out, c.wantLine) {
				t.Errorf("exit %d, want %d; want the line %q in:\n%s\nstderr: %s", code, cli.ExitFail, c.wantLine, out, errOut)
			}
			if errOut != "" {
				t.Errorf("stderr = %q, want nothing: a failed pack is a report, not a command error", errOut)
			}
		})
	}
}

func TestVerifyPackFailsOnASegmentLineTooLongToRead(t *testing.T) {
	t.Parallel()
	h := recordedHarness(t)
	good, bad := h.workFile("good.zip"), h.workFile("bad.zip")
	h.check(t, runCase{args: []string{"pack", "--from", "2026-03-01", "--to", "2026-03-31", "--no-tsa", "--out", good}, wantCode: cli.ExitOK})
	rewritePack(t, good, bad, func(files map[string][]byte, _ *pack.Manifest) {
		files["ledger/segment.jsonl"] = bytes.Repeat([]byte("a"), 17<<20)
	})
	h.check(t, runCase{args: []string{"verify-pack", bad}, wantCode: cli.ExitFail, wantErr: "error: bufio.Scanner: token too long"})
}
