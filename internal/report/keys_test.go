package report_test

import (
	"testing"

	"github.com/devrdias/ambermoth/internal/ledger"
	"github.com/devrdias/ambermoth/internal/report"
)

// verify-pack re-renders summary.html and requires identical bytes, so the
// key order must not depend on map iteration.
func TestSummarizeOrdersTiedKeysByFingerprint(t *testing.T) {
	entries := []ledger.Entry{
		{"type": ledger.TypeKeyEvent, "seq": int64(1), "key_fp": "sha256:bb", "provider": "openai", "action": "enroll"},
		{"type": ledger.TypeKeyEvent, "seq": int64(2), "key_fp": "sha256:aa", "provider": "openai", "action": "enroll"},
		{"type": ledger.TypeKeyEvent, "seq": int64(3), "key_fp": "sha256:cc", "provider": "openai", "action": "enroll"},
	}
	for i := 0; i < 50; i++ {
		keys := report.Summarize(entries, report.Filter{}).Keys
		got := []string{keys[0].FP, keys[1].FP, keys[2].FP}
		want := []string{"sha256:aa", "sha256:bb", "sha256:cc"}
		if got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
			t.Fatalf("run %d: key order %v, want %v", i, got, want)
		}
	}
}
