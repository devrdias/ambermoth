package ops_test

import (
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/devrdias/ambermoth/internal/ledger"
	"github.com/devrdias/ambermoth/internal/ops"
	"github.com/devrdias/ambermoth/internal/report"
)

// amendStore holds, by seq: 1-3 calls without a purpose, 4 a key event, 5 a call, 6 an amendment of 5.
func amendStore(t *testing.T) *ledger.Store {
	t.Helper()
	s := newFixedStore(t)
	calls := []ledgerCall{
		{day2, "openai", 1, 1}, {day3, "openai", 2, 2}, {day4, "openai", 3, 3},
	}
	appendCalls(t, s, calls)
	if err := s.Append(ledger.Entry{"type": ledger.TypeKeyEvent, "action": "enroll", "key_fp": "sha256:x"}); err != nil {
		t.Fatal(err)
	}
	appendCalls(t, s, []ledgerCall{{day4, "openai", 4, 4}})
	if err := s.Append(ledger.Entry{"type": ledger.TypeAmendment, "target_from": int64(5), "target_to": int64(5),
		"new_purpose": "batch-job", "reason": "seed", "by": "seed"}); err != nil {
		t.Fatal(err)
	}
	return s
}

type amendRefuseCase struct {
	name          string
	from, to      int64
	purpose, note string
	why, who      string
	want          string
}

var amendRefuseCases = []amendRefuseCase{
	{name: "no reason", from: 1, to: 1, purpose: "eval-regression", who: "me",
		want: "--reason and --by are required: amendments must say why and who"},
	{name: "no author", from: 1, to: 1, purpose: "eval-regression", why: "x",
		want: "--reason and --by are required: amendments must say why and who"},
	{name: "no reason and no author", from: 1, to: 1, purpose: "eval-regression",
		want: "--reason and --by are required: amendments must say why and who"},
	{name: "reason is checked before the range", from: 99, to: 1, purpose: "eval-regression", who: "me",
		want: "--reason and --by are required"},
	{name: "neither purpose nor annotation", from: 1, to: 1, why: "x", who: "me",
		want: "give --purpose and/or --annotation"},
	{name: "genesis is not a target", from: 0, to: 1, purpose: "eval-regression", why: "x", who: "me",
		want: "invalid target range 0..1 (head is 6)"},
	{name: "negative start", from: -1, to: 2, purpose: "eval-regression", why: "x", who: "me",
		want: "invalid target range -1..2 (head is 6)"},
	{name: "reversed range", from: 3, to: 2, purpose: "eval-regression", why: "x", who: "me",
		want: "invalid target range 3..2 (head is 6)"},
	{name: "end past the head", from: 1, to: 7, purpose: "eval-regression", why: "x", who: "me",
		want: "invalid target range 1..7 (head is 6)"},
	{name: "start past the head", from: 7, to: 7, purpose: "eval-regression", why: "x", who: "me",
		want: "invalid target range 7..7 (head is 6)"},
	{name: "a key event is not a call", from: 4, to: 4, purpose: "eval-regression", why: "x", who: "me",
		want: "no call entries in range 4..4"},
	{name: "an amendment is not a call", from: 6, to: 6, purpose: "eval-regression", why: "x", who: "me",
		want: "no call entries in range 6..6"},
	{name: "genesis only is out of range, not a call", from: 0, to: 0, note: "n", why: "x", who: "me",
		want: "invalid target range 0..0 (head is 6)"},
}

func TestAmendRefuses(t *testing.T) {
	for _, c := range amendRefuseCases {
		t.Run(c.name, func(t *testing.T) {
			s := amendStore(t)
			before := ledgerBytes(t, s)
			e, err := ops.Amend(s, c.from, c.to, c.purpose, c.note, c.why, c.who)
			if err == nil {
				t.Fatal("want an error")
			}
			if e != nil {
				t.Errorf("want no entry, got %v", e)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("error %q, want it to contain %q", err, c.want)
			}
			assertAppendOnly(t, before, ledgerBytes(t, s), 0)
		})
	}
}

type amendOKCase struct {
	name          string
	from, to      int64
	purpose, note string
	why, who      string
}

var amendOKCases = []amendOKCase{
	{name: "single seq", from: 2, to: 2, purpose: "eval-regression", why: "mis-tagged", who: "rafa"},
	{name: "range of calls", from: 1, to: 3, purpose: "eval-benchmark", why: "run on the 2nd", who: "rafa"},
	{name: "range with a non-call in it", from: 3, to: 5, purpose: "batch-job", why: "mixed", who: "rafa"},
	{name: "range over an earlier amendment", from: 5, to: 6, purpose: "rag-query", why: "again", who: "rafa"},
	{name: "annotation only", from: 1, to: 1, note: "ran during the outage", why: "context", who: "rafa"},
	{name: "purpose and annotation", from: 1, to: 2, purpose: "dev-experiment", note: "scratch", why: "context", who: "rafa"},
	{name: "a purpose outside the taxonomy", from: 1, to: 1, purpose: "Not A Tag!", why: "x", who: "rafa"},
	{name: "a blank reason", from: 1, to: 1, purpose: "x-custom", why: " ", who: "rafa"},
}

