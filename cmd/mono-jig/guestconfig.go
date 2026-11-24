package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"

	"mono-jig/guest"
)

// The guest's settings for this sandbox's providers, broker mode and
// agent (guest.Config): the sandbox's environment at create and every
// start (openSandbox), and what provision.sh's apply component writes at
// every start. The providers' parts are theirs (providers.Provider.Guest),
// the agent's its own (agent.Agent.Guest).

// guestConfig is the guest's settings for c.
func guestConfig(c config) guest.Config {
	g := guest.Config{
		Env: map[string]string{
			"GIT_TERMINAL_PROMPT": "0",
			// The system store, with the broker's CA and the extra ones:
			// Node-based tools (Claude Code's MCP client) and Python's
			// requests (certifi) don't read it by default.
			"NODE_EXTRA_CA_CERTS": "/etc/ssl/certs/ca-certificates.crt",
			"SSL_CERT_FILE":       "/etc/ssl/certs/ca-certificates.crt",
			"REQUESTS_CA_BUNDLE":  "/etc/ssl/certs/ca-certificates.crt",
		},
		// What provision.sh set before apply recorded its git keys.
		GitClear: legacyGitKeys,
	}
	g.Add(c.broker.Guest())
	if len(g.Instructions) > 0 {
		g.Instructions = slices.Insert(g.Instructions, 0, guest.Instructions("header", nil))
	}
	g.Add(theAgent.Guest(c.broker, g.Instructions))
	return g
}

// legacyGitKeys and legacyEnv are what sandboxes provisioned before the
// guest's settings were recorded may have: cleared when not set.
var (
	legacyGitKeys = []string{"url.https://github.broker.internal/.insteadOf", "url.https://gitlab.broker.internal/.insteadOf"}
	legacyEnv     = []string{"ANTHROPIC_BASE_URL", "CLAUDE_CODE_OAUTH_TOKEN", "GH_ENTERPRISE_TOKEN", "GH_HOST", "GH_TOKEN",
		"GITLAB_API_PROTOCOL", "GITLAB_HOST", "GITLAB_TOKEN", "MCP_GATEWAY_URL"}
)

// envFile records the sandbox environment's names, so those no longer set
// are removed at the next start.
func envFile(state string) string { return filepath.Join(state, "guest-env") }

// staleEnv are the names the sandbox had (recorded, or legacyEnv) that
// env doesn't set.
func staleEnv(state string, env map[string]string) []string {
	had := slices.Clone(legacyEnv)
	if b, err := os.ReadFile(envFile(state)); err == nil {
		had = append(had, strings.Fields(string(b))...)
	}
	var gone []string
	for _, k := range slices.Compact(slices.Sorted(slices.Values(had))) {
		if _, ok := env[k]; !ok {
			gone = append(gone, k)
		}
	}
	return gone
}

// recordEnv writes env's names to envFile.
func recordEnv(state string, env map[string]string) error {
	var names []string
	for k := range env {
		names = append(names, k)
	}
	slices.Sort(names)
	return os.WriteFile(envFile(state), []byte(strings.Join(names, "\n")+"\n"), 0o600)
}
