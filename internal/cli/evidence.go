package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/devrdias/ambermoth/internal/ops"
	"github.com/devrdias/ambermoth/internal/pack"
	"github.com/devrdias/ambermoth/internal/report"
)

// packArgs is a parsed 'ambermoth pack' command line.
type packArgs struct {
	dir, out      string
	noTSA         bool
	tsaCAs        multi
	allowUnpinned bool
	opts          pack.Options
}

// parsePackArgs reads and checks the pack flags. Fields of opts that need the
// ledger (TSAs, Roots) are left for cmdPack to fill in.
func (a *App) parsePackArgs(args []string) (*packArgs, error) {
	fs, dir := a.newFlags("pack")
	pa := &packArgs{}
	from := fs.String("from", "", "first day, YYYY-MM-DD (required)")
	to := fs.String("to", "", "last day, YYYY-MM-DD inclusive (required)")
	provider := fs.String("provider", "", "limit itemised usage to one provider")
	account := fs.String("account", "", "account / organisation identifier to print on the cover")
	disclose := fs.String("disclose", "", "comma-separated seqs whose per-entry keys to disclose")
	out := fs.String("out", "", "output zip (default evidence-pack-<from>-<to>.zip)")
	fs.BoolVar(&pa.noTSA, "no-tsa", false, "do not timestamp the manifest")
	fs.Var(&pa.tsaCAs, "tsa-ca", "PEM file of an extra trusted TSA root (repeatable)")
	fs.BoolVar(&pa.allowUnpinned, "allow-unpinned-tsa", false, "accept anchors from TSAs that do not chain to a pinned root")
	if _, err := parse(fs, args); err != nil {
		return nil, err
	}
	if *from == "" || *to == "" {
		return nil, errors.New("--from and --to are required")
	}
	f, err := parseDay("from", *from)
	if err != nil {
		return nil, err
	}
	t, err := parseDay("to", *to)
	if err != nil {
		return nil, err
	}
	seqs, err := parseSeqs(*disclose)
	if err != nil {
		return nil, err
	}
	pa.dir, pa.out = *dir, *out
	if pa.out == "" {
		pa.out = fmt.Sprintf("evidence-pack-%s-%s.zip", *from, *to)
	}
	pa.opts = pack.Options{From: f, To: t.AddDate(0, 0, 1), Provider: *provider, Account: *account,
		Disclose: seqs, AllowUnpinned: pa.allowUnpinned, Out: pa.out, Generator: a.generator()}
	return pa, nil
}

func (a *App) cmdPack(ctx context.Context, args []string) error {
	pa, err := a.parsePackArgs(args)
	if err != nil {
		return err
	}
	s, cfg, err := a.open(pa.dir)
	if err != nil {
		return err
	}
	if pa.opts.Roots, err = ops.TrustPool(pa.tsaCAs); err != nil {
		return err
	}
	if !pa.noTSA {
		pa.opts.TSAs = cfg.TSAs
	}
	m, err := pack.Build(ctx, s, pa.opts)
	if err != nil {
		return err
	}
	printPackWritten(a.out, m, pa.out)
	return nil
}

func printPackWritten(w io.Writer, m *pack.Manifest, out string) {
	_, _ = fmt.Fprintf(w, "Evidence pack written: %s\n", out)
	_, _ = fmt.Fprintf(w, "  %d calls, %d purposes, segment seq %d…%d (start anchored: %v, end anchored: %v)\n",
		m.Totals.All.Calls, len(m.Totals.ByPurpose), m.Segment.FirstSeq, m.Segment.LastSeq, m.Segment.StartAnchored, m.Segment.EndAnchored)
	_, _ = fmt.Fprintln(w, "  Next: write your explanation in letter-template.md (inside the zip). Don't edit summary.html:")
	_, _ = fmt.Fprintln(w, "        verify-pack re-renders it from the ledger and rejects any change. Then check it yourself:")
	_, _ = fmt.Fprintf(w, "        ambermoth verify-pack %s --report report.html\n", out)
}

