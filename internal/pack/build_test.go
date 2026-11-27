package pack_test

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/devrdias/ambermoth/internal/ledger"
	"github.com/devrdias/ambermoth/internal/pack"
	"github.com/devrdias/ambermoth/internal/report"
	"github.com/devrdias/ambermoth/internal/trust"
)

func segmentOf(t *testing.T, path string) []ledger.Entry {
	t.Helper()
	seg, _, err := ledger.Read(bytes.NewReader(readPack(t, path)["ledger/segment.jsonl"]))
	if err != nil {
		t.Fatal(err)
	}
	return seg
}

func seqsOf(es []ledger.Entry) []int64 {
	var out []int64
	for _, e := range es {
		out = append(out, e.Seq())
	}
	return out
}

func typesOf(t *testing.T, jsonl []byte) []string {
	t.Helper()
	es, _, err := ledger.Read(bytes.NewReader(jsonl))
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range es {
		out = append(out, e.Type())
	}
	return out
}

func mustFail(t *testing.T, e *env, o pack.Options) error {
	t.Helper()
	e.on(40)
	if _, err := pack.Build(context.Background(), e.s, o); err != nil {
		if _, statErr := os.Stat(o.Out); statErr == nil {
			t.Errorf("Build failed with %q but left %s behind", err, o.Out)
		}
		return err
	}
	t.Fatal("Build succeeded, want an error")
	return nil
}

type optionsMod func(pack.Options) pack.Options

type noCallsCase struct {
	name string
	mod  optionsMod
}

func TestBuildRefusesAPeriodWithNoCalls(t *testing.T) {
	e := newEnv(t)
	e.keyEvent(0)
	e.call(5, "anthropic", "prod")
	e.gap(6)
	cases := []noCallsCase{
		{"before the first call", func(o pack.Options) pack.Options { return period(o, 1, 4) }},
		{"after the last call", func(o pack.Options) pack.Options { return period(o, 6, 9) }},
		{"for a provider with no calls", func(o pack.Options) pack.Options { o.Provider = "openai"; return o }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := mustFail(t, e, c.mod(e.opts()))
			if err.Error() != "no calls in the selected period" {
				t.Fatalf("error = %q", err)
			}
		})
	}
}

func TestBuildPeriodWithOnlyOtherEntriesHasNoCalls(t *testing.T) {
	e := newEnv(t)
	e.keyEvent(0)
	err := mustFail(t, e, e.opts())
	if err.Error() != "no calls in the selected period" {
		t.Fatalf("error = %q", err)
	}
}

func TestBuildFromGenesis(t *testing.T) {
	e := newEnv(t)
	e.keyEvent(0)
	e.call(1, "anthropic", "prod")
	e.call(2, "anthropic", "prod")
	o := period(e.opts(), 1, 2)
	m := e.build(o)

	want := pack.Segment{FirstSeq: 0, LastSeq: 3, FirstPrevHash: ledger.ZeroHash, LastHash: m.Segment.LastHash}
	if m.Segment != want {
		t.Errorf("segment = %+v, want %+v", m.Segment, want)
	}
	files := readPack(t, o.Out)
	if _, ok := files["ledger/context.jsonl"]; ok || m.ContextEntries != 0 {
		t.Errorf("a segment from genesis has no context: %d entries, file present %v", m.ContextEntries, ok)
	}
	if got := typesOf(t, files["ledger/segment.jsonl"]); got[0] != ledger.TypeGenesis || len(got) != 4 {
		t.Errorf("segment types = %v, want genesis first and 4 entries", got)
	}
	if first, _, _ := bytes.Cut(files["ledger/segment.jsonl"], []byte("\n")); !bytes.Equal(append(first, '\n'), files["ledger/genesis.json"]) {
		t.Error("genesis.json is not the first line of the segment")
	}
	if !e.verify(o.Out).OK() {
		t.Error("pack does not verify")
	}
}

func TestBuildFromGenesisWhenGenesisIsAnchored(t *testing.T) {
	e := newEnv(t)
	e.anchor(0, 1)
	e.call(1, "anthropic", "prod")
	m := e.build(period(e.opts(), 1, 1))
	if m.Segment.FirstSeq != 0 || !m.Segment.StartAnchored || m.Segment.EndAnchored {
		t.Fatalf("segment = %+v, want it to start at the anchored genesis and end unanchored", m.Segment)
	}
}

