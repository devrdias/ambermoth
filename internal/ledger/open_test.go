package ledger_test

import (
	"bytes"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/devrdias/ambermoth/internal/ledger"
)

// threeEntryStore returns a store holding the genesis and two calls, and the
// raw ledger bytes.
func threeEntryStore(t *testing.T) (*ledger.Store, []byte) {
	t.Helper()
	s := initStoreAt(t, epoch)
	if err := s.Append(call(), call()); err != nil {
		t.Fatal(err)
	}
	return s, readBytes(t, s.LedgerPath())
}

func TestOpenGoodLedger(t *testing.T) {
	s, _ := threeEntryStore(t)
	wantHash := assertHead(t, s, 2)

	o, err := ledger.Open(s.Dir, fixedClock(epoch))
	if err != nil {
		t.Fatal(err)
	}
	if o.Dir != s.Dir || o.LedgerID != s.LedgerID || !bytes.Equal(o.MasterKey, s.MasterKey) {
		t.Errorf("opened store differs: %+v vs %+v", o, s)
	}
	if got := assertHead(t, o, 2); got != wantHash {
		t.Errorf("head hash = %s, want %s", got, wantHash)
	}
	if !o.Now().Equal(epoch) {
		t.Errorf("Now = %v, want the injected clock %v", o.Now(), epoch)
	}
}

func TestOpenGenesisOnlyTail(t *testing.T) {
	s := initStoreAt(t, epoch)
	o, err := ledger.Open(s.Dir, fixedClock(epoch))
	if err != nil {
		t.Fatal(err)
	}
	es, _ := o.Entries()
	if h := assertHead(t, o, 0); h != es[0].Hash() {
		t.Errorf("head hash %s, want the genesis hash", h)
	}
}

func TestOpenThenAppendContinuesTheChain(t *testing.T) {
	s, _ := threeEntryStore(t)
	head := assertHead(t, s, 2)
	o, err := ledger.Open(s.Dir, fixedClock(epoch))
	if err != nil {
		t.Fatal(err)
	}
	e := call()
	if err := o.Append(e); err != nil {
		t.Fatal(err)
	}
	if e.Seq() != 3 || e.Str("prev_hash") != head {
		t.Errorf("appended seq %d prev %s, want 3 after %s", e.Seq(), e.Str("prev_hash"), head)
	}
	assertValid(t, o)
}

func TestOpenNilClock(t *testing.T) {
	s := initStoreAt(t, epoch)
	if _, err := ledger.Open(s.Dir, nil); !errors.Is(err, ledger.ErrNilClock) {
		t.Fatalf("err = %v, want ErrNilClock", err)
	}
}

