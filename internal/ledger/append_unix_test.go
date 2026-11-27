//go:build unix

package ledger_test

import (
	"bytes"
	"errors"
	"os/signal"
	"syscall"
	"testing"
)

// capFileSize makes any write that grows a file past size fail with EFBIG until
// the returned function is called (or the test ends). It is the only way to get
// a failing write out of a regular file without a seam in the store.
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

func TestAppendWriteErrorLeavesTheTailAlone(t *testing.T) {
	s, before := threeEntryStore(t)
	head := assertHead(t, s, 2)
	lift := capFileSize(t, uint64(len(before)))
	err := s.Append(call(), call())
	if !errors.Is(err, syscall.EFBIG) {
		t.Fatalf("err = %v, want EFBIG", err)
	}
	if !bytes.Equal(readBytes(t, s.LedgerPath()), before) {
		t.Fatal("ledger changed by a failed write")
	}
	lift()

	if got := assertHead(t, s, 2); got != head {
		t.Errorf("head moved to %s after a failed write", got)
	}
	e := call()
	if err := s.Append(e); err != nil {
		t.Fatal(err)
	}
	if e.Seq() != 3 || e.Str("prev_hash") != head {
		t.Errorf("next entry seq %d prev %s, want 3 after %s", e.Seq(), e.Str("prev_hash"), head)
	}
	if r := assertValid(t, s); r.LastSeq != 3 {
		t.Errorf("report %+v", r)
	}
}