func TestBuildFromTheMiddle(t *testing.T) {
	e := newEnv(t)
	e.keyEvent(0)                                                                       // 1
	e.link(0, "eval", "git@host:r.git", "0123456789abcdef", "2026-01-02T00:00:00.000Z") // 2
	e.call(1, "anthropic", "prod")                                                      // 3
	e.anchor(1, 1)                                                                      // 4
	e.call(5, "anthropic", "eval")                                                      // 5
	e.call(6, "anthropic", "eval")                                                      // 6
	e.anchor(6, 1)                                                                      // 7
	e.call(8, "anthropic", "prod")                                                      // 8
	o := period(e.opts(), 5, 6)
	m := e.build(o)

	entries, err := e.s.Entries()
	if err != nil {
		t.Fatal(err)
	}
	want := pack.Segment{FirstSeq: 3, LastSeq: 7, FirstPrevHash: entries[2].Hash(), LastHash: entries[7].Hash(),
		StartAnchored: true, EndAnchored: true}
	if m.Segment != want {
		t.Errorf("segment = %+v, want %+v", m.Segment, want)
	}
	if got := seqsOf(segmentOf(t, o.Out)); !reflect.DeepEqual(got, []int64{3, 4, 5, 6, 7}) {
		t.Errorf("segment seqs = %v", got)
	}
	files := readPack(t, o.Out)
	if m.ContextEntries != 2 || !reflect.DeepEqual(typesOf(t, files["ledger/context.jsonl"]), []string{ledger.TypeKeyEvent, ledger.TypeCodeLink}) {
		t.Errorf("context = %d entries %v", m.ContextEntries, typesOf(t, files["ledger/context.jsonl"]))
	}
	if m.Totals.All.Calls != 2 || m.Totals.ByPurpose["prod"] != nil {
		t.Errorf("totals count the call at seq 3 that is in the segment but outside the period: %+v", m.Totals.All)
	}
	if n := bytes.Count(files["summary.html"], []byte("before the segment · self-attested")); n != 2 {
		t.Errorf("summary labels %d context entries as self-attested, want 2", n)
	}
	if r := e.verify(o.Out); !r.OK() || !strings.Contains(strings.Join(r.Notes, "\n"), "2 context entries") {
		t.Errorf("verify: ok %v errors %v notes %v", r.OK(), r.Errors, r.Notes)
	}
}

func TestBuildContextHoldsOnlyKeyEventsCodeLinksAndReconciliations(t *testing.T) {
	e := newEnv(t)
	e.keyEvent(0)                     // 1
	e.link(0, "eval", "r", "abc", "") // 2
	e.reconcile(0)                    // 3
	e.gap(0)                          // 4
	e.call(1, "anthropic", "prod")    // 5
	e.amend(2, 5, 5, "eval")          // 6
	e.anchor(2, 1)                    // 7
	e.call(5, "anthropic", "eval")    // 8
	e.anchor(5, 1)                    // 9
	o := period(e.opts(), 5, 5)
	m := e.build(o)

	if m.Segment.FirstSeq != 8 {
		t.Fatalf("segment starts at seq %d, want 8", m.Segment.FirstSeq)
	}
	got := typesOf(t, readPack(t, o.Out)["ledger/context.jsonl"])
	if want := []string{ledger.TypeKeyEvent, ledger.TypeCodeLink, ledger.TypeReconciliation}; !reflect.DeepEqual(got, want) {
		t.Errorf("context types = %v, want %v", got, want)
	}
	if m.ContextEntries != 3 || m.Totals.All.Calls != 1 {
		t.Errorf("context entries %d, calls %d; the call at seq 5 is before the segment and must not count", m.ContextEntries, m.Totals.All.Calls)
	}
	if !e.verify(o.Out).OK() {
		t.Error("pack does not verify")
	}
}

