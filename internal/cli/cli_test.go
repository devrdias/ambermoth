package cli_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/devrdias/ambermoth/internal/cli"
	"github.com/devrdias/ambermoth/internal/ledger"
)

type runCase struct {
	name     string
	args     []string
	wantCode int
	wantOut  string // substring of stdout
	wantErr  string // substring of stderr
}

type harness struct {
	t      *testing.T
	dir    string
	env    map[string]string
	secret string
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	return &harness{t: t, dir: filepath.Join(t.TempDir(), "ledger"), env: map[string]string{}}
}

func fixedClock() time.Time { return time.Date(2026, 3, 4, 12, 0, 0, 0, time.UTC) }

// run executes the CLI with --dir pointing at the harness ledger.
func (h *harness) run(args ...string) (code int, stdout, stderr string) {
	h.t.Helper()
	var out, errOut bytes.Buffer
	app := cli.New(cli.Env{
		Stdin:      strings.NewReader(""),
		Stdout:     &out,
		Stderr:     &errOut,
		Getenv:     func(k string) string { return h.env[k] },
		Clock:      fixedClock,
		Logger:     discardLogger(),
		Version:    "test",
		ReadSecret: func() (string, error) { return h.secret, nil },
	})
	code = app.Run(context.Background(), append(args, "--dir", h.dir))
	return code, out.String(), errOut.String()
}

func (h *harness) check(t *testing.T, c runCase) {
	t.Helper()
	code, out, errOut := h.run(c.args...)
	if code != c.wantCode {
		t.Fatalf("exit code = %d, want %d\nstdout:\n%s\nstderr:\n%s", code, c.wantCode, out, errOut)
	}
	if !strings.Contains(out, c.wantOut) {
		t.Errorf("stdout does not contain %q:\n%s", c.wantOut, out)
	}
	if !strings.Contains(errOut, c.wantErr) {
		t.Errorf("stderr does not contain %q:\n%s", c.wantErr, errOut)
	}
}

func TestRunWithoutLedger(t *testing.T) {
	t.Parallel()
	cases := []runCase{
		{name: "prints usage with no command", args: nil, wantCode: cli.ExitUsage, wantErr: "Usage: ambermoth"},
		{name: "prints the version", args: []string{"version"}, wantCode: cli.ExitOK, wantOut: "ambermoth test"},
		{name: "prints help on stdout", args: []string{"help"}, wantCode: cli.ExitOK, wantOut: "verify-pack"},
		{name: "rejects an unknown command", args: []string{"frobnicate"}, wantCode: cli.ExitUsage, wantErr: `unknown command "frobnicate"`},
		{name: "rejects an unknown flag", args: []string{"report", "--nope"}, wantCode: cli.ExitUsage, wantErr: "flag provided but not defined"},
		{name: "explains a missing ledger", args: []string{"report"}, wantCode: cli.ExitFail, wantErr: "run 'ambermoth init'"},
		{name: "requires pack dates", args: []string{"pack"}, wantCode: cli.ExitFail, wantErr: "--from and --to are required"},
		{name: "rejects a malformed day", args: []string{"pack", "--from", "2026-13-01", "--to", "2026-01-02"}, wantCode: cli.ExitFail, wantErr: "want YYYY-MM-DD"},
		{name: "rejects a bad disclose list", args: []string{"pack", "--from", "2026-01-01", "--to", "2026-01-02", "--disclose", "1,x"}, wantCode: cli.ExitFail, wantErr: "--disclose"},
		{name: "requires a link purpose", args: []string{"link"}, wantCode: cli.ExitFail, wantErr: "--purpose is required"},
		{name: "requires a key action", args: []string{"key"}, wantCode: cli.ExitFail, wantErr: "usage: ambermoth key"},
		{name: "requires reconcile inputs", args: []string{"reconcile"}, wantCode: cli.ExitFail, wantErr: "--provider and --csv are required"},
		{name: "rejects an invalid amend purpose", args: []string{"amend", "--seq", "1", "--purpose", "Not Valid"}, wantCode: cli.ExitFail, wantErr: "invalid purpose"},
		{name: "needs one pack path", args: []string{"verify-pack"}, wantCode: cli.ExitFail, wantErr: "usage: ambermoth verify-pack"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			newHarness(t).check(t, c)
		})
	}
}

