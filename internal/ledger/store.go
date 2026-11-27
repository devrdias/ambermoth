package ledger

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/devrdias/ambermoth/internal/canon"
)

const (
	LedgerFile = "ledger.jsonl"
	KeyFile    = "secret.key"
	LockFile   = "ledger.lock"
	AnchorDir  = "anchors"
	ConfigFile = "config.json"
)

// Store is an open ledger directory. It is safe for concurrent use within a
// process and coordinates with other processes through an advisory lock.
type Store struct {
	Dir       string
	MasterKey []byte
	LedgerID  string

	clock Clock

	mu       sync.Mutex
	lastSeq  int64
	lastHash string
	size     int64
}

// ErrNilClock is returned by Init and Open when no clock is given.
var ErrNilClock = errors.New("ledger: nil clock")

// Init creates a new ledger directory with a fresh master key and genesis entry.
func Init(dir, generator string, clock Clock) (*Store, error) {
	if clock == nil {
		return nil, ErrNilClock
	}
	if _, err := os.Stat(filepath.Join(dir, LedgerFile)); err == nil {
		return nil, fmt.Errorf("ledger already exists in %s", dir)
	}
	if err := os.MkdirAll(filepath.Join(dir, AnchorDir), 0o700); err != nil {
		return nil, err
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(dir, KeyFile), []byte(hex.EncodeToString(key)+"\n"), 0o600); err != nil {
		return nil, err
	}
	idb := make([]byte, 16)
	rand.Read(idb)
	idb[6] = (idb[6] & 0x0f) | 0x40
	idb[8] = (idb[8] & 0x3f) | 0x80
	id := fmt.Sprintf("%x-%x-%x-%x-%x", idb[0:4], idb[4:6], idb[6:8], idb[8:10], idb[10:])

	g := Entry{
		"v": FormatVersion, "seq": 0, "ts": FormatTime(clock()), "type": TypeGenesis,
		"prev_hash": ZeroHash, "ledger_id": id,
		"hmac_key_commitment": KeyCommitment(key), "generator": generator,
	}
	h, err := ComputeHash(g)
	if err != nil {
		return nil, err
	}
	g["entry_hash"] = h
	line, err := canon.Marshal(map[string]any(g))
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(dir, LedgerFile), append(line, '\n'), 0o600); err != nil {
		return nil, err
	}
	return Open(dir, clock)
}

// Now returns the store's current time in UTC, from its injected clock.
func (s *Store) Now() time.Time { return s.clock().UTC() }

// Open opens an existing ledger directory.
func Open(dir string, clock Clock) (*Store, error) {
	if clock == nil {
		return nil, ErrNilClock
	}
	key, err := readMasterKey(dir)
	if err != nil {
		return nil, err
	}
	s := &Store{Dir: dir, MasterKey: key, lastSeq: -1, clock: clock}
	g, err := readGenesis(s.path())
	if err != nil {
		return nil, err
	}
	s.LedgerID = g.Str("ledger_id")
	if g.Str("hmac_key_commitment") != KeyCommitment(key) {
		return nil, errors.New("secret.key does not match the genesis key commitment")
	}
	if err := s.refreshTail(); err != nil {
		return nil, err
	}
	return s, nil
}

// readMasterKey reads and decodes the hex master key in dir.
func readMasterKey(dir string) ([]byte, error) {
	raw, err := os.ReadFile(filepath.Join(dir, KeyFile))
	if err != nil {
		return nil, fmt.Errorf("reading master key: %w", err)
	}
	key, err := hex.DecodeString(strings.TrimSpace(string(raw)))
	if err != nil || len(key) != 32 {
		return nil, errors.New("malformed secret.key")
	}
	return key, nil
}

// readGenesis parses the first line of the ledger at path.
func readGenesis(path string) (Entry, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	first, err := bufio.NewReader(f).ReadBytes('\n')
	_ = f.Close()
	if err != nil {
		return nil, fmt.Errorf("reading genesis: %w", err)
	}
	gv, err := canon.Parse(bytes.TrimSpace(first))
	if err != nil {
		return nil, err
	}
	return asObject("genesis line", gv)
}

// asObject returns v as an Entry, or an error naming what and the JSON kind
// it was when it is not an object.
func asObject(what string, v any) (Entry, error) {
	m, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%s is not a JSON object (got %s)", what, jsonKind(v))
	}
	return Entry(m), nil
}

func jsonKind(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case []any:
		return "array"
	case string:
		return "string"
	case json.Number:
		return "number"
	case bool:
		return "boolean"
	}
	return "unknown"
}

func (s *Store) path() string { return filepath.Join(s.Dir, LedgerFile) }

