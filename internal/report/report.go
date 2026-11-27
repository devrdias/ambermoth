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

func (b *Bucket) add(e ledger.Entry, retagged bool) {
	b.Calls++
	b.TokensIn += e.Int("tokens_in")
	b.TokensOut += e.Int("tokens_out")
	b.CachedIn += e.Int("cached_in")
	if s := e.Int("status"); s == 0 || s >= 400 {
		b.Errors++
	}
	if retagged {
		b.Retagged++
	}
	b.ByEndpoint[e.Str("endpoint")]++
	b.ByModel[e.Str("model")]++
	if d := e.Day(); d != "" {
		b.ByDay[d]++
	}
	b.ByKey[e.Str("key_fp")]++
	if b.First == "" {
		b.First = e.Str("ts")
	}
	b.Last = e.Str("ts")
	if len(b.Exemplars) < 3 {
		b.Exemplars = append(b.Exemplars, Exemplar{e.Seq(), e.Str("ts"), e.Str("model"), e.Str("prompt_hmac"), e.Str("output_hmac")})
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
	for s := am.Int("target_from"); s <= am.Int("target_to") && s < am.Seq(); s++ {
		if _, ok := eff[s]; ok {
			eff[s] = np
		}
	}
}

// Summarize aggregates entries under filter f.
func Summarize(entries []ledger.Entry, f Filter) *Summary {
	sz := summarizer{
		f:   f,
		eff: EffectivePurposes(entries),
		s: &Summary{
			Totals:   Totals{All: newBucket(), ByPurpose: map[string]*Bucket{}, ByProvider: map[string]*Bucket{}},
			Original: map[int64]string{}, Effective: map[int64]string{},
		},
		keys:    map[string]*KeyInfo{},
		inRange: map[int64]bool{},
	}
	for _, e := range entries {
		if e != nil {
			sz.add(e)
		}
	}
	sz.collectAmendments(entries)
	sz.collectKeys()
	return sz.s
}

// summarizer accumulates one Summary.
type summarizer struct {
	f       Filter
	eff     map[int64]string // seq -> effective purpose
	s       *Summary
	keys    map[string]*KeyInfo
	inRange map[int64]bool // seqs of the calls f matched
}

func (z *summarizer) add(e ledger.Entry) {
	switch e.Type() {
	case ledger.TypeCall:
		z.addCall(e)
	case ledger.TypeKeyEvent:
		z.addKeyEvent(e)
	case ledger.TypeCodeLink:
		z.s.CodeLinks = append(z.s.CodeLinks, e)
	case ledger.TypeGap:
		z.s.Gaps = append(z.s.Gaps, e)
	case ledger.TypeReconciliation:
		if z.providerMatches(e) {
			z.s.Reconciliations = append(z.s.Reconciliations, e)
		}
	}
}

func (z *summarizer) providerMatches(e ledger.Entry) bool {
	return z.f.Provider == "" || e.Str("provider") == z.f.Provider
}

func (z *summarizer) addCall(e ledger.Entry) {
	if !z.f.Match(e) {
		return
	}
	p := z.eff[e.Seq()]
	retagged := p != e.Str("purpose")
	if retagged {
		z.s.Original[e.Seq()] = e.Str("purpose")
	}
	z.s.Effective[e.Seq()] = p
	z.inRange[e.Seq()] = true
	t := &z.s.Totals
	t.All.add(e, retagged)
	bucket(t.ByPurpose, p).add(e, retagged)
	pr := e.Str("provider")
	bucket(t.ByProvider, pr).add(e, retagged)
	if fp := e.Str("key_fp"); fp != "" {
		z.key(fp, pr).Calls++
	}
}

func (z *summarizer) addKeyEvent(e ledger.Entry) {
	if !z.providerMatches(e) {
		return
	}
	z.s.KeyEvents = append(z.s.KeyEvents, e)
	k := z.key(e.Str("key_fp"), e.Str("provider"))
	k.Events = append(k.Events, e)
	k.Enrolled = e.Str("action") != "revoke"
}

func (z *summarizer) key(fp, provider string) *KeyInfo {
	k, ok := z.keys[fp]
	if !ok {
		k = &KeyInfo{FP: fp, Provider: provider}
		z.keys[fp] = k
	}
	return k
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

// collectAmendments keeps the amendments that target at least one matched call.
func (z *summarizer) collectAmendments(entries []ledger.Entry) {
	for _, e := range entries {
		if e != nil && e.Type() == ledger.TypeAmendment && z.targetsMatchedCall(e) {
			z.s.Amendments = append(z.s.Amendments, e)
		}
	}
}

func (z *summarizer) targetsMatchedCall(am ledger.Entry) bool {
	for q := am.Int("target_from"); q <= am.Int("target_to"); q++ {
		if z.inRange[q] {
			return true
		}
	}
	return false
}

// collectKeys keeps the keys with calls or events, busiest first and then by
// fingerprint, so the order (and the rendered summary) is deterministic.
func (z *summarizer) collectKeys() {
	for _, k := range z.keys {
		if k.Calls > 0 || len(k.Events) > 0 {
			z.s.Keys = append(z.s.Keys, k)
		}
	}
	ks := z.s.Keys
	sort.Slice(ks, func(i, j int) bool {
		if ks[i].Calls != ks[j].Calls {
			return ks[i].Calls > ks[j].Calls
		}
		return ks[i].FP < ks[j].FP
	})
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
