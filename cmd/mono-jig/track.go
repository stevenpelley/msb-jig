package main

import (
	"maps"
	"path/filepath"
	"strings"

	"mono-jig/agent"
	"mono-jig/editor"
	"mono-jig/guest"
)

// The personal track's agent and editor. Step 7 of docs/library-split.md
// makes these, the providers (registry.go) and the base image a Track.
var (
	theAgent  agent.Agent   = agent.ClaudeCode{}
	theEditor editor.Editor = editor.VSCode{}
)

// editorProfile is what the editor's settings need: every sandbox under
// root, and the agent's extensions.
func editorProfile(root string) editor.Profile {
	return editor.Profile{SSHConfig: filepath.Join(root, "ssh_config"), Hosts: sshHosts(root), Extensions: theAgent.EditorExtensions()}
}

// provisionFiles are provision/'s files with the agent's component.
func provisionFiles() map[string]string {
	f := guest.Provision()
	maps.Copy(f, theAgent.Components())
	return f
}

// agentComponents are the agent's provision components, space-separated.
func agentComponents() string { return strings.Join(theAgent.ComponentNames(), " ") }
