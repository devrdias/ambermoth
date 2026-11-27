package report_test

import (
	"github.com/devrdias/ambermoth/internal/report"
	"testing"
	"time"

	"github.com/devrdias/ambermoth/internal/ledger"
)

func TestEffectivePurposeAndAlerts(t *testing.T) {
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	var es []ledger.Entry
	seq := int64(0)
	add := func(e ledger.Entry) {
		seq++
		e["seq"] = seq
		es = append(es, e)
	}
	// 14 quiet days of 10 calls, then a 2,000-call benchmark day.
	for d := 15; d >= 1; d-- {
		for i := 0; i < 10; i++ {
			add(ledger.Entry{"type": "call", "ts": ledger.FormatTime(now.AddDate(0, 0, -d)), "key_fp": "sha256:k", "purpose": "production-inference", "endpoint": "messages"})
		}
	}
	first := seq + 1
	for i := 0; i < 2000; i++ {
		add(ledger.Entry{"type": "call", "ts": ledger.FormatTime(now.Add(-time.Hour)), "key_fp": "sha256:k", "purpose": "untagged", "endpoint": "messages"})
	}
	add(ledger.Entry{"type": "amendment", "target_from": first, "target_to": seq, "new_purpose": "eval-benchmark", "reason": "r", "by": "b"})

	s := report.Summarize(es, report.Filter{})
	if s.Totals.ByPurpose["eval-benchmark"].Calls != 2000 || s.Totals.ByPurpose["eval-benchmark"].Retagged != 2000 {
		t.Fatalf("amendment not applied: %+v", s.Totals.ByPurpose)
	}
	kinds := map[string]int{}
	for _, a := range report.Alerts(es, now, report.AlertConfig{}) {
		kinds[a.Kind]++
	}
	if kinds["volume"] == 0 || kinds["concentration"] == 0 {
		t.Fatalf("expected volume and concentration alerts, got %v", kinds)
	}
}
