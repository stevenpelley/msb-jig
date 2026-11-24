package main

import (
	"fmt"
	"mono-jig/editor"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"syscall"
)

// sshConfig writes the sandbox's Host block to <state>/ssh_config, and the
// file VS Code reads (<root>/ssh_config) that includes every sandbox's, and
// returns the block. The key is the sandbox's own; msb ssh authorize adds
// it for all sandboxes (docs/microsandbox.md).
func sshConfig(name, state string) string {
	key := filepath.Join(state, "ssh_key")
	if _, err := os.Stat(key); err != nil {
		if out, err := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-C", "mono-jig-"+name, "-f", key).CombinedOutput(); err != nil {
			fail("ssh-keygen: %v: %s", err, out)
		}
		if out, err := exec.Command("msb", "ssh", "authorize", "--file", key+".pub").CombinedOutput(); err != nil {
			fail("msb ssh authorize: %v: %s", err, out)
		}
	}
	msb, err := exec.LookPath("msb")
	if err != nil {
		fail("msb not found")
	}
	// No agent or X11 forwarding: the Mac's credentials stay on the Mac.
	// The host key is new with each sandbox (rm removes known_hosts), so
	// the first one is accepted; it reaches us over msb's own channel.
	block := fmt.Sprintf(`Host %[1]s
  User %[2]s
  IdentityFile %[3]s
  IdentitiesOnly yes
  UserKnownHostsFile %[4]s
  StrictHostKeyChecking accept-new
  ProxyCommand %[5]s ssh serve %[6]s --stdio
  ForwardAgent no
  ForwardX11 no
  ClearAllForwardings no
`, sshHost(name), guestUser, key, filepath.Join(state, "known_hosts"), msb, name)
	if err := os.WriteFile(filepath.Join(state, "ssh_config"), []byte(block), 0o600); err != nil {
		fail("%v", err)
	}
	root := filepath.Dir(state)
	all := fmt.Sprintf("# Written by mono-jig: every sandbox's Host block.\nInclude %s\n", filepath.Join(root, "*", "ssh_config"))
	if err := os.WriteFile(filepath.Join(root, "ssh_config"), []byte(all), 0o600); err != nil {
		fail("%v", err)
	}
	return block
}

func sshHost(name string) string { return "mono-jig-" + name }

// sshHosts are the host names of every sandbox under root.
func sshHosts(root string) []string {
	var hs []string
	for _, n := range sandboxNames(root) {
		hs = append(hs, sshHost(n))
	}
	return hs
}

// code opens the harness's own VS Code instance (its own user data and
// extensions, under <root>/_vscode) on the sandbox, with the settings
// that close the credential bridges (editor.VSCodeSettings), on dir (relative
// to the guest user's home) or the home itself.
func code(name, state string, args []string) {
	if pidOf(state) == 0 {
		fail("%s isn't up: mono-jig up %s", name, name)
	}
	sshConfig(name, state)
	root := filepath.Dir(state)
	data, ext := filepath.Join(root, "_vscode"), filepath.Join(root, "_vscode-ext")
	if _, err := editor.WriteVSCodeSettings(data, filepath.Join(root, "ssh_config"), sshHosts(root)); err != nil {
		fail("%v", err)
	}
	bin, err := exec.LookPath("code")
	if err != nil {
		fail("VS Code's code command isn't on PATH (VS Code: Shell Command: Install 'code' command in PATH)")
	}
	argv := []string{"code", "--user-data-dir", data, "--extensions-dir", ext,
		"--remote", "ssh-remote+" + sshHost(name), guestDir(args)}
	fmt.Println(strings.Join(argv, " "))
	fail("%v", syscall.Exec(bin, argv, os.Environ()))
}

// guestDir is the folder mono-jig code opens: an absolute guest path
// as given, anything else under the guest user's home.
func guestDir(args []string) string {
	home := "/home/" + guestUser
	if len(args) == 0 || args[0] == "" {
		return home
	}
	if path.IsAbs(args[0]) {
		return path.Clean(args[0])
	}
	return path.Join(home, args[0])
}
