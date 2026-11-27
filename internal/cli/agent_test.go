package cli_test

import (
	"strings"
	"testing"

	"github.com/devrdias/ambermoth/internal/cli"
	"github.com/devrdias/ambermoth/internal/proxy"
)

func TestAgentPrompt(t *testing.T) {
	t.Parallel()
	cases := []runCase{
		{name: "prints the repo prompt by default", args: []string{"agent-prompt"}, wantCode: cli.ExitOK, wantOut: "Set up ambermoth in this repository"},
		{name: "prints the machine prompt", args: []string{"agent-prompt", "--machine"}, wantCode: cli.ExitOK, wantOut: "Set up ambermoth on this machine"},
		{name: "rejects an unknown flag", args: []string{"agent-prompt", "--nope"}, wantCode: cli.ExitUsage, wantErr: "flag provided but not defined"},
		{name: "is listed in help", args: []string{"help"}, wantCode: cli.ExitOK, wantOut: "agent-prompt"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			newHarness(t).check(t, c)
		})
	}
}

func TestAgentPromptNeedsNoLedger(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	code, out, errOut := h.run("agent-prompt")
	if code != cli.ExitOK || out == "" {
		t.Fatalf("exit %d, stdout %q, stderr %q", code, out, errOut)
	}
}

type promptCase struct {
	name string
	args []string
}

func TestAgentPromptUsesTheLedgerDir(t *testing.T) {
	t.Parallel()
	cases := []promptCase{
		{name: "repo prompt", args: []string{"agent-prompt"}},
		{name: "machine prompt", args: []string{"agent-prompt", "--machine"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t)
			_, out, _ := h.run(c.args...)
			if !strings.Contains(out, "--dir "+h.dir) {
				t.Errorf("prompt does not pass --dir %s:\n%s", h.dir, out)
			}
			if strings.Contains(out, "{{") {
				t.Errorf("unexpanded template:\n%s", out)
			}
		})
	}
}

func TestAgentPromptListsEveryPurpose(t *testing.T) {
	t.Parallel()
	_, out, _ := newHarness(t).run("agent-prompt")
	for _, p := range proxy.Taxonomy() {
		if !strings.Contains(out, p) {
			t.Errorf("repo prompt does not list purpose %q", p)
		}
	}
}
