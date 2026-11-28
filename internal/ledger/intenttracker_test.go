package ledger_test

import (
	"reflect"
	"testing"

	"github.com/devrdias/ambermoth/internal/ledger"
)

func trackerIntent(seq int64, id, provider string) ledger.Entry {
	return ledger.Entry{"type": ledger.TypeIntent, "seq": seq, "call_id": id, "provider": provider,
		"ts": "2026-03-01T00:00:00.000Z", "endpoint": "chat"}
}

func trackerCall(seq int64, id string, intentSeq int64) ledger.Entry {
	return ledger.Entry{"type": ledger.TypeCall, "seq": seq, "call_id": id, "intent_seq": intentSeq}
}

type trackerCase struct {
	name    string
	keep    func(ledger.Entry) bool
	entries []ledger.Entry
	want    []int64
}

func TestIntentTracker(t *testing.T) {
	onlyOpenAI := func(e ledger.Entry) bool { return e.Str("provider") == "openai" }
	cases := []trackerCase{
		{name: "no entries"},
		{name: "an intent with no call is an orphan", entries: []ledger.Entry{trackerIntent(1, "a", "openai")}, want: []int64{1}},
		{name: "a call closes its intent", entries: []ledger.Entry{trackerIntent(1, "a", "openai"), trackerCall(2, "a", 1)}},
		{name: "a call with the wrong intent_seq closes nothing",
			entries: []ledger.Entry{trackerIntent(1, "a", "openai"), trackerCall(2, "a", 7)}, want: []int64{1}},
		{name: "a call with another call_id closes nothing",
			entries: []ledger.Entry{trackerIntent(1, "a", "openai"), trackerCall(2, "b", 1)}, want: []int64{1}},
		{name: "a call with no call_id is not looked at",
			entries: []ledger.Entry{trackerIntent(1, "a", "openai"), {"type": ledger.TypeCall, "seq": int64(2)}}, want: []int64{1}},
		{name: "a call whose intent is not seen changes nothing",
			entries: []ledger.Entry{trackerCall(5, "x", 2), trackerIntent(6, "a", "openai")}, want: []int64{6}},
		{name: "orphans come in seq order",
			entries: []ledger.Entry{trackerIntent(9, "c", "openai"), trackerIntent(3, "a", "openai"), trackerIntent(5, "b", "openai")},
			want:    []int64{3, 5, 9}},
		{name: "keep leaves out the intents it rejects",
			keep:    onlyOpenAI,
			entries: []ledger.Entry{trackerIntent(1, "a", "anthropic"), trackerIntent(2, "b", "openai")}, want: []int64{2}},
		{name: "an intent without a call_id is not tracked", entries: []ledger.Entry{{"type": ledger.TypeIntent, "seq": int64(1)}}},
		{name: "other entry types and nil are skipped",
			entries: []ledger.Entry{nil, {"type": ledger.TypeGap, "seq": int64(1)}, trackerIntent(2, "a", "openai")}, want: []int64{2}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tr := ledger.NewIntentTracker(c.keep)
			for _, e := range c.entries {
				tr.Add(e)
			}
			var got []int64
			for _, o := range tr.Orphans() {
				got = append(got, o.Seq)
			}
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("orphans = %v, want %v", got, c.want)
			}
		})
	}
}

func TestIntentTrackerOrphanCarriesSeqTimeAndEndpoint(t *testing.T) {
	tr := ledger.NewIntentTracker(nil)
	tr.Add(trackerIntent(4, "a", "openai"))
	want := []ledger.OrphanIntent{{Seq: 4, TS: "2026-03-01T00:00:00.000Z", Endpoint: "chat"}}
	if got := tr.Orphans(); !reflect.DeepEqual(got, want) {
		t.Errorf("orphans = %+v, want %+v", got, want)
	}
}