func TestLedgerCommands(t *testing.T) {
	h := newHarness(t)
	h.env["TEST_KEY"] = "sk-from-env"
	h.secret = "sk-from-terminal"
	steps := []runCase{
		{name: "init creates the ledger", args: []string{"init"}, wantCode: cli.ExitOK, wantOut: "Ledger created in"},
		{name: "init refuses an existing ledger", args: []string{"init"}, wantCode: cli.ExitFail, wantErr: "error:"},
		{name: "enrolls a key from the environment", args: []string{"key", "enroll", "--provider", "openai", "--key-env", "TEST_KEY"}, wantCode: cli.ExitOK, wantOut: "recorded key enroll: sha256:"},
		{name: "enrolls a key from the terminal", args: []string{"key", "enroll", "--provider", "anthropic"}, wantCode: cli.ExitOK, wantErr: "API key (read from stdin, not stored)"},
		{name: "rejects an empty key variable", args: []string{"key", "enroll", "--provider", "openai", "--key-env", "UNSET"}, wantCode: cli.ExitFail, wantErr: "$UNSET is empty"},
		{name: "verifies the chain", args: []string{"verify"}, wantCode: cli.ExitOK, wantOut: "Ledger verification: OK"},
		{name: "verifies as JSON", args: []string{"verify", "--json"}, wantCode: cli.ExitOK, wantOut: `"head_hash"`},
		{name: "reports usage", args: []string{"report", "--from", "2026-03-01", "--to", "2026-03-31"}, wantCode: cli.ExitOK, wantOut: "2 key(s)"},
		{name: "lists alerts", args: []string{"alerts"}, wantCode: cli.ExitOK},
		{name: "lists alerts as JSON", args: []string{"alerts", "--json"}, wantCode: cli.ExitOK},
		{name: "amends only call entries", args: []string{"amend", "--seq", "1", "--annotation", "test key", "--reason", "setup", "--by", "rafa"}, wantCode: cli.ExitFail, wantErr: "no call entries in range 1..1"},
		{name: "refuses an empty pack", args: []string{"pack", "--from", "2026-03-01", "--to", "2026-03-31", "--no-tsa", "--out", filepath.Join(filepath.Dir(h.dir), "p.zip")}, wantCode: cli.ExitFail, wantErr: "no calls in the selected period"},
	}
	for _, c := range steps {
		t.Run(c.name, func(t *testing.T) { h.check(t, c) })
	}
}

func TestVerifyFailsOnTamperedLedger(t *testing.T) {
	h := newHarness(t)
	h.env["TEST_KEY"] = "sk-test"
	h.check(t, runCase{args: []string{"init"}, wantCode: cli.ExitOK})
	h.check(t, runCase{args: []string{"key", "enroll", "--provider", "openai", "--key-env", "TEST_KEY"}, wantCode: cli.ExitOK})
	path := filepath.Join(h.dir, ledger.LedgerFile)
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, bytes.Replace(b, []byte(`"openai"`), []byte(`"0penai"`), 1), 0o600); err != nil {
		t.Fatal(err)
	}
	h.check(t, runCase{args: []string{"verify"}, wantCode: cli.ExitFail, wantOut: "Ledger verification: FAILED"})
}

func TestDirFromEnvironment(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "from-env")
	var out bytes.Buffer
	app := cli.New(cli.Env{
		Stdin: strings.NewReader(""), Stdout: &out, Stderr: &out,
		Getenv: func(k string) string {
			if k == "AMBERMOTH_DIR" {
				return dir
			}
			return ""
		},
		Clock: fixedClock, Logger: discardLogger(), Version: "test",
	})
	if code := app.Run(context.Background(), []string{"init"}); code != cli.ExitOK {
		t.Fatalf("init exit %d: %s", code, out.String())
	}
	if _, err := os.Stat(filepath.Join(dir, ledger.LedgerFile)); err != nil {
		t.Fatalf("ledger not created in $AMBERMOTH_DIR: %v", err)
	}
}
