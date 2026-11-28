package proxy_test

import (
	"testing"

	"github.com/devrdias/ambermoth/internal/proxy"
)

const maxCount = 1<<53 - 1

type sseResult struct {
	in, out, cached int64
	reported        bool
	model           string
}

func sseOf(chunks ...string) sseResult {
	in, out, cached, reported, model := proxy.SSEUsage(chunks...)
	return sseResult{in, out, cached, reported, model}
}

// FuzzSSEUsage checks what a client or an upstream can do to the stream parser
// by the way the bytes arrive and by what they hold: it never panics, it gives
// the same answer however the stream is cut into reads, counts stay inside the
// range an entry can hold, and more stream never lowers a count.
func FuzzSSEUsage(f *testing.F) {
	f.Add([]byte(sseStart+sseUsage+sseDone), []byte(sseUsage), uint16(7), uint16(90))
	f.Add([]byte(sseUsage), []byte("data: {\"usage\":{\"prompt_tokens\":2,\"completion_tokens\":1}}\n"), uint16(1), uint16(2))
	f.Add([]byte(": ping\nevent: message_start\n"+sseUsage), []byte("data: {\"usage\":{\"output_tokens\":99}}\n"), uint16(3), uint16(3))
	f.Add([]byte("data: {\"message\":{\"model\":\"m\",\"usage\":{\"input_tokens\":4}}}\r\n\r\n"), []byte("data: [DONE]"), uint16(60), uint16(1))
	f.Add([]byte(`data: {"usage":{"prompt_tokens":9007199254740993,"completion_tokens":-1}}`+"\n"), []byte{}, uint16(0), uint16(0))
	f.Add([]byte(`data: {"response":{"model":"r","usage":{"input_tokens_details":{"cached_tokens":2}}}}`), []byte("data:"), uint16(40), uint16(10))
	f.Add([]byte("data: [1,2]\ndata: {not json\ndata:\n\n\xff\xfe"), []byte("\r"), uint16(5), uint16(500))
	f.Add([]byte{}, []byte{}, uint16(0), uint16(0))
	f.Fuzz(func(t *testing.T, data, extra []byte, a, b uint16) {
		if len(data) > proxy.MaxSSELine || len(extra) > proxy.MaxSSELine {
			t.Skip("a line over the limit is dropped only when it arrives in pieces")
		}
		whole := sseOf(string(data))
		i, j := cut(len(data), a, b)
		if got := sseOf(string(data[:i]), string(data[i:j]), string(data[j:])); got != whole {
			t.Fatalf("cut at %d and %d changed the result: %+v, whole %+v", i, j, got, whole)
		}
		var bytewise []string
		for k := range data {
			bytewise = append(bytewise, string(data[k:k+1]))
		}
		if got := sseOf(bytewise...); got != whole {
			t.Fatalf("one byte at a time changed the result: %+v, whole %+v", got, whole)
		}
		checkSSERange(t, whole)
		more := sseOf(string(data) + "\n" + string(extra))
		if more.in < whole.in || more.out < whole.out || more.cached < whole.cached || (whole.reported && !more.reported) {
			t.Fatalf("more stream lowered the counts: %+v, before %+v", more, whole)
		}
		if whole.model != "" && more.model != whole.model {
			t.Fatalf("more stream changed the model: %q, before %q", more.model, whole.model)
		}
	})
}

// cut turns two numbers into ordered cut points in [0, n].
func cut(n int, a, b uint16) (int, int) {
	i, j := int(a)%(n+1), int(b)%(n+1)
	if i > j {
		i, j = j, i
	}
	return i, j
}

func checkSSERange(t *testing.T, r sseResult) {
	t.Helper()
	counts := []int64{r.in, r.out, r.cached}
	for _, n := range counts {
		if n < 0 || n > maxCount {
			t.Fatalf("count %d is outside what an entry can hold", n)
		}
	}
	if !r.reported && (r.in != 0 || r.out != 0 || r.cached != 0) {
		t.Fatalf("counts %+v without a usage object", r)
	}
}