func TestBuildFilterByProviderAndAccount(t *testing.T) {
	e := newEnv(t)
	e.keyEvent(0)
	e.call(2, "openai", "eval")
	e.call(3, "anthropic", "prod")
	e.call(4, "anthropic", "prod")
	e.call(4, "openai", "eval")
	e.anchor(4, 1)
	base := period(e.opts(), 2, 4)

	all := e.build(base)
	filtered := base
	filtered.Out = filepath.Join(t.TempDir(), "f.zip")
	filtered.Provider, filtered.Account = "anthropic", "acme"
	m := e.build(filtered)

	if all.ProviderFilter != "" || all.Account != "(account not specified)" {
		t.Errorf("unfiltered manifest: provider %q account %q", all.ProviderFilter, all.Account)
	}
	if m.ProviderFilter != "anthropic" || m.Account != "acme" {
		t.Errorf("manifest: provider %q account %q", m.ProviderFilter, m.Account)
	}
	if all.Totals.All.Calls != 4 || m.Totals.All.Calls != 2 {
		t.Errorf("calls: all %d, anthropic %d, want 4 and 2", all.Totals.All.Calls, m.Totals.All.Calls)
	}
	if _, ok := m.Totals.ByProvider["openai"]; ok || len(m.Totals.ByProvider) != 1 {
		t.Errorf("by_provider = %v, want anthropic only", m.Totals.ByProvider)
	}
	if got := seqsOf(segmentOf(t, filtered.Out)); len(got) != len(seqsOf(segmentOf(t, base.Out))) {
		t.Errorf("the other provider's entries stay in the segment: filtered %v", got)
	}
	if m.Period != (pack.Period{From: "2026-03-03", To: "2026-03-05"}) {
		t.Errorf("period = %+v", m.Period)
	}
	if !e.verify(filtered.Out).OK() {
		t.Error("filtered pack does not verify")
	}
}

func TestBuildAccountIsOnlyALabel(t *testing.T) {
	e := newEnv(t)
	e.call(1, "anthropic", "prod")
	a := e.opts()
	b := e.opts()
	b.Account = "acme"
	ma, mb := e.build(a), e.build(b)
	if !reflect.DeepEqual(ma.Totals, mb.Totals) || ma.Segment != mb.Segment {
		t.Error("the account changed the totals or the segment")
	}
}

func TestBuildFilterMovesTheSegmentStart(t *testing.T) {
	e := newEnv(t)
	e.call(1, "openai", "eval")    // 1
	e.call(2, "anthropic", "prod") // 2
	e.anchor(3, 1)                 // 3, target 2
	e.call(4, "anthropic", "prod") // 4
	e.anchor(4, 1)                 // 5, target 4
	unfiltered := e.build(e.opts())
	o := e.opts()
	o.Provider = "anthropic"
	filtered := e.build(o)

	if unfiltered.Segment.FirstSeq != 0 || unfiltered.Segment.StartAnchored {
		t.Errorf("unfiltered start = %+v, want genesis", unfiltered.Segment)
	}
	if filtered.Segment.FirstSeq != 2 || !filtered.Segment.StartAnchored {
		t.Errorf("filtered start = %+v, want the anchored seq 2", filtered.Segment)
	}
}

type endCase struct {
	name        string
	setup       func(e *env)
	provider    string
	from, to    int
	wantLast    int64
	wantAnchor  bool
	wantTokens  int
	wantRetaged int64
}

