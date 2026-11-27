package ledger_test

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/devrdias/ambermoth/internal/canon"
	"github.com/devrdias/ambermoth/internal/ledger"
)

func TestAppendNothingIsANoOp(t *testing.T) {
	s, before := threeEntryStore(t)
	if err := s.Append(); err != nil {
		t.Fatal(err)
	}
	if err := s.Append([]ledger.Entry{}...); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(readBytes(t, s.LedgerPath()), before) {
		t.Error("ledger changed")
	}
	if _, err := os.Stat(filepath.Join(s.Dir, ledger.LockFile)); err != nil {
		t.Fatalf("lock file from the earlier append: %v", err)
	}
	fresh := initStoreAt(t, epoch)
	if err := fresh.Append(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(fresh.Dir, ledger.LockFile)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("an empty append took the lock: %v", err)
	}
	assertHead(t, fresh, 0)
	assertValid(t, s)
}

func TestAppendAssignsFieldsInPlace(t *testing.T) {
	at := epoch.Add(90 * time.Minute)
	s := initStoreAt(t, at)
	genesis := assertHead(t, s, 0)
	e := ledger.Entry{"type": ledger.TypeCall, "provider": "openai", "tokens_in": 12}

	if err := s.Append(e); err != nil {
		t.Fatal(err)
	}
	if e.Int("v") != ledger.FormatVersion || e.Seq() != 1 || e.Str("prev_hash") != genesis {
		t.Errorf("v/seq/prev_hash not assigned: %v", e)
	}
	if e.Str("ts") != "2026-03-01T01:30:00.000Z" {
		t.Errorf("ts = %q, want the injected clock", e.Str("ts"))
	}
	want, err := ledger.ComputeHash(e)
	if err != nil || e.Hash() == "" || e.Hash() != want {
		t.Errorf("entry_hash = %q, want %q (%v)", e.Hash(), want, err)
	}
	if e.Str("provider") != "openai" || e.Int("tokens_in") != 12 || e.Type() != ledger.TypeCall {
		t.Errorf("caller fields changed: %v", e)
	}
	if got := assertHead(t, s, 1); got != e.Hash() {
		t.Errorf("head hash %s, want the entry hash", got)
	}
	assertValid(t, s)
}

type tsCase struct {
	name   string
	entry  func() ledger.Entry
	wantTS string
}

var tsCases = []tsCase{
	{"missing ts comes from the clock", func() ledger.Entry { return ledger.Entry{"type": ledger.TypeCall} }, "2026-03-01T00:00:00.000Z"},
	{"empty ts comes from the clock", func() ledger.Entry { return ledger.Entry{"type": ledger.TypeCall, "ts": ""} }, "2026-03-01T00:00:00.000Z"},
	{"given ts is kept", func() ledger.Entry { return ledger.Entry{"type": ledger.TypeCall, "ts": "2025-12-31T23:59:59.123Z"} }, "2025-12-31T23:59:59.123Z"},
}

func TestAppendTimestamp(t *testing.T) {
	for _, c := range tsCases {
		t.Run(c.name, func(t *testing.T) {
			s := initStoreAt(t, epoch)
			e := c.entry()
			if err := s.Append(e); err != nil {
				t.Fatal(err)
			}
			if e.Str("ts") != c.wantTS {
				t.Errorf("ts = %q, want %q", e.Str("ts"), c.wantTS)
			}
			es, _ := s.Entries()
			if es[1].Str("ts") != c.wantTS {
				t.Errorf("stored ts = %q, want %q", es[1].Str("ts"), c.wantTS)
			}
		})
	}
}

