package cli

import (
	"errors"
	"fmt"
	"io"
	"syscall"
)

// outWriter passes writes to w until one fails, then keeps that error and
// drops later writes. Commands print without checking each call; exitCode
// looks at err once, when the command is over.
type outWriter struct {
	w   io.Writer
	err error
}

func (o *outWriter) Write(p []byte) (int, error) {
	if o.err != nil {
		return 0, o.err
	}
	n, err := o.w.Write(p)
	o.err = err
	return n, err
}

// changesState reports whether the command has done its work by the time it
// prints, so that a failed print must not be answered with a retry.
func changesState(command string) bool {
	switch command {
	case "init", "key", "link", "amend", "anchor", "reconcile", "pack":
		return true
	}
	return false
}

// reportWriteFailure tells the user that the result of command was not
// delivered. A closed pipe (EPIPE) is the reader's choice, as with '| head',
// so it is not an error worth a message.
func (a *App) reportWriteFailure(command string, err error) {
	if errors.Is(err, syscall.EPIPE) {
		return
	}
	note := ""
	if changesState(command) {
		note = " (the work itself was done; do not run it again)"
	}
	_, _ = fmt.Fprintf(a.env.Stderr, "error: writing output: %v%s\n", err, note)
}
