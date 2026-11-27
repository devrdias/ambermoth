package ops_test

import (
	"bytes"
	"os"
	"testing"
	"time"

	"github.com/devrdias/ambermoth/internal/ledger"
)

// clockAt is the fixed time every entry written by the code under test gets.
var clockAt = time.Date(2026, 3, 10, 8, 30, 0, 0, time.UTC)

func fixedClock(at time.Time) ledger.Clock { return func() time.Time { return at } }

func newFixedStore(t *testing.T) *ledger.Store {
	t.Helper()
	s, err := ledger.Init(t.TempDir(), "test", fixedClock(clockAt))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func ledgerBytes(t *testing.T, s *ledger.Store) []byte {
	t.Helper()
	b, err := os.ReadFile(s.LedgerPath())
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func readLedger(t *testing.T, s *ledger.Store) []ledger.Entry {
	t.Helper()
	es, _, err := ledger.ReadFile(s.LedgerPath())
	if err != nil {
		t.Fatal(err)
	}
	return es
}

// assertAppendOnly fails unless after is before plus exactly n new lines.
func assertAppendOnly(t *testing.T, before, after []byte, n int) {
	t.Helper()
	if !bytes.HasPrefix(after, before) {
		t.Fatal("existing ledger bytes changed")
	}
	if got := bytes.Count(after[len(before):], []byte("\n")); got != n {
		t.Fatalf("appended %d lines, want %d", got, n)
	}
}

type ledgerCall struct {
	ts       string
	provider string
	in, out  int64
}

func appendCalls(t *testing.T, s *ledger.Store, calls []ledgerCall) {
	t.Helper()
	for _, c := range calls {
		e := ledger.Entry{"type": ledger.TypeCall, "ts": c.ts, "provider": c.provider,
			"tokens_in": c.in, "tokens_out": c.out}
		if err := s.Append(e); err != nil {
			t.Fatal(err)
		}
	}
}

// corrupt appends a line that is not JSON, as a torn write would leave.
func corrupt(t *testing.T, s *ledger.Store) {
	t.Helper()
	f, err := os.OpenFile(s.LedgerPath(), os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("{torn\n"); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}