func TestAppendReplacesCallerAssignedFields(t *testing.T) {
	s := initStoreAt(t, epoch)
	genesis := assertHead(t, s, 0)
	e := ledger.Entry{"type": ledger.TypeCall, "v": 99, "seq": 500, "prev_hash": "sha256:bogus", "entry_hash": "sha256:forged"}
	if err := s.Append(e); err != nil {
		t.Fatal(err)
	}
	if e.Int("v") != 1 || e.Seq() != 1 || e.Str("prev_hash") != genesis || e.Hash() == "sha256:forged" {
		t.Errorf("caller values survived: %v", e)
	}
	es, _ := s.Entries()
	if es[1].Hash() != e.Hash() || es[1].Seq() != 1 {
		t.Errorf("stored entry differs: %v", es[1])
	}
	assertValid(t, s)
}

func TestAppendSeveralEntriesAreContiguous(t *testing.T) {
	s := initStoreAt(t, epoch)
	genesis := assertHead(t, s, 0)
	batch := []ledger.Entry{call(), call("n", "2"), call("n", "3")}
	if err := s.Append(batch...); err != nil {
		t.Fatal(err)
	}
	prev := genesis
	for i, e := range batch {
		if e.Seq() != int64(i+1) || e.Str("prev_hash") != prev {
			t.Errorf("entry %d: seq %d prev %s, want seq %d prev %s", i, e.Seq(), e.Str("prev_hash"), i+1, prev)
		}
		prev = e.Hash()
	}
	if got := assertHead(t, s, 3); got != prev {
		t.Errorf("head %s, want %s", got, prev)
	}
	if err := s.Append(call()); err != nil {
		t.Fatal(err)
	}
	if r := assertValid(t, s); r.Entries != 5 || r.LastSeq != 4 {
		t.Errorf("report %+v", r)
	}
}

func TestAppendWritesTheCanonicalLines(t *testing.T) {
	s := initStoreAt(t, epoch)
	before := readBytes(t, s.LedgerPath())
	batch := []ledger.Entry{call("b", "é<&>"), {"type": ledger.TypeGap, "dropped": 3, "a": []any{"x", nil, true}}}
	if err := s.Append(batch...); err != nil {
		t.Fatal(err)
	}
	var want bytes.Buffer
	for _, e := range batch {
		line, err := canon.Marshal(map[string]any(e))
		if err != nil {
			t.Fatal(err)
		}
		want.Write(line)
		want.WriteByte('\n')
	}
	got := readBytes(t, s.LedgerPath())
	if !bytes.Equal(got, append(before, want.Bytes()...)) {
		t.Errorf("ledger bytes differ.\n got: %q\nwant: %q", got[len(before):], want.Bytes())
	}
	assertValid(t, s)
}

func TestAppendFromTwoStoresInterleaves(t *testing.T) {
	a := initStoreAt(t, epoch)
	b, err := ledger.Open(a.Dir, fixedClock(epoch))
	if err != nil {
		t.Fatal(err)
	}
	order := []*ledger.Store{a, b, b, a, b, a}
	var entries []ledger.Entry
	for _, s := range order {
		e := call()
		if err := s.Append(e); err != nil {
			t.Fatal(err)
		}
		entries = append(entries, e)
	}
	for i, e := range entries {
		if e.Seq() != int64(i+1) {
			t.Errorf("append %d got seq %d, want %d", i, e.Seq(), i+1)
		}
	}
	for _, s := range order[:2] {
		if h := assertHead(t, s, 6); h != entries[5].Hash() {
			t.Errorf("head hash %s, want %s", h, entries[5].Hash())
		}
	}
	if r := assertValid(t, a); r.Entries != 7 {
		t.Errorf("entries = %d, want 7", r.Entries)
	}
}

func TestAppendConcurrentFromTwoStores(t *testing.T) {
	a := initStoreAt(t, epoch)
	b, err := ledger.Open(a.Dir, fixedClock(epoch))
	if err != nil {
		t.Fatal(err)
	}
	const workers, each = 8, 10
	seqs := make(chan int64, workers*each)
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		s := []*ledger.Store{a, b}[w%2]
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < each; i++ {
				e := call()
				if err := s.Append(e); err != nil {
					t.Error(err)
					return
				}
				seqs <- e.Seq()
			}
		}()
	}
	wg.Wait()
	close(seqs)
	seen := map[int64]bool{}
	for q := range seqs {
		if seen[q] {
			t.Errorf("seq %d assigned twice", q)
		}
		seen[q] = true
	}
	if len(seen) != workers*each {
		t.Errorf("%d distinct seqs, want %d", len(seen), workers*each)
	}
	if r := assertValid(t, a); r.LastSeq != workers*each || r.Entries != workers*each+1 {
		t.Errorf("report %+v", r)
	}
}

