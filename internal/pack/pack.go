// Package pack builds and verifies evidence packs (SPEC §9).
package pack

import (
	"archive/zip"
	"bufio"
	"bytes"
	"cmp"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/x509"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	ttemplate "text/template"

	"github.com/devrdias/ambermoth/internal/canon"
	"github.com/devrdias/ambermoth/internal/ledger"
	"github.com/devrdias/ambermoth/internal/ops"
	"github.com/devrdias/ambermoth/internal/report"
	"github.com/devrdias/ambermoth/internal/tsa"
)

const Format = "ambermoth-pack/1"

//go:embed summary.html.tmpl
var summaryTmpl string

// Options configures Build.
type Options struct {
	From, To      time.Time // To is exclusive (start of the day after the last day)
	Provider      string
	Account       string
	Disclose      []int64
	TSAs          []string       // timestamp manifest.json if set
	Roots         *x509.CertPool // pinned TSA roots (trust.Pool); required
	AllowUnpinned bool           // accept anchors whose TSA does not chain to Roots
	Out           string
	Generator     string
	Client        *http.Client

	verify verifyFunc // nil means ledger.VerifyReader; tests count the calls
}

type verifyFunc func(io.Reader, ledger.VerifyOptions) (*ledger.Report, error)

func (o Options) verifier() verifyFunc {
	if o.verify == nil {
		return ledger.VerifyReader
	}
	return o.verify
}

// Period is the human period (inclusive dates).
type Period struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// Segment identifies the contiguous ledger range in the pack.
type Segment struct {
	FirstSeq      int64  `json:"first_seq"`
	LastSeq       int64  `json:"last_seq"`
	FirstPrevHash string `json:"first_prev_hash"`
	LastHash      string `json:"last_hash"`
	StartAnchored bool   `json:"start_anchored"`
	EndAnchored   bool   `json:"end_anchored"`
}

// File is a manifest file record.
type File struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Bytes  int64  `json:"bytes"`
}

// Manifest is manifest.json.
type Manifest struct {
	Format         string        `json:"format"`
	Generator      string        `json:"generator"`
	Created        string        `json:"created"`
	LedgerID       string        `json:"ledger_id"`
	Account        string        `json:"account"`
	Period         Period        `json:"period"`
	ProviderFilter string        `json:"provider_filter"`
	Segment        Segment       `json:"segment"`
	ContextEntries int           `json:"context_entries"`
	Disclosed      []int64       `json:"disclosed"`
	Totals         report.Totals `json:"totals"`
	Files          []File        `json:"files"`
}

func (o Options) filter() report.Filter {
	return report.Filter{From: o.From, To: o.To, Provider: o.Provider}
}

// Build writes an evidence pack zip and returns its manifest.
func Build(ctx context.Context, s *ledger.Store, o Options) (*Manifest, error) {
	src, err := readSource(s, o)
	if err != nil {
		return nil, err
	}
	defer src.close()
	sp, err := src.chooseSpan()
	if err != nil {
		return nil, err
	}
	c, err := src.cutAt(sp, o.filter())
	if err != nil {
		return nil, err
	}
	files, disclosed, err := c.files(s, src, o.Disclose)
	if err != nil {
		return nil, err
	}
	m := c.manifest(s, o, disclosed)
	if err := addReadables(files, m, c.summary, c.report); err != nil {
		return nil, err
	}
	names, err := addManifest(ctx, files, c.segmentFile(), m, o)
	if err != nil {
		return nil, err
	}
	return m, writeZipFrom(o.Out, names, s.Now(), func(name string, w io.Writer) error {
		if name == segmentName {
			return src.writeSegment(c.span, w)
		}
		_, err := w.Write(files[name])
		return err
	})
}

// source is a ledger file read for packing, verified from genesis. It keeps an
// index of the ledger, not the entries; the lines are read again from the file
// when a pack needs them.
type source struct {
	f      *os.File
	size   int64 // the part of the file read: later appends are not part of the pack
	ix     *index
	full   *ledger.Report
	check  ledger.VerifyOptions
	verify verifyFunc
}

func readSource(s *ledger.Store, o Options) (*source, error) {
	f, err := os.Open(s.LedgerPath())
	if err != nil {
		return nil, err
	}
	src, err := newSource(f, s, o)
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	return src, nil
}

func newSource(f *os.File, s *ledger.Store, o Options) (*source, error) {
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	src := &source{f: f, size: fi.Size(), verify: o.verifier(),
		check: ledger.VerifyOptions{AnchorDir: anchorDir(s), CheckAnchor: ops.AnchorVerifier(o.Roots, o.AllowUnpinned)}}
	src.ix = &index{}
	flt := o.filter()
	opt := src.check
	opt.OnEntry = func(e ledger.Entry, _ []byte, off int64) { src.ix.add(e, off, flt) }
	if src.full, err = src.verify(src.section(0, src.size), opt); err != nil {
		return nil, err
	}
	src.ix.end(src.size)
	if !src.full.OK() || !src.full.StartsAtGenesis {
		return nil, fmt.Errorf("the ledger does not verify; refusing to build a pack from it:\n  %s", strings.Join(src.full.Errors, "\n  "))
	}
	return src, nil
}

func (src *source) close() { _ = src.f.Close() }

func (src *source) section(from, to int64) *io.SectionReader {
	return io.NewSectionReader(src.f, from, to-from)
}

// chooseSpan picks the segment for the calls the filter matched.
func (src *source) chooseSpan() (span, error) {
	firstCall, lastCall, ok := src.ix.callRange()
	if !ok {
		return span{}, errors.New("no calls in the selected period")
	}
	last := src.ix.lastRelevantSeq(firstCall, lastCall)
	return selectSpan(src.full.Anchors, src.ix.entries(), firstCall, last), nil
}

// segmentName is the zip entry holding the segment's lines.
const segmentName = "ledger/segment.jsonl"

