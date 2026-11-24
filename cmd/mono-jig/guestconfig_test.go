package main

import (
	"slices"
	"strings"
	"testing"

	"mono-jig/guest"
)

func allOn(proxy bool) config {
	var c config
	c.proxy = proxy
	c.broker.GitLabUser, c.broker.GitHubUser = "gl-user", "gh-user"
	c.broker.GitLabURL = "https://gitlab.example.org"
	c.broker.AnthropicTokenCmd = "true"
	c.broker.AWSProfiles = []string{"dev", "prod"}
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
		"MCP_GATEWAY_URL": awsURL, "GIT_TERMINAL_PROMPT": "0",
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
	if len(g.GitClear) != 0 {
		t.Errorf("GitClear: %v", g.GitClear)
	}
	md, ok := file(g, "~/.claude/CLAUDE.md")
	if !ok {
		t.Fatal("no CLAUDE.md")
	}
	for _, s := range []string{claudeMarker + "\n# Working in the mono-jig sandbox", "## GitLab", "agent/gl-user/",
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
	want := []string{"url.https://github.broker.internal/.insteadOf", "url.https://gitlab.broker.internal/.insteadOf"}
	if !slices.Equal(g.GitClear, want) {
		t.Errorf("GitClear %v, want %v", g.GitClear, want)
	}
}

// With no provider: the base environment only, nothing for the agent, and
// every provider's settings cleared.
func TestGuestConfigNone(t *testing.T) {
	var c config
	g := guestConfig(c)
	if len(g.Env) != 2 || len(g.Files) != 0 || len(g.Instructions) != 0 || len(g.GitClear) != 2 {
		t.Errorf("%+v", g)
	}
	want := []string{"ANTHROPIC_BASE_URL", "CLAUDE_CODE_OAUTH_TOKEN", "GH_ENTERPRISE_TOKEN", "GH_HOST", "GH_TOKEN",
		"GITLAB_API_PROTOCOL", "GITLAB_HOST", "GITLAB_TOKEN", "MCP_GATEWAY_URL"}
	if got := optionalEnv(c); !slices.Equal(got, want) {
		t.Errorf("optionalEnv %v, want %v", got, want)
	}
}
