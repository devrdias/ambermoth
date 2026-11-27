package cli_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/devrdias/ambermoth/internal/cli"
	"github.com/devrdias/ambermoth/internal/ledger"
)

// epipeWriter fails like a write to a pipe whose reader has gone.
type epipeWriter struct{}

func (epipeWriter) Write([]byte) (int, error) {
	return 0, &os.PathError{Op: "write", Path: "|1", Err: syscall.EPIPE}
}

// countingWriter counts the writes that reach it and fails every one.
type countingWriter struct{ writes int }

func (c *countingWriter) Write([]byte) (int, error) { c.writes++; return 0, errBrokenPipe }

// outputCase is one command whose result is printed to stdout.
type outputCase struct {
	name    string
	args    func(h *harness) []string
	fresh   bool // no ledger yet; the command creates it
	changes bool // the command has done its work before it prints
	check   func(t *testing.T, h *harness)
}

func fixed(args ...string) func(*harness) []string { return func(*harness) []string { return args } }

func outputCases(t *testing.T) []outputCase {
	t.Helper()
	pack := func(h *harness) string { return h.workFile("p.zip") }
	return []outputCase{
		{name: "version", args: fixed("version"), fresh: true},
		{name: "help", args: fixed("help"), fresh: true},
		{name: "agent-prompt", args: fixed("agent-prompt"), fresh: true},
		{name: "agent-prompt --machine", args: fixed("agent-prompt", "--machine"), fresh: true},
		{name: "report", args: fixed("report")},
		{name: "alerts", args: fixed("alerts")},
		{name: "alerts --json", args: fixed("alerts", "--json")},
		{name: "verify", args: fixed("verify")},
		{name: "verify --json", args: fixed("verify", "--json")},
		{name: "verify-pack", args: func(h *harness) []string { return []string{"verify-pack", pack(h)} }},
		{name: "verify-pack --json", args: func(h *harness) []string { return []string{"verify-pack", pack(h), "--json"} }},
		{name: "init", args: fixed("init"), fresh: true, changes: true,
			check: func(t *testing.T, h *harness) { mustExist(t, filepath.Join(h.dir, ledger.LedgerFile)) }},
		{name: "key enroll", args: fixed("key", "enroll", "--provider", "openai", "--fp", "sha256:abc"), changes: true,
			check: func(t *testing.T, h *harness) { wantEntries(t, h, ledger.TypeKeyEvent, 2) }}, // the proxy recorded the first
		{name: "amend", changes: true,
			args: func(h *harness) []string {
				seq := strconv.FormatInt(entriesOfType(t, h.dir, ledger.TypeCall)[0].Seq(), 10)
				return []string{"amend", "--seq", seq, "--annotation", "n", "--reason", "r", "--by", "me"}
			},
			check: func(t *testing.T, h *harness) { wantEntries(t, h, ledger.TypeAmendment, 1) }},
		{name: "reconcile", changes: true,
			args: func(h *harness) []string {
				writeFile(t, h.workFile("u.csv"), "date,model,input_tokens,output_tokens\n2026-03-04,gpt-test,100,20\n")
				return []string{"reconcile", "--provider", "openai", "--csv", h.workFile("u.csv")}
			},
			check: func(t *testing.T, h *harness) { wantEntries(t, h, ledger.TypeReconciliation, 1) }},
		{name: "pack", changes: true,
			args: func(h *harness) []string {
				return []string{"pack", "--from", "2026-03-04", "--to", "2026-03-04", "--no-tsa", "--out", h.workFile("new.zip")}
			},
			check: func(t *testing.T, h *harness) { mustExist(t, h.workFile("new.zip")) }},
	}
}

func mustExist(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); err != nil {
		t.Errorf("the work was not done: %v", err)
	}
}

func wantEntries(t *testing.T, h *harness, typ string, n int) {
	t.Helper()
	if got := len(entriesOfType(t, h.dir, typ)); got != n {
		t.Errorf("%d %s entries, want %d", got, typ, n)
	}
}

// outputHarness is a harness with a recorded call and a verifiable pack, or a
// bare one for commands that create the ledger.
func outputHarness(t *testing.T, c outputCase) *harness {
	t.Helper()
	if c.fresh {
		return newHarness(t)
	}
	h := recordedHarness(t)
	h.check(t, runCase{args: []string{"pack", "--from", "2026-03-04", "--to", "2026-03-04", "--no-tsa", "--out", h.workFile("p.zip")}, wantCode: cli.ExitOK})
	return h
}

