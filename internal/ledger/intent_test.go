package ledger_test

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/devrdias/ambermoth/internal/ledger"
)

func TestVerifyIntents(t *testing.T) {
	for _, c := range allIntentCases() {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			r := verifyCorpusInput(t, c.build(t))
			if c.errPart == "" && !r.OK() {
				t.Errorf("errors: %v", r.Errors)
			}
			if c.errPart != "" && !strings.Contains(strings.Join(r.Errors, "\n"), c.errPart) {
				t.Errorf("errors %q lack %q", r.Errors, c.errPart)
			}
			var got []int64
			for _, o := range r.Orphans {
				got = append(got, o.Seq)
			}
			if len(got) != len(c.orphans) || r.OrphanIntents != len(c.orphans) {
				t.Fatalf("orphans %v (count %d), want %v", got, r.OrphanIntents, c.orphans)
			}
			for i := range got {
				if got[i] != c.orphans[i] {
					t.Fatalf("orphans %v, want %v", got, c.orphans)
				}
			}
		})
	}
}

// An orphan is listed with its seq, ts and endpoint, in seq order, and an
// orphan changes neither the verdict nor the other fields of the report.
func TestVerifyOrphanListCarriesSeqTimeAndEndpoint(t *testing.T) {
	bd := newCorpusBuilder(t)
	fillBasic(bd)
	e := intentEntry(hexID("a"))
	e["endpoint"] = "realtime"
	bd.put(minutes(8), e)
	bd.put(minutes(9), intentEntry(hexID("b")))
	r := verifyCorpusInput(t, bd.input(fakeAnchorCheck))

	if !r.OK() || r.OrphanIntents != 2 || len(r.Orphans) != 2 {
		t.Fatalf("ok=%v count=%d list=%v", r.OK(), r.OrphanIntents, r.Orphans)
	}
	want := []ledger.OrphanIntent{
		{Seq: 8, TS: "2026-03-01T00:08:00.000Z", Endpoint: "realtime"},
		{Seq: 9, TS: "2026-03-01T00:09:00.000Z", Endpoint: "chat.completions"},
	}
	for i, w := range want {
		if r.Orphans[i] != w {
			t.Errorf("orphan %d = %+v, want %+v", i, r.Orphans[i], w)
		}
	}
	if len(r.Warnings) != 0 {
		t.Errorf("an orphan is information, not a warning: %v", r.Warnings)
	}
}

// A ledger without intents reports none, and the JSON has no trace of the fields.
func TestReportJSONOmitsIntentFieldsWithoutIntents(t *testing.T) {
	r := verifyCorpusInput(t, basicInput(t))
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	keys := []string{"orphan", "intent"}
	for _, key := range keys {
		if bytes.Contains(b, []byte(key)) {
			t.Errorf("report JSON mentions %q: %s", key, b)
		}
	}
}

// Verify, over parsed entries, and VerifyReader, over the file, agree on intents.
func TestVerifyReaderAgreesWithVerifyOnIntents(t *testing.T) {
	for _, c := range allIntentCases() {
		in := c.build(t)
		got := projectReport(t, verifyReaderCorpusInput(t, in))
		want := projectReport(t, verifyCorpusInput(t, in))
		if !bytes.Equal(got, want) {
			t.Errorf("%s: reports differ\n%s", c.name, firstDifference(got, want))
		}
	}
}
