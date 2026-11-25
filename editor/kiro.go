package editor

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
)

// Kiro is Kiro IDE (1.5 or later) with its own Remote SSH (the built-in
// kiro-remote extension, remote authority kiro-ssh), as its own instance:
// user data in <root>/_kiro, extensions in <root>/_kiro-ext. Kiro installs
// its server and agent on the remote itself, downloading them from the
// guest (no Mac-side download, unlike VS Code's localServerDownload), so
// Install has nothing to do.
//
// Read from Kiro 1.5.11's bundle (2026-11-25): kiro --help lists
// --user-data-dir and --extensions-dir; kiro-remote contributes
// kiroRemote.sshConfigFile, kiroRemote.httpProxy, kiroRemote.httpsProxy and
// kiroRemote.serverDownloadUrlTemplate, and resolves kiro-ssh. --remote
// (VS Code's flag, not in --help) with KiroAuthority reaches kiro-remote
// (checked 2026-11-25: the trust prompt for the host appears).
type Kiro struct{}

// KiroAuthority is the remote authority of a host in the SSH config:
// kiro-ssh+config-<the alias in hex>, as kiro-remote builds it for Connect
// to Host. It refuses any other spelling as noncanonical. A bare
// kiro-ssh+<hex> is a destination typed in by hand. Read from Kiro
// 1.5.11's extension.js, 2026-11-25.
func KiroAuthority(alias string) string {
	return "kiro-ssh+config-" + hex.EncodeToString([]byte(alias))
}

func (Kiro) Name() string { return "kiro" }

func (Kiro) Find() (string, error) {
	bin, err := exec.LookPath("kiro")
	if err != nil {
		return "", errors.New("Kiro IDE's kiro command isn't on PATH (Kiro: Command Palette, Shell Command: Install 'kiro' command in PATH)")
	}
	return bin, nil
}

func (Kiro) dirs(root string) (data, ext string) {
	return filepath.Join(root, "_kiro"), filepath.Join(root, "_kiro-ext")
}

// KiroSettings are the user settings of the harness's own Kiro instance:
// VS Code's that Kiro 1.5.11 has (the git and github extensions' and the
// core remote, terminal and extension settings), and kiro-remote's SSH
// config file. Kiro has no Remote-SSH agent-forwarding setting; the SSH
// config's ForwardAgent no and the guest's sshd refuse it. As with VS
// Code's, they are defaults that a workspace's settings override.
func KiroSettings(sshConfig string) map[string]any {
	return map[string]any{
		"kiroRemote.sshConfigFile": sshConfig,
		// Ports: nothing forwarded unless the user asks (core settings, unverified in Kiro).
		"remote.autoForwardPorts":      false,
		"remote.restoreForwardedPorts": false,
		"remote.localPortHost":         "localhost",
		// Git: never let the editor answer git's credential prompts.
		"git.terminalAuthentication": false,
		"github.gitAuthentication":   false,
		"git.useIntegratedAskPass":   false,
		// Remote terminals: no helper that opens URLs in the Mac's browser.
		"terminal.integrated.env.linux":    map[string]any{"BROWSER": nil},
		"extensions.ignoreRecommendations": true,
		"extensions.autoUpdate":            false,
		"telemetry.telemetryLevel":         "off",
	}
}

// FlattenSSHConfig returns the SSH config at path with its Include lines
// replaced by the files they name (absolute globs, one level, in sorted
// order). Kiro reads SSH configs with its own parser, which doesn't
// support everything OpenSSH does; whether it follows Include globs is
// unverified, so it gets one file.
func FlattenSSHConfig(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	var out strings.Builder
	for _, line := range strings.SplitAfter(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) < 2 || !strings.EqualFold(f[0], "Include") {
			out.WriteString(line)
			continue
		}
		for _, pat := range f[1:] {
			if !filepath.IsAbs(pat) {
				return "", fmt.Errorf("%s: Include %s: not an absolute path", path, pat)
			}
			files, err := filepath.Glob(pat)
			if err != nil {
				return "", err
			}
			sort.Strings(files)
			for _, inc := range files {
				ib, err := os.ReadFile(inc)
				if err != nil {
					return "", err
				}
				fmt.Fprintf(&out, "# from %s\n%s", inc, ib)
				if len(ib) > 0 && ib[len(ib)-1] != '\n' {
					out.WriteString("\n")
				}
			}
		}
	}
	return out.String(), nil
}

// Configure writes the flattened SSH config to <root>/_kiro/ssh_config
// and the settings to <root>/_kiro/User/settings.json.
func (k Kiro) Configure(root string, p Profile) error {
	data, _ := k.dirs(root)
	if err := os.MkdirAll(filepath.Join(data, "User"), 0o700); err != nil {
		return err
	}
	cfg := filepath.Join(data, "ssh_config")
	flat := "# Written by mono-jig: no sandboxes yet.\n"
	if _, err := os.Stat(p.SSHConfig); err == nil {
		if flat, err = FlattenSSHConfig(p.SSHConfig); err != nil {
			return err
		}
	}
	if err := os.WriteFile(cfg, []byte(flat), 0o600); err != nil {
		return err
	}
	b, err := json.MarshalIndent(KiroSettings(cfg), "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(data, "User", "settings.json"), append(b, '\n'), 0o600)
}

func (Kiro) Install(string) error { return nil }

func (k Kiro) Open(root string, p Profile, host, dir string) error {
	if err := k.Configure(root, p); err != nil {
		return err
	}
	bin, err := k.Find()
	if err != nil {
		return err
	}
	data, ext := k.dirs(root)
	argv := []string{"kiro", "--user-data-dir", data, "--extensions-dir", ext, "--remote", KiroAuthority(host), dir}
	fmt.Println(strings.Join(argv, " "))
	return syscall.Exec(bin, argv, os.Environ())
}
