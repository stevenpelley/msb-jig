package jig

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stevenpelley/msb-jig/broker/providers"
)

// login's Settings configure the broker; the JIG_* variables override them.
func TestLoadConfigSettings(t *testing.T) {
	t.Setenv("JIG_STATE", t.TempDir())
	t.Setenv("JIG_CLAUDE_TOKEN_COMMAND", "")
	for _, k := range []string{"JIG_GITLAB_USER", "JIG_GITLAB_URL", "JIG_GITLAB_TOKEN_COMMAND", "JIG_AWS_PROFILES",
		"JIG_AWS_MCP_COMMAND", "JIG_GITHUB_USER", "JIG_GITHUB_HOST", "JIG_GITHUB_TOKEN_COMMAND"} {
		os.Unsetenv(k)
	}
	gitlab := func(c config) providers.GitLab { p, _ := c.broker.Get("gitlab").(providers.GitLab); return p }
	aws := func(c config) providers.AWS { p, _ := c.broker.Get("aws").(providers.AWS); return p }
	c := loadConfig("t", "")
	if len(c.broker.Providers) != 0 {
		t.Fatalf("no settings: %+v", c.broker)
	}

	var s Settings
	s.SetProvider("gitlab", gitlabSettings{User: "alice", URL: "https://gitlab.example.com", Token: "keychain"})
	s.SetProvider("aws", awsSettings{Profiles: []string{"a", "b"}})
	s.SetProvider("github", githubSettings{User: "bob", Host: "octo.ghe.com", Token: "gh"})
	s.Image = "img"
	SaveSettings(s)
	c = loadConfig("t", "")
	if g := gitlab(c); g.User != "alice" || g.URL != "https://gitlab.example.com" ||
		!strings.Contains(g.TokenCmd, keychainGitLab) || !slices.Equal(aws(c).Profiles, []string{"a", "b"}) {
		t.Fatalf("settings: %+v", c.broker)
	}
	if g, _ := c.broker.Get("github").(providers.GitHub); g.Web() != "octo.ghe.com" || g.API() != "api.octo.ghe.com" ||
		g.TokenCmd != "gh auth token --hostname octo.ghe.com" {
		t.Fatalf("github: %+v", g)
	}
	if got := LoadSettings(); got.Image != "img" {
		t.Fatalf("image: %q", got.Image)
	}

	s = Settings{}
	s.SetProvider("gitlab", gitlabSettings{User: "alice", URL: "https://gitlab.com", Token: "glab"})
	SaveSettings(s)
	if c = loadConfig("t", ""); !strings.Contains(gitlab(c).TokenCmd, "glab config get token --host gitlab.com") {
		t.Fatalf("glab: %q", gitlab(c).TokenCmd)
	}

	t.Setenv("JIG_GITLAB_USER", "")
	t.Setenv("JIG_AWS_PROFILES", "c")
	if c = loadConfig("t", ""); c.broker.Get("gitlab") != nil || !slices.Equal(aws(c).Profiles, []string{"c"}) {
		t.Fatalf("env over settings: %+v", c.broker)
	}
	if fi, err := os.Stat(settingsPath()); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("settings file: %v %v", fi, err)
	}
}

// settings.json keeps its shape: each provider under its name, and image.
func TestSettingsJSON(t *testing.T) {
	in := `{"aws":{"profiles":["p"]},"gitlab":{"user":"u","url":"https://gitlab.com","token":"glab"},"image":"i"}`
	var s Settings
	if err := json.Unmarshal([]byte(in), &s); err != nil {
		t.Fatal(err)
	}
	if a := ProviderSettings[awsSettings](s, "aws"); s.Image != "i" || a == nil || a.Profiles[0] != "p" ||
		ProviderSettings[githubSettings](s, "github") != nil {
		t.Fatalf("%+v", s)
	}
	out, _ := json.Marshal(s)
	if string(out) != in {
		t.Fatalf("round trip: %s", out)
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

// Jira is on with its settings and a token command.
func TestJiraBroker(t *testing.T) {
	var s Settings
	t.Setenv("JIG_JIRA_TOKEN_COMMAND", "echo tok")
	if p := jiraBroker(s); p != nil {
		t.Fatalf("no settings: %+v", p)
	}
	s.SetProvider("jira", jiraSettings{Site: "example.atlassian.net", CloudID: "01234567-89ab-cdef-0123-456789abcdef", Email: "a@example.org"})
	j, ok := jiraBroker(s).(providers.Jira)
	if !ok || j.Site != "example.atlassian.net" || j.Email != "a@example.org" || j.TokenCmd != "echo tok" {
		t.Fatalf("jira: %+v", j)
	}
	t.Setenv("JIG_JIRA_TOKEN_COMMAND", "")
	if p := jiraBroker(s); p != nil {
		t.Fatalf("no token: %+v", p)
	}
}
