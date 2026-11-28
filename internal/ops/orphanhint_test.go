package ops_test

import (
	"strings"
	"testing"

	"github.com/devrdias/ambermoth/internal/ledger"
	"github.com/devrdias/ambermoth/internal/ops"
)

func intentAt(ts, provider, id string) ledger.Entry {
	return ledger.Entry{"type": ledger.TypeIntent, "ts": ts, "provider": provider, "call_id": id, "endpoint": "chat"}
}

type hintCase struct {
	name     string
	csv      string
	calls    []ledgerCall
	intents  []ledger.Entry
	complete bool // a call completes the first intent, by its seq
	hint     string
	result   string
}

const (
	hintOne  = "1 intent in this period has no call entry. A request that was sent and not recorded shows as export above ledger."
	hintMany = "2 intents in this period have no call entry. A request that was sent and not recorded shows as export above ledger."
)

func hintCases() []hintCase {
	above := csvHead + "2026-03-02,m,200,20\n"
	calls := []ledgerCall{{day2, "openai", 100, 10}}
	return []hintCase{
		{name: "export above the ledger with an orphan in the period", csv: above, calls: calls,
			intents: []ledger.Entry{intentAt(day2, "openai", "a")}, hint: hintOne, result: "mismatch"},
		{name: "two orphans are counted", csv: above, calls: calls,
			intents: []ledger.Entry{intentAt(day2, "openai", "a"), intentAt(day2, "openai", "b")}, hint: hintMany, result: "mismatch"},
		{name: "export above the ledger and no intent", csv: above, calls: calls, result: "mismatch"},
		{name: "an intent that a call completed is not an orphan", csv: above, calls: calls,
			intents: []ledger.Entry{intentAt(day2, "openai", "a")}, complete: true, result: "mismatch"},
		{name: "a match gets no hint", csv: csvHead + "2026-03-02,m,100,10\n", calls: calls,
			intents: []ledger.Entry{intentAt(day2, "openai", "a")}, result: "match"},
		{name: "ledger above the export gets no hint", csv: csvHead + "2026-03-02,m,50,5\n", calls: calls,
			intents: []ledger.Entry{intentAt(day2, "openai", "a")}, result: "mismatch"},
		{name: "another provider's orphan is not counted", csv: above, calls: calls,
			intents: []ledger.Entry{intentAt(day2, "anthropic", "a")}, result: "mismatch"},
		{name: "an orphan outside the period is not counted", csv: above, calls: calls,
			intents: []ledger.Entry{intentAt(day3, "openai", "a"), intentAt("2026-03-01T10:00:00.000Z", "openai", "b")}, result: "mismatch"},
		{name: "an orphan on a later day of the period is counted", csv: csvHead + "2026-03-02,m,100,10\n2026-03-04,m,5,1\n", calls: calls,
			intents: []ledger.Entry{intentAt(day3, "openai", "a")}, hint: hintOne, result: "mismatch"},
	}
}

func TestReconcileHintsAtOrphansNextToAnExportAboveTheLedger(t *testing.T) {
	for _, c := range hintCases() {
		t.Run(c.name, func(t *testing.T) {
			s := newFixedStore(t)
			appendCalls(t, s, c.calls)
			for _, in := range c.intents {
				if err := s.Append(in); err != nil {
					t.Fatal(err)
				}
			}
			if c.complete {
				seq := readLedger(t, s)[2].Seq()
				done := ledger.Entry{"type": ledger.TypeCall, "ts": day2, "provider": "openai", "call_id": "a", "intent_seq": seq}
				if err := s.Append(done); err != nil {
					t.Fatal(err)
				}
			}
			e, lines, err := ops.Reconcile(s, "openai", writeCSV(t, c.csv), 0.02)
			if err != nil {
				t.Fatal(err)
			}
			if e.Str("result") != c.result {
				t.Errorf("result = %s, want %s", e.Str("result"), c.result)
			}
			last := lines[len(lines)-1]
			if c.hint == "" && strings.Contains(strings.Join(lines, "\n"), "intent") {
				t.Errorf("lines mention intents: %q", lines)
			}
			if c.hint != "" && last != c.hint {
				t.Errorf("last line = %q, want %q", last, c.hint)
			}
		})
	}
}

// The recorded entry and the totals are what they were: intents are not calls.
func TestReconcileEntryIgnoresIntents(t *testing.T) {
	run := func(withIntent bool) ledger.Entry {
		s := newFixedStore(t)
		appendCalls(t, s, []ledgerCall{{day2, "openai", 100, 10}})
		if withIntent {
			if err := s.Append(intentAt(day2, "openai", "a")); err != nil {
				t.Fatal(err)
			}
		}
		e, _, err := ops.Reconcile(s, "openai", writeCSV(t, csvHead+"2026-03-02,m,200,20\n"), 0.02)
		if err != nil {
			t.Fatal(err)
		}
		return e
	}
	with, without := run(true), run(false)
	for k, v := range without {
		if k == "seq" || k == "prev_hash" || k == "entry_hash" {
			continue
		}
		if with[k] != v {
			t.Errorf("field %s = %v with an intent, %v without", k, with[k], v)
		}
	}
	if len(with) != len(without) {
		t.Errorf("fields differ: %v and %v", with, without)
	}
}