// cut is the part of the ledger a pack carries: the segment, and the context
// entries before it. The segment's lines stay in the ledger file; the cut
// holds what was learned reading them.
type cut struct {
	span
	genesis     []byte
	context     []byte // the context entries' lines, each ended by a newline
	contextN    int
	first, last ledger.Entry // the segment's first and last entry
	anchors     []ledger.Entry
	segSum      string // sha256 of the segment file
	segBytes    int64
	summary     *report.Summary // context entries, then the segment
	report      *ledger.Report  // verification of the segment
}

// cutAt reads genesis, the context before sp and the segment itself, once.
func (src *source) cutAt(sp span, f report.Filter) (*cut, error) {
	c := &cut{span: sp}
	g, _, err := src.lineAt(0)
	if err != nil {
		return nil, err
	}
	c.genesis = append(g, '\n')
	z := report.NewSummarizer(f)
	if err := src.readContext(c, z); err != nil {
		return nil, err
	}
	if err := src.scanSegment(c, z); err != nil {
		return nil, err
	}
	c.summary = z.Summary()
	return c, nil
}

// scanSegment feeds the segment to z, and measures the file the pack will hold.
// It also gives the verification report of the segment: when the segment is the
// whole ledger that is the report readSource already has, and when not the
// segment is verified in the same read.
func (src *source) scanSegment(c *cut, z *report.Summarizer) error {
	h := sha256.New()
	n := 0
	see := func(e ledger.Entry, line []byte, _ int64) {
		_, _ = h.Write(line)
		_, _ = io.WriteString(h, "\n")
		c.segBytes += int64(len(line)) + 1
		z.Add(e)
		if n == 0 {
			c.first = e
		}
		c.last = e
		n++
		if e.Type() == ledger.TypeAnchor {
			c.anchors = append(c.anchors, e)
		}
	}
	err := src.readSegment(c, see)
	c.segSum = "sha256:" + hex.EncodeToString(h.Sum(nil))
	return err
}

func (src *source) readSegment(c *cut, see func(e ledger.Entry, line []byte, off int64)) error {
	r := src.section(src.ix.bounds(c.start, c.end))
	if c.start == 0 && c.end == int64(src.ix.entries()-1) {
		c.report = src.full
		return ledger.EachLine(r, func(line []byte, off int64) { see(ledger.ParseEntry(line), line, off) })
	}
	opt := src.check
	opt.OnEntry = see
	var err error
	c.report, err = src.verify(r, opt)
	return err
}

// lineAt reads entry i, line and parsed, from the file.
func (src *source) lineAt(i int64) ([]byte, ledger.Entry, error) {
	from, to := src.ix.bounds(i, i)
	buf := make([]byte, to-from)
	if _, err := src.f.ReadAt(buf, from); err != nil {
		return nil, nil, err
	}
	_, line, _ := bufio.ScanLines(buf, true)
	return line, ledger.ParseEntry(line), nil
}

// readContext adds the key events, code links and reconciliations recorded
// before the segment starts to c and to z.
func (src *source) readContext(c *cut, z *report.Summarizer) error {
	for i := int64(1); i < c.start; i++ {
		if src.ix.slots[i].kind != kindContext {
			continue
		}
		line, e, err := src.lineAt(i)
		if err != nil {
			return err
		}
		z.Add(e)
		c.context = append(append(c.context, line...), '\n')
		c.contextN++
	}
	return nil
}

// writeSegment writes the segment's lines to w, each ended by a newline.
func (src *source) writeSegment(sp span, w io.Writer) error {
	bw := bufio.NewWriter(w)
	err := ledger.EachLine(src.section(src.ix.bounds(sp.start, sp.end)), func(line []byte, _ int64) {
		_, _ = bw.Write(line) // a write error sticks and comes out of Flush
		_ = bw.WriteByte('\n')
	})
	if err != nil {
		return err
	}
	return bw.Flush()
}

// segmentFile is the manifest record of the segment, which is not in memory.
func (c *cut) segmentFile() File {
	return File{Path: segmentName, SHA256: c.segSum, Bytes: c.segBytes}
}

// files returns the small files of the pack that hold ledger data, with the
// anchor tokens and the disclosure keys, and the seqs disclosed. The segment
// is written from the ledger file when the zip is.
func (c *cut) files(s *ledger.Store, src *source, disclose []int64) (map[string][]byte, []int64, error) {
	files := map[string][]byte{"ledger/genesis.json": c.genesis}
	if c.contextN > 0 {
		files["ledger/context.jsonl"] = c.context
	}
	if err := addAnchorTokens(files, anchorDir(s), c.anchors); err != nil {
		return nil, nil, err
	}
	disclosed, err := src.addDisclosures(files, s, c.span, disclose)
	if err != nil {
		return nil, nil, err
	}
	return files, disclosed, nil
}

// manifest describes the cut, without the file list.
func (c *cut) manifest(s *ledger.Store, o Options, disclosed []int64) *Manifest {
	m := &Manifest{
		Format: Format, Generator: o.Generator, Created: ledger.FormatTime(s.Now()), LedgerID: s.LedgerID,
		Account: o.Account, ProviderFilter: o.Provider,
		Period: Period{From: o.From.Format("2006-01-02"), To: o.To.AddDate(0, 0, -1).Format("2006-01-02")},
		Segment: Segment{FirstSeq: c.first.Seq(), LastSeq: c.last.Seq(), FirstPrevHash: c.first.Str("prev_hash"),
			LastHash: c.last.Hash(), StartAnchored: c.startAnchored, EndAnchored: c.endAnchored},
		// Context entries are never calls or amendments, so they add nothing to the totals.
		ContextEntries: c.contextN, Disclosed: disclosed, Totals: c.summary.Totals,
	}
	if m.Account == "" {
		m.Account = "(account not specified)"
	}
	return m
}

// span is the segment of the ledger a pack carries.
type span struct {
	start, end                 int64
	startAnchored, endAnchored bool
}

