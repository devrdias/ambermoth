// Package report aggregates ledger entries for packs, alerts and the CLI.
// Aggregation is deterministic so verify-pack can recompute and compare it.
package report

import (
	"sort"
	"time"

	"github.com/devrdias/ambermoth/internal/ledger"
)

// Filter selects call entries.
type Filter struct {
	From, To time.Time // inclusive; zero = unbounded
	Provider string    // empty = all
}

func (f Filter) Match(e ledger.Entry) bool {
	if e.Type() != ledger.TypeCall {
		return false
	}
	if f.Provider != "" && e.Str("provider") != f.Provider {
		return false
	}
	t := e.Time()
	if !f.From.IsZero() && t.Before(f.From) {
		return false
	}
	if !f.To.IsZero() && !t.Before(f.To) {
		return false
	}
	return true
}

// Exemplar is a sample entry reference (hashes only).
type Exemplar struct {
	Seq        int64  `json:"seq"`
	Ts         string `json:"ts"`
	Model      string `json:"model"`
	PromptHMAC string `json:"prompt_hmac"`
	OutputHMAC string `json:"output_hmac"`
}

// Bucket aggregates calls.
type Bucket struct {
	Calls      int64            `json:"calls"`
	TokensIn   int64            `json:"tokens_in"`
	TokensOut  int64            `json:"tokens_out"`
	CachedIn   int64            `json:"cached_in"`
	Errors     int64            `json:"errors"`
	Retagged   int64            `json:"retagged"`
	ByEndpoint map[string]int64 `json:"by_endpoint"`
	ByModel    map[string]int64 `json:"by_model"`
	ByDay      map[string]int64 `json:"by_day"`
	ByKey      map[string]int64 `json:"by_key"`
	First      string           `json:"first"`
	Last       string           `json:"last"`
	Exemplars  []Exemplar       `json:"exemplars"`
}

func newBucket() *Bucket {
	return &Bucket{ByEndpoint: map[string]int64{}, ByModel: map[string]int64{},
		ByDay: map[string]int64{}, ByKey: map[string]int64{}, Exemplars: []Exemplar{}}
}

func (b *Bucket) add(c *callRec, retagged bool) {
	b.Calls++
	b.TokensIn += c.tokensIn
	b.TokensOut += c.tokensOut
	b.CachedIn += c.cachedIn
	if c.status == 0 || c.status >= 400 {
		b.Errors++
	}
	if retagged {
		b.Retagged++
	}
	b.ByEndpoint[c.endpoint]++
	b.ByModel[c.model]++
	if c.day != "" {
		b.ByDay[c.day]++
	}
	b.ByKey[c.key]++
	if b.First == "" {
		b.First = c.ts
	}
	b.Last = c.ts
	if len(b.Exemplars) < 3 {
		b.Exemplars = append(b.Exemplars, Exemplar{c.seq, c.ts, c.model, c.promptHMAC, c.outputHMAC})
	}
}

// Totals is the machine-readable part of a pack manifest.
type Totals struct {
	All        *Bucket            `json:"all"`
	ByPurpose  map[string]*Bucket `json:"by_purpose"`
	ByProvider map[string]*Bucket `json:"by_provider"`
}

// KeyInfo describes a key fingerprint.
type KeyInfo struct {
	FP       string         `json:"key_fp"`
	Provider string         `json:"provider"`
	Calls    int64          `json:"calls"`
	Enrolled bool           `json:"enrolled"`
	Events   []ledger.Entry `json:"-"`
}

// Summary is everything a pack needs.
type Summary struct {
	Totals          Totals
	Amendments      []ledger.Entry
	KeyEvents       []ledger.Entry
	CodeLinks       []ledger.Entry
	Gaps            []ledger.Entry
	Reconciliations []ledger.Entry
	Keys            []*KeyInfo
	Original        map[int64]string // seq -> original purpose (for retagged calls)
	Effective       map[int64]string // seq -> effective purpose
}

// EffectivePurposes applies amendments in ledger order (SPEC §4.3).
func EffectivePurposes(entries []ledger.Entry) map[int64]string {
	eff := map[int64]string{}
	for _, e := range entries {
		if e == nil {
			continue
		}
		switch e.Type() {
		case ledger.TypeCall:
			eff[e.Seq()] = e.Str("purpose")
		case ledger.TypeAmendment:
			retag(eff, e)
		}
	}
	return eff
}

