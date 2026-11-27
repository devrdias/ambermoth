package proxy_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/devrdias/ambermoth/internal/ledger"
	"github.com/devrdias/ambermoth/internal/proxy"
)

// tooBig is outside the canonical number range (SPEC §3.1), so the entry
// cannot be encoded.
const tooBig = int64(1) << 60

func goodCall(tokens int64) ledger.Entry {
	return ledger.Entry{"type": ledger.TypeCall, "tokens_in": tokens}
}

func badCall() ledger.Entry { return goodCall(tooBig) }

// writeBatch queues entries and closes the writer, so they go out as one batch.
func writeBatch(t *testing.T, s *ledger.Store, entries ...ledger.Entry) {
	t.Helper()
	w := proxy.NewWriter(s, 100, discard)
	w.FlushEvery = time.Hour
	for _, e := range entries {
		if !w.Enqueue(e) {
			t.Fatal("queue full")
		}
	}
	go func() { _ = w.Run(context.Background()) }()
	w.Close()
}

func newStore(t *testing.T) *ledger.Store {
	t.Helper()
	s, err := ledger.Init(t.TempDir(), "test", time.Now)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

type batchCase struct {
	name     string
	batch    []ledger.Entry
	wantIn   []int64 // tokens_in of the call entries written, in order
	wantGone int64   // dropped count of the gap entry, 0 for none
}

func TestWriterDropsOnlyTheEntryThatCannotBeEncoded(t *testing.T) {
	cases := []batchCase{
		{name: "the third of five", batch: []ledger.Entry{goodCall(1), goodCall(2), badCall(), goodCall(4), goodCall(5)},
			wantIn: []int64{1, 2, 4, 5}, wantGone: 1},
		{name: "the first", batch: []ledger.Entry{badCall(), goodCall(2), goodCall(3)}, wantIn: []int64{2, 3}, wantGone: 1},
		{name: "the last", batch: []ledger.Entry{goodCall(1), goodCall(2), badCall()}, wantIn: []int64{1, 2}, wantGone: 1},
		{name: "two apart", batch: []ledger.Entry{badCall(), goodCall(2), badCall(), goodCall(4)}, wantIn: []int64{2, 4}, wantGone: 2},
		{name: "all of them", batch: []ledger.Entry{badCall(), badCall(), badCall()}, wantGone: 3},
		{name: "none", batch: []ledger.Entry{goodCall(1), goodCall(2)}, wantIn: []int64{1, 2}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := newStore(t)
			writeBatch(t, s, c.batch...)
			es, err := s.Entries()
			if err != nil {
				t.Fatal(err)
			}
			var got []int64
			var gaps []ledger.Entry
			for _, e := range es {
				switch e.Type() {
				case ledger.TypeCall:
					got = append(got, e.Int("tokens_in"))
				case ledger.TypeGap:
					gaps = append(gaps, e)
				}
			}
			if len(got) != len(c.wantIn) {
				t.Fatalf("calls written %v, want %v", got, c.wantIn)
			}
			for i := range got {
				if got[i] != c.wantIn[i] {
					t.Fatalf("calls written %v, want %v", got, c.wantIn)
				}
			}
			checkGap(t, gaps, c.wantGone)
			if r := verifyStore(t, s); !r.OK() {
				t.Fatalf("ledger does not verify: %v", r.Errors)
			}
		})
	}
}

func checkGap(t *testing.T, gaps []ledger.Entry, want int64) {
	t.Helper()
	if want == 0 {
		if len(gaps) != 0 {
			t.Fatalf("%d gap entries, want none", len(gaps))
		}
		return
	}
	if len(gaps) != 1 {
		t.Fatalf("%d gap entries, want 1", len(gaps))
	}
	cause := gaps[0].Str("cause")
	if gaps[0].Int("dropped") != want || !strings.Contains(cause, "could not be encoded") {
		t.Errorf("gap dropped %d cause %q, want %d and 'could not be encoded'", gaps[0].Int("dropped"), cause, want)
	}
	if strings.Contains(cause, "1152921504606846976") {
		t.Errorf("cause carries the entry's content: %q", cause)
	}
}

func verifyStore(t *testing.T, s *ledger.Store) *ledger.Report {
	t.Helper()
	es, lines, err := ledger.ReadFile(s.LedgerPath())
	if err != nil {
		t.Fatal(err)
	}
	return ledger.Verify(es, lines, ledger.VerifyOptions{})
}

// A gap that is already pending is written before the entries of a batch that
// has an unencodable one, and counts only what it counted before.
func TestWriterPendingGapSurvivesAnEncodeFailure(t *testing.T) {
	s := newStore(t)
	w := proxy.NewWriter(s, 1, discard)
	w.FlushEvery = time.Hour
	w.Enqueue(goodCall(1))
	if w.Enqueue(goodCall(2)) { // queue of 1 is full: dropped, pending gap
		t.Fatal("want the second entry dropped")
	}
	go func() { _ = w.Run(context.Background()) }()
	w.Close()
	es, _ := s.Entries()
	if n := len(es); n != 3 || es[1].Type() != ledger.TypeGap || es[1].Int("dropped") != 1 {
		t.Fatalf("setup: %v", es)
	}

	// Same again, now with the batch holding a bad entry.
	w = proxy.NewWriter(s, 2, discard)
	w.FlushEvery = time.Hour
	w.Enqueue(badCall())
	w.Enqueue(goodCall(3))
	w.Enqueue(goodCall(4)) // full: dropped
	go func() { _ = w.Run(context.Background()) }()
	w.Close()
	es, _ = s.Entries()
	var types []string
	var dropped []int64
	for _, e := range es[3:] {
		types = append(types, e.Type())
		dropped = append(dropped, e.Int("dropped"))
	}
	// The queue-full drop and the encode drop are one gap, written after the good entry.
	if strings.Join(types, ",") != "gap,call,gap" || dropped[0] != 1 || dropped[2] != 1 {
		t.Fatalf("types %v dropped %v", types, dropped)
	}
}
