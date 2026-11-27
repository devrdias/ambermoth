package proxy_test

import (
	"strings"
	"testing"

	"github.com/devrdias/ambermoth/internal/ledger"
	"github.com/devrdias/ambermoth/internal/proxy"
)

type purposeCase struct {
	name     string
	tune     func(*proxy.Config)
	path     string // after the upstream name
	upstream string // "" means openai
	header   *string
	note     *string
	want     string
	wantNote string
}

func str(s string) *string { return &s }

func purposeRoutes(rules ...proxy.RouteRule) func(*proxy.Config) {
	return func(c *proxy.Config) { c.Routes = rules }
}

func purposeConfig(def string, keyDefault string, rules ...proxy.RouteRule) func(*proxy.Config) {
	return func(c *proxy.Config) {
		c.DefaultPurpose = def
		c.Routes = rules
		if keyDefault != "" {
			c.KeyDefaults = map[string]string{ledger.KeyFingerprint("sk-purpose"): keyDefault}
		}
	}
}

func runPurposeCase(t *testing.T, tc purposeCase) {
	t.Helper()
	r := newRig(t, jsonUpstream(), tc.tune)
	hdr := map[string]string{"Authorization": "Bearer sk-purpose"}
	if tc.header != nil {
		hdr["X-Provenance-Purpose"] = *tc.header
	}
	if tc.note != nil {
		hdr["X-Provenance-Note"] = *tc.note
	}
	up := tc.upstream
	if up == "" {
		up = "openai"
	}
	status, _, err := r.send("POST", "/"+up+tc.path, `{"model":"m"}`, hdr)
	if status != 200 || err != nil {
		t.Fatalf("call was not passed through: status %d err %v", status, err)
	}
	es := r.entries()
	if len(es) != 1 {
		t.Fatalf("want 1 entry, got %d", len(es))
	}
	if got := es[0].Str("purpose"); got != tc.want {
		t.Errorf("purpose = %q, want %q", got, tc.want)
	}
	if got := es[0].Str("note"); got != tc.wantNote {
		t.Errorf("note = %q, want %q", got, tc.wantNote)
	}
}

func TestPurposeHeader(t *testing.T) {
	chat := proxy.RouteRule{Prefix: "/v1/chat", Purpose: "rag-query"}
	cases := []purposeCase{
		{name: "header wins over route key default and default", tune: purposeConfig("dev-experiment", "batch-job", chat),
			path: "/v1/chat/completions", header: str("eval-regression"), want: "eval-regression"},
		{name: "custom x- tag is kept", path: "/v1/chat/completions", header: str("x-nightly"), want: "x-nightly"},
		{name: "untagged header is valid", tune: purposeRoutes(chat), path: "/v1/chat/completions", header: str("untagged"), want: "untagged"},
		{name: "surrounding spaces are trimmed", path: "/v1/chat/completions", header: str("  eval-benchmark \t"), want: "eval-benchmark"},
		{name: "empty header falls through to the route", tune: purposeRoutes(chat), path: "/v1/chat/completions", header: str(""), want: "rag-query"},
		{name: "blank header falls through to the route", tune: purposeRoutes(chat), path: "/v1/chat/completions", header: str("   "), want: "rag-query"},
		{name: "x- tag of 64 bytes is valid", path: "/v1/chat/completions", header: str("x-" + strings.Repeat("a", 62)), want: "x-" + strings.Repeat("a", 62)},
		{name: "other-declared without a note gets a note", path: "/v1/chat/completions", header: str("other-declared"),
			want: "other-declared", wantNote: "other-declared without a note"},
		{name: "other-declared keeps the caller's note", path: "/v1/chat/completions", header: str("other-declared"), note: str("my reason"),
			want: "other-declared", wantNote: "my reason"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) { runPurposeCase(t, tc) })
	}
}

func TestPurposeInvalidHeaderIsRecordedNotRejected(t *testing.T) {
	chat := proxy.RouteRule{Prefix: "/v1/chat", Purpose: "rag-query"}
	long := "x-" + strings.Repeat("a", 63)
	cases := []purposeCase{
		{name: "unknown tag", path: "/v1/chat/completions", header: str("training"),
			want: "untagged", wantNote: "invalid purpose header: training"},
		{name: "invalid header does not fall through to the route", tune: purposeConfig("dev-experiment", "batch-job", chat),
			path: "/v1/chat/completions", header: str("training"), want: "untagged", wantNote: "invalid purpose header: training"},
		{name: "tags are case sensitive", path: "/v1/chat/completions", header: str("Eval-Regression"),
			want: "untagged", wantNote: "invalid purpose header: Eval-Regression"},
		{name: "bare x- prefix", path: "/v1/chat/completions", header: str("x-"),
			want: "untagged", wantNote: "invalid purpose header: x-"},
		{name: "x- tag of 65 bytes", path: "/v1/chat/completions", header: str(long),
			want: "untagged", wantNote: "invalid purpose header: " + long},
		{name: "appended to the caller's note", path: "/v1/chat/completions", header: str("training"), note: str("ctx"),
			want: "untagged", wantNote: "ctx invalid purpose header: training"},
		{name: "value with inner spaces", path: "/v1/chat/completions", header: str("a b"),
			want: "untagged", wantNote: "invalid purpose header: a b"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) { runPurposeCase(t, tc) })
	}
}