func TestOpenMissingFiles(t *testing.T) {
	t.Run("missing directory", func(t *testing.T) {
		_, err := ledger.Open(filepath.Join(t.TempDir(), "nope"), fixedClock(epoch))
		if !errors.Is(err, fs.ErrNotExist) || !strings.HasPrefix(err.Error(), "reading master key: ") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("missing key", func(t *testing.T) {
		s := initStoreAt(t, epoch)
		if err := os.Remove(keyPath(s.Dir)); err != nil {
			t.Fatal(err)
		}
		_, err := ledger.Open(s.Dir, fixedClock(epoch))
		if !errors.Is(err, fs.ErrNotExist) || !strings.HasPrefix(err.Error(), "reading master key: ") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("missing ledger", func(t *testing.T) {
		s := initStoreAt(t, epoch)
		if err := os.Remove(s.LedgerPath()); err != nil {
			t.Fatal(err)
		}
		if _, err := ledger.Open(s.Dir, fixedClock(epoch)); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("err = %v, want not exist", err)
		}
	})
}

type keyCase struct {
	name    string
	content func(good string) string
	wantErr string
}

var keyCases = []keyCase{
	{"empty key file", func(string) string { return "" }, "malformed secret.key"},
	{"not hex", func(string) string { return strings.Repeat("zz", 32) }, "malformed secret.key"},
	{"31 bytes", func(g string) string { return g[:62] }, "malformed secret.key"},
	{"33 bytes", func(g string) string { return g + "ab" }, "malformed secret.key"},
	{"odd number of digits", func(g string) string { return g[:63] }, "malformed secret.key"},
	{"another valid key", func(string) string { return strings.Repeat("ab", 32) },
		"secret.key does not match the genesis key commitment"},
	{"surrounding whitespace is trimmed", func(g string) string { return "  " + g + " \r\n\n" }, ""},
	{"no trailing newline", func(g string) string { return g }, ""},
}

func TestOpenKeyValidation(t *testing.T) {
	for _, c := range keyCases {
		t.Run(c.name, func(t *testing.T) {
			s := initStoreAt(t, epoch)
			good := hex.EncodeToString(s.MasterKey)
			if err := os.WriteFile(keyPath(s.Dir), []byte(c.content(good)), 0o600); err != nil {
				t.Fatal(err)
			}
			o, err := ledger.Open(s.Dir, fixedClock(epoch))
			if c.wantErr == "" {
				if err != nil || !bytes.Equal(o.MasterKey, s.MasterKey) {
					t.Fatalf("Open = %v, %v; want the same key", o, err)
				}
				return
			}
			if err == nil || err.Error() != c.wantErr {
				t.Fatalf("err = %v, want %q", err, c.wantErr)
			}
		})
	}
}

func TestOpenGenesisFromAnotherLedger(t *testing.T) {
	a, b := initStoreAt(t, epoch), initStoreAt(t, epoch)
	if err := os.WriteFile(a.LedgerPath(), readBytes(t, b.LedgerPath()), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := ledger.Open(a.Dir, fixedClock(epoch))
	if err == nil || err.Error() != "secret.key does not match the genesis key commitment" {
		t.Fatalf("err = %v", err)
	}
}

// What Open does with a ledger that a crash or a copy cut short. The repair
// command will have to start from these results.
type ledgerShapeCase struct {
	name    string
	mutate  func(b []byte) []byte
	wantErr string // prefix; empty means Open succeeds
	wantSeq int64  // head seq when Open succeeds
}

func dropLastLine(b []byte) []byte {
	i := bytes.LastIndexByte(b[:len(b)-1], '\n')
	return b[:i+1]
}

var ledgerShapeCases = []ledgerShapeCase{
	{"empty file", func([]byte) []byte { return nil }, "reading genesis: EOF", 0},
	{"only a newline", func([]byte) []byte { return []byte("\n") }, "EOF", 0},
	{"genesis cut before its newline", func(b []byte) []byte { return b[:30] }, "reading genesis: EOF", 0},
	{"genesis is not JSON", func([]byte) []byte { return []byte("nope\n") }, "invalid character", 0},
	{"last line cut in the middle", func(b []byte) []byte { return b[:len(b)-20] },
		"last ledger line is corrupt (truncated write?): EOF", 0},
	{"last line cut to one character", func(b []byte) []byte { return append(dropLastLine(b), '{') },
		"last ledger line is corrupt (truncated write?): ", 0},
	{"last line complete but its newline missing", func(b []byte) []byte { return b[:len(b)-1] },
		"last ledger line is corrupt (truncated write?): EOF", 0},
	{"blank line after the last entry", func(b []byte) []byte { return append(b, '\n') },
		"last ledger line is corrupt (truncated write?): EOF", 0},
	{"cut exactly at a line boundary", dropLastLine, "", 1},
}

func TestOpenDamagedLedger(t *testing.T) {
	for _, c := range ledgerShapeCases {
		t.Run(c.name, func(t *testing.T) {
			s, orig := threeEntryStore(t)
			damaged := c.mutate(orig)
			if err := os.WriteFile(s.LedgerPath(), damaged, 0o600); err != nil {
				t.Fatal(err)
			}
			o, err := ledger.Open(s.Dir, fixedClock(epoch))
			if !bytes.Equal(readBytes(t, s.LedgerPath()), damaged) {
				t.Error("Open changed the ledger file")
			}
			if c.wantErr != "" {
				if err == nil || !strings.HasPrefix(err.Error(), c.wantErr) {
					t.Fatalf("err = %v, want prefix %q", err, c.wantErr)
				}
				if o != nil {
					t.Error("a store was returned with the error")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			assertHead(t, o, c.wantSeq)
		})
	}
}

func TestOpenCutLastLineWrapsParseError(t *testing.T) {
	s, orig := threeEntryStore(t)
	if err := os.WriteFile(s.LedgerPath(), orig[:len(orig)-20], 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ledger.Open(s.Dir, fixedClock(epoch)); !errors.Is(err, io.EOF) {
		t.Fatalf("err = %v, want it to wrap io.EOF", err)
	}
}

// A store that is already open sees a half-written line from another writer
// as an error, and does not write after it.
func TestAppendRefusesToWriteAfterACutLine(t *testing.T) {
	s, orig := threeEntryStore(t)
	cut := orig[:len(orig)-20]
	if err := os.WriteFile(s.LedgerPath(), cut, 0o600); err != nil {
		t.Fatal(err)
	}
	err := s.Append(call())
	if err == nil || !strings.HasPrefix(err.Error(), "last ledger line is corrupt (truncated write?)") {
		t.Fatalf("err = %v", err)
	}
	if !bytes.Equal(readBytes(t, s.LedgerPath()), cut) {
		t.Error("the ledger was written to")
	}
	if _, _, err := s.Head(); err == nil {
		t.Error("Head succeeded on a cut ledger")
	}
}

func TestOpenReadsLongLastLine(t *testing.T) {
	s := initStoreAt(t, epoch)
	big := strings.Repeat("x", 200*1024)
	if err := s.Append(call(), call("note", big)); err != nil {
		t.Fatal(err)
	}
	o, err := ledger.Open(s.Dir, fixedClock(epoch))
	if err != nil {
		t.Fatal(err)
	}
	assertHead(t, o, 2)
	assertValid(t, o)
}

// Valid JSON that is not an object, and what the error calls it.
type notObjectCase struct {
	line string
	kind string
}

var notObjectCases = []notObjectCase{
	{"[1]", "array"},
	{`"x"`, "string"},
	{"3", "number"},
	{"null", "null"},
	{"true", "boolean"},
}

func replaceLine(b []byte, i int, line string) []byte {
	lines := bytes.SplitAfter(b, []byte("\n"))
	lines[i] = []byte(line + "\n")
	return bytes.Join(lines, nil)
}

func TestOpenGenesisNotAnObject(t *testing.T) {
	for _, c := range notObjectCases {
		t.Run(c.line, func(t *testing.T) {
			s, orig := threeEntryStore(t)
			bad := replaceLine(orig, 0, c.line)
			if err := os.WriteFile(s.LedgerPath(), bad, 0o600); err != nil {
				t.Fatal(err)
			}
			o, err := ledger.Open(s.Dir, fixedClock(epoch))
			want := "genesis line is not a JSON object (got " + c.kind + ")"
			if err == nil || err.Error() != want {
				t.Fatalf("err = %v, want %q", err, want)
			}
			if o != nil {
				t.Error("a store was returned with the error")
			}
		})
	}
}

func TestOpenLastLineNotAnObject(t *testing.T) {
	for _, c := range notObjectCases {
		t.Run(c.line, func(t *testing.T) {
			s, orig := threeEntryStore(t)
			bad := replaceLine(orig, 2, c.line)
			if err := os.WriteFile(s.LedgerPath(), bad, 0o600); err != nil {
				t.Fatal(err)
			}
			o, err := ledger.Open(s.Dir, fixedClock(epoch))
			want := "last ledger line is not a JSON object (got " + c.kind + ")"
			if err == nil || err.Error() != want {
				t.Fatalf("err = %v, want %q", err, want)
			}
			if o != nil {
				t.Error("a store was returned with the error")
			}
			if !bytes.Equal(readBytes(t, s.LedgerPath()), bad) {
				t.Error("Open changed the ledger file")
			}
		})
	}
}

// Another writer leaves a complete line of the wrong shape after this store
// was opened: Append and Head return the error and Append writes nothing.
func TestOpenStoreSeesLastLineNotAnObject(t *testing.T) {
	for _, c := range notObjectCases {
		t.Run(c.line, func(t *testing.T) {
			s, orig := threeEntryStore(t)
			bad := replaceLine(orig, 2, c.line)
			if err := os.WriteFile(s.LedgerPath(), bad, 0o600); err != nil {
				t.Fatal(err)
			}
			want := "last ledger line is not a JSON object (got " + c.kind + ")"
			if err := s.Append(call()); err == nil || err.Error() != want {
				t.Fatalf("Append err = %v, want %q", err, want)
			}
			if _, _, err := s.Head(); err == nil || err.Error() != want {
				t.Fatalf("Head err = %v, want %q", err, want)
			}
			if !bytes.Equal(readBytes(t, s.LedgerPath()), bad) {
				t.Error("the ledger was written to")
			}
		})
	}
}
