package report_test

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/devrdias/ambermoth/internal/ledger"
	"github.com/devrdias/ambermoth/internal/report"
)

func (b *built) intent(provider, id string, day, hour int) int64 {
	return b.add(ledger.Entry{"type": "intent", "ts": at(day, hour), "provider": provider, "call_id": id,
		"endpoint": "chat", "purpose": "prod"})
}

func (b *built) complete(provider, id string, intentSeq int64, day, hour int) int64 {
	return b.add(ledger.Entry{"type": "call", "ts": at(day, hour), "provider": provider, "purpose": "prod", "status": int64(200),
		"call_id": id, "intent_seq": intentSeq, "tokens_in": int64(10), "tokens_out": int64(2)})
}

type orphanCase struct {
	name   string
	build  func(b *built)
	filter report.Filter
	want   []int64
	calls  int64
}

func orphanCases() []orphanCase {
	return []orphanCase{
		{name: "no intents", build: func(b *built) { b.call(callSpec{day: 1}) }, calls: 1},
		{name: "completed intents leave no orphan", build: func(b *built) {
			i := b.intent("openai", "a", 1, 0)
			b.complete("openai", "a", i, 1, 0)
		}, calls: 1},
		{name: "an intent with no call is an orphan and adds no call", build: func(b *built) {
			b.intent("openai", "a", 1, 0)
			b.call(callSpec{day: 1, hour: 1})
		}, want: []int64{1}, calls: 1},
		{name: "a call that names another intent closes nothing", build: func(b *built) {
			b.intent("openai", "a", 1, 0)
			b.complete("openai", "a", 9, 1, 1)
		}, want: []int64{1}, calls: 1},
		{name: "the provider filter drops the other providers' orphans", build: func(b *built) {
			b.intent("openai", "a", 1, 0)
			b.intent("anthropic", "b", 1, 1)
		}, filter: report.Filter{Provider: "anthropic"}, want: []int64{2}},
		{name: "the period filter drops orphans outside it", build: func(b *built) {
			b.intent("openai", "a", 1, 0)
			b.intent("openai", "b", 3, 0)
			b.intent("openai", "c", 6, 0)
		}, filter: report.Filter{From: day(2), To: day(5)}, want: []int64{2}},
		{name: "an intent completed after the period is still not an orphan of it", build: func(b *built) {
			i := b.intent("openai", "a", 1, 23)
			b.complete("openai", "a", i, 2, 0)
		}, filter: report.Filter{To: day(2)}, calls: 0},
	}
}

func TestSummarizerCountsOrphanIntents(t *testing.T) {
	for _, c := range orphanCases() {
		t.Run(c.name, func(t *testing.T) {
			var b built
			c.build(&b)
			s := report.Summarize(b.es, c.filter)
			var got []int64
			for _, o := range s.Orphans {
				got = append(got, o.Seq)
			}
			if !reflect.DeepEqual(got, c.want) || s.OrphanIntents != len(c.want) {
				t.Errorf("orphans %v (count %d), want %v", got, s.OrphanIntents, c.want)
			}
			if s.Totals.All.Calls != c.calls {
				t.Errorf("calls = %d, want %d: totals come from call entries only", s.Totals.All.Calls, c.calls)
			}
		})
	}
}

func TestSummaryOrphanFieldsCarrySeqTimeAndEndpoint(t *testing.T) {
	var b built
	b.intent("openai", "a", 1, 5)
	got := report.Summarize(b.es, report.Filter{}).Orphans
	want := []ledger.OrphanIntent{{Seq: 1, TS: at(1, 5), Endpoint: "chat"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("orphans = %+v, want %+v", got, want)
	}
}

// A summary of a ledger without orphans has no trace of the fields in its JSON.
func TestSummaryJSONOmitsOrphansWhenThereAreNone(t *testing.T) {
	var b built
	i := b.intent("openai", "a", 1, 0)
	b.complete("openai", "a", i, 1, 0)
	j, err := json.Marshal(report.Summarize(b.es, report.Filter{}))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(j, []byte("Orphan")) {
		t.Errorf("summary JSON mentions orphans: %s", j)
	}
}

func TestSummaryJSONHasOrphansWhenThereAreSome(t *testing.T) {
	var b built
	b.intent("openai", "a", 1, 0)
	j, err := json.Marshal(report.Summarize(b.es, report.Filter{}))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(j, []byte(`"OrphanIntents":1`)) || !bytes.Contains(j, []byte(`"Orphans":[{"seq":1`)) {
		t.Errorf("summary JSON lacks the orphans: %s", j)
	}
}