func TestBuildSegmentEnd(t *testing.T) {
	cases := []endCase{
		{name: "stops at the first anchor entry after the last call", from: 1, to: 2, wantLast: 4, wantAnchor: true, wantTokens: 2,
			setup: func(e *env) {
				e.call(1, "anthropic", "prod") // 1
				e.anchor(1, 1)                 // 2
				e.call(2, "anthropic", "prod") // 3
				e.anchor(2, 1)                 // 4
				e.call(9, "anthropic", "prod") // 5
				e.anchor(9, 1)                 // 6
			}},
		{name: "keeps the anchor entry of every TSA", from: 2, to: 2, wantLast: 5, wantAnchor: true, wantTokens: 2,
			setup: func(e *env) {
				e.call(1, "anthropic", "prod") // 1
				e.anchor(1, 1)                 // 2
				e.call(2, "anthropic", "prod") // 3
				e.anchor(2, 2)                 // 4, 5
				e.call(9, "anthropic", "prod") // 6
				e.anchor(9, 2)                 // 7, 8
			}},
		{name: "ends at the head when the last call is not anchored", from: 1, to: 2, wantLast: 3, wantAnchor: false, wantTokens: 1,
			setup: func(e *env) {
				e.call(1, "anthropic", "prod") // 1
				e.anchor(1, 1)                 // 2
				e.call(2, "anthropic", "prod") // 3
			}},
		{name: "reaches past the anchor to an amendment that retags a call", from: 1, to: 1, wantLast: 4, wantAnchor: true, wantTokens: 2, wantRetaged: 1,
			setup: func(e *env) {
				e.call(1, "anthropic", "prod") // 1
				e.anchor(1, 1)                 // 2
				e.amend(3, 1, 1, "eval")       // 3
				e.anchor(3, 1)                 // 4
			}},
		{name: "ignores an amendment of another provider's calls", provider: "anthropic", from: 1, to: 1, wantLast: 3, wantAnchor: true, wantTokens: 1,
			setup: func(e *env) {
				e.call(1, "anthropic", "prod") // 1
				e.call(1, "openai", "prod")    // 2
				e.anchor(1, 1)                 // 3
				e.amend(3, 2, 2, "eval")       // 4
				e.anchor(3, 1)                 // 5
			}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t)
			c.setup(e)
			o := period(e.opts(), c.from, c.to)
			o.Provider = c.provider
			m := e.build(o)
			if m.Segment.LastSeq != c.wantLast || m.Segment.EndAnchored != c.wantAnchor {
				t.Errorf("segment end = seq %d anchored %v, want seq %d anchored %v", m.Segment.LastSeq, m.Segment.EndAnchored, c.wantLast, c.wantAnchor)
			}
			tokens := 0
			for n := range readPack(t, o.Out) {
				if strings.HasPrefix(n, "anchors/") {
					tokens++
				}
			}
			if tokens != c.wantTokens {
				t.Errorf("%d token files, want %d", tokens, c.wantTokens)
			}
			if m.Totals.All.Retagged != c.wantRetaged {
				t.Errorf("retagged = %d, want %d", m.Totals.All.Retagged, c.wantRetaged)
			}
			if !e.verify(o.Out).OK() {
				t.Error("pack does not verify")
			}
		})
	}
}

func TestBuildUnanchoredTailIsReportedByVerifyPack(t *testing.T) {
	e := newEnv(t)
	e.call(1, "anthropic", "prod") // 1
	e.anchor(1, 1)                 // 2
	e.call(2, "anthropic", "prod") // 3
	e.call(3, "anthropic", "prod") // 4
	o := period(e.opts(), 2, 3)
	m := e.build(o)
	if m.Segment.EndAnchored || m.Segment.LastSeq != 4 || !m.Segment.StartAnchored || m.Segment.FirstSeq != 1 {
		t.Fatalf("segment = %+v", m.Segment)
	}
	r := e.verify(o.Out)
	if !r.OK() || !reflect.DeepEqual(r.Warnings, []string{"the last 2 entries of the segment are not anchored (self-attested)"}) {
		t.Errorf("ok %v warnings %v errors %v", r.OK(), r.Warnings, r.Errors)
	}
	if !bytes.Contains(readPack(t, o.Out)["summary.html"], []byte("not anchored (self-attested tail)")) {
		t.Error("summary does not say the tail is self-attested")
	}
}

