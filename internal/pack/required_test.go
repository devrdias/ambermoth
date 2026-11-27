package pack_test

import (
	"testing"

	"github.com/devrdias/ambermoth/internal/pack"
)

func dropFile(name string) func(*draft) {
	return func(d *draft) { delete(d.files, name) }
}

const missing = ": required by the pack format but missing"

// requiredCases leave one file out, with the manifest rewritten to match.
func requiredCases() []badPackCase {
	var cases []badPackCase
	texts := []string{"LIMITS.md", "VERIFY.md", "letter-template.md"}
	for _, f := range texts {
		cases = append(cases, badPackCase{name: f + " left out", fixture: barePack, touches: []string{f},
			edit: dropFile(f), want: []string{f + missing}})
	}
	return append(cases,
		badPackCase{name: "summary.html left out", fixture: barePack, touches: []string{"summary.html"},
			edit: dropFile("summary.html"), want: []string{"summary.html" + missing, summaryEdited}},
		// another generator skips the byte comparison, so only the presence check can notice
		badPackCase{name: "summary.html left out of a pack from another version", fixture: barePack, touches: []string{"summary.html"},
			edit: func(d *draft) { delete(d.files, "summary.html"); d.m.Generator = "other" }, want: []string{"summary.html" + missing}},
	)
}

func TestVerifyPackRequiresEveryFileOfThePack(t *testing.T) {
	packs := allPacks(t)
	cases := requiredCases()
	cases = append(cases,
		badPackCase{name: "ledger/genesis.json left out", fixture: genesisPack, touches: []string{"ledger/genesis.json"},
			edit: dropFile("ledger/genesis.json"), want: []string{"ledger/genesis.json" + missing, "ledger/genesis.json: unparseable", "segment starts at seq 0 but differs from genesis.json"}},
		badPackCase{name: "ledger/segment.jsonl left out", fixture: barePack, touches: []string{"ledger/segment.jsonl"},
			edit: dropFile("ledger/segment.jsonl"), want: []string{"ledger/segment.jsonl: required by the pack format but missing", totalsDiffer, summaryEdited, "ledger is empty"}},
		badPackCase{name: "a token left out", fixture: genesisPack, rerender: true, touches: []string{"anchors/2-0-HOST.tsr"},
			edit: dropTokens(3), want: []string{"anchor seq 3: token file missing: 2-0-HOST.tsr"}},
		badPackCase{name: "the disclosure keys left out", fixture: disclosedPack, rerender: true, touches: []string{"disclosures/entry-keys.json"},
			edit: dropFile("disclosures/entry-keys.json"), want: []string{"disclosures/entry-keys.json: required by the pack format (the manifest lists 1 disclosed entries) but missing"}},
		badPackCase{name: "a disclosure key the manifest does not list", fixture: disclosedPack, rerender: true,
			edit: manifest(func(m *pack.Manifest) { m.Disclosed = nil }), want: []string{"disclosures/entry-keys.json: has a key for seq 1, which the manifest does not list as disclosed"}},
		badPackCase{name: "a disclosed seq without a key", fixture: disclosedPack, rerender: true,
			edit: manifest(func(m *pack.Manifest) { m.Disclosed = []int64{2} }),
			want: []string{"disclosures/entry-keys.json: has a key for seq 1, which the manifest does not list as disclosed", "disclosures/entry-keys.json: has no key for seq 2, which the manifest lists as disclosed"}},
		badPackCase{name: "the disclosure keys left out and none listed", fixture: disclosedPack, rerender: true, accepted: true,
			edit: func(d *draft) { delete(d.files, "disclosures/entry-keys.json"); d.m.Disclosed = []int64{} }},
	)
	runBadPackCases(t, packs, cases)
}
