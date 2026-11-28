package pack_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/devrdias/ambermoth/internal/ledger"
	"github.com/devrdias/ambermoth/internal/pack"
)

// intent appends the intent of a call the proxy began on the given day.
func (e *env) intent(day int, provider, endpoint string) ledger.Entry {
	e.on(day)
	return e.append(ledger.Entry{"type": ledger.TypeIntent, "call_id": ledger.NewCallID(), "provider": provider,
		"api_style": provider, "endpoint": endpoint, "method": "POST", "path": "/v1/" + endpoint,
		"key_fp": ledger.KeyFingerprint("sk-1"), "purpose": "prod", "salt_id": ledger.NewSaltID()})
}

// complete appends the call that completes in.
func (e *env) complete(day int, in ledger.Entry, purpose string) ledger.Entry {
	e.on(day)
	salt := in.Str("salt_id")
	return e.append(ledger.Entry{"type": ledger.TypeCall, "provider": in.Str("provider"), "api_style": in.Str("api_style"),
		"endpoint": in.Str("endpoint"), "model": "m", "status": 200, "tokens_in": 100, "tokens_out": 20, "cached_in": 0,
		"key_fp": in.Str("key_fp"), "purpose": purpose, "salt_id": salt, "prompt_hmac": "sha256:" + strings.Repeat("a", 64),
		"output_hmac": "sha256:" + strings.Repeat("b", 64), "call_id": in.Str("call_id"), "intent_seq": in.Seq()})
}

// orphanSentence is what the summary and the letter say, from ADR-0004.
func orphanSentence(n int) string {
	return fmt.Sprintf("%d intents have no call entry in this segment. Each is a request the proxy began to send. Whether it was sent, or answered, is not known.", n)
}

type formatCase struct {
	name    string
	fill    func(e *env)
	opts    func(o pack.Options) pack.Options
	format  string
	orphans []int64 // seqs, in order
	calls   int64
	context int
}

func wholeLedger(o pack.Options) pack.Options { return o }

func formatCases() []formatCase {
	return []formatCase{
		{name: "no intent stays pack/1", fill: func(e *env) {
			e.keyEvent(0)
			e.call(1, "anthropic", "prod")
			e.call(2, "anthropic", "prod")
		}, opts: wholeLedger, format: pack.Format, calls: 2},
		{name: "intents that are all completed make pack/2 with no orphans", fill: func(e *env) {
			e.keyEvent(0)
			a := e.intent(1, "anthropic", "messages")
			e.complete(1, a, "prod")
			b := e.intent(2, "anthropic", "messages")
			e.complete(2, b, "prod")
		}, opts: wholeLedger, format: pack.FormatV2, calls: 2},
		{name: "an orphan between calls", fill: func(e *env) {
			a := e.intent(1, "anthropic", "messages")
			e.complete(1, a, "prod")
			e.intent(2, "anthropic", "messages")
			e.call(3, "anthropic", "prod")
		}, opts: wholeLedger, format: pack.FormatV2, orphans: []int64{3}, calls: 2},
		{name: "an orphan at the end of the segment", fill: func(e *env) {
			e.call(1, "anthropic", "prod")
			e.intent(2, "anthropic", "messages")
		}, opts: wholeLedger, format: pack.FormatV2, orphans: []int64{2}, calls: 1},
		{name: "a call completed just after the segment is an orphan in it", fill: func(e *env) {
			e.keyEvent(0)
			e.call(1, "anthropic", "prod")
			a := e.intent(1, "anthropic", "messages")
			e.anchor(2, 1)
			e.complete(3, a, "prod")
		}, opts: func(o pack.Options) pack.Options { return period(o, 1, 1) }, format: pack.FormatV2, orphans: []int64{3}, calls: 1},
		{name: "a call completing an intent from before the segment keeps pack/1", fill: func(e *env) {
			e.keyEvent(0)
			x := e.intent(1, "anthropic", "messages")
			e.call(1, "anthropic", "prod")
			e.anchor(2, 1)
			e.complete(3, x, "prod")
			e.call(3, "anthropic", "prod")
		}, opts: func(o pack.Options) pack.Options { return period(o, 3, 3) }, format: pack.Format, calls: 2, context: 1},
		{name: "orphans are the segment's, not the period's or the provider's", fill: func(e *env) {
			e.intent(1, "openai", "chat.completions")
			e.call(2, "anthropic", "prod")
			e.call(3, "anthropic", "prod")
			e.call(4, "anthropic", "prod")
			e.intent(5, "anthropic", "messages")
		}, opts: func(o pack.Options) pack.Options { o.Provider = "anthropic"; return period(o, 3, 3) },
			format: pack.FormatV2, orphans: []int64{1, 5}, calls: 1},
		{name: "context entries and a disclosure", fill: func(e *env) {
			e.keyEvent(0)
			e.call(1, "anthropic", "prod")
			e.anchor(2, 2)
			e.intent(3, "anthropic", "messages")
			e.call(4, "anthropic", "prod")
			e.call(4, "anthropic", "prod")
			e.anchor(5, 1)
		}, opts: func(o pack.Options) pack.Options { o.Disclose = []int64{6}; return period(o, 4, 4) },
			format: pack.FormatV2, orphans: []int64{5}, calls: 2, context: 1},
	}
}

