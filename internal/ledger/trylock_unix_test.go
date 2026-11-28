//go:build unix

package ledger_test

import (
	"errors"
	"testing"

	"github.com/devrdias/ambermoth/internal/ledger"
)

func TestTryLockExcludesWritersWithoutWaiting(t *testing.T) {
	s, _ := threeEntryStore(t)
	unlock, err := ledger.TryLock(s.Dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ledger.TryLock(s.Dir); !errors.Is(err, ledger.ErrLocked) {
		t.Errorf("second TryLock = %v, want ErrLocked", err)
	}
	unlock()
	again, err := ledger.TryLock(s.Dir)
	if err != nil {
		t.Fatalf("TryLock after unlock: %v", err)
	}
	again()
	if err := s.Append(call()); err != nil {
		t.Errorf("Append after unlock: %v", err)
	}
}
