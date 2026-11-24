// Package providers lists the broker's providers. Start reviewing the
// broker here: Build says, for each provider, what turns it on and which
// function holds its policy. That function (Routes, in each provider's
// routes.go) defines the provider's upstreams, each with its credential,
// then its route table: every request it serves, the check run on it and
// where it goes. A request that matches no route is denied.
//
// The checks are next to Routes (checks.go and the like); the rules the
// providers share are in broker/policy.go, credential headers in
// broker/upstream.go.
package providers

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"

	"mono-jig/broker"
	"mono-jig/broker/anthropic"
	"mono-jig/broker/awsmcp"
	"mono-jig/broker/github"
	"mono-jig/broker/gitlab"
)

// Hostnames the sandbox uses for each provider.
const (
	GitLabHost    = "gitlab.broker.internal"
	GitHubHost    = "github.broker.internal"
	AnthropicHost = "anthropic.broker.internal"
	AWSHost       = "aws.broker.internal"
)

// Config says which providers to serve: an empty user, command or list
// leaves one out. The token commands print a credential (glab's or gh's
// token, a Keychain item); they run on the Mac, at most once a minute.
type Config struct {
	GitLabUser, GitLabURL, GitLabTokenCmd string
	GitLabMCPTokenCmd                     string // a fine-grained PAT for GitLab's MCP server; default the GitLab token
	GitHubUser, GitHubTokenCmd            string
	AnthropicTokenCmd                     string
	AWSProfiles                           []string // the first is the default
	AWSMCPCommand                         string   // tests: a stdio MCP server instead of AWS's proxy

	// Overrides for tests and mono-jig-sbx; empty means the default.
	GitHubURL, GitHubAPIURL, AnthropicURL string                     // https://github.com, https://api.github.com, https://api.anthropic.com
	Public                                func(host string) *url.URL // how the sandbox reaches host: https://<host>
	Client                                *http.Client               // the upstreams': broker.NewUpstreamClient()
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

// Build returns the configured providers by hostname. Processes they run
// (the AWS MCP proxy) stop when ctx ends; their files go in state.
func Build(ctx context.Context, c Config, state string, log *slog.Logger) (map[string]http.Handler, error) {
	if u, err := url.Parse(c.GitLabURL); c.GitLabUser != "" && (err != nil || u.Host == "") {
		return nil, fmt.Errorf("the GitLab URL %q: want https://host", c.GitLabURL)
	}
	b := newBuilder(c, log)

	// gitlab.broker.internal: git, LFS, REST, GraphQL, GitLab's MCP server.
	// On once mono-jig login gitlab recorded a user.
	if c.GitLabUser != "" {
		b.serve(GitLabHost, gitlab.Routes(b.upstreams(GitLabHost, "gitlab"), gitlab.Config{
			User: c.GitLabUser, URL: c.GitLabURL,
			Token: token(c.GitLabTokenCmd), MCPToken: optToken(c.GitLabMCPTokenCmd)}))
	}

	// github.broker.internal: git, REST, GraphQL, as gh calls a GitHub
	// Enterprise Server. On once mono-jig login github recorded a user.
	if c.GitHubUser != "" {
		b.serve(GitHubHost, github.Routes(b.upstreams(GitHubHost, "github"), github.Config{
			User: c.GitHubUser, Token: token(c.GitHubTokenCmd),
			GitURL: c.GitHubURL, APIURL: c.GitHubAPIURL}))
	}

	// anthropic.broker.internal: the Claude API, for Claude Code. On once
	// mono-jig login claude stored a token.
	if c.AnthropicTokenCmd != "" {
		b.serve(AnthropicHost, anthropic.Routes(b.upstreams(AnthropicHost, "claude"), anthropic.Config{
			Token: token(c.AnthropicTokenCmd), APIURL: c.AnthropicURL}))
	}

	// aws.broker.internal: the AWS MCP Server, through AWS's stdio proxy on
	// the Mac. On once mono-jig login aws recorded profiles.
	if c.AWSOn() {
		proxy, err := awsmcp.Start(ctx, awsmcp.Config{Profiles: c.AWSProfiles, Command: c.AWSMCPCommand, State: state, Log: log})
		if err != nil {
			return nil, err
		}
		b.serve(AWSHost, awsmcp.Routes(proxy))
	}
	return b.out, nil
}

// builder collects the providers, and makes their upstreams.
type builder struct {
	public func(host string) *url.URL
	client *http.Client
	log    *slog.Logger
	out    map[string]http.Handler
}

func newBuilder(c Config, log *slog.Logger) *builder {
	b := &builder{public: c.Public, client: c.Client, log: log, out: map[string]http.Handler{}}
	if b.client == nil {
		b.client = broker.NewUpstreamClient()
	}
	if b.public == nil {
		b.public = func(host string) *url.URL { return &url.URL{Scheme: "https", Host: host} }
	}
	return b
}

// upstreams are a provider's: reached by the sandbox at host, with a
// missing credential sending the user to mono-jig login <login>.
func (b *builder) upstreams(host, login string) broker.Upstreams {
	return broker.Upstreams{Public: b.public(host), Client: b.client,
		NoCredential: "no credential: run mono-jig login " + login}
}

// serve serves host from its route table.
func (b *builder) serve(host string, routes []broker.Route) {
	name := host[:len(host)-len(".broker.internal")]
	b.out[host] = &broker.Provider{Name: name, Routes: routes, Log: b.log}
}

// token runs the token command (at most once a minute).
func token(cmd string) broker.Token { return broker.TokenSource(cmd, "") }

// optToken is token, or nil without a command.
func optToken(cmd string) broker.Token {
	if cmd == "" {
		return nil
	}
	return token(cmd)
}