func TestPackFormatFollowsIntentsInTheSegment(t *testing.T) {
	for _, c := range formatCases() {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t)
			c.fill(e)
			o := c.opts(e.opts())
			m := e.build(o)

			if m.Format != c.format {
				t.Errorf("format = %q, want %q", m.Format, c.format)
			}
			var got []int64
			for _, x := range m.Orphans {
				got = append(got, x.Seq)
			}
			if fmt.Sprint(got) != fmt.Sprint(c.orphans) || m.OrphanIntents != len(c.orphans) {
				t.Errorf("orphans %v (count %d), want %v", got, m.OrphanIntents, c.orphans)
			}
			if m.Totals.All.Calls != c.calls || m.ContextEntries != c.context {
				t.Errorf("calls %d, context %d; want %d, %d: totals come from call entries only", m.Totals.All.Calls, m.ContextEntries, c.calls, c.context)
			}
			files := readPack(t, o.Out)
			if has := strings.Contains(string(files["manifest.json"]), "orphan"); has != (len(c.orphans) > 0) {
				t.Errorf("manifest mentions orphans: %v, want %v", has, len(c.orphans) > 0)
			}
			r := e.verify(o.Out)
			if !r.OK() || !r.SummaryMatches {
				t.Errorf("honest pack: errors %v, ledger %v, summary matches %v", r.Errors, r.Ledger.Errors, r.SummaryMatches)
			}
			if r.Manifest.Format != c.format {
				t.Errorf("verified format = %q", r.Manifest.Format)
			}
		})
	}
}

func TestSummaryAndLetterSayWhatIsNotKnown(t *testing.T) {
	e := newEnv(t)
	e.keyEvent(0)
	a := e.intent(1, "anthropic", "messages")
	e.complete(1, a, "prod")
	e.intent(2, "anthropic", "messages")
	e.intent(2, "anthropic", "batches")
	e.call(3, "anthropic", "prod")
	o := e.opts()
	m := e.build(o)
	files := readPack(t, o.Out)

	sum, letter := string(files["summary.html"]), string(files["letter-template.md"])
	want := orphanSentence(2)
	if !strings.Contains(sum, want) || !strings.Contains(letter, want) {
		t.Errorf("summary has the sentence: %v, letter: %v", strings.Contains(sum, want), strings.Contains(letter, want))
	}
	for _, o := range m.Orphans {
		parts := []string{fmt.Sprintf("<td>%d</td>", o.Seq), o.TS, o.Endpoint}
		for _, part := range parts {
			if !strings.Contains(sum, part) {
				t.Errorf("summary lacks %q of orphan %+v", part, o)
			}
		}
	}
	if !strings.Contains(sum, "just after the segment") || !strings.Contains(letter, "just after the segment") {
		t.Error("the near-the-end note is missing")
	}
	if m.Totals.All.Calls != 2 {
		t.Errorf("calls = %d, want 2: the intents are not calls", m.Totals.All.Calls)
	}
	if strings.Count(letter, "**Keys.**") != 1 || strings.Index(letter, want) > strings.Index(letter, "**Keys.**") {
		t.Errorf("the orphan paragraph sits before the Keys paragraph:\n%s", letter)
	}
}

