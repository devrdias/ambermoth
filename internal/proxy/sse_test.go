package proxy_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/devrdias/ambermoth/internal/proxy"
)

const (
	sseStart = `data: {"model":"mödel-✓","usage":{"prompt_tokens":11,"prompt_tokens_details":{"cached_tokens":3}}}` + "\n\n"
	sseUsage = `data: {"choices":[],"usage":{"prompt_tokens":11,"completion_tokens":7}}` + "\n\n"
	sseDone  = "data: [DONE]\n\n"
)

type sseCase struct {
	name     string
	chunks   []string
	in, out  int64
	cached   int64
	reported bool
	model    string
}

func TestSSEParserBoundaries(t *testing.T) {
	whole := sseStart + sseUsage + sseDone
	cases := []sseCase{
		{name: "whole stream in one write", chunks: []string{whole}, in: 11, out: 7, cached: 3, reported: true, model: "mödel-✓"},
		{name: "cut in the middle of an event", chunks: []string{sseStart + `data: {"choices":[],"usa`, `ge":{"prompt_tokens":11,"completion_tokens":7}}` + "\n\n"},
			in: 11, out: 7, cached: 3, reported: true, model: "mödel-✓"},
		{name: "cut in the middle of a line prefix", chunks: []string{sseStart + "da", "ta: " + `{"usage":{"completion_tokens":9}}` + "\n"},
			in: 11, out: 9, cached: 3, reported: true, model: "mödel-✓"},
		{name: "cut between the newlines of an event", chunks: []string{sseStart + sseUsage[:len(sseUsage)-1], "\n" + sseDone},
			in: 11, out: 7, cached: 3, reported: true, model: "mödel-✓"},
		{name: "cut in the middle of a utf-8 sequence", chunks: []string{sseStart[:len(`data: {"model":"m`)+1], sseStart[len(`data: {"model":"m`)+1:] + sseUsage},
			in: 11, out: 7, cached: 3, reported: true, model: "mödel-✓"},
		{name: "final event without the trailing blank line", chunks: []string{sseStart + strings.TrimSuffix(sseUsage, "\n\n") + "\n"},
			in: 11, out: 7, cached: 3, reported: true, model: "mödel-✓"},
		{name: "final event without any newline", chunks: []string{sseStart + strings.TrimSuffix(sseUsage, "\n\n")},
			in: 11, out: 7, cached: 3, reported: true, model: "mödel-✓"},
		{name: "final event cut in the middle of the json", chunks: []string{sseStart + `data: {"usage":{"completion_tokens":7`},
			in: 11, out: 0, cached: 3, reported: true, model: "mödel-✓"},
		{name: "done marker is ignored", chunks: []string{sseDone}},
		{name: "done marker without a space and with trailing blanks", chunks: []string{"data:[DONE]  \n\n", "data: [DONE]\r\n"}},
		{name: "stream ends before any usage", chunks: []string{`data: {"choices":[{"delta":{"content":"Hi"}}]}` + "\n\n"}},
		{name: "empty stream"},
		{name: "model without usage", chunks: []string{`data: {"model":"only-model"}` + "\n\n"}, model: "only-model"},
		{name: "crlf line endings", chunks: []string{strings.ReplaceAll(sseStart+sseUsage+sseDone, "\n", "\r\n")},
			in: 11, out: 7, cached: 3, reported: true, model: "mödel-✓"},
		{name: "comments and other fields are ignored", chunks: []string{": ping\nevent: message_start\nid: 1\n" + sseUsage},
			in: 11, out: 7, reported: true},
		{name: "data field without a space", chunks: []string{`data:{"usage":{"prompt_tokens":5}}` + "\n"}, in: 5, reported: true},
		{name: "invalid json is skipped", chunks: []string{"data: {not json\n\n" + sseUsage}, in: 11, out: 7, reported: true},
		{name: "json that is not an object is skipped", chunks: []string{"data: [1,2]\n\n", "data: 5\n\n", "data: \"x\"\n\n"}},
		{name: "largest total wins", chunks: []string{sseUsage, `data: {"usage":{"prompt_tokens":2,"completion_tokens":1}}` + "\n"},
			in: 11, out: 7, reported: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) { checkSSE(t, tc, tc.chunks...) })
	}
}

func checkSSE(t *testing.T, tc sseCase, chunks ...string) {
	t.Helper()
	in, out, cached, reported, model := proxy.SSEUsage(chunks...)
	if in != tc.in || out != tc.out || cached != tc.cached || reported != tc.reported || model != tc.model {
		t.Errorf("got in=%d out=%d cached=%d reported=%v model=%q; want in=%d out=%d cached=%d reported=%v model=%q",
			in, out, cached, reported, model, tc.in, tc.out, tc.cached, tc.reported, tc.model)
	}
}

// Splitting the stream at any byte, utf-8 sequences included, gives the same result.
func TestSSEParserSplitAtEveryByte(t *testing.T) {
	whole := sseStart + sseUsage + sseDone
	want := sseCase{in: 11, out: 7, cached: 3, reported: true, model: "mödel-✓"}
	for i := 0; i <= len(whole); i++ {
		checkSSE(t, want, whole[:i], whole[i:])
	}
	for i := 0; i < len(whole); i++ {
		for j := i; j <= len(whole); j++ {
			if i%7 == 0 && j%5 == 0 {
				checkSSE(t, want, whole[:i], whole[i:j], whole[j:])
			}
		}
	}
	var bytewise []string
	for i := 0; i < len(whole); i++ {
		bytewise = append(bytewise, whole[i:i+1])
	}
	checkSSE(t, want, bytewise...)
}

