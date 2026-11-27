package ledger

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/devrdias/ambermoth/internal/canon"
)

// AnchorCheck is what an AnchorVerifier learned from a valid token.
type AnchorCheck struct {
	GenTime time.Time // the TSA's time, from the signed token
	Signer  string    // signer certificate common name (organisation)
	Pinned  bool      // the signer chains to a trusted root
}

// AnchorVerifier checks an RFC 3161 token against the expected entry hash.
type AnchorVerifier func(tokenDER []byte, targetHash string) (AnchorCheck, error)

// MaxComfortableAnchorLag is the lag above which Verify warns (SPEC §8 step 6).
const MaxComfortableAnchorLag = 24 * time.Hour

// VerifyOptions configures Verify.
type VerifyOptions struct {
	AnchorDir     string                            // where token files live
	LoadToken     func(name string) ([]byte, error) // overrides AnchorDir (e.g. reading from a zip)
	CheckAnchor   AnchorVerifier                    // nil = skip cryptographic token checks (reported)
	TimeTolerance time.Duration                     // default 5s
}

// AnchorResult describes one anchor entry.
type AnchorResult struct {
	Seq       int64     `json:"seq"`
	TargetSeq int64     `json:"target_seq"`
	TSA       string    `json:"tsa"`
	GenTime   time.Time `json:"gen_time"`
	Signer    string    `json:"signer,omitempty"`
	Pinned    bool      `json:"pinned"`
	OK        bool      `json:"ok"`
	Error     string    `json:"error,omitempty"`
}

// Report is the outcome of Verify.
type Report struct {
	Entries         int            `json:"entries"`
	FirstSeq        int64          `json:"first_seq"`
	LastSeq         int64          `json:"last_seq"`
	HeadHash        string         `json:"head_hash"`
	StartsAtGenesis bool           `json:"starts_at_genesis"`
	Errors          []string       `json:"errors"`
	Warnings        []string       `json:"warnings"`
	Anchors         []AnchorResult `json:"anchors"`
	LastAnchoredSeq int64          `json:"last_anchored_seq"`
	UnanchoredTail  int            `json:"unanchored_tail"`
	Gaps            []Entry        `json:"-"`
	// MaxAnchorLagSeconds is the largest gap between an entry's own timestamp
	// and the earliest TSA time that covers it; -1 when nothing is anchored.
	MaxAnchorLagSeconds   int64 `json:"max_anchor_lag_seconds"`
	MaxAnchorLagSeq       int64 `json:"max_anchor_lag_seq"`
	EntriesLaggingOver24h int   `json:"entries_lagging_over_24h"`
	DroppedCalls          int64 `json:"dropped_calls"`
	AnchorChecksSkip      bool  `json:"anchor_checks_skipped"`
}

// OK reports whether verification passed.
func (r *Report) OK() bool { return len(r.Errors) == 0 }

func (r *Report) errf(format string, a ...any) {
	r.Errors = append(r.Errors, fmt.Sprintf(format, a...))
}
func (r *Report) warnf(format string, a ...any) {
	r.Warnings = append(r.Warnings, fmt.Sprintf(format, a...))
}

// Verify runs the SPEC §8 algorithm over parsed entries and raw lines.
func Verify(entries []Entry, lines [][]byte, opt VerifyOptions) *Report {
	if opt.TimeTolerance == 0 {
		opt.TimeTolerance = 5 * time.Second
	}
	r := &Report{Entries: len(entries), LastAnchoredSeq: -1, Errors: []string{}, Warnings: []string{}, Anchors: []AnchorResult{},
		MaxAnchorLagSeconds: -1, MaxAnchorLagSeq: -1}
	if len(entries) == 0 {
		r.errf("ledger is empty")
		return r
	}
	v := &verifier{opt: opt, r: r, bySeq: map[int64]Entry{}, prevSeq: -1}
	for i, e := range entries {
		v.checkEntry(i, e, lines[i])
	}
	r.AnchorChecksSkip = opt.CheckAnchor == nil
	for _, e := range entries {
		if e != nil && e.Type() == TypeAnchor {
			v.checkAnchor(e)
		}
	}
	for _, ar := range r.Anchors {
		v.warnClockAhead(ar)
	}
	r.computeAnchorLag(entries)
	r.UnanchoredTail = unanchoredTail(entries, r.LastAnchoredSeq)
	return r
}

