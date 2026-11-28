package ledger_test

import (
	"bytes"
	"os"
	"testing"

	"github.com/devrdias/ambermoth/internal/canon"
	"github.com/devrdias/ambermoth/internal/ledger"
)

// fuzzLedgerSeed writes a small clean ledger through the Store and returns its bytes.
func fuzzLedgerSeed(f *testing.F, calls int) []byte {
	f.Helper()
	s, err := ledger.Init(f.TempDir(), "ambermoth/fuzz", fixedClock(epoch))
	if err != nil {
		f.Fatal(err)
	}
	for i := 1; i <= calls; i++ {
		if err := s.Append(callEntry(i)); err != nil {
			f.Fatal(err)
		}
	}
	b, err := os.ReadFile(s.LedgerPath())
	if err != nil {
		f.Fatal(err)
	}
	return b
}

func FuzzLedgerLines(f *testing.F) {
	clean := fuzzLedgerSeed(f, 3)
	lines := splitLines(clean)
	f.Add(clean)
	f.Add(fuzzLedgerSeed(f, 0))
	f.Add(clean[:len(clean)-9])
	f.Add(bytes.ReplaceAll(clean, []byte("\n"), []byte("\r\n")))
	f.Add(append(append([]byte("\n  \n"), clean...), "\n\n"...))
	f.Add(joinLines(lines[1:]))
	f.Add(joinLines([][]byte{lines[0], lines[2], lines[1]}))
	f.Add(bytes.Replace(clean, []byte(`"openai"`), []byte(`"openaj"`), 1))
	odd := []string{"", "\n", "{}", "[]", "null", `{"seq":1e999}`, `{"a":1,"a":2}`, "\xff\xfe{", `{"seq":9007199254740993}`}
	for _, s := range odd {
		f.Add([]byte(s))
	}
	f.Fuzz(checkLedgerBytes)
}

// checkLedgerBytes holds the properties of reading and verifying any bytes: no
// panic, the streaming and the in-memory verifier agree, and a ledger that
// verifies really is canonical, hashed and chained.
func checkLedgerBytes(t *testing.T, data []byte) {
	t.Helper()
	opt := ledger.VerifyOptions{}
	es, lines, readErr := ledger.Read(bytes.NewReader(data))
	streamed, streamErr := ledger.VerifyReader(bytes.NewReader(data), opt)
	if (readErr == nil) != (streamErr == nil) {
		t.Fatalf("Read error %v, VerifyReader error %v", readErr, streamErr)
	}
	if readErr != nil {
		return
	}
	memory := ledger.Verify(es, lines, opt)
	if got, want := projectReport(t, streamed), projectReport(t, memory); !bytes.Equal(got, want) {
		t.Fatalf("VerifyReader and Verify differ\n%s", firstDifference(got, want))
	}
	if streamed.Entries != len(lines) {
		t.Fatalf("report counts %d entries for %d lines", streamed.Entries, len(lines))
	}
	if streamed.OK() {
		assertVerifiedLines(t, lines)
	}
}

// required are the fields every entry has (SPEC §3.1).
//
//rules:readonly read-only table
var required = []string{"v", "seq", "ts", "type", "prev_hash", "entry_hash"}

// assertVerifiedLines restates what an OK report promises, without the verifier's code.
func assertVerifiedLines(t *testing.T, lines [][]byte) {
	t.Helper()
	if len(lines) == 0 {
		t.Fatal("an empty ledger verified")
	}
	var prevHash string
	var prevSeq int64
	for i, line := range lines {
		e := ledger.ParseEntry(line)
		if e == nil {
			t.Fatalf("line %d is not an object but the ledger verified", i+1)
		}
		c, err := canon.Marshal(map[string]any(e))
		if err != nil || !bytes.Equal(c, bytes.TrimSpace(line)) {
			t.Fatalf("line %d is not canonical but the ledger verified", i+1)
		}
		for _, k := range required {
			if _, ok := e[k]; !ok {
				t.Fatalf("line %d has no %q but the ledger verified", i+1, k)
			}
		}
		if h, err := ledger.ComputeHash(e); err != nil || h != e.Hash() {
			t.Fatalf("line %d has a wrong entry_hash but the ledger verified", i+1)
		}
		if i > 0 && (e.Str("prev_hash") != prevHash || e.Seq() != prevSeq+1) {
			t.Fatalf("line %d is not linked to line %d but the ledger verified", i+1, i)
		}
		prevHash, prevSeq = e.Hash(), e.Seq()
	}
}

func FuzzEachLine(f *testing.F) {
	shapes := []string{"", "a\n", "a\r\nb", "\n\n  \n{\"a\":1}\n", "x\n\r\ny", "\r", " \t\n"}
	for _, s := range shapes {
		f.Add([]byte(s))
	}
	f.Add(fuzzLedgerSeed(f, 2))
	f.Fuzz(func(t *testing.T, data []byte) {
		var n int
		err := ledger.EachLine(bytes.NewReader(data), func(line []byte, off int64) {
			n++
			if len(bytes.TrimSpace(line)) == 0 {
				t.Fatalf("blank line %q handed to the callback", line)
			}
			if off < 0 || off+int64(len(line)) > int64(len(data)) || !bytes.Equal(data[off:off+int64(len(line))], line) {
				t.Fatalf("offset %d does not point at line %q", off, line)
			}
		})
		if err != nil {
			t.Fatalf("a short input gave an error: %v", err)
		}
		_, lines, err := ledger.Read(bytes.NewReader(data))
		if err != nil || len(lines) != n {
			t.Fatalf("Read gave %d lines and %v, EachLine %d", len(lines), err, n)
		}
	})
}

func FuzzParseEntry(f *testing.F) {
	seedLines := splitLines(fuzzLedgerSeed(f, 2))
	for _, l := range seedLines {
		f.Add(l)
	}
	odd := []string{"{}", `{"a":{"b":[1,2,{"c":null}]}}`, "[", `{"a":1}{"b":2}`, `{"a":1} x`, `{"\ud800":1}`, `{"n":1e400}`}
	for _, s := range odd {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, line []byte) {
		e := ledger.ParseEntry(line)
		if e == nil {
			return
		}
		// Whatever was accepted must be writable, and a second parse must give the same entry.
		out, err := canon.Marshal(map[string]any(e))
		if err != nil {
			return
		}
		again := ledger.ParseEntry(out)
		back, err := canon.Marshal(map[string]any(again))
		if err != nil || !bytes.Equal(out, back) {
			t.Fatalf("canonical form is not stable: %q then %q (%v)", out, back, err)
		}
		_, _, _, _ = e.Seq(), e.Time(), e.Day(), e.Int("x")
		_, _ = ledger.ComputeHash(e)
	})
}
