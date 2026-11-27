// Package ops implements the ledger-writing CLI operations: anchoring,
// amendments, key events, code links and reconciliation.
package ops

import (
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
	seq, head, err := s.Head()
	if err != nil {
		return nil, err
	}
	digest, err := ledger.HashBytes(head)
	if err != nil {
		return nil, err
	}
	var out []ledger.Entry
	var errs []string
	for i, u := range tsaURLs {
		der, info, err := tsa.Request(ctx, u, digest, client)
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", u, err))
			continue
		}
		name := fmt.Sprintf("%d-%d-%s.tsr", seq, i, hostOf(u))
		if err := os.WriteFile(filepath.Join(s.Dir, ledger.AnchorDir, name), der, 0o600); err != nil {
			return out, err
		}
		e := ledger.Entry{"type": ledger.TypeAnchor, "kind": "rfc3161", "target_seq": seq, "target_hash": head,
			"tsa": u, "token_file": name, "token_sha256": ledger.SHA256Bytes(der),
			"gen_time": ledger.FormatTime(info.GenTime), "tsa_signer": info.Signer}
		if err := s.Append(e); err != nil {
			return out, err
		}
		out = append(out, e)
	}
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
	if reason == "" || by == "" {
		return nil, errors.New("--reason and --by are required: amendments must say why and who")
	}
	if newPurpose == "" && annotation == "" {
		return nil, errors.New("give --purpose and/or --annotation")
	}
	head, _, err := s.Head()
	if err != nil {
		return nil, err
	}
	if from < 1 || to < from || to > head {
		return nil, fmt.Errorf("invalid target range %d..%d (head is %d)", from, to, head)
	}
	entries, err := s.Entries()
	if err != nil {
		return nil, err
	}
	calls := 0
	for q := from; q <= to && q < int64(len(entries)); q++ {
		if entries[q] != nil && entries[q].Type() == ledger.TypeCall {
			calls++
		}
	}
	if calls == 0 {
		return nil, fmt.Errorf("no call entries in range %d..%d", from, to)
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
	r := csv.NewReader(strings.NewReader(string(raw)))
	header, err := r.Read()
	if err != nil {
		return nil, nil, err
	}
	col := map[string]int{}
	for i, h := range header {
		col[strings.ToLower(strings.TrimSpace(h))] = i
	}
	if err := requireColumns(col, "date", "input_tokens", "output_tokens"); err != nil {
		return nil, nil, err
	}
	type tot struct{ in, out int64 }
	exp := map[string]*tot{}
	for {
		rec, err := r.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, nil, err
		}
		d := strings.TrimSpace(rec[col["date"]])
		if len(d) >= 10 {
			d = d[:10]
		}
		in, _ := strconv.ParseInt(strings.TrimSpace(rec[col["input_tokens"]]), 10, 64)
		out, _ := strconv.ParseInt(strings.TrimSpace(rec[col["output_tokens"]]), 10, 64)
		if exp[d] == nil {
			exp[d] = &tot{}
		}
		exp[d].in += in
		exp[d].out += out
	}
	if len(exp) == 0 {
		return nil, nil, errors.New("CSV has no rows")
	}
	var days []string
	for d := range exp {
		days = append(days, d)
	}
	sort.Strings(days)
	from, _ := time.Parse("2006-01-02", days[0])
	to, _ := time.Parse("2006-01-02", days[len(days)-1])
	entries, err := s.Entries()
	if err != nil {
		return nil, nil, err
	}
	sum := report.Summarize(entries, report.Filter{From: from, To: to.AddDate(0, 0, 1), Provider: provider})
	led := map[string]*tot{}
	for _, e := range entries {
		if e == nil || e.Type() != ledger.TypeCall || e.Str("provider") != provider {
			continue
		}
		d := e.Day()
		if exp[d] == nil {
			continue
		}
		if led[d] == nil {
			led[d] = &tot{}
		}
		led[d].in += e.Int("tokens_in")
		led[d].out += e.Int("tokens_out")
	}
	var lines []string
	var mismatched int64
	var ei, eo int64
	for _, d := range days {
		l := led[d]
		if l == nil {
			l = &tot{}
		}
		ei += exp[d].in
		eo += exp[d].out
		diff := func(a, b int64) bool {
			if b == 0 {
				return a != 0
			}
			x := float64(a-b) / float64(b)
			return x > tolerance || x < -tolerance
		}
		status := "ok"
		if diff(l.in, exp[d].in) || diff(l.out, exp[d].out) {
			status = "MISMATCH"
			mismatched++
		}
		lines = append(lines, fmt.Sprintf("%s  ledger in/out %d/%d  export in/out %d/%d  %s", d, l.in, l.out, exp[d].in, exp[d].out, status))
	}
	result := "match"
	if mismatched > 0 {
		result = "mismatch"
	}
	e := ledger.Entry{"type": ledger.TypeReconciliation, "provider": provider,
		"period_from": days[0], "period_to": days[len(days)-1], "source_sha256": ledger.SHA256Bytes(raw),
		"ledger_tokens_in": sum.Totals.All.TokensIn, "ledger_tokens_out": sum.Totals.All.TokensOut,
		"export_tokens_in": ei, "export_tokens_out": eo, "days_mismatched": mismatched, "result": result,
		"tolerance_pct": int64(tolerance * 100)}
	return e, lines, s.Append(e)
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
