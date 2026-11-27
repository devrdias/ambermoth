package canon_test

import (
	"fmt"
	"math/rand"
	"slices"
	"sort"
	"testing"

	"github.com/devrdias/ambermoth/internal/canon"
)

// keyRunes mixes ASCII, two-byte, BMP below and above the surrogate range,
// and supplementary-plane runes, so every UTF-16 ordering case shows up.
var keyRunes = []string{
	"a", "B", "~", "\x00", "é", "日", "퟿", "", "￿", "�",
	"\U00010000", "\U00010001", "\U0001f600", "\U0001f601", "\U0010ffff",
}

type lessCase struct {
	name string
	a, b string
}

func TestUTF16LessMatchesReferenceOnNamedCases(t *testing.T) {
	t.Parallel()
	cases := []lessCase{
		{"empty against empty", "", ""},
		{"empty against text", "", "a"},
		{"prefix is smaller", "ab", "abc"},
		{"ascii order", "abc", "abd"},
		{"supplementary sorts before BMP high", "\U00010000", ""},
		{"supplementary sorts before U+FFFF", "\U0001f600", "￿"},
		{"BMP below the surrogates sorts before supplementary", "퟿", "\U00010000"},
		{"supplementary against ASCII", "\U00010000", "z"},
		{"same high surrogate, different low", "\U0001f600", "\U0001f601"},
		{"different high surrogate", "\U00010000", "\U0001f600"},
		{"supplementary after a shared prefix", "a\U00010000", "a"},
		{"byte order and UTF-16 order disagree", "～", "\U00010000"},
		{"invalid byte against replacement rune", "\xff", "�"},
		{"invalid byte against ASCII", "\xff", "a"},
		{"truncated sequence", "\xf0\x9f", "\U0001f600"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			checkLessPair(t, c.a, c.b)
		})
	}
}

func checkLessPair(t *testing.T, a, b string) {
	t.Helper()
	if got, want := canon.UTF16Less(a, b), referenceUTF16Less(a, b); got != want {
		t.Fatalf("less(%q, %q) = %v, reference says %v", a, b, got, want)
	}
}

func TestUTF16LessMatchesReferenceOnEveryShortCombination(t *testing.T) {
	t.Parallel()
	words := []string{""}
	for _, x := range keyRunes {
		words = append(words, x)
		for _, y := range keyRunes {
			words = append(words, x+y)
		}
	}
	for _, a := range words {
		for _, b := range words {
			checkLessPair(t, a, b)
		}
	}
}

func TestUTF16LessMatchesReferenceOnEveryByte(t *testing.T) {
	t.Parallel()
	for x := 0; x < 256; x++ {
		a := string([]byte{'a', byte(x)})
		for y := 0; y < 256; y++ {
			checkLessPair(t, a, string([]byte{'a', byte(y)}))
			checkLessPair(t, a, string([]byte{byte(y), 'a'}))
		}
	}
}

func FuzzUTF16LessMatchesReference(f *testing.F) {
	for _, r := range keyRunes {
		f.Add(r, "a"+r)
	}
	f.Add("\xff", "�")
	f.Fuzz(func(t *testing.T, a, b string) {
		checkLessPair(t, a, b)
		checkLessPair(t, b, a)
	})
}

// referenceKeys orders keys the way the frozen writer does.
func referenceKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return referenceUTF16Less(keys[i], keys[j]) })
	return keys
}

func randomKey(rng *rand.Rand, pool []string) string {
	n := 1 + rng.Intn(4)
	k := ""
	for i := 0; i < n; i++ {
		k += pool[rng.Intn(len(pool))]
	}
	return k
}

func TestSortedKeysMatchesReference(t *testing.T) {
	t.Parallel()
	ascii := []string{"a", "b", "z", "A", "_", "0", "9", "-", "."}
	pools := map[string][]string{"ascii only": ascii, "mixed planes": keyRunes}
	for name, pool := range pools {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			rng := rand.New(rand.NewSource(11))
			for round := 0; round < 300; round++ {
				m := map[string]any{}
				for i, n := 0, rng.Intn(40); i < n; i++ {
					m[randomKey(rng, pool)] = i
				}
				got, want := canon.SortedKeys(m), referenceKeys(m)
				if !slices.Equal(got, want) {
					t.Fatalf("keys of %d-entry map:\ngot  %q\nwant %q", len(m), got, want)
				}
			}
		})
	}
}

func TestSortedKeysSingleNonASCIIKeyAmongASCII(t *testing.T) {
	t.Parallel()
	m := map[string]any{"b": 1, "a": 2, "\U0001f600": 3, "￿": 4, "z": 5}
	want := []string{"a", "b", "z", "\U0001f600", "￿"}
	if got := canon.SortedKeys(m); !slices.Equal(got, want) {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func BenchmarkSortedKeys(b *testing.B) {
	g := entryGen{rng: rand.New(rand.NewSource(7))}
	call := g.call(42)
	call["entry_hash"] = "sha256:" + g.hex(64)
	nonASCII := map[string]any{}
	for i := 0; i < 20; i++ {
		nonASCII[fmt.Sprintf("campo_%02d_é", i)] = i
	}
	nonASCII["\U0001f600"] = 1
	cases := map[string]map[string]any{"entry": call, "non-ASCII": nonASCII}
	for name, m := range cases {
		b.Run(name, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				canon.SortedKeys(m)
			}
		})
	}
}
