package ledger_test

import (
	"bytes"
	"github.com/devrdias/ambermoth/internal/ledger"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

func newTestStore(t *testing.T) *ledger.Store {
	t.Helper()
	s, err := ledger.Init(t.TempDir(), "ambermoth/test", time.Now)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func verifyFile(t *testing.T, s *ledger.Store) *ledger.Report {
	t.Helper()
	es, lines, err := ledger.ReadFile(s.LedgerPath())
	if err != nil {
		t.Fatal(err)
	}
	return ledger.Verify(es, lines, ledger.VerifyOptions{})
}

func TestAppendAndVerify(t *testing.T) {
	s := newTestStore(t)
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := s.Append(ledger.Entry{"type": ledger.TypeCall, "provider": "openai", "tokens_in": 10}); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	r := verifyFile(t, s)
	if !r.OK() {
		t.Fatalf("expected OK, got %v", r.Errors)
	}
	if r.LastSeq != 20 || !r.StartsAtGenesis {
		t.Fatalf("unexpected report %+v", r)
	}
	// Reopen and keep appending: tail must be picked up from disk.
	s2, err := ledger.Open(s.Dir, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	if err := s2.Append(ledger.Entry{"type": ledger.TypeCall}); err != nil {
		t.Fatal(err)
	}
	if r := verifyFile(t, s2); !r.OK() || r.LastSeq != 21 {
		t.Fatalf("after reopen: %+v", r)
	}
}

func TestTamperDetection(t *testing.T) {
	s := newTestStore(t)
	for i := 0; i < 5; i++ {
		s.Append(ledger.Entry{"type": ledger.TypeCall, "purpose": "eval-regression", "tokens_in": 100})
	}
	orig, _ := os.ReadFile(s.LedgerPath())

	cases := map[string]func([]byte) []byte{
		"edit": func(b []byte) []byte {
			return bytes.Replace(b, []byte(`"tokens_in":100`), []byte(`"tokens_in":101`), 1)
		},
		"delete": func(b []byte) []byte {
			ls := strings.SplitAfter(string(b), "\n")
			return []byte(strings.Join(append(ls[:2], ls[3:]...), ""))
		},
		"reorder": func(b []byte) []byte {
			ls := strings.SplitAfter(string(b), "\n")
			ls[2], ls[3] = ls[3], ls[2]
			return []byte(strings.Join(ls, ""))
		},
		"non-canonical": func(b []byte) []byte {
			return bytes.Replace(b, []byte(`{"entry_hash"`), []byte(`{ "entry_hash"`), 1)
		},
	}
	for name, mut := range cases {
		os.WriteFile(s.LedgerPath(), mut(orig), 0o600)
		if r := verifyFile(t, s); r.OK() {
			t.Errorf("%s: tampering not detected", name)
		}
	}
	os.WriteFile(s.LedgerPath(), orig, 0o600)
	if r := verifyFile(t, s); !r.OK() {
		t.Fatalf("restored ledger should verify: %v", r.Errors)
	}
}

func TestWrongKeyRejected(t *testing.T) {
	s := newTestStore(t)
	os.WriteFile(s.Dir+"/"+ledger.KeyFile, []byte(strings.Repeat("ab", 32)), 0o600)
	if _, err := ledger.Open(s.Dir, time.Now); err == nil {
		t.Fatal("expected key commitment mismatch")
	}
}

// Entries anchored days after their own timestamps are only proven from the
// anchor time on; ledger.Verify must say so.
func TestAnchorLag(t *testing.T) {
	s := newTestStore(t)
	now := time.Now().UTC()
	s.Append(ledger.Entry{"type": ledger.TypeCall, "ts": ledger.FormatTime(now.Add(-72 * time.Hour))})
	s.Append(ledger.Entry{"type": ledger.TypeCall, "ts": ledger.FormatTime(now.Add(-time.Minute))})
	_, head, _ := s.Head()
	s.Append(ledger.Entry{"type": ledger.TypeAnchor, "target_seq": int64(2), "target_hash": head, "token_file": "t.tsr", "token_sha256": ledger.SHA256Bytes([]byte("tok"))})
	os.WriteFile(s.Dir+"/"+ledger.AnchorDir+"/t.tsr", []byte("tok"), 0o600)
	es, lines, _ := ledger.ReadFile(s.LedgerPath())
	r := ledger.Verify(es, lines, ledger.VerifyOptions{AnchorDir: s.Dir + "/" + ledger.AnchorDir,
		CheckAnchor: func([]byte, string) (ledger.AnchorCheck, error) {
			return ledger.AnchorCheck{GenTime: now, Signer: "x", Pinned: true}, nil
		}})
	if !r.OK() {
		t.Fatal(r.Errors)
	}
	if r.MaxAnchorLagSeq != 1 || r.MaxAnchorLagSeconds < 71*3600 || r.EntriesLaggingOver24h != 1 {
		t.Fatalf("lag not computed: seq %d, %ds, over24h %d", r.MaxAnchorLagSeq, r.MaxAnchorLagSeconds, r.EntriesLaggingOver24h)
	}
	if !strings.Contains(strings.Join(r.Warnings, " "), "first anchored more than 24h") {
		t.Errorf("missing lag warning: %v", r.Warnings)
	}
}
