package pack

import (
	"github.com/devrdias/ambermoth/internal/ledger"
	"github.com/devrdias/ambermoth/internal/report"
)

// kind is what pack selection needs to know of an entry's type.
type kind uint8

const (
	kindOther     kind = iota
	kindCall           // a call
	kindAmendment      // an amendment, also listed in index.amends
	kindContext        // a key event, code link or reconciliation
)

// slot is what the index keeps of one entry.
type slot struct {
	off   int64 // where its line starts in the ledger file
	kind  kind
	match bool // a call the filter matches
}

// amendment is the part of an amendment entry that selection reads.
type amendment struct{ seq, from, to int64 }

// index is what choosing and cutting a segment needs of a ledger: a slot per
// entry, and the amendments. Line index equals seq because the ledger
// verified from genesis.
type index struct {
	slots  []slot // one per entry, then one at the end of the data read
	amends []amendment
}

// add records the next entry, which starts at off. Entries come in ledger
// order. f decides which calls match.
func (ix *index) add(e ledger.Entry, off int64, f report.Filter) {
	s := slot{off: off, kind: kindOf(e), match: f.Match(e)}
	if s.kind == kindAmendment {
		ix.amends = append(ix.amends, amendment{int64(len(ix.slots)), e.Int("target_from"), e.Int("target_to")})
	}
	ix.slots = append(ix.slots, s)
}

// end closes the index at the end of the data read.
func (ix *index) end(size int64) { ix.slots = append(ix.slots, slot{off: size}) }

func kindOf(e ledger.Entry) kind {
	switch e.Type() {
	case ledger.TypeCall:
		return kindCall
	case ledger.TypeAmendment:
		return kindAmendment
	case ledger.TypeKeyEvent, ledger.TypeCodeLink, ledger.TypeReconciliation:
		return kindContext
	}
	return kindOther
}

// entries is the number of entries in the ledger.
func (ix *index) entries() int { return len(ix.slots) - 1 }

// bounds is the byte range of the entries first..last, blank lines after the
// last one included.
func (ix *index) bounds(first, last int64) (from, to int64) {
	return ix.slots[first].off, ix.slots[last+1].off
}

// callRange returns the first and last seq of the calls the filter matched.
func (ix *index) callRange() (first, last int64, ok bool) {
	first, last = -1, -1
	for i, s := range ix.slots[:ix.entries()] {
		if !s.match {
			continue
		}
		if first < 0 {
			first = int64(i)
		}
		last = int64(i)
	}
	return first, last, first >= 0
}

// lastRelevantSeq extends the period's last call to the last amendment that
// re-tags a matched call in first..last, so the segment includes it.
func (ix *index) lastRelevantSeq(first, last int64) int64 {
	relevant := last
	for _, am := range ix.amends {
		if am.seq >= last && ix.amendsMatchedCall(am, first, last) {
			relevant = am.seq
		}
	}
	return relevant
}

func (ix *index) amendsMatchedCall(am amendment, first, last int64) bool {
	for q := max(am.from, first); q <= min(am.to, last); q++ {
		if ix.slots[q].match {
			return true
		}
	}
	return false
}
