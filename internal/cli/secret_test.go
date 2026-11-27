package cli_test

import (
	"bytes"
	"os"
	"strings"
	"testing"

	"github.com/devrdias/ambermoth/internal/cli"
)

type secretCase struct {
	name  string
	input string
	want  string
}

// pipeWith returns the read end of a pipe that holds input and is closed.
func pipeWith(t *testing.T, input string) *os.File {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close() })
	if _, err := w.WriteString(input); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return r
}

func TestTerminalSecretReadsAPipe(t *testing.T) {
	t.Parallel()
	cases := []secretCase{
		{name: "reads a line", input: "sk-one\n", want: "sk-one"},
		{name: "takes a last line without a newline", input: "sk-two", want: "sk-two"},
		{name: "drops the carriage return of a CRLF line", input: "sk-three\r\n", want: "sk-three"},
		{name: "trims surrounding spaces", input: "  sk-four \t\n", want: "sk-four"},
		{name: "reads only the first line", input: "sk-five\nsecond\n", want: "sk-five"},
		{name: "returns an empty key for an empty line", input: "\n", want: ""},
		{name: "returns an empty key at end of input", input: "", want: ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			var errOut bytes.Buffer
			got, err := cli.TerminalSecret(pipeWith(t, c.input), &errOut)()
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != c.want {
				t.Errorf("secret = %q, want %q", got, c.want)
			}
			if errOut.Len() != 0 {
				t.Errorf("wrote %q to the error stream; a pipe is not a terminal, so there is nothing to restore", errOut.String())
			}
		})
	}
}

func TestTerminalSecretOnACharacterDevice(t *testing.T) {
	t.Parallel()
	// /dev/null is a character device but not a terminal: stty cannot turn
	// echo off on it, so the read must carry on without a restore step.
	in, err := os.Open(os.DevNull)
	if err != nil {
		t.Skipf("no %s: %v", os.DevNull, err)
	}
	t.Cleanup(func() { _ = in.Close() })
	var errOut bytes.Buffer
	got, err := cli.TerminalSecret(in, &errOut)()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "" || errOut.Len() != 0 {
		t.Errorf("secret = %q, error stream = %q; want both empty", got, errOut.String())
	}
}

func TestTerminalSecretReportsReadErrors(t *testing.T) {
	t.Parallel()
	in := pipeWith(t, "sk-closed\n")
	if err := in.Close(); err != nil {
		t.Fatal(err)
	}
	got, err := cli.TerminalSecret(in, &bytes.Buffer{})()
	if err == nil || !strings.Contains(err.Error(), "reading key") {
		t.Fatalf("error = %v, want one that starts with 'reading key'", err)
	}
	if got != "" {
		t.Errorf("secret = %q, want none with an error", got)
	}
}
