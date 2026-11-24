package editor

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
)

// VSCodeSettings returns the user settings for the harness's own VS Code
// instance (code --user-data-dir <state>/vscode). They close the credential
// bridges listed in docs/design.md, "The new trust boundary: the IDE
// connection". Settings marked unverified there need step 2's check.
//
// They are user settings, so they are defaults, not rules: a workspace's
// .vscode/settings.json, which the agent can write, overrides any of them
// (docs/prototype-plan.md step 2). What they close is what a repo didn't ask for.
//
// hosts are the sandboxes' SSH host names: all Linux, so Remote-SSH
// doesn't ask. agentExts are the agent's extensions, installed on the
// remote at connect and allowed.
func VSCodeSettings(sshConfig string, hosts, agentExts []string) map[string]any {
	platform := map[string]string{}
	for _, h := range hosts {
		platform[h] = "linux"
	}
	allowed := map[string]any{
		"ms-vscode-remote.remote-ssh":      true,
		"ms-vscode-remote.remote-ssh-edit": true,
		"ms-vscode.remote-explorer":        true, // Remote-SSH depends on it
	}
	for _, e := range agentExts {
		allowed[e] = true
	}
	return map[string]any{
		// SSH: our config file only, no agent forwarding, server fetched by the Mac.
		"remote.SSH.configFile":            sshConfig,
		"remote.SSH.remotePlatform":        platform,
		"remote.SSH.enableAgentForwarding": false,
		"remote.SSH.localServerDownload":   "always",
		"remote.downloadExtensionsLocally": true, // checked 2026-11-22: the guest needs no marketplace
		// The agent's extensions, installed on the remote at connect
		// (checked 2026-11-22 with Claude Code's).
		"remote.SSH.defaultExtensions": append([]string{}, agentExts...),
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
		"extensions.allowed":               allowed,
		"extensions.ignoreRecommendations": true,
		"extensions.autoUpdate":            false,
		"telemetry.telemetryLevel":         "off",
		// The start page, for its Clone Git Repository and Open Folder.
		"workbench.startupEditor": "welcomePage",
	}
}

// WriteVSCodeSettings writes settings.json into the harness instance's
// user-data directory.
func WriteVSCodeSettings(userDataDir, sshConfig string, hosts, agentExts []string) (string, error) {
	dir := filepath.Join(userDataDir, "User")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	b, err := json.MarshalIndent(VSCodeSettings(sshConfig, hosts, agentExts), "", "  ")
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, "settings.json")
	return path, os.WriteFile(path, append(b, '\n'), 0o600)
}

// VSCode is VS Code with Microsoft's Remote-SSH, as its own instance:
// user data in <root>/_vscode, extensions in <root>/_vscode-ext (only
// Remote-SSH; the agent's go on the remote).
type VSCode struct{}

func (VSCode) Name() string { return "vscode" }

func (VSCode) Find() (string, error) {
	bin, err := exec.LookPath("code")
	if err != nil {
		return "", errors.New("VS Code's code command isn't on PATH (VS Code: Shell Command: Install 'code' command in PATH)")
	}
	return bin, nil
}

func (VSCode) dirs(root string) (data, ext string) {
	return filepath.Join(root, "_vscode"), filepath.Join(root, "_vscode-ext")
}

func (v VSCode) Configure(root string, p Profile) error {
	data, _ := v.dirs(root)
	_, err := WriteVSCodeSettings(data, p.SSHConfig, p.Hosts, p.Extensions)
	return err
}

func (v VSCode) Install(root string) error {
	bin, err := v.Find()
	if err != nil {
		return err
	}
	data, ext := v.dirs(root)
	c := exec.Command(bin, "--user-data-dir", data, "--extensions-dir", ext, "--install-extension", "ms-vscode-remote.remote-ssh")
	c.Stdout, c.Stderr = os.Stdout, os.Stderr
	return c.Run()
}

func (v VSCode) Open(root string, p Profile, host, dir string) error {
	if err := v.Configure(root, p); err != nil {
		return err
	}
	bin, err := v.Find()
	if err != nil {
		return err
	}
	data, ext := v.dirs(root)
	argv := []string{"code", "--user-data-dir", data, "--extensions-dir", ext, "--remote", "ssh-remote+" + host, dir}
	fmt.Println(strings.Join(argv, " "))
	return syscall.Exec(bin, argv, os.Environ())
}