func TestAppendLockFailureLeavesTheTailAlone(t *testing.T) {
	s, before := threeEntryStore(t)
	lock := filepath.Join(s.Dir, ledger.LockFile)
	if err := os.Remove(lock); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(lock, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := s.Append(call()); err == nil {
		t.Fatal("Append succeeded without a lock file")
	}
	if _, _, err := s.Head(); err == nil {
		t.Error("Head succeeded without a lock file")
	}
	if !bytes.Equal(readBytes(t, s.LedgerPath()), before) {
		t.Error("ledger changed")
	}
	if err := os.Remove(lock); err != nil {
		t.Fatal(err)
	}
	e := call()
	if err := s.Append(e); err != nil {
		t.Fatal(err)
	}
	if e.Seq() != 3 {
		t.Errorf("seq = %d, want 3", e.Seq())
	}
	assertValid(t, s)
}

type badEntryCase struct {
	name    string
	bad     func() ledger.Entry
	wantErr string
}

var badEntryCases = []badEntryCase{
	{"a float", func() ledger.Entry { return ledger.Entry{"type": ledger.TypeCall, "cost": 1.5} }, "entry 4: unsupported type float64"},
	{"a number outside 2^53", func() ledger.Entry { return ledger.Entry{"type": ledger.TypeCall, "n": int64(1) << 60} }, `entry 4: number out of range "1152921504606846976"`},
	{"invalid UTF-8", func() ledger.Entry { return ledger.Entry{"type": ledger.TypeCall, "note": "\xff"} }, "entry 4: invalid UTF-8 in string"},
}

func TestAppendUnhashableEntryWritesNothing(t *testing.T) {
	for _, c := range badEntryCases {
		t.Run(c.name, func(t *testing.T) {
			s, before := threeEntryStore(t)
			head := assertHead(t, s, 2)
			// the good entry comes first in the batch, so the bad one is seq 4
			err := s.Append(call(), c.bad())
			if err == nil || err.Error() != c.wantErr {
				t.Fatalf("err = %v, want %q", err, c.wantErr)
			}
			if !errors.Is(err, ledger.ErrEncode) {
				t.Errorf("err = %v, want it to match ErrEncode", err)
			}
			if !bytes.Equal(readBytes(t, s.LedgerPath()), before) {
				t.Error("part of the batch was written")
			}
			if got := assertHead(t, s, 2); got != head {
				t.Errorf("head moved to %s", got)
			}
			e := call()
			if err := s.Append(e); err != nil {
				t.Fatal(err)
			}
			if e.Seq() != 3 {
				t.Errorf("next seq = %d, want 3 (no seq skipped)", e.Seq())
			}
			assertValid(t, s)
		})
	}
}

func TestAppendLedgerNotWritableByAnotherUser(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores file modes")
	}
	s, before := threeEntryStore(t)
	if err := os.Chmod(s.LedgerPath(), 0o400); err != nil {
		t.Fatal(err)
	}
	if err := s.Append(call()); !errors.Is(err, os.ErrPermission) {
		t.Fatalf("err = %v, want permission denied", err)
	}
	if err := os.Chmod(s.LedgerPath(), 0o600); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(readBytes(t, s.LedgerPath()), before) {
		t.Error("ledger changed")
	}
	e := call()
	if err := s.Append(e); err != nil || e.Seq() != 3 {
		t.Fatalf("append after the failure: seq %d, %v", e.Seq(), err)
	}
	assertValid(t, s)
}