func TestOneOrphanIsSingular(t *testing.T) {
	e := newEnv(t)
	e.intent(1, "anthropic", "messages")
	e.call(2, "anthropic", "prod")
	o := e.opts()
	e.build(o)
	files := readPack(t, o.Out)
	want := "1 intent has no call entry in this segment. It is a request the proxy began to send. Whether it was sent, or answered, is not known."
	if !strings.Contains(string(files["summary.html"]), want) || !strings.Contains(string(files["letter-template.md"]), want) {
		t.Errorf("no singular sentence")
	}
}

func TestPackWithoutIntentsHasNoOrphanText(t *testing.T) {
	e := newEnv(t)
	e.call(1, "anthropic", "prod")
	o := e.opts()
	e.build(o)
	for name, b := range readPack(t, o.Out) {
		if strings.Contains(string(b), "intent") && name != "ledger/segment.jsonl" {
			t.Errorf("%s mentions intents", name)
		}
	}
}

func TestOrphanListIsCappedAndSaysSo(t *testing.T) {
	e := newEnv(t)
	e.call(1, "anthropic", "prod")
	for i := 0; i < 60; i++ {
		e.intent(2, "anthropic", "messages")
	}
	o := e.opts()
	m := e.build(o)
	if m.OrphanIntents != 60 || len(m.Orphans) != pack.MaxOrphansListed || m.Orphans[0].Seq != 2 || m.Orphans[49].Seq != 51 {
		t.Fatalf("count %d, listed %d", m.OrphanIntents, len(m.Orphans))
	}
	sum := string(readPack(t, o.Out)["summary.html"])
	if !strings.Contains(sum, "first 50") || !strings.Contains(sum, "10 more") || !strings.Contains(sum, orphanSentence(60)) {
		t.Errorf("summary does not state the cap:\n%s", sum)
	}
	if strings.Contains(sum, "<td>52</td>") {
		t.Error("the 51st orphan is listed")
	}
	if r := e.verify(o.Out); !r.OK() {
		t.Errorf("errors: %v", r.Errors)
	}
}

// draftOf builds a pack from fill and returns it taken apart for editing.
func draftOf(t *testing.T, fill func(e *env)) (*env, *corpusDraft) {
	t.Helper()
	e := newEnv(t)
	fill(e)
	o := e.opts()
	e.build(o)
	return e, newCorpusDraft(t, o.Out)
}

func fillTwoOrphans(e *env) {
	e.keyEvent(0)
	e.call(1, "anthropic", "prod")
	e.intent(2, "anthropic", "messages")
	e.intent(2, "anthropic", "batches")
	e.call(3, "anthropic", "prod")
}

func fillCompleted(e *env) {
	a := e.intent(1, "anthropic", "messages")
	e.complete(1, a, "prod")
}

func fillNoIntents(e *env) {
	e.keyEvent(0)
	e.call(1, "anthropic", "prod")
	e.call(2, "anthropic", "prod")
}

type intentTamper struct {
	name   string
	fill   func(e *env)
	edit   func(d *corpusDraft)
	want   string // part of an error
	notErr string // part that must not be in any error
}