func TestBuildDisclosures(t *testing.T) {
	e := newEnv(t)
	e.keyEvent(0)                           // 1
	e.callBody(1, "anthropic", "prod", "a") // 2
	e.anchor(1, 1)                          // 3
	e.callBody(2, "anthropic", "prod", "b") // 4
	e.callBody(3, "anthropic", "prod", "c") // 5
	e.anchor(3, 1)                          // 6
	e.call(9, "anthropic", "prod")          // 7
	base := period(e.opts(), 2, 3)

	t.Run("writes the keys of calls inside the segment, in order", func(t *testing.T) {
		o := base
		o.Disclose = []int64{5, 4}
		m := e.build(o)
		if !reflect.DeepEqual(m.Disclosed, []int64{4, 5}) {
			t.Errorf("disclosed = %v", m.Disclosed)
		}
		var keys map[string]string
		if err := json.Unmarshal(readPack(t, o.Out)["disclosures/entry-keys.json"], &keys); err != nil {
			t.Fatal(err)
		}
		entries, _ := e.s.Entries()
		want := map[string]string{"4": hex.EncodeToString(ledger.EntryKey(e.s.MasterKey, entries[4].Str("salt_id"))),
			"5": hex.EncodeToString(ledger.EntryKey(e.s.MasterKey, entries[5].Str("salt_id")))}
		if !reflect.DeepEqual(keys, want) {
			t.Errorf("keys = %v, want %v", keys, want)
		}
		if ok, _, err := pack.CheckSample(o.Out, 4, []byte("b"), nil); err != nil || !ok {
			t.Errorf("sample for seq 4: ok %v err %v", ok, err)
		}
		if !strings.Contains(strings.Join(e.verify(o.Out).Notes, "\n"), "2 entr(ies) disclosed for sample checking: [4 5]") {
			t.Error("verify-pack does not note the disclosures")
		}
	})
	t.Run("lists a seq once when it is given more than once", func(t *testing.T) {
		o := base
		o.Disclose = []int64{5, 4, 5, 4, 4}
		m := e.build(o)
		if !reflect.DeepEqual(m.Disclosed, []int64{4, 5}) {
			t.Errorf("disclosed = %v, want [4 5]", m.Disclosed)
		}
		var keys map[string]string
		if err := json.Unmarshal(readPack(t, o.Out)["disclosures/entry-keys.json"], &keys); err != nil {
			t.Fatal(err)
		}
		if len(keys) != 2 || keys["4"] == "" || keys["5"] == "" {
			t.Errorf("keys file holds %v, want exactly seqs 4 and 5", keys)
		}
		if r := e.verify(o.Out); !r.OK() || !strings.Contains(strings.Join(r.Notes, "\n"), "2 entr(ies) disclosed for sample checking: [4 5]") {
			t.Errorf("verify-pack: errors %v, notes %v", r.Errors, r.Notes)
		}
	})
	t.Run("accepts a pack from an older version that lists a seq twice", func(t *testing.T) {
		g := newDisclosedPack(t)
		d := g.draft(t)
		d.m.Disclosed = []int64{1, 1}
		if r := g.e.verify(d.write(false)); !r.OK() {
			t.Errorf("verify-pack rejects the duplicate: %v", lines(r))
		}
	})
	t.Run("writes an empty list and no file when nothing is disclosed", func(t *testing.T) {
		o := base
		e.build(o)
		files := readPack(t, o.Out)
		if _, ok := files["disclosures/entry-keys.json"]; ok {
			t.Error("entry-keys.json present without --disclose")
		}
		if !bytes.Contains(files["manifest.json"], []byte(`"disclosed": []`)) {
			t.Error(`manifest does not hold "disclosed": []`)
		}
	})
}

type discloseCase struct {
	name string
	seq  int64
}

func TestBuildRejectsDisclosuresThatAreNotCallsInTheSegment(t *testing.T) {
	e := newEnv(t)
	e.keyEvent(0)                  // 1
	e.call(1, "anthropic", "prod") // 2
	e.anchor(1, 1)                 // 3
	e.call(2, "anthropic", "prod") // 4
	e.call(3, "anthropic", "prod") // 5
	e.anchor(3, 1)                 // 6
	e.call(9, "anthropic", "prod") // 7
	cases := []discloseCase{
		{"a negative seq", -1}, {"a key event before the segment", 1}, {"an anchor entry in the segment", 3},
		{"a call after the segment", 7}, {"a seq past the ledger", 99},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			o := period(e.opts(), 2, 3)
			o.Disclose = []int64{4, c.seq}
			err := mustFail(t, e, o)
			if want := "--disclose " + itoa(c.seq) + ": not a call entry inside the pack segment"; err.Error() != want {
				t.Fatalf("error = %q, want %q", err, want)
			}
		})
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

// editLedger rewrites the ledger file in place.
func editLedger(t *testing.T, e *env, edit func([]byte) []byte) {
	t.Helper()
	b, err := os.ReadFile(e.s.LedgerPath())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(e.s.LedgerPath(), edit(b), 0o600); err != nil {
		t.Fatal(err)
	}
}

