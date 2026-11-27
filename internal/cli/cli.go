// Package cli implements the ambermoth command line. Everything it takes from
// the process (streams, environment, clock, logger) comes in through Env, so
// main only wires it up and the commands can be tested in-process.
package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"time"

	"github.com/devrdias/ambermoth/internal/ledger"
	"github.com/devrdias/ambermoth/internal/proxy"
)

// Exit codes returned by App.Run.
const (
	ExitOK    = 0 // success
	ExitFail  = 1 // an error, or a verification that failed
	ExitUsage = 2 // bad command line
)

// defaultDir is the ledger directory used when neither --dir nor $AMBERMOTH_DIR is set.
const defaultDir = ".ambermoth"

const usage = `ambermoth — a tamper-evident ledger of your LLM API usage

Usage: ambermoth <command> [flags]

Setup and capture
  init                       create a ledger in ./.ambermoth (or --dir)
  proxy                      run the recording proxy
  key enroll|rotate|revoke   record a key event (key read from stdin or --key-env)
  link                       bind a purpose to a git repository / commit
  amend                      re-tag or annotate earlier calls (itself recorded)
  agent-prompt               print setup instructions for a coding agent (--machine for this machine)

Integrity
  anchor                     RFC 3161-timestamp the chain head
  verify                     verify the ledger's chain and anchors

Evidence
  pack                       build an evidence pack (zip)
  verify-pack <pack.zip>     verify a pack offline (for reviewers)
  check-sample               check original request/response against a disclosed entry
  reconcile                  compare the ledger with a provider usage export (CSV)
  alerts                     advisory anomaly checks (volume, concentration, new keys)
  report                     usage summary in the terminal

Global: --dir (default $AMBERMOTH_DIR or .ambermoth)
Run 'ambermoth <command> -h' for flags.
`

// Env is what the CLI needs from the process.
type Env struct {
	Stdin   io.Reader
	Stdout  io.Writer
	Stderr  io.Writer
	Getenv  func(string) string
	Clock   ledger.Clock
	Logger  *slog.Logger
	Version string
	// ReadSecret reads one line (an API key) without echoing it.
	ReadSecret func() (string, error)
}

// App runs ambermoth commands.
type App struct {
	env      Env
	commands map[string]command
}

// command runs one subcommand with the arguments after its name.
type command func(ctx context.Context, args []string) error

// exitError ends a command with a specific exit code and no further message.
type exitError int

func (e exitError) Error() string { return fmt.Sprintf("exit %d", int(e)) }

// New returns an App that uses env.
func New(env Env) *App {
	a := &App{env: env}
	a.commands = map[string]command{
		"init": a.cmdInit, "proxy": a.cmdProxy, "key": a.cmdKey, "link": a.cmdLink,
		"amend": a.cmdAmend, "anchor": a.cmdAnchor, "verify": a.cmdVerify, "pack": a.cmdPack,
		"verify-pack": a.cmdVerifyPack, "check-sample": a.cmdCheckSample, "reconcile": a.cmdReconcile,
		"alerts": a.cmdAlerts, "report": a.cmdReport, "agent-prompt": a.cmdAgentPrompt,
	}
	return a
}

// Run executes the command line (without the program name) and returns the exit code.
func (a *App) Run(ctx context.Context, args []string) int {
	if len(args) == 0 {
		_, _ = fmt.Fprint(a.env.Stderr, usage)
		return ExitUsage
	}
	name, rest := args[0], args[1:]
	switch name {
	case "version", "--version", "-v":
		_, _ = fmt.Fprintln(a.env.Stdout, "ambermoth", a.env.Version)
		return ExitOK
	case "help", "-h", "--help":
		_, _ = fmt.Fprint(a.env.Stdout, usage)
		return ExitOK
	}
	cmd, ok := a.commands[name]
	if !ok {
		_, _ = fmt.Fprintf(a.env.Stderr, "unknown command %q\n\n%s", name, usage)
		return ExitUsage
	}
	return a.exitCode(cmd(ctx, rest))
}

func (a *App) exitCode(err error) int {
	var ee exitError
	switch {
	case err == nil, errors.Is(err, flag.ErrHelp):
		return ExitOK
	case errors.As(err, &ee):
		return int(ee)
	case errors.Is(err, errUsage):
		return ExitUsage
	}
	_, _ = fmt.Fprintln(a.env.Stderr, "error:", err)
	return ExitFail
}

// errUsage marks a bad command line; flag parsing has already printed why.
var errUsage = errors.New("usage")

func (a *App) generator() string { return "ambermoth/" + a.env.Version }

func (a *App) now() time.Time { return a.env.Clock().UTC() }

// newFlags returns a flag set for a command and its --dir flag.
func (a *App) newFlags(name string) (*flag.FlagSet, *string) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(a.env.Stderr)
	def := a.env.Getenv("AMBERMOTH_DIR")
	if def == "" {
		def = defaultDir
	}
	return fs, fs.String("dir", def, "ledger directory")
}

// parse parses flags, allowing positional arguments before them, and returns
// the positional arguments.
func parse(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		pos = append(pos, args[0])
		args = args[1:]
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil, err
		}
		return nil, errUsage
	}
	return append(pos, fs.Args()...), nil
}

// multi is a repeatable string flag.
type multi []string

func (m *multi) String() string     { return strings.Join(*m, ",") }
func (m *multi) Set(v string) error { *m = append(*m, v); return nil }

// open opens the ledger in dir and its proxy config.
func (a *App) open(dir string) (*ledger.Store, proxy.Config, error) {
	s, err := ledger.Open(dir, a.env.Clock)
	if err != nil {
		return nil, proxy.Config{}, fmt.Errorf("opening ledger in %s: %w (run 'ambermoth init'?)", dir, err)
	}
	cfg, err := proxy.LoadConfig(filepath.Join(dir, ledger.ConfigFile))
	return s, cfg, err
}

// parseDay parses a YYYY-MM-DD flag value.
func parseDay(flagName, s string) (time.Time, error) {
	t, err := time.Parse("2006-01-02", s)
	if err != nil {
		return time.Time{}, fmt.Errorf("--%s: want YYYY-MM-DD, got %q", flagName, s)
	}
	return t, nil
}
