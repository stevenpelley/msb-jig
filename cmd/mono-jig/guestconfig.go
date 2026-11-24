package main

import (
	"maps"
	"net/url"
	"slices"
	"strings"

	"mono-jig/broker/awsmcp"
	"mono-jig/broker/providers"
	"mono-jig/guest"
)

// The guest's settings for this sandbox's providers, broker mode and
// agent (guest.Config): the sandbox's environment at create and every
// start (openSandbox), and what provision.sh's apply component writes at
// every start. One function per provider and one for the agent, so each
// can move to its own module (docs/library-split.md).

// guestConfig is the guest's settings for c.
func guestConfig(c config) guest.Config {
	g := guest.Config{Env: map[string]string{
		"GIT_TERMINAL_PROMPT": "0",
		// Node-based tools (Claude Code's MCP client) don't read the system store.
		"NODE_EXTRA_CA_CERTS": "/etc/ssl/certs/ca-certificates.crt",
	}}
	for _, p := range []func(config) guest.Config{gitlabGuest, githubGuest, awsGuest} {
		g.Add(p(c))
	}
	if len(g.Instructions) > 0 {
		g.Instructions = slices.Insert(g.Instructions, 0, guest.Instructions("header", nil))
	}
	g.Add(claudeCode(c, g.Instructions))
	// The git keys any provider would set, in either mode, so one no
	// longer served leaves nothing behind.
	for _, k := range everything(c).GitKeys() {
		if _, ok := g.Git[k]; !ok {
			g.GitClear = append(g.GitClear, k)
		}
	}
	return g
}

// everything is the union of guestConfig's settings with every provider
// on, in both broker modes: what a provider may have set before.
func everything(c config) guest.Config {
	var all guest.Config
	c.broker.GitLabUser, c.broker.GitHubUser, c.broker.AnthropicTokenCmd = "u", "u", "true"
	c.broker.AWSProfiles = []string{"p"}
	if c.broker.GitLabURL == "" {
		c.broker.GitLabURL = "https://gitlab.com"
	}
	for _, proxy := range []bool{false, true} {
		c.proxy = proxy
		for _, p := range []func(config) guest.Config{gitlabGuest, githubGuest, awsGuest} {
			all.Add(p(c))
		}
		all.Add(claudeCode(c, []string{"x"}))
	}
	return all
}

// optionalEnv are the variables a provider or the agent may set: those
// guestConfig doesn't are removed from a sandbox's environment.
func optionalEnv(c config) []string {
	return slices.Sorted(maps.Keys(everything(c).Env))
}

// insteadOf points git's URLs for host at the broker's URL.
func insteadOf(broker, host string) map[string][]string {
	return map[string][]string{"url." + broker + "/.insteadOf": {
		"https://" + host + "/", "git@" + host + ":", "ssh://git@" + host + "/", "http://" + host + "/",
	}}
}

// gitlabGuest: glab and git call the broker as a self-hosted GitLab (in
// proxy mode, GitLab's own host, which the broker terminates). The token
// is a placeholder; the broker injects the user's.
func gitlabGuest(c config) guest.Config {
	if c.broker.GitLabUser == "" {
		return guest.Config{}
	}
	host := gitlabHost(c)
	real := host
	if u, err := url.Parse(c.broker.GitLabURL); err == nil && u.Host != "" {
		real = u.Host
	}
	g := guest.Config{
		Env: map[string]string{"GITLAB_HOST": host, "GITLAB_TOKEN": "placeholder", "GITLAB_API_PROTOCOL": "https"},
		// glab's own config too: auth status and clone read it, not
		// GITLAB_HOST. Only this host: glab checks every host listed (its
		// default gitlab.com fails with the placeholder), and https, since
		// glab repo clone would otherwise use SSH, which the sandbox lacks.
		Files: []guest.File{{
			Path: "~/.config/glab-cli/config.yml", Owner: "user", Mode: "0600", Marker: glabMarker,
			Content: glabMarker + "\nhost: " + host + "\ngit_protocol: https\ncheck_update: false\nhosts:\n    " +
				host + ":\n        api_protocol: https\n        git_protocol: https\n",
		}},
		Instructions: []string{guest.Instructions("gitlab", map[string]string{"User": c.broker.GitLabUser, "Host": real})},
	}
	if !c.proxy {
		// A URL rewritten to a name the broker has no certificate for
		// fails its TLS handshake, so only for a provider it serves.
		g.Git = insteadOf("https://"+host, real)
	}
	return g
}

