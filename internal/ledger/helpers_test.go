package ledger_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/devrdias/ambermoth/internal/ledger"
)

// epoch is the fixed start time used by tests that need a clock.
var epoch = time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)

func fixedClock(at time.Time) ledger.Clock { return func() time.Time { return at } }

func initStoreAt(t *testing.T, at time.Time) *ledger.Store {
	t.Helper()
	s, err := ledger.Init(t.TempDir(), "ambermoth/test", fixedClock(at))
	if err != nil {
		t.Fatalf("init: %v", err)
	}
	return s
}

func readBytes(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// assertValid runs the verifier over the store's ledger and fails on any error.
func assertValid(t *testing.T, s *ledger.Store) *ledger.Report {
	t.Helper()
	r := verifyFile(t, s)
	if !r.OK() {
		t.Fatalf("ledger does not verify: %v", r.Errors)
	}
	return r
}

func assertHead(t *testing.T, s *ledger.Store, wantSeq int64) string {
	t.Helper()
	seq, hash, err := s.Head()
	if err != nil {
		t.Fatalf("head: %v", err)
	}
	if seq != wantSeq {
		t.Fatalf("head seq = %d, want %d", seq, wantSeq)
	}
	return hash
}

func ledgerPath(dir string) string { return filepath.Join(dir, ledger.LedgerFile) }

func keyPath(dir string) string { return filepath.Join(dir, ledger.KeyFile) }

func call(extra ...string) ledger.Entry {
	e := ledger.Entry{"type": ledger.TypeCall, "provider": "openai"}
	for i := 0; i+1 < len(extra); i += 2 {
		e[extra[i]] = extra[i+1]
	}
	return e
}
