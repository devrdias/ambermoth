// Package pack builds and verifies evidence packs (SPEC §9).
package pack

import (
	"archive/zip"
	"bytes"
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
	f := o.filter()
	sp, err := src.chooseSpan(f)
	if err != nil {
		return nil, err
	}
	c := src.cutAt(sp)
	files, disclosed, err := c.files(s, src.entries, o.Disclose)
	if err != nil {
		return nil, err
	}
	m := c.manifest(s, o, disclosed)
	if err := addReadables(files, m, c.summary(f), ledger.Verify(c.seg, c.segLines, src.check)); err != nil {
		return nil, err
	}
	names, err := addManifest(ctx, files, m, o)
	if err != nil {
		return nil, err
	}
	return m, writeZip(o.Out, names, files, s.Now())
}

// source is a ledger read for packing, verified from genesis.
type source struct {
	entries []ledger.Entry
	lines   [][]byte
	full    *ledger.Report
	check   ledger.VerifyOptions
}

func readSource(s *ledger.Store, o Options) (*source, error) {
	entries, lines, err := ledger.ReadFile(s.LedgerPath())
	if err != nil {
		return nil, err
	}
	check := ledger.VerifyOptions{AnchorDir: anchorDir(s), CheckAnchor: ops.AnchorVerifier(o.Roots, o.AllowUnpinned)}
	full := ledger.Verify(entries, lines, check)
	if !full.OK() || !full.StartsAtGenesis {
		return nil, fmt.Errorf("the ledger does not verify; refusing to build a pack from it:\n  %s", strings.Join(full.Errors, "\n  "))
	}
	return &source{entries: entries, lines: lines, full: full, check: check}, nil
}

// chooseSpan picks the segment for the calls f matches.
func (src *source) chooseSpan(f report.Filter) (span, error) {
	firstCall, lastCall, ok := callRange(src.entries, f)
	if !ok {
		return span{}, errors.New("no calls in the selected period")
	}
	last := lastRelevantSeq(src.entries, f, firstCall, lastCall)
	return selectSpan(src.full.Anchors, len(src.entries), firstCall, last), nil
}

// cut is the part of the ledger a pack carries: the segment, and the context
// entries before it.
type cut struct {
	span
	genesis    []byte
	seg        []ledger.Entry
	segLines   [][]byte
	ctxEntries []ledger.Entry
	ctxLines   [][]byte
}

func (src *source) cutAt(sp span) cut {
	ctxEntries, ctxLines := contextEntries(src.entries, src.lines, sp.start)
	return cut{span: sp, genesis: append(append([]byte{}, src.lines[0]...), '\n'),
		seg: src.entries[sp.start : sp.end+1], segLines: src.lines[sp.start : sp.end+1],
		ctxEntries: ctxEntries, ctxLines: ctxLines}
}

// files returns the ledger files of the pack, with the anchor tokens and the
// disclosure keys, and the seqs disclosed.
func (c cut) files(s *ledger.Store, entries []ledger.Entry, disclose []int64) (map[string][]byte, []int64, error) {
	files := map[string][]byte{"ledger/genesis.json": c.genesis, "ledger/segment.jsonl": joinLines(c.segLines)}
	if len(c.ctxLines) > 0 {
		files["ledger/context.jsonl"] = joinLines(c.ctxLines)
	}
	if err := addAnchorTokens(files, anchorDir(s), c.seg); err != nil {
		return nil, nil, err
	}
	disclosed, err := addDisclosures(files, s, entries, c.seg, disclose)
	if err != nil {
		return nil, nil, err
	}
	return files, disclosed, nil
}

// manifest describes the cut, without the file list.
func (c cut) manifest(s *ledger.Store, o Options, disclosed []int64) *Manifest {
	last := c.seg[len(c.seg)-1]
	m := &Manifest{
		Format: Format, Generator: o.Generator, Created: ledger.FormatTime(s.Now()), LedgerID: s.LedgerID,
		Account: o.Account, ProviderFilter: o.Provider,
		Period: Period{From: o.From.Format("2006-01-02"), To: o.To.AddDate(0, 0, -1).Format("2006-01-02")},
		Segment: Segment{FirstSeq: c.seg[0].Seq(), LastSeq: last.Seq(), FirstPrevHash: c.seg[0].Str("prev_hash"),
			LastHash: last.Hash(), StartAnchored: c.startAnchored, EndAnchored: c.endAnchored},
		ContextEntries: len(c.ctxLines), Disclosed: disclosed, Totals: report.Summarize(c.seg, o.filter()).Totals,
	}
	if m.Account == "" {
		m.Account = "(account not specified)"
	}
	return m
}

// summary aggregates the context and the segment together.
func (c cut) summary(f report.Filter) *report.Summary {
	return report.Summarize(append(append([]ledger.Entry{}, c.ctxEntries...), c.seg...), f)
}

