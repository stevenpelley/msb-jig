package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// VSCodeSettings returns the user settings for the harness's own VS Code
// instance (code --user-data-dir <state>/vscode). They close the credential
// bridges listed in investigator-design.md, "The new trust boundary: the IDE
// connection". Settings marked unverified there need step 2's check.
//
// They are user settings, so they are defaults, not rules: a workspace's
// .vscode/settings.json, which the agent can write, overrides any of them
// (prototype-plan.md step 2). What they close is what a repo didn't ask for.
func VSCodeSettings(sshConfig string) map[string]any {
	return map[string]any{
		// SSH: our config file only, no agent forwarding, server fetched by the Mac.
		"remote.SSH.configFile":            sshConfig,
		"remote.SSH.enableAgentForwarding": false,
		"remote.SSH.localServerDownload":   "always",
		"remote.downloadExtensionsLocally": true, // checked 2026-11-22: the guest needs no marketplace
		// Claude Code's extension, installed on the remote at connect (checked 2026-11-22).
		"remote.SSH.defaultExtensions": []string{"anthropic.claude-code"},
		// Ports: nothing forwarded unless the user asks.
		"remote.autoForwardPorts":      false,
		"remote.restoreForwardedPorts": false,
		"remote.localPortHost":         "localhost",
		// Git: never let the editor answer git's credential prompts.
		"git.terminalAuthentication": false,
		"github.gitAuthentication":   false,
		"git.useIntegratedAskPass":   false,
		// Dev Containers: don't copy .gitconfig or forward credentials (names unverified).
		"dev.containers.copyGitConfig":                     false,
		"dev.containers.gitCredentialHelperConfigLocation": "none",
		// Remote terminals: no helper that opens URLs in the Mac's browser (checked 2026-11-22, VS Code 1.147).
		"terminal.integrated.env.linux": map[string]any{"BROWSER": nil},
		// Extensions: our short list; repos can't recommend new ones in.
		"extensions.allowed": map[string]any{
			"ms-vscode-remote.remote-ssh":      true,
			"ms-vscode-remote.remote-ssh-edit": true,
			"ms-vscode.remote-explorer":        true, // Remote-SSH depends on it
			"anthropic.claude-code":            true,
		},
		"extensions.ignoreRecommendations": true,
		"extensions.autoUpdate":            false,
		"telemetry.telemetryLevel":         "off",
		"workbench.startupEditor":          "none",
	}
}

// WriteVSCodeSettings writes settings.json into the harness instance's
// user-data directory.
func WriteVSCodeSettings(userDataDir, sshConfig string) (string, error) {
	dir := filepath.Join(userDataDir, "User")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	b, err := json.MarshalIndent(VSCodeSettings(sshConfig), "", "  ")
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, "settings.json")
	return path, os.WriteFile(path, append(b, '\n'), 0o600)
}