// retag applies an amendment's new purpose to the earlier calls it targets.
func retag(eff map[int64]string, am ledger.Entry) {
	np := am.Str("new_purpose")
	if np == "" {
		return
	}
	from, to := am.Int("target_from"), am.Int("target_to")
	// Walk the calls, not the range: a hostile pack can name 0..MaxInt64.
	for s := range eff {
		if s >= from && s <= to && s < am.Seq() {
			eff[s] = np
		}
	}
}

// Summarize aggregates entries under filter f. It feeds a Summarizer, so a
// caller that can read entries one at a time should use that and drop each
// entry after Add.
func Summarize(entries []ledger.Entry, f Filter) *Summary {
	z := NewSummarizer(f)
	for _, e := range entries {
		z.Add(e)
	}
	return z.Summary()
}

// callRec is what a matched call keeps until Summary: the fields a bucket
// reads, so a later amendment can still move the call to another purpose.
type callRec struct {
	seq                                     int64
	tokensIn, tokensOut, cachedIn, status   int64
	purpose, provider, key, endpoint, model string
	day, ts, promptHMAC, outputHMAC         string
}

// Summarizer builds a Summary from entries fed in ledger order. It keeps a
// record and a purpose for each call, and the amendments, key events, gaps,
// code links and reconciliations whole. It never keeps a call entry.
type Summarizer struct {
	f          Filter
	eff        map[int64]string // seq -> effective purpose so far, every call
	calls      []callRec        // calls f matched, in ledger order
	amendments []ledger.Entry
	keyEvents  []ledger.Entry
	codeLinks  []ledger.Entry
	gaps       []ledger.Entry
	recons     []ledger.Entry
	keys       map[string]*KeyInfo
	strs       map[string]string // interned call strings
}

// NewSummarizer returns an empty Summarizer for filter f.
func NewSummarizer(f Filter) *Summarizer {
	return &Summarizer{f: f, eff: map[int64]string{}, keys: map[string]*KeyInfo{}, strs: map[string]string{}}
}

// Add feeds the next entry. Entries must come in ledger order; nil is skipped.
func (z *Summarizer) Add(e ledger.Entry) {
	if e == nil {
		return
	}
	switch e.Type() {
	case ledger.TypeCall:
		z.addCall(e)
	case ledger.TypeAmendment:
		retag(z.eff, e)
		z.amendments = append(z.amendments, e)
	case ledger.TypeKeyEvent:
		z.addKeyEvent(e)
	case ledger.TypeCodeLink:
		z.codeLinks = append(z.codeLinks, e)
	case ledger.TypeGap:
		z.gaps = append(z.gaps, e)
	case ledger.TypeReconciliation:
		if z.providerMatches(e) {
			z.recons = append(z.recons, e)
		}
	}
}

func (z *Summarizer) providerMatches(e ledger.Entry) bool {
	return z.f.Provider == "" || e.Str("provider") == z.f.Provider
}

// in returns one shared copy of s, so records don't each pin their own.
func (z *Summarizer) in(s string) string {
	if c, ok := z.strs[s]; ok {
		return c
	}
	z.strs[s] = s
	return s
}

func (z *Summarizer) addCall(e ledger.Entry) {
	z.eff[e.Seq()] = e.Str("purpose")
	if !z.f.Match(e) {
		return
	}
	pr := z.in(e.Str("provider"))
	z.calls = append(z.calls, callRec{
		seq: e.Seq(), tokensIn: e.Int("tokens_in"), tokensOut: e.Int("tokens_out"), cachedIn: e.Int("cached_in"),
		status: e.Int("status"), purpose: z.in(e.Str("purpose")), provider: pr, key: z.in(e.Str("key_fp")),
		endpoint: z.in(e.Str("endpoint")), model: z.in(e.Str("model")), day: z.in(e.Day()),
		ts: e.Str("ts"), promptHMAC: e.Str("prompt_hmac"), outputHMAC: e.Str("output_hmac"),
	})
	if fp := e.Str("key_fp"); fp != "" {
		z.key(fp, pr).Calls++
	}
}