// callRange returns the first and last seq of the calls f matches.
func callRange(entries []ledger.Entry, f report.Filter) (first, last int64, ok bool) {
	first, last = -1, -1
	for _, e := range entries {
		if !f.Match(e) {
			continue
		}
		if first < 0 {
			first = e.Seq()
		}
		last = e.Seq()
	}
	return first, last, first >= 0
}

// lastRelevantSeq extends the period's last call to the last amendment that
// re-tags a matched call in first..last, so the segment includes it.
func lastRelevantSeq(entries []ledger.Entry, f report.Filter, first, last int64) int64 {
	relevant := last
	for _, e := range entries[last:] {
		if e.Type() == ledger.TypeAmendment && amendsMatchedCall(entries, f, e, first, last) {
			relevant = e.Seq()
		}
	}
	return relevant
}

func amendsMatchedCall(entries []ledger.Entry, f report.Filter, am ledger.Entry, first, last int64) bool {
	for q := max(am.Int("target_from"), first); q <= min(am.Int("target_to"), last); q++ {
		if f.Match(entries[q]) {
			return true
		}
	}
	return false
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

// contextEntries returns the key events, code links and reconciliations
// recorded before the segment starts.
func contextEntries(entries []ledger.Entry, lines [][]byte, start int64) ([]ledger.Entry, [][]byte) {
	var es []ledger.Entry
	var ls [][]byte
	for i := int64(1); i < start; i++ {
		switch entries[i].Type() {
		case ledger.TypeKeyEvent, ledger.TypeCodeLink, ledger.TypeReconciliation:
			es = append(es, entries[i])
			ls = append(ls, lines[i])
		}
	}
	return es, ls
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
// call entries inside seg, and returns their seqs in order.
func addDisclosures(files map[string][]byte, s *ledger.Store, entries, seg []ledger.Entry, seqs []int64) ([]int64, error) {
	disclosed := []int64{}
	if len(seqs) == 0 {
		return disclosed, nil
	}
	keys := map[string]string{}
	for _, q := range seqs {
		if q < seg[0].Seq() || q > seg[len(seg)-1].Seq() || entries[q].Type() != ledger.TypeCall {
			return nil, fmt.Errorf("--disclose %d: not a call entry inside the pack segment", q)
		}
		keys[strconv.FormatInt(q, 10)] = hex.EncodeToString(ledger.EntryKey(s.MasterKey, entries[q].Str("salt_id")))
		disclosed = append(disclosed, q)
	}
	b, err := json.MarshalIndent(keys, "", "  ")
	if err != nil {
		return nil, err
	}
	files["disclosures/entry-keys.json"] = append(b, '\n')
	sort.Slice(disclosed, func(i, j int) bool { return disclosed[i] < disclosed[j] })
	return disclosed, nil
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

// addManifest lists every file in m, adds manifest.json and, when a TSA is
// configured, its timestamp, and returns the zip order: manifest first.
func addManifest(ctx context.Context, files map[string][]byte, m *Manifest, o Options) ([]string, error) {
	names := make([]string, 0, len(files)+2)
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
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

func joinLines(ls [][]byte) []byte {
	var b bytes.Buffer
	for _, l := range ls {
		b.Write(l)
		b.WriteByte('\n')
	}
	return b.Bytes()
}

func writeZip(path string, names []string, files map[string][]byte, modified time.Time) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	zw := zip.NewWriter(f)
	for _, n := range names {
		w, err := zw.CreateHeader(&zip.FileHeader{Name: n, Method: zip.Deflate, Modified: modified})
		if err != nil {
			_ = f.Close()
			return err
		}
		if _, err := w.Write(files[n]); err != nil {
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
	t, err := template.New("summary").Funcs(funcs).Parse(summaryTmpl)
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

func renderLetter(m *Manifest, s *report.Summary) ([]byte, error) {
	t := ttemplate.Must(ttemplate.New("letter").Parse(letterTmpl))
	rs := rows(m.Totals)
	d := map[string]any{"Provider": m.ProviderFilter, "Account": m.Account, "From": m.Period.From, "To": m.Period.To,
		"Calls": fmtN(m.Totals.All.Calls), "Keys": len(s.Keys)}
	if d["Provider"] == "" {
		d["Provider"] = "[provider]"
	}
	if len(rs) > 0 {
		d["TopPurpose"], d["TopShare"] = rs[0].Name, rs[0].Share
		if link, since := codeLinkFor(s, rs[0].Name); link != "" {
			d["TopLink"] = link
			if since != "" {
				d["TopLinkSince"] = since
			}
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

// ReadPack loads all files from a pack zip.
func ReadPack(path string) (map[string][]byte, error) {
	zr, err := zip.OpenReader(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = zr.Close() }()
	const maxTotal = 1 << 30 // 1 GiB uncompressed per pack
	files := map[string][]byte{}
	var total int64
	for _, f := range zr.File {
		if err := checkEntryName(f, files); err != nil {
			return nil, err
		}
		if f.UncompressedSize64 > maxTotal {
			return nil, fmt.Errorf("%s: too large", f.Name)
		}
		rc, err := f.Open()
		if err != nil {
			return nil, err
		}
		b, err := io.ReadAll(io.LimitReader(rc, maxTotal-total+1))
		_ = rc.Close()
		if err != nil {
			return nil, err
		}
		if total += int64(len(b)); total > maxTotal {
			return nil, errors.New("pack is larger than 1 GiB uncompressed")
		}
		files[f.Name] = b
	}
	return files, nil
}

// checkEntryName refuses the zip entries that could land outside the pack when
// someone unzips it, or that hide one file behind another: names that are not a
// plain relative path, repeated names, and anything but a regular file.
func checkEntryName(f *zip.File, seen map[string][]byte) error {
	if !fs.ValidPath(f.Name) || strings.Contains(f.Name, `\`) {
		return fmt.Errorf("zip entry %q: unsafe name (want a relative path with no \"..\" or empty parts and no backslash)", f.Name)
	}
	if _, dup := seen[f.Name]; dup {
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
	files, err := ReadPack(path)
	if err != nil {
		return nil, err
	}
	c, err := newPackCheck(files, o)
	if err != nil {
		return nil, err
	}
	c.checkFiles()
	c.checkRequired()
	c.checkGenesis()
	c.checkSegment()
	c.checkAnchoringClaims()
	c.checkContext()
	c.checkDisclosures()
	c.checkTotals()
	c.checkManifestTimestamp()
	c.addNotes()
	c.renderReport()
	return c.r, nil
}

// packCheck holds the state of one VerifyPack run.
type packCheck struct {
	o          VerifyOptions
	files      map[string][]byte
	mb         []byte // manifest.json as stored
	m          *Manifest
	r          *PackReport
	genesis    []byte // trimmed genesis line
	seg        []ledger.Entry
	ctxEntries []ledger.Entry
	filter     report.Filter
	totals     report.Totals // recomputed from the segment
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
		b, ok := c.files[f.Path]
		switch {
		case !ok:
			c.errf("%s: listed in manifest but missing", f.Path)
		case ledger.SHA256Bytes(b) != f.SHA256:
			c.errf("%s: SHA-256 does not match manifest (file altered)", f.Path)
		}
	}
	for n := range c.files {
		if !listed[n] {
			c.errf("%s: present in zip but not listed in manifest", n)
		}
	}
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
		if _, ok := c.files[n]; !ok {
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
// match the manifest.
func (c *packCheck) checkSegment() {
	seg, lines, err := ledger.Read(bytes.NewReader(c.files["ledger/segment.jsonl"]))
	if err != nil {
		// what was read before the bad line still goes through the checks
		c.errf("ledger/segment.jsonl: %v", err)
	}
	c.seg = seg
	c.r.Ledger = ledger.Verify(seg, lines, ledger.VerifyOptions{
		LoadToken:   c.anchorToken,
		CheckAnchor: ops.AnchorVerifier(c.o.Roots, c.o.AllowUnpinned),
	})
	if len(seg) == 0 || seg[0] == nil {
		return
	}
	c.checkSegmentStart(seg[0], lines[0])
	c.checkSegmentEnd(seg[len(seg)-1])
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

// checkEndClaim only asks for some valid anchor, not one that covers the last entry.
func (c *packCheck) checkEndClaim(ms Segment) {
	if !ms.EndAnchored {
		c.warnf("the last %d entries of the segment are not anchored (self-attested)", c.r.Ledger.UnanchoredTail)
		return
	}
	if c.r.Ledger.LastAnchoredSeq < 0 {
		c.errf("manifest claims the segment end is anchored, but no valid anchor found")
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
	cb, present := c.files[contextFile]
	ce, _, err := ledger.Read(bytes.NewReader(cb))
	if err != nil {
		c.errf("%s: %v", contextFile, err)
		return
	}
	c.ctxEntries = ce
	for _, e := range ce {
		c.checkContextEntry(e)
	}
	if len(ce) != c.m.ContextEntries {
		c.errf("manifest lists %d context entries but %s has %d", c.m.ContextEntries, contextFile, len(ce))
	}
	if !present {
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

// checkTotals recomputes the usage totals from the segment and compares them
// with the manifest's.
func (c *packCheck) checkTotals() {
	from, errFrom := time.Parse("2006-01-02", c.m.Period.From)
	to, errTo := time.Parse("2006-01-02", c.m.Period.To)
	if errFrom != nil || errTo != nil {
		c.errf("manifest period %q to %q is not two YYYY-MM-DD dates", c.m.Period.From, c.m.Period.To)
	}
	c.filter = report.Filter{From: from, To: to.AddDate(0, 0, 1), Provider: c.m.ProviderFilter}
	c.totals = report.Summarize(c.seg, c.filter).Totals
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
	ctxSummary := report.Summarize(append(append([]ledger.Entry{}, c.ctxEntries...), c.seg...), c.filter)
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
