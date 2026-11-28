package ledger_test

import (
	"errors"
	"os"
	"testing"
)

var errSync = errors.New("fsync failed")

// An fsync error comes back from Append, and the chain carries on after it:
// the bytes may be on disk, so the next append continues from them.
func TestAppendReturnsAnFsyncErrorAndTheChainContinues(t *testing.T) {
	s := initStoreAt(t, epoch)
	assertHead(t, s, 0)
	failing := true
	s.SetSync(func(f *os.File) error {
		if failing {
			return errSync
		}
		return f.Sync()
	})

	if err := s.Append(call("provider", "a")); !errors.Is(err, errSync) {
		t.Fatalf("append error = %v, want the fsync error", err)
	}
	failing = false
	next := call("provider", "b")
	if err := s.Append(next); err != nil {
		t.Fatalf("append after the failure: %v", err)
	}
	if next.Seq() != 2 {
		t.Errorf("seq after a failed sync = %d, want 2 (the unsynced entry is on disk)", next.Seq())
	}
	assertValid(t, s)
}

// One batch is one fsync, however many entries it holds.
func TestAppendSyncsOncePerBatch(t *testing.T) {
	s := initStoreAt(t, epoch)
	n := 0
	s.SetSync(func(f *os.File) error { n++; return f.Sync() })

	if err := s.Append(call(), call(), call()); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("%d fsyncs for one batch of 3, want 1", n)
	}
	if err := s.Append(call()); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("%d fsyncs after two appends, want 2", n)
	}
}