// The note is documented as capped at 512 bytes (SPEC §4.2), but the invalid
// value is appended after the cap, so a long value goes through whole.
func TestPurposeInvalidValueBypassesNoteCap(t *testing.T) {
	long := strings.Repeat("a", 2000)
	runPurposeCase(t, purposeCase{path: "/v1/chat/completions", header: str(long),
		want: "untagged", wantNote: "invalid purpose header: " + long})
}

func TestPurposeResolutionWithoutHeader(t *testing.T) {
	emb := proxy.RouteRule{Prefix: "/v1/embeddings", Purpose: "rag-indexing"}
	cases := []purposeCase{
		{name: "route wins over key default and default", tune: purposeConfig("dev-experiment", "batch-job", emb),
			path: "/v1/embeddings", want: "rag-indexing"},
		{name: "key default wins over default", tune: purposeConfig("dev-experiment", "batch-job", emb),
			path: "/v1/chat/completions", want: "batch-job"},
		{name: "default when nothing else applies", tune: purposeConfig("dev-experiment", "", emb),
			path: "/v1/chat/completions", want: "dev-experiment"},
		{name: "no rule and no default is untagged", tune: purposeConfig("", ""), path: "/v1/chat/completions", want: "untagged"},
		{name: "untouched defaults are untagged", path: "/v1/chat/completions", want: "untagged"},
		{name: "route prefix matches the start of the path", tune: purposeConfig("", "", proxy.RouteRule{Prefix: "/v1/emb", Purpose: "rag-indexing"}),
			path: "/v1/embeddings", want: "rag-indexing"},
		{name: "route prefix does not match mid path", tune: purposeConfig("", "", proxy.RouteRule{Prefix: "/embeddings", Purpose: "rag-indexing"}),
			path: "/v1/embeddings", want: "untagged"},
		{name: "route prefix is case sensitive", tune: purposeConfig("", "", proxy.RouteRule{Prefix: "/v1/Embeddings", Purpose: "rag-indexing"}),
			path: "/v1/embeddings", want: "untagged"},
		{name: "empty prefix matches every path", tune: purposeConfig("", "", proxy.RouteRule{Prefix: "", Purpose: "batch-job"}),
			path: "/v1/anything", want: "batch-job"},
		{name: "first matching rule wins", tune: purposeRoutes(
			proxy.RouteRule{Prefix: "/v1", Purpose: "dev-experiment"}, proxy.RouteRule{Prefix: "/v1/embeddings", Purpose: "rag-indexing"}),
			path: "/v1/embeddings", want: "dev-experiment"},
		{name: "rule for another upstream is skipped", tune: purposeRoutes(
			proxy.RouteRule{Upstream: "anthropic", Prefix: "/v1", Purpose: "batch-job"}),
			path: "/v1/embeddings", want: "untagged"},
		{name: "rule for this upstream matches", tune: purposeRoutes(
			proxy.RouteRule{Upstream: "anthropic", Prefix: "/v1", Purpose: "batch-job"}),
			upstream: "anthropic", path: "/v1/messages", want: "batch-job"},
		{name: "rule without upstream matches any", tune: purposeRoutes(proxy.RouteRule{Prefix: "/v1", Purpose: "batch-job"}),
			upstream: "anthropic", path: "/v1/messages", want: "batch-job"},
		{name: "a skipped rule leaves the next one", tune: purposeRoutes(
			proxy.RouteRule{Upstream: "anthropic", Prefix: "/v1", Purpose: "batch-job"},
			proxy.RouteRule{Prefix: "/v1", Purpose: "rag-query"}),
			path: "/v1/embeddings", want: "rag-query"},
		{name: "rule purpose is not validated", tune: purposeRoutes(proxy.RouteRule{Prefix: "/v1", Purpose: "bogus"}),
			path: "/v1/embeddings", want: "bogus"},
		{name: "default purpose is not validated", tune: purposeConfig("bogus", ""), path: "/v1/embeddings", want: "bogus"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) { runPurposeCase(t, tc) })
	}
}

// A call with no key has an empty fingerprint, which no key default matches.
func TestPurposeKeyDefaultNeedsAKey(t *testing.T) {
	r := newRig(t, jsonUpstream(), purposeConfig("dev-experiment", "batch-job"))
	if _, _, err := r.send("POST", "/openai/v1/embeddings", `{}`, nil); err != nil {
		t.Fatal(err)
	}
	es := r.entries()
	if len(es) != 1 || es[0].Str("purpose") != "dev-experiment" || es[0].Str("key_fp") != "" {
		t.Fatalf("unexpected entries: %v", es)
	}
}