// gitlabHost is the GitLab host the guest calls: the broker's name, or in
// proxy mode GitLab's own.
func gitlabHost(c config) string {
	if !c.proxy {
		return providers.GitLabHost
	}
	u, _ := url.Parse(c.broker.GitLabURL)
	return u.Hostname()
}

// glabMarker is the first line of glab's config, as provision.sh wrote it
// before guest.Config, so those files stay ours.
const glabMarker = "# Written by mono-jig provision.sh; rewritten on every start."

// githubGuest: gh and git call the broker as a GitHub Enterprise Server
// (in proxy mode, github.com, which the broker terminates).
func githubGuest(c config) guest.Config {
	if c.broker.GitHubUser == "" {
		return guest.Config{}
	}
	g := guest.Config{Instructions: []string{guest.Instructions("github", map[string]string{"User": c.broker.GitHubUser})}}
	if c.proxy {
		g.Env = map[string]string{"GH_TOKEN": "placeholder"}
		return g
	}
	g.Env = map[string]string{"GH_HOST": providers.GitHubHost, "GH_ENTERPRISE_TOKEN": "placeholder"}
	g.Git = insteadOf("https://"+providers.GitHubHost, providers.GitHubWebHost)
	return g
}

// awsURL is the broker's AWS MCP endpoint, for mcpcli and curl.
const awsURL = "https://" + providers.AWSHost + awsmcp.Path

// mcpcliDir is where mcpcli goes in the guest.
const mcpcliDir = "/usr/local/lib/mono-jig/mcpcli"

// awsGuest: mcpcli, the agent's client for the broker's AWS MCP
// endpoint. The agent calls the AWS MCP Server from the shell (mcpcli or
// curl), never as native MCP tools, so it chooses what enters its context.
func awsGuest(c config) guest.Config {
	if !c.broker.AWSOn() {
		return guest.Config{}
	}
	p := c.broker.AWSProfiles
	g := guest.Config{
		Env:      map[string]string{"MCP_GATEWAY_URL": awsURL},
		Packages: []string{"python3", "python3-venv"},
		Files:    []guest.File{{Path: "/usr/local/bin/mcpcli", Link: mcpcliDir + "/mcpcli"}},
		Instructions: []string{guest.Instructions("aws", map[string]string{
			"URL": awsURL, "Profile": p[0], "Others": strings.Join(p[1:], " "),
		})},
	}
	for _, name := range slices.Sorted(maps.Keys(guest.MCPCLI)) {
		g.Files = append(g.Files, guest.File{Path: mcpcliDir + "/" + name, Mode: "0755", Content: guest.MCPCLI[name]})
	}
	return g
}

// claudeMarker is the first line of the agent's instructions, as
// provision.sh wrote it before guest.Config.
const claudeMarker = "<!-- Written by mono-jig provision.sh; edits are lost on reprovisioning. -->"

// claudeCode is the agent's part: Claude Code calls the broker for
// Anthropic with a placeholder token (the credential stays on the Mac),
// and reads the instructions from ~/.claude/CLAUDE.md.
func claudeCode(c config, instructions []string) guest.Config {
	var g guest.Config
	if c.broker.AnthropicTokenCmd != "" {
		g.Env = map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": "placeholder"}
		if !c.proxy {
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
		g.Files = append(g.Files, guest.File{Path: "~/.claude/CLAUDE.md", Owner: "user", Marker: claudeMarker,
			Content: claudeMarker + "\n" + strings.Join(instructions, "")})
	}
	return g
}
