package cli_test

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/devrdias/ambermoth/internal/cli"
	"github.com/devrdias/ambermoth/internal/ledger"
	"github.com/devrdias/ambermoth/internal/tsa/tsatest"
)

// recordedHarness is a harness whose ledger holds one recorded call.
func recordedHarness(t *testing.T) *harness {
	t.Helper()
	h := newHarness(t)
	h.recordCall(t)
	return h
}

func (h *harness) workFile(name string) string { return filepath.Join(filepath.Dir(h.dir), name) }

// failingWriter fails every write, like a closed pipe.
type failingWriter struct{}

var errBrokenPipe = errors.New("broken pipe")

func (failingWriter) Write([]byte) (int, error) { return 0, errBrokenPipe }

// runWithStdout is h.run with stdout replaced.
func (h *harness) runWithStdout(stdout io.Writer, args ...string) (int, string) {
	h.t.Helper()
	var errOut bytes.Buffer
	app := cli.New(cli.Env{
		Stdin: strings.NewReader(""), Stdout: stdout, Stderr: &errOut,
		Getenv: func(string) string { return "" }, Clock: fixedClock, Logger: discardLogger(), Version: "test",
	})
	code := app.Run(context.Background(), append(args, "--dir", h.dir))
	return code, errOut.String()
}

func TestPackCommandErrors(t *testing.T) {
	t.Parallel()
	h := recordedHarness(t)
	cases := []runCase{
		{name: "rejects an unknown flag", args: []string{"pack", "--nope"}, wantCode: cli.ExitUsage, wantErr: "flag provided but not defined"},
		{name: "rejects a malformed end day", args: []string{"pack", "--from", "2026-03-01", "--to", "soon"}, wantCode: cli.ExitFail, wantErr: `--to: want YYYY-MM-DD, got "soon"`},
		{name: "rejects an unreadable TSA root", args: []string{"pack", "--from", "2026-03-01", "--to", "2026-03-31", "--no-tsa", "--tsa-ca", "/nonexistent/root.pem"}, wantCode: cli.ExitFail, wantErr: "no such file"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) { t.Parallel(); h.check(t, c) })
	}
	t.Run("explains a missing ledger", func(t *testing.T) {
		t.Parallel()
		newHarness(t).check(t, runCase{args: []string{"pack", "--from", "2026-03-01", "--to", "2026-03-31"}, wantCode: cli.ExitFail, wantErr: "run 'ambermoth init'"})
	})
}

// This test changes the working directory, so it must not be parallel.
func TestPackDefaultsToANamedFileInTheWorkingDirectory(t *testing.T) {
	h := recordedHarness(t)
	work := t.TempDir()
	old, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(work); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(old) })

	h.check(t, runCase{args: []string{"pack", "--from", "2026-03-04", "--to", "2026-03-04", "--no-tsa"}, wantCode: cli.ExitOK,
		wantOut: "Evidence pack written: evidence-pack-2026-03-04-2026-03-04.zip"})
	if _, err := os.Stat(filepath.Join(work, "evidence-pack-2026-03-04-2026-03-04.zip")); err != nil {
		t.Errorf("default pack file not written: %v", err)
	}
}

func TestPackWithATimestamp(t *testing.T) {
	t.Parallel()
	auth := tsatest.New(t)
	h := recordedHarness(t)
	useTSAs(t, h.dir, auth.Server(t).URL)
	ca := h.workFile("ca.pem")
	writeFile(t, ca, string(auth.CAPEM))
	out := h.workFile("stamped.zip")

	h.check(t, runCase{args: []string{"pack", "--from", "2026-03-01", "--to", "2026-03-31", "--out", out, "--provider", "openai", "--account", "acme", "--tsa-ca", ca},
		wantCode: cli.ExitOK, wantOut: "1 calls, 1 purposes"})
	cases := []runCase{
		{name: "verifies with the TSA root pinned", args: []string{"verify-pack", out, "--tsa-ca", ca}, wantCode: cli.ExitOK, wantOut: "Evidence pack verification: OK"},
		{name: "prints the account on the report", args: []string{"verify-pack", out, "--tsa-ca", ca}, wantCode: cli.ExitOK, wantOut: "account acme"},
		{name: "fails when the TSA root is not trusted", args: []string{"verify-pack", out}, wantCode: cli.ExitFail, wantOut: "Evidence pack verification: FAILED"},
		{name: "accepts an unpinned TSA when asked", args: []string{"verify-pack", out, "--allow-unpinned-tsa"}, wantCode: cli.ExitOK, wantOut: "Evidence pack verification: OK"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) { t.Parallel(); h.check(t, c) })
	}
}

