package pack_test

import (
	"strings"
	"testing"

	"github.com/devrdias/ambermoth/internal/ledger"
	"github.com/devrdias/ambermoth/internal/pack"
	"github.com/devrdias/ambermoth/internal/report"
)

func codeLink(purpose, repo, commit, firstTs string) ledger.Entry {
	return ledger.Entry{"type": ledger.TypeCodeLink, "purpose": purpose, "repo": repo, "commit": commit, "first_commit_ts": firstTs}
}

type codeLinkCase struct {
	name      string
	links     []ledger.Entry
	wantLink  string
	wantSince string
}

func TestCodeLinkFor(t *testing.T) {
	const sha = "0123456789abcdef0123"
	cases := []codeLinkCase{
		{name: "has nothing without links"},
		{name: "cuts the commit to 12 characters and the day to 10",
			links:    []ledger.Entry{codeLink("eval", "git@host:r.git", sha, "2026-01-02T03:04:05.000Z")},
			wantLink: "git@host:r.git @ 0123456789ab", wantSince: "2026-01-02"},
		{name: "takes the last of several links",
			links: []ledger.Entry{codeLink("eval", "old", "aaaaaaaaaaaa", "2025-01-01T00:00:00Z"),
				codeLink("eval", "new", "bbbbbbbbbbbb", "2026-02-02T00:00:00Z")},
			wantLink: "new @ bbbbbbbbbbbb", wantSince: "2026-02-02"},
		{name: "drops the day of an earlier link when the last has none",
			links:    []ledger.Entry{codeLink("eval", "old", "aaaa", "2025-01-01T00:00:00Z"), codeLink("eval", "new", "bbbb", "")},
			wantLink: "new @ bbbb", wantSince: ""},
		{name: "ignores a link for another purpose",
			links: []ledger.Entry{codeLink("prod", "other", "cccc", "2025-01-01T00:00:00Z")}},
		{name: "keeps its own link when a later one is for another purpose",
			links:    []ledger.Entry{codeLink("eval", "mine", "aaaa", "2025-01-01T00:00:00Z"), codeLink("prod", "other", "cccc", "2026-01-01T00:00:00Z")},
			wantLink: "mine @ aaaa", wantSince: "2025-01-01"},
		{name: "keeps a short commit whole",
			links:    []ledger.Entry{codeLink("eval", "r", "abc", "2025-01-01")},
			wantLink: "r @ abc", wantSince: "2025-01-01"},
		{name: "keeps an empty commit",
			links:    []ledger.Entry{codeLink("eval", "r", "", "2025-01-01T00:00:00Z")},
			wantLink: "r @ ", wantSince: "2025-01-01"},
		{name: "ignores a day shorter than 10 characters",
			links:    []ledger.Entry{codeLink("eval", "r", "abcd", "2025-01")},
			wantLink: "r @ abcd", wantSince: ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			link, since := pack.CodeLinkFor(&report.Summary{CodeLinks: c.links}, "eval")
			if link != c.wantLink || since != c.wantSince {
				t.Errorf("got %q, %q; want %q, %q", link, since, c.wantLink, c.wantSince)
			}
		})
	}
}

func letterOf(t *testing.T, e *env, o pack.Options) string {
	t.Helper()
	e.build(o)
	return string(readPack(t, o.Out)["letter-template.md"])
}

const producedBy = "It is produced by "

func TestLetterNamesTheCodeOfTheLargestPurpose(t *testing.T) {
	e := newEnv(t)
	e.link(0, "eval", "git@host:r.git", "0123456789abcdef", "2026-01-02T00:00:00.000Z")
	e.link(0, "prod", "git@host:other.git", "fedcba9876543210", "2025-05-05T00:00:00.000Z")
	e.call(1, "anthropic", "eval")
	e.call(1, "anthropic", "eval")
	e.call(1, "anthropic", "prod")
	letter := letterOf(t, e, e.opts())
	want := producedBy + "git@host:r.git @ 0123456789ab, whose history begins 2026-01-02."
	if !strings.Contains(letter, want) || strings.Contains(letter, "other.git") {
		t.Errorf("letter lacks %q or names the smaller purpose's repo:\n%s", want, letter)
	}
	if !strings.Contains(letter, `The largest class, 67% of calls, is
"eval"`) {
		t.Errorf("letter does not name the largest purpose and its share:\n%s", letter)
	}
}

