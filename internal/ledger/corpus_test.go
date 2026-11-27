package ledger_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/devrdias/ambermoth/internal/ledger"
)

// The corpus pins what Verify reports for a ledger and damaged copies of it.
// The reports live in testdata/verifycorpus/*.json. A change to the verifier
// that moves any of them is a change in what users see: run with -update to
// rewrite them, and read the diff.
var updateCorpus = flag.Bool("update", false, "rewrite the verify corpus and its reports")

const corpusDir = "testdata/verifycorpus"

// corpusResult is what a corpus case pins: the report, the seqs of the gap
// entries (not in the JSON of a Report) and the error Read gave, if any.
type corpusResult struct {
	ReadError string
	GapSeqs   []int64
	Report    *ledger.Report
}

type corpusOptions struct {
	skipChecks bool
	loadToken  func(name string) ([]byte, error)
}

type corpusCase struct {
	name   string
	mutate func(base []byte) []byte
	opts   corpusOptions
}

// tokenGenTime reads the TSA time out of a corpus token, which is the text
// "gen=<RFC 3339 time>". Any other token fails to verify.
func tokenGenTime(tok []byte) (time.Time, error) {
	s, ok := strings.CutPrefix(string(tok), "gen=")
	if !ok {
		return time.Time{}, errors.New("bad token")
	}
	return time.Parse(time.RFC3339, s)
}

func corpusCheck(tok []byte, _ string) (ledger.AnchorCheck, error) {
	gen, err := tokenGenTime(tok)
	if err != nil {
		return ledger.AnchorCheck{}, err
	}
	return ledger.AnchorCheck{GenTime: gen, Signer: "corpus", Pinned: true}, nil
}

func corpusLines(base []byte) [][]byte {
	ls := bytes.SplitAfter(base, []byte("\n"))
	return ls[:len(ls)-1] // SplitAfter leaves an empty tail after the last newline
}

func joinCorpus(ls ...[]byte) []byte { return bytes.Join(ls, nil) }

// withLine returns base with line i replaced by s plus a newline.
func withLine(base []byte, i int, s string) []byte {
	ls := corpusLines(base)
	ls[i] = []byte(s + "\n")
	return joinCorpus(ls...)
}

func withoutLine(base []byte, i int) []byte {
	ls := corpusLines(base)
	return joinCorpus(append(ls[:i:i], ls[i+1:]...)...)
}

func replaceOnce(old, repl string) func([]byte) []byte {
	return func(b []byte) []byte {
		if !bytes.Contains(b, []byte(old)) {
			panic("corpus base has no " + old)
		}
		return bytes.Replace(b, []byte(old), []byte(repl), 1)
	}
}