// selectSpan picks the segment (SPEC section 9.1): it starts at the last anchored
// head at or before the first call and ends at the first anchored head at or
// after the last relevant entry, or at the ledger's ends when there is none.
// Line index == seq because the ledger verified from genesis.
func selectSpan(anchors []ledger.AnchorResult, n int, firstCall, lastRelevant int64) span {
	sp := span{start: 0, end: int64(n - 1)}
	// target seq -> last anchor entry seq for that target, so a pack keeps
	// every TSA's token for its end head (independent timestamps), not just one.
	anchoredBy := map[int64]int64{}
	for _, ar := range anchors {
		if ar.OK && ar.Seq > anchoredBy[ar.TargetSeq] {
			anchoredBy[ar.TargetSeq] = ar.Seq
		}
	}
	targets := make([]int64, 0, len(anchoredBy))
	for t := range anchoredBy {
		targets = append(targets, t)
	}
	sort.Slice(targets, func(i, j int) bool { return targets[i] < targets[j] })
	for _, t := range targets {
		if t <= firstCall {
			sp.start, sp.startAnchored = t, true
		}
	}
	for _, t := range targets {
		if t >= lastRelevant {
			sp.end, sp.endAnchored = anchoredBy[t], true
			break
		}
	}
	return sp
}

// addAnchorTokens adds the TSA token of every anchor entry in seg.
func addAnchorTokens(files map[string][]byte, dir string, seg []ledger.Entry) error {
	for _, e := range seg {
		if e.Type() != ledger.TypeAnchor {
			continue
		}
		name := e.Str("token_file")
		if !plainFileName(name) {
			return fmt.Errorf("anchor entry seq %d: token_file %q is not a plain file name", e.Seq(), name)
		}
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return err
		}
		files["anchors/"+name] = b
	}
	return nil
}

// plainFileName reports whether name is one file name, with no directory part.
func plainFileName(name string) bool {
	return name != "" && name != "." && name != ".." && !strings.ContainsAny(name, "/\\\x00")
}

// addDisclosures adds the per-entry keys of the disclosed calls, which must be
// call entries inside sp, and returns their seqs, each once, ascending.
func (src *source) addDisclosures(files map[string][]byte, s *ledger.Store, sp span, seqs []int64) ([]int64, error) {
	if len(seqs) == 0 {
		return []int64{}, nil
	}
	keys := map[string]string{}
	for _, q := range seqs {
		if q < sp.start || q > sp.end || src.ix.slots[q].kind != kindCall {
			return nil, fmt.Errorf("--disclose %d: not a call entry inside the pack segment", q)
		}
		_, e, err := src.lineAt(q)
		if err != nil {
			return nil, err
		}
		keys[strconv.FormatInt(q, 10)] = hex.EncodeToString(ledger.EntryKey(s.MasterKey, e.Str("salt_id")))
	}
	b, err := json.MarshalIndent(keys, "", "  ")
	if err != nil {
		return nil, err
	}
	files["disclosures/entry-keys.json"] = append(b, '\n')
	disclosed := slices.Clone(seqs)
	slices.Sort(disclosed)
	return slices.Compact(disclosed), nil
}

// addReadables adds the human-readable files.
func addReadables(files map[string][]byte, m *Manifest, ctxSummary *report.Summary, segReport *ledger.Report) error {
	files["LIMITS.md"] = []byte(LimitsText)
	files["VERIFY.md"] = []byte(verifyText)
	html, err := renderSummary(m, ctxSummary, segReport, nil)
	if err != nil {
		return err
	}
	files["summary.html"] = html
	letter, err := renderLetter(m, ctxSummary)
	if err != nil {
		return err
	}
	files["letter-template.md"] = letter
	return nil
}

// addManifest lists every file in m, the segment's record seg among them, adds
// manifest.json and, when a TSA is configured, its timestamp, and returns the
// zip order: manifest first.
func addManifest(ctx context.Context, files map[string][]byte, seg File, m *Manifest, o Options) ([]string, error) {
	names := make([]string, 0, len(files)+3)
	for n := range files {
		names = append(names, n)
	}
	names = append(names, seg.Path)
	sort.Strings(names)
	for _, n := range names {
		if n == seg.Path {
			m.Files = append(m.Files, seg)
			continue
		}
		m.Files = append(m.Files, File{Path: n, SHA256: ledger.SHA256Bytes(files[n]), Bytes: int64(len(files[n]))})
	}
	mb, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return nil, err
	}
	mb = append(mb, '\n')
	files["manifest.json"] = mb
	names = append([]string{"manifest.json"}, names...)
	if len(o.TSAs) == 0 {
		return names, nil
	}
	sum := sha256.Sum256(mb)
	der, _, err := tsa.Request(ctx, o.TSAs[0], sum[:], o.Client)
	if err != nil {
		return nil, fmt.Errorf("timestamping manifest: %w", err)
	}
	files["manifest.tsr"] = der
	return append(names, "manifest.tsr"), nil
}

func anchorDir(s *ledger.Store) string { return s.Dir + "/" + ledger.AnchorDir }

func writeZip(path string, names []string, files map[string][]byte, modified time.Time) error {
	return writeZipFrom(path, names, modified, func(name string, w io.Writer) error {
		_, err := w.Write(files[name])
		return err
	})
}

// writeZipFrom writes the zip, calling fill to write each entry's contents.
func writeZipFrom(path string, names []string, modified time.Time, fill func(name string, w io.Writer) error) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	zw := zip.NewWriter(f)
	for _, n := range names {
		w, err := zw.CreateHeader(&zip.FileHeader{Name: n, Method: zip.Deflate, Modified: modified})
		if err == nil {
			err = fill(n, w)
		}
		if err != nil {
			_ = f.Close()
			return err
		}
	}
	if err := zw.Close(); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

type purposeRow struct {
	Name  string
	Share int64
	B     *report.Bucket
}

func rows(t report.Totals) []purposeRow {
	var out []purposeRow
	for _, p := range t.SortedPurposes() {
		b := t.ByPurpose[p]
		share := int64(0)
		if t.All.Calls > 0 {
			share = (b.Calls*100 + t.All.Calls/2) / t.All.Calls
		}
		out = append(out, purposeRow{p, share, b})
	}
	return out
}

