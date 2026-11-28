package cli

import (
	"context"
	"fmt"
	"io"
	"path/filepath"

	"github.com/devrdias/ambermoth/internal/ops"
)

func (a *App) cmdRepair(_ context.Context, args []string) error {
	fs, dir := a.newFlags("repair")
	dryRun := fs.Bool("dry-run", false, "show what would be done and change nothing")
	asJSON := fs.Bool("json", false, "machine-readable output")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	res, err := ops.Repair(*dir, a.env.Clock, *dryRun)
	if err != nil {
		return err
	}
	if *asJSON {
		return writeJSON(a.out, res)
	}
	printRepair(a.out, *dir, res)
	return nil
}

func printRepair(w io.Writer, dir string, r *ops.RepairResult) {
	prefix, verb := "repaired", "removed"
	if r.DryRun {
		prefix, verb = "dry run, nothing changed", "would remove"
	}
	switch r.Action {
	case ops.RepairNone:
		_, _ = fmt.Fprintf(w, "nothing to repair: the ledger ends in a complete entry (seq %d)\n", r.LastSeq)
	case ops.RepairNewline:
		_, _ = fmt.Fprintf(w, "%s: the last entry (seq %d) is complete and only lacks its newline; no entry is removed and no gap entry is needed\n", prefix, r.LastSeq)
	case ops.RepairGap:
		_, _ = fmt.Fprintf(w, "%s: a torn line of %d bytes (%s) was removed earlier and never recorded\n", prefix, r.RemovedBytes, r.RemovedSHA256)
		_, _ = fmt.Fprintf(w, "  kept in %s\n  gap entries to record: %d\n", filepath.Join(dir, r.Sidecar), r.Gaps)
	default:
		_, _ = fmt.Fprintf(w, "%s: %s a torn last line of %d bytes (%s) after seq %d\n", prefix, verb, r.RemovedBytes, r.RemovedSHA256, r.LastSeq)
		_, _ = fmt.Fprintf(w, "  kept in %s\n", filepath.Join(dir, r.Sidecar))
		_, _ = fmt.Fprintf(w, "  gap entries to record: %d\n", r.Gaps)
	}
	if r.AnchorProblems > 0 {
		_, _ = fmt.Fprintf(w, "  note: %d anchor(s) fail verification, which repair does not touch; run 'ambermoth verify'\n", r.AnchorProblems)
	}
}