// tamperedCopy copies the pack at src to dst with one byte appended to summary.html.
func tamperedCopy(t *testing.T, src, dst string) {
	t.Helper()
	zr, err := zip.OpenReader(src)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = zr.Close() })
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, f := range zr.File {
		rc, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		b, err := io.ReadAll(rc)
		_ = rc.Close()
		if err != nil {
			t.Fatal(err)
		}
		if f.Name == "summary.html" {
			b = append(b, ' ')
		}
		w, err := zw.Create(f.Name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(b); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	writeFile(t, dst, buf.String())
}

func TestVerifyPackCommand(t *testing.T) {
	t.Parallel()
	h := recordedHarness(t)
	good, bad := h.workFile("good.zip"), h.workFile("bad.zip")
	h.check(t, runCase{args: []string{"pack", "--from", "2026-03-01", "--to", "2026-03-31", "--no-tsa", "--out", good}, wantCode: cli.ExitOK})
	tamperedCopy(t, good, bad)

	cases := []runCase{
		{name: "rejects an unknown flag", args: []string{"verify-pack", good, "--nope"}, wantCode: cli.ExitUsage, wantErr: "flag provided but not defined"},
		{name: "rejects an unreadable TSA root", args: []string{"verify-pack", good, "--tsa-ca", "/nonexistent/root.pem"}, wantCode: cli.ExitFail, wantErr: "no such file"},
		{name: "reports a missing pack", args: []string{"verify-pack", h.workFile("none.zip")}, wantCode: cli.ExitFail, wantErr: "error:"},
		{name: "rejects a pack with an edited summary", args: []string{"verify-pack", bad}, wantCode: cli.ExitFail, wantOut: "Evidence pack verification: FAILED"},
		{name: "reports a rejected pack as JSON", args: []string{"verify-pack", bad, "--json"}, wantCode: cli.ExitFail, wantOut: `"ok": false`},
		{name: "cannot write the report into a missing directory", args: []string{"verify-pack", good, "--report", h.workFile("nodir/report.html")}, wantCode: cli.ExitFail, wantErr: "no such file"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) { t.Parallel(); h.check(t, c) })
	}

	t.Run("writes the verifier's report", func(t *testing.T) {
		t.Parallel()
		report := h.workFile("report.html")
		h.check(t, runCase{args: []string{"verify-pack", good, "--report", report}, wantCode: cli.ExitOK, wantOut: "report: " + report})
		b, err := os.ReadFile(report)
		if err != nil || !strings.Contains(string(b), "<html") {
			t.Errorf("report file = %q, err %v; want an HTML document", b, err)
		}
	})
	t.Run("writes no report unless asked", func(t *testing.T) {
		t.Parallel()
		code, out, _ := h.run("verify-pack", good)
		if code != cli.ExitOK || strings.Contains(out, "report:") {
			t.Errorf("exit %d, output:\n%s", code, out)
		}
	})
	t.Run("fails when the JSON cannot be written", func(t *testing.T) {
		t.Parallel()
		code, errOut := h.runWithStdout(failingWriter{}, "verify-pack", good, "--json")
		if code != cli.ExitFail || !strings.Contains(errOut, "error: "+errBrokenPipe.Error()) {
			t.Errorf("exit %d, stderr %q; want exit 1 and the write error", code, errOut)
		}
	})
}

type reconcileCase struct {
	name       string
	csv        string
	flags      []string
	wantLine   string
	wantResult string
}