// parseSeqs parses a comma-separated list of entry sequence numbers.
func parseSeqs(list string) ([]int64, error) {
	var seqs []int64
	for _, p := range strings.Split(list, ",") {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		n, err := strconv.ParseInt(p, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("--disclose: %w", err)
		}
		seqs = append(seqs, n)
	}
	return seqs, nil
}

func (a *App) cmdVerifyPack(_ context.Context, args []string) error {
	fs, _ := a.newFlags("verify-pack")
	var cas multi
	fs.Var(&cas, "tsa-ca", "PEM file of an extra trusted TSA root (repeatable; FreeTSA and DigiCert are pinned by default)")
	allowUnpinned := fs.Bool("allow-unpinned-tsa", false, "accept anchors from TSAs that do not chain to a pinned root")
	reportPath := fs.String("report", "", "write the verifier's own HTML report here (read this, not the sender's summary.html)")
	asJSON := fs.Bool("json", false, "machine-readable output")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return errors.New("usage: ambermoth verify-pack <pack.zip> [--report report.html] [--tsa-ca root.pem]")
	}
	roots, err := ops.TrustPool(cas)
	if err != nil {
		return err
	}
	r, err := pack.VerifyPack(pos[0], pack.VerifyOptions{Roots: roots, AllowUnpinned: *allowUnpinned, Generator: a.generator(), Clock: a.env.Clock})
	if err != nil {
		return err
	}
	if err := writeVerifierReport(*reportPath, r.ReportHTML); err != nil {
		return err
	}
	if *asJSON {
		err = writeJSON(a.out, map[string]any{"ok": r.OK(), "errors": r.Errors, "warnings": r.Warnings,
			"notes": r.Notes, "ledger": r.Ledger, "manifest": r.Manifest})
	} else {
		printPackReport(a.out, r, *reportPath)
	}
	if err != nil {
		return err
	}
	if !r.OK() {
		return exitError(ExitFail)
	}
	return nil
}

// writeVerifierReport saves the verifier's HTML report; it does nothing when
// no path was asked for or the pack produced no report.
func writeVerifierReport(path string, html []byte) error {
	if path == "" || html == nil {
		return nil
	}
	return os.WriteFile(path, html, 0o644)
}

func printPackReport(w io.Writer, r *pack.PackReport, reportPath string) {
	m := r.Manifest
	_, _ = fmt.Fprintf(w, "Evidence pack verification: %s\n", status(r.OK()))
	_, _ = fmt.Fprintf(w, "  format %s · ledger %s · account %s\n  period %s → %s · %d calls\n", m.Format, m.LedgerID, m.Account, m.Period.From, m.Period.To, m.Totals.All.Calls)
	_, _ = fmt.Fprintf(w, "  files: %d checked · totals recomputed from the ledger segment\n", len(m.Files))
	if r.SummaryMatches {
		_, _ = fmt.Fprintln(w, "  summary.html: identical to a re-render from the verified ledger")
	}
	printLedgerReport(w, r.Ledger)
	printLines(w, "  ERROR:", r.Errors)
	printLines(w, "  warning:", r.Warnings)
	printLines(w, "  note:", r.Notes)
	if reportPath != "" {
		_, _ = fmt.Fprintf(w, "  report: %s (rendered by this verifier from the ledger)\n", reportPath)
	}
}

func (a *App) cmdCheckSample(_ context.Context, args []string) error {
	fs, _ := a.newFlags("check-sample")
	packPath := fs.String("pack", "", "pack zip")
	seq := fs.Int64("seq", -1, "disclosed entry seq")
	req := fs.String("request", "", "file with the exact request body bytes")
	resp := fs.String("response", "", "file with the exact response body bytes")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	rb, err := readOptional(*req)
	if err != nil {
		return err
	}
	sb, err := readOptional(*resp)
	if err != nil {
		return err
	}
	okReq, okResp, err := pack.CheckSample(*packPath, *seq, rb, sb)
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintf(a.out, "seq %d request:  %s\nseq %d response: %s\n", *seq, sampleResult(rb != nil, okReq), *seq, sampleResult(sb != nil, okResp))
	if (rb != nil && !okReq) || (sb != nil && !okResp) {
		return exitError(ExitFail)
	}
	return nil
}

