package pack_test

import (
	"bytes"
	"fmt"
	"os"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/devrdias/ambermoth/internal/canon"
	"github.com/devrdias/ambermoth/internal/ledger"
	"github.com/devrdias/ambermoth/internal/pack"
)

type fixtureKind int

const (
	midPack       fixtureKind = iota // segment 2..6, anchored at both ends, one context entry
	genesisPack                      // segment 0..4, anchor at seq 3, unanchored tail
	barePack                         // segment 0..2, no anchors
	disclosedPack                    // the bare pack with the keys of seq 1 disclosed
)

const (
	summaryEdited = "summary.html does not match a re-render from the verified ledger: it was edited after the pack was built. Read the verifier's report instead."
	totalsDiffer  = "usage totals in manifest do not match totals recomputed from the ledger segment"
	startDiffers  = "segment start does not match manifest"
	endDiffers    = "segment end does not match manifest"
	noContext     = "manifest lists 1 context entries but ledger/context.jsonl has 0"
)

// tokenName turns "3-0-127.0.0.1_41234.tsr" into "3-0-HOST.tsr": the port is random.
var tokenName = regexp.MustCompile(`(\d+-\d+)-127\.0\.0\.1_\d+\.tsr`)

func normalised(ls []string) []string {
	out := make([]string, len(ls))
	for i, l := range ls {
		out[i] = tokenName.ReplaceAllString(l, "$1-HOST.tsr")
	}
	return out
}

type badPackCase struct {
	name     string
	fixture  fixtureKind
	touches  []string // files the edit changes; the unfixed run must flag each
	rerender bool     // the sender regenerates summary.html after the edit
	accepted bool     // verify-pack finds nothing wrong
	edit     func(d *draft)
	want     []string // exact errors with the manifest fixed: the pack's, then the ledger's
}

func setField(seq int64, key string, val any) func(*draft) {
	return func(d *draft) {
		d.rechain(func(es []ledger.Entry) []ledger.Entry {
			entryWithSeq(es, seq)[key] = val
			return es
		})
	}
}

// dropSeq removes the entry with seq from the segment and links the rest again.
func dropSeq(seq int64) func(*draft) {
	return func(d *draft) {
		d.rechain(func(es []ledger.Entry) []ledger.Entry {
			var out []ledger.Entry
			for _, e := range es {
				if e.Seq() != seq {
					out = append(out, e)
				}
			}
			return out
		})
	}
}

// pinned prints what a case reports, to write its want from.
func pinned(name string, got []string) {
	if os.Getenv("PIN") != "" {
		fmt.Printf("PIN %s\n", name)
		for _, l := range got {
			fmt.Printf("\t\t%q,\n", l)
		}
	}
}

// rawLine edits the segment line of seq without linking or hashing again.
func rawLine(seq int64, edit func(line []byte) []byte) func(*draft) {
	return func(d *draft) {
		ls := bytes.SplitAfter(d.files["ledger/segment.jsonl"], []byte("\n"))
		key := []byte(fmt.Sprintf(`"seq":%d,`, seq))
		for i, l := range ls {
			if bytes.Contains(l, key) {
				ls[i] = edit(l)
			}
		}
		d.files["ledger/segment.jsonl"] = bytes.Join(ls, nil)
	}
}

func rawDrop(seq int64) func(*draft) {
	return rawLine(seq, func([]byte) []byte { return nil })
}

// rawSwap swaps the segment line of seq with the one after it.
func rawSwap(seq int64) func(*draft) {
	return func(d *draft) {
		ls := bytes.SplitAfter(d.files["ledger/segment.jsonl"], []byte("\n"))
		for i := range ls[:len(ls)-1] {
			if bytes.Contains(ls[i], []byte(fmt.Sprintf(`"seq":%d,`, seq))) {
				ls[i], ls[i+1] = ls[i+1], ls[i]
				break
			}
		}
		d.files["ledger/segment.jsonl"] = bytes.Join(ls, nil)
	}
}