func bigSSELine(total int) string {
	const head, tail = `data: {"pad":"`, `","usage":{"prompt_tokens":999}}`
	return head + strings.Repeat("a", total-len(head)-len(tail)) + tail
}

// A buffered line over maxSSELine is dropped up to its newline; the parser then carries on.
func TestSSEParserDropsOversizedLine(t *testing.T) {
	max := proxy.MaxSSELine
	cases := []sseCase{
		{name: "line of exactly the limit is kept", chunks: []string{bigSSELine(max)[:max-5], bigSSELine(max)[max-5:] + "\n"}, in: 999, reported: true},
		{name: "line one byte over the limit is dropped", chunks: []string{bigSSELine(max + 1), "\n"}},
		{name: "oversized line then an event", chunks: []string{bigSSELine(max + 10), "\n" + sseUsage}, in: 11, out: 7, reported: true},
		// The size check runs only on text buffered without a newline, so a write that
		// carries the newline is parsed however long the line has become.
		{name: "oversized line closed by the write that carries its newline is parsed", chunks: []string{bigSSELine(max + 10)[:max-5], bigSSELine(max + 10)[max-5:] + "\n"},
			in: 999, reported: true},
		{name: "oversized line inside one write is parsed", chunks: []string{bigSSELine(max+10) + "\n"}, in: 999, reported: true},
		{name: "oversized line over several writes", chunks: []string{bigSSELine(2*max + 100)[:max/2+1], strings.Repeat("a", max), "aaa\n" + sseUsage},
			in: 11, out: 7, reported: true},
		{name: "skipped line continues in writes without a newline", chunks: []string{bigSSELine(max + 10)[:max-1], "aaaaaa", "aaaaaa", "bbbb\n", sseUsage},
			in: 11, out: 7, reported: true},
		{name: "oversized line at the end of the stream", chunks: []string{sseUsage, bigSSELine(max + 10)}, in: 11, out: 7, reported: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) { checkSSE(t, tc, tc.chunks...) })
	}
}

// sseUpstream writes each chunk and flushes, then optionally aborts the response mid-body.
func sseUpstream(abort bool, chunks ...string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, c := range chunks {
			_, _ = w.Write([]byte(c))
			w.(http.Flusher).Flush()
		}
		if abort {
			panic(http.ErrAbortHandler)
		}
	})
}

type sseStreamCase struct {
	name     string
	abort    bool
	chunks   []string
	in, out  int64
	reported bool
	readErr  bool
	aborted  bool
}

func TestSSEStreamThroughProxy(t *testing.T) {
	cut := `data: {"choices":[],"usa`
	cases := []sseStreamCase{
		{name: "events split inside lines and utf-8", chunks: []string{sseStart[:20], sseStart[20:60], sseStart[60:] + sseUsage[:10], sseUsage[10:] + sseDone},
			in: 11, out: 7, reported: true},
		{name: "final event without the trailing blank line", chunks: []string{sseStart + strings.TrimSuffix(sseUsage, "\n\n")},
			in: 11, out: 7, reported: true},
		{name: "stream ends before any usage", chunks: []string{`data: {"choices":[{"delta":{"content":"Hi"}}]}` + "\n\n", sseDone}},
		{name: "upstream cut in the middle of an event", abort: true, chunks: []string{sseStart + cut},
			in: 11, reported: true, readErr: true, aborted: true},
		{name: "upstream cut before any usage", abort: true, chunks: []string{`data: {"choices":[{"delta":{"content":"H`},
			readErr: true, aborted: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) { runSSEStream(t, tc) })
	}
}

func runSSEStream(t *testing.T, tc sseStreamCase) {
	t.Helper()
	r := newRig(t, sseUpstream(tc.abort, tc.chunks...), nil)
	_, got, err := r.send("POST", "/openai/v1/chat/completions", `{"model":"m","stream":true}`, nil)
	sent := strings.Join(tc.chunks, "")
	if got != sent {
		t.Errorf("client received %q, want %q", got, sent)
	}
	if (err != nil) != tc.readErr {
		t.Errorf("client read error = %v, want error %v", err, tc.readErr)
	}
	es := r.entries()
	if len(es) != 1 {
		t.Fatalf("want 1 entry, got %d", len(es))
	}
	e := es[0]
	if e.Int("tokens_in") != tc.in || e.Int("tokens_out") != tc.out || e.Bool("usage_reported") != tc.reported {
		t.Errorf("usage in=%d out=%d reported=%v, want in=%d out=%d reported=%v",
			e.Int("tokens_in"), e.Int("tokens_out"), e.Bool("usage_reported"), tc.in, tc.out, tc.reported)
	}
	if e.Bool("client_aborted") != tc.aborted || !e.Bool("stream") || e.Int("bytes_out") != int64(len(sent)) {
		t.Errorf("client_aborted=%v stream=%v bytes_out=%d, want %v true %d",
			e.Bool("client_aborted"), e.Bool("stream"), e.Int("bytes_out"), tc.aborted, len(sent))
	}
}
