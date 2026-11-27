package cli

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
)

// TerminalSecret returns an Env.ReadSecret that reads one line from in. When
// in is a terminal, echo is turned off (with stty, to stay dependency-free)
// so the key is not shown; progress goes to errOut.
func TerminalSecret(in *os.File, errOut io.Writer) func() (string, error) {
	return func() (string, error) {
		if restore := noEcho(in); restore != nil {
			defer func() {
				restore()
				fmt.Fprintln(errOut)
			}()
		}
		return readLine(in)
	}
}

// noEcho turns terminal echo off and returns the function that turns it back
// on, or nil when in is not a terminal or stty is unavailable.
func noEcho(in *os.File) func() {
	fi, err := in.Stat()
	if err != nil || fi.Mode()&os.ModeCharDevice == 0 {
		return nil
	}
	stty := func(arg string) error {
		c := exec.Command("stty", arg)
		c.Stdin = in
		return c.Run()
	}
	if stty("-echo") != nil {
		return nil
	}
	return func() { _ = stty("echo") }
}

// readLine reads one line from r without its trailing newline.
func readLine(r io.Reader) (string, error) {
	line, err := bufio.NewReader(r).ReadString('\n')
	if err != nil && err != io.EOF {
		return "", fmt.Errorf("reading key: %w", err)
	}
	return strings.TrimSpace(line), nil
}
