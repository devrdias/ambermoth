package ledger_test

import (
	"bytes"
	"strings"
	"testing"

	"github.com/devrdias/ambermoth/internal/ledger"
)

// verifyBytes runs Read and Verify over raw ledger bytes, the way verify does
// for a file somebody else sent.
func verifyBytes(t *testing.T, b []byte) *ledger.Report {
	t.Helper()
	es, lines, err := ledger.Read(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	return ledger.Verify(es, lines, ledger.VerifyOptions{})
}

func TestVerifyReportsLinesThatAreNotObjects(t *testing.T) {
	for _, c := range notObjectCases {
		for i := 0; i < 3; i++ {
			t.Run(c.line+" on line "+string(rune('1'+i)), func(t *testing.T) {
				_, orig := threeEntryStore(t)
				r := verifyBytes(t, replaceLine(orig, i, c.line))
				want := "line " + string(rune('1'+i)) + ": not valid JSON object"
				if r.OK() || !strings.Contains(strings.Join(r.Errors, "\n"), want) {
					t.Fatalf("want an error containing %q, got %v", want, r.Errors)
				}
			})
		}
	}
}

// Fields of the wrong JSON type read as their zero value; Verify must report
// the entry, not crash on it.
func TestVerifyEntriesWithWrongFieldTypes(t *testing.T) {
	lines := []string{
		`{"v":1,"seq":"a","ts":5,"type":[1],"prev_hash":{},"entry_hash":null}`,
		`{"v":1,"seq":1.5,"ts":"x","type":"anchor","target_seq":"z","target_hash":7,"token_file":[],"token_sha256":true,"entry_hash":"x"}`,
		`{"v":1,"seq":1,"ts":"2026-03-01T00:00:00.000Z","type":"anchor","target_seq":99999999999999999999,"token_file":null}`,
		`{"v":1,"seq":-5,"type":"gap","dropped":"many"}`,
	}
	for _, l := range lines {
		t.Run(l, func(t *testing.T) {
			r := verifyBytes(t, []byte(l+"\n"))
			if r.OK() {
				t.Fatal("a malformed entry verified")
			}
		})
	}
}
