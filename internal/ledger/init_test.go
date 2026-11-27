package ledger_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/devrdias/ambermoth/internal/ledger"
)

var uuidV4 = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

type initDirCase struct {
	name    string
	prepare func(t *testing.T) string
}

var initDirCases = []initDirCase{
	{"creates a directory that does not exist", func(t *testing.T) string {
		return filepath.Join(t.TempDir(), "a", "b", ".ambermoth")
	}},
	{"uses an existing empty directory", func(t *testing.T) string { return t.TempDir() }},
	{"keeps unrelated files in the directory", func(t *testing.T) string {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, ledger.ConfigFile), []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
		return dir
	}},
}

func TestInitCreatesLedger(t *testing.T) {
	for _, c := range initDirCases {
		t.Run(c.name, func(t *testing.T) {
			dir := c.prepare(t)
			s, err := ledger.Init(dir, "ambermoth/9.9.9", fixedClock(epoch))
			if err != nil {
				t.Fatal(err)
			}
			if s.Dir != dir {
				t.Errorf("Dir = %q, want %q", s.Dir, dir)
			}
			st, err := os.Stat(filepath.Join(dir, ledger.AnchorDir))
			if err != nil || !st.IsDir() {
				t.Errorf("anchors directory missing: %v", err)
			}
			if r := assertValid(t, s); r.Entries != 1 || !r.StartsAtGenesis || r.LastSeq != 0 {
				t.Errorf("unexpected report %+v", r)
			}
		})
	}
}

func TestInitFileModes(t *testing.T) {
	s := initStoreAt(t, epoch)
	files := []string{keyPath(s.Dir), ledgerPath(s.Dir)}
	for _, p := range files {
		st, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if st.Mode().Perm() != 0o600 {
			t.Errorf("%s mode = %v, want 0600", filepath.Base(p), st.Mode().Perm())
		}
	}
}

func TestInitKeyFile(t *testing.T) {
	s := initStoreAt(t, epoch)
	raw := readBytes(t, keyPath(s.Dir))
	if len(raw) != 65 || raw[64] != '\n' {
		t.Fatalf("key file is %d bytes, want 64 hex characters and a newline", len(raw))
	}
	key, err := hex.DecodeString(string(raw[:64]))
	if err != nil {
		t.Fatalf("key file is not hex: %v", err)
	}
	if !bytes.Equal(key, s.MasterKey) || len(key) != 32 {
		t.Errorf("store key does not match the key file")
	}
	other := initStoreAt(t, epoch)
	if bytes.Equal(other.MasterKey, s.MasterKey) {
		t.Error("two ledgers got the same master key")
	}
}

func TestInitGenesisEntry(t *testing.T) {
	s := initStoreAt(t, epoch.Add(1500*time.Millisecond))
	es, lines, err := ledger.ReadFile(s.LedgerPath())
	if err != nil {
		t.Fatal(err)
	}
	if len(es) != 1 || len(lines) != 1 {
		t.Fatalf("ledger has %d entries, want only the genesis", len(es))
	}
	g := es[0]
	sum := sha256.Sum256(append([]byte("ambermoth/key-commit/v1:"), s.MasterKey...))
	wantHash, err := ledger.ComputeHash(g)
	if err != nil {
		t.Fatal(err)
	}
	checks := []struct {
		field     string
		got, want any
	}{
		{"v", g.Int("v"), int64(ledger.FormatVersion)},
		{"seq", g.Seq(), int64(0)},
		{"ts", g.Str("ts"), "2026-03-01T00:00:01.500Z"},
		{"type", g.Type(), ledger.TypeGenesis},
		{"prev_hash", g.Str("prev_hash"), ledger.ZeroHash},
		{"generator", g.Str("generator"), "ambermoth/test"},
		{"hmac_key_commitment", g.Str("hmac_key_commitment"), "sha256:" + hex.EncodeToString(sum[:])},
		{"entry_hash", g.Hash(), wantHash},
		{"ledger_id", g.Str("ledger_id"), s.LedgerID},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %v, want %v", c.field, c.got, c.want)
		}
	}
	if !uuidV4.MatchString(s.LedgerID) {
		t.Errorf("ledger_id %q is not a UUIDv4", s.LedgerID)
	}
	if len(g) != 9 {
		t.Errorf("genesis has %d fields, want 9: %v", len(g), g)
	}
	if !bytes.HasSuffix(readBytes(t, s.LedgerPath()), []byte("}\n")) {
		t.Error("genesis line does not end with a newline")
	}
	if h := assertHead(t, s, 0); h != g.Hash() {
		t.Errorf("head hash %s, want genesis hash", h)
	}
}

