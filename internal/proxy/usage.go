package proxy

import (
	"bytes"
	"encoding/json"
	"regexp"
	"strings"
)

// usage accumulates token counts parsed from a response (SPEC §6.3).
type usage struct {
	In, Out, Cached int64
	Reported        bool
	Model           string
}

func num(m map[string]any, k string) (int64, bool) {
	f, ok := m[k].(float64)
	return int64(f), ok
}

func sub(m map[string]any, k string) map[string]any {
	s, _ := m[k].(map[string]any)
	return s
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

// absorbUsage reads a provider "usage" object in any supported dialect.
func (u *usage) absorbUsage(m map[string]any) {
	if m == nil {
		return
	}
	u.Reported = true
	u.In = largest(u.In, m, "prompt_tokens", "input_tokens")
	u.Out = largest(u.Out, m, "completion_tokens", "output_tokens")
	u.Cached = largest(u.Cached, m, "cache_read_input_tokens")
	u.Cached = largest(u.Cached, sub(m, "prompt_tokens_details"), "cached_tokens")
	u.Cached = largest(u.Cached, sub(m, "input_tokens_details"), "cached_tokens")
}

// largest returns the largest of cur and the numeric values of keys in m.
// Streams repeat usage with growing totals, so the largest value wins.
func largest(cur int64, m map[string]any, keys ...string) int64 {
	for _, k := range keys {
		if v, ok := num(m, k); ok {
			cur = max64(cur, v)
		}
	}
	return cur
}

// absorbObject handles a full JSON response body or one SSE data payload.
func (u *usage) absorbObject(m map[string]any) {
	if u.Model == "" {
		if s, ok := m["model"].(string); ok {
			u.Model = s
		}
	}
	u.absorbUsage(sub(m, "usage"))
	if msg := sub(m, "message"); msg != nil { // anthropic message_start
		if u.Model == "" {
			u.Model, _ = msg["model"].(string)
		}
		u.absorbUsage(sub(msg, "usage"))
	}
	if resp := sub(m, "response"); resp != nil { // openai responses API events
		if u.Model == "" {
			u.Model, _ = resp["model"].(string)
		}
		u.absorbUsage(sub(resp, "usage"))
	}
}

func (u *usage) absorbJSON(b []byte) {
	var m map[string]any
	if json.Unmarshal(b, &m) == nil {
		u.absorbObject(m)
	}
}

// sseParser incrementally extracts usage from a text/event-stream.
type sseParser struct {
	u        *usage
	line     []byte
	skipping bool
}

const maxSSELine = 1 << 20

func (p *sseParser) Write(b []byte) {
	for len(b) > 0 {
		i := bytes.IndexByte(b, '\n')
		if i < 0 {
			p.partial(b)
			return
		}
		if !p.skipping {
			p.line = append(p.line, b[:i]...)
			p.handle(p.line)
		}
		p.line, p.skipping = p.line[:0], false
		b = b[i+1:]
	}
}

// partial buffers the start of a line, and drops a line longer than
// maxSSELine (it cannot hold usage) up to its newline.
func (p *sseParser) partial(b []byte) {
	if p.skipping {
		return
	}
	p.line = append(p.line, b...)
	if len(p.line) > maxSSELine {
		p.line, p.skipping = p.line[:0], true
	}
}

func (p *sseParser) Flush() {
	if len(p.line) > 0 && !p.skipping {
		p.handle(p.line)
	}
	p.line = p.line[:0]
}

func (p *sseParser) handle(line []byte) {
	line = bytes.TrimRight(line, "\r")
	if !bytes.HasPrefix(line, []byte("data:")) {
		return
	}
	data := bytes.TrimSpace(line[5:])
	if len(data) == 0 || bytes.Equal(data, []byte("[DONE]")) {
		return
	}
	p.u.absorbJSON(data)
}

// requestInfo is what the proxy needs from the request body.
type requestInfo struct {
	Model  string `json:"model"`
	Stream bool   `json:"stream"`
}

var (
	modelRe  = regexp.MustCompile(`"model"\s*:\s*"([^"]{1,200})"`)
	streamRe = regexp.MustCompile(`"stream"\s*:\s*true`)
)

func parseRequest(prefix []byte, complete bool) requestInfo {
	var ri requestInfo
	if complete && json.Unmarshal(prefix, &ri) == nil {
		return ri
	}
	if m := modelRe.FindSubmatch(prefix); m != nil {
		ri.Model = string(m[1])
	}
	ri.Stream = streamRe.Match(prefix)
	return ri
}

// endpointClass normalises an upstream path (SPEC §4.2).
func endpointClass(path string) string {
	p := strings.ToLower(path)
	switch {
	case strings.Contains(p, "/realtime"):
		return "realtime"
	case strings.Contains(p, "/chat/completions"):
		return "chat.completions"
	case strings.Contains(p, "/responses"):
		return "responses"
	case strings.Contains(p, "/completions"):
		return "completions"
	case strings.Contains(p, "/embeddings"):
		return "embeddings"
	case strings.Contains(p, "/messages/count_tokens"):
		return "messages.count_tokens"
	case strings.Contains(p, "/batches"):
		return "batches"
	case strings.Contains(p, "/messages"):
		return "messages"
	case strings.Contains(p, "/files"):
		return "files"
	case strings.Contains(p, "/models"):
		return "models"
	}
	return "other"
}
