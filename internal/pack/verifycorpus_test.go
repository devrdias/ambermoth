package pack_test

import (
	"archive/zip"
	"bytes"
	"compress/flate"
	"crypto/x509"
	"encoding/json"
	"hash/crc32"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/devrdias/ambermoth/internal/ledger"
	"github.com/devrdias/ambermoth/internal/pack"
)

// The corpus test runs VerifyPack on honest packs built from the stored ledgers
// and on damaged copies of them, and compares the whole PackReport, or the
// error, with testdata/verify-corpus/reports.json. A change to how a pack is
// verified must not change a byte of any report.
//
//	go test ./internal/pack -run TestVerifyPackCorpus -update

const corpusFile = "testdata/verify-corpus/reports.json"

var corpusClock = func() time.Time { return day0.AddDate(0, 0, 41) }

// corpusBases are the golden cases that damaged packs are made from.
const (
	baseMid     = "rich both ends anchored with disclosure" // context entry, anchors at both ends, one disclosure
	baseBare    = "plain whole"                             // from genesis, no anchors
	baseGenesis = "rich first days, end anchored"           // from genesis, anchored end
	baseTail    = "rich unanchored end"                     // context entries, anchors, unanchored tail
	baseCRLF    = "crlf whole"
)

// corpusDraft is a pack taken apart for editing. names keeps the zip order.
type corpusDraft struct {
	t     *testing.T
	names []string
	files map[string][]byte
	m     *pack.Manifest
	// rawManifest keeps manifest.json as the edit left it instead of writing m
	rawManifest bool
}

func newCorpusDraft(t *testing.T, path string) *corpusDraft {
	t.Helper()
	d := &corpusDraft{t: t, names: zipNames(t, path), files: readPack(t, path), m: &pack.Manifest{}}
	if err := json.Unmarshal(d.files["manifest.json"], d.m); err != nil {
		t.Fatal(err)
	}
	return d
}

const (
	segName = "ledger/segment.jsonl"
	ctxName = "ledger/context.jsonl"
	keyName = "disclosures/entry-keys.json"
)

// lines is the segment split after each line end, so the lines join back to the file.
func (d *corpusDraft) lines() [][]byte {
	ls := bytes.SplitAfter(d.files[segName], []byte("\n"))
	if len(ls) > 0 && len(ls[len(ls)-1]) == 0 {
		ls = ls[:len(ls)-1]
	}
	return ls
}

func (d *corpusDraft) setLines(ls [][]byte) { d.files[segName] = bytes.Join(ls, nil) }

// alter replaces old with new in file, once, and fails when old is not there.
func (d *corpusDraft) alter(file, old, new string) {
	d.t.Helper()
	if !bytes.Contains(d.files[file], []byte(old)) {
		d.t.Fatalf("%s has no %q", file, old)
	}
	d.files[file] = bytes.Replace(d.files[file], []byte(old), []byte(new), 1)
}

// write saves the draft as a zip. With fix, the manifest lists the files as
// they are now, so the hash check passes and a deeper check has to notice.
func (d *corpusDraft) write(fix bool) string {
	d.t.Helper()
	if fix {
		var kept []pack.File
		for _, f := range d.m.Files {
			if b, ok := d.files[f.Path]; ok {
				kept = append(kept, pack.File{Path: f.Path, SHA256: ledger.SHA256Bytes(b), Bytes: int64(len(b))})
			}
		}
		d.m.Files = kept
	}
	mb, err := json.MarshalIndent(d.m, "", "  ")
	if err != nil {
		d.t.Fatal(err)
	}
	if _, ok := d.files["manifest.json"]; ok && !d.rawManifest {
		d.files["manifest.json"] = append(mb, '\n')
	}
	var names []string
	seen := map[string]bool{}
	for _, n := range d.names {
		if _, ok := d.files[n]; ok {
			names = append(names, n)
			seen[n] = true
		}
	}
	var extra []string
	for n := range d.files {
		if !seen[n] {
			extra = append(extra, n)
		}
	}
	sort.Strings(extra)
	out := filepath.Join(d.t.TempDir(), "bad.zip")
	if err := pack.WriteZip(out, append(names, extra...), d.files, day0); err != nil {
		d.t.Fatal(err)
	}
	return out
}

