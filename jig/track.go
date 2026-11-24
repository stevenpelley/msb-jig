package jig

import (
	"maps"
	"path/filepath"
	"slices"
	"strings"

	"github.com/stevenpelley/msb-jig/agent"
	"github.com/stevenpelley/msb-jig/allowlist"
	"github.com/stevenpelley/msb-jig/editor"
	"github.com/stevenpelley/msb-jig/guest"
	"github.com/stevenpelley/msb-jig/sandbox"
)

// Track is what differs between the personal and the work harness
// (docs/tracks.md): the providers the broker can serve, the agent in the
// sandbox, the editor on the Mac, the VM runtime, the image the base image
// builds on, the built-in allowlist, and the track's own provisioning and
// guest settings. The rest (the broker, the allowlist's format,
// provisioning, the commands) is shared. A track's own command calls Main
// with it.
type Track struct {
	// Providers are the providers mono-jig login offers, in the order
	// login all asks (ProviderCLI: GitLab, GitHub, Claude, AWS, or a
	// track's own).
	Providers []ProviderCLI
	Agent     agent.Agent     // required
	Editor    editor.Editor   // required
	Backend   sandbox.Backend // required
	// BaseImage is what the base image builds on, and the plain image
	// without one; JIG_BASE_IMAGE overrides. Its packages must suit the
	// provision components (Debian or Ubuntu, unless Components replaces
	// base). Empty: ubuntu:26.04.
	BaseImage string
	// Allowlist is the built-in allowlist layers, before JIG_LAYERS and
	// the user's (allowlist.Base, an org's from allowlist.MustParseLayer).
	// Empty: allowlist.Base() alone.
	Allowlist []allowlist.Layer
	// Components are provision files the track adds or replaces, by path
	// (components/NN-<name>.sh; guest.Provision lists the library's). A
	// new component runs at the image build and a sandbox's first start,
	// like the agent's; a replaced one runs when the library's would.
	Components map[string]string
	// Guest is the track's own guest settings, applied at every start
	// after the providers': environment (package mirrors, say), packages,
	// files, and a section of the agent's instructions.
	Guest guest.Config
	// BrokerMode is the broker's mode for a sandbox created without
	// --broker: "server" or "proxy" (jig/brokermode.go). Empty: server.
	BrokerMode string
}

// Personal is the personal track: GitLab, GitHub, Claude and AWS; Claude
// Code; VS Code; microsandbox; Ubuntu; the base allowlist.
func Personal() Track {
	return Track{
		Providers: []ProviderCLI{GitLab, GitHub, Claude, AWS},
		Agent:     agent.ClaudeCode{},
		Editor:    editor.VSCode{},
		Backend:   sandbox.Microsandbox{},
		BaseImage: "ubuntu:26.04",
		Allowlist: []allowlist.Layer{allowlist.Base()},
	}
}

// withDefaults fills t's empty optional fields, and fails on a missing
// required one.
func (t Track) withDefaults() Track {
	if t.Agent == nil || t.Editor == nil || t.Backend == nil {
		fail("the track needs an Agent, an Editor and a Backend (jig.Track)")
	}
	if t.BaseImage == "" {
		t.BaseImage = "ubuntu:26.04"
	}
	if len(t.Allowlist) == 0 {
		t.Allowlist = []allowlist.Layer{allowlist.Base()}
	}
	if t.BrokerMode == "" {
		t.BrokerMode = modeServer
	}
	if t.BrokerMode != modeServer && t.BrokerMode != modeProxy {
		fail("the track's BrokerMode %q: want server or proxy", t.BrokerMode)
	}
	if err := t.Guest.Validate(); err != nil {
		fail("the track's guest settings: %v", err)
	}
	return t
}

// track is the one Main runs.
var track Track

// editorProfile is what the editor's Settings need: every sandbox under
// root, and the agent's extensions.
func editorProfile(root string) editor.Profile {
	return editor.Profile{SSHConfig: filepath.Join(root, "ssh_config"), Hosts: sshHosts(root), Extensions: track.Agent.EditorExtensions()}
}

// provisionFiles are provision/'s files with the agent's and the
// track's.
func provisionFiles() map[string]string {
	f := guest.Provision()
	maps.Copy(f, track.Agent.Components())
	maps.Copy(f, track.Components)
	return f
}

// addedComponents are the components the agent and the track add (not
// those they replace), in run order, space-separated.
func addedComponents() string {
	lib := guest.Provision()
	var names []string
	for _, p := range slices.Sorted(maps.Keys(provisionFiles())) {
		if _, replaced := lib[p]; !replaced {
			if n := guest.ComponentName(p); n != "" {
				names = append(names, n)
			}
		}
	}
	return strings.Join(names, " ")
}
