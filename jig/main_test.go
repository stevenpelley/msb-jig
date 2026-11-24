package jig

import (
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/stevenpelley/msb-jig/agent"
	"github.com/stevenpelley/msb-jig/editor"
	"github.com/stevenpelley/msb-jig/guest"
	"github.com/stevenpelley/msb-jig/sandbox"
)

// The tests run as the personal track.
func TestMain(m *testing.M) {
	track = Personal()
	os.Exit(m.Run())
}

// A track offers only its own providers: another's settings are ignored.
func TestTrackProviders(t *testing.T) {
	defer func(old Track) { track = old }(track)
	t.Setenv("JIG_STATE", t.TempDir())
	t.Setenv("JIG_CLAUDE_TOKEN_COMMAND", "")
	for _, k := range []string{"JIG_GITLAB_USER", "JIG_AWS_PROFILES", "JIG_AWS_MCP_COMMAND"} {
		os.Unsetenv(k)
	}
	var s Settings
	s.SetProvider("gitlab", gitlabSettings{User: "alice", URL: "https://gitlab.com", Token: "glab"})
	s.SetProvider("aws", awsSettings{Profiles: []string{"p"}})
	SaveSettings(s)
	track = Personal()
	track.Providers = []ProviderCLI{AWS}
	b := brokerConfig()
	if len(b.Providers) != 1 || b.Get("aws") == nil {
		t.Errorf("%+v", b.Providers)
	}
	if _, ok := cliProviderNamed("gitlab"); ok {
		t.Error("gitlab offered")
	}
}

// A track's components: a new one runs at the image build and the first
// start, after the library's and the agent's; a replaced one doesn't add a
// run.
func TestTrackComponents(t *testing.T) {
	defer func(old Track) { track = old }(track)
	track = Personal()
	track.Components = map[string]string{
		"components/20-base.sh":  "# base for another distribution\n",
		"components/65-tools.sh": "# tools\n",
	}
	if got := addedComponents(); got != "tools claude" {
		t.Errorf("added %q", got)
	}
	if got := provisionFiles()["components/20-base.sh"]; got != "# base for another distribution\n" {
		t.Errorf("base not replaced: %q", got)
	}
	if got := imageComponents(); got != "base git docker tools claude" {
		t.Errorf("image components %q", got)
	}
}

// A track's guest settings follow the providers', and its instructions
// come after theirs, under the header.
func TestTrackGuest(t *testing.T) {
	defer func(old Track) { track = old }(track)
	track = Personal()
	track.Guest = guest.Config{Env: map[string]string{"GOPROXY": "https://proxy.example.org"},
		Packages: []string{"make"}, Instructions: []string{"## Work\n"}}
	g := guestConfig(allOn(false))
	if g.Env["GOPROXY"] != "https://proxy.example.org" || !slices.Contains(g.Packages, "make") {
		t.Errorf("env %v packages %v", g.Env, g.Packages)
	}
	f, ok := file(g, "~/.claude/CLAUDE.md")
	if !ok || !strings.HasSuffix(f.Content, "## Work\n") || strings.Index(f.Content, "## Work") < strings.Index(f.Content, "GitLab") {
		t.Errorf("instructions: %q", f.Content)
	}
}

// Main fills a track's optional fields.
func TestTrackDefaults(t *testing.T) {
	tr := Track{Agent: agent.ClaudeCode{}, Editor: editor.VSCode{}, Backend: sandbox.Microsandbox{}}.withDefaults()
	if tr.BaseImage != "ubuntu:26.04" || len(tr.Allowlist) != 1 || tr.Allowlist[0].Name != "base" {
		t.Errorf("%+v", tr)
	}
}