func TestReconcileCommand(t *testing.T) {
	t.Parallel()
	const header = "date,model,input_tokens,output_tokens\n"
	cases := []reconcileCase{
		{name: "matches an export equal to the ledger", csv: header + "2026-03-04,gpt-test,100,20\n",
			wantLine: "2026-03-04  ledger in/out 100/20  export in/out 100/20  ok", wantResult: "match"},
		{name: "sums the rows of a day", csv: header + "2026-03-04,gpt-a,60,10\n2026-03-04T08:00:00Z,gpt-b,40,10\n",
			wantLine: "export in/out 100/20  ok", wantResult: "match"},
		{name: "flags a day that differs", csv: header + "2026-03-04,gpt-test,200,20\n",
			wantLine: "ledger in/out 100/20  export in/out 200/20  MISMATCH", wantResult: "mismatch"},
		{name: "flags a day the ledger has no calls for", csv: header + "2026-03-05,gpt-test,10,10\n",
			wantLine: "2026-03-05  ledger in/out 0/0  export in/out 10/10  MISMATCH", wantResult: "mismatch"},
		{name: "flags a ledger day the export skips between its first and last day", csv: header + "2026-03-03,gpt-test,5,5\n2026-03-05,gpt-test,5,5\n",
			wantLine: "2026-03-04  ledger in/out 100/20  export in/out 0/0  MISMATCH", wantResult: "mismatch"},
		{name: "accepts a small difference by default", csv: header + "2026-03-04,gpt-test,101,20\n",
			wantLine: "export in/out 101/20  ok", wantResult: "match"},
		{name: "rejects the same difference with a tighter tolerance", csv: header + "2026-03-04,gpt-test,101,20\n",
			flags: []string{"--tolerance", "0.001"}, wantLine: "export in/out 101/20  MISMATCH", wantResult: "mismatch"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			h := recordedHarness(t)
			csv := h.workFile("usage.csv")
			writeFile(t, csv, c.csv)
			code, out, errOut := h.run(append([]string{"reconcile", "--provider", "openai", "--csv", csv}, c.flags...)...)
			if code != cli.ExitOK {
				t.Fatalf("exit %d\nstderr: %s", code, errOut)
			}
			recs := entriesOfType(t, h.dir, ledger.TypeReconciliation)
			if len(recs) != 1 {
				t.Fatalf("%d reconciliation entries, want 1", len(recs))
			}
			if got := recs[0].Str("result"); got != c.wantResult {
				t.Errorf("recorded result = %q, want %q", got, c.wantResult)
			}
			if !strings.Contains(out, c.wantLine) {
				t.Errorf("output lacks %q:\n%s", c.wantLine, out)
			}
			want := fmt.Sprintf("reconciliation %s, recorded at seq %d", strings.ToUpper(c.wantResult), recs[0].Seq())
			if !strings.Contains(out, want) {
				t.Errorf("output lacks %q:\n%s", want, out)
			}
		})
	}
}

func TestReconcileCommandErrors(t *testing.T) {
	t.Parallel()
	h := recordedHarness(t)
	noColumns, noRows, badDate := h.workFile("nocols.csv"), h.workFile("norows.csv"), h.workFile("baddate.csv")
	writeFile(t, badDate, "date,model,input_tokens,output_tokens\n2026-03-04,m,1,1\nyesterday,m,1,1\n")
	writeFile(t, noColumns, "when,tokens\n2026-03-04,5\n")
	writeFile(t, noRows, "date,model,input_tokens,output_tokens\n")
	cases := []runCase{
		{name: "rejects an unknown flag", args: []string{"reconcile", "--nope"}, wantCode: cli.ExitUsage, wantErr: "flag provided but not defined"},
		{name: "reports a missing CSV", args: []string{"reconcile", "--provider", "openai", "--csv", h.workFile("none.csv")}, wantCode: cli.ExitFail, wantErr: "no such file"},
		{name: "reports missing columns", args: []string{"reconcile", "--provider", "openai", "--csv", noColumns}, wantCode: cli.ExitFail, wantErr: "error:"},
		{name: "reports a CSV without rows", args: []string{"reconcile", "--provider", "openai", "--csv", noRows}, wantCode: cli.ExitFail, wantErr: "CSV has no rows"},
		{name: "names the row and value of a bad date", args: []string{"reconcile", "--provider", "openai", "--csv", badDate}, wantCode: cli.ExitFail, wantErr: `row 3: invalid date "yesterday"`},
		{name: "refuses a negative tolerance", args: []string{"reconcile", "--provider", "openai", "--csv", noRows, "--tolerance", "-0.1"}, wantCode: cli.ExitFail, wantErr: "invalid tolerance -0.1"},
		{name: "refuses a tolerance over 100%", args: []string{"reconcile", "--provider", "openai", "--csv", noRows, "--tolerance", "2"}, wantCode: cli.ExitFail, wantErr: "invalid tolerance 2"},
	}
	t.Run("each fails without recording", func(t *testing.T) {
		for _, c := range cases {
			t.Run(c.name, func(t *testing.T) { t.Parallel(); h.check(t, c) })
		}
	})
	t.Run("explains a missing ledger", func(t *testing.T) {
		t.Parallel()
		newHarness(t).check(t, runCase{args: []string{"reconcile", "--provider", "openai", "--csv", noRows}, wantCode: cli.ExitFail, wantErr: "run 'ambermoth init'"})
	})
	if n := len(entriesOfType(t, h.dir, ledger.TypeReconciliation)); n != 0 {
		t.Errorf("%d reconciliation entries after failed runs, want 0", n)
	}
}
