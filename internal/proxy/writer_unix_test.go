//go:build unix

package proxy_test

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/devrdias/ambermoth/internal/proxy"
)

// capFileSize makes a write that grows a file past size fail with EFBIG until
// the returned function is called or the test ends.
func capFileSize(t *testing.T, size uint64) (lift func()) {
	t.Helper()
	var old syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_FSIZE, &old); err != nil {
		t.Skipf("no RLIMIT_FSIZE: %v", err)
	}
	signal.Ignore(syscall.SIGXFSZ)
	lim := old
	lim.Cur = size
	if err := syscall.Setrlimit(syscall.RLIMIT_FSIZE, &lim); err != nil {
		signal.Reset(syscall.SIGXFSZ)
		t.Skipf("cannot lower RLIMIT_FSIZE: %v", err)
	}
	lift = func() {
		_ = syscall.Setrlimit(syscall.RLIMIT_FSIZE, &old)
		signal.Reset(syscall.SIGXFSZ)
	}
	t.Cleanup(lift)
	return lift
}

// syncBuf is a log sink the writer goroutine and the test can share.
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) count(sub string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return strings.Count(s.b.String(), sub)
}

// An I/O failure drops the batch as one, as before: the entries are not retried
// one by one, and the next write that succeeds starts with a gap entry.
func TestWriterIOFailureDropsTheBatchOnce(t *testing.T) {
	s := newStore(t)
	before, err := os.ReadFile(s.LedgerPath())
	if err != nil {
		t.Fatal(err)
	}
	lift := capFileSize(t, uint64(len(before)))

	var logs syncBuf
	w := proxy.NewWriter(s, 100, slog.New(slog.NewTextHandler(&logs, nil)))
	w.FlushEvery = 5 * time.Millisecond
	go func() { _ = w.Run(context.Background()) }()
	for i := int64(1); i <= 3; i++ {
		w.Enqueue(goodCall(i))
	}
	deadline := time.Now().Add(3 * time.Second)
	for logs.count("entries=3") == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if n := logs.count("entries=3"); n != 1 {
		t.Fatalf("%d failures logged for the batch of 3, want 1", n)
	}
	if n := logs.count("entries=1 "); n != 0 {
		t.Fatalf("%d single-entry retries after an I/O failure, want none", n)
	}
	lift()

	w.Enqueue(goodCall(4))
	w.Close()
	es, err := s.Entries()
	if err != nil {
		t.Fatal(err)
	}
	if len(es) != 3 || es[1].Str("type") != "gap" || es[1].Int("dropped") != 3 ||
		!strings.Contains(es[1].Str("cause"), "ledger write error") || es[2].Int("tokens_in") != 4 {
		t.Fatalf("ledger after the failure: %v", es)
	}
	if r := verifyStore(t, s); !r.OK() {
		t.Fatalf("ledger does not verify: %v", r.Errors)
	}
}
