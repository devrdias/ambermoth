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

// Init only looks for the ledger file. A key left in a directory without a
// ledger is replaced; this pins the current behaviour.
func TestInitReplacesKeyWhenNoLedgerExists(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(keyPath(dir), []byte("left over\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := ledger.Init(dir, "x", fixedClock(epoch))
	if err != nil {
		t.Fatal(err)
	}
	if got := readBytes(t, keyPath(dir)); !bytes.Equal(got, []byte(hex.EncodeToString(s.MasterKey)+"\n")) {
		t.Errorf("key file not replaced: %q", got)
	}
	assertValid(t, s)
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
