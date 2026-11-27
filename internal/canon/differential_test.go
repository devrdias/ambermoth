package canon_test

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"testing"

	"github.com/devrdias/ambermoth/internal/canon"
)

// sameAsReference fails the test unless the package and the frozen reference
// writer agree on v: same bytes, same error or none, same error text.
func sameAsReference(t *testing.T, v any) {
	t.Helper()
	want, wantErr := referenceMarshal(v)
	got, gotErr := canon.Marshal(v)
	if (wantErr == nil) != (gotErr == nil) {
		t.Fatalf("error mismatch for %#v: reference %v, package %v", v, wantErr, gotErr)
	}
	if wantErr != nil && wantErr.Error() != gotErr.Error() {
		t.Fatalf("error text for %#v: reference %q, package %q", v, wantErr, gotErr)
	}
	if !bytes.Equal(want, got) {
		t.Fatalf("bytes for %#v:\nreference %q\npackage   %q", v, want, got)
	}
}

func TestMatchesReferenceOnGoldenInputs(t *testing.T) {
	t.Parallel()
	for _, c := range goldenCases() {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			sameAsReference(t, c.in)
		})
	}
}

func TestMatchesReferenceOnEveryByteAsAString(t *testing.T) {
	t.Parallel()
	for b := 0; b < 256; b++ {
		s := string([]byte{'a', byte(b), 'z'})
		sameAsReference(t, s)
		sameAsReference(t, map[string]any{s: s})
	}
}

// decodeAny decodes the first JSON value in data the way the ledger reads
// entries: numbers stay json.Number. ok is false when data holds no value.
func decodeAny(data []byte) (v any, ok bool) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil {
		return nil, false
	}
	return v, true
}

// compareFuzzInput builds values from the fuzz arguments and checks each one.
func compareFuzzInput(t *testing.T, data []byte, raw string) {
	t.Helper()
	if v, ok := decodeAny(data); ok {
		sameAsReference(t, v)
		sameAsReference(t, []any{v, map[string]any{"k": v}})
	}
	sameAsReference(t, raw)
	sameAsReference(t, json.Number(raw))
	sameAsReference(t, map[string]any{raw: raw, "n": json.Number(raw)})
	sameAsReference(t, []any{raw, json.Number(raw), nil, true})
	if len(data) >= 8 {
		n := int64(binary.LittleEndian.Uint64(data))
		sameAsReference(t, n)
		sameAsReference(t, int(n))
		sameAsReference(t, n>>11)
	}
}

func seedFuzz(f *testing.F) {
	for _, c := range goldenCases() {
		raw := ""
		switch in := c.in.(type) {
		case string:
			raw = in
		case json.Number:
			raw = string(in)
		}
		data, err := json.Marshal(c.in)
		if err != nil {
			data = nil
		}
		f.Add(data, raw)
	}
	for _, line := range corpusLines(f) {
		f.Add(line, "")
	}
}

func FuzzMarshalMatchesReference(f *testing.F) {
	seedFuzz(f)
	f.Fuzz(compareFuzzInput)
}
