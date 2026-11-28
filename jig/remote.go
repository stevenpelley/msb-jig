package jig

import (
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"

	"github.com/stevenpelley/msb-jig/editor"
)

// guestSSHD reports whether the track's editor needs OpenSSH's sshd in the
// guest (editor.GuestSSHD: Kiro).
func guestSSHD() bool {
	e, ok := track.Editor.(editor.GuestSSHD)
	return ok && e.GuestSSHD()
}

// sandboxKey is the sandbox's own SSH key, made the first time and
// authorized with msb ssh authorize, which adds it for all sandboxes
// (docs/microsandbox.md). With guestSSHD it's also the guest sshd's
// authorized key (sshdSetup).
func sandboxKey(name, state string) string {
	key := filepath.Join(state, "ssh_key")
	if _, err := os.Stat(key); err != nil {
		if err := os.MkdirAll(state, 0o700); err != nil {
			fail("%v", err)
		}
		if out, err := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-C", sshHost(name), "-f", key).CombinedOutput(); err != nil {
			fail("ssh-keygen: %v: %s", err, out)
		}
		if out, err := exec.Command("msb", "ssh", "authorize", "--file", key+".pub").CombinedOutput(); err != nil {
			fail("msb ssh authorize: %v: %s", err, out)
		}
	}
	return key
}

// sshFiles are a sandbox's SSH files on the Mac, under its state directory.
type sshFiles struct {
	key        string // ssh_key: ours, for both hops
	knownHosts string // known_hosts: msb's SSH server's key, accepted on first use
	sshdKnown  string // sshd_known_hosts: the guest sshd's key, pinned at each start (sshdSetup)
	msbConfig  string // msb_ssh_config: the hop through msb's SSH, with guestSSHD
}

func sshFilesFor(state string) sshFiles {
	return sshFiles{
		key:        filepath.Join(state, "ssh_key"),
		knownHosts: filepath.Join(state, "known_hosts"),
		sshdKnown:  filepath.Join(state, "sshd_known_hosts"),
		msbConfig:  filepath.Join(state, "msb_ssh_config"),
	}
}

// sshBlocks are a sandbox's Host blocks: the editor's (host), and with
// sshd the hop it goes through (hop; empty without). No agent or X11
// forwarding: the Mac's credentials stay on the Mac.
//
// Without sshd, host is msb's own SSH server (msb ssh serve). Its host key
// is new with each sandbox (rm removes known_hosts), so the first one is
// accepted; it reaches us over msb's own channel.
//
// With sshd, host is the guest's OpenSSH sshd on 127.0.0.1:22, reached by
// ssh -W through the hop (msb's SSH carries TCP forwards, not Unix-socket
// ones). Its host key is pinned (sshd_known_hosts, written from the guest
// at each start), so a mismatch is refused.
func sshBlocks(name string, f sshFiles, msb, sshBin string, sshd bool) (host, hop string) {
	msbBlock := func(alias, knownHosts string) string {
		return fmt.Sprintf(`Host %[1]s
  User %[2]s
  IdentityFile %[3]s
  IdentitiesOnly yes
  UserKnownHostsFile %[4]s
  StrictHostKeyChecking accept-new
  ProxyCommand %[5]s ssh serve %[6]s --stdio
  ForwardAgent no
  ForwardX11 no
  ClearAllForwardings no
`, alias, guestUser, f.key, knownHosts, msb, name)
	}
	if !sshd {
		return msbBlock(sshHost(name), f.knownHosts), ""
	}
	hopAlias := sshHost(name) + "-msb"
	host = fmt.Sprintf(`Host %[1]s
  User %[2]s
  IdentityFile %[3]s
  IdentitiesOnly yes
  UserKnownHostsFile %[4]s
  StrictHostKeyChecking yes
  ProxyCommand %[5]s -F %[6]s -W 127.0.0.1:22 %[7]s
  ForwardAgent no
  ForwardX11 no
  ClearAllForwardings no
`, sshHost(name), guestUser, f.key, f.sshdKnown, sshBin, f.msbConfig, hopAlias)
	return host, msbBlock(hopAlias, f.knownHosts)
}

// sshConfig writes the sandbox's Host block to <state>/ssh_config (and,
// with sshd, the hop to <state>/msb_ssh_config, which the editor's file
// doesn't include), and the file the editor reads (<root>/ssh_config) that
// includes every sandbox's, and returns the block.
func sshConfig(name, state string) string {
	sandboxKey(name, state)
	msb, err := exec.LookPath("msb")
	if err != nil {
		fail("msb not found")
	}
	sshBin, err := exec.LookPath("ssh")
	if err != nil {
		fail("ssh not found")
	}
	f := sshFilesFor(state)
	block, hop := sshBlocks(name, f, msb, sshBin, guestSSHD())
	if hop != "" {
		if err := os.WriteFile(f.msbConfig, []byte(hop), 0o600); err != nil {
			fail("%v", err)
		}
	}
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

// code opens the harness's own editor instance (its own settings and
// extensions, under <root>; for VS Code, the settings that close the
// credential bridges, editor.VSCodeSettings) on the sandbox, on dir
// (relative to the guest user's home) or the home itself, starting the
// sandbox first if it's stopped.
func code(name, state string, args []string) {
	ensureStarted(name, state)
	sshConfig(name, state)
	root := filepath.Dir(state)
	fail("%v", track.Editor.Open(root, editorProfile(root), sshHost(name), guestDir(args)))
}

// guestDir is the folder mono-jig <editor> opens: an absolute guest path
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