func fmtN(n int64) string {
	s := strconv.FormatInt(n, 10)
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	var b strings.Builder
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(c)
	}
	if neg {
		return "-" + b.String()
	}
	return b.String()
}

// VerifierBanner is shown at the top of a report rendered by verify-pack.
type VerifierBanner struct {
	OK          bool
	Verifier    string
	At          string
	Errors      []string
	Warnings    []string
	SummaryNote string
}

// renderSummary renders summary.html. Its output depends only on the manifest,
// the ledger entries and the verification result, so verify-pack can render it
// again and compare it byte for byte with the sender's copy (SPEC §9.3).
func renderSummary(m *Manifest, s *report.Summary, v *ledger.Report, banner *VerifierBanner) ([]byte, error) {
	funcs := template.FuncMap{
		"fmtN":  fmtN,
		"short": ledger.ShortFP,
		"day": func(s string) string {
			if len(s) >= 10 {
				return s[:10]
			}
			return s
		},
		"fmtT": func(t time.Time) string { return ledger.FormatTime(t) },
		"lag":  ledger.FormatLag,
	}
	t, err := template.New("summary").Option("missingkey=error").Funcs(funcs).Parse(summaryTmpl)
	if err != nil {
		return nil, err
	}
	provSet, modelSet := map[string]bool{}, map[string]bool{}
	for p, b := range m.Totals.ByProvider {
		provSet[p] = true
		for mdl := range b.ByModel {
			modelSet[mdl] = true
		}
	}
	keys := func(m map[string]bool) []string {
		var out []string
		for k := range m {
			out = append(out, k)
		}
		sort.Strings(out)
		return out
	}
	rs := rows(m.Totals)
	var top *purposeRow
	if len(rs) > 0 {
		top = &rs[0]
	}
	var buf bytes.Buffer
	err = t.Execute(&buf, map[string]any{
		"M": m, "S": s, "T": m.Totals, "V": v, "Purposes": rs, "Top": top,
		"Providers": keys(provSet), "Models": keys(modelSet), "Limits": LimitsText, "Banner": banner,
	})
	return buf.Bytes(), err
}

// noFirstCommit stands in for the day a code link's history begins when the
// link has none (a path with no commits). It is a placeholder for the sender,
// like the other bracketed parts of the letter.
const noFirstCommit = "[date not recorded]"

// renderLetter fills letterTmpl. Every key the template reads has a value, and
// a missing key is an error, so the letter can never print "<no value>".
func renderLetter(m *Manifest, s *report.Summary) ([]byte, error) {
	t := ttemplate.Must(ttemplate.New("letter").Option("missingkey=error").Parse(letterTmpl))
	rs := rows(m.Totals)
	d := map[string]any{"Provider": m.ProviderFilter, "Account": m.Account, "From": m.Period.From, "To": m.Period.To,
		"Calls": fmtN(m.Totals.All.Calls), "Keys": len(s.Keys),
		"TopPurpose": "[purpose]", "TopShare": "[share]", "TopLink": "", "TopLinkSince": ""}
	if d["Provider"] == "" {
		d["Provider"] = "[provider]"
	}
	if len(rs) > 0 {
		d["TopPurpose"], d["TopShare"] = rs[0].Name, rs[0].Share
		if link, since := codeLinkFor(s, rs[0].Name); link != "" {
			d["TopLink"], d["TopLinkSince"] = link, cmp.Or(since, noFirstCommit)
		}
	}
	var b bytes.Buffer
	if err := t.Execute(&b, d); err != nil {
		return nil, fmt.Errorf("rendering letter: %w", err)
	}
	return b.Bytes(), nil
}

// codeLinkFor returns "repo @ short-commit" and the first commit day of the
// last code link for purpose, or "" when there is none.
func codeLinkFor(s *report.Summary, purpose string) (link, since string) {
	for _, cl := range s.CodeLinks {
		if cl.Str("purpose") != purpose {
			continue
		}
		commit := cl.Str("commit")
		link = cl.Str("repo") + " @ " + commit[:min(12, len(commit))]
		since = ""
		if ts := cl.Str("first_commit_ts"); len(ts) >= 10 {
			since = ts[:10]
		}
	}
	return link, since
}

// ---------------------------------------------------------------- verify-pack

// PackReport is the outcome of VerifyPack.
type PackReport struct {
	Manifest *Manifest
	// ReportHTML is the summary rendered by this verifier from the verified
	// ledger, with a verification banner. Reviewers should read this, not the
	// sender's summary.html.
	ReportHTML []byte
	// SummaryMatches is true when the sender's summary.html is byte-identical
	// to a re-render from the verified data.
	SummaryMatches bool
	Ledger         *ledger.Report
	Errors         []string
	Warnings       []string
	Notes          []string
}

func (r *PackReport) OK() bool { return len(r.Errors) == 0 && r.Ledger != nil && r.Ledger.OK() }

// maxPackBytes is how much a pack may hold uncompressed, in all or in one file.
const maxPackBytes = 1 << 30

// ReadPack loads all files from a pack zip.
func ReadPack(path string) (map[string][]byte, error) {
	files, _, err := scanPack(path, "")
	return files, err
}

// streamedFile is what scanPack keeps of the file it hashes instead of loading.
type streamedFile struct {
	present bool
	sum     string // "sha256:" and the hex of the file's SHA-256
}