func swapSeqs(a, b int64) func(*draft) {
	return func(d *draft) {
		d.rechain(func(es []ledger.Entry) []ledger.Entry {
			var i, j int
			for k, e := range es {
				if e.Seq() == a {
					i = k
				}
				if e.Seq() == b {
					j = k
				}
			}
			es[i], es[j] = es[j], es[i]
			return es
		})
	}
}

func manifest(edit func(m *pack.Manifest)) func(*draft) {
	return func(d *draft) { edit(d.m) }
}

func segmentCases() []badPackCase {
	const seg = "ledger/segment.jsonl"
	changeTokens := func(line []byte) []byte {
		return bytes.Replace(line, []byte(`"tokens_in":100`), []byte(`"tokens_in":1`), 1)
	}
	return []badPackCase{
		{name: "a changed entry", fixture: midPack, touches: []string{seg}, edit: rawLine(4, changeTokens),
			want: []string{totalsDiffer, summaryEdited, "seq 4: entry_hash mismatch (entry altered after writing)"}},
		{name: "a changed entry with the chain rebuilt after it", fixture: midPack, touches: []string{seg}, edit: setField(4, "tokens_in", int64(1)),
			want: []string{totalsDiffer, summaryEdited, "anchor seq 6: target_hash does not match the target entry"}},
		{name: "a removed entry", fixture: midPack, touches: []string{seg}, edit: rawDrop(4),
			want: []string{totalsDiffer, summaryEdited, "seq 5 follows seq 3: sequence is not contiguous (entry removed or inserted)", "seq 5: prev_hash does not match previous entry_hash (chain broken)"}},
		{name: "a removed entry with the chain rebuilt", fixture: midPack, touches: []string{seg}, edit: dropSeq(4),
			want: []string{totalsDiffer, summaryEdited, "seq 5 follows seq 3: sequence is not contiguous (entry removed or inserted)", "anchor seq 6: target_hash does not match the target entry"}},
		{name: "a removed first entry", fixture: midPack, touches: []string{seg}, edit: dropSeq(2),
			want: []string{startDiffers, "manifest claims the segment start is anchored, but no valid anchor covers seq 2", summaryEdited, "anchor seq 3: target seq 2 is outside the verified range"}},
		{name: "a removed last entry", fixture: midPack, touches: []string{seg}, edit: dropSeq(6),
			want: []string{endDiffers, summaryEdited}},
		{name: "reordered entries", fixture: midPack, touches: []string{seg}, edit: rawSwap(4),
			want: []string{totalsDiffer, summaryEdited, "seq 5 follows seq 3: sequence is not contiguous (entry removed or inserted)", "seq 5: prev_hash does not match previous entry_hash (chain broken)", "seq 4 follows seq 5: sequence is not contiguous (entry removed or inserted)", "seq 4: prev_hash does not match previous entry_hash (chain broken)", "seq 6 follows seq 4: sequence is not contiguous (entry removed or inserted)", "seq 6: prev_hash does not match previous entry_hash (chain broken)"}},
		{name: "reordered entries with the chain rebuilt", fixture: midPack, touches: []string{seg}, edit: swapSeqs(4, 5),
			want: []string{totalsDiffer, summaryEdited, "seq 5 follows seq 3: sequence is not contiguous (entry removed or inserted)", "seq 4 follows seq 5: sequence is not contiguous (entry removed or inserted)", "seq 6 follows seq 4: sequence is not contiguous (entry removed or inserted)", "anchor seq 6: target_hash does not match the target entry"}},
		{name: "a first entry with another prev_hash", fixture: midPack, touches: []string{seg}, edit: setField(2, "prev_hash", ledger.ZeroHash),
			want: []string{startDiffers, "manifest claims the segment start is anchored, but no valid anchor covers seq 2", "manifest claims the segment end is anchored, but no valid anchor found", summaryEdited, "anchor seq 3: target_hash does not match the target entry", "anchor seq 6: target_hash does not match the target entry"}},
		{name: "a manifest with another first prev_hash", fixture: midPack,
			edit: manifest(func(m *pack.Manifest) { m.Segment.FirstPrevHash = ledger.ZeroHash }), want: []string{startDiffers}},
		{name: "a manifest with another first seq", fixture: midPack,
			edit: manifest(func(m *pack.Manifest) { m.Segment.FirstSeq++ }), want: []string{startDiffers, "manifest claims the segment start is anchored, but no valid anchor covers seq 3", summaryEdited}},
		{name: "a manifest with another last seq", fixture: midPack,
			edit: manifest(func(m *pack.Manifest) { m.Segment.LastSeq-- }), want: []string{endDiffers, summaryEdited}},
		{name: "a manifest with another last hash", fixture: midPack,
			edit: manifest(func(m *pack.Manifest) { m.Segment.LastHash = ledger.ZeroHash }), want: []string{endDiffers, summaryEdited}},
		{name: "a genesis entry that differs from genesis.json", fixture: genesisPack, touches: []string{seg}, edit: setField(0, "generator", "other"),
			want: []string{"segment starts at seq 0 but differs from genesis.json", summaryEdited, "anchor seq 3: target_hash does not match the target entry"}},
		{name: "a genesis.json that differs from the segment", fixture: genesisPack, touches: []string{"ledger/genesis.json"}, edit: otherGenesis,
			want: []string{"segment starts at seq 0 but differs from genesis.json"}},
		{name: "an empty segment", fixture: midPack, touches: []string{seg}, edit: func(d *draft) { d.files[seg] = nil },
			want: []string{"manifest claims the segment start is anchored, but no valid anchor covers seq 2", "manifest claims the segment end is anchored, but no valid anchor found", totalsDiffer, summaryEdited, "ledger is empty"}},
		{name: "a context entry that is not before the segment", fixture: midPack, touches: []string{"ledger/context.jsonl"},
			edit: func(d *draft) {
				var line []byte
				rawLine(4, func(l []byte) []byte { line = l; return l })(d)
				d.files["ledger/context.jsonl"] = append(d.files["ledger/context.jsonl"], line...)
			},
			want: []string{"context entry seq 4 is not before the segment", "manifest lists 1 context entries but ledger/context.jsonl has 2", summaryEdited}},
	}
}

