package proxy_test

import (
	"github.com/devrdias/ambermoth/internal/ledger"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"
)

const (
	invalidPrefix = "invalid purpose header: "
	cutMarker     = "…[truncated]"
)

// callWith sends one call with the given headers and returns its only entry.
func callWith(t *testing.T, hdr map[string]string) ledger.Entry {
	t.Helper()
	r := newRig(t, jsonUpstream(), nil)
	hdr["Authorization"] = "Bearer sk-note"
	if status, _, err := r.send("POST", "/openai/v1/chat/completions", `{"model":"m"}`, hdr); status != 200 || err != nil {
		t.Fatalf("call was not passed through: status %d err %v", status, err)
	}
	es := r.entries()
	if len(es) != 1 {
		t.Fatalf("want 1 entry, got %d: the entry was lost", len(es))
	}
	return es[0]
}

// show shortens a long value for a failure message.
func show(s string) string {
	if len(s) <= 100 {
		return s
	}
	return s[:50] + " … " + s[len(s)-50:]
}

func checkField(t *testing.T, name, got string, limit int) {
	t.Helper()
	if len(got) > limit {
		t.Errorf("%s is %d bytes, limit %d", name, len(got), limit)
	}
	if !utf8.ValidString(got) {
		t.Errorf("%s is not valid UTF-8: %q", name, show(got))
	}
	if strings.ContainsRune(got, utf8.RuneError) {
		t.Errorf("%s holds a replacement character: %q", name, show(got))
	}
}

type invalidNoteCase struct {
	name   string
	value  string
	note   string
	prefix string // the note must start with this
	whole  bool   // the value must survive uncut
	suffix string // the note must end with this; the cut marker if empty
}

func TestInvalidPurposeNoteStaysWithinTheCap(t *testing.T) {
	a := func(n int) string { return strings.Repeat("a", n) }
	cases := []invalidNoteCase{
		{name: "1 byte", value: "a", prefix: invalidPrefix + "a", whole: true},
		{name: "fits exactly", value: a(488), prefix: invalidPrefix + a(488), whole: true},
		{name: "one byte over", value: a(489), prefix: invalidPrefix},
		{name: "511 bytes", value: a(511), prefix: invalidPrefix},
		{name: "512 bytes", value: a(512), prefix: invalidPrefix},
		{name: "513 bytes", value: a(513), prefix: invalidPrefix},
		{name: "2000 bytes", value: a(2000), prefix: invalidPrefix},
		{name: "two-byte characters", value: strings.Repeat("é", 1000), prefix: invalidPrefix},
		{name: "two-byte characters at the other parity", value: "a" + strings.Repeat("é", 1000), prefix: invalidPrefix},
		{name: "three-byte characters", value: strings.Repeat("€", 700), prefix: invalidPrefix},
		{name: "four-byte characters", value: "ab" + strings.Repeat("😀", 500), prefix: invalidPrefix},
		{name: "short user note", value: a(2000), note: "ctx", prefix: "ctx " + invalidPrefix},
		{name: "short user note and short value", value: "training", note: "ctx", prefix: "ctx " + invalidPrefix + "training", whole: true},
		{name: "long user note and long value", value: a(2000), note: strings.Repeat("n", 2000), prefix: "nnnn"},
		{name: "long user note and short value", value: "training", note: strings.Repeat("n", 2000), prefix: "nnnn", suffix: " " + invalidPrefix + "training"},
		{name: "multi-byte user note", value: a(2000), note: strings.Repeat("é", 1000), prefix: "éé"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hdr := map[string]string{"X-Provenance-Purpose": tc.value}
			if tc.note != "" {
				hdr["X-Provenance-Note"] = tc.note
			}
			e := callWith(t, hdr)
			note := e.Str("note")
			checkField(t, "note", note, 512)
			if e.Str("purpose") != "untagged" {
				t.Errorf("purpose = %q, want untagged", e.Str("purpose"))
			}
			if !strings.HasPrefix(note, tc.prefix) {
				t.Errorf("note = %q, want prefix %q", show(note), tc.prefix)
			}
			if !strings.Contains(note, invalidPrefix) {
				t.Errorf("the invalid-purpose text is missing from %q", show(note))
			}
			if tc.whole && strings.Contains(note, cutMarker) {
				t.Errorf("note was cut although it fits: %q", show(note))
			}
			if want := cmpSuffix(tc); !tc.whole && !strings.HasSuffix(note, want) {
				t.Errorf("note should end with %q: %q", want, show(note))
			}
		})
	}
}

func cmpSuffix(tc invalidNoteCase) string {
	if tc.suffix != "" {
		return tc.suffix
	}
	return cutMarker
}

