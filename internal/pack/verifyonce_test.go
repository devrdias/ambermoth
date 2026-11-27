package pack_test

import (
	"testing"

	"github.com/devrdias/ambermoth/internal/pack"
)

// verifiedSizes builds a pack from o and returns how many entries each ledger
// verification inside Build covered, and the ledger's length.
func verifiedSizes(t *testing.T, e *env, o pack.Options) (sizes []int, total int) {
	t.Helper()
	es, err := e.s.Entries()
	if err != nil {
		t.Fatal(err)
	}
	e.build(pack.CountVerify(o, &sizes))
	return sizes, len(es)
}

func TestBuildVerifiesAWholeLedgerOnce(t *testing.T) {
	t.Parallel()
	cases := map[string]func(e *env){
		"unanchored tail after an anchor": func(e *env) {
			e.keyEvent(0)
			e.call(1, "anthropic", "a")
			e.call(2, "anthropic", "b")
			e.anchor(2, 1)
			e.gap(3)
			e.call(4, "anthropic", "tail")
		},
		"no anchors": func(e *env) {
			e.keyEvent(0)
			e.call(1, "anthropic", "a")
			e.call(2, "anthropic", "b")
		},
	}
	for name, fill := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t)
			fill(e)
			sizes, total := verifiedSizes(t, e, e.opts())
			if len(sizes) != 1 || sizes[0] != total {
				t.Fatalf("verifications covered %v entries, want one over all %d", sizes, total)
			}
		})
	}
}

func TestBuildVerifiesAPartialSegmentOnItsOwn(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.keyEvent(0)
	e.call(1, "anthropic", "a")
	e.anchor(2, 1)
	e.call(10, "anthropic", "b")
	e.anchor(11, 1)
	e.call(20, "anthropic", "c")
	e.anchor(21, 1)
	sizes, total := verifiedSizes(t, e, period(e.opts(), 10, 12))
	if len(sizes) != 2 || sizes[0] != total || sizes[1] >= total {
		t.Fatalf("verifications covered %v entries, want all %d and then a shorter segment", sizes, total)
	}
}