func intentTampers() []intentTamper {
	return []intentTamper{
		{name: "orphan count lowered", fill: fillTwoOrphans, edit: func(d *corpusDraft) { d.m.OrphanIntents = 1 },
			want: "orphan intents in manifest do not match"},
		{name: "orphan claim removed", fill: fillTwoOrphans, edit: func(d *corpusDraft) { d.m.OrphanIntents, d.m.Orphans = 0, nil },
			want: "orphan intents in manifest do not match"},
		{name: "orphan list changed", fill: fillTwoOrphans, edit: func(d *corpusDraft) { d.m.Orphans[0].Seq++ },
			want: "orphan intents in manifest do not match"},
		{name: "orphan endpoint changed", fill: fillTwoOrphans, edit: func(d *corpusDraft) { d.m.Orphans[1].Endpoint = "chat" },
			want: "orphan intents in manifest do not match"},
		{name: "orphan list cut short", fill: fillTwoOrphans, edit: func(d *corpusDraft) { d.m.Orphans = d.m.Orphans[:1] },
			want: "orphan intents in manifest do not match"},
		{name: "orphan claimed in a pack with none", fill: fillCompleted,
			edit: func(d *corpusDraft) { d.m.OrphanIntents = 1 }, want: "orphan intents in manifest do not match"},
		{name: "summary.html without the orphan line", fill: fillTwoOrphans,
			edit: func(d *corpusDraft) { d.alter("summary.html", "Whether it was sent, or answered, is not known.", "") },
			want: "summary.html does not match a re-render"},
		{name: "pack/2 with no intent in the segment", fill: fillNoIntents, edit: func(d *corpusDraft) { d.m.Format = pack.FormatV2 },
			want: "ambermoth-pack/2 but the segment holds no intent entry"},
		{name: "pack/1 with an intent in the segment", fill: fillTwoOrphans, edit: func(d *corpusDraft) {
			d.m.Format, d.m.OrphanIntents, d.m.Orphans = pack.Format, 0, nil
		}, want: "ambermoth-pack/1 but the segment holds 2 intent entries", notErr: "orphan intents in manifest"},
		{name: "a format this verifier does not know", fill: fillTwoOrphans, edit: func(d *corpusDraft) { d.m.Format = "ambermoth-pack/99" },
			want: `unsupported pack format "ambermoth-pack/99"`, notErr: "holds"},
	}
}

func TestVerifyPackChecksFormatAndOrphans(t *testing.T) {
	for _, c := range intentTampers() {
		t.Run(c.name, func(t *testing.T) {
			e, d := draftOf(t, c.fill)
			c.edit(d)
			r := e.verify(d.write(true))
			joined := strings.Join(r.Errors, "\n")
			if r.OK() || !strings.Contains(joined, c.want) {
				t.Errorf("ok=%v, errors %q lack %q", r.OK(), r.Errors, c.want)
			}
			if c.notErr != "" && strings.Contains(joined, c.notErr) {
				t.Errorf("errors mention %q: %q", c.notErr, r.Errors)
			}
		})
	}
}

func TestVerifyPackReportsOrphansOfAnHonestPack(t *testing.T) {
	e, d := draftOf(t, fillTwoOrphans)
	r := e.verify(d.write(false))
	if !r.OK() || !r.SummaryMatches || r.Ledger.OrphanIntents != 2 {
		t.Fatalf("ok=%v errors=%v matches=%v orphans=%d", r.OK(), r.Errors, r.SummaryMatches, r.Ledger.OrphanIntents)
	}
	if !strings.Contains(strings.Join(r.Notes, "\n"), orphanSentence(2)) {
		t.Errorf("notes lack the orphan sentence: %q", r.Notes)
	}
	if !strings.Contains(string(r.ReportHTML), orphanSentence(2)) {
		t.Error("the verifier's report lacks the orphan sentence")
	}
}

// The verifier's report is rendered from the recomputed orphans, whatever the
// manifest claims.
func TestVerifyPackReportRendersRecomputedOrphans(t *testing.T) {
	e, d := draftOf(t, fillTwoOrphans)
	d.m.OrphanIntents, d.m.Orphans = 0, nil
	r := e.verify(d.write(true))
	if r.OK() || !strings.Contains(string(r.ReportHTML), orphanSentence(2)) {
		t.Errorf("ok=%v, report lacks the recomputed orphans", r.OK())
	}
}

func TestVerifyPackOfPlainPackHasNoOrphanNotes(t *testing.T) {
	e, d := draftOf(t, fillNoIntents)
	r := e.verify(d.write(false))
	if !r.OK() || r.Manifest.Format != pack.Format || strings.Contains(strings.Join(r.Notes, "\n"), "intent") {
		t.Errorf("ok=%v format=%s notes=%q", r.OK(), r.Manifest.Format, r.Notes)
	}
}

// A /2 pack from this tool is left in a file: an older verifier reads only the
// format and says it does not support it.
func TestPackV2NamesItsFormatInTheManifest(t *testing.T) {
	e := newEnv(t)
	e.intent(1, "anthropic", "messages")
	e.call(2, "anthropic", "prod")
	o := e.opts()
	e.build(o)
	if !strings.Contains(string(readPack(t, o.Out)["manifest.json"]), `"format": "ambermoth-pack/2"`) {
		t.Error("manifest.json does not say ambermoth-pack/2")
	}
}