type brokenLedgerCase struct {
	name    string
	damage  func(t *testing.T, e *env)
	roots   bool // build with only the pinned roots, which the test TSA is not among
	wantErr string
}

const refusal = "the ledger does not verify; refusing to build a pack from it:\n  "

func TestBuildRefusesALedgerThatFailsVerification(t *testing.T) {
	cases := []brokenLedgerCase{
		{name: "an entry edited after writing",
			damage: func(t *testing.T, e *env) {
				editLedger(t, e, func(b []byte) []byte { return bytes.Replace(b, []byte(`"tokens_in":100`), []byte(`"tokens_in":1`), 1) })
			},
			wantErr: refusal + "seq 1: entry_hash mismatch (entry altered after writing)"},
		{name: "a removed entry",
			damage: func(t *testing.T, e *env) {
				editLedger(t, e, func(b []byte) []byte {
					lines := bytes.SplitAfter(b, []byte("\n"))
					return bytes.Join(append(lines[:1:1], lines[2:]...), nil)
				})
			},
			wantErr: refusal + "seq 2 follows seq 0: sequence is not contiguous (entry removed or inserted)\n  seq 2: prev_hash does not match previous entry_hash (chain broken)"},
		{name: "a missing token file",
			damage: func(t *testing.T, e *env) {
				files, _ := filepath.Glob(filepath.Join(e.s.Dir, ledger.AnchorDir, "*"))
				for _, f := range files {
					if err := os.Remove(f); err != nil {
						t.Fatal(err)
					}
				}
			},
			wantErr: refusal + "anchor seq 2: token file missing: "},
		{name: "anchors from a TSA that is not trusted", roots: true,
			damage:  func(*testing.T, *env) {},
			wantErr: refusal + "anchor seq 2: "},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t)
			e.call(1, "anthropic", "prod") // 1
			e.anchor(1, 1)                 // 2
			c.damage(t, e)
			o := e.opts()
			if c.roots {
				o.Roots, _ = trust.Pool(nil)
			}
			err := mustFail(t, e, o)
			if !strings.HasPrefix(err.Error(), c.wantErr) {
				t.Fatalf("error = %q, want prefix %q", err, c.wantErr)
			}
		})
	}
}

func TestBuildNeedsTheLedgerFile(t *testing.T) {
	e := newEnv(t)
	e.call(1, "anthropic", "prod")
	if err := os.Remove(e.s.LedgerPath()); err != nil {
		t.Fatal(err)
	}
	if err := mustFail(t, e, e.opts()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("error = %v, want a missing-file error", err)
	}
}

func TestBuildReplacesAnExistingOutputFile(t *testing.T) {
	e := newEnv(t)
	e.call(1, "anthropic", "prod")
	o := e.opts()
	if err := os.WriteFile(o.Out, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	e.build(o)
	if !e.verify(o.Out).OK() {
		t.Error("the existing file was not replaced by a valid pack")
	}
}

func TestBuildFailureLeavesAnExistingOutputFileAlone(t *testing.T) {
	e := newEnv(t)
	e.call(1, "anthropic", "prod")
	o := e.opts()
	o.Disclose = []int64{99}
	if err := os.WriteFile(o.Out, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	e.on(40)
	if _, err := pack.Build(context.Background(), e.s, o); err == nil {
		t.Fatal("want an error")
	}
	if b, _ := os.ReadFile(o.Out); string(b) != "old" {
		t.Errorf("output file = %q, want it untouched", b)
	}
}

func TestBuildCannotWriteIntoAMissingDirectory(t *testing.T) {
	e := newEnv(t)
	e.call(1, "anthropic", "prod")
	o := e.opts()
	o.Out = filepath.Join(filepath.Dir(o.Out), "nodir", "pack.zip")
	e.on(40)
	if _, err := pack.Build(context.Background(), e.s, o); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("error = %v, want a missing-directory error", err)
	}
}

func TestBuildTimestampFailureIsReported(t *testing.T) {
	e := newEnv(t)
	e.call(1, "anthropic", "prod")
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusInternalServerError) }))
	t.Cleanup(down.Close)
	o := e.opts()
	o.TSAs = []string{down.URL}
	err := mustFail(t, e, o)
	if !strings.HasPrefix(err.Error(), "timestamping manifest: ") {
		t.Fatalf("error = %q", err)
	}
}