type corpusCase struct {
	name string
	base string
	fix  bool // the manifest lists the files as they are after the edit
	edit func(d *corpusDraft)
}

func setManifest(edit func(m *pack.Manifest)) func(*corpusDraft) {
	return func(d *corpusDraft) { edit(d.m) }
}

func removeFile(name string) func(*corpusDraft) {
	return func(d *corpusDraft) { delete(d.files, name) }
}

// dropListed removes a file from the zip and from the manifest.
func dropListed(name string) func(*corpusDraft) {
	return func(d *corpusDraft) {
		delete(d.files, name)
		var kept []pack.File
		for _, f := range d.m.Files {
			if f.Path != name {
				kept = append(kept, f)
			}
		}
		d.m.Files = kept
	}
}

func replaceFile(name, body string) func(*corpusDraft) {
	return func(d *corpusDraft) {
		d.files[name] = []byte(body)
		d.rawManifest = d.rawManifest || name == "manifest.json"
	}
}

func corpusSegmentCases() []corpusCase {
	huge := string(bytes.Repeat([]byte("a"), 17<<20))
	tokens := func(d *corpusDraft) { d.alter(segName, `"tokens_in":100`, `"tokens_in":1`) }
	swap := func(i, j int) func(*corpusDraft) {
		return func(d *corpusDraft) {
			ls := d.lines()
			ls[i], ls[j] = ls[j], ls[i]
			d.setLines(ls)
		}
	}
	drop := func(i int) func(*corpusDraft) {
		return func(d *corpusDraft) {
			ls := d.lines()
			d.setLines(append(ls[:i:i], ls[i+1:]...))
		}
	}
	dup := func(i int) func(*corpusDraft) {
		return func(d *corpusDraft) {
			ls := d.lines()
			d.setLines(append(ls[:i+1:i+1], ls[i:]...))
		}
	}
	last := func(edit func(ls [][]byte) [][]byte) func(*corpusDraft) {
		return func(d *corpusDraft) { d.setLines(edit(d.lines())) }
	}
	return []corpusCase{
		{"segment line altered, stale manifest", baseMid, false, tokens},
		{"segment line altered, manifest fixed", baseMid, true, tokens},
		{"segment line altered in a bare pack", baseBare, true, tokens},
		{"segment first line removed", baseMid, true, drop(0)},
		{"segment middle line removed", baseMid, true, drop(2)},
		{"segment last line removed", baseMid, true, func(d *corpusDraft) { ls := d.lines(); d.setLines(ls[:len(ls)-1]) }},
		{"segment last line removed in a bare pack", baseBare, true, func(d *corpusDraft) { ls := d.lines(); d.setLines(ls[:len(ls)-1]) }},
		{"segment lines swapped", baseMid, true, swap(1, 2)},
		{"segment first lines swapped in a bare pack", baseBare, true, swap(0, 1)},
		{"segment line duplicated", baseMid, true, dup(2)},
		{"segment last line duplicated", baseBare, true, func(d *corpusDraft) { ls := d.lines(); d.setLines(append(ls, ls[len(ls)-1])) }},
		{"segment last line cut short", baseMid, true, func(d *corpusDraft) {
			d.files[segName] = d.files[segName][:len(d.files[segName])-40]
		}},
		{"segment first line cut short", baseBare, true, func(d *corpusDraft) {
			ls := d.lines()
			ls[0] = ls[0][:30]
			d.setLines(append(ls[:1:1], append([]byte("\n"), bytes.Join(ls[1:], nil)...)))
		}},
		{"segment with blank lines", baseMid, true, func(d *corpusDraft) {
			d.files[segName] = append(append([]byte("\n  \n"), bytes.ReplaceAll(d.files[segName], []byte("\n"), []byte("\n\n"))...), '\n')
		}},
		{"segment with CRLF line ends", baseMid, true, func(d *corpusDraft) {
			d.files[segName] = bytes.ReplaceAll(d.files[segName], []byte("\n"), []byte("\r\n"))
		}},
		{"segment without the last line end", baseMid, true, func(d *corpusDraft) {
			d.files[segName] = bytes.TrimSuffix(d.files[segName], []byte("\n"))
		}},
		{"segment line that is not an object", baseMid, true, last(func(ls [][]byte) [][]byte {
			return append(ls[:2:2], append([][]byte{[]byte("[1,2]\n")}, ls[2:]...)...)
		})},
		{"segment last line that is not an object", baseMid, true, last(func(ls [][]byte) [][]byte { return append(ls, []byte("42\n")) })},
		{"segment first line that is not JSON", baseMid, true, last(func(ls [][]byte) [][]byte {
			return append([][]byte{[]byte("garbage\n")}, ls...)
		})},
		{"segment line not in canonical form", baseMid, true, func(d *corpusDraft) {
			ls := d.lines()
			ls[2] = bytes.Replace(ls[2], []byte(`"seq":`), []byte(`"seq": `), 1)
			d.setLines(ls)
		}},
		{"segment empty", baseMid, true, replaceFile(segName, "")},
		{"segment only newlines", baseMid, true, replaceFile(segName, "\n\n\n")},
		{"segment first line too long", baseMid, true, replaceFile(segName, huge)},
		{"segment later line too long", baseMid, true, func(d *corpusDraft) {
			d.files[segName] = append(append(d.files[segName], '\n', '\n'), huge...)
		}},
		{"segment line too long after a blank one", baseBare, true, func(d *corpusDraft) {
			d.files[segName] = append(append(append([]byte("\n"), d.files[segName]...), '\n'), huge...)
		}},
		{"segment missing, listed", baseMid, false, removeFile(segName)},
		{"segment missing and unlisted", baseMid, true, dropListed(segName)},
		{"segment missing in a bare pack", baseBare, true, dropListed(segName)},
		{"segment hash differs from the manifest", baseMid, false, setManifest(func(m *pack.Manifest) {
			for i := range m.Files {
				if m.Files[i].Path == segName {
					m.Files[i].SHA256 = "sha256:" + strings.Repeat("0", 64)
				}
			}
		})},
		{"segment not listed in the manifest", baseMid, true, func(d *corpusDraft) {
			var kept []pack.File
			for _, f := range d.m.Files {
				if f.Path != segName {
					kept = append(kept, f)
				}
			}
			d.m.Files = kept
		}},
		{"segment listed twice", baseMid, true, func(d *corpusDraft) {
			for _, f := range d.m.Files {
				if f.Path == segName {
					d.m.Files = append(d.m.Files, f)
					return
				}
			}
		}},
		{"segment at seq 0 that differs from genesis", baseBare, true, tokensBare},
		{"segment from the crlf ledger, edited", baseCRLF, true, tokens},
	}
}