var corpusCases = []corpusCase{
	{name: "base", mutate: func(b []byte) []byte { return b }},
	{name: "base-no-token-checks", mutate: func(b []byte) []byte { return b }, opts: corpusOptions{skipChecks: true}},
	{name: "empty-file", mutate: func([]byte) []byte { return nil }},
	{name: "only-blank-lines", mutate: func([]byte) []byte { return []byte("\n\n  \n") }},
	{name: "blank-lines-between-entries", mutate: func(b []byte) []byte {
		ls := corpusLines(b)
		return joinCorpus(ls[0], []byte("\n"), ls[1], []byte("  \n\n"), joinCorpus(ls[2:]...))
	}},
	{name: "trailing-blank-lines", mutate: func(b []byte) []byte { return append(append([]byte{}, b...), "\n\n"...) }},
	{name: "no-final-newline", mutate: func(b []byte) []byte { return bytes.TrimSuffix(b, []byte("\n")) }},
	{name: "crlf-line-endings", mutate: func(b []byte) []byte { return bytes.ReplaceAll(b, []byte("\n"), []byte("\r\n")) }},
	{name: "truncated-last-line", mutate: func(b []byte) []byte { return b[:len(b)-40] }},
	{name: "truncated-first-line", mutate: func(b []byte) []byte { return b[40:] }},
	{name: "edited-value", mutate: replaceOnce(`"tokens_in":300`, `"tokens_in":301`)},
	{name: "non-canonical-spacing", mutate: replaceOnce(`{"entry_hash"`, `{ "entry_hash"`)},
	{name: "deleted-first-line", mutate: func(b []byte) []byte { return withoutLine(b, 0) }},
	{name: "deleted-middle-line", mutate: func(b []byte) []byte { return withoutLine(b, 4) }},
	{name: "deleted-anchor-target", mutate: func(b []byte) []byte { return withoutLine(b, 5) }},
	{name: "deleted-last-line", mutate: func(b []byte) []byte { return withoutLine(b, len(corpusLines(b))-1) }},
	{name: "deleted-two-lines", mutate: func(b []byte) []byte { return withoutLine(withoutLine(b, 3), 3) }},
	{name: "reordered-lines", mutate: func(b []byte) []byte {
		ls := corpusLines(b)
		ls[6], ls[7] = ls[7], ls[6]
		return joinCorpus(ls...)
	}},
	{name: "duplicated-line", mutate: func(b []byte) []byte {
		ls := corpusLines(b)
		return joinCorpus(append(ls[:7:7], append([][]byte{ls[6]}, ls[7:]...)...)...)
	}},
	{name: "non-object-array", mutate: func(b []byte) []byte { return withLine(b, 3, `[1,2]`) }},
	{name: "non-object-null-first", mutate: func(b []byte) []byte { return withLine(b, 0, `null`) }},
	{name: "non-object-text-last", mutate: func(b []byte) []byte { return withLine(b, len(corpusLines(b))-1, `not json`) }},
	{name: "empty-object", mutate: func(b []byte) []byte { return withLine(b, 3, `{}`) }},
	{name: "missing-entry-hash", mutate: replaceOnce(`"entry_hash":"sha256:`, `"entry_hasx":"sha256:`)},
	{name: "genesis-with-bad-prev-hash", mutate: func(b []byte) []byte {
		ls := corpusLines(b)
		first := bytes.Replace(ls[0], []byte(`"prev_hash":"sha256:0`), []byte(`"prev_hash":"sha256:1`), 1)
		return joinCorpus(append([][]byte{first}, ls[1:]...)...)
	}},
	{name: "starts-after-genesis", mutate: func(b []byte) []byte { return joinCorpus(corpusLines(b)[1:]...) }},
	{name: "anchor-target-hash-altered", mutate: replaceOnce(`"target_hash":"sha256:`, `"target_hash":"sha256:0`)},
	{name: "anchor-target-out-of-range", mutate: replaceOnce(`"target_seq":14`, `"target_seq":999`)},
	{name: "anchor-token-file-with-path", mutate: replaceOnce(`"token_file":"tok-c.tsr"`, `"token_file":"../tok-c.tsr"`)},
	{name: "token-missing", mutate: func(b []byte) []byte { return b }, opts: corpusOptions{
		loadToken: func(name string) ([]byte, error) {
			if name == "tok-b.tsr" {
				return nil, os.ErrNotExist
			}
			return os.ReadFile(filepath.Join(corpusDir, "anchors", name))
		}}},
	{name: "token-bytes-altered", mutate: func(b []byte) []byte { return b }, opts: corpusOptions{
		loadToken: func(name string) ([]byte, error) {
			tok, err := os.ReadFile(filepath.Join(corpusDir, "anchors", name))
			if name == "tok-c.tsr" {
				tok = append(tok, ' ')
			}
			return tok, err
		}}},
	{name: "token-does-not-verify", mutate: func(b []byte) []byte { return b }, opts: corpusOptions{
		loadToken: func(name string) ([]byte, error) {
			if name == "tok-a.tsr" {
				return []byte("fail"), nil
			}
			return os.ReadFile(filepath.Join(corpusDir, "anchors", name))
		}}},
	{name: "line-over-the-limit", mutate: func(b []byte) []byte {
		ls := corpusLines(b)
		big := bytes.Repeat([]byte("x"), ledger.MaxLineBytes+1)
		return joinCorpus(append(ls[:3:3], append([][]byte{append(big, '\n')}, ls[3:]...)...)...)
	}},
	{name: "line-just-under-the-limit", mutate: func(b []byte) []byte {
		ls := corpusLines(b)
		big := append([]byte(`"`), bytes.Repeat([]byte("x"), ledger.MaxLineBytes-3)...)
		big = append(big, '"', '\n')
		return joinCorpus(append(ls[:3:3], append([][]byte{big}, ls[3:]...)...)...)
	}},
}

func runCorpusCase(t *testing.T, c corpusCase, base []byte) corpusResult {
	t.Helper()
	var res corpusResult
	es, lines, err := ledger.Read(bytes.NewReader(c.mutate(base)))
	if err != nil {
		res.ReadError = err.Error()
	}
	opt := ledger.VerifyOptions{AnchorDir: filepath.Join(corpusDir, "anchors"), LoadToken: c.opts.loadToken}
	if !c.opts.skipChecks {
		opt.CheckAnchor = corpusCheck
	}
	res.Report = ledger.Verify(es, lines, opt)
	for _, g := range res.Report.Gaps {
		res.GapSeqs = append(res.GapSeqs, g.Seq())
	}
	return res
}

func marshalCorpus(t *testing.T, r corpusResult) []byte {
	t.Helper()
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return append(b, '\n')
}

