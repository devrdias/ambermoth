// Package ops implements the ledger-writing CLI operations: anchoring,
// amendments, key events, code links and reconciliation.
package ops

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/devrdias/ambermoth/internal/ledger"
	"github.com/devrdias/ambermoth/internal/report"
	"github.com/devrdias/ambermoth/internal/trust"
	"github.com/devrdias/ambermoth/internal/tsa"
)

// Anchor timestamps the current head with each TSA and appends anchor entries.
func Anchor(ctx context.Context, s *ledger.Store, tsaURLs []string, client *http.Client) ([]ledger.Entry, error) {
	if len(tsaURLs) == 0 {
		return nil, errors.New("no TSA configured")
	}
	target, err := headTarget(s)
	if err != nil {
		return nil, err
	}
	var out []ledger.Entry
	var errs []string
	for i, u := range tsaURLs {
		der, info, err := tsa.Request(ctx, u, target.digest, client)
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", u, err))
			continue
		}
		e, err := storeAnchor(s, target, i, u, der, info)
		if err != nil {
			return out, err
		}
		out = append(out, e)
	}
	return anchorOutcome(out, errs)
}

// anchorTarget is the head entry an anchor run timestamps.
type anchorTarget struct {
	seq    int64
	hash   string
	digest []byte
}

func headTarget(s *ledger.Store) (anchorTarget, error) {
	seq, head, err := s.Head()
	if err != nil {
		return anchorTarget{}, err
	}
	digest, err := ledger.HashBytes(head)
	if err != nil {
		return anchorTarget{}, err
	}
	return anchorTarget{seq: seq, hash: head, digest: digest}, nil
}

// storeAnchor writes the token for TSA number idx and appends its anchor entry.
func storeAnchor(s *ledger.Store, t anchorTarget, idx int, tsaURL string, der []byte, info *tsa.TokenInfo) (ledger.Entry, error) {
	name := fmt.Sprintf("%d-%d-%s.tsr", t.seq, idx, hostOf(tsaURL))
	if err := os.WriteFile(filepath.Join(s.Dir, ledger.AnchorDir, name), der, 0o600); err != nil {
		return nil, err
	}
	e := ledger.Entry{"type": ledger.TypeAnchor, "kind": "rfc3161", "target_seq": t.seq, "target_hash": t.hash,
		"tsa": tsaURL, "token_file": name, "token_sha256": ledger.SHA256Bytes(der),
		"gen_time": ledger.FormatTime(info.GenTime), "tsa_signer": info.Signer}
	return e, s.Append(e)
}

// anchorOutcome turns the anchors made and the TSA failures into Anchor's result.
func anchorOutcome(out []ledger.Entry, errs []string) ([]ledger.Entry, error) {
	if len(out) == 0 {
		return nil, fmt.Errorf("all TSAs failed: %s", strings.Join(errs, "; "))
	}
	if len(errs) > 0 {
		return out, fmt.Errorf("some TSAs failed: %s", strings.Join(errs, "; "))
	}
	return out, nil
}

func hostOf(u string) string {
	p, err := url.Parse(u)
	if err != nil || p.Host == "" {
		return "tsa"
	}
	h := strings.NewReplacer(":", "_", "/", "_").Replace(p.Host)
	return h
}

// TrustPool returns the pinned TSA roots plus any extra PEM files (--tsa-ca).
func TrustPool(extraPEM []string) (*x509.CertPool, error) { return trust.Pool(extraPEM) }

// AnchorVerifier adapts tsa.Verify to ledger.Verify. A token whose signer does
// not chain to roots fails, unless allowUnpinned is set: then it passes with
// Pinned=false, which every report shows.
func AnchorVerifier(roots *x509.CertPool, allowUnpinned bool) ledger.AnchorVerifier {
	return func(tok []byte, target string) (ledger.AnchorCheck, error) {
		d, err := ledger.HashBytes(target)
		if err != nil {
			return ledger.AnchorCheck{}, err
		}
		if roots == nil && !allowUnpinned {
			return ledger.AnchorCheck{}, errors.New("no trusted TSA roots configured")
		}
		info, err := tsa.Verify(tok, d, roots)
		if errors.Is(err, tsa.ErrUntrustedChain) && allowUnpinned {
			if info, err = tsa.Verify(tok, d, nil); err == nil {
				return ledger.AnchorCheck{GenTime: info.GenTime, Signer: info.Signer, Pinned: false}, nil
			}
		}
		if errors.Is(err, tsa.ErrUntrustedChain) {
			return ledger.AnchorCheck{}, fmt.Errorf("%w (pin its root with --tsa-ca, or pass --allow-unpinned-tsa to accept it unpinned)", err)
		}
		if err != nil {
			return ledger.AnchorCheck{}, err
		}
		return ledger.AnchorCheck{GenTime: info.GenTime, Signer: info.Signer, Pinned: roots != nil}, nil
	}
}

