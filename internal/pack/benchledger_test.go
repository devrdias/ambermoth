package pack_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/devrdias/ambermoth/internal/ledger"
	"github.com/devrdias/ambermoth/internal/ops"
	"github.com/devrdias/ambermoth/internal/pack"
)

// The benchmarks are only worth reading if their ledger is valid and the
// packs built from it verify; this runs the generator small.
func TestGeneratedLedgerVerifiesAndPacks(t *testing.T) {
	const anchors = 3
	bl := generateLedger(t, t.TempDir(), 900, anchors)
	es, lines, err := ledger.ReadFile(bl.ledgerPath())
	if err != nil {
		t.Fatal(err)
	}
	opt := ledger.VerifyOptions{AnchorDir: filepath.Join(bl.dir, ledger.AnchorDir), CheckAnchor: ops.AnchorVerifier(bl.roots, false)}
	if r := ledger.Verify(es, lines, opt); !r.OK() || len(r.Anchors) != anchors {
		t.Fatalf("Verify: errors %v, %d anchors, want none and %d", r.Errors, len(r.Anchors), anchors)
	}
	if len(es) != bl.entries {
		t.Fatalf("ledger has %d entries, generator says %d", len(es), bl.entries)
	}
	assertEntryTypes(t, es, ledger.TypeCall, ledger.TypeGap, ledger.TypeAmendment, ledger.TypeKeyEvent, ledger.TypeAnchor)
	for _, sc := range benchScopes {
		s, err := ledger.Open(bl.dir, time.Now)
		if err != nil {
			t.Fatal(err)
		}
		out := filepath.Join(t.TempDir(), sc.name+".zip")
		if _, err := pack.Build(context.Background(), s, bl.options(sc, out, bl.roots)); err != nil {
			t.Fatalf("%s: Build: %v", sc.name, err)
		}
		r, err := pack.VerifyPack(out, pack.VerifyOptions{Roots: bl.roots, Generator: "bench", Clock: time.Now})
		if err != nil || !r.OK() {
			t.Fatalf("%s: VerifyPack: %v, %v", sc.name, err, r.Errors)
		}
	}
}

func assertEntryTypes(t *testing.T, es []ledger.Entry, want ...string) {
	t.Helper()
	seen := map[string]bool{}
	for _, e := range es {
		seen[e.Type()] = true
	}
	for _, typ := range want {
		if !seen[typ] {
			t.Errorf("generated ledger has no %s entry", typ)
		}
	}
}
