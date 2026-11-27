package proxy_test

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/devrdias/ambermoth/internal/proxy"
)

func bodyUpstream(body string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, body)
	})
}

type usageCase struct {
	name         string
	body         string
	wantIn       int64
	wantOut      int64
	wantReported bool
	wantNote     bool
}

var usageCases = []usageCase{
	{name: "the largest count that can be written", body: `{"usage":{"prompt_tokens":9007199254740991,"completion_tokens":2}}`,
		wantIn: 9007199254740991, wantOut: 2, wantReported: true},
	{name: "a count past 2^53 is dropped with a note", body: `{"usage":{"prompt_tokens":9007199254740992,"completion_tokens":2}}`,
		wantIn: 0, wantOut: 2, wantReported: true, wantNote: true},
	{name: "an exponent form far out of range", body: `{"usage":{"prompt_tokens":1e300,"completion_tokens":1e16}}`,
		wantIn: 0, wantOut: 0, wantReported: true, wantNote: true},
	{name: "a negative count is ignored", body: `{"usage":{"prompt_tokens":-5,"completion_tokens":2}}`,
		wantIn: 0, wantOut: 2, wantReported: true},
	{name: "a fraction is cut to a whole number", body: `{"usage":{"prompt_tokens":12.7,"completion_tokens":2}}`,
		wantIn: 12, wantOut: 2, wantReported: true},
	{name: "a cached count out of range", body: `{"usage":{"prompt_tokens":3,"completion_tokens":2,"cache_read_input_tokens":1e20}}`,
		wantIn: 3, wantOut: 2, wantReported: true, wantNote: true},
}

func TestUsageOutOfRangeNeverMakesAnUnwritableEntry(t *testing.T) {
	for _, c := range usageCases {
		t.Run(c.name, func(t *testing.T) {
			r := newRig(t, bodyUpstream(c.body), nil)
			_, _, _ = r.send("POST", "/openai/v1/chat/completions", `{"model":"m"}`, map[string]string{"X-Provenance-Note": "mine"})
			_, _, _ = r.send("POST", "/openai/v1/chat/completions", `{"model":"m"}`, nil)
			cs := r.entries()
			if len(cs) != 2 {
				t.Fatalf("%d call entries, want 2", len(cs))
			}
			for _, e := range cs {
				if e.Int("tokens_in") != c.wantIn || e.Int("tokens_out") != c.wantOut || e.Bool("usage_reported") != c.wantReported {
					t.Errorf("in/out/reported = %d/%d/%v, want %d/%d/%v", e.Int("tokens_in"), e.Int("tokens_out"),
						e.Bool("usage_reported"), c.wantIn, c.wantOut, c.wantReported)
				}
				if got := strings.Contains(e.Str("note"), "usage count out of range"); got != c.wantNote {
					t.Errorf("note %q: out-of-range note = %v, want %v", e.Str("note"), got, c.wantNote)
				}
			}
			if c.wantNote && !strings.HasPrefix(cs[0].Str("note"), "mine ") {
				t.Errorf("caller's note lost: %q", cs[0].Str("note"))
			}
		})
	}
}

func TestSSEUsageOutOfRangeIsDropped(t *testing.T) {
	in, out, _, reported, _ := proxy.SSEUsage(`data: {"usage":{"prompt_tokens":1e17,"completion_tokens":7}}` + "\n")
	if in != 0 || out != 7 || !reported {
		t.Errorf("in/out/reported = %d/%d/%v, want 0/7/true", in, out, reported)
	}
}
