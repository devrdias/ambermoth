package proxy

// Exported for the black-box tests in this directory.
const MaxNoteBytes = maxNoteBytes

// Exported for the black-box tests in this directory.
var TruncateUTF8 = truncateUTF8

// MaybeEnroll is exported for the black-box tests in this directory.
func (p *Proxy) MaybeEnroll(fp, upstream string) { p.maybeEnroll(fp, upstream) }
