package providers

import (
	"context"
	"log/slog"
	"slices"
	"testing"
)

func TestConfigHelpers(t *testing.T) {
	c := Config{Providers: []Provider{
		GitLab{User: "u", URL: "https://GitLab.Example.org:8443"},
		GitHub{User: "u", WebHost: "octo.ghe.com", APIHost: "api.octo.ghe.com"},
		Anthropic{TokenCmd: "true"},
		AWS{Profiles: []string{"p"}},
	}}
	if got, want := c.Hosts(), []string{"gitlab.example.org", "octo.ghe.com", "api.octo.ghe.com", AnthropicAPIHost}; !slices.Equal(got, want) {
		t.Errorf("Hosts %v, want %v", got, want)
	}
	if got, want := c.Internal(), []string{GitLabHost, GitHubHost, AnthropicHost, AWSHost}; !slices.Equal(got, want) {
		t.Errorf("Internal (server) %v, want %v", got, want)
	}
	c.Proxy = true
	if got := c.Internal(); !slices.Equal(got, []string{AWSHost}) {
		t.Errorf("Internal (proxy) %v", got)
	}
	if got := c.InternalOnly(); !slices.Equal(got, []string{"aws"}) {
		t.Errorf("InternalOnly %v", got)
	}
	if got := c.Brokered(); !slices.Equal(got, []string{"gitlab", "github", "anthropic", "aws"}) {
		t.Errorf("Brokered %v", got)
	}
	if c.Get("github") == nil || c.Get("datadog") != nil {
		t.Error("Get")
	}
}

// GitHub on another host: proxy mode serves that host and its API host;
// the guest's gh treats it as an enterprise host.
func TestGitHubHost(t *testing.T) {
	g := GitHub{User: "u", TokenCmd: "true", WebHost: "octo.ghe.com", APIHost: "api.octo.ghe.com"}
	ps, err := Build(context.Background(), Config{Proxy: true, Providers: []Provider{g}}, "", slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := ps["octo.ghe.com"]; !ok || len(ps) != 2 {
		t.Errorf("served: %v", ps)
	}
	if e := g.Guest(true).Env; e["GH_HOST"] != "octo.ghe.com" || e["GH_TOKEN"] != "" {
		t.Errorf("proxy env: %v", e)
	}
	gc := g.Guest(false)
	if v := gc.Git["url.https://"+GitHubHost+"/.insteadOf"]; !slices.Contains(v, "git@octo.ghe.com:") {
		t.Errorf("insteadOf: %v", gc.Git)
	}
}

func TestBuildRefusesTwoProvidersOnOneHost(t *testing.T) {
	c := Config{Providers: []Provider{Anthropic{TokenCmd: "true"}, Anthropic{TokenCmd: "true"}}}
	if _, err := Build(context.Background(), c, "", slog.New(slog.DiscardHandler)); err == nil {
		t.Error("built")
	}
}
