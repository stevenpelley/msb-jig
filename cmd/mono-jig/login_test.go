package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// login's settings configure the broker; the JIG_* variables override them.
func TestLoadConfigSettings(t *testing.T) {
	t.Setenv("JIG_STATE", t.TempDir())
	t.Setenv("JIG_CLAUDE_TOKEN_COMMAND", "")
	for _, k := range []string{"JIG_GITLAB_USER", "JIG_GITLAB_URL", "JIG_GITLAB_TOKEN_COMMAND", "JIG_AWS_PROFILES"} {
		os.Unsetenv(k)
	}
	c := loadConfig("t", "")
	if c.broker.GitLabUser != "" || len(c.broker.AWSProfiles) != 0 {
		t.Fatalf("no settings: %+v", c.broker)
	}

	saveSettings(settings{
		GitLab: &gitlabSettings{User: "alice", URL: "https://gitlab.example.com", Token: "keychain"},
		AWS:    &awsSettings{Profiles: []string{"a", "b"}},
	})
	c = loadConfig("t", "")
	if c.broker.GitLabUser != "alice" || c.broker.GitLabURL != "https://gitlab.example.com" ||
		!strings.Contains(c.broker.GitLabTokenCmd, keychainGitLab) || !slices.Equal(c.broker.AWSProfiles, []string{"a", "b"}) {
		t.Fatalf("settings: %+v", c.broker)
	}

	saveSettings(settings{GitLab: &gitlabSettings{User: "alice", URL: "https://gitlab.com", Token: "glab"}})
	if c = loadConfig("t", ""); !strings.Contains(c.broker.GitLabTokenCmd, "glab config get token --host gitlab.com") {
		t.Fatalf("glab: %q", c.broker.GitLabTokenCmd)
	}

	t.Setenv("JIG_GITLAB_USER", "")
	t.Setenv("JIG_AWS_PROFILES", "c")
	if c = loadConfig("t", ""); c.broker.GitLabUser != "" || !slices.Equal(c.broker.AWSProfiles, []string{"c"}) {
		t.Fatalf("env over settings: %+v", c.broker)
	}
	if fi, err := os.Stat(settingsPath()); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("settings file: %v %v", fi, err)
	}
}

func TestPruneAuthorizedKeys(t *testing.T) {
	t.Setenv("MSB_HOME", t.TempDir())
	p := authorizedKeys()
	if err := pruneAuthorizedKeys(func(string) bool { return true }); err != nil {
		t.Fatalf("no file: %v", err)
	}
	os.MkdirAll(filepath.Dir(p), 0o700)
	os.WriteFile(p, []byte("ssh-ed25519 AAA mono-jig-a\nssh-ed25519 BBB me@mac\nssh-ed25519 CCC mono-jig-b\n"), 0o600)
	if err := pruneAuthorizedKeys(func(c string) bool { return c == sshHost("a") }); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(p); string(b) != "ssh-ed25519 BBB me@mac\nssh-ed25519 CCC mono-jig-b\n" {
		t.Fatalf("one: %q", b)
	}
	pruneAuthorizedKeys(func(c string) bool { return strings.HasPrefix(c, "mono-jig-") })
	if b, _ := os.ReadFile(p); string(b) != "ssh-ed25519 BBB me@mac\n" {
		t.Fatalf("all: %q", b)
	}
}

func TestSandboxNames(t *testing.T) {
	root := t.TempDir()
	for _, d := range []string{"inv1", "_vscode", "_vscode-ext", "Bad"} {
		os.Mkdir(filepath.Join(root, d), 0o700)
	}
	os.WriteFile(filepath.Join(root, "settings.json"), nil, 0o600)
	if got := sandboxNames(root); !slices.Equal(got, []string{"inv1"}) {
		t.Fatalf("%v", got)
	}
}
