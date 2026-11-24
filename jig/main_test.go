package jig

import (
	"os"
	"testing"
)

// The tests run as the personal track.
func TestMain(m *testing.M) {
	track = Personal()
	os.Exit(m.Run())
}

// A track offers only its own providers: another's settings are ignored.
func TestTrackProviders(t *testing.T) {
	defer func(old Track) { track = old }(track)
	t.Setenv("JIG_STATE", t.TempDir())
	t.Setenv("JIG_CLAUDE_TOKEN_COMMAND", "")
	for _, k := range []string{"JIG_GITLAB_USER", "JIG_AWS_PROFILES", "JIG_AWS_MCP_COMMAND"} {
		os.Unsetenv(k)
	}
	var s Settings
	s.SetProvider("gitlab", gitlabSettings{User: "alice", URL: "https://gitlab.com", Token: "glab"})
	s.SetProvider("aws", awsSettings{Profiles: []string{"p"}})
	SaveSettings(s)
	track = Personal()
	track.Providers = []ProviderCLI{AWS}
	b := brokerConfig()
	if len(b.Providers) != 1 || b.Get("aws") == nil {
		t.Errorf("%+v", b.Providers)
	}
	if _, ok := cliProviderNamed("gitlab"); ok {
		t.Error("gitlab offered")
	}
}