// Head returns the last seq and entry hash.
func (s *Store) Head() (int64, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	unlock, err := lockFile(filepath.Join(s.Dir, LockFile))
	if err != nil {
		return 0, "", err
	}
	defer unlock()
	if err := s.refreshTail(); err != nil {
		return 0, "", err
	}
	return s.lastSeq, s.lastHash, nil
}

// refreshTail re-reads the last line if the file changed size.
func (s *Store) refreshTail() error {
	st, err := os.Stat(s.path())
	if err != nil {
		return err
	}
	if st.Size() == s.size && s.lastSeq >= 0 {
		return nil
	}
	last, err := readLastLine(s.path(), st.Size())
	if err != nil {
		return err
	}
	v, err := canon.Parse(last)
	if err != nil {
		return fmt.Errorf("last ledger line is corrupt (truncated write?): %w", err)
	}
	e, err := asObject("last ledger line", v)
	if err != nil {
		return err
	}
	s.lastSeq, s.lastHash, s.size = e.Seq(), e.Hash(), st.Size()
	return nil
}

func readLastLine(path string, size int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	const chunk = 64 * 1024
	end := size
	if end > 0 {
		end-- // skip final newline
	}
	var buf []byte
	for pos := end; pos > 0; {
		n := int64(chunk)
		if pos < n {
			n = pos
		}
		pos -= n
		b := make([]byte, n)
		if _, err := f.ReadAt(b, pos); err != nil && err != io.EOF {
			return nil, err
		}
		buf = append(b, buf...)
		if i := bytes.LastIndexByte(buf, '\n'); i >= 0 && int64(i) < int64(len(buf)) {
			return bytes.TrimSpace(buf[i+1:]), nil
		}
	}
	return bytes.TrimSpace(buf), nil
}

// Append assigns seq/prev_hash/entry_hash to each entry, writes them and fsyncs.
// The passed entries are updated in place.
func (s *Store) Append(entries ...Entry) error {
	if len(entries) == 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	unlock, err := lockFile(filepath.Join(s.Dir, LockFile))
	if err != nil {
		return err
	}
	defer unlock()
	if err := s.refreshTail(); err != nil {
		return err
	}
	buf, seq, prev, err := s.sealEntries(entries)
	if err != nil {
		return err
	}
	if err := s.writeSync(buf); err != nil {
		return err
	}
	s.lastSeq, s.lastHash = seq, prev
	s.size += int64(len(buf))
	return nil
}

// sealEntries assigns seq, prev_hash and entry_hash to entries, continuing
// from the in-memory tail, and returns their canonical lines with the new tail.
func (s *Store) sealEntries(entries []Entry) (lines []byte, seq int64, prev string, err error) {
	var buf bytes.Buffer
	seq, prev = s.lastSeq, s.lastHash
	for _, e := range entries {
		seq++
		e["v"] = FormatVersion
		e["seq"] = seq
		e["prev_hash"] = prev
		if e.Str("ts") == "" {
			e["ts"] = FormatTime(s.Now())
		}
		delete(e, "entry_hash")
		h, err := ComputeHash(e)
		if err != nil {
			return nil, 0, "", fmt.Errorf("entry %d: %w", seq, err)
		}
		e["entry_hash"] = h
		line, err := canon.Marshal(map[string]any(e))
		if err != nil {
			return nil, 0, "", err
		}
		buf.Write(line)
		buf.WriteByte('\n')
		prev = h
	}
	return buf.Bytes(), seq, prev, nil
}

// writeSync appends data to the ledger file and fsyncs it.
func (s *Store) writeSync(data []byte) error {
	f, err := os.OpenFile(s.path(), os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// ReadFile parses every line of a ledger file. It returns raw lines alongside
// parsed entries so the verifier can check canonical form.
func ReadFile(path string) ([]Entry, [][]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = f.Close() }()
	return Read(f)
}

// Read parses ledger lines from r. Parse errors are returned as nil entries
// with the raw line kept, so Verify can report them.
func Read(r io.Reader) ([]Entry, [][]byte, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1024*1024), 16*1024*1024)
	var entries []Entry
	var lines [][]byte
	for sc.Scan() {
		raw := append([]byte(nil), sc.Bytes()...)
		if len(bytes.TrimSpace(raw)) == 0 {
			continue
		}
		lines = append(lines, raw)
		v, err := canon.Parse(raw)
		m, ok := v.(map[string]any)
		if err != nil || !ok {
			entries = append(entries, nil)
			continue
		}
		entries = append(entries, Entry(m))
	}
	return entries, lines, sc.Err()
}

// Entries reads all entries of the store.
func (s *Store) Entries() ([]Entry, error) {
	es, _, err := ReadFile(s.path())
	return es, err
}

// LedgerPath returns the ledger file path.
func (s *Store) LedgerPath() string { return s.path() }

// SHA256Bytes hashes bytes.
func SHA256Bytes(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}
