package ledger

import (
	"encoding/json"
	"sort"
	"strings"
)

// OrphanIntent is an intent entry with no call entry that completes it.
type OrphanIntent struct {
	Seq      int64  `json:"seq"`
	TS       string `json:"ts"`
	Endpoint string `json:"endpoint"`
}

// openIntents are the intents seen so far that no call has completed, by
// call_id. What is left at the end of a run are the orphans.
type openIntents map[string]OrphanIntent

// checkIntent records an intent entry as open.
func (v *verifier) checkIntent(e Entry) {
	id := e.Str("call_id")
	if id == "" {
		v.r.errf("seq %d: intent without a call_id", e.Seq())
		return
	}
	if o, dup := v.intents[id]; dup {
		v.r.errf("seq %d: call_id %s is already open at seq %d", e.Seq(), id, o.Seq)
		return
	}
	v.intents[strings.Clone(id)] = OrphanIntent{Seq: e.Seq(), TS: strings.Clone(e.Str("ts")), Endpoint: strings.Clone(e.Str("endpoint"))}
}

// checkCompletion checks a call that names an intent. A call with neither
// call_id nor intent_seq is from before write-ahead mode or from a proxy that
// has it off, and is not looked at.
func (v *verifier) checkCompletion(e Entry) {
	_, hasID := e["call_id"]
	_, hasSeq := e["intent_seq"]
	if !hasID && !hasSeq {
		return
	}
	seq := e.Seq()
	id, iseq, ok := intentRef(e)
	switch {
	case !ok:
		v.r.errf("seq %d: call needs both call_id and intent_seq", seq)
	case iseq < v.r.FirstSeq:
		// The intent lies before the start of a segment: it cannot be checked here.
	case iseq >= seq:
		v.r.errf("seq %d: intent_seq %d is not before the call", seq, iseq)
	case !v.isOpen(id, iseq):
		v.r.errf("seq %d: intent_seq %d is not an open intent with call_id %s (wrong seq or call_id, or already completed)", seq, iseq, id)
	default:
		delete(v.intents, id)
	}
}

// isOpen reports whether the intent at seq with this call_id has not been completed.
func (v *verifier) isOpen(id string, seq int64) bool {
	o, ok := v.intents[id]
	return ok && o.Seq == seq
}

// intentRef reads the call_id and intent_seq of a call; ok is false when
// either is missing or has the wrong type.
func intentRef(e Entry) (id string, seq int64, ok bool) {
	id = e.Str("call_id")
	switch n := e["intent_seq"].(type) {
	case json.Number:
		var err error
		seq, err = n.Int64()
		return id, seq, id != "" && err == nil
	case int, int64:
		return id, e.Int("intent_seq"), id != ""
	}
	return id, 0, false
}

// orphans lists what is left of the open set, in seq order.
func (o openIntents) orphans() []OrphanIntent {
	if len(o) == 0 {
		return nil
	}
	out := make([]OrphanIntent, 0, len(o))
	for _, x := range o {
		out = append(out, x)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Seq < out[j].Seq })
	return out
}