func tokensBare(d *corpusDraft) { d.alter(segName, `"provider":"anthropic"`, `"provider":"openai"`) }

func manifestCases() []corpusCase {
	seg := func(edit func(s *pack.Segment)) func(*pack.Manifest) {
		return func(m *pack.Manifest) { edit(&m.Segment) }
	}
	return []corpusCase{
		{"manifest first_seq wrong", baseMid, true, setManifest(seg(func(s *pack.Segment) { s.FirstSeq++ }))},
		{"manifest first_prev_hash wrong", baseMid, true, setManifest(seg(func(s *pack.Segment) { s.FirstPrevHash = "sha256:" + strings.Repeat("1", 64) }))},
		{"manifest last_seq wrong", baseMid, true, setManifest(seg(func(s *pack.Segment) { s.LastSeq++ }))},
		{"manifest last_hash wrong", baseMid, true, setManifest(seg(func(s *pack.Segment) { s.LastHash = "sha256:" + strings.Repeat("2", 64) }))},
		{"manifest first_seq wrong in a bare pack", baseBare, true, setManifest(seg(func(s *pack.Segment) { s.FirstSeq = 5 }))},
		{"manifest start_anchored off", baseMid, true, setManifest(seg(func(s *pack.Segment) { s.StartAnchored = false }))},
		{"manifest start_anchored claimed at genesis", baseGenesis, true, setManifest(seg(func(s *pack.Segment) { s.StartAnchored = true }))},
		{"manifest start_anchored claimed with no anchors", baseBare, true, setManifest(seg(func(s *pack.Segment) { s.StartAnchored = true }))},
		{"manifest end_anchored claimed with no anchors", baseBare, true, setManifest(seg(func(s *pack.Segment) { s.EndAnchored = true }))},
		{"manifest end_anchored claimed past the last anchor", baseTail, true, setManifest(seg(func(s *pack.Segment) { s.EndAnchored = true }))},
		{"manifest end_anchored off though anchored", baseMid, true, setManifest(seg(func(s *pack.Segment) { s.EndAnchored = false }))},
		{"manifest end_anchored off on a tail", baseTail, true, setManifest(seg(func(s *pack.Segment) { s.EndAnchored = false }))},
		{"manifest end_anchored with a wrong last_seq", baseMid, true, setManifest(seg(func(s *pack.Segment) { s.LastSeq-- }))},
		{"manifest context_entries too many", baseMid, true, setManifest(func(m *pack.Manifest) { m.ContextEntries = 7 })},
		{"manifest context_entries zero", baseMid, true, setManifest(func(m *pack.Manifest) { m.ContextEntries = 0 })},
		{"manifest context_entries counted but no file", baseMid, true, dropListed(ctxName)},
		{"manifest context file left out and count zero", baseMid, true, func(d *corpusDraft) { dropListed(ctxName)(d); d.m.ContextEntries = 0 }},
		{"manifest totals tampered", baseMid, true, setManifest(func(m *pack.Manifest) { m.Totals.All.Calls++ })},
		{"manifest totals tampered, stale", baseMid, false, setManifest(func(m *pack.Manifest) { m.Totals.All.TokensIn += 5 })},
		{"manifest period not dates", baseMid, true, setManifest(func(m *pack.Manifest) { m.Period.From = "last week" })},
		{"manifest period narrowed", baseMid, true, setManifest(func(m *pack.Manifest) { m.Period.To = m.Period.From })},
		{"manifest provider filter added", baseMid, true, setManifest(func(m *pack.Manifest) { m.ProviderFilter = "openai" })},
		{"manifest ledger_id wrong", baseMid, true, setManifest(func(m *pack.Manifest) { m.LedgerID = "someone-else" })},
		{"manifest format unsupported", baseMid, true, setManifest(func(m *pack.Manifest) { m.Format = "ambermoth-pack/99" })},
		{"manifest from another generator", baseMid, true, setManifest(func(m *pack.Manifest) { m.Generator = "ambermoth/9.9.9" })},
		{"manifest disclosed lists a seq with no key", baseMid, true, setManifest(func(m *pack.Manifest) { m.Disclosed = append(m.Disclosed, 4) })},
		{"manifest not JSON", baseMid, false, replaceFile("manifest.json", "{")},
		{"manifest not an object", baseMid, false, replaceFile("manifest.json", "[]")},
		{"manifest with wrong field types", baseMid, false, replaceFile("manifest.json", `{"format":3}`)},
		{"manifest missing", baseMid, false, removeFile("manifest.json")},
		{"manifest.tsr that is not a token", baseMid, true, replaceFile("manifest.tsr", "not a token")},
		{"manifest.tsr given though the manifest changed", baseMid, true, func(d *corpusDraft) {
			d.files["manifest.tsr"] = d.files[firstToken(d)]
		}},
	}
}

