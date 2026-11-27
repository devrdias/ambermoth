package trust_test

import (
	"testing"

	"github.com/devrdias/ambermoth/internal/trust"
)

// The pinned roots are part of the trust model: a changed fingerprint must be a
// deliberate, reviewed change, so this test lists them explicitly.
func TestPinnedRootFingerprints(t *testing.T) {
	want := map[string]string{
		"digicert-assured-id-root-ca.pem": "3e9099b5015e8f486c00bcea9d111ee721faba355a89bcf1df69561e3dc6325c",
		"digicert-trusted-root-g4.pem":    "552f7bdcf1a7af9e6ce672017f4f12abf77240c78e761ac203d1d9d20ac89988",
		"freetsa-root.pem":                "a6379e7cecc05faa3cbf076013d745e327bbbaa38c0b9af22469d4701d18aabc",
	}
	roots, err := trust.Roots()
	if err != nil {
		t.Fatal(err)
	}
	if len(roots) != len(want) {
		t.Fatalf("want %d roots, got %d", len(want), len(roots))
	}
	for _, r := range roots {
		if want[r.File] != r.SHA256 {
			t.Errorf("%s: fingerprint %s, want %s", r.File, r.SHA256, want[r.File])
		}
	}
	if _, err := trust.Pool(nil); err != nil {
		t.Fatal(err)
	}
}