func TestAmendAppends(t *testing.T) {
	for _, c := range amendOKCases {
		t.Run(c.name, func(t *testing.T) {
			s := amendStore(t)
			before := ledgerBytes(t, s)
			head, headHash, err := s.Head()
			if err != nil {
				t.Fatal(err)
			}
			e, err := ops.Amend(s, c.from, c.to, c.purpose, c.note, c.why, c.who)
			if err != nil {
				t.Fatal(err)
			}
			assertAppendOnly(t, before, ledgerBytes(t, s), 1)

			want := []string{"by", "entry_hash", "prev_hash", "reason", "seq", "target_from", "target_to", "ts", "type", "v"}
			if c.purpose != "" {
				want = append(want, "new_purpose")
			}
			if c.note != "" {
				want = append(want, "annotation")
			}
			sort.Strings(want)
			got := make([]string, 0, len(e))
			for k := range e {
				got = append(got, k)
			}
			sort.Strings(got)
			if !reflect.DeepEqual(got, want) {
				t.Errorf("fields:\n got %v\nwant %v", got, want)
			}
			fields := []struct {
				name      string
				got, want any
			}{
				{"type", e.Type(), ledger.TypeAmendment},
				{"seq", e.Seq(), head + 1},
				{"prev_hash", e.Str("prev_hash"), headHash},
				{"ts", e.Str("ts"), ledger.FormatTime(clockAt)},
				{"target_from", e.Int("target_from"), c.from},
				{"target_to", e.Int("target_to"), c.to},
				{"reason", e.Str("reason"), c.why},
				{"by", e.Str("by"), c.who},
				{"new_purpose", e.Str("new_purpose"), c.purpose},
				{"annotation", e.Str("annotation"), c.note},
			}
			for _, f := range fields {
				if f.got != f.want {
					t.Errorf("%s = %v, want %v", f.name, f.got, f.want)
				}
			}
			disk := readLedger(t, s)
			if last := disk[len(disk)-1]; last.Hash() != e.Hash() {
				t.Errorf("entry on disk differs from the returned one")
			}
			if r := verify(t, s, nil); !r.OK() {
				t.Fatalf("ledger does not verify: %v", r.Errors)
			}
		})
	}
}

func TestAmendEffectivePurpose(t *testing.T) {
	s := amendStore(t)
	effective := func() map[int64]string {
		return report.EffectivePurposes(readLedger(t, s))
	}
	if got := effective(); got[1] != "" || got[5] != "batch-job" {
		t.Fatalf("start: %v", got)
	}
	if _, err := ops.Amend(s, 1, 3, "eval-benchmark", "", "first", "me"); err != nil {
		t.Fatal(err)
	}
	if got := effective(); got[1] != "eval-benchmark" || got[3] != "eval-benchmark" || got[5] != "batch-job" {
		t.Fatalf("after the range: %v", got)
	}
	// The later amendment wins where both cover a call.
	if _, err := ops.Amend(s, 2, 5, "rag-query", "", "second", "me"); err != nil {
		t.Fatal(err)
	}
	got := effective()
	want := map[int64]string{1: "eval-benchmark", 2: "rag-query", 3: "rag-query", 5: "rag-query"}
	for seq, p := range want {
		if got[seq] != p {
			t.Errorf("seq %d: %q, want %q", seq, got[seq], p)
		}
	}
	// An annotation alone changes no purpose.
	if _, err := ops.Amend(s, 1, 5, "", "note", "third", "me"); err != nil {
		t.Fatal(err)
	}
	for seq, p := range want {
		if effective()[seq] != p {
			t.Errorf("seq %d changed by an annotation: %q", seq, effective()[seq])
		}
	}
}

func TestAmendNeverRewritesTargets(t *testing.T) {
	s := amendStore(t)
	before := readLedger(t, s)
	raw := ledgerBytes(t, s)
	if _, err := ops.Amend(s, 1, 5, "eval-regression", "n", "why", "me"); err != nil {
		t.Fatal(err)
	}
	after := readLedger(t, s)
	for i := range before {
		if !reflect.DeepEqual(before[i], after[i]) {
			t.Errorf("entry %d changed", i)
		}
	}
	if len(after) != len(before)+1 {
		t.Fatalf("%d entries, want %d", len(after), len(before)+1)
	}
	assertAppendOnly(t, raw, ledgerBytes(t, s), 1)
}
