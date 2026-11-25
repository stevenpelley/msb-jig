package editor

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Include lines are replaced by the files they name, in order; other
// lines stay.
func TestFlattenSSHConfig(t *testing.T) {
	root := t.TempDir()
	for name, block := range map[string]string{"b": "Host mono-jig-b\n  User u\n", "a": "Host mono-jig-a\n  User u"} {
		if err := os.MkdirAll(filepath.Join(root, name), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, name, "ssh_config"), []byte(block), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	top := filepath.Join(root, "ssh_config")
	if err := os.WriteFile(top, []byte("# top\nInclude "+filepath.Join(root, "*", "ssh_config")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := FlattenSSHConfig(top)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got, "Include") {
		t.Errorf("Include left:\n%s", got)
	}
	a, b := strings.Index(got, "Host mono-jig-a\n  User u\n"), strings.Index(got, "Host mono-jig-b")
	if !strings.HasPrefix(got, "# top\n") || a < 0 || b < a {
		t.Errorf("flattened:\n%s", got)
	}
	if err := os.WriteFile(top, []byte("Include relative/*\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := FlattenSSHConfig(top); err == nil {
		t.Error("relative Include accepted")
	}
}

// Configure points kiroRemote.sshConfigFile at the flattened copy and
// turns off the credential bridges Kiro has settings for; with no
// sandboxes yet it still writes both files.
func TestKiroConfigure(t *testing.T) {
	root := t.TempDir()
	k := Kiro{}
	if err := k.Configure(root, Profile{SSHConfig: filepath.Join(root, "ssh_config")}); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(root, "_kiro", "User", "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	var s map[string]any
	if err := json.Unmarshal(b, &s); err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(root, "_kiro", "ssh_config")
	if s["kiroRemote.sshConfigFile"] != cfg {
		t.Errorf("sshConfigFile %v", s["kiroRemote.sshConfigFile"])
	}
	for _, off := range []string{"git.terminalAuthentication", "github.gitAuthentication", "git.useIntegratedAskPass", "remote.autoForwardPorts"} {
		if s[off] != false {
			t.Errorf("%s = %v", off, s[off])
		}
	}
	if _, err := os.Stat(cfg); err != nil {
		t.Error(err)
	}
}

// The authority kiro-remote builds for a configured host (Kiro 1.5.11).
func TestKiroAuthority(t *testing.T) {
	if got := KiroAuthority("mono-jig-work"); got != "kiro-ssh+config-6d6f6e6f2d6a69672d776f726b" {
		t.Errorf("got %s", got)
	}
}