// scanPack reads the zip at path as ReadPack does, except that the file called
// stream is hashed while it is read and not kept. Its contents are read again
// with openEntry when they are needed.
func scanPack(path, stream string) (map[string][]byte, streamedFile, error) {
	var sf streamedFile
	zr, err := zip.OpenReader(path)
	if err != nil {
		return nil, sf, err
	}
	defer func() { _ = zr.Close() }()
	files := map[string][]byte{}
	seen := map[string]bool{}
	var total int64
	for _, f := range zr.File {
		if err := checkEntryName(f, seen); err != nil {
			return nil, sf, err
		}
		if f.UncompressedSize64 > maxPackBytes {
			return nil, sf, fmt.Errorf("%s: too large", f.Name)
		}
		rc, err := f.Open()
		if err != nil {
			return nil, sf, err
		}
		n, err := readEntry(rc, f.Name == stream, maxPackBytes-total+1, files, &sf, f.Name)
		_ = rc.Close()
		if err != nil {
			return nil, sf, err
		}
		if total += n; total > maxPackBytes {
			return nil, sf, errors.New("pack is larger than 1 GiB uncompressed")
		}
		seen[f.Name] = true
	}
	return files, sf, nil
}

// readEntry reads up to limit bytes of a zip entry into files, or only hashes
// them when stream is set. It returns how many bytes it read.
func readEntry(rc io.Reader, stream bool, limit int64, files map[string][]byte, sf *streamedFile, name string) (int64, error) {
	if !stream {
		b, err := io.ReadAll(io.LimitReader(rc, limit))
		if err != nil {
			return 0, err
		}
		files[name] = b
		return int64(len(b)), nil
	}
	h := sha256.New()
	n, err := io.Copy(h, io.LimitReader(rc, limit))
	if err != nil {
		return 0, err
	}
	*sf = streamedFile{present: true, sum: "sha256:" + hex.EncodeToString(h.Sum(nil))}
	return n, nil
}

// openEntry opens one file of the zip at path for reading.
func openEntry(path, name string) (io.ReadCloser, error) {
	zr, err := zip.OpenReader(path)
	if err != nil {
		return nil, err
	}
	for _, f := range zr.File {
		if f.Name != name {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			_ = zr.Close()
			return nil, err
		}
		return entryReader{rc, zr}, nil
	}
	_ = zr.Close()
	return nil, fmt.Errorf("%s: not in the pack", name)
}

// entryReader closes the zip with the entry.
type entryReader struct {
	io.ReadCloser
	zr *zip.ReadCloser
}

func (e entryReader) Close() error {
	err := e.ReadCloser.Close()
	_ = e.zr.Close()
	return err
}

