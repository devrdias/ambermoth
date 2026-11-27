package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"

	"github.com/devrdias/ambermoth/internal/ledger"
	"github.com/devrdias/ambermoth/internal/ops"
)

func (a *App) cmdAnchor(ctx context.Context, args []string) error {
	fs, dir := a.newFlags("anchor")
	var tsas multi
	fs.Var(&tsas, "tsa", "TSA URL (repeatable; default from config)")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	s, cfg, err := a.open(*dir)
	if err != nil {
		return err
	}
	if len(tsas) == 0 {
		tsas = cfg.TSAs
	}
	es, err := ops.Anchor(ctx, s, tsas, nil)
	for _, e := range es {
		fmt.Fprintf(a.env.Stdout, "anchored seq %d (%s) via %s at %s\n", e.Int("target_seq"), ledger.ShortFP(e.Str("target_hash")), e.Str("tsa"), e.Str("gen_time"))
	}
	return err
}

func (a *App) cmdVerify(_ context.Context, args []string) error {
	fs, dir := a.newFlags("verify")
	var cas multi
	fs.Var(&cas, "tsa-ca", "PEM file of an extra trusted TSA root (repeatable; FreeTSA and DigiCert are pinned by default)")
	allowUnpinned := fs.Bool("allow-unpinned-tsa", false, "accept anchors from TSAs that do not chain to a pinned root")
	asJSON := fs.Bool("json", false, "machine-readable output")
	file := fs.String("file", "", "verify a ledger file directly (anchors from --anchors)")
	anchors := fs.String("anchors", "", "anchor token directory for --file")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	path, adir := filepath.Join(*dir, ledger.LedgerFile), filepath.Join(*dir, ledger.AnchorDir)
	if *file != "" {
		path, adir = *file, *anchors
	}
	roots, err := ops.TrustPool(cas)
	if err != nil {
		return err
	}
	es, lines, err := ledger.ReadFile(path)
	if err != nil {
		return err
	}
	r := ledger.Verify(es, lines, ledger.VerifyOptions{AnchorDir: adir, CheckAnchor: ops.AnchorVerifier(roots, *allowUnpinned)})
	if *asJSON {
		if err := writeJSON(a.env.Stdout, r); err != nil {
			return err
		}
	} else {
		printLedgerReport(a.env.Stdout, r)
	}
	if !r.OK() {
		return exitError(ExitFail)
	}
	return nil
}

func writeJSON(w io.Writer, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(w, string(b))
	return err
}

func printLedgerReport(w io.Writer, r *ledger.Report) {
	fmt.Fprintf(w, "Ledger verification: %s\n", status(r.OK()))
	origin := ", segment"
	if r.StartsAtGenesis {
		origin = ", from genesis"
	}
	fmt.Fprintf(w, "  entries:   %d (seq %d…%d)%s\n", r.Entries, r.FirstSeq, r.LastSeq, origin)
	fmt.Fprintf(w, "  head:      %s\n", r.HeadHash)
	printAnchors(w, r)
	if r.MaxAnchorLagSeconds >= 0 {
		fmt.Fprintf(w, "  anchor lag: largest %s (seq %d); %d entries anchored more than 24h after their own timestamp\n",
			ledger.FormatLag(r.MaxAnchorLagSeconds), r.MaxAnchorLagSeq, r.EntriesLaggingOver24h)
	}
	if r.DroppedCalls > 0 {
		fmt.Fprintf(w, "  gaps:      %d gap entries, %d calls not recorded (fail-open)\n", len(r.Gaps), r.DroppedCalls)
	}
	printLines(w, "  ERROR:", r.Errors)
	printLines(w, "  warning:", r.Warnings)
}

func printAnchors(w io.Writer, r *ledger.Report) {
	fmt.Fprintf(w, "  anchors:   %d", len(r.Anchors))
	if r.LastAnchoredSeq < 0 {
		fmt.Fprintln(w, " — nothing is anchored yet; run 'ambermoth anchor'")
	} else {
		fmt.Fprintf(w, ", anchored through seq %d, %d entries after the last anchor\n", r.LastAnchoredSeq, r.UnanchoredTail)
	}
	for _, an := range r.Anchors {
		if !an.OK {
			fmt.Fprintf(w, "    ✗ seq %-6d %s  %s\n", an.TargetSeq, an.TSA, an.Error)
			continue
		}
		pin := "pinned"
		if !an.Pinned {
			pin = "NOT PINNED"
		}
		fmt.Fprintf(w, "    ✓ seq %-6d %s  %s  %s (%s)\n", an.TargetSeq, ledger.FormatTime(an.GenTime), an.TSA, an.Signer, pin)
	}
}

func printLines(w io.Writer, prefix string, lines []string) {
	for _, l := range lines {
		fmt.Fprintln(w, prefix, l)
	}
}

func status(ok bool) string {
	if ok {
		return "OK"
	}
	return "FAILED"
}