func TestInitLedgerIDsDiffer(t *testing.T) {
	a, b := initStoreAt(t, epoch), initStoreAt(t, epoch)
	if a.LedgerID == b.LedgerID {
		t.Fatalf("two ledgers share the id %s", a.LedgerID)
	}
}

func TestInitNilClock(t *testing.T) {
	dir := t.TempDir()
	if _, err := ledger.Init(dir, "x", nil); !errors.Is(err, ledger.ErrNilClock) {
		t.Fatalf("err = %v, want ErrNilClock", err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("a nil clock left %d files behind", len(entries))
	}
}

func TestInitRefusesExistingLedger(t *testing.T) {
	s := initStoreAt(t, epoch)
	if err := s.Append(call()); err != nil {
		t.Fatal(err)
	}
	ledgerBefore, keyBefore := readBytes(t, s.LedgerPath()), readBytes(t, keyPath(s.Dir))

	_, err := ledger.Init(s.Dir, "other", fixedClock(epoch.Add(24*time.Hour)))
	if err == nil || !strings.Contains(err.Error(), "ledger already exists in "+s.Dir) {
		t.Fatalf("err = %v, want 'ledger already exists'", err)
	}
	if !bytes.Equal(readBytes(t, s.LedgerPath()), ledgerBefore) {
		t.Error("ledger changed")
	}
	if !bytes.Equal(readBytes(t, keyPath(s.Dir)), keyBefore) {
		t.Error("key changed")
	}
	assertValid(t, s)
}

type keepKeyCase struct {
	name string
	key  string
}

// Whatever is in secret.key, a ledger that belongs to it may be somewhere else.
var keepKeyCases = []keepKeyCase{
	{"a key left without its ledger", "left over\n"},
	{"an empty key file", ""},
	{"a valid key", strings.Repeat("ab", 32) + "\n"},
	{"a file much larger than a key", strings.Repeat("x", 1<<20)},
}

func TestInitRefusesExistingKeyWithoutLedger(t *testing.T) {
	for _, c := range keepKeyCases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(keyPath(dir), []byte(c.key), 0o600); err != nil {
				t.Fatal(err)
			}
			s, err := ledger.Init(dir, "x", fixedClock(epoch))
			want := "secret.key already exists in " + dir + ": not overwriting it"
			if err == nil || !strings.Contains(err.Error(), want) || s != nil {
				t.Fatalf("Init = %v, %v; want an error containing %q", s, err, want)
			}
			if !strings.Contains(err.Error(), "remove secret.key yourself") || !strings.Contains(err.Error(), "Restore the ledger.jsonl") {
				t.Errorf("error does not say what to do: %v", err)
			}
			if got := readBytes(t, keyPath(dir)); string(got) != c.key {
				t.Errorf("key file changed: %d bytes, want %d", len(got), len(c.key))
			}
			entries, _ := os.ReadDir(dir)
			if len(entries) != 1 {
				t.Errorf("Init left %d files in the directory, want only the key", len(entries))
			}
		})
	}
}

// After the user removes the key on purpose, init works again.
func TestInitSucceedsAfterTheKeyIsRemoved(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(keyPath(dir), []byte("left over\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(keyPath(dir)); err != nil {
		t.Fatal(err)
	}
	s, err := ledger.Init(dir, "x", fixedClock(epoch))
	if err != nil {
		t.Fatal(err)
	}
	assertValid(t, s)
}

// A ledger without its key is refused too, and no key is made for it.
func TestInitRefusesExistingLedgerWithoutKey(t *testing.T) {
	s := initStoreAt(t, epoch)
	before := readBytes(t, s.LedgerPath())
	if err := os.Remove(keyPath(s.Dir)); err != nil {
		t.Fatal(err)
	}
	_, err := ledger.Init(s.Dir, "x", fixedClock(epoch))
	if err == nil || !strings.Contains(err.Error(), "ledger already exists in "+s.Dir) {
		t.Fatalf("err = %v", err)
	}
	if _, statErr := os.Lstat(keyPath(s.Dir)); statErr == nil {
		t.Error("a key was created next to an existing ledger")
	}
	if !bytes.Equal(readBytes(t, s.LedgerPath()), before) {
		t.Error("ledger changed")
	}
}

// Two init runs on one empty directory: one creates the ledger, the others
// are refused, and the key that was written belongs to the ledger.
func TestInitConcurrentCalls(t *testing.T) {
	const runs = 8
	dir := t.TempDir()
	errs := make([]error, runs)
	stores := make([]*ledger.Store, runs)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < runs; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			stores[i], errs[i] = ledger.Init(dir, "x", fixedClock(epoch))
		}()
	}
	close(start)
	wg.Wait()

	var winner *ledger.Store
	for i := range errs {
		if errs[i] == nil {
			if winner != nil {
				t.Fatal("more than one Init succeeded")
			}
			winner = stores[i]
			continue
		}
		if stores[i] != nil {
			t.Errorf("run %d returned a store with an error", i)
		}
	}
	if winner == nil {
		t.Fatalf("no Init succeeded: %v", errs)
	}
	assertValid(t, winner)
	reopened, err := ledger.Open(dir, fixedClock(epoch))
	if err != nil {
		t.Fatalf("the directory does not open after the race: %v", err)
	}
	if !bytes.Equal(reopened.MasterKey, winner.MasterKey) || reopened.LedgerID != winner.LedgerID {
		t.Error("the key on disk is not the winner's")
	}
}