// Amend appends an amendment entry (SPEC §4.3).
func Amend(s *ledger.Store, from, to int64, newPurpose, annotation, reason, by string) (ledger.Entry, error) {
	if err := checkAmendment(newPurpose, annotation, reason, by); err != nil {
		return nil, err
	}
	if err := checkAmendTarget(s, from, to); err != nil {
		return nil, err
	}
	e := ledger.Entry{"type": ledger.TypeAmendment, "target_from": from, "target_to": to,
		"reason": reason, "by": by}
	if newPurpose != "" {
		e["new_purpose"] = newPurpose
	}
	if annotation != "" {
		e["annotation"] = annotation
	}
	return e, s.Append(e)
}

func checkAmendment(newPurpose, annotation, reason, by string) error {
	if reason == "" || by == "" {
		return errors.New("--reason and --by are required: amendments must say why and who")
	}
	if newPurpose == "" && annotation == "" {
		return errors.New("give --purpose and/or --annotation")
	}
	return nil
}

// checkAmendTarget refuses a range outside the ledger or without any call in it.
func checkAmendTarget(s *ledger.Store, from, to int64) error {
	head, _, err := s.Head()
	if err != nil {
		return err
	}
	if from < 1 || to < from || to > head {
		return fmt.Errorf("invalid target range %d..%d (head is %d)", from, to, head)
	}
	entries, err := s.Entries()
	if err != nil {
		return err
	}
	if countCalls(entries, from, to) == 0 {
		return fmt.Errorf("no call entries in range %d..%d", from, to)
	}
	return nil
}

// countCalls counts the call entries with seq in from..to. An entry's seq is
// its index in entries.
func countCalls(entries []ledger.Entry, from, to int64) int {
	calls := 0
	for q := from; q <= to && q < int64(len(entries)); q++ {
		if entries[q] != nil && entries[q].Type() == ledger.TypeCall {
			calls++
		}
	}
	return calls
}

// KeyEvent appends a key_event entry.
func KeyEvent(s *ledger.Store, action, apiKey, fp, provider, label, reason, replacedBy string) (ledger.Entry, error) {
	switch action {
	case "enroll", "rotate", "revoke":
	default:
		return nil, fmt.Errorf("unknown key action %q", action)
	}
	if apiKey != "" {
		fp = ledger.KeyFingerprint(apiKey)
	}
	if !strings.HasPrefix(fp, "sha256:") {
		return nil, errors.New("need a key (stdin / env) or a sha256: fingerprint")
	}
	e := ledger.Entry{"type": ledger.TypeKeyEvent, "action": action, "key_fp": fp,
		"provider": provider, "label": label, "reason": reason}
	if replacedBy != "" {
		e["replaced_by"] = replacedBy
	}
	return e, s.Append(e)
}

func git(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	return strings.TrimSpace(string(out)), err
}

// Link records a code_link entry from a git repository (SPEC §4.5).
func Link(s *ledger.Store, purpose, repoDir, path string) (ledger.Entry, error) {
	commit, err := git(repoDir, "rev-parse", "HEAD")
	if err != nil {
		return nil, fmt.Errorf("%s is not a git repository with commits", repoDir)
	}
	args := []string{"log", "--reverse", "--format=%cI", "HEAD"}
	countArgs := []string{"rev-list", "--count", "HEAD"}
	if path != "" {
		args = append(args, "--", path)
		countArgs = append(countArgs, "--", path)
	}
	dates, _ := git(repoDir, args...)
	first, _, _ := strings.Cut(dates, "\n")
	count, _ := git(repoDir, countArgs...)
	n, _ := strconv.ParseInt(count, 10, 64)
	remote, err := git(repoDir, "config", "--get", "remote.origin.url")
	if err != nil || remote == "" {
		abs, _ := filepath.Abs(repoDir)
		remote = "file://" + abs
	}
	dirty, _ := git(repoDir, "status", "--porcelain")
	e := ledger.Entry{"type": ledger.TypeCodeLink, "purpose": purpose, "repo": redactURL(remote),
		"commit": commit, "first_commit_ts": first, "commit_count": n, "path": path, "dirty": dirty != ""}
	return e, s.Append(e)
}

func redactURL(u string) string {
	p, err := url.Parse(u)
	if err == nil && p.User != nil {
		p.User = nil
		return p.String()
	}
	return u
}

// Reconcile compares per-day token totals with a provider usage export CSV
// (header: date,model,input_tokens,output_tokens) and records the result.
func Reconcile(s *ledger.Store, provider, csvPath string, tolerance float64) (ledger.Entry, []string, error) {
	raw, err := os.ReadFile(csvPath)
	if err != nil {
		return nil, nil, err
	}
	exp, err := readExport(raw)
	if err != nil {
		return nil, nil, err
	}
	days := sortedDays(exp)
	entries, err := s.Entries()
	if err != nil {
		return nil, nil, err
	}
	cmp := compareDays(days, exp, dayTotals(entries, provider, exp), tolerance)
	e := reconciliationEntry(provider, raw, days, periodTotals(entries, provider, days), cmp, tolerance)
	return e, cmp.lines, s.Append(e)
}

