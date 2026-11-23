// Package providers is the list of the broker's providers: the hostname
// each is served on in the sandbox, and what turns it on. Start here to
// review the broker: each provider's package has a routes.go (or, for
// the two with no per-request rules, a New) listing every request it
// serves.
//
//	gitlab.broker.internal     broker/gitlab     git, LFS, REST, GraphQL, GitLab's MCP server
//	github.broker.internal     broker/github     git, REST, GraphQL (as gh calls an Enterprise Server)
//	anthropic.broker.internal  broker/anthropic  the Claude API, for Claude Code
//	aws.broker.internal        broker/awsmcp     the AWS MCP Server, through AWS's stdio proxy
package providers

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"

	"mcp-restrict/broker"
	"mcp-restrict/broker/anthropic"
	"mcp-restrict/broker/awsmcp"
	"mcp-restrict/broker/github"
	"mcp-restrict/broker/gitlab"
)

// Hostnames the sandbox uses for each provider.
const (
	GitLabHost    = "gitlab.broker.internal"
	GitHubHost    = "github.broker.internal"
	AnthropicHost = "anthropic.broker.internal"
	AWSHost       = "aws.broker.internal"
)

// Config says which providers to serve. An empty user, command or list
// leaves a provider out. The token commands print a credential (glab's or
// gh's token, a Keychain item); they run on the Mac, at most once a
// minute.
type Config struct {
	GitLabUser, GitLabURL, GitLabTokenCmd string
	GitLabMCPTokenCmd                     string // a fine-grained PAT for GitLab's MCP server; default the GitLab token
	GitHubUser, GitHubTokenCmd            string
	AnthropicTokenCmd                     string
	AWSProfiles                           []string // the first is the default
	AWSMCPCommand                         string   // tests: a stdio MCP server instead of AWS's proxy
}

// Brokered names the configured providers, as the allowlist's groups name
// them: their hosts leave the allowlist.
func (c Config) Brokered() []string {
	var out []string
	if c.GitLabUser != "" {
		out = append(out, "gitlab")
	}
	if c.GitHubUser != "" {
		out = append(out, "github")
	}
	if c.AnthropicTokenCmd != "" {
		out = append(out, "anthropic")
	}
	if c.AWSOn() {
		out = append(out, "aws")
	}
	return out
}

// AWSOn reports whether the AWS provider is configured.
func (c Config) AWSOn() bool { return len(c.AWSProfiles) > 0 || c.AWSMCPCommand != "" }

func public(host string) *url.URL { return &url.URL{Scheme: "https", Host: host} }

func mustURL(s string) *url.URL {
	u, err := url.Parse(s)
	if err != nil {
		panic(err)
	}
	return u
}

// Build returns the configured providers by hostname. Processes they run
// (the AWS MCP proxy) stop when ctx ends; their files go in state.
func Build(ctx context.Context, c Config, state string, log *slog.Logger) (map[string]http.Handler, error) {
	out := map[string]http.Handler{}
	if c.GitLabUser != "" {
		up, err := url.Parse(c.GitLabURL)
		if err != nil || up.Scheme != "https" || up.Host == "" {
			return nil, fmt.Errorf("the GitLab URL %q: want https://host", c.GitLabURL)
		}
		gc := gitlab.Config{User: c.GitLabUser, Token: broker.TokenSource(c.GitLabTokenCmd, ""),
			Upstream: up, Public: public(GitLabHost), Client: broker.NewUpstreamClient(), Log: log}
		if c.GitLabMCPTokenCmd != "" {
			gc.MCPToken = broker.TokenSource(c.GitLabMCPTokenCmd, "")
		}
		out[GitLabHost] = gitlab.New(gc)
	}
	if c.GitHubUser != "" {
		out[GitHubHost] = github.New(github.Config{User: c.GitHubUser, Token: broker.TokenSource(c.GitHubTokenCmd, ""),
			Git: mustURL("https://github.com"), API: mustURL("https://api.github.com"),
			Public: public(GitHubHost), Client: broker.NewUpstreamClient(), Log: log})
	}
	if c.AnthropicTokenCmd != "" {
		out[AnthropicHost] = anthropic.New(anthropic.Config{Token: broker.TokenSource(c.AnthropicTokenCmd, ""),
			API: mustURL("https://api.anthropic.com"), Public: public(AnthropicHost), Client: broker.NewUpstreamClient(), Log: log})
	}
	if c.AWSOn() {
		p, err := awsmcp.New(ctx, awsmcp.Config{Profiles: c.AWSProfiles, Command: c.AWSMCPCommand, State: state, Log: log})
		if err != nil {
			return nil, err
		}
		out[AWSHost] = p
	}
	return out, nil
}
