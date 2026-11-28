package ledger_test

import (
	"strings"
	"testing"

	"github.com/devrdias/ambermoth/internal/ledger"
)

// intentCase is a ledger with intent entries, with what verify must say about
// it: the seqs of the orphans, and a part of the error text when it must fail.
type intentCase struct {
	name    string
	build   func(t *testing.T) corpusInput
	orphans []int64
	errPart string
}

func intentEntry(id string) ledger.Entry {
	return ledger.Entry{
		"type": ledger.TypeIntent, "call_id": id, "provider": "openai", "api_style": "openai",
		"endpoint": "chat.completions", "method": "POST", "path": "/v1/chat/completions",
		"key_fp": "sha256:" + strings.Repeat("0", 64), "purpose": "support",
		"salt_id": strings.Repeat("5", 32), "declared_bytes_in": int64(120),
	}
}

// completion is a call that closes the intent at seq with the given call id.
func completion(i int, id string, seq int64) ledger.Entry {
	e := callEntry(i)
	e["call_id"], e["intent_seq"] = id, seq
	return e
}

func hexID(c string) string { return strings.Repeat(c, 32) }

// withIntents builds the basic ledger (seq 1 to 7) and lets fn add what follows, from seq 8.
func withIntents(fn func(b *corpusBuilder)) func(t *testing.T) corpusInput { return anchoredWith(fn) }

// segmentFrom cuts the ledger that build makes to its lines from the line at index from.
func segmentFrom(build func(t *testing.T) corpusInput, from int) func(t *testing.T) corpusInput {
	return func(t *testing.T) corpusInput {
		in := build(t)
		in.data = joinLines(fromLine(from)(t, splitLines(in.data)))
		return in
	}
}

func intentCases() []intentCase {
	a, b, c := hexID("a"), hexID("b"), hexID("c")
	pair := func(bd *corpusBuilder) {
		bd.put(minutes(8), intentEntry(a))
		bd.put(minutes(9), completion(9, a, 8))
	}
	three := func(bd *corpusBuilder) {
		bd.put(minutes(8), intentEntry(a))
		bd.put(minutes(9), intentEntry(b))
		bd.put(minutes(10), completion(10, a, 8))
		bd.put(minutes(11), intentEntry(c))
		bd.put(minutes(12), completion(12, c, 11))
	}
	return []intentCase{
		{name: "intent-completed", build: withIntents(pair)},
		{name: "intent-orphan", build: withIntents(func(bd *corpusBuilder) { bd.put(minutes(8), intentEntry(a)) }), orphans: []int64{8}},
		{name: "intent-completed-out-of-order", build: withIntents(three), orphans: []int64{9}},
		{name: "intent-completion-after-gap", build: withIntents(func(bd *corpusBuilder) {
			bd.put(minutes(8), intentEntry(a))
			bd.put(minutes(9), gapEntry(1))
			bd.put(minutes(10), intentEntry(b))
			bd.put(minutes(11), completion(11, b, 10))
		}), orphans: []int64{8}},
		{name: "intent-call-points-at-a-call", build: withIntents(func(bd *corpusBuilder) {
			bd.put(minutes(8), intentEntry(a))
			bd.put(minutes(9), completion(9, a, 7))
		}), orphans: []int64{8}, errPart: "seq 9: intent_seq 7 is not an open intent with call_id " + a},
		{name: "intent-call-with-another-call-id", build: withIntents(func(bd *corpusBuilder) {
			bd.put(minutes(8), intentEntry(a))
			bd.put(minutes(9), completion(9, b, 8))
		}), orphans: []int64{8}, errPart: "seq 9: intent_seq 8 is not an open intent with call_id " + b},
		{name: "intent-completed-twice", build: withIntents(func(bd *corpusBuilder) {
			pair(bd)
			bd.put(minutes(10), completion(10, a, 8))
		}), errPart: "seq 10: intent_seq 8 is not an open intent"},
		{name: "intent-call-points-forward", build: withIntents(func(bd *corpusBuilder) {
			bd.put(minutes(8), completion(8, a, 9))
			bd.put(minutes(9), intentEntry(a))
		}), orphans: []int64{9}, errPart: "seq 8: intent_seq 9 is not before the call"},
		{name: "intent-call-without-intent-seq", build: withIntents(func(bd *corpusBuilder) {
			bd.put(minutes(8), intentEntry(a))
			e := completion(9, a, 8)
			delete(e, "intent_seq")
			bd.put(minutes(9), e)
		}), orphans: []int64{8}, errPart: "seq 9: call needs both call_id and intent_seq"},
		{name: "intent-call-without-call-id", build: withIntents(func(bd *corpusBuilder) {
			bd.put(minutes(8), intentEntry(a))
			e := completion(9, a, 8)
			delete(e, "call_id")
			bd.put(minutes(9), e)
		}), orphans: []int64{8}, errPart: "seq 9: call needs both call_id and intent_seq"},
		{name: "intent-without-call-id", build: withIntents(func(bd *corpusBuilder) {
			e := intentEntry(a)
			delete(e, "call_id")
			bd.put(minutes(8), e)
		}), errPart: "seq 8: intent without a call_id"},
		{name: "intent-call-id-repeated", build: withIntents(func(bd *corpusBuilder) {
			bd.put(minutes(8), intentEntry(a))
			bd.put(minutes(9), intentEntry(a))
		}), orphans: []int64{8}, errPart: "seq 9: call_id " + a + " is already open at seq 8"},
	}
}

// intentSegmentCases are segments whose first line comes after an intent that a call in it closes.
func intentSegmentCases() []intentCase {
	a, b := hexID("a"), hexID("b")
	ledgerWith := func(bd *corpusBuilder) {
		bd.put(minutes(8), intentEntry(a))
		bd.put(minutes(9), intentEntry(b))
		bd.put(minutes(10), completion(10, a, 8))
		bd.put(minutes(11), completion(11, b, 9))
		bd.put(minutes(12), intentEntry(hexID("c")))
	}
	return []intentCase{
		// The segment starts at seq 9: the call at seq 10 closes an intent before it.
		{name: "intent-segment-completes-an-earlier-intent", build: segmentFrom(withIntents(ledgerWith), 9), orphans: []int64{12}},
		{name: "intent-segment-starts-at-the-completion", build: segmentFrom(withIntents(ledgerWith), 10), orphans: []int64{12}},
	}
}

func allIntentCases() []intentCase { return append(intentCases(), intentSegmentCases()...) }

func intentCorpus() []corpusCase {
	var out []corpusCase
	for _, c := range allIntentCases() {
		out = append(out, corpusCase{c.name, c.build})
	}
	return out
}
