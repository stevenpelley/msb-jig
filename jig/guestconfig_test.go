package jig

import (
	"slices"
	"strings"
	"testing"

	"github.com/stevenpelley/msb-jig/agent"
	"github.com/stevenpelley/msb-jig/broker/providers"
	"github.com/stevenpelley/msb-jig/guest"
)

func allOn(proxy bool) config {
	var c config
	c.proxy, c.broker.Proxy = proxy, proxy
	c.broker.Providers = []providers.Provider{
		providers.GitLab{User: "gl-user", URL: "https://gitlab.example.org"},
		providers.GitHub{User: "gh-user"},
		providers.Anthropic{TokenCmd: "true"},
		providers.AWS{Profiles: []string{"dev", "prod"}},
	}
	return c
}

func file(g guest.Config, path string) (guest.File, bool) {
	i := slices.IndexFunc(g.Files, func(f guest.File) bool { return f.Path == path })
	if i < 0 {
		return guest.File{}, false
	}
	return g.Files[i], true
}

func TestGuestConfigServerMode(t *testing.T) {
	g := guestConfig(allOn(false))
	if _, err := g.JSON(); err != nil {
		t.Fatal(err)
	}
	for k, v := range map[string]string{
		"GITLAB_HOST": "gitlab.broker.internal", "GITLAB_TOKEN": "placeholder", "GH_HOST": "github.broker.internal",
		"ANTHROPIC_BASE_URL": "https://anthropic.broker.internal", "CLAUDE_CODE_OAUTH_TOKEN": "placeholder",
		"MCP_GATEWAY_URL": providers.AWSURL, "GIT_TERMINAL_PROMPT": "0",
		"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC": "1", "CLAUDE_CODE_NO_FLICKER": "1",
	} {
		if g.Env[k] != v {
			t.Errorf("%s = %q, want %q", k, g.Env[k], v)
		}
	}
	if _, ok := g.Env["GH_TOKEN"]; ok {
		t.Error("GH_TOKEN in server mode")
	}
	gl := g.Git["url.https://gitlab.broker.internal/.insteadOf"]
	if !slices.Contains(gl, "git@gitlab.example.org:") || !slices.Contains(gl, "https://gitlab.example.org/") {
		t.Errorf("GitLab insteadOf: %v", gl)
	}
	if gh := g.Git["url.https://github.broker.internal/.insteadOf"]; !slices.Contains(gh, "https://github.com/") {
		t.Errorf("GitHub insteadOf: %v", gh)
	}
	if !slices.Equal(g.GitClear, legacyGitKeys) {
		t.Errorf("GitClear: %v", g.GitClear)
	}
	md, ok := file(g, "~/.claude/CLAUDE.md")
	if !ok {
		t.Fatal("no CLAUDE.md")
	}
	for _, s := range []string{agent.ClaudeMarker + "\n# Working in the mono-jig sandbox", "## GitLab", "agent/gl-user/",
		"the broker handles gitlab.example.org", "## GitHub", "agent/gh-user/", "## AWS", "profile\n`dev`", "Other profiles: prod."} {
		if !strings.Contains(md.Content, s) {
			t.Errorf("CLAUDE.md lacks %q:\n%s", s, md.Content)
		}
	}
	if glab, _ := file(g, "~/.config/glab-cli/config.yml"); !strings.Contains(glab.Content, "host: gitlab.broker.internal\n") {
		t.Errorf("glab config: %q", glab.Content)
	}
	if _, ok := file(g, "/usr/local/bin/mcpcli"); !ok {
		t.Error("no mcpcli link")
	}
}

func TestGuestConfigProxyMode(t *testing.T) {
	g := guestConfig(allOn(true))
	if g.Env["GH_TOKEN"] != "placeholder" || g.Env["GITLAB_HOST"] != "gitlab.example.org" {
		t.Errorf("env: %v", g.Env)
	}
	for _, k := range []string{"GH_HOST", "GH_ENTERPRISE_TOKEN", "ANTHROPIC_BASE_URL"} {
		if _, ok := g.Env[k]; ok {
			t.Errorf("%s in proxy mode", k)
		}
	}
	if len(g.Git) != 0 {
		t.Errorf("git in proxy mode: %v", g.Git)
	}
	// No internal names: AWS is at its own host, and the line only
	// removes an older sandbox's.
	if h := hostsLine(allOn(true)); strings.Contains(h, "127.0.0.1") {
		t.Errorf("hostsLine: %s", h)
	}
	if u := g.Env["MCP_GATEWAY_URL"]; u != "https://aws-mcp.us-east-1.api.aws/mcp" {
		t.Errorf("MCP_GATEWAY_URL %q", u)
	}
}

// With no provider: the base environment and the agent's own settings
// (no nonessential traffic, the full-screen renderer), and no files or instructions. The hosts line
// only removes the broker's names.
func TestGuestConfigNone(t *testing.T) {
	var c config
	g := guestConfig(c)
	if len(g.Env) != 6 || g.Env["CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC"] != "1" || g.Env["CLAUDE_CODE_NO_FLICKER"] != "1" || len(g.Files) != 0 || len(g.Instructions) != 0 {
		t.Errorf("%+v", g)
	}
	if h := hostsLine(c); strings.Contains(h, "127.0.0.1") {
		t.Errorf("hostsLine: %s", h)
	}
	if h := hostsLine(allOn(false)); !strings.Contains(h, `127.0.0.1 gitlab.broker.internal github.broker.internal anthropic.broker.internal aws.broker.internal\n`) {
		t.Errorf("hostsLine: %s", h)
	}
}

// The names the sandbox had (recorded at the last start, and the legacy
// ones) that aren't set now are removed.
func TestStaleEnv(t *testing.T) {
	state := t.TempDir()
	if err := recordEnv(state, map[string]string{"A": "1", "GH_HOST": "x", "KEEP": "y"}); err != nil {
		t.Fatal(err)
	}
	gone := staleEnv(state, map[string]string{"KEEP": "y", "GITLAB_HOST": "z"})
	if !slices.Contains(gone, "A") || !slices.Contains(gone, "GH_HOST") || slices.Contains(gone, "KEEP") || slices.Contains(gone, "GITLAB_HOST") {
		t.Errorf("staleEnv: %v", gone)
	}
}
