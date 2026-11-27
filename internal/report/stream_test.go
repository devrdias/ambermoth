package report_test

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"testing"
	"time"

	"github.com/devrdias/ambermoth/internal/ledger"
	"github.com/devrdias/ambermoth/internal/report"
)

func asJSON(t testing.TB, s *report.Summary) string {
	t.Helper()
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func stream(es []ledger.Entry, f report.Filter) *report.Summary {
	z := report.NewSummarizer(f)
	for _, e := range es {
		z.Add(e)
	}
	return z.Summary()
}

func TestSummarizerMatchesTheReferenceOnTheCorpus(t *testing.T) {
	for _, c := range corpusCases() {
		t.Run(c.name, func(t *testing.T) {
			var b built
			c.build(&b)
			want := asJSON(t, refSummarize(b.es, c.filter))
			if got := asJSON(t, stream(b.es, c.filter)); got != want {
				t.Errorf("accumulator differs\n got: %s\nwant: %s", got, want)
			}
			if got := asJSON(t, report.Summarize(b.es, c.filter)); got != want {
				t.Errorf("Summarize differs\n got: %s\nwant: %s", got, want)
			}
		})
	}
}

func TestSummaryMidStreamDoesNotChangeTheFinalSummary(t *testing.T) {
	var b built
	a := b.call(callSpec{day: 1})
	b.keyEvent("openai", "sha256:k1", "enroll", 1)
	z := report.NewSummarizer(report.Filter{})
	for i, e := range b.es {
		z.Add(e)
		_ = z.Summary()
		if i == 0 {
			b.amend(a, a, "eval")
			b.call(callSpec{day: 2})
		}
	}
	for _, e := range b.es[2:] {
		z.Add(e)
	}
	if got, want := asJSON(t, z.Summary()), asJSON(t, refSummarize(b.es, report.Filter{})); got != want {
		t.Errorf("got %s\nwant %s", got, want)
	}
}

// randomLedger builds a ledger that is not always valid: repeated and
// out-of-order seqs, bad timestamps, empty purposes, nil entries and ranges
// that reach past the calls, since a pack from someone else can hold any of them.
func randomLedger(r *rand.Rand, n int) []ledger.Entry {
	providers := []string{"openai", "anthropic", ""}
	purposes := []string{"untagged", "prod", "eval", ""}
	keys := []string{"sha256:a", "sha256:b", "sha256:c", ""}
	es := make([]ledger.Entry, 0, n)
	seq := int64(0)
	for i := 0; i < n; i++ {
		seq++
		s := seq
		switch r.Intn(20) {
		case 0:
			s = int64(r.Intn(n + 2))
		case 1:
			s = seq + int64(r.Intn(5))
		}
		es = append(es, randomEntry(r, s, providers, purposes, keys))
	}
	return es
}

func randomEntry(r *rand.Rand, seq int64, providers, purposes, keys []string) ledger.Entry {
	pick := func(xs []string) string { return xs[r.Intn(len(xs))] }
	ts := ledger.FormatTime(time.Date(2026, 3, 1+r.Intn(6), r.Intn(24), 0, 0, 0, time.UTC))
	if r.Intn(25) == 0 {
		ts = "bad"
	}
	var sq any = seq
	switch r.Intn(3) {
	case 0:
		sq = int(seq)
	case 1:
		sq = json.Number(fmt.Sprint(seq))
	}
	e := ledger.Entry{"seq": sq, "ts": ts, "provider": pick(providers)}
	switch k := r.Intn(20); {
	case k < 12:
		e["type"] = "call"
		e["purpose"] = pick(purposes)
		e["key_fp"] = pick(keys)
		e["endpoint"] = pick([]string{"chat", "embed"})
		e["model"] = pick([]string{"m1", "m2", "m3"})
		e["status"] = int64([]int{0, 200, 200, 429, 500}[r.Intn(5)])
		e["tokens_in"], e["tokens_out"], e["cached_in"] = int64(r.Intn(100)), int64(r.Intn(100)), int64(r.Intn(10))
		e["prompt_hmac"], e["output_hmac"] = fmt.Sprint("p", seq), fmt.Sprint("o", seq)
	case k < 16:
		e["type"] = "amendment"
		from := int64(r.Intn(int(seq) + 3))
		e["target_from"], e["target_to"] = from, from+int64(r.Intn(6))
		if r.Intn(8) == 0 {
			e["target_to"] = int64(1<<63 - 1)
		}
		e["new_purpose"] = pick(purposes)
	case k < 18:
		e["type"] = "key_event"
		e["key_fp"] = pick(keys)
		e["action"] = pick([]string{"enroll", "revoke", "auto_enroll"})
	default:
		e["type"] = pick([]string{"gap", "code_link", "reconciliation", "anchor", "genesis"})
	}
	return e
}

func randomFilter(r *rand.Rand) report.Filter {
	var f report.Filter
	f.Provider = []string{"", "", "openai", "anthropic"}[r.Intn(4)]
	if r.Intn(2) == 0 {
		f.From = time.Date(2026, 3, 1+r.Intn(4), 0, 0, 0, 0, time.UTC)
	}
	if r.Intn(2) == 0 {
		f.To = time.Date(2026, 3, 3+r.Intn(5), 0, 0, 0, 0, time.UTC)
	}
	return f
}

func checkRandom(t *testing.T, seed int64, n int) {
	t.Helper()
	r := rand.New(rand.NewSource(seed))
	es := randomLedger(r, n)
	if n > 0 && r.Intn(4) == 0 {
		es[r.Intn(len(es))] = nil
	}
	f := randomFilter(r)
	want := asJSON(t, refSummarize(es, f))
	if got := asJSON(t, stream(es, f)); got != want {
		t.Fatalf("seed %d n %d filter %+v\n got: %s\nwant: %s", seed, n, f, got, want)
	}
}

func TestSummarizerMatchesTheReferenceOnRandomLedgers(t *testing.T) {
	for seed := int64(0); seed < 400; seed++ {
		checkRandom(t, seed, int(seed%60))
	}
}

func FuzzSummarizerMatchesTheReference(f *testing.F) {
	f.Add(int64(1), uint8(30))
	f.Add(int64(42), uint8(200))
	f.Fuzz(func(t *testing.T, seed int64, n uint8) {
		checkRandom(t, seed, int(n))
	})
}

func TestSummarizerMatchesTheReferenceOnALargeLedger(t *testing.T) {
	es := make([]ledger.Entry, 0, 30000)
	for i := 0; i < 30000; i++ {
		es = append(es, benchEntry(i))
	}
	f := report.Filter{Provider: "openai", From: time.Date(2026, 1, 5, 0, 0, 0, 0, time.UTC)}
	want := asJSON(t, refSummarize(es, f))
	if got := asJSON(t, stream(es, f)); got != want {
		t.Fatal("accumulator differs from the reference on a generated ledger")
	}
}
