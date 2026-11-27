package canon_test

import (
	"math/rand"
	"testing"

	"github.com/devrdias/ambermoth/internal/canon"
)

func BenchmarkMarshalCallEntry(b *testing.B) {
	g := entryGen{rng: rand.New(rand.NewSource(7))}
	e := g.call(42)
	e["entry_hash"] = "sha256:" + g.hex(64)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := canon.Marshal(e); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkMarshalEscapedStrings(b *testing.B) {
	v := map[string]any{"note": "tab\tnewline\n \"quoted\" \x01 café 日本 <&>", "other": []any{"a\\b", "\x1f", "plain text"}}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := canon.Marshal(v); err != nil {
			b.Fatal(err)
		}
	}
}