func TestBuildManifestMatchesThePack(t *testing.T) {
	e := newEnv(t)
	e.keyEvent(0)
	e.call(1, "anthropic", "prod")
	e.call(2, "anthropic", "eval")
	e.anchor(2, 1)
	o := period(e.opts(), 1, 2)
	o.Account, o.Provider = "acme", "anthropic"
	m := e.build(o)

	if m.Format != pack.Format || m.Generator != "test" || m.LedgerID != e.s.LedgerID || m.Created != ledger.FormatTime(e.now) {
		t.Errorf("identity fields: %+v", m)
	}
	files := readPack(t, o.Out)
	want, err := json.MarshalIndent(m, "", "  ")
	if err != nil || !bytes.Equal(append(want, '\n'), files["manifest.json"]) {
		t.Errorf("manifest.json is not the returned manifest, indented, with a trailing newline")
	}
	var listed []string
	for _, f := range m.Files {
		listed = append(listed, f.Path)
		if b := files[f.Path]; f.SHA256 != ledger.SHA256Bytes(b) || f.Bytes != int64(len(b)) {
			t.Errorf("%s: manifest has %s, %d bytes; file has %s, %d", f.Path, f.SHA256, f.Bytes, ledger.SHA256Bytes(b), len(b))
		}
	}
	if names := zipNames(t, o.Out); !reflect.DeepEqual(listed, names[1:]) {
		t.Errorf("manifest lists %v, zip holds %v after manifest.json", listed, names[1:])
	}
}

func TestBuildTotalsMatchTheSegment(t *testing.T) {
	e := newEnv(t)
	e.keyEvent(0)
	e.call(1, "anthropic", "prod")
	e.call(1, "openai", "prod")
	e.call(2, "anthropic", "eval")
	e.amend(3, 4, 4, "prod")
	e.anchor(3, 1)
	o := period(e.opts(), 1, 2)
	o.Provider = "anthropic"
	m := e.build(o)
	f := report.Filter{From: o.From, To: o.To, Provider: o.Provider}
	if got := report.Summarize(segmentOf(t, o.Out), f).Totals; !reflect.DeepEqual(got, m.Totals) {
		t.Errorf("manifest totals differ from totals recomputed from the segment:\n%+v\n%+v", m.Totals.All, got.All)
	}
	if m.Totals.All.Calls != 2 || m.Totals.ByPurpose["prod"].Retagged != 1 {
		t.Errorf("totals = %+v", m.Totals.All)
	}
}

func TestBuildZipHoldsTheFilesInOrder(t *testing.T) {
	e := newEnv(t)
	e.keyEvent(0)
	e.call(1, "anthropic", "prod")
	e.anchor(1, 1)
	e.call(2, "anthropic", "prod")
	e.anchor(2, 2)
	o := period(e.opts(), 2, 2)
	o.Disclose = []int64{4}
	o.TSAs = e.urls[:1]
	m := e.build(o)

	names := zipNames(t, o.Out)
	if len(names) != 12 {
		t.Fatalf("%d files: %v", len(names), names)
	}
	var anchors []string
	var rest []string
	for _, n := range names {
		if strings.HasPrefix(n, "anchors/") {
			anchors = append(anchors, n)
			continue
		}
		rest = append(rest, n)
	}
	wantRest := []string{"manifest.json", "LIMITS.md", "VERIFY.md", "disclosures/entry-keys.json", "ledger/context.jsonl",
		"ledger/genesis.json", "ledger/segment.jsonl", "letter-template.md", "summary.html", "manifest.tsr"}
	if !reflect.DeepEqual(rest, wantRest) || len(anchors) != 2 {
		t.Errorf("files = %v with %d tokens, want %v", rest, len(anchors), wantRest)
	}
	if !e.verify(o.Out).OK() || len(m.Files) != 10 {
		t.Errorf("pack does not verify or lists %d files, want 10", len(m.Files))
	}
}