func TestLetterUsesTheLastLinkOfThePurpose(t *testing.T) {
	e := newEnv(t)
	e.link(0, "eval", "old", "aaaaaaaaaaaaaaaa", "2025-01-01T00:00:00.000Z")
	e.link(0, "eval", "new", "bbbbbbbbbbbbbbbb", "2026-02-02T00:00:00.000Z")
	e.call(1, "anthropic", "eval")
	letter := letterOf(t, e, e.opts())
	if !strings.Contains(letter, producedBy+"new @ bbbbbbbbbbbb, whose history begins 2026-02-02.") {
		t.Errorf("letter does not use the last link:\n%s", letter)
	}
}

func TestLetterSaysNothingOfCodeWithoutALinkForTheLargestPurpose(t *testing.T) {
	e := newEnv(t)
	e.link(0, "prod", "git@host:other.git", "fedcba9876543210", "2025-05-05T00:00:00.000Z")
	e.call(1, "anthropic", "eval")
	e.call(1, "anthropic", "eval")
	e.call(1, "anthropic", "prod")
	o := e.opts()
	letter := letterOf(t, e, o)
	if strings.Contains(letter, producedBy) {
		t.Errorf("letter names code for a purpose with no link:\n%s", letter)
	}
	if !strings.Contains(string(readPack(t, o.Out)["summary.html"]), "other.git") {
		t.Error("the summary should still list the link of the smaller purpose")
	}
}

func TestLetterPrintsNoValueWhenTheLinkHasNoFirstCommitDay(t *testing.T) {
	e := newEnv(t)
	e.link(0, "eval", "r", "abcdef", "")
	e.call(1, "anthropic", "eval")
	letter := letterOf(t, e, e.opts())
	if !strings.Contains(letter, producedBy+"r @ abcdef, whose history begins <no value>.") {
		t.Errorf("letter:\n%s", letter)
	}
}

func TestLetterAddressesTheFilteredProvider(t *testing.T) {
	e := newEnv(t)
	e.link(0, "eval", "r", "abcdef", "2026-01-01T00:00:00.000Z")
	e.call(1, "openai", "eval")
	o := e.opts()
	o.Provider = "openai"
	letter := letterOf(t, e, o)
	if !strings.HasPrefix(letter, "To the openai review team,") {
		t.Errorf("letter does not address the filtered provider:\n%s", letter)
	}
	o = e.opts()
	if letter := letterOf(t, e, o); !strings.HasPrefix(letter, "To the [provider] review team,") {
		t.Errorf("letter without a provider filter:\n%s", letter)
	}
}

func TestCodeLinkPlacementDecidesWhatTheLetterAndSummaryShow(t *testing.T) {
	type placement struct {
		name  string
		setup func(e *env)
		want  bool
	}
	cases := []placement{
		{"a link recorded after the calls it describes, inside the segment", func(e *env) {
			e.call(1, "anthropic", "eval")                    // 1
			e.link(2, "eval", "late", "abcdef", "2026-01-01") // 2
			e.anchor(2, 1)                                    // 3
		}, true},
		{"a link before the segment", func(e *env) {
			e.link(0, "eval", "early", "abcdef", "2026-01-01") // 1
			e.call(1, "anthropic", "eval")                     // 2
			e.anchor(1, 1)                                     // 3
			e.call(5, "anthropic", "eval")                     // 4
			e.anchor(5, 1)                                     // 5
		}, true},
		{"a link after the end of the segment", func(e *env) {
			e.call(1, "anthropic", "eval")                    // 1
			e.anchor(1, 1)                                    // 2
			e.link(2, "eval", "late", "abcdef", "2026-01-01") // 3
		}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t)
			c.setup(e)
			o := period(e.opts(), 1, 5)
			if strings.HasPrefix(c.name, "a link before") {
				o = period(e.opts(), 5, 5)
			}
			letter := letterOf(t, e, o)
			summary := string(readPack(t, o.Out)["summary.html"])
			if got := strings.Contains(letter, producedBy); got != c.want {
				t.Errorf("letter names the code: %v, want %v", got, c.want)
			}
			if got := !strings.Contains(summary, "No code links recorded"); got != c.want {
				t.Errorf("summary lists the link: %v, want %v", got, c.want)
			}
		})
	}
}
