package cli_test

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/x509"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/devrdias/ambermoth/internal/cli"
	"github.com/devrdias/ambermoth/internal/ledger"
	"github.com/devrdias/ambermoth/internal/pack"
)

// verify-pack prints a report for every pack it can open. These tests pin the
// text and the JSON it prints, with the exit code and stderr, for packs built
// from the stored ledgers of internal/pack/testdata/golden and damaged copies
// of them. A change to how a pack is verified must not change a byte.
//
//	go test ./internal/cli -run TestVerifyPackOutputGolden -update
var updateOutput = flag.Bool("update", false, "rewrite the files in testdata/verify-pack")

const (
	packFixtures  = "../pack/testdata/golden"
	packOutputDir = "testdata/verify-pack"
)

type packOutputCase struct {
	name     string
	fixture  string
	days     [2]int // inclusive, from 2026-03-01
	disclose []int64
	edit     func(files map[string][]byte, m *pack.Manifest)
	rawZip   func(t *testing.T, good string) string
}

func packOutputCases() []packOutputCase {
	changeTokens := func(files map[string][]byte, _ *pack.Manifest) {
		files["ledger/segment.jsonl"] = bytes.Replace(files["ledger/segment.jsonl"], []byte(`"tokens_in":100`), []byte(`"tokens_in":1`), 1)
	}
	return []packOutputCase{
		{name: "honest whole ledger", fixture: "rich", days: [2]int{0, 60}},
		{name: "honest middle with a disclosure", fixture: "rich", days: [2]int{3, 5}, disclose: []int64{9}},
		{name: "honest without anchors", fixture: "plain", days: [2]int{0, 60}},
		{name: "honest with an unanchored tail", fixture: "rich", days: [2]int{15, 16}},
		{name: "changed entry", fixture: "rich", days: [2]int{3, 5}, edit: changeTokens},
		{name: "last line removed", fixture: "rich", days: [2]int{3, 5}, edit: func(files map[string][]byte, _ *pack.Manifest) {
			ls := bytes.SplitAfter(files["ledger/segment.jsonl"], []byte("\n"))
			files["ledger/segment.jsonl"] = bytes.Join(ls[:len(ls)-2], nil)
		}},
		{name: "end claimed anchored on a tail", fixture: "rich", days: [2]int{15, 16}, edit: func(_ map[string][]byte, m *pack.Manifest) { m.Segment.EndAnchored = true }},
		{name: "totals tampered", fixture: "rich", days: [2]int{3, 5}, edit: func(_ map[string][]byte, m *pack.Manifest) { m.Totals.All.Calls++ }},
		{name: "summary edited", fixture: "rich", days: [2]int{3, 5}, edit: func(files map[string][]byte, _ *pack.Manifest) {
			files["summary.html"] = append(files["summary.html"], " "...)
		}},
		{name: "disclosure key missing", fixture: "rich", days: [2]int{3, 5}, disclose: []int64{9}, edit: func(files map[string][]byte, _ *pack.Manifest) {
			files["disclosures/entry-keys.json"] = []byte("{}")
		}},
		{name: "segment line too long", fixture: "plain", days: [2]int{0, 60}, edit: func(files map[string][]byte, _ *pack.Manifest) {
			files["ledger/segment.jsonl"] = bytes.Repeat([]byte("a"), 17<<20)
		}},
		{name: "extra file", fixture: "plain", days: [2]int{0, 60}, edit: func(files map[string][]byte, _ *pack.Manifest) { files["extra.txt"] = []byte("x") }},
		{name: "segment missing", fixture: "plain", days: [2]int{0, 60}, edit: func(files map[string][]byte, _ *pack.Manifest) { delete(files, "ledger/segment.jsonl") }},
		{name: "unsafe entry name", fixture: "plain", days: [2]int{0, 60}, rawZip: func(t *testing.T, good string) string {
			files, err := pack.ReadPack(good)
			if err != nil {
				t.Fatal(err)
			}
			return zipWith(t, files, "../evil")
		}},
		{name: "truncated zip", fixture: "plain", days: [2]int{0, 60}, rawZip: func(t *testing.T, good string) string {
			b, err := os.ReadFile(good)
			if err != nil {
				t.Fatal(err)
			}
			out := filepath.Join(t.TempDir(), "cut.zip")
			if err := os.WriteFile(out, b[:len(b)/2], 0o600); err != nil {
				t.Fatal(err)
			}
			return out
		}},
	}
}