type initFailCase struct {
	name         string
	prepare      func(t *testing.T) string
	wantNoLedger bool
}

var initFailCases = []initFailCase{
	{"parent is a file", func(t *testing.T) string {
		parent := filepath.Join(t.TempDir(), "file")
		if err := os.WriteFile(parent, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		return filepath.Join(parent, ".ambermoth")
	}, true},
	{"key path is a directory", func(t *testing.T) string {
		dir := t.TempDir()
		if err := os.Mkdir(keyPath(dir), 0o700); err != nil {
			t.Fatal(err)
		}
		return dir
	}, true},
	{"key path is a dangling link", func(t *testing.T) string {
		dir := t.TempDir()
		if err := os.Symlink(filepath.Join(dir, "missing", "key"), keyPath(dir)); err != nil {
			t.Skip("symlinks not available")
		}
		return dir
	}, true},
	{"ledger path is a dangling link", func(t *testing.T) string {
		dir := t.TempDir()
		if err := os.Symlink(filepath.Join(dir, "missing", "ledger"), ledgerPath(dir)); err != nil {
			t.Skip("symlinks not available")
		}
		return dir
	}, false},
}

func TestInitReturnsWriteErrors(t *testing.T) {
	for _, c := range initFailCases {
		t.Run(c.name, func(t *testing.T) {
			dir := c.prepare(t)
			s, err := ledger.Init(dir, "x", fixedClock(epoch))
			if err == nil || s != nil {
				t.Fatalf("Init = %v, %v; want an error and no store", s, err)
			}
			if _, statErr := os.Lstat(ledgerPath(dir)); c.wantNoLedger && statErr == nil {
				t.Error("a ledger file was created")
			}
		})
	}
}

// A key reached through a link is not written through it.
func TestInitDoesNotWriteThroughAKeyLink(t *testing.T) {
	dir, elsewhere := t.TempDir(), filepath.Join(t.TempDir(), "old.key")
	if err := os.WriteFile(elsewhere, []byte("the old key\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(elsewhere, keyPath(dir)); err != nil {
		t.Skip("symlinks not available")
	}
	if _, err := ledger.Init(dir, "x", fixedClock(epoch)); err == nil || !strings.Contains(err.Error(), "secret.key already exists") {
		t.Fatalf("err = %v", err)
	}
	if got := readBytes(t, elsewhere); string(got) != "the old key\n" {
		t.Errorf("the linked key was changed: %q", got)
	}
}

// When the ledger cannot be created, the key made for it is removed again, so
// a retry is not refused because of a key nothing uses.
func TestInitRemovesItsKeyWhenTheLedgerCannotBeCreated(t *testing.T) {
	dir := t.TempDir()
	link := ledgerPath(dir)
	if err := os.Symlink(filepath.Join(dir, "missing", "ledger"), link); err != nil {
		t.Skip("symlinks not available")
	}
	if _, err := ledger.Init(dir, "x", fixedClock(epoch)); err == nil || !strings.Contains(err.Error(), "ledger already exists in "+dir) {
		t.Fatalf("err = %v", err)
	}
	if _, err := os.Lstat(keyPath(dir)); err == nil {
		t.Error("the key was left behind")
	}
	if _, err := os.Readlink(link); err != nil {
		t.Errorf("the link at the ledger path was touched: %v", err)
	}
}
