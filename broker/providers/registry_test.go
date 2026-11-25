package providers

import (
	"context"
	"log/slog"
	"slices"
	"strings"
	"testing"
)

func TestConfigHelpers(t *testing.T) {
	c := Config{Providers: []Provider{
		GitLab{User: "u", URL: "https://GitLab.Example.org:8443"},
		GitHub{User: "u", WebHost: "octo.ghe.com", APIHost: "api.octo.ghe.com"},
		Anthropic{TokenCmd: "true"},
		AWS{Profiles: []string{"p"}},
	}}
	if got, want := c.Internal(), []string{GitLabHost, GitHubHost, AnthropicHost, AWSHost}; !slices.Equal(got, want) {
		t.Errorf("Internal (server) %v, want %v", got, want)
	}
	c.Proxy = true
	if got := c.Internal(); len(got) != 0 {
		t.Errorf("Internal (proxy) %v", got)
	}
	if got, want := c.Hosts(), []string{"gitlab.example.org", "octo.ghe.com", "api.octo.ghe.com", AnthropicAPIHost, "aws-mcp.us-east-1.api.aws"}; !slices.Equal(got, want) {
		t.Errorf("Hosts (proxy) %v, want %v", got, want)
	}
	if err := c.Validate(); err != nil {
		t.Errorf("Validate: %v", err)
	}
	if got := c.SelfServed(); !slices.Equal(got, []string{"aws"}) {
		t.Errorf("SelfServed %v", got)
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

// internalOnly is a provider with no real host.
type internalOnly struct{ AWS }

func (internalOnly) Name() string        { return "internal" }
func (internalOnly) Hosts(bool) []string { return []string{"x" + InternalSuffix} }

// Proxy mode refuses a provider with only an internal name: the guest
// has no route to it there.
func TestValidateProxy(t *testing.T) {
	c := Config{Providers: []Provider{internalOnly{}}}
	if err := c.Validate(); err != nil {
		t.Errorf("server mode: %v", err)
	}
	c.Proxy = true
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "x.broker.internal") {
		t.Errorf("proxy mode: %v", err)
	}
	if _, err := Build(context.Background(), c, t.TempDir(), slog.Default()); err == nil {
		t.Error("Build in proxy mode")
	}
}