// tokenOf returns the path of the token file of the anchor entry with seq.
func tokenOf(d *draft, seq int64) string {
	for _, e := range d.segment() {
		if e.Seq() == seq {
			return "anchors/" + e.Str("token_file")
		}
	}
	d.t.Fatalf("no anchor entry with seq %d", seq)
	return ""
}

// useToken makes the anchor entry with seq point at token, a file in the pack.
func useToken(seq int64, token func(d *draft) []byte) func(*draft) {
	return func(d *draft) {
		b := token(d)
		d.files[tokenOf(d, seq)] = b
		setField(seq, "token_sha256", ledger.SHA256Bytes(b))(d)
	}
}

func dropTokens(seqs ...int64) func(*draft) {
	return func(d *draft) {
		for _, q := range seqs {
			delete(d.files, tokenOf(d, q))
		}
	}
}

func anchoringCases() []badPackCase {
	return []badPackCase{
		{name: "a start claimed anchored at genesis", fixture: genesisPack, rerender: true,
			edit: manifest(func(m *pack.Manifest) { m.Segment.StartAnchored = true }), want: []string{"manifest claims the segment start is anchored, but no valid anchor covers seq 0"}},
		{name: "a start claimed anchored in a pack with no anchors", fixture: barePack, rerender: true,
			edit: manifest(func(m *pack.Manifest) { m.Segment.StartAnchored = true }), want: []string{"manifest claims the segment start is anchored, but no valid anchor covers seq 0"}},
		{name: "an end claimed anchored in a pack with no anchors", fixture: barePack, rerender: true,
			edit: manifest(func(m *pack.Manifest) { m.Segment.EndAnchored = true }), want: []string{"manifest claims the segment end is anchored, but no valid anchor found"}},
		{name: "an end claimed anchored past the last anchor", fixture: genesisPack, rerender: true, accepted: true,
			edit: manifest(func(m *pack.Manifest) { m.Segment.EndAnchored = true })},
		{name: "an end claimed anchored, with the summary left as it was", fixture: genesisPack,
			edit: manifest(func(m *pack.Manifest) { m.Segment.EndAnchored = true }), want: []string{summaryEdited}},
		{name: "a start that is neither genesis nor claimed anchored", fixture: midPack, rerender: true,
			edit: manifest(func(m *pack.Manifest) { m.Segment.StartAnchored = false }), want: []string{"segment neither starts at genesis nor at an anchored entry"}},
		{name: "an end not claimed anchored although it is", fixture: midPack, rerender: true, accepted: true,
			edit: manifest(func(m *pack.Manifest) { m.Segment.EndAnchored = false })},
		{name: "a token file missing for the start anchor", fixture: midPack, rerender: true, touches: []string{"anchors/2-0-HOST.tsr"},
			edit: dropTokens(3), want: []string{"manifest claims the segment start is anchored, but no valid anchor covers seq 2", "anchor seq 3: token file missing: 2-0-HOST.tsr"}},
		{name: "every token file missing", fixture: midPack, rerender: true, touches: []string{"anchors/2-0-HOST.tsr", "anchors/5-0-HOST.tsr"},
			edit: dropTokens(3, 6), want: []string{"manifest claims the segment start is anchored, but no valid anchor covers seq 2", "manifest claims the segment end is anchored, but no valid anchor found", "anchor seq 3: token file missing: 2-0-HOST.tsr", "anchor seq 6: token file missing: 5-0-HOST.tsr"}},
		{name: "a token for another head", fixture: midPack, rerender: true, touches: []string{"anchors/5-0-HOST.tsr"},
			edit: useToken(6, func(d *draft) []byte { return d.files[tokenOf(d, 3)] }), want: []string{"anchor seq 6: token's message imprint does not match the anchored hash"}},
		{name: "a token that fails verification", fixture: midPack, rerender: true, touches: []string{"anchors/5-0-HOST.tsr"},
			edit: useToken(6, func(*draft) []byte { return []byte("not a token") }), want: []string{"anchor seq 6: not a TimeStampResp: asn1: syntax error: data truncated"}},
		{name: "a token that does not match its anchor entry", fixture: midPack, touches: []string{"anchors/5-0-HOST.tsr"},
			edit: func(d *draft) { d.files[tokenOf(d, 6)] = d.files[tokenOf(d, 3)] }, want: []string{summaryEdited, "anchor seq 6: token file hash mismatch"}},
		{name: "a context count that disagrees with the file", fixture: midPack, rerender: true,
			edit: manifest(func(m *pack.Manifest) { m.ContextEntries = 7 }), want: []string{"manifest lists 7 context entries but ledger/context.jsonl has 1"}},
		{name: "a context file left out", fixture: midPack, rerender: true, touches: []string{contextFile},
			edit: func(d *draft) { delete(d.files, contextFile) }, want: []string{noContext}},
		{name: "a context file in a pack that lists no context entries", fixture: midPack, rerender: true,
			edit: manifest(func(m *pack.Manifest) { m.ContextEntries = 0 }), want: []string{"manifest lists 0 context entries but ledger/context.jsonl has 1"}},
		{name: "a context file left out and the count set to zero", fixture: midPack, rerender: true, accepted: true,
			edit: func(d *draft) { delete(d.files, contextFile); d.m.ContextEntries = 0 }},
		{name: "an anchor of a seq before the segment", fixture: midPack, rerender: true, touches: []string{"ledger/segment.jsonl"},
			edit: setField(6, "target_seq", int64(1)), want: []string{"anchor seq 6: target seq 1 is outside the verified range"}},
		{name: "an anchor of a seq after the segment", fixture: midPack, rerender: true, touches: []string{"ledger/segment.jsonl"},
			edit: setField(6, "target_seq", int64(99)), want: []string{"anchor seq 6: target seq 99 is outside the verified range"}},
		{name: "an anchor of another entry in the segment", fixture: midPack, rerender: true, touches: []string{"ledger/segment.jsonl"},
			edit: setField(6, "target_seq", int64(4)), want: []string{"anchor seq 6: target_hash does not match the target entry"}},
	}
}