func (z *Summarizer) addKeyEvent(e ledger.Entry) {
	if !z.providerMatches(e) {
		return
	}
	z.keyEvents = append(z.keyEvents, e)
	k := z.key(e.Str("key_fp"), e.Str("provider"))
	k.Events = append(k.Events, e)
	k.Enrolled = e.Str("action") != "revoke"
}

func (z *Summarizer) key(fp, provider string) *KeyInfo {
	k, ok := z.keys[fp]
	if !ok {
		k = &KeyInfo{FP: fp, Provider: provider}
		z.keys[fp] = k
	}
	return k
}

// Summary returns the aggregate of everything added so far. It can be called
// at any point and again after more entries; each call builds a new value.
func (z *Summarizer) Summary() *Summary {
	s := &Summary{
		Totals:   Totals{All: newBucket(), ByPurpose: map[string]*Bucket{}, ByProvider: map[string]*Bucket{}},
		Original: map[int64]string{}, Effective: map[int64]string{},
		KeyEvents: z.keyEvents[:len(z.keyEvents):len(z.keyEvents)],
		CodeLinks: z.codeLinks[:len(z.codeLinks):len(z.codeLinks)],
		Gaps:      z.gaps[:len(z.gaps):len(z.gaps)], Reconciliations: z.recons[:len(z.recons):len(z.recons)],
	}
	for i := range z.calls {
		z.place(s, &z.calls[i])
	}
	s.Amendments = z.matchedAmendments()
	s.Keys = z.sortedKeys()
	return s
}

// place adds one matched call to the buckets under its final purpose.
func (z *Summarizer) place(s *Summary, c *callRec) {
	p := z.eff[c.seq]
	retagged := p != c.purpose
	if retagged {
		s.Original[c.seq] = c.purpose
	}
	s.Effective[c.seq] = p
	t := &s.Totals
	t.All.add(c, retagged)
	bucket(t.ByPurpose, p).add(c, retagged)
	bucket(t.ByProvider, c.provider).add(c, retagged)
}

// bucket returns m[name], creating it when missing.
func bucket(m map[string]*Bucket, name string) *Bucket {
	b, ok := m[name]
	if !ok {
		b = newBucket()
		m[name] = b
	}
	return b
}

// matchedAmendments keeps the amendments that target at least one matched
// call, whether the call came before or after the amendment.
func (z *Summarizer) matchedAmendments() []ledger.Entry {
	seqs := make([]int64, len(z.calls))
	for i := range z.calls {
		seqs[i] = z.calls[i].seq
	}
	sort.Slice(seqs, func(i, j int) bool { return seqs[i] < seqs[j] })
	var out []ledger.Entry
	for _, am := range z.amendments {
		from, to := am.Int("target_from"), am.Int("target_to")
		i := sort.Search(len(seqs), func(i int) bool { return seqs[i] >= from })
		if i < len(seqs) && seqs[i] <= to {
			out = append(out, am)
		}
	}
	return out
}

// sortedKeys returns copies of the keys with calls or events, busiest first
// and then by fingerprint, so the order (and the rendered summary) is
// deterministic.
func (z *Summarizer) sortedKeys() []*KeyInfo {
	var ks []*KeyInfo
	for _, k := range z.keys {
		if k.Calls > 0 || len(k.Events) > 0 {
			c := *k
			c.Events = k.Events[:len(k.Events):len(k.Events)]
			ks = append(ks, &c)
		}
	}
	sort.Slice(ks, func(i, j int) bool {
		if ks[i].Calls != ks[j].Calls {
			return ks[i].Calls > ks[j].Calls
		}
		return ks[i].FP < ks[j].FP
	})
	return ks
}

// SortedPurposes returns purposes ordered by call count, descending.
func (t Totals) SortedPurposes() []string {
	var ps []string
	for p := range t.ByPurpose {
		ps = append(ps, p)
	}
	sort.Slice(ps, func(i, j int) bool {
		a, b := t.ByPurpose[ps[i]].Calls, t.ByPurpose[ps[j]].Calls
		if a != b {
			return a > b
		}
		return ps[i] < ps[j]
	})
	return ps
}