// firstToken names a token in the pack.
func firstToken(d *corpusDraft) string {
	for _, n := range d.names {
		if strings.HasPrefix(n, "anchors/") {
			return n
		}
	}
	d.t.Fatal("no token in the pack")
	return ""
}

func fileCases() []corpusCase {
	return []corpusCase{
		{"summary.html edited", baseMid, true, func(d *corpusDraft) { d.files["summary.html"] = append(d.files["summary.html"], " "...) }},
		{"summary.html edited, stale manifest", baseMid, false, func(d *corpusDraft) { d.alter("summary.html", "<html", "<html ") }},
		{"summary.html missing", baseMid, true, dropListed("summary.html")},
		{"LIMITS.md missing", baseMid, false, removeFile("LIMITS.md")},
		{"LIMITS.md missing and unlisted", baseMid, true, dropListed("LIMITS.md")},
		{"VERIFY.md altered", baseMid, false, func(d *corpusDraft) { d.files["VERIFY.md"] = []byte("changed") }},
		{"letter-template.md missing", baseBare, true, dropListed("letter-template.md")},
		{"genesis missing", baseMid, true, dropListed("ledger/genesis.json")},
		{"genesis altered", baseMid, false, func(d *corpusDraft) { d.alter("ledger/genesis.json", `"generator":`, `"generator":"x",`+`"y":`) }},
		{"genesis not JSON", baseMid, true, replaceFile("ledger/genesis.json", "nope")},
		{"genesis from another ledger", baseBare, true, setManifest(func(m *pack.Manifest) { m.LedgerID = "other" })},
		{"genesis too long", baseMid, true, replaceFile("ledger/genesis.json", string(bytes.Repeat([]byte("g"), 17<<20)))},
		{"extra file", baseMid, true, replaceFile("extra.txt", "x")},
		{"extra file, stale manifest", baseMid, false, replaceFile("notes/extra.txt", "x")},
		{"required files all missing", baseBare, true, func(d *corpusDraft) {
			required := []string{"LIMITS.md", "VERIFY.md", "summary.html", "letter-template.md", "ledger/genesis.json"}
			for _, n := range required {
				dropListed(n)(d)
			}
		}},
		{"context altered", baseMid, true, func(d *corpusDraft) { d.alter(ctxName, `"prod"`, `"test"`) }},
		{"context line not JSON", baseMid, true, func(d *corpusDraft) { d.files[ctxName] = append(d.files[ctxName], "not json\n"...) }},
		{"context line not an object", baseMid, true, func(d *corpusDraft) { d.files[ctxName] = append(d.files[ctxName], "7\n"...) }},
		{"context with a line too long", baseMid, true, replaceFile(ctxName, string(bytes.Repeat([]byte("c"), 17<<20)))},
		{"context entry inside the segment", baseMid, true, func(d *corpusDraft) {
			ls := d.lines()
			d.files[ctxName] = append(d.files[ctxName], ls[0]...)
			d.m.ContextEntries++
		}},
		{"context empty", baseMid, true, replaceFile(ctxName, "")},
		{"disclosure keys with an extra seq", baseMid, true, func(d *corpusDraft) { d.alter(keyName, "{", `{"4":"00",`) }},
		{"disclosure keys missing one", baseMid, true, replaceFile(keyName, "{}")},
		{"disclosure keys not JSON", baseMid, true, replaceFile(keyName, "[")},
		{"disclosure keys with a wrong value type", baseMid, true, replaceFile(keyName, `{"9":7}`)},
		{"disclosure keys file missing", baseMid, true, dropListed(keyName)},
		{"disclosure keys file in a pack that discloses nothing", baseBare, true, replaceFile(keyName, `{"1":"00"}`)},
		{"token missing", baseMid, true, func(d *corpusDraft) { dropListed(firstToken(d))(d) }},
		{"token altered", baseMid, true, func(d *corpusDraft) { d.files[firstToken(d)] = []byte("not a token") }},
		{"token altered, stale manifest", baseMid, false, func(d *corpusDraft) { d.files[firstToken(d)] = []byte("not a token") }},
		{"tokens swapped", baseMid, true, func(d *corpusDraft) {
			var toks []string
			for _, n := range d.names {
				if strings.HasPrefix(n, "anchors/") {
					toks = append(toks, n)
				}
			}
			d.files[toks[0]], d.files[toks[1]] = d.files[toks[1]], d.files[toks[0]]
		}},
		{"tokens all missing", baseTail, true, func(d *corpusDraft) {
			for _, n := range append([]string{}, d.names...) {
				if strings.HasPrefix(n, "anchors/") {
					dropListed(n)(d)
				}
			}
		}},
		{"zip order: segment first", baseMid, true, func(d *corpusDraft) { d.names = moveTo(d.names, segName, 0) }},
		{"zip order: segment last", baseMid, true, func(d *corpusDraft) { d.names = moveTo(d.names, segName, len(d.names)) }},
		{"zip order: segment last, with a damaged line", baseMid, true, func(d *corpusDraft) {
			d.names = moveTo(d.names, segName, len(d.names))
			d.alter(segName, `"tokens_in":100`, `"tokens_in":1`)
		}},
	}
}