const contextFile = "ledger/context.jsonl"

func allPacks(t *testing.T) map[fixtureKind]goodPack {
	return map[fixtureKind]goodPack{midPack: newMidPack(t), genesisPack: newGenesisPack(t), barePack: newBarePack(t), disclosedPack: newDisclosedPack(t)}
}

func TestVerifyPackAnchoringFailures(t *testing.T) {
	packs := allPacks(t)
	runBadPackCases(t, packs, anchoringCases())
}

// otherGenesis swaps genesis.json for a valid genesis of the same ledger that
// is not the segment's first line.
func otherGenesis(d *draft) {
	gv, err := canon.Parse(bytes.TrimSpace(d.files["ledger/genesis.json"]))
	if err != nil {
		d.t.Fatal(err)
	}
	g := ledger.Entry(gv.(map[string]any))
	g["generator"] = "other"
	delete(g, "entry_hash")
	h, err := ledger.ComputeHash(g)
	if err != nil {
		d.t.Fatal(err)
	}
	g["entry_hash"] = h
	line, err := canon.Marshal(map[string]any(g))
	if err != nil {
		d.t.Fatal(err)
	}
	d.files["ledger/genesis.json"] = append(line, '\n')
}

func TestVerifyPackSegmentFailures(t *testing.T) {
	packs := allPacks(t)
	runBadPackCases(t, packs, segmentCases())
}

