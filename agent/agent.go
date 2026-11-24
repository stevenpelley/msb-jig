// Package agent is the coding agent in the sandbox: the provision
// component that installs it, its settings and where its instructions go
// (guest.Config), and the editor extensions it brings. ClaudeCode is the
// personal track's; a track picks one (docs/library-split.md).
package agent

import (
	_ "embed"
	"strings"

	"mono-jig/broker/providers"
	"mono-jig/guest"
)

// Agent is a coding agent.
type Agent interface {
	Name() string
	// Components are the provision files it adds (components/NN-<name>.sh,
	// as guest.Provision), and ComponentNames the components to run: at
	// the image build and a first start.
	Components() map[string]string
	ComponentNames() []string
	// Guest is its settings for the providers that are on, with the
	// instructions (the providers' sections) in its own file.
	Guest(b providers.Config, instructions []string) guest.Config
	// EditorExtensions are extensions the editor installs on the remote
	// for it (marketplace IDs).
	EditorExtensions() []string
}

// ClaudeCode is Claude Code: installed for the sandbox's user, calling the
// broker for Anthropic with a placeholder token (the credential stays on
// the Mac), reading its instructions from ~/.claude/CLAUDE.md.
type ClaudeCode struct{}

//go:embed 70-claude.sh
var claudeComponent string

func (ClaudeCode) Name() string { return "claude-code" }

func (ClaudeCode) Components() map[string]string {
	return map[string]string{"components/70-claude.sh": claudeComponent}
}

func (ClaudeCode) ComponentNames() []string { return []string{"claude"} }

func (ClaudeCode) EditorExtensions() []string { return []string{"anthropic.claude-code"} }

// ClaudeMarker is the first line of the agent's instructions, as
// provision.sh wrote it before guest.Config.
const ClaudeMarker = "<!-- Written by mono-jig provision.sh; edits are lost on reprovisioning. -->"

func (ClaudeCode) Guest(b providers.Config, instructions []string) guest.Config {
	var g guest.Config
	if b.Get("anthropic") != nil {
		g.Env = map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": "placeholder"}
		if !b.Proxy {
			g.Env["ANTHROPIC_BASE_URL"] = "https://" + providers.AnthropicHost
		}
		// Until onboarding is done, interactive claude checks
		// api.anthropic.com directly, ignoring ANTHROPIC_BASE_URL, and the
		// sandbox can't resolve it ("Unable to connect to Anthropic
		// services"; -p skips the check).
		g.Files = append(g.Files, guest.File{Path: "~/.claude.json", Owner: "user", Mode: "0600",
			Merge: map[string]any{"hasCompletedOnboarding": true}})
	}
	if len(instructions) > 0 {
		g.Files = append(g.Files, guest.File{Path: "~/.claude/CLAUDE.md", Owner: "user", Marker: ClaudeMarker,
			Content: ClaudeMarker + "\n" + strings.Join(instructions, "")})
	}
	return g
}