func moveTo(names []string, name string, at int) []string {
	var out []string
	for _, n := range names {
		if n != name {
			out = append(out, n)
		}
	}
	at = min(at, len(out))
	return append(out[:at:at], append([]string{name}, out[at:]...)...)
}

// honestCases are every golden pack, undamaged.
func honestCases() []string {
	names := make([]string, len(goldenCases))
	for i, c := range goldenCases {
		names[i] = c.name
	}
	return names
}

func goldenCaseNamed(t *testing.T, name string) goldenCase {
	t.Helper()
	for _, c := range goldenCases {
		if c.name == name {
			return c
		}
	}
	t.Fatalf("no golden case %q", name)
	return goldenCase{}
}

// rawEntry is a file of a hand-made zip.
type rawEntry struct {
	name string
	body string
	mode os.FileMode
}

func writeRawZip(t *testing.T, entries []rawEntry) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "raw.zip")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	for _, e := range entries {
		h := &zip.FileHeader{Name: e.name, Method: zip.Deflate, Modified: day0}
		if e.mode != 0 {
			h.SetMode(e.mode)
		}
		w, err := zw.CreateHeader(h)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(e.body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

// zipEntries lists the files of a draft in its order, then the entries that follow.
func zipEntries(d *corpusDraft, tail ...rawEntry) []rawEntry {
	var es []rawEntry
	for _, n := range d.names {
		if b, ok := d.files[n]; ok {
			es = append(es, rawEntry{name: n, body: string(b)})
		}
	}
	return append(es, tail...)
}

// zeroEntry adds a deflated entry of n MiB of zeros without compressing them: the
// compressed form is one flushed block repeated.
func zeroEntry(t *testing.T, zw *zip.Writer, name string, mib int) {
	t.Helper()
	zeros := make([]byte, 1<<20)
	var buf bytes.Buffer
	fw, err := flate.NewWriter(&buf, flate.BestCompression)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fw.Write(zeros); err != nil {
		t.Fatal(err)
	}
	if err := fw.Flush(); err != nil {
		t.Fatal(err)
	}
	block := append([]byte{}, buf.Bytes()...)
	if err := fw.Close(); err != nil {
		t.Fatal(err)
	}
	end := buf.Bytes()[len(block):]
	var crc uint32
	for i := 0; i < mib; i++ {
		crc = crc32.Update(crc, crc32.IEEETable, zeros)
	}
	h := &zip.FileHeader{Name: name, Method: zip.Deflate, Modified: day0, CRC32: crc,
		UncompressedSize64: uint64(mib) << 20, CompressedSize64: uint64(mib*len(block) + len(end))}
	w, err := zw.CreateRaw(h)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < mib; i++ {
		if _, err := w.Write(block); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := w.Write(end); err != nil {
		t.Fatal(err)
	}
}

// zipCases are the packs that are not made by editing a draft.
func zipCases() []struct {
	name string
	base string
	make func(t *testing.T, base string) string
} {
	type zc = struct {
		name string
		base string
		make func(t *testing.T, base string) string
	}
	withTail := func(tail ...rawEntry) func(t *testing.T, base string) string {
		return func(t *testing.T, base string) string {
			return writeRawZip(t, zipEntries(newCorpusDraft(t, base), tail...))
		}
	}
	cut := func(drop int) func(t *testing.T, base string) string {
		return func(t *testing.T, base string) string {
			b, err := os.ReadFile(base)
			if err != nil {
				t.Fatal(err)
			}
			out := filepath.Join(t.TempDir(), "cut.zip")
			if err := os.WriteFile(out, b[:len(b)-drop], 0o600); err != nil {
				t.Fatal(err)
			}
			return out
		}
	}
	return []zc{
		{"zip with a path traversal entry", baseMid, withTail(rawEntry{name: "../evil", body: "x"})},
		{"zip with a path traversal entry before the segment", baseMid, func(t *testing.T, base string) string {
			return writeRawZip(t, append([]rawEntry{{name: "anchors/../../evil", body: "x"}}, zipEntries(newCorpusDraft(t, base))...))
		}},
		{"zip with an absolute name", baseMid, withTail(rawEntry{name: "/etc/evil", body: "x"})},
		{"zip with a backslash name", baseMid, withTail(rawEntry{name: `a\b`, body: "x"})},
		{"zip with a duplicate name", baseMid, withTail(rawEntry{name: "LIMITS.md", body: "again"})},
		{"zip with a duplicate segment", baseMid, withTail(rawEntry{name: segName, body: "again"})},
		{"zip with a symlink", baseMid, withTail(rawEntry{name: "link", body: "/etc/passwd", mode: os.ModeSymlink | 0o777})},
		{"zip with a symlink as the segment", baseMid, func(t *testing.T, base string) string {
			d := newCorpusDraft(t, base)
			delete(d.files, segName)
			return writeRawZip(t, zipEntries(d, rawEntry{name: segName, body: "/etc/passwd", mode: os.ModeSymlink | 0o777}))
		}},
		{"zip with a directory entry", baseMid, withTail(rawEntry{name: "empty/", mode: os.ModeDir | 0o755})},
		{"zip that is not a zip", baseMid, func(t *testing.T, _ string) string {
			p := filepath.Join(t.TempDir(), "x.zip")
			if err := os.WriteFile(p, []byte("hello"), 0o600); err != nil {
				t.Fatal(err)
			}
			return p
		}},
		{"zip that is empty", baseMid, func(t *testing.T, _ string) string { return writeRawZip(t, nil) }},
		{"zip missing the file", baseMid, func(t *testing.T, _ string) string { return filepath.Join(t.TempDir(), "none.zip") }},
		{"zip truncated by 10 bytes", baseMid, cut(10)},
		{"zip truncated by a quarter", baseMid, func(t *testing.T, base string) string {
			b, err := os.ReadFile(base)
			if err != nil {
				t.Fatal(err)
			}
			return cut(len(b)/4)(t, base)
		}},
		{"zip with a corrupt segment stream", baseMid, corruptSegment},
		{"zip with a segment entry larger than the limit", baseMid, func(t *testing.T, base string) string {
			return headerOnly(t, newCorpusDraft(t, base), segName)
		}},
		{"zip with another entry larger than the limit", baseMid, func(t *testing.T, base string) string {
			return headerOnly(t, newCorpusDraft(t, base), "LIMITS.md")
		}},
		{"zip with more than 1 GiB in the segment and one more file", baseMid, bigPack},
	}
}

// headerOnly makes a pack where name claims to be 1 GiB and one byte.
func headerOnly(t *testing.T, d *corpusDraft, name string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "big.zip")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	for _, n := range d.names {
		if n == name {
			if _, err := zw.CreateRaw(&zip.FileHeader{Name: n, Method: zip.Store, Modified: day0, UncompressedSize64: 1<<30 + 1}); err != nil {
				t.Fatal(err)
			}
			continue
		}
		w, err := zw.CreateHeader(&zip.FileHeader{Name: n, Method: zip.Deflate, Modified: day0})
		if err != nil {
			t.Fatal(err)
		}
		_, _ = w.Write(d.files[n])
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

// bigPack has a segment of exactly 1 GiB of zeros and then one more byte.
func bigPack(t *testing.T, base string) string {
	t.Helper()
	d := newCorpusDraft(t, base)
	path := filepath.Join(t.TempDir(), "big.zip")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	for _, n := range d.names {
		if n == segName {
			zeroEntry(t, zw, n, 1024)
			continue
		}
		w, err := zw.CreateHeader(&zip.FileHeader{Name: n, Method: zip.Deflate, Modified: day0})
		if err != nil {
			t.Fatal(err)
		}
		_, _ = w.Write(d.files[n])
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

// corruptSegment flips bytes in the middle of the segment's compressed data.
func corruptSegment(t *testing.T, base string) string {
	t.Helper()
	zr, err := zip.OpenReader(base)
	if err != nil {
		t.Fatal(err)
	}
	var off, size int64
	for _, zf := range zr.File {
		if zf.Name == segName {
			if off, err = zf.DataOffset(); err != nil {
				t.Fatal(err)
			}
			size = int64(zf.CompressedSize64)
		}
	}
	_ = zr.Close()
	b, err := os.ReadFile(base)
	if err != nil {
		t.Fatal(err)
	}
	for i := int64(0); i < 8; i++ {
		b[off+size/2+i] ^= 0xff
	}
	out := filepath.Join(t.TempDir(), "corrupt.zip")
	if err := os.WriteFile(out, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return out
}

// verifyView is the report as the golden file holds it: the whole PackReport,
// with the HTML as its hash, and the ledger gaps that the ledger report leaves out of its JSON.
func verifyView(t *testing.T, path string, r *pack.PackReport, err error) any {
	t.Helper()
	if err != nil {
		return map[string]any{"error": strings.ReplaceAll(err.Error(), path, "<pack>")}
	}
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	view := map[string]any{}
	if err := json.Unmarshal(b, &view); err != nil {
		t.Fatal(err)
	}
	// the HTML is pinned by hash: it is the same page, 40 KB of it, in every report
	view["ReportHTML"] = map[string]any{"bytes": len(r.ReportHTML), "sha256": ledger.SHA256Bytes(r.ReportHTML)}
	if r.Ledger != nil {
		view["LedgerGaps"] = r.Ledger.Gaps
	}
	view["OK"] = r.OK()
	return view
}

func verifyOpts(roots *x509.CertPool) pack.VerifyOptions {
	return pack.VerifyOptions{Roots: roots, Generator: "test", Clock: corpusClock}
}

// corpusReports verifies every pack of the corpus and returns the views by name.
func corpusReports(t *testing.T) map[string]any {
	t.Helper()
	zips := map[string]string{}
	roots := map[string]*x509.CertPool{}
	baseOf := func(name string) string {
		if p, ok := zips[name]; ok {
			return p
		}
		p, r := goldenZip(t, goldenCaseNamed(t, name))
		zips[name], roots[name] = p, r
		return p
	}
	got := map[string]any{}
	put := func(name, path, base string) {
		if _, dup := got[name]; dup {
			t.Fatalf("two corpus packs are called %q", name)
		}
		r, err := pack.VerifyPack(path, verifyOpts(roots[base]))
		got[name] = verifyView(t, path, r, err)
	}
	for _, n := range honestCases() {
		put("honest: "+n, baseOf(n), n)
	}
	groups := [][]corpusCase{corpusSegmentCases(), manifestCases(), fileCases()}
	for _, cs := range groups {
		for _, c := range cs {
			put("damaged: "+c.name, damaged(t, baseOf(c.base), c), c.base)
		}
	}
	for _, c := range zipCases() {
		if isBig(c.name) && !runBig() {
			continue
		}
		put("zip: "+c.name, c.make(t, baseOf(c.base)), c.base)
	}
	return got
}

// The pack with a 1 GiB segment takes 10 s to read, and 40 s with -race, so it
// runs with -update and when AMBERMOTH_BIG_PACK is set.
func isBig(name string) bool { return strings.Contains(name, "1 GiB") }

func runBig() bool { return *updateGolden || os.Getenv("AMBERMOTH_BIG_PACK") != "" }

func damaged(t *testing.T, base string, c corpusCase) string {
	t.Helper()
	d := newCorpusDraft(t, base)
	c.edit(d)
	return d.write(c.fix)
}

func TestVerifyPackCorpus(t *testing.T) {
	got := corpusReports(t)
	if *updateGolden {
		b, err := json.MarshalIndent(got, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Dir(corpusFile), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(corpusFile, append(b, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	b, err := os.ReadFile(corpusFile)
	if err != nil {
		t.Fatal(err)
	}
	var want map[string]json.RawMessage
	if err := json.Unmarshal(b, &want); err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(got))
	for n := range got {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		w, ok := want[n]
		if !ok {
			t.Errorf("%s: not in %s", n, corpusFile)
			continue
		}
		g, err := json.Marshal(got[n])
		if err != nil {
			t.Fatal(err)
		}
		if !sameJSON(t, g, w) {
			t.Errorf("%s: report differs from the golden one\n got: %s\nwant: %s", n, g, w)
		}
	}
	if runBig() && len(want) != len(got) {
		t.Errorf("%d golden reports, %d corpus packs", len(want), len(got))
	}
}

// sameJSON compares two JSON texts as values, so key order and indentation do not matter.
func sameJSON(t *testing.T, a, b []byte) bool {
	t.Helper()
	var x, y bytes.Buffer
	var av, bv any
	if err := json.Unmarshal(a, &av); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &bv); err != nil {
		t.Fatal(err)
	}
	if err := json.NewEncoder(&x).Encode(av); err != nil {
		t.Fatal(err)
	}
	if err := json.NewEncoder(&y).Encode(bv); err != nil {
		t.Fatal(err)
	}
	return bytes.Equal(x.Bytes(), y.Bytes())
}