// readOptional reads path, or returns nil when path is empty.
func readOptional(path string) ([]byte, error) {
	if path == "" {
		return nil, nil
	}
	return os.ReadFile(path)
}

func sampleResult(given, ok bool) string {
	switch {
	case !given:
		return "not checked"
	case ok:
		return "MATCHES the ledger"
	}
	return "DOES NOT MATCH"
}

func (a *App) cmdReconcile(_ context.Context, args []string) error {
	fs, dir := a.newFlags("reconcile")
	provider := fs.String("provider", "", "provider name as used by the proxy (required)")
	csvPath := fs.String("csv", "", "usage export CSV: date,model,input_tokens,output_tokens (required)")
	tol := fs.Float64("tolerance", 0.02, "allowed relative difference per day, from 0 to 1 (0.02 is 2%)")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	if *provider == "" || *csvPath == "" {
		return errors.New("--provider and --csv are required")
	}
	s, _, err := a.open(*dir)
	if err != nil {
		return err
	}
	e, lines, err := ops.Reconcile(s, *provider, *csvPath, *tol)
	if err != nil {
		return err
	}
	printLines(a.out, " ", lines)
	_, _ = fmt.Fprintf(a.out, "reconciliation %s, recorded at seq %d\n", strings.ToUpper(e.Str("result")), e.Seq())
	return nil
}

func (a *App) cmdAlerts(_ context.Context, args []string) error {
	fs, dir := a.newFlags("alerts")
	asJSON := fs.Bool("json", false, "machine-readable output")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	s, _, err := a.open(*dir)
	if err != nil {
		return err
	}
	es, err := s.Entries()
	if err != nil {
		return err
	}
	as := report.Alerts(es, a.now(), report.AlertConfig{})
	if *asJSON {
		return writeJSON(a.out, as)
	}
	if len(as) == 0 {
		_, _ = fmt.Fprintln(a.out, "No alerts. (Alerts are advisory: they point at traffic worth explaining, not at wrongdoing.)")
	}
	for _, al := range as {
		_, _ = fmt.Fprintf(a.out, "[review · %s] %s\n", al.Kind, al.Message)
	}
	return nil
}

func (a *App) cmdReport(_ context.Context, args []string) error {
	fs, dir := a.newFlags("report")
	from := fs.String("from", "", "first day YYYY-MM-DD")
	to := fs.String("to", "", "last day YYYY-MM-DD")
	provider := fs.String("provider", "", "provider filter")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	f, err := reportFilter(*from, *to, *provider)
	if err != nil {
		return err
	}
	s, _, err := a.open(*dir)
	if err != nil {
		return err
	}
	es, err := s.Entries()
	if err != nil {
		return err
	}
	printSummary(a.out, report.Summarize(es, f))
	return nil
}

func reportFilter(from, to, provider string) (report.Filter, error) {
	f := report.Filter{Provider: provider}
	if from != "" {
		t, err := parseDay("from", from)
		if err != nil {
			return f, err
		}
		f.From = t
	}
	if to != "" {
		t, err := parseDay("to", to)
		if err != nil {
			return f, err
		}
		f.To = t.AddDate(0, 0, 1)
	}
	return f, nil
}

func printSummary(w io.Writer, sum *report.Summary) {
	t := sum.Totals
	_, _ = fmt.Fprintf(w, "%d calls · %d tokens in · %d tokens out\n\n", t.All.Calls, t.All.TokensIn, t.All.TokensOut)
	_, _ = fmt.Fprintf(w, "%-24s %10s %14s %14s %8s\n", "purpose", "calls", "tokens in", "tokens out", "retag")
	for _, p := range t.SortedPurposes() {
		b := t.ByPurpose[p]
		_, _ = fmt.Fprintf(w, "%-24s %10d %14d %14d %8d\n", p, b.Calls, b.TokensIn, b.TokensOut, b.Retagged)
	}
	_, _ = fmt.Fprintf(w, "\n%d key(s), %d code link(s), %d gap(s), %d amendment(s)\n", len(sum.Keys), len(sum.CodeLinks), len(sum.Gaps), len(sum.Amendments))
}
