// Command ambermoth records LLM API usage in a tamper-evident ledger and
// builds evidence packs from it. The commands live in internal/cli.
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/devrdias/ambermoth/internal/cli"
)

// version is the release version.
//
//rules:readonly set at build time with -ldflags "-X main.version=<release>", which only works on a var.
var version = "dev"

func main() {
	// Without this, a write to a closed stdout or stderr kills the process with
	// SIGPIPE before the CLI sees an error. Taking the signal makes the write
	// fail with EPIPE instead, which is what the tests see too. Nobody reads the
	// channel; it only has to exist.
	signal.Notify(make(chan os.Signal, 1), syscall.SIGPIPE)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	app := cli.New(cli.Env{
		Stdin:      os.Stdin,
		Stdout:     os.Stdout,
		Stderr:     os.Stderr,
		Getenv:     os.Getenv,
		Clock:      time.Now,
		Logger:     slog.New(slog.NewTextHandler(os.Stderr, nil)),
		Version:    version,
		ReadSecret: cli.TerminalSecret(os.Stdin, os.Stderr),
	})
	code := app.Run(ctx, os.Args[1:])
	stop()
	os.Exit(code)
}