func TestVerifyCorpus(t *testing.T) {
	if *updateCorpus {
		writeCorpusBase(t)
	}
	base, err := os.ReadFile(filepath.Join(corpusDir, "base.jsonl"))
	if err != nil {
		t.Fatalf("%v (run go test ./internal/ledger -run TestVerifyCorpus -update)", err)
	}
	for _, c := range corpusCases {
		t.Run(c.name, func(t *testing.T) {
			got := marshalCorpus(t, runCorpusCase(t, c, base))
			path := filepath.Join(corpusDir, c.name+".json")
			if *updateCorpus {
				if err := os.WriteFile(path, got, 0o644); err != nil {
					t.Fatal(err)
				}
				return
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, want) {
				t.Errorf("report changed for %s\ngot:\n%s\nwant:\n%s", c.name, got, want)
			}
		})
	}
}

// The corpus must hold what the cases assume: a base that verifies with
// warnings only, and a report for every case.
func TestVerifyCorpusBaseIsSound(t *testing.T) {
	base, err := os.ReadFile(filepath.Join(corpusDir, "base.jsonl"))
	if err != nil {
		t.Skip("no corpus yet")
	}
	r := runCorpusCase(t, corpusCases[0], base).Report
	if !r.OK() || !r.StartsAtGenesis || r.Entries != 20 || len(r.Anchors) != 3 || len(r.Warnings) == 0 {
		t.Fatalf("base ledger is not what the corpus expects: %+v", r)
	}
	entries, err := os.ReadDir(corpusDir)
	if err != nil {
		t.Fatal(err)
	}
	have := map[string]bool{}
	for _, e := range entries {
		have[e.Name()] = true
	}
	for _, c := range corpusCases {
		if !have[c.name+".json"] {
			t.Errorf("no pinned report for %s", c.name)
		}
	}
}

// writeCorpusBase writes the ledger the cases start from, and its token files.
// It uses fixed timestamps, but a ledger id and key are random, so a rewrite
// changes every hash and every report.
func writeCorpusBase(t *testing.T) {
	t.Helper()
	s := initStoreAt(t, epoch)
	h := func(n int) string { return ledger.FormatTime(epoch.Add(time.Duration(n) * time.Hour)) }
	call := func(n int, tokens int) {
		e := ledger.Entry{"type": ledger.TypeCall, "provider": "openai", "ts": h(n), "tokens_in": tokens}
		if err := s.Append(e); err != nil {
			t.Fatal(err)
		}
	}
	anchor := func(file string, target int64, tsa string, gen time.Time) {
		es, err := s.Entries()
		if err != nil {
			t.Fatal(err)
		}
		tok := "gen=" + gen.Format(time.RFC3339)
		e := ledger.Entry{"type": ledger.TypeAnchor, "ts": h(len(es)), "target_seq": target, "target_hash": es[target].Hash(),
			"token_file": file, "token_sha256": ledger.SHA256Bytes([]byte(tok)), "tsa": tsa}
		if err := s.Append(e); err != nil {
			t.Fatal(err)
		}
		dir := filepath.Join(corpusDir, "anchors")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, file), []byte(tok), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for i := 1; i <= 4; i++ {
		call(i, 100*i)
	}
	call(5, 300)
	if err := s.Append(ledger.Entry{"type": ledger.TypeGap, "ts": h(6), "dropped": 3}); err != nil { // seq 6
		t.Fatal(err)
	}
	call(7, 700)
	call(8, 800)
	if err := s.Append(ledger.Entry{"type": ledger.TypeKeyEvent, "ts": h(9), "event": "rotate"}); err != nil { // seq 9
		t.Fatal(err)
	}
	// seq 10: the TSA time is before entries 4 and 5, so they warn as clock ahead.
	anchor("tok-a.tsr", 5, "tsa-a", epoch.Add(3*time.Hour))
	call(11, 1100)
	call(12, 1200)
	call(10, 1000) // seq 13: timestamp goes backwards
	call(13, 1300) // seq 14
	call(14, 1400) // seq 15
	// seq 16 and 17: two anchors on the same head, the second much later.
	anchor("tok-b.tsr", 14, "tsa-b", epoch.Add(50*time.Hour))
	anchor("tok-c.tsr", 14, "tsa-c", epoch.Add(60*time.Hour))
	call(15, 1500) // seq 18
	call(16, 1600) // seq 19, the unanchored tail
	if err := os.WriteFile(filepath.Join(corpusDir, "base.jsonl"), readBytes(t, s.LedgerPath()), 0o644); err != nil {
		t.Fatal(err)
	}
	fmt.Fprintln(os.Stderr, "corpus base rewritten")
}
