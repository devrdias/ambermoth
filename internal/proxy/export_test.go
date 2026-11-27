package proxy

// Exported for the black-box tests in this directory.
const MaxNoteBytes = maxNoteBytes

// Exported for the black-box tests in this directory.
var TruncateUTF8 = truncateUTF8

// MaybeEnroll is exported for the black-box tests in this directory.
func (p *Proxy) MaybeEnroll(fp, upstream string) { p.maybeEnroll(fp, upstream) }

// Exported for the black-box tests in this directory.
const MaxSSELine = maxSSELine

// SSEUsage feeds chunks to the SSE parser, flushes it, and returns what it extracted.
func SSEUsage(chunks ...string) (in, out, cached int64, reported bool, model string) {
	var u usage
	p := &sseParser{u: &u}
	for _, c := range chunks {
		p.Write([]byte(c))
	}
	p.Flush()
	return u.In, u.Out, u.Cached, u.Reported, u.Model
}
