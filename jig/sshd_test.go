package jig

import (
	"strings"
	"testing"

	"github.com/stevenpelley/msb-jig/agent"
	"github.com/stevenpelley/msb-jig/editor"
	"github.com/stevenpelley/msb-jig/sandbox"
)

const testPub = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIFakeKeyForTestsOnly0123456789abcdefghijklm"

// Only an ed25519 key's type and key go into the guest's script; its
// comment, and anything that isn't a key, don't.
func TestAuthorizedKey(t *testing.T) {
	k, err := authorizedKey(testPub + " mono-jig-work\n")
	if err != nil || k != testPub {
		t.Errorf("%q, %v", k, err)
	}
	for _, bad := range []string{"", "ssh-rsa AAAA", "ssh-ed25519 AAAA'; rm -rf /", "ssh-ed25519"} {
		if _, err := authorizedKey(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

// The script keeps sshd on loopback, allows only local Unix-socket
// forwards, and installs the key for the guest user.
func TestSSHDScript(t *testing.T) {
	s := sshdScript(testPub)
	for _, want := range []string{"ListenAddress 127.0.0.1", "AllowStreamLocalForwarding local", "'" + testPub + "'", "sshd -t", "exit 3"} {
		if !strings.Contains(s, want) {
			t.Errorf("script lacks %q:\n%s", want, s)
		}
	}
	if strings.Contains(s, "%!") {
		t.Errorf("format error:\n%s", s)
	}
}

func TestKnownHostsLine(t *testing.T) {
	l, err := knownHostsLine("mono-jig-work", testPub+" root@guest\n")
	if err != nil || l != "mono-jig-work "+testPub+"\n" {
		t.Errorf("%q, %v", l, err)
	}
}

// Without sshd the editor's host is msb's SSH server; with it, the guest's
// sshd through a hop that the editor's config doesn't name, with the host
// key pinned.
func TestSSHBlocks(t *testing.T) {
	f := sshFilesFor("/state/work")
	host, hop := sshBlocks("work", f, "/bin/msb", "/usr/bin/ssh", false)
	if hop != "" || !strings.Contains(host, "ProxyCommand /bin/msb ssh serve work --stdio") || !strings.Contains(host, "StrictHostKeyChecking accept-new") {
		t.Errorf("without sshd:\n%s\nhop:\n%s", host, hop)
	}
	host, hop = sshBlocks("work", f, "/bin/msb", "/usr/bin/ssh", true)
	for _, want := range []string{
		"Host mono-jig-work\n",
		"ProxyCommand /usr/bin/ssh -F /state/work/msb_ssh_config -W 127.0.0.1:22 mono-jig-work-msb",
		"UserKnownHostsFile /state/work/sshd_known_hosts",
		"StrictHostKeyChecking yes",
		"ForwardAgent no",
	} {
		if !strings.Contains(host, want) {
			t.Errorf("host lacks %q:\n%s", want, host)
		}
	}
	for _, want := range []string{"Host mono-jig-work-msb\n", "ProxyCommand /bin/msb ssh serve work --stdio", "UserKnownHostsFile /state/work/known_hosts"} {
		if !strings.Contains(hop, want) {
			t.Errorf("hop lacks %q:\n%s", want, hop)
		}
	}
}

// Kiro brings sshd into the image and every sandbox; VS Code doesn't.
func TestSSHDComponents(t *testing.T) {
	saved := track
	defer func() { track = saved }()
	track = Track{Agent: agent.KiroCLI{}, Editor: editor.Kiro{}, Backend: sandbox.Microsandbox{}}.withDefaults()
	if !guestSSHD() || !strings.Contains(imageComponents(), "sshd") || !strings.Contains(components(config{}), "sshd") {
		t.Errorf("Kiro: image %q, sandbox %q", imageComponents(), components(config{}))
	}
	track = Track{Agent: agent.KiroCLI{}, Editor: editor.VSCode{}, Backend: sandbox.Microsandbox{}}.withDefaults()
	if guestSSHD() || strings.Contains(imageComponents(), "sshd") || strings.Contains(components(config{}), "sshd") {
		t.Errorf("VS Code: image %q, sandbox %q", imageComponents(), components(config{}))
	}
}