// tally is a pair of token counts.
type tally struct{ in, out int64 }

// readExport sums the rows of a usage CSV by day.
func readExport(raw []byte) (map[string]*tally, error) {
	r := csv.NewReader(bytes.NewReader(raw))
	header, err := r.Read()
	if err != nil {
		return nil, err
	}
	col := map[string]int{}
	for i, h := range header {
		col[strings.ToLower(strings.TrimSpace(h))] = i
	}
	if err := requireColumns(col, "date", "input_tokens", "output_tokens"); err != nil {
		return nil, err
	}
	exp := map[string]*tally{}
	for {
		rec, err := r.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		addRow(exp, col, rec)
	}
	if len(exp) == 0 {
		return nil, errors.New("CSV has no rows")
	}
	return exp, nil
}

// addRow adds one CSV row to its day. Counts that do not parse are zero, and
// a date is kept as written apart from being cut to its first ten characters.
func addRow(exp map[string]*tally, col map[string]int, rec []string) {
	d := strings.TrimSpace(rec[col["date"]])
	if len(d) >= 10 {
		d = d[:10]
	}
	if exp[d] == nil {
		exp[d] = &tally{}
	}
	exp[d].in += parseCount(rec[col["input_tokens"]])
	exp[d].out += parseCount(rec[col["output_tokens"]])
}

func parseCount(s string) int64 {
	n, _ := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	return n
}

func sortedDays(exp map[string]*tally) []string {
	var days []string
	for d := range exp {
		days = append(days, d)
	}
	sort.Strings(days)
	return days
}

// dayTotals sums the provider's calls on the days the export has rows for.
func dayTotals(entries []ledger.Entry, provider string, exp map[string]*tally) map[string]*tally {
	led := map[string]*tally{}
	for _, e := range entries {
		if e == nil || e.Type() != ledger.TypeCall || e.Str("provider") != provider {
			continue
		}
		d := e.Day()
		if exp[d] == nil {
			continue
		}
		if led[d] == nil {
			led[d] = &tally{}
		}
		led[d].in += e.Int("tokens_in")
		led[d].out += e.Int("tokens_out")
	}
	return led
}

// periodTotals sums the provider's calls from the first export day to the
// end of the last. A day that does not parse is a zero time.
func periodTotals(entries []ledger.Entry, provider string, days []string) tally {
	from, _ := time.Parse("2006-01-02", days[0])
	to, _ := time.Parse("2006-01-02", days[len(days)-1])
	sum := report.Summarize(entries, report.Filter{From: from, To: to.AddDate(0, 0, 1), Provider: provider})
	return tally{sum.Totals.All.TokensIn, sum.Totals.All.TokensOut}
}

// comparison is the per-day result of checking the ledger against the export.
type comparison struct {
	lines      []string
	mismatched int64
	export     tally
}

func compareDays(days []string, exp, led map[string]*tally, tolerance float64) comparison {
	var c comparison
	for _, d := range days {
		l := led[d]
		if l == nil {
			l = &tally{}
		}
		c.export.in += exp[d].in
		c.export.out += exp[d].out
		status := "ok"
		if outside(l.in, exp[d].in, tolerance) || outside(l.out, exp[d].out, tolerance) {
			status = "MISMATCH"
			c.mismatched++
		}
		c.lines = append(c.lines, fmt.Sprintf("%s  ledger in/out %d/%d  export in/out %d/%d  %s", d, l.in, l.out, exp[d].in, exp[d].out, status))
	}
	return c
}

// outside reports whether ledger count a differs from export count b by more than tolerance.
func outside(a, b int64, tolerance float64) bool {
	if b == 0 {
		return a != 0
	}
	x := float64(a-b) / float64(b)
	return x > tolerance || x < -tolerance
}

func reconciliationEntry(provider string, raw []byte, days []string, led tally, c comparison, tolerance float64) ledger.Entry {
	result := "match"
	if c.mismatched > 0 {
		result = "mismatch"
	}
	return ledger.Entry{"type": ledger.TypeReconciliation, "provider": provider,
		"period_from": days[0], "period_to": days[len(days)-1], "source_sha256": ledger.SHA256Bytes(raw),
		"ledger_tokens_in": led.in, "ledger_tokens_out": led.out,
		"export_tokens_in": c.export.in, "export_tokens_out": c.export.out, "days_mismatched": c.mismatched, "result": result,
		"tolerance_pct": int64(tolerance * 100)}
}

// requireColumns returns an error naming the first of names missing from col.
func requireColumns(col map[string]int, names ...string) error {
	for _, n := range names {
		if _, ok := col[n]; !ok {
			return fmt.Errorf("CSV missing column %q (need date,model,input_tokens,output_tokens)", n)
		}
	}
	return nil
}