func TestInvalidPurposeNoteSharesTheCap(t *testing.T) {
	long := strings.Repeat("n", 2000)
	e := callWith(t, map[string]string{"X-Provenance-Purpose": strings.Repeat("a", 2000), "X-Provenance-Note": long})
	note := e.Str("note")
	user, rest, found := strings.Cut(note, " "+invalidPrefix)
	if !found {
		t.Fatalf("no invalid-purpose text in %q", show(note))
	}
	if len(user) < 240 || len(rest) < 200 {
		t.Errorf("unfair split: user note %d bytes, purpose text %d bytes", len(user), len(rest))
	}
	e = callWith(t, map[string]string{"X-Provenance-Purpose": "training", "X-Provenance-Note": long})
	if note := e.Str("note"); !strings.HasSuffix(note, " "+invalidPrefix+"training") || len(note) != 512 {
		t.Errorf("a short purpose value should stay whole and the user note take the rest: %d bytes %q", len(note), show(note))
	}
}

func TestInvalidUTF8InHeadersNeverLosesTheEntry(t *testing.T) {
	cases := []struct {
		name string
		hdr  map[string]string
		want map[string]string
	}{
		{"purpose", map[string]string{"X-Provenance-Purpose": "bad\xff\xfe"}, map[string]string{"purpose": "untagged", "note": invalidPrefix + "bad�"}},
		{"purpose with a valid x- prefix", map[string]string{"X-Provenance-Purpose": "x-\xff"}, map[string]string{"purpose": "untagged", "note": invalidPrefix + "x-�"}},
		{"long purpose", map[string]string{"X-Provenance-Purpose": strings.Repeat("\xff", 2000)}, map[string]string{"purpose": "untagged", "note": invalidPrefix + "�"}},
		{"note", map[string]string{"X-Provenance-Note": "n\xff"}, map[string]string{"note": "n�"}},
		{"note after a valid purpose", map[string]string{"X-Provenance-Purpose": "other-declared", "X-Provenance-Note": "\xc3"}, map[string]string{"purpose": "other-declared", "note": "�"}},
		{"git ref", map[string]string{"X-Provenance-Git-Ref": "abc\xe2\x82"}, map[string]string{"git_ref": "abc�"}},
		{"purpose and note", map[string]string{"X-Provenance-Purpose": "\xff", "X-Provenance-Note": "\xfe"}, map[string]string{"note": "� " + invalidPrefix + "�"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := callWith(t, tc.hdr)
			for k, want := range tc.want {
				if got := e.Str(k); got != want {
					t.Errorf("%s = %q, want %q", k, got, want)
				}
			}
		})
	}
}

func TestOtherCharactersInHeadersAreKept(t *testing.T) {
	const note = "line\tone\u2028two\u0085three"
	e := callWith(t, map[string]string{"X-Provenance-Note": note, "X-Provenance-Git-Ref": "a\tb"})
	if e.Str("note") != note || e.Str("git_ref") != "a\tb" {
		t.Errorf("note = %q git_ref = %q", e.Str("note"), e.Str("git_ref"))
	}
}

func TestNoteAndGitRefAreCapped(t *testing.T) {
	sizes := []int{0, 1, 255, 256, 257, 511, 512, 513, 2000}
	units := []string{"a", "é", "€"}
	for _, n := range sizes {
		for _, unit := range units {
			name := strings.Repeat(unit, n)
			t.Run(unit+"/"+strconv.Itoa(n), func(t *testing.T) {
				e := callWith(t, map[string]string{"X-Provenance-Note": name, "X-Provenance-Git-Ref": name})
				checkField(t, "note", e.Str("note"), 512)
				checkField(t, "git_ref", e.Str("git_ref"), 256)
				if len(name) <= 256 && (e.Str("note") != name || e.Str("git_ref") != name) {
					t.Errorf("a value that fits was changed")
				}
				if len(name) > 512 && !strings.HasSuffix(e.Str("note"), cutMarker) {
					t.Errorf("cut note has no marker")
				}
			})
		}
	}
}

func TestPathWithInvalidUTF8KeepsTheEntry(t *testing.T) {
	r := newRig(t, jsonUpstream(), nil)
	if status, _, err := r.send("POST", "/openai/v1/%ff/x", `{}`, nil); status != 200 || err != nil {
		t.Fatalf("status %d err %v", status, err)
	}
	es := r.entries()
	if len(es) != 1 {
		t.Fatalf("want 1 entry, got %d: the entry was lost", len(es))
	}
	if p := es[0].Str("path"); !utf8.ValidString(p) || p != "/v1/\uFFFD/x" {
		t.Errorf("path = %q", p)
	}
}
