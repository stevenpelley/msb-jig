package agent

import (
	_ "embed"
	"strings"

	"github.com/stevenpelley/msb-jig/broker/providers"
	"github.com/stevenpelley/msb-jig/guest"
)

// KiroCLI is kiro-cli: installed in /usr/local/bin (a pinned release),
// reading its instructions from a global steering file
// (~/.kiro/steering/mono-jig.md), allowed every tool without prompts
// (~/.kiro/settings/permissions.yaml: the sandbox is the boundary), with
// its self-update off. It signs in inside the sandbox (kiro-cli login
// --use-device-flow), so its credential is the sandbox's; no provider
// holds it. It reaches MCP servers through the broker with mcpcli or
// curl, as the providers' instructions say, not through its own MCP
// configuration.
type KiroCLI struct{}

//go:embed 70-kiro.sh
var kiroComponent string

func (KiroCLI) Name() string { return "kiro-cli" }

func (KiroCLI) Components() map[string]string {
	return map[string]string{"components/70-kiro.sh": kiroComponent}
}

// EditorExtensions: none; kiro-cli runs in the editor's terminal.
func (KiroCLI) EditorExtensions() []string { return nil }

// KiroMarker is the first line of the files mono-jig writes for kiro-cli.
const KiroMarker = "<!-- Written by mono-jig; edits are lost at the next start. -->"

// kiroPermissionsMarker is the permissions file's, as a YAML comment.
const kiroPermissionsMarker = "# Written by mono-jig; edits are lost at the next start."

func (KiroCLI) Guest(_ providers.Config, instructions []string) guest.Config {
	g := guest.Config{Files: []guest.File{
		{Path: "~/.kiro/settings/permissions.yaml", Owner: "user", Marker: kiroPermissionsMarker,
			Content: kiroPermissionsMarker + "\n" +
				"# The sandbox is the boundary: every capability is allowed without a\n" +
				"# prompt (Kiro still asks for its own protected paths).\n" +
				"rules:\n  - capability: all\n    effect: allow\n"},
		{Path: "~/.kiro/settings/cli.json", Owner: "user", Mode: "0600",
			Merge: map[string]any{"app.disableAutoupdates": true}},
	}}
	if len(instructions) > 0 {
		// No front matter: a steering file without one is always included
		// (unverified for kiro-cli 3.5.0).
		g.Files = append(g.Files, guest.File{Path: "~/.kiro/steering/mono-jig.md", Owner: "user", Marker: KiroMarker,
			Content: KiroMarker + "\n" + strings.Join(instructions, "")})
	}
	return g
}
