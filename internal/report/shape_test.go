package report_test

import (
	"testing"
	"time"

	"github.com/devrdias/ambermoth/internal/ledger"
	"github.com/devrdias/ambermoth/internal/report"
)

const maxInt64 = int64(1<<63 - 1)

// Entries from a pack someone else sent can carry any seq and any target
// range. Summarizing them must finish and apply the amendment to the call it
// really targets.
func TestSummarizeAmendmentWithHugeRange(t *testing.T) {
	ts := ledger.FormatTime(time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC))
	call := ledger.Entry{"type": "call", "seq": int64(1), "ts": ts, "purpose": "untagged", "provider": "openai"}
	ranges := []struct {
		name     string
		seq      int64
		from, to int64
	}{
		{"far apart", maxInt64, 0, maxInt64},
		{"ends at the largest int", 5, 0, maxInt64},
		{"starts below zero", 5, -maxInt64, 1},
	}
	for _, r := range ranges {
		t.Run(r.name, func(t *testing.T) {
			am := ledger.Entry{"type": "amendment", "seq": r.seq, "ts": ts, "new_purpose": "eval", "target_from": r.from, "target_to": r.to}
			done := make(chan *report.Summary, 1)
			go func() { done <- report.Summarize([]ledger.Entry{call, am}, report.Filter{}) }()
			select {
			case s := <-done:
				if got := s.Totals.ByPurpose["eval"]; got == nil || got.Calls != 1 {
					t.Errorf("amendment not applied: %+v", s.Totals.ByPurpose)
				}
				if len(s.Amendments) != 1 {
					t.Errorf("%d amendments listed, want 1", len(s.Amendments))
				}
			case <-time.After(5 * time.Second):
				t.Fatal("Summarize did not finish")
			}
		})
	}
}