func runBadPackCases(t *testing.T, packs map[fixtureKind]goodPack, cases []badPackCase) {
	for _, c := range cases {
		g := packs[c.fixture]
		t.Run("fixed manifest: "+c.name, func(t *testing.T) {
			d := g.draft(t)
			c.edit(d)
			if c.rerender {
				d.rerender(g.e)
			}
			r, err := pack.VerifyPack(d.write(true), g.e.verifyOptions())
			if err != nil {
				t.Fatalf("VerifyPack: %v", err)
			}
			got := normalised(lines(r))
			pinned(c.name, got)
			if c.accepted {
				if !r.OK() || len(got) > 0 {
					t.Errorf("want the pack accepted, got OK=%v errors:\n%s", r.OK(), strings.Join(got, "\n"))
				}
				return
			}
			if r.OK() || !reflect.DeepEqual(got, c.want) {
				t.Errorf("OK=%v, errors:\n%s\nwant:\n%s", r.OK(), strings.Join(got, "\n"), strings.Join(c.want, "\n"))
			}
		})
		if len(c.touches) == 0 || c.accepted {
			continue
		}
		t.Run("stale manifest: "+c.name, func(t *testing.T) {
			d := g.draft(t)
			c.edit(d)
			r, err := pack.VerifyPack(d.write(false), g.e.verifyOptions())
			if err != nil {
				t.Fatalf("VerifyPack: %v", err)
			}
			got := normalised(r.Errors)
			for _, f := range c.touches {
				if !hasPrefixLine(got, f+": ") {
					t.Errorf("no error about %s in:\n%s", f, strings.Join(got, "\n"))
				}
			}
			if r.OK() {
				t.Error("pack verified")
			}
		})
	}
}

func hasPrefixLine(ls []string, prefix string) bool {
	for _, l := range ls {
		if strings.HasPrefix(l, prefix) {
			return true
		}
	}
	return false
}

func TestVerifyPackFailsOnASegmentLineTooLongToRead(t *testing.T) {
	g := newBarePack(t)
	d := g.draft(t)
	d.files["ledger/segment.jsonl"] = bytes.Repeat([]byte("a"), 17<<20)
	if _, err := pack.VerifyPack(d.write(true), g.e.verifyOptions()); err == nil || err.Error() != "bufio.Scanner: token too long" {
		t.Fatalf("error = %v, want bufio.Scanner: token too long", err)
	}
}
