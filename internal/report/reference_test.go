package report_test

import (
	"sort"

	"github.com/devrdias/ambermoth/internal/ledger"
	"github.com/devrdias/ambermoth/internal/report"
)

// refSummarize is Summarize as it was before the accumulator: it needs the
// whole slice. It stays here so the accumulator is tested against it.
func refSummarize(entries []ledger.Entry, f report.Filter) *report.Summary {
	sz := refSummarizer{
		f:   f,
		eff: report.EffectivePurposes(entries),
		s: &report.Summary{
			Totals:   report.Totals{All: refBucket(), ByPurpose: map[string]*report.Bucket{}, ByProvider: map[string]*report.Bucket{}},
			Original: map[int64]string{}, Effective: map[int64]string{},
		},
		keys:    map[string]*report.KeyInfo{},
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

func refBucket() *report.Bucket {
	return &report.Bucket{ByEndpoint: map[string]int64{}, ByModel: map[string]int64{},
		ByDay: map[string]int64{}, ByKey: map[string]int64{}, Exemplars: []report.Exemplar{}}
}

func refBucketAdd(b *report.Bucket, e ledger.Entry, retagged bool) {
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
		b.Exemplars = append(b.Exemplars, report.Exemplar{Seq: e.Seq(), Ts: e.Str("ts"), Model: e.Str("model"),
			PromptHMAC: e.Str("prompt_hmac"), OutputHMAC: e.Str("output_hmac")})
	}
}

type refSummarizer struct {
	f       report.Filter
	eff     map[int64]string
	s       *report.Summary
	keys    map[string]*report.KeyInfo
	inRange map[int64]bool
}

func (z *refSummarizer) add(e ledger.Entry) {
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

func (z *refSummarizer) providerMatches(e ledger.Entry) bool {
	return z.f.Provider == "" || e.Str("provider") == z.f.Provider
}

func (z *refSummarizer) addCall(e ledger.Entry) {
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
	refBucketAdd(t.All, e, retagged)
	refBucketAdd(refBucketFor(t.ByPurpose, p), e, retagged)
	pr := e.Str("provider")
	refBucketAdd(refBucketFor(t.ByProvider, pr), e, retagged)
	if fp := e.Str("key_fp"); fp != "" {
		z.key(fp, pr).Calls++
	}
}

func (z *refSummarizer) addKeyEvent(e ledger.Entry) {
	if !z.providerMatches(e) {
		return
	}
	z.s.KeyEvents = append(z.s.KeyEvents, e)
	k := z.key(e.Str("key_fp"), e.Str("provider"))
	k.Events = append(k.Events, e)
	k.Enrolled = e.Str("action") != "revoke"
}

func (z *refSummarizer) key(fp, provider string) *report.KeyInfo {
	k, ok := z.keys[fp]
	if !ok {
		k = &report.KeyInfo{FP: fp, Provider: provider}
		z.keys[fp] = k
	}
	return k
}

func refBucketFor(m map[string]*report.Bucket, name string) *report.Bucket {
	b, ok := m[name]
	if !ok {
		b = refBucket()
		m[name] = b
	}
	return b
}

func (z *refSummarizer) collectAmendments(entries []ledger.Entry) {
	for _, e := range entries {
		if e != nil && e.Type() == ledger.TypeAmendment && z.targetsMatchedCall(e) {
			z.s.Amendments = append(z.s.Amendments, e)
		}
	}
}

func (z *refSummarizer) targetsMatchedCall(am ledger.Entry) bool {
	from, to := am.Int("target_from"), am.Int("target_to")
	for q := range z.inRange {
		if q >= from && q <= to {
			return true
		}
	}
	return false
}

func (z *refSummarizer) collectKeys() {
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