func TestCommandFailsWhenItsOutputCannotBeWritten(t *testing.T) {
	for _, c := range outputCases(t) {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			h := outputHarness(t, c)
			code, errOut := h.runWithStdout(failingWriter{}, c.args(h)...)
			if code != cli.ExitFail {
				t.Errorf("exit %d, want %d; stderr: %q", code, cli.ExitFail, errOut)
			}
			if !strings.Contains(errOut, "error: writing output: "+errBrokenPipe.Error()) {
				t.Errorf("stderr lacks the write error: %q", errOut)
			}
			if strings.Count(errOut, "error:") != 1 {
				t.Errorf("the error should be reported once: %q", errOut)
			}
			saysDone := strings.Contains(errOut, "the work itself was done")
			if saysDone != c.changes {
				t.Errorf("stderr says the work was done: %v, want %v: %q", saysDone, c.changes, errOut)
			}
			if c.check != nil {
				c.check(t, h)
			}
		})
	}
}

func TestBrokenPipeEndsQuietlyWithFailure(t *testing.T) {
	for _, c := range outputCases(t) {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			h := outputHarness(t, c)
			code, errOut := h.runWithStdout(epipeWriter{}, c.args(h)...)
			if code != cli.ExitFail || errOut != "" {
				t.Errorf("exit %d, stderr %q; want exit 1 and nothing on stderr", code, errOut)
			}
			if c.check != nil {
				c.check(t, h)
			}
		})
	}
}

func TestVerifyNeverExitsOKWhenItsReportWasNotDelivered(t *testing.T) {
	t.Parallel()
	h := recordedHarness(t)
	if code, _ := h.runWithStdout(failingWriter{}, "verify"); code != cli.ExitFail {
		t.Errorf("verified ledger, undeliverable report: exit %d, want %d", code, cli.ExitFail)
	}
	path := filepath.Join(h.dir, ledger.LedgerFile)
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, path, strings.Replace(string(b), `"openai"`, `"0penai"`, 1))
	code, errOut := h.runWithStdout(failingWriter{}, "verify")
	if code != cli.ExitFail || !strings.Contains(errOut, "writing output") {
		t.Errorf("failed ledger, undeliverable report: exit %d, stderr %q", code, errOut)
	}
}

func TestStdoutIsNotWrittenAfterTheFirstFailure(t *testing.T) {
	t.Parallel()
	h := recordedHarness(t)
	w := &countingWriter{}
	h.runWithStdout(w, "verify")
	if w.writes != 1 {
		t.Errorf("%d writes reached stdout, want 1", w.writes)
	}
}

func TestAnchorWithAnUnwritableConfirmationStillAnchors(t *testing.T) {
	t.Parallel()
	f := newAnchorFixture(t)
	code, errOut := f.runWithStdout(failingWriter{}, "anchor", "--tsa", f.tsaURL)
	if code != cli.ExitFail || !strings.Contains(errOut, "the work itself was done") {
		t.Errorf("exit %d, stderr %q", code, errOut)
	}
	if n := len(entriesOfType(t, f.dir, ledger.TypeAnchor)); n != 1 {
		t.Errorf("%d anchor entries, want 1", n)
	}
}

func TestReportToAFullDisk(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("needs /dev/full")
	}
	full, err := os.OpenFile("/dev/full", os.O_WRONLY, 0)
	if err != nil {
		t.Skipf("no /dev/full: %v", err)
	}
	t.Cleanup(func() { _ = full.Close() })
	h := recordedHarness(t)
	var errOut bytes.Buffer
	app := cli.New(cli.Env{
		Stdin: strings.NewReader(""), Stdout: full, Stderr: &errOut,
		Getenv: func(string) string { return "" }, Clock: fixedClock, Logger: discardLogger(), Version: "test",
	})
	code := app.Run(context.Background(), []string{"report", "--dir", h.dir})
	if code != cli.ExitFail || !strings.Contains(errOut.String(), "error: writing output: write /dev/full: no space left on device") {
		t.Errorf("exit %d, stderr %q", code, errOut.String())
	}
}
