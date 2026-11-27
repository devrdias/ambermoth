package cli

import (
	"context"
	"embed"
	"strings"
	"text/template"

	"github.com/devrdias/ambermoth/internal/proxy"
)

//go:embed prompts/*.md
var promptFiles embed.FS

var prompts = template.Must(template.ParseFS(promptFiles, "prompts/*.md"))

// promptData fills the agent prompts. Purposes comes from the taxonomy so the
// prompt can't drift from what the proxy accepts.
type promptData struct {
	Dir      string
	Purposes string
}

// cmdAgentPrompt prints setup instructions to paste into a coding agent.
func (a *App) cmdAgentPrompt(_ context.Context, args []string) error {
	fs, dir := a.newFlags("agent-prompt")
	machine := fs.Bool("machine", false, "set up this machine instead of the current repository")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	name := "repo.md"
	if *machine {
		name = "machine.md"
	}
	return prompts.ExecuteTemplate(a.out, name, promptData{
		Dir:      *dir,
		Purposes: strings.Join(proxy.Taxonomy(), ", "),
	})
}