func TestVerifyPackOutputGolden(t *testing.T) {
	for _, c := range packOutputCases() {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			good, roots := buildFixturePack(t, c)
			path := good
			switch {
			case c.rawZip != nil:
				path = c.rawZip(t, good)
			case c.edit != nil:
				path = filepath.Join(t.TempDir(), "bad.zip")
				rewritePack(t, good, path, c.edit)
			}
			modes := []string{"text", "json"}
			for _, mode := range modes {
				args := []string{"verify-pack", path, "--tsa-ca", roots}
				if mode == "json" {
					args = append(args, "--json")
				}
				checkOutputGolden(t, strings.ReplaceAll(c.name, " ", "-")+"."+mode, runVerifyPack(args, path))
			}
		})
	}
}

// runVerifyPack runs the command and returns what it printed with the pack path hidden.
func runVerifyPack(args []string, path string) string {
	var out, errOut bytes.Buffer
	app := cli.New(cli.Env{Stdin: strings.NewReader(""), Stdout: &out, Stderr: &errOut,
		Getenv: func(string) string { return "" }, Clock: fixedClock, Logger: discardLogger(), Version: "test"})
	code := app.Run(context.Background(), args)
	return strings.ReplaceAll(fmt.Sprintf("exit %d\n--- stdout\n%s--- stderr\n%s", code, out.String(), errOut.String()), path, "<pack>")
}

func checkOutputGolden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join(packOutputDir, name+".txt")
	if *updateOutput {
		if err := os.MkdirAll(packOutputDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got != string(want) {
		t.Errorf("%s differs from the golden output\n got:\n%s\nwant:\n%s", name, got, want)
	}
}

// buildFixturePack builds the pack of c from a stored ledger and returns its path and the PEM file of its TSA root.
func buildFixturePack(t *testing.T, c packOutputCase) (pack_, rootsPEM string) {
	t.Helper()
	dir := t.TempDir()
	copyFixture(t, filepath.Join(packFixtures, c.fixture), dir)
	rootsPEM = filepath.Join(dir, "roots.pem")
	pem, err := os.ReadFile(rootsPEM)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(pem)
	day0 := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	now := day0.AddDate(0, 0, 40)
	s, err := ledger.Open(dir, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	o := pack.Options{From: day0.AddDate(0, 0, c.days[0]), To: day0.AddDate(0, 0, c.days[1]+1), Roots: roots,
		Generator: "ambermoth/test", Out: filepath.Join(t.TempDir(), "pack.zip"), Disclose: c.disclose}
	if _, err := pack.Build(context.Background(), s, o); err != nil {
		t.Fatalf("Build: %v", err)
	}
	return o.Out, rootsPEM
}

func copyFixture(t *testing.T, src, dst string) {
	t.Helper()
	err := filepath.Walk(src, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		if info.Name() == "ledger.lock" {
			return nil
		}
		if info.IsDir() {
			return os.MkdirAll(filepath.Join(dst, rel), 0o700)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(dst, rel), b, 0o600)
	})
	if err != nil {
		t.Fatal(err)
	}
}

// zipWith writes files and one more entry, named extra, into a new zip.
func zipWith(t *testing.T, files map[string][]byte, extra string) string {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range append(names, extra) {
		w, err := zw.Create(n)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(files[n]); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "unsafe.zip")
	writeFile(t, path, buf.String())
	return path
}
