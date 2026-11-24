package jig

import (
	"maps"
	"path/filepath"
	"strings"

	"github.com/stevenpelley/msb-jig/agent"
	"github.com/stevenpelley/msb-jig/editor"
	"github.com/stevenpelley/msb-jig/guest"
	"github.com/stevenpelley/msb-jig/sandbox"
)

// Track is what differs between the personal and the work harness
// (docs/library-split.md): the providers the broker can serve, the agent
// in the sandbox, the editor on the Mac, the VM runtime and the image the
// base image builds on. The rest (the broker, the allowlist, provisioning,
// the commands) is shared. A track's own command calls Main with it.
type Track struct {
	// Providers are the providers mono-jig login offers, in the order
	// login all asks (ProviderCLI: GitLab, GitHub, Claude, AWS, or a
	// track's own).
	Providers []ProviderCLI
	Agent     agent.Agent
	Editor    editor.Editor
	Backend   sandbox.Backend
	// BaseImage is what the base image builds on, and the plain image
	// without one; JIG_BASE_IMAGE overrides. Its packages must suit
	// provision/'s components.
	BaseImage string
}

// Personal is the personal track: GitLab, GitHub, Claude and AWS; Claude
// Code; VS Code; microsandbox; Ubuntu.
func Personal() Track {
	return Track{
		Providers: []ProviderCLI{GitLab, GitHub, Claude, AWS},
		Agent:     agent.ClaudeCode{},
		Editor:    editor.VSCode{},
		Backend:   sandbox.Microsandbox{},
		BaseImage: "ubuntu:26.04",
	}
}

// track is the one Main runs.
var track Track

// editorProfile is what the editor's Settings need: every sandbox under
// root, and the agent's extensions.
func editorProfile(root string) editor.Profile {
	return editor.Profile{SSHConfig: filepath.Join(root, "ssh_config"), Hosts: sshHosts(root), Extensions: track.Agent.EditorExtensions()}
}

// provisionFiles are provision/'s files with the agent's component.
func provisionFiles() map[string]string {
	f := guest.Provision()
	maps.Copy(f, track.Agent.Components())
	return f
}

// agentComponents are the agent's provision components, space-separated.
func agentComponents() string { return strings.Join(track.Agent.ComponentNames(), " ") }