// checkEntryName refuses the zip entries that could land outside the pack when
// someone unzips it, or that hide one file behind another: names that are not a
// plain relative path, repeated names, and anything but a regular file.
func checkEntryName(f *zip.File, seen map[string]bool) error {
	if !fs.ValidPath(f.Name) || strings.Contains(f.Name, `\`) {
		return fmt.Errorf("zip entry %q: unsafe name (want a relative path with no \"..\" or empty parts and no backslash)", f.Name)
	}
	if seen[f.Name] {
		return fmt.Errorf("zip entry %q: appears more than once", f.Name)
	}
	if !f.Mode().IsRegular() {
		return fmt.Errorf("zip entry %q: not a regular file", f.Name)
	}
	return nil
}

// VerifyOptions configures VerifyPack.
type VerifyOptions struct {
	Roots         *x509.CertPool // pinned TSA roots (trust.Pool); required
	AllowUnpinned bool
	Generator     string       // this verifier's generator string, e.g. ambermoth/0.2.0
	Clock         ledger.Clock // required: the time stamped on the verifier's report
}

// VerifyPack checks a pack offline (SPEC §9.3).
func VerifyPack(path string, o VerifyOptions) (*PackReport, error) {
	if o.Clock == nil {
		return nil, ledger.ErrNilClock
	}
	files, seg, err := scanPack(path, segmentName)
	if err != nil {
		return nil, err
	}
	c, err := newPackCheck(files, o)
	if err != nil {
		return nil, err
	}
	c.path, c.segFile = path, seg
	c.checkFiles()
	c.checkRequired()
	c.checkGenesis()
	if err := c.checkSegment(); err != nil {
		return nil, err
	}
	c.checkAnchoringClaims()
	c.checkContext()
	c.checkDisclosures()
	c.checkTotals()
	c.checkManifestTimestamp()
	c.addNotes()
	c.renderReport()
	return c.r, nil
}

// packCheck holds the state of one VerifyPack run. The segment is not in
// files: it is streamed out of the zip at path, and seg is what the checks
// keep of it.
type packCheck struct {
	o          VerifyOptions
	path       string
	files      map[string][]byte
	segFile    streamedFile
	mb         []byte // manifest.json as stored
	m          *Manifest
	r          *PackReport
	genesis    []byte // trimmed genesis line
	seg        segmentFacts
	ctxEntries []ledger.Entry
	ctxErr     error
	ctxPresent bool
	filter     report.Filter
	periodOK   bool
	totals     report.Totals   // recomputed from the segment
	summary    *report.Summary // context entries, then the segment
}

// segmentFacts is what the checks need of the segment's lines, taken as they
// stream past.
type segmentFacts struct {
	n         int          // lines, readable or not
	first     ledger.Entry // nil when the line did not parse
	firstLine []byte
	last      ledger.Entry // nil when the line did not parse
	end       int64        // where the last line ends
}

func (f *segmentFacts) see(e ledger.Entry, line []byte, off int64) {
	if f.n == 0 {
		f.first, f.firstLine = e, append([]byte(nil), line...)
	}
	f.last = e
	f.n++
	f.end = off + int64(len(line))
}

func newPackCheck(files map[string][]byte, o VerifyOptions) (*packCheck, error) {
	mb, ok := files["manifest.json"]
	if !ok {
		return nil, errors.New("not an evidence pack: manifest.json missing")
	}
	var m Manifest
	if err := json.Unmarshal(mb, &m); err != nil {
		return nil, fmt.Errorf("manifest.json: %w", err)
	}
	c := &packCheck{o: o, files: files, mb: mb, m: &m, r: &PackReport{Manifest: &m}}
	c.filter, c.periodOK = periodFilter(&m)
	if m.Format != Format {
		c.errf("unsupported pack format %q (this verifier supports %s)", m.Format, Format)
	}
	return c, nil
}

func (c *packCheck) errf(f string, a ...any) { c.r.Errors = append(c.r.Errors, fmt.Sprintf(f, a...)) }

func (c *packCheck) warnf(f string, a ...any) {
	c.r.Warnings = append(c.r.Warnings, fmt.Sprintf(f, a...))
}

// checkFiles compares the zip's files with the manifest's list and hashes.
func (c *packCheck) checkFiles() {
	listed := map[string]bool{"manifest.json": true, "manifest.tsr": true}
	for _, f := range c.m.Files {
		listed[f.Path] = true
		sum, ok := c.fileSum(f.Path)
		switch {
		case !ok:
			c.errf("%s: listed in manifest but missing", f.Path)
		case sum != f.SHA256:
			c.errf("%s: SHA-256 does not match manifest (file altered)", f.Path)
		}
	}
	for n := range c.files {
		if !listed[n] {
			c.errf("%s: present in zip but not listed in manifest", n)
		}
	}
	if c.segFile.present && !listed[segmentName] {
		c.errf("%s: present in zip but not listed in manifest", segmentName)
	}
}

// has reports whether the pack holds a file called name.
func (c *packCheck) has(name string) bool {
	_, ok := c.fileSum(name)
	return ok
}

// fileSum returns the SHA-256 of a file of the pack, and whether it is there.
func (c *packCheck) fileSum(name string) (string, bool) {
	if name == segmentName {
		return c.segFile.sum, c.segFile.present
	}
	b, ok := c.files[name]
	if !ok {
		return "", false
	}
	return ledger.SHA256Bytes(b), true
}

// alwaysFiles are in every pack whatever its manifest lists.
//
//rules:readonly fixed by the pack format (SPEC 9.2)
var alwaysFiles = []string{"ledger/genesis.json", "ledger/segment.jsonl", "summary.html", "LIMITS.md", "VERIFY.md", "letter-template.md"}

const (
	contextFile     = "ledger/context.jsonl"
	disclosuresFile = "disclosures/entry-keys.json"
)

// checkRequired asks for every file the format puts in all packs. The manifest
// lists what the sender chose to include, so it cannot say what may be left out.
// Context and disclosures depend on the manifest's counts and are checked on their own.
func (c *packCheck) checkRequired() {
	for _, n := range alwaysFiles {
		if !c.has(n) {
			c.errf("%s: required by the pack format but missing", n)
		}
	}
}

// checkGenesis checks genesis.json is a valid genesis entry of the manifest's ledger.
func (c *packCheck) checkGenesis() {
	c.genesis = bytes.TrimSpace(c.files["ledger/genesis.json"])
	if len(c.genesis) > ledger.MaxLineBytes {
		c.errf("ledger/genesis.json: line 1 is longer than %d MiB", ledger.MaxLineBytes>>20)
		return
	}
	gv, err := canon.Parse(c.genesis)
	gm, ok := gv.(map[string]any)
	if err != nil || !ok {
		c.errf("ledger/genesis.json: unparseable")
		return
	}
	g := ledger.Entry(gm)
	gr := ledger.Verify([]ledger.Entry{g}, [][]byte{c.genesis}, ledger.VerifyOptions{})
	if !gr.OK() || !gr.StartsAtGenesis {
		c.errf("ledger/genesis.json: invalid genesis entry: %v", gr.Errors)
	}
	if g.Str("ledger_id") != c.m.LedgerID {
		c.errf("genesis ledger_id %s does not match manifest %s", g.Str("ledger_id"), c.m.LedgerID)
	}
}

// checkSegment verifies the segment's chain and anchors and that its ends
// match the manifest. It streams the segment out of the zip once, which
// also gives the summaries the report needs.
func (c *packCheck) checkSegment() error {
	c.loadContext()
	all, segOnly := c.summarizers()
	opt := ledger.VerifyOptions{
		LoadToken:   c.anchorToken,
		CheckAnchor: ops.AnchorVerifier(c.o.Roots, c.o.AllowUnpinned),
		OnEntry: func(e ledger.Entry, line []byte, off int64) {
			c.seg.see(e, line, off)
			all.Add(e)
			if segOnly != all {
				segOnly.Add(e)
			}
		},
	}
	rep, err := c.verifySegment(opt, -1)
	if err != nil {
		return err
	}
	c.r.Ledger = rep
	c.summary = all.Summary()
	c.totals = c.summary.Totals
	if segOnly != all {
		c.totals = segOnly.Summary().Totals
	}
	if c.seg.n == 0 || c.seg.first == nil {
		return nil
	}
	c.checkSegmentStart(c.seg.first, c.seg.firstLine)
	c.checkSegmentEnd(c.seg.last)
	return nil
}

// verifySegment runs the ledger check over the segment, or over its first
// limit bytes when limit is not negative. A line too long to read ends the
// segment there: what came before it is checked and the error is in the report.
func (c *packCheck) verifySegment(opt ledger.VerifyOptions, limit int64) (*ledger.Report, error) {
	var src io.Reader = strings.NewReader("")
	if c.segFile.present {
		rc, err := openEntry(c.path, segmentName)
		if err != nil {
			return nil, err
		}
		defer func() { _ = rc.Close() }()
		src = rc
	}
	if limit >= 0 {
		src = io.LimitReader(src, limit)
	}
	trap := &errTrap{r: src}
	rep, err := ledger.VerifyReader(trap, opt)
	if err == nil {
		return rep, nil
	}
	if trap.err != nil || limit >= 0 {
		return nil, err
	}
	c.errf("%s: %v", segmentName, err)
	opt.OnEntry = nil
	return c.verifySegment(opt, c.seg.end)
}

// errTrap remembers the first error its reader gave other than the end of input.
type errTrap struct {
	r   io.Reader
	err error
}

func (t *errTrap) Read(p []byte) (int, error) {
	n, err := t.r.Read(p)
	if err != nil && err != io.EOF && t.err == nil {
		t.err = err
	}
	return n, err
}

// summarizers returns what totals and the report are computed from. The
// context entries come first in all. The totals only count calls and the
// amendments that retag them, so when the context holds none of those the
// segment alone gives the same totals, and one summarizer does for both.
func (c *packCheck) summarizers() (all, segOnly *report.Summarizer) {
	all = report.NewSummarizer(c.filter)
	segOnly = all
	for _, e := range c.ctxEntries {
		if e.Type() == ledger.TypeCall || e.Type() == ledger.TypeAmendment {
			segOnly = report.NewSummarizer(c.filter)
		}
		all.Add(e)
	}
	return all, segOnly
}

// loadContext reads the context file, which is small. A file that does not
// parse is reported by checkContext, and counts as no entries.
func (c *packCheck) loadContext() {
	cb, present := c.files[contextFile]
	ce, _, err := ledger.Read(bytes.NewReader(cb))
	c.ctxPresent, c.ctxErr = present, err
	if err == nil {
		c.ctxEntries = ce
	}
}

func (c *packCheck) checkSegmentStart(first ledger.Entry, line []byte) {
	ms := c.m.Segment
	if first.Seq() != ms.FirstSeq || first.Str("prev_hash") != ms.FirstPrevHash {
		c.errf("segment start does not match manifest")
	}
	if first.Seq() == 0 && !bytes.Equal(bytes.TrimSpace(line), c.genesis) {
		c.errf("segment starts at seq 0 but differs from genesis.json")
	}
}

// checkSegmentEnd takes the last entry, which is nil when its line did not parse.
func (c *packCheck) checkSegmentEnd(last ledger.Entry) {
	ms := c.m.Segment
	if last == nil || last.Seq() != ms.LastSeq || last.Hash() != ms.LastHash {
		c.errf("segment end does not match manifest")
	}
}

func (c *packCheck) anchorToken(name string) ([]byte, error) {
	b, ok := c.files["anchors/"+name]
	if !ok {
		return nil, os.ErrNotExist
	}
	return b, nil
}

// checkAnchoringClaims checks the manifest's claims about anchored ends.
func (c *packCheck) checkAnchoringClaims() {
	ms := c.m.Segment
	c.checkStartClaim(ms)
	c.checkEndClaim(ms)
	if !ms.StartAnchored && ms.FirstSeq != 0 {
		c.errf("segment neither starts at genesis nor at an anchored entry")
	}
}

func (c *packCheck) checkStartClaim(ms Segment) {
	if ms.StartAnchored && !c.anchorCovers(ms.FirstSeq) {
		c.errf("manifest claims the segment start is anchored, but no valid anchor covers seq %d", ms.FirstSeq)
	}
}

// checkEndClaim requires an anchored end to be an anchor entry: a segment built
// with an anchored end stops at the anchor entries for its head. A bad token on
// that entry is reported by the ledger check.
func (c *packCheck) checkEndClaim(ms Segment) {
	if !ms.EndAnchored {
		c.warnf("the last %d entries of the segment are not anchored (self-attested)", c.r.Ledger.UnanchoredTail)
		return
	}
	if c.r.Ledger.LastAnchoredSeq < 0 {
		c.errf("manifest claims the segment end is anchored, but no valid anchor found")
		return
	}
	if c.seg.n == 0 {
		return
	}
	// a missing, unparsed or moved last entry is already an error from checkSegmentEnd
	if last := c.seg.last; last != nil && last.Seq() == ms.LastSeq && last.Type() != ledger.TypeAnchor {
		c.errf("manifest claims the segment end is anchored, but the last entry (seq %d) is not an anchor", ms.LastSeq)
	}
}

// anchorCovers reports whether a valid anchor in the segment targets seq.
func (c *packCheck) anchorCovers(seq int64) bool {
	for _, a := range c.r.Ledger.Anchors {
		if a.OK && a.TargetSeq == seq {
			return true
		}
	}
	return false
}

// checkContext checks the context entries are self-consistent and precede the segment.
func (c *packCheck) checkContext() {
	if c.ctxErr != nil {
		c.errf("%s: %v", contextFile, c.ctxErr)
		return
	}
	ce := c.ctxEntries
	for _, e := range ce {
		c.checkContextEntry(e)
	}
	if len(ce) != c.m.ContextEntries {
		c.errf("manifest lists %d context entries but %s has %d", c.m.ContextEntries, contextFile, len(ce))
	}
	if !c.ctxPresent {
		return
	}
	c.r.Notes = append(c.r.Notes, fmt.Sprintf("%d context entries (key history / code links before the segment) are self-consistent; their chain membership is not covered by this segment", len(ce)))
}

func (c *packCheck) checkContextEntry(e ledger.Entry) {
	if e == nil {
		c.errf("ledger/context.jsonl: unparseable line")
		return
	}
	if h, err := ledger.ComputeHash(e); err != nil || h != e.Hash() {
		c.errf("context entry seq %d: hash mismatch", e.Seq())
	}
	if e.Seq() >= c.m.Segment.FirstSeq {
		c.errf("context entry seq %d is not before the segment", e.Seq())
	}
}

// checkDisclosures requires the key file exactly when the manifest lists
// disclosed entries, and a key for each of them and for no others.
func (c *packCheck) checkDisclosures() {
	b, present := c.files[disclosuresFile]
	if !present {
		if len(c.m.Disclosed) > 0 {
			c.errf("%s: required by the pack format (the manifest lists %d disclosed entries) but missing", disclosuresFile, len(c.m.Disclosed))
		}
		return
	}
	var keys map[string]string
	if err := json.Unmarshal(b, &keys); err != nil {
		c.errf("%s: %v", disclosuresFile, err)
		return
	}
	want := map[string]bool{}
	for _, q := range c.m.Disclosed {
		want[strconv.FormatInt(q, 10)] = true
	}
	for _, k := range diffKeys(keys, want) {
		c.errf("%s: has a key for seq %s, which the manifest does not list as disclosed", disclosuresFile, k)
	}
	for _, k := range diffKeys(want, keys) {
		c.errf("%s: has no key for seq %s, which the manifest lists as disclosed", disclosuresFile, k)
	}
}

// diffKeys returns the sorted keys of a that b does not have.
func diffKeys[A, B any](a map[string]A, b map[string]B) []string {
	var out []string
	for k := range a {
		if _, ok := b[k]; !ok {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// periodFilter is the filter the manifest's period and provider describe, and
// false when the period is not two dates. A bad date counts as the zero time.
func periodFilter(m *Manifest) (report.Filter, bool) {
	from, errFrom := time.Parse("2006-01-02", m.Period.From)
	to, errTo := time.Parse("2006-01-02", m.Period.To)
	return report.Filter{From: from, To: to.AddDate(0, 0, 1), Provider: m.ProviderFilter}, errFrom == nil && errTo == nil
}

// checkTotals compares the usage totals recomputed from the segment with the
// manifest's.
func (c *packCheck) checkTotals() {
	if !c.periodOK {
		c.errf("manifest period %q to %q is not two YYYY-MM-DD dates", c.m.Period.From, c.m.Period.To)
	}
	got, err := canon.FromStruct(c.totals)
	if err != nil {
		c.errf("recomputed totals: %v", err)
		return
	}
	claimed, err := canon.FromStruct(c.m.Totals)
	if err != nil || !bytes.Equal(got, claimed) {
		c.errf("usage totals in manifest do not match totals recomputed from the ledger segment")
	}
}

// checkManifestTimestamp verifies manifest.tsr, when present.
func (c *packCheck) checkManifestTimestamp() {
	tok, ok := c.files["manifest.tsr"]
	if !ok {
		return
	}
	sum := sha256.Sum256(c.mb)
	chk, err := ops.AnchorVerifier(c.o.Roots, c.o.AllowUnpinned)(tok, "sha256:"+hex.EncodeToString(sum[:]))
	if err != nil {
		c.errf("manifest.tsr: %v", err)
		return
	}
	pin := ""
	if !chk.Pinned {
		pin = " (TSA NOT pinned)"
	}
	c.r.Notes = append(c.r.Notes, "manifest timestamped by "+chk.Signer+pin+" at "+ledger.FormatTime(chk.GenTime))
}

func (c *packCheck) addNotes() {
	if len(c.m.Disclosed) > 0 {
		c.r.Notes = append(c.r.Notes, fmt.Sprintf("%d entr(ies) disclosed for sample checking: %v", len(c.m.Disclosed), c.m.Disclosed))
	}
	for _, a := range c.r.Ledger.Anchors {
		if a.OK && !a.Pinned {
			c.warnf("anchor seq %d: TSA %s is NOT pinned (accepted only because --allow-unpinned-tsa was given)", a.Seq, a.Signer)
		}
	}
}

// renderReport re-renders the summary from the verified data, compares it
// with the sender's summary.html (SPEC section 9.3) and renders the verifier's report.
func (c *packCheck) renderReport() {
	rm := *c.m
	rm.Totals = c.totals
	ctxSummary := c.summary
	note := c.compareSummary(&rm, ctxSummary)
	r := c.r
	banner := &VerifierBanner{OK: r.OK(), Verifier: c.o.Generator, At: ledger.FormatTime(c.o.Clock().UTC()),
		Errors: append(append([]string{}, r.Errors...), r.Ledger.Errors...), Warnings: append(append([]string{}, r.Warnings...), r.Ledger.Warnings...), SummaryNote: note}
	if html, err := renderSummary(&rm, ctxSummary, r.Ledger, banner); err == nil {
		r.ReportHTML = html
	}
}

// compareSummary checks the sender's summary.html against a re-render and
// returns the note for the verifier's banner.
func (c *packCheck) compareSummary(rm *Manifest, ctxSummary *report.Summary) string {
	if c.o.Generator != "" && c.m.Generator != c.o.Generator {
		note := fmt.Sprintf("summary.html was made by %s; this verifier is %s, so it cannot re-render it byte for byte. This report is rendered from the verified ledger.", c.m.Generator, c.o.Generator)
		c.r.Warnings = append(c.r.Warnings, note)
		return note
	}
	again, err := renderSummary(rm, ctxSummary, c.r.Ledger, nil)
	if err != nil || !bytes.Equal(again, c.files["summary.html"]) {
		c.errf("summary.html does not match a re-render from the verified ledger: it was edited after the pack was built. Read the verifier's report instead.")
		return ""
	}
	c.r.SummaryMatches = true
	return "The sender's summary.html is identical to this report's content."
}

// CheckSample verifies original request/response bytes against a disclosed entry.
func CheckSample(packPath string, seq int64, request, response []byte) (bool, bool, error) {
	files, err := ReadPack(packPath)
	if err != nil {
		return false, false, err
	}
	var keys map[string]string
	if err := json.Unmarshal(files["disclosures/entry-keys.json"], &keys); err != nil {
		return false, false, errors.New("pack has no disclosures")
	}
	kh, ok := keys[strconv.FormatInt(seq, 10)]
	if !ok {
		return false, false, fmt.Errorf("seq %d was not disclosed", seq)
	}
	key, err := hex.DecodeString(kh)
	if err != nil {
		return false, false, err
	}
	seg, _, _ := ledger.Read(bytes.NewReader(files["ledger/segment.jsonl"]))
	for _, e := range seg {
		if e != nil && e.Seq() == seq {
			return hmacEq(key, request, e.Str("prompt_hmac")), hmacEq(key, response, e.Str("output_hmac")), nil
		}
	}
	return false, false, fmt.Errorf("seq %d not in segment", seq)
}

func hmacEq(key, data []byte, want string) bool {
	if data == nil {
		return false
	}
	m := hmac.New(sha256.New, key)
	m.Write(data)
	return hmac.Equal([]byte(ledger.FormatHMAC(m.Sum(nil))), []byte(want))
}