// verifier holds the state of one Verify run.
type verifier struct {
	opt      VerifyOptions
	r        *Report
	bySeq    map[int64]Entry
	prevHash string
	prevSeq  int64 // -1 after an unreadable line
	prevTime time.Time
}

// checkEntry checks the form, chain link, hash and time of the entry on line i+1.
func (v *verifier) checkEntry(i int, e Entry, line []byte) {
	r := v.r
	if e == nil {
		r.errf("line %d: not valid JSON object", i+1)
		v.prevSeq = -1
		return
	}
	checkForm(r, i+1, e, line)
	seq := e.Seq()
	if i == 0 {
		r.FirstSeq = seq
		r.StartsAtGenesis = checkGenesis(r, e)
	} else {
		checkLink(r, e, v.prevSeq, v.prevHash)
	}
	if h, err := ComputeHash(e); err != nil || h != e.Hash() {
		r.errf("seq %d: entry_hash mismatch (entry altered after writing)", seq)
	}
	v.checkTime(e)
	if e.Type() == TypeGap {
		r.Gaps = append(r.Gaps, e)
		r.DroppedCalls += e.Int("dropped")
	}
	v.bySeq[seq] = e
	v.prevHash, v.prevSeq = e.Hash(), seq
	r.LastSeq, r.HeadHash = seq, e.Hash()
}

// checkTime requires a parseable timestamp and warns when time goes backwards.
func (v *verifier) checkTime(e Entry) {
	t := e.Time()
	if t.IsZero() {
		v.r.errf("seq %d: unparseable ts %q", e.Seq(), e.Str("ts"))
		return
	}
	if !v.prevTime.IsZero() && t.Add(v.opt.TimeTolerance).Before(v.prevTime) {
		v.r.warnf("seq %d: timestamp goes backwards (%s < %s)", e.Seq(), e.Str("ts"), FormatTime(v.prevTime))
	}
	v.prevTime = t
}

// checkAnchor checks one anchor entry against its target and token, and records the result.
func (v *verifier) checkAnchor(e Entry) {
	ar := AnchorResult{Seq: e.Seq(), TargetSeq: e.Int("target_seq"), TSA: e.Str("tsa")}
	if err := v.verifyAnchor(e, &ar); err != nil {
		ar.Error = err.Error()
		v.r.errf("anchor seq %d: %s", ar.Seq, ar.Error)
	}
	if ar.OK && ar.TargetSeq > v.r.LastAnchoredSeq {
		v.r.LastAnchoredSeq = ar.TargetSeq
	}
	v.r.Anchors = append(v.r.Anchors, ar)
}

func (v *verifier) verifyAnchor(e Entry, ar *AnchorResult) error {
	target, ok := v.bySeq[ar.TargetSeq]
	if !ok {
		return fmt.Errorf("target seq %d is outside the verified range", ar.TargetSeq)
	}
	if target.Hash() != e.Str("target_hash") {
		return errors.New("target_hash does not match the target entry")
	}
	tok, err := v.anchorToken(filepath.Base(e.Str("token_file")))
	if err != nil {
		return errors.New("token file missing: " + e.Str("token_file"))
	}
	if SHA256Bytes(tok) != e.Str("token_sha256") {
		return errors.New("token file hash mismatch")
	}
	if v.opt.CheckAnchor == nil {
		ar.OK = true
		return nil
	}
	chk, err := v.opt.CheckAnchor(tok, target.Hash())
	if err != nil {
		return err
	}
	ar.GenTime, ar.Signer, ar.Pinned, ar.OK = chk.GenTime, chk.Signer, chk.Pinned, true
	return nil
}

func (v *verifier) anchorToken(name string) ([]byte, error) {
	if v.opt.LoadToken != nil {
		return v.opt.LoadToken(name)
	}
	return os.ReadFile(filepath.Join(v.opt.AnchorDir, name))
}

// warnClockAhead warns about entries timestamped after the TSA time that anchors them.
func (v *verifier) warnClockAhead(ar AnchorResult) {
	if !ar.OK || ar.GenTime.IsZero() {
		return
	}
	limit := ar.GenTime.Add(v.opt.TimeTolerance)
	for s := ar.TargetSeq; s >= v.r.FirstSeq; s-- {
		e, ok := v.bySeq[s]
		// earlier entries are older still (checked above for monotonicity)
		if !ok || !e.Time().After(limit) {
			return
		}
		v.r.warnf("seq %d: ts %s is later than the TSA time %s that anchors it (writer clock ahead?)", s, e.Str("ts"), FormatTime(ar.GenTime))
	}
}

// unanchoredTail counts the entries after the last anchored seq, not
// counting the anchor entries themselves.
func unanchoredTail(entries []Entry, lastAnchored int64) int {
	n := 0
	for _, e := range entries {
		if e != nil && e.Seq() > lastAnchored && e.Type() != TypeAnchor {
			n++
		}
	}
	return n
}

// computeAnchorLag finds, for every entry, the earliest TSA time of any valid
// anchor at or after it, and records the largest gap between that time and the
// entry's own timestamp. A record anchored long after its entries only proves
// they existed from the anchor time on (SPEC §8 step 6).
func (r *Report) computeAnchorLag(entries []Entry) {
	type anc struct {
		target int64
		gen    time.Time
	}
	var as []anc
	for _, a := range r.Anchors {
		if a.OK && !a.GenTime.IsZero() {
			as = append(as, anc{a.TargetSeq, a.GenTime})
		}
	}
	if len(as) == 0 {
		return
	}
	sort.Slice(as, func(i, j int) bool { return as[i].target < as[j].target })
	// suffix minimum of TSA time: earliest proof for any entry at or before target
	for i := len(as) - 2; i >= 0; i-- {
		if as[i+1].gen.Before(as[i].gen) {
			as[i].gen = as[i+1].gen
		}
	}
	var worst time.Duration = -1
	k := 0
	for _, e := range entries {
		if e == nil || e.Type() == TypeAnchor {
			continue
		}
		for k < len(as) && as[k].target < e.Seq() {
			k++
		}
		if k == len(as) {
			break // the rest is unanchored
		}
		t := e.Time()
		if t.IsZero() {
			continue
		}
		lag := as[k].gen.Sub(t)
		if lag > MaxComfortableAnchorLag {
			r.EntriesLaggingOver24h++
		}
		if lag > worst {
			worst, r.MaxAnchorLagSeq = lag, e.Seq()
		}
	}
	if worst < 0 {
		worst = 0
	}
	r.MaxAnchorLagSeconds = int64(worst / time.Second)
	if r.EntriesLaggingOver24h > 0 {
		r.warnf("%d entries were first anchored more than 24h after their own timestamps (largest gap %s, seq %d): they are only proven to exist from their anchor time on",
			r.EntriesLaggingOver24h, worst.Round(time.Minute), r.MaxAnchorLagSeq)
	}
}

// FormatLag renders a lag in seconds for people.
func FormatLag(sec int64) string {
	if sec < 0 {
		return "n/a (nothing anchored)"
	}
	return (time.Duration(sec) * time.Second).String()
}

// requiredFields are present in every entry.
//
//rules:readonly lookup table, never written.
var requiredFields = []string{"v", "seq", "ts", "type", "prev_hash", "entry_hash"}

// checkForm checks that line ln holds e in canonical form with every required field.
func checkForm(r *Report, ln int, e Entry, line []byte) {
	c, err := canon.Marshal(map[string]any(e))
	switch {
	case err != nil:
		r.errf("line %d: %v", ln, err)
	case !bytes.Equal(c, bytes.TrimSpace(line)):
		r.errf("line %d (seq %d): not in canonical form", ln, e.Seq())
	}
	for _, k := range requiredFields {
		if _, ok := e[k]; !ok {
			r.errf("line %d: missing field %q", ln, k)
		}
	}
}

// checkGenesis reports whether the first entry is a genesis entry, and
// records an error when it is one but malformed.
func checkGenesis(r *Report, e Entry) bool {
	if e.Type() != TypeGenesis {
		return false
	}
	if e.Seq() != 0 || e.Str("prev_hash") != ZeroHash || e.Str("ledger_id") == "" {
		r.errf("seq %d: malformed genesis entry", e.Seq())
	}
	return true
}

// checkLink checks that e directly follows the entry with prevSeq and prevHash.
// prevSeq is -1 when the previous line was unreadable.
func checkLink(r *Report, e Entry, prevSeq int64, prevHash string) {
	seq := e.Seq()
	if prevSeq >= 0 && seq != prevSeq+1 {
		r.errf("seq %d follows seq %d: sequence is not contiguous (entry removed or inserted)", seq, prevSeq)
	}
	if e.Str("prev_hash") != prevHash {
		r.errf("seq %d: prev_hash does not match previous entry_hash (chain broken)", seq)
	}
}
